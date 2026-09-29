package macvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/pgsty/barn/internal/lock"
	"golang.org/x/crypto/ssh"
)

// MachineView is one machine as status reports it. State is the lifecycle
// state (absent, prepared, starting, running, stopping, stopped or unknown);
// Ready is true only when this check reached the guest over SSH with sudo.
type MachineView struct {
	Name          string         `json:"name"`
	Kind          string         `json:"kind"`
	State         string         `json:"state"`
	Ready         bool           `json:"ready"`
	SSH           string         `json:"ssh"`
	Address       string         `json:"address"`
	AddressStable bool           `json:"address_stable"`
	SSHHost       string         `json:"ssh_host"`
	SSHPort       uint16         `json:"ssh_port"`
	User          string         `json:"user"`
	Image         ImageRef       `json:"image"`
	Observed      *ImageRef      `json:"observed,omitempty"`
	CPUs          int            `json:"cpus"`
	MemoryBytes   int64          `json:"memory_bytes"`
	Disk          DiskUsage      `json:"disk"`
	Network       MachineNetwork `json:"network"`
	Shares        []ShareView    `json:"shares"`
	Clipboard     bool           `json:"clipboard"`
	PID           int            `json:"pid,omitempty"`
	WindowVisible bool           `json:"window_visible,omitempty"`
	InstanceID    string         `json:"instance_id"`
	Warnings      []string       `json:"warnings"`
	Error         string         `json:"error,omitempty"`

	machine *Machine
}

type ImageRef struct {
	Version string `json:"version"`
	Build   string `json:"build"`
	BaseID  string `json:"base_id,omitempty"`
}

type DiskUsage struct {
	CapacityBytes  int64  `json:"capacity_bytes"`
	AllocatedBytes *int64 `json:"allocated_bytes,omitempty"`
}

type ShareView struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Guest    string `json:"guest"`
	ReadOnly bool   `json:"readonly"`
}

func newMachineView(machine *Machine) MachineView {
	view := MachineView{Name: machine.Name, Kind: "macos", State: displayState(machine), SSH: "unchecked",
		Address: machine.Network.Address, AddressStable: true, SSHHost: machine.Network.Address, SSHPort: 22,
		User: machine.User, Image: ImageRef{Version: machine.Version, Build: machine.Build, BaseID: machine.BaseID},
		CPUs: machine.CPU, MemoryBytes: machine.MemoryBytes, Disk: DiskUsage{CapacityBytes: machine.DiskBytes},
		Network: machine.Network, Clipboard: machine.Clipboard, InstanceID: machine.InstanceID,
		Shares: []ShareView{}, Warnings: []string{}, Error: machine.LastError, machine: machine}
	if machine.ObservedBuild != "" && machine.ObservedBuild != machine.Build {
		view.Observed = &ImageRef{Version: machine.ObservedVersion, Build: machine.ObservedBuild}
	}
	for _, share := range machine.Shares {
		view.Shares = append(view.Shares, ShareView{Name: share.Name, Host: share.Path, Guest: GuestShareRoot + "/" + share.Name, ReadOnly: share.ReadOnly})
	}
	return view
}

// displayState maps a persisted stage to what a stopped machine looks like.
func displayState(machine *Machine) string {
	if !machine.Initialized && (machine.State == "prepared" || machine.State == "failed") {
		return "prepared"
	}
	return "stopped"
}

func (m *Manager) status(ctx context.Context, machine *Machine) (RuntimeStatus, error) {
	socket, err := m.socket(machine.Name, false)
	if err != nil {
		return RuntimeStatus{}, err
	}
	probe, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return m.Runner.RPC(probe, socket, machine.InstanceID, "status", false)
}

// inspectRuntime proves every claim it makes: a runner answer shows running,
// a free runner lock shows stopped, anything else is unknown.
func (m *Manager) inspectRuntime(ctx context.Context, view *MachineView, probeSSH bool) {
	machine := view.machine
	status, err := m.status(ctx, machine)
	if err != nil {
		if ctx.Err() != nil {
			view.State, view.Error = "unknown", ctx.Err().Error()
			return
		}
		// An RPC error is not proof of shutdown. An in-flight operation can also
		// be between publishing starting state and acquiring the runner lock.
		path, pathErr := m.Store.Path("runtime", machine.Name+".operation.lock")
		if pathErr != nil {
			view.State, view.Error = "unknown", pathErr.Error()
			return
		}
		if _, statErr := os.Stat(path); statErr == nil {
			op, lockErr := lock.TryAcquire(path, false)
			if lockErr != nil {
				view.State, view.Error = "unknown", "another barn mac command is changing this machine"
				return
			}
			defer func() {
				if err := op.Release(); err != nil {
					view.State, view.Error = "unknown", err.Error()
				}
			}()
		} else if !errors.Is(statErr, os.ErrNotExist) {
			view.State, view.Error = "unknown", statErr.Error()
			return
		}
		proof, proofErr := m.stoppedRunnerLock(machine)
		if proofErr != nil {
			view.State, view.Error = "unknown", proofErr.Error()
			return
		}
		if proof != nil {
			if err := proof.Release(); err != nil {
				view.State, view.Error = "unknown", err.Error()
				return
			}
		}
		view.SSH = "offline"
		view.State = displayState(machine)
		return
	}
	view.PID, view.WindowVisible = status.PID, status.WindowVisible
	switch status.State {
	case "running":
		view.State = "running"
	case "starting", "resuming", "restoring":
		view.State = "starting"
	case "stopping", "pausing", "saving":
		view.State = "stopping"
	case "stopped":
		view.State = displayState(machine)
	case "paused", "error", "unknown":
		view.State, view.Error = "unknown", "the VM reports "+status.State
	default:
		view.State, view.Error = "unknown", fmt.Sprintf("runner returned an invalid runtime state %q", status.State)
		return
	}
	if view.State != "running" {
		view.SSH = "offline"
		return
	}
	if !machine.Initialized {
		view.SSH = "pending"
		return
	}
	if !probeSSH {
		return
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := m.probeGuestSSH(check, machine); err != nil {
		view.SSH = "unavailable"
		view.Warnings = append(view.Warnings, "ssh: "+err.Error())
		return
	}
	view.SSH, view.Ready, view.Error = "ready", true, ""
}

func (m *Manager) probeGuestSSH(ctx context.Context, machine *Machine) error {
	signer, verify, err := m.sshIdentity(machine)
	if err != nil {
		return err
	}
	client, err := dialSSH(ctx, machine.Network.Address, machine.User, []ssh.AuthMethod{ssh.PublicKeys(signer)}, verify)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	_, err = observeSSH(ctx, client, machine.User)
	return err
}

// A failed RPC permits a new runner only after independently proving the old
// runner no longer owns the machine. A responding runner that is not running
// cannot be reported running.
func (m *Manager) shouldLaunch(ctx context.Context, machine *Machine) (bool, error) {
	status, err := m.status(ctx, machine)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err == nil {
		if status.State != "running" {
			return false, fmt.Errorf("%s is %s; wait for it to finish, then retry", machine.Name, status.State)
		}
		return false, nil
	}
	proof, err := m.stoppedRunnerLock(machine)
	if err != nil {
		return false, err
	}
	if proof != nil {
		if err = proof.Release(); err != nil {
			return false, err
		}
	}
	return true, nil
}

// stoppedRunnerLock proves no runner owns the machine by taking its runner
// lock. A machine that never booted has no lock file yet.
func (m *Manager) stoppedRunnerLock(machine *Machine) (*lock.File, error) {
	path, err := m.Store.machineFile(machine.Name, "runner.lock")
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
		bootPath, pathErr := m.Store.machineFile(machine.Name, "boot-requested")
		if pathErr != nil {
			return nil, pathErr
		}
		_, bootErr := os.Stat(bootPath)
		if machine.Initialized || bootErr == nil {
			return nil, fmt.Errorf("%s previously booted but its runner lock is missing; stopped state is unconfirmed", machine.Name)
		}
		if !errors.Is(bootErr, os.ErrNotExist) {
			return nil, bootErr
		}
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	proof, err := lock.TryAcquire(path, false)
	if err != nil {
		return nil, fmt.Errorf("%s runtime is unresponsive or still active; stopped state is unconfirmed: %w", machine.Name, err)
	}
	return proof, nil
}
