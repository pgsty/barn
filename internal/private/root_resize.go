package private

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pgsty/barn/internal/disk"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/identity"
	"github.com/pgsty/barn/internal/lock"
	"github.com/pgsty/barn/internal/qemu"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/sshkeys"
	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/vm"
)

//go:embed root_resize_guest.py
var rootResizeGuest string

type rootResizeActions struct {
	save   func(*state.RootResize) error
	start  func() error
	stop   func() error
	probe  func(string, state.RootFilesystem) (state.RootFilesystem, error)
	grow   func() error
	commit func() error
}

// Every step can be retried after losing its response. In particular, "image"
// remains durable until qemu-img and its verification have both succeeded.
func runRootResize(journal state.RootResize, actions rootResizeActions) error {
	if err := journal.Validate(); err != nil {
		return err
	}
	if journal.Stage == "preflight" {
		if err := actions.start(); err != nil {
			return err
		}
		guest, err := actions.probe("preflight", state.RootFilesystem{})
		if err != nil {
			return err
		}
		if guest.DiskBytes != journal.Original {
			return errors.New("guest root disk size differs from the applied size")
		}
		journal.Guest, journal.Stage = guest, "image"
		if err := journal.Validate(); err != nil {
			return err
		}
		if err := actions.save(&journal); err != nil {
			return err
		}
	}
	if journal.Stage == "image" {
		if err := actions.stop(); err != nil {
			return err
		}
		if err := actions.grow(); err != nil {
			return err
		}
		journal.Stage = "guest"
		if err := actions.save(&journal); err != nil {
			return err
		}
	}
	if journal.Stage == "guest" {
		if err := actions.commit(); err != nil {
			return err
		}
		if err := actions.start(); err != nil {
			return err
		}
		if _, err := actions.probe("grow", journal.Guest); err != nil {
			return err
		}
		journal.Stage = "verified"
		if err := actions.save(&journal); err != nil {
			return err
		}
	}
	if err := actions.commit(); err != nil {
		return err
	}
	if journal.WasRunning {
		if err := actions.start(); err != nil {
			return err
		}
	} else if err := actions.stop(); err != nil {
		return err
	}
	return actions.save(nil)
}

func validateRootInvocation(node state.NodeState, root, serial string) error {
	if node.RootDisk != root {
		return errors.New("root disk path does not match the managed node directory")
	}
	fileMatches, formatMatches, deviceMatches := false, false, false
	args := node.Invocation.Args
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "-blockdev" {
			var entry struct {
				Driver   string `json:"driver"`
				Filename string `json:"filename"`
				Name     string `json:"node-name"`
				File     string `json:"file"`
			}
			if err := json.Unmarshal([]byte(args[index+1]), &entry); err != nil {
				return err
			}
			if entry.Name == "root-file" {
				fileMatches = entry.Driver == "file" && entry.Filename == root
			}
			if entry.Name == "root" {
				formatMatches = entry.Driver == "qcow2" && entry.File == "root-file"
			}
		}
		if args[index] == "-device" && args[index+1] == "virtio-blk-pci,drive=root,serial="+serial+",bootindex=1" {
			deviceMatches = true
		}
	}
	if !fileMatches || !formatMatches || !deviceMatches {
		return errors.New("root disk path or serial does not match the recorded QEMU invocation")
	}
	return nil
}

func (m Manager) rootGuest(ctx context.Context, deployment Deployment, node state.NodeState, user, mode string, target int64, expected state.RootFilesystem) (state.RootFilesystem, error) {
	ssh, err := m.lookPath("ssh")
	if err != nil {
		return state.RootFilesystem{}, err
	}
	key, known, _, err := sshkeys.EnsureExistingKeys(ctx, m.runner(), deployment.Root)
	if err != nil {
		return state.RootFilesystem{}, err
	}
	serial, err := identity.DiskSerial(node.Node, "root")
	if err != nil {
		return state.RootFilesystem{}, err
	}
	encoded, err := json.Marshal(expected)
	if err != nil {
		return state.RootFilesystem{}, err
	}
	args := vm.SSHArgsForInstance(user, key, known, vm.HostKeyAlias(node.VMUUID), node.SSHPort,
		"sudo", "-n", "python3", "-c", rootResizeGuest, mode, serial, strconv.FormatInt(target, 10), string(encoded))
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	result, err := guestSetupRunner(m.runner()).Run(checkCtx, ssh, args...)
	if err != nil {
		return state.RootFilesystem{}, fmt.Errorf("guest root %s: %w", mode, guestSetupError(err))
	}
	var actual state.RootFilesystem
	if err := json.Unmarshal(result.Stdout, &actual); err != nil {
		return actual, fmt.Errorf("decode guest root geometry: %w", err)
	}
	return actual, nil
}

func (m Manager) resizeRoot(ctx context.Context, target spec.Node) (_ string, returnErr error) {
	deployment, err := m.openDeployment(false)
	if err != nil {
		return "", err
	}
	held, err := acquireDeploymentLock(ctx, deployment.Root, false, m.Progress)
	if err != nil {
		return "", err
	}
	defer func() { returnErr = lock.JoinRelease(returnErr, held, "root resize lock") }()
	m.Nodes, m.NoWait = []string{target.Name}, false
	store := state.Store{Root: deployment.Root}
	// Converge interrupted start/stop transitions while holding the same lock
	// that will exclude new QEMU processes throughout the offline image edit.
	if _, err := m.statusForLocked(ctx, deployment, ""); err != nil {
		return "", err
	}
	applied, err := store.ReadDeployment()
	if err != nil {
		return "", err
	}
	before, found := resizeTargetNode(applied.Resolved, target.Name)
	if !found || !resourceOnlyNodeChange(applied.Resolved, before, applied.Resolved, target) {
		return "", errors.New("node definition changed before root growth; retry with the current inventory")
	}
	node, err := store.ReadNode(target.Name)
	if err != nil {
		return "", err
	}
	if node.Phase != state.Running && node.Phase != state.Stopped && node.Phase != state.Prepared {
		return "", interruptedPhaseError(node)
	}
	nodeDir, err := deployment.NodeDir(node.Node)
	if err != nil {
		return "", err
	}
	serial, err := identity.DiskSerial(node.Node, "root")
	if err != nil {
		return "", err
	}
	if err := validateRootInvocation(node, filepath.Join(nodeDir, "root.qcow2"), serial); err != nil {
		return "", err
	}
	if node.RootResize == nil && target.RootDisk == before.RootDisk {
		return "unchanged", nil
	}
	if target.RootDisk < before.RootDisk || target.RootDisk%512 != 0 {
		return "", errors.New("root resize only supports growth to a 512-byte-aligned absolute size")
	}
	if node.RootResize != nil && node.RootResize.Target != target.RootDisk {
		return "", fmt.Errorf("root growth to %d bytes is unfinished; retry the same target first", node.RootResize.Target)
	}
	qemuImg, err := m.lookPath("qemu-img")
	if err != nil {
		return "", err
	}
	profile, err := m.nativeProfile()
	if err != nil {
		return "", err
	}
	backend, err := m.preflight(ctx, profile, resolvedNodeSelection(applied.Resolved, m.Nodes))
	if err != nil {
		return "", err
	}
	shareBinaries, err := selectedShareInvocationBinaries(store, applied.Resolved, m.Nodes)
	if err != nil {
		return "", err
	}
	if err := validatePrivateShareDeviceHelp(ctx, m.runner(), shareBinaries); err != nil {
		return "", err
	}
	if node.RootResize == nil {
		node.RootResize = &state.RootResize{Original: before.RootDisk, Target: target.RootDisk, WasRunning: node.Phase == state.Running, Stage: "preflight"}
		node.UpdatedAt = time.Now().UTC()
		if err := store.WriteNode(node); err != nil {
			return "", err
		}
	}
	journal := *node.RootResize
	if !journal.WasRunning {
		m.report("resize", "Temporarily starting "+node.Node+" to verify root growth; it will return to stopped")
	}
	save := func(value *state.RootResize) error {
		current, err := store.ReadNode(target.Name)
		if err != nil {
			return err
		}
		current.RootResize, current.UpdatedAt = value, time.Now().UTC()
		return store.WriteNode(current)
	}
	stop := func() error {
		_, err := m.stopLocked(ctx, deployment)
		return err
	}
	start := func() error {
		current, err := store.ReadDeployment()
		if err != nil {
			return err
		}
		node, err := store.ReadNode(target.Name)
		if err != nil {
			return err
		}
		if node.Phase == state.Running {
			return nil
		}
		if err := m.ensureSSHAddressesUnused([]spec.Node{target}); err != nil {
			return err
		}
		_, err = m.startExistingLocked(ctx, deployment, current, backend, m.Nodes)
		return err
	}
	actions := rootResizeActions{
		save: save, start: start, stop: stop,
		probe: func(mode string, expected state.RootFilesystem) (state.RootFilesystem, error) {
			node, err := store.ReadNode(target.Name)
			if err != nil {
				return state.RootFilesystem{}, err
			}
			return m.rootGuest(ctx, deployment, node, applied.Resolved.SSHUser, mode, target.RootDisk, expected)
		},
		grow: func() error {
			node, err := store.ReadNode(target.Name)
			if err != nil {
				return err
			}
			observation, err := RuntimeIdentityAuditor(m.runner(), time.Second)(ctx, node)
			if err != nil {
				return err
			}
			if observation.Live || observation.Authority != "dead" || node.Process.PID != 0 {
				return errors.New("root growth requires proven QEMU process death")
			}
			disks := disk.Manager{QEMUImg: qemuImg, Runner: m.runner()}
			info, err := disks.Inspect(ctx, node.RootDisk)
			if err != nil {
				return err
			}
			if info.VirtualSize < journal.Original || info.VirtualSize > journal.Target {
				return errors.New("root image capacity is outside the recorded growth interval")
			}
			m.report("resize", "Growing stopped root image for "+node.Node)
			_, _, err = disks.Grow(ctx, node.RootDisk, journal.Target, true)
			return err
		},
		commit: func() error {
			current, err := store.ReadDeployment()
			if err != nil {
				return err
			}
			definition, _ := resizeTargetNode(current.Resolved, target.Name)
			node, err := store.ReadNode(target.Name)
			if err != nil {
				return err
			}
			updated, err := resizeDeploymentState(current, target.Name, target, m.BarnVersion, time.Now().UTC())
			if err != nil {
				return err
			}
			wantedHash, err := spec.NodeHash(updated.Resolved, target.Name)
			if err != nil {
				return err
			}
			if definition.CPUs == target.CPUs && definition.Memory == target.Memory && definition.RootDisk == target.RootDisk && node.SpecHash == wantedHash {
				return nil
			}
			if err := stop(); err != nil {
				return err
			}
			node, err = store.ReadNode(target.Name)
			if err != nil {
				return err
			}
			next, err := qemu.ConfigureResources(resizeBaseInvocation(node), target.CPUs, target.Memory)
			if err != nil {
				return err
			}
			_, _, err = commitResize(store, current, node, target, next, false, m.BarnVersion, time.Now().UTC())
			return err
		},
	}
	if err := runRootResize(journal, actions); err != nil {
		// Restore an initially stopped VM even when guest growth fails. A killed
		// CLI skips this cleanup, but the durable power intent remains for retry.
		if !journal.WasRunning {
			recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
			_, stopErr := m.stopLocked(recoveryCtx, deployment)
			cancel()
			err = errors.Join(err, stopErr)
		}
		// Before image mutation, a rejected layout or missing tool must not
		// pin an unusable target once the original power state is restored.
		current, readErr := store.ReadNode(target.Name)
		if readErr == nil && rootPreflightCanBeDiscarded(current) {
			if clearErr := save(nil); clearErr == nil {
				return "", fmt.Errorf("node %s root growth did not start: %w", target.Name, err)
			} else {
				err = errors.Join(err, clearErr)
			}
		}
		return "", failure.WithNext(fmt.Errorf("node %s root growth remains pending: %w", target.Name, err), "barn resize -D "+strconv.FormatInt(journal.Target, 10)+"B "+target.Name)
	}
	if journal.WasRunning {
		return "offline restart", nil
	}
	return "offline temporary boot; restored stopped", nil
}

func rootPreflightCanBeDiscarded(node state.NodeState) bool {
	if node.RootResize == nil || node.RootResize.Stage != "preflight" {
		return false
	}
	if node.RootResize.WasRunning {
		return node.Phase == state.Running
	}
	return (node.Phase == state.Stopped || node.Phase == state.Prepared) && node.Process.PID == 0
}
