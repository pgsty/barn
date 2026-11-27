package linuxvm

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/lock"
	"github.com/pgsty/barn/internal/process"
	"github.com/pgsty/barn/internal/qmp"
	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/vm"
)

type NodeStatus struct {
	Warnings     []state.GuestWarning `json:"warnings,omitempty"`
	Repairs      []string             `json:"repairs,omitempty"`
	hostKeyAlias string               // carries the identity checked in this snapshot into direct SSH
	Ready        bool                 `json:"ready,omitempty"`
	Error        string               `json:"error,omitempty"`
	Image        string               `json:"image,omitempty"`
	CPUs         int                  `json:"cpus,omitempty"`
	Memory       int64                `json:"memory_bytes,omitempty"`
	RootDisk     int64                `json:"root_disk_bytes,omitempty"`
	Name         string               `json:"name"`
	Address      string               `json:"address"`
	State        state.Phase          `json:"state"`
	Runtime      string               `json:"runtime"`
	GuestArch    string               `json:"guest_arch,omitempty"`
	Accel        string               `json:"accelerator,omitempty"`
	SSHHost      string               `json:"ssh_host"`
	SSHPort      uint16               `json:"ssh_port"`
	ProcessID    int                  `json:"pid,omitempty"`
}

type Status struct {
	OperationID string       `json:"operation_id,omitempty"`
	SpecHash    string       `json:"spec_hash"`
	Nodes       []NodeStatus `json:"nodes"`
	Message     string       `json:"message,omitempty"`
	// Note carries context that must remain visible alongside a status or progress table.
	Note string `json:"note,omitempty"`
}

func invocationOption(arguments []string, name string) string {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}
	return ""
}

func invocationGuestArch(binary string, arguments []string) string {
	switch filepath.Base(binary) {
	case "qemu-system-aarch64":
		return "arm64"
	case "qemu-system-x86_64":
		return "amd64"
	}
	machine := strings.SplitN(invocationOption(arguments, "-machine"), ",", 2)[0]
	switch machine {
	case "virt":
		return "arm64"
	case "q35":
		return "amd64"
	default:
		return ""
	}
}

// interruptedPhaseError names the command that finishes a transition an
// interrupted command left behind, once status could not settle it.
func interruptedPhaseError(node state.NodeState) error {
	next := "barn stop " + node.Node
	if node.Phase == state.Destroying {
		next = "barn destroy " + node.Node
	}
	return failure.New(failure.Conflict, fmt.Errorf("node %s is still %s after an interrupted command", node.Node, node.Phase)).Because("interrupted_transition").Then(next)
}

func appendStatusMessage(current, addition string) string {
	current = strings.TrimSpace(current)
	addition = strings.TrimSpace(addition)
	if current == "" {
		return addition
	}
	if addition == "" {
		return current
	}
	return current + "; " + addition
}

func (m Manager) statusForLocked(ctx context.Context, deploymentValue Deployment, message string) (Status, error) {
	return m.observeStatus(ctx, deploymentValue, message, true)
}

// observeStatus audits every selected node. With converge it also publishes
// what the audit proved (stopped runtimes, adopted starts); without it, it
// only reads, for a command that runs while another one holds the lock.
func (m Manager) observeStatus(ctx context.Context, deploymentValue Deployment, message string, converge bool) (Status, error) {
	store := state.Store{Root: deploymentValue.Root}
	deploymentState, err := store.ReadDeployment()
	if err != nil {
		return Status{}, err
	}
	selected, err := selectedNodeNames(deploymentState.Resolved, m.Nodes)
	if err != nil {
		return Status{}, err
	}
	selectedSet := nodeNameSet(selected)
	result := Status{OperationID: m.OperationID, SpecHash: deploymentState.SpecHash, Message: message, Nodes: make([]NodeStatus, 0, len(selected))}
	lifecycle := vm.Lifecycle{Runner: m.runner(), QMP: &qmp.Client{Timeout: 5 * time.Second}, SSHUser: deploymentState.Resolved.SSHUser}
	convergenceCandidates := make([]state.NodeState, 0)
	readErrors := make(map[string]error)
	failures := make([]NodeFailure, 0)
	nodes := make(map[string]state.NodeState, len(selected))
	for _, definition := range deploymentState.Resolved.Nodes {
		if _, include := selectedSet[definition.Name]; !include {
			continue
		}
		node, err := store.ReadNode(definition.Name)
		if missingPath(err) {
			continue
		}
		if err != nil {
			readErrors[definition.Name] = err
			continue
		}
		nodes[node.Node] = node
	}
	for _, definition := range deploymentState.Resolved.Nodes {
		node, include := nodes[definition.Name]
		if !include {
			if _, isSelected := selectedSet[definition.Name]; isSelected {
				row := NodeStatus{Name: definition.Name, Address: definition.Address, State: state.Absent, Runtime: "absent", Image: definition.Image, CPUs: definition.CPUs, Memory: definition.Memory, RootDisk: definition.RootDisk}
				if readErr := readErrors[definition.Name]; readErr != nil {
					row.State, row.Runtime, row.Error = "unknown", "unknown", readErr.Error()
					failures = append(failures, NodeFailure{Node: definition.Name, Stage: "status", Error: row.Error})
				}
				result.Nodes = append(result.Nodes, row)
			}
			continue
		}
		runtimeState := "inactive"
		var converged bool
		var nodeErr error
		switch {
		case node.Phase == state.Running:
			runtimeState, converged, nodeErr = m.auditRunningNode(ctx, lifecycle, &node, converge)
		case node.Phase == state.Stopping || node.Phase == state.Starting || node.Phase == state.Destroying:
			if !converge {
				// The command holding the lock owns this transition.
				runtimeState = "unknown"
			} else {
				runtimeState, converged, nodeErr = m.auditInterruptedNode(ctx, &node, &result.Message)
			}
		}
		if converged {
			convergenceCandidates = append(convergenceCandidates, node)
		}
		rowMessage := ""
		stage, reason := "status", ""
		if nodeErr != nil {
			runtimeState, rowMessage = "unknown", nodeErr.Error()
		}
		if node.ResizeIncomplete {
			resizeMessage := "online resize did not finish; live CPU and memory may differ from the applied configuration; retry barn resize or run barn restart " + node.Node
			rowMessage = appendStatusMessage(rowMessage, resizeMessage)
			if nodeErr == nil {
				stage, reason = "resize", "resize_incomplete"
			}
		}
		if node.RootResize != nil {
			node.GuestWarnings = append(node.GuestWarnings, state.GuestWarning{Stage: "root-resize", Detail: fmt.Sprintf("root growth to %d bytes is pending (%s); retry barn resize -D %dB %s", node.RootResize.Target, node.RootResize.Stage, node.RootResize.Target, node.Node)})
		}
		if rowMessage != "" {
			failures = append(failures, NodeFailure{Node: node.Node, Stage: stage, Error: rowMessage, Reason: reason})
		}
		result.Nodes = append(result.Nodes, NodeStatus{
			Warnings:     node.GuestWarnings,
			hostKeyAlias: vm.HostKeyAlias(node.VMUUID),
			Name:         node.Node, Address: definition.Address, State: node.Phase, Runtime: runtimeState, Error: rowMessage,
			Image: node.Image.Alias + "@" + node.Image.Release, CPUs: definition.CPUs, Memory: definition.Memory, RootDisk: definition.RootDisk,
			GuestArch: invocationGuestArch(node.Invocation.Binary, node.Invocation.Args), Accel: invocationOption(node.Invocation.Args, "-accel"),
			SSHHost: "127.0.0.1", SSHPort: node.SSHPort, ProcessID: node.Process.PID,
		})
	}
	if !converge {
		convergenceCandidates = nil
	}
	for _, node := range convergenceCandidates {
		if err := store.WriteNode(node); err != nil {
			failures = append(failures, NodeFailure{Node: node.Node, Stage: "status", Error: err.Error()})
			for index := range result.Nodes {
				if result.Nodes[index].Name == node.Node {
					result.Nodes[index].Error = err.Error()
				}
			}
		}
	}
	if len(failures) != 0 {
		return result, newPartialError(failures, len(selected))
	}
	return result, nil
}

func (m Manager) auditRunningNode(ctx context.Context, lifecycle vm.Lifecycle, node *state.NodeState, converge bool) (string, bool, error) {
	identity := process.Identity{PID: node.Process.PID, Executable: node.Process.Executable, Started: node.Process.Started, ArgvHash: node.Process.ArgvHash}
	qmpErr := lifecycle.ValidateIdentity(ctx, node.Runtime.QMP, node.Node, node.VMUUID)
	if err := ctx.Err(); err != nil {
		return "inactive", false, err
	}
	processMatches := process.MatchesLive(ctx, m.runner(), identity, node.Invocation)
	if err := ctx.Err(); err != nil {
		return "inactive", false, err
	}
	switch {
	case qmpErr == nil && processMatches:
		return "running", false, nil
	case qmpErr == nil:
		return "inactive", false, fmt.Errorf("node %s has matching QMP but its recorded process identity does not match; recreate --force it", node.Node)
	case errors.Is(qmpErr, vm.ErrQMPIdentityMismatch):
		return "inactive", false, fmt.Errorf("node %s has mismatched QMP identity; recreate --force it: %w", node.Node, qmpErr)
	case processMatches:
		return "inactive", false, failure.WithNext(fmt.Errorf("node %s QEMU is running but its QMP socket does not answer", node.Node), "barn stop "+node.Node+" (it stops the verified process by signal)")
	case node.Runtime.Directory == "" || node.Runtime.QMP == "" || node.Runtime.PIDFile == "":
		return "inactive", false, fmt.Errorf("node %s is recorded running with incomplete runtime identity; recreate is required", node.Node)
	case !completeProcess(node.Process):
		return "inactive", false, fmt.Errorf("node %s is recorded running with incomplete process identity; recreate is required", node.Node)
	case observeProcess(ctx, m.runner(), *node, node.Process.PID) == process.Unknown:
		// A reused PID (after a reboot, or another exit) is proven
		// foreign and converges below; only an unreadable one blocks.
		return "inactive", false, fmt.Errorf("node %s recorded PID %d is alive but its identity cannot be read; Barn will not treat it as stopped", node.Node, node.Process.PID)
	default:
		// ValidateIdentity alone cannot distinguish a wholly stale QMP
		// socket from a live endpoint whose name responded but UUID query
		// failed. Require the stricter runtime auditor to prove the whole
		// QMP/process/pidfile identity dead before converging.
		observation, auditErr := RuntimeIdentityAuditor(m.runner(), time.Second)(ctx, *node)
		if auditErr != nil {
			return "inactive", false, fmt.Errorf("node %s runtime death audit is inconclusive: %w", node.Node, auditErr)
		}
		if observation.Live || observation.Authority != "dead" {
			return "inactive", false, fmt.Errorf("node %s runtime became live during death audit: %s", node.Node, observation.Evidence)
		}
		// This is the safe self-halt case. The death audit proved both QMP
		// and every recorded PID dead, so remove the bounded runtime residue
		// before publishing a startable stopped state.
		if !converge {
			node.Phase = state.Stopped
			node.ResizeIncomplete = false
			return "inactive", false, nil
		}
		if err := cleanupRuntime(*node); err != nil {
			return "inactive", false, fmt.Errorf("clean dead private runtime for %s: %w", node.Node, err)
		}
		node.Phase = state.Stopped
		node.ResizeIncomplete = false
		node.Process = state.ProcessIdentity{}
		node.UpdatedAt = time.Now().UTC()
		return "inactive", true, nil
	}
}

// auditInterruptedNode settles a node left in starting/stopping/destroying by
// a killed CLI. A QMP-bound starting process is adopted; other live transitions
// resume only with complete process identity; proven-dead runtimes are cleaned.
func (m Manager) auditInterruptedNode(ctx context.Context, node *state.NodeState, resultMessage *string) (string, bool, error) {
	observation, auditErr := RuntimeIdentityAuditor(m.runner(), time.Second)(ctx, *node)
	if auditErr != nil {
		return "inactive", false, fmt.Errorf("node %s was interrupted mid-%s and its runtime death audit is inconclusive: %w", node.Node, node.Phase, auditErr)
	}
	switch {
	case observation.Live && node.Phase == state.Starting && !completeProcess(node.Process):
		if observation.Authority != "qmp" {
			return "inactive", false, fmt.Errorf("node %s interrupted start lacks matching QMP authority", node.Node)
		}
		identityValue, captureErr := captureRuntimeProcess(ctx, m.runner(), *node)
		if captureErr != nil {
			return "inactive", false, fmt.Errorf("adopt interrupted private start for %s: %w", node.Node, captureErr)
		}
		node.Process = recordProcess(identityValue)
		node.Phase = state.Running
		node.UpdatedAt = time.Now().UTC()
		*resultMessage = appendStatusMessage(*resultMessage, fmt.Sprintf("adopted interrupted start for %s (pid %d)", node.Node, identityValue.PID))
		return "running", true, nil
	case observation.Live && node.Phase != state.Destroying && completeProcess(node.Process):
		// A stop or start interrupted while QEMU kept running: the
		// verified process is simply running again.
		node.Phase = state.Running
		node.UpdatedAt = time.Now().UTC()
		*resultMessage = appendStatusMessage(*resultMessage, "resumed "+node.Node+" as running after an interrupted command")
		return "running", true, nil
	case observation.Live:
		return "running", false, nil
	default:
		if node.Runtime.Directory != "" {
			if err := cleanupRuntime(*node); err != nil {
				return "inactive", false, fmt.Errorf("clean dead private runtime for %s: %w", node.Node, err)
			}
		}
		if node.Phase != state.Destroying {
			node.Phase = state.Stopped
		}
		node.Process = state.ProcessIdentity{}
		node.ResizeIncomplete = false
		node.UpdatedAt = time.Now().UTC()
		if node.Phase == state.Destroying {
			*resultMessage = appendStatusMessage(*resultMessage, fmt.Sprintf("destroy remains pending for %s; rerun barn destroy", node.Node))
		}
		return "inactive", true, nil
	}
}

func (m Manager) statusFor(ctx context.Context, deploymentValue Deployment, message string) (_ Status, returnErr error) {
	deploymentLock, err := acquireDeploymentLock(ctx, deploymentValue.Root, false, m.Progress)
	if err != nil {
		return Status{}, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, deploymentLock, "deployment status lock")
	}()
	return m.statusForLocked(ctx, deploymentValue, message)
}

func (m Manager) Status(ctx context.Context) (Status, error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return Status{}, err
	}
	return m.statusReadOnly(ctx, deploymentValue)
}

// statusReadOnly never queues behind another barn command: while one holds
// the deployment, it reports the recorded state without converging it.
func (m Manager) statusReadOnly(ctx context.Context, deploymentValue Deployment) (_ Status, returnErr error) {
	deploymentLock, holder, err := tryDeploymentLock(deploymentValue.Root, false)
	if err != nil {
		return Status{}, err
	}
	if deploymentLock == nil {
		status, err := m.observeStatus(ctx, deploymentValue, "", false)
		status.Note = holder + " is running; showing recorded state"
		return status, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, deploymentLock, "deployment status lock")
	}()
	return m.statusForLocked(ctx, deploymentValue, "")
}
