package private

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/pgsty/farrow/internal/activity"
	"github.com/pgsty/farrow/internal/cloudinit"
	"github.com/pgsty/farrow/internal/process"
	"github.com/pgsty/farrow/internal/runtimepath"
	"github.com/pgsty/farrow/internal/spec"
	"github.com/pgsty/farrow/internal/state"
	"github.com/pgsty/farrow/internal/vm"
)

// NodeLifecycle starts one guest, undoes a start whose state could not be
// persisted, and waits for the guest's ready marker.
type NodeLifecycle interface {
	Start(context.Context, state.NodeState) (process.Identity, error)
	AbortStart(context.Context, state.NodeState, process.Identity) error
	WaitReady(context.Context, state.NodeState, time.Duration) ([]state.GuestWarning, error)
}

type NativeLifecycle struct {
	RetryGuestSetup bool
	Resolved        spec.Resolved
	VM              vm.Lifecycle
	Deployment      Deployment
	Shares          map[string][]spec.Share
	SSHPath         string
	PrivateKey      string
	KnownHosts      string
	DarwinSocket    string
}

func (l NativeLifecycle) PreflightStart(node state.NodeState) error {
	bundle, err := openPrivateNodeShares(l.Deployment, l.Shares, node)
	if err != nil {
		return err
	}
	if err := bundle.Close(); err != nil {
		return fmt.Errorf("close preflight host shares for private node %s: %w", node.Node, err)
	}
	return nil
}

func (l NativeLifecycle) privateNetworkFile() (*os.File, error) {
	if l.DarwinSocket == "" {
		return nil, errors.New("FD invocation has no Darwin socket")
	}
	connection, err := net.DialTimeout("unix", l.DarwinSocket, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial socket_vmnet FD fallback: %w", err)
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		_ = connection.Close()
		return nil, errors.New("socket_vmnet fallback did not return a Unix connection")
	}
	file, err := unixConnection.File()
	_ = unixConnection.Close()
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (l NativeLifecycle) Start(ctx context.Context, node state.NodeState) (_ process.Identity, returnErr error) {
	bundle, err := openPrivateNodeShares(l.Deployment, l.Shares, node)
	if err != nil {
		return process.Identity{}, err
	}
	defer func() {
		if err := bundle.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close inherited host-share files: %w", err))
		}
	}()

	shareFiles := bundle.Files()
	extraFiles := make([]*os.File, 0, len(shareFiles)+1)
	if node.Invocation.UsesPrivateFD3() {
		file, err := l.privateNetworkFile()
		if err != nil {
			return process.Identity{}, err
		}
		defer func() {
			if err := file.Close(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close inherited private-network file: %w", err))
			}
		}()
		extraFiles = append(extraFiles, file)
	}
	extraFiles = append(extraFiles, shareFiles...)

	var identityValue process.Identity
	if len(extraFiles) == 0 {
		identityValue, err = l.VM.Start(ctx, node.Invocation, node.Runtime.QMP, node.Runtime.PIDFile, node.Node, node.VMUUID)
	} else {
		identityValue, err = l.VM.StartWithExtraFiles(ctx, node.Invocation, node.Runtime.QMP, node.Runtime.PIDFile, node.Node, node.VMUUID, extraFiles)
	}
	if err != nil {
		return process.Identity{}, err
	}
	return identityValue, nil
}

func (l NativeLifecycle) AbortStart(ctx context.Context, node state.NodeState, identity process.Identity) error {
	if err := l.VM.AbortIdentity(ctx, node.Runtime.QMP, node.Node, node.VMUUID, identity, node.Invocation); err != nil {
		return err
	}
	return cleanupRuntime(node)
}

func (l NativeLifecycle) WaitReady(ctx context.Context, node state.NodeState, timeout time.Duration) ([]state.GuestWarning, error) {
	l.VM.HostKeyAlias = vm.HostKeyAlias(node.VMUUID)
	if l.RetryGuestSetup {
		l.VM.GuestSetupVersion = cloudinit.SetupVersion
	}
	expected := vm.ReadyMarker{Node: node.Node, Generation: node.Generation, SpecHash: node.SpecHash}
	warnings, err := l.VM.WaitReady(ctx, l.SSHPath, l.PrivateKey, l.KnownHosts, node.SSHPort, expected, timeout)
	if !l.RetryGuestSetup {
		return warnings, err
	}
	var bootstrap *vm.BootstrapError
	if err != nil && !errors.As(err, &bootstrap) {
		return nil, err
	}
	notices := make([]state.GuestWarning, 0)
	for _, warning := range warnings {
		if warning.Stage == "disk-reset" {
			notices = append(notices, warning)
		}
	}
	stages := make([]string, 0)
	updateAll := err != nil
	for _, warning := range warnings {
		switch warning.Stage {
		case "setup-update":
			updateAll = true
		case "data-disks", "shares", "hosts", "control-ssh", "private-network":
			if !slices.Contains(stages, warning.Stage) {
				stages = append(stages, warning.Stage)
			}
		}
	}
	if !updateAll && !slices.Contains(stages, "data-disks") && len(node.DataDisks) != 0 {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		args := vm.SSHArgsForInstance(l.VM.SSHUser, l.PrivateKey, l.KnownHosts, l.VM.HostKeyAlias, node.SSHPort,
			"sudo", "-n", "/usr/local/libexec/farrow-init-disks", "--check")
		_, checkErr := l.VM.Runner.Run(checkCtx, l.SSHPath, args...)
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if checkErr != nil {
			stages = append(stages, "data-disks")
		}
	}
	control := false
	for _, definition := range l.Resolved.Nodes {
		if definition.Name == node.Node {
			control = definition.Control
			break
		}
	}
	if control && !updateAll && !slices.Contains(stages, "control-ssh") {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		args := vm.SSHArgsForInstance(l.VM.SSHUser, l.PrivateKey, l.KnownHosts, l.VM.HostKeyAlias, node.SSHPort,
			"sudo", "-n", "/usr/local/libexec/farrow-install-control-ssh")
		_, checkErr := l.VM.Runner.Run(checkCtx, l.SSHPath, args...)
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if checkErr != nil {
			stages = append(stages, "control-ssh")
		}
	}
	if !updateAll && len(stages) == 0 && len(warnings) == 0 {
		return warnings, nil
	}
	if updateAll {
		stages = nil
	} else {
		stages = append(stages, "ready")
	}
	script, err := l.guestSetupScript(node, stages)
	if err != nil {
		return nil, err
	}
	setupCtx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	args := vm.SSHArgsForInstance(l.VM.SSHUser, l.PrivateKey, l.KnownHosts, l.VM.HostKeyAlias, node.SSHPort,
		"sudo", "-n", "bash", "-c", script)
	if _, err := guestSetupRunner(l.VM.Runner).Run(setupCtx, l.SSHPath, args...); err != nil {
		if setupCtx.Err() != nil {
			err = setupCtx.Err()
		}
		return nil, guestSetupError(err)
	}
	warnings, err = l.VM.WaitReady(ctx, l.SSHPath, l.PrivateKey, l.KnownHosts, node.SSHPort, expected, 5*time.Second)
	return append(warnings, notices...), err
}

type StartConfig struct {
	Deployment   Deployment
	Lifecycle    NodeLifecycle
	Nodes        []string
	Concurrency  int
	ReadyTimeout time.Duration
	NoWait       bool
	SetupRuntime func(string) error
	Now          func() time.Time
	Progress     activity.Reporter
}

type StartOutcome struct {
	Warnings         []state.GuestWarning `json:"warnings,omitempty"`
	Repairs          []string             `json:"repairs,omitempty"`
	BootstrapFailed  bool                 `json:"bootstrap_failed,omitempty"`
	SetupFailed      bool                 `json:"setup_failed,omitempty"`
	ReadinessSkipped bool                 `json:"readiness_skipped,omitempty"`
	Node             string               `json:"node"`
	Running          bool                 `json:"running"`
	Ready            bool                 `json:"ready"`
	Error            string               `json:"error,omitempty"`
	Reason           string               `json:"reason,omitempty"`
}

func (config StartConfig) now() time.Time {
	if config.Now != nil {
		return config.Now().UTC()
	}
	return time.Now().UTC()
}

func setupRuntime(path string) error {
	if err := runtimepath.Ensure(path, os.Getuid()); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return errors.New("runtime directory is unsafe")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("runtime directory is not empty; run farrow status to converge it, then retry")
	}
	return nil
}

func loadNodes(store state.Store, names []string) ([]state.NodeState, error) {
	if len(names) == 0 {
		return nil, errors.New("start requires at least one node")
	}
	result := make([]state.NodeState, 0, len(names))
	seen := make(map[string]struct{})
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("start repeats node %s", name)
		}
		seen[name] = struct{}{}
		node, err := store.ReadNode(name)
		if err != nil {
			return nil, err
		}
		// A running node is accepted so a later start or up can recheck guest
		// readiness without restarting QEMU.
		if node.Phase == state.Running {
			if node.Process.PID <= 0 {
				return nil, fmt.Errorf("node %s is marked running without a process", name)
			}
			result = append(result, node)
			continue
		}
		if node.Phase != state.Prepared && node.Phase != state.Stopped {
			return nil, fmt.Errorf("node %s phase %s is not startable", name, node.Phase)
		}
		if node.Process != (state.ProcessIdentity{}) {
			return nil, fmt.Errorf("node %s has a recorded process identity", name)
		}
		result = append(result, node)
	}
	return result, nil
}

func writeNodes(store state.Store, nodes []state.NodeState) error {
	for _, node := range nodes {
		if err := store.WriteNode(node); err != nil {
			return err
		}
	}
	return nil
}

func StartPrepared(ctx context.Context, config StartConfig) ([]StartOutcome, error) {
	if config.Deployment.Root == "" || config.Lifecycle == nil {
		return nil, errors.New("start deployment or lifecycle is incomplete")
	}
	if config.Concurrency <= 0 {
		config.Concurrency = 4
	}
	if config.Concurrency > 16 {
		config.Concurrency = 16
	}
	if config.ReadyTimeout <= 0 {
		config.ReadyTimeout = 180 * time.Second
	}
	if config.SetupRuntime == nil {
		config.SetupRuntime = setupRuntime
	}
	store := state.Store{Root: config.Deployment.Root}
	nodes, err := loadNodes(store, config.Nodes)
	if err != nil {
		return nil, err
	}
	outcomes := make([]StartOutcome, len(nodes))
	portProbe, recoverPorts := config.Lifecycle.(interface {
		PortAvailable(string, uint16) bool
	})
	var reserved map[uint16]struct{}
	if recoverPorts {
		reserved, err = reservedNodePorts(store)
		if err != nil {
			return nil, err
		}
	}
	preflight, hasPreflight := config.Lifecycle.(interface {
		PreflightStart(state.NodeState) error
	})
	starting := make([]state.NodeState, 0, len(nodes))
	for index, node := range nodes {
		outcomes[index].Node = node.Node
		if node.Phase == state.Running {
			continue
		}
		if hasPreflight {
			if err := preflight.PreflightStart(node); err != nil {
				outcomes[index].Error = fmt.Sprintf("preflight node %s before start: %v", node.Node, err)
				outcomes[index].Reason = failureReason(err)
				continue
			}
		} else if len(node.Invocation.ShareFiles()) != 0 {
			outcomes[index].Error = fmt.Sprintf("node %s lifecycle cannot preflight host shares", node.Node)
			continue
		}
		if recoverPorts {
			repaired, repairs, err := repairManagementPort(node, reserved, portProbe.PortAvailable)
			if err != nil {
				outcomes[index].Error = err.Error()
				continue
			}
			nodes[index] = repaired
			outcomes[index].Repairs = repairs
		}
		nodes[index].Phase = state.Starting
		nodes[index].UpdatedAt = config.now()
		starting = append(starting, nodes[index])
	}
	if err := writeNodes(store, starting); err != nil {
		return nil, err
	}
	var progressMu sync.Mutex
	reportNode := func(name, phase string) {
		progressMu.Lock()
		defer progressMu.Unlock()
		config.Progress.Report(activity.Event{Phase: "guest-ready", Node: name, State: phase, Message: name + ": " + phase, Done: phase == "ready" || phase == "limited" || phase == "setup failed" || phase == "SSH pending" || phase == "failed" || phase == "started"})
	}
	for _, node := range nodes {
		reportNode(node.Node, "queued")
	}
	jobs := make(chan int)
	var wait sync.WaitGroup
	for worker := 0; worker < config.Concurrency; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				node := nodes[index]
				if outcomes[index].Error != "" {
					reportNode(node.Node, "failed")
					continue
				}
				reportNode(node.Node, "starting")
				if node.Phase == state.Running {
					outcomes[index].Running = true
				} else if err := startOne(ctx, config, store, &node); err != nil {
					outcomes[index].Error, outcomes[index].Reason = err.Error(), failureReason(err)
					reportNode(node.Node, "failed")
					continue
				}
				nodes[index] = node
				outcomes[index].Running = true
				if config.NoWait {
					outcomes[index].ReadinessSkipped = true
					reportNode(node.Node, "started")
					continue
				}
				reportNode(node.Node, "waiting for guest setup")
				warnings, err := config.Lifecycle.WaitReady(ctx, node, config.ReadyTimeout)
				if err != nil {
					outcomes[index].Error = err.Error()
					var bootstrap *vm.BootstrapError
					var setup *GuestSetupError
					outcomes[index].SetupFailed = errors.As(err, &setup)
					outcomes[index].BootstrapFailed = errors.As(err, &bootstrap)
					if outcomes[index].SetupFailed {
						reportNode(node.Node, "setup incomplete")
					} else if outcomes[index].BootstrapFailed {
						reportNode(node.Node, "setup failed")
					} else {
						reportNode(node.Node, "SSH pending")
					}
					continue
				}
				limitations := make([]state.GuestWarning, 0, len(warnings))
				for _, warning := range warnings {
					if warning.Stage == "disk-reset" {
						outcomes[index].Repairs = append(outcomes[index].Repairs, warning.Detail)
					} else {
						limitations = append(limitations, warning)
					}
				}
				warnings = limitations
				outcomes[index].Warnings = warnings
				if !slices.Equal(node.GuestWarnings, warnings) {
					if err := store.WriteGuestWarnings(node, warnings); err != nil {
						outcomes[index].Warnings = append(outcomes[index].Warnings, state.GuestWarning{Stage: "warning-state", Detail: "could not save guest warnings: " + err.Error()})
					}
				}
				outcomes[index].Ready = true
				if len(warnings) > 0 {
					reportNode(node.Node, "limited")
				} else {
					reportNode(node.Node, "ready")
				}
			}
		}()
	}
	for index := range nodes {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	return outcomes, nil
}

func statusWithReadiness(status Status, outcomes []StartOutcome) Status {
	for index := range status.Nodes {
		for _, outcome := range outcomes {
			if status.Nodes[index].Name != outcome.Node {
				continue
			}
			status.Nodes[index].Ready = outcome.Ready && status.Nodes[index].Error == "" && status.Nodes[index].State == state.Running && (status.Nodes[index].Runtime == "" || status.Nodes[index].Runtime == "running")
			status.Nodes[index].Warnings = outcome.Warnings
			status.Nodes[index].Repairs = outcome.Repairs
			if outcome.Error != "" {
				status.Nodes[index].Error = appendStatusMessage(status.Nodes[index].Error, outcome.Error)
			}
		}
	}
	return status
}

// startOne launches QEMU for a prepared or stopped node and persists its
// process identity. A persist failure stops the process again so state and
// reality never disagree.
func startOne(ctx context.Context, config StartConfig, store state.Store, node *state.NodeState) error {
	if err := config.SetupRuntime(node.Runtime.Directory); err != nil {
		return err
	}
	identityValue, err := config.Lifecycle.Start(ctx, *node)
	if err != nil {
		return err
	}
	node.Process = recordProcess(identityValue)
	node.Phase = state.Running
	node.UpdatedAt = config.now()
	if err := store.WriteNode(*node); err != nil {
		message := fmt.Sprintf("persist running state for %s: %v", node.Node, err)
		if abortErr := config.Lifecycle.AbortStart(ctx, *node, identityValue); abortErr != nil {
			message += "; compensation failed: " + abortErr.Error() + "; the QEMU process may still be running; run farrow status"
		} else {
			message += "; compensation stopped QEMU and removed its runtime files"
		}
		return errors.New(message)
	}
	return nil
}
