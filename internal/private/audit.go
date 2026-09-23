package private

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/farrow/internal/execx"
	"github.com/pgsty/farrow/internal/process"
	"github.com/pgsty/farrow/internal/qmp"
	"github.com/pgsty/farrow/internal/runtimepath"
	"github.com/pgsty/farrow/internal/state"
)

// Observation is one liveness verdict about a node's recorded runtime.
type Observation struct {
	Node      string `json:"node"`
	Live      bool   `json:"live"`
	Authority string `json:"authority"`
	Evidence  string `json:"evidence"`
}

// RuntimeAuditor proves whether a node's recorded runtime is live or dead.
// Ambiguity is an error, never a guess.
type RuntimeAuditor func(context.Context, state.NodeState) (Observation, error)

func completeProcess(value state.ProcessIdentity) bool {
	return value.PID > 0 && value.Executable != "" && value.Started != "" && value.ArgvHash != ""
}

func recordProcess(value process.Identity) state.ProcessIdentity {
	return state.ProcessIdentity{PID: value.PID, Executable: value.Executable, Started: value.Started, ArgvHash: value.ArgvHash}
}

// observeProcess classifies whatever holds pid now against the node's
// recorded QEMU. Any other PID (a pidfile's) has no recorded start time, so
// only a non-QEMU executable proves it foreign.
func observeProcess(ctx context.Context, runner execx.Runner, node state.NodeState, pid int) process.Verdict {
	if pid != node.Process.PID {
		return process.Observe(ctx, runner, process.Identity{PID: pid, Executable: node.Invocation.Binary}, node.Invocation)
	}
	recorded := process.Identity{PID: pid, Executable: node.Process.Executable, Started: node.Process.Started, ArgvHash: node.Process.ArgvHash}
	return process.Observe(ctx, runner, recorded, node.Invocation)
}

func captureRuntimeProcess(ctx context.Context, runner execx.Runner, node state.NodeState) (process.Identity, error) {
	if runner == nil {
		return process.Identity{}, errors.New("runtime process capture requires a command runner")
	}
	if err := runtimepath.Validate(node.Runtime.Directory, node.Node, os.Getuid()); err != nil {
		return process.Identity{}, err
	}
	if node.Runtime.QMP != filepath.Join(node.Runtime.Directory, "qmp.sock") || node.Runtime.PIDFile != filepath.Join(node.Runtime.Directory, "qemu.pid") {
		return process.Identity{}, errors.New("runtime process paths do not match the node directory")
	}
	info, err := os.Lstat(node.Runtime.PIDFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > 32 {
		return process.Identity{}, errors.New("runtime pidfile is missing or unsafe")
	}
	data, err := os.ReadFile(node.Runtime.PIDFile)
	if err != nil {
		return process.Identity{}, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 || !process.Alive(pid) {
		return process.Identity{}, errors.New("runtime pidfile does not identify a live process")
	}
	identity, err := process.Capture(ctx, runner, node.Invocation, pid)
	if err != nil {
		return process.Identity{}, err
	}
	if identity.ArgvHash != process.ExpectedArgvHash(node.Invocation) {
		return process.Identity{}, errors.New("runtime process command does not match the persisted invocation")
	}
	second, err := process.Capture(ctx, runner, node.Invocation, pid)
	if err != nil || second != identity {
		return process.Identity{}, errors.New("runtime process identity changed during capture")
	}
	return identity, nil
}

// RuntimeIdentityAuditor audits by authority order: matching QMP name+UUID,
// then the full process identity tuple, then the pidfile. A live-but-unproven
// runtime is an error.
func RuntimeIdentityAuditor(runner execx.Runner, timeout time.Duration) RuntimeAuditor {
	if timeout <= 0 {
		timeout = time.Second
	}
	return func(ctx context.Context, node state.NodeState) (Observation, error) {
		if runner == nil {
			return Observation{}, errors.New("runtime identity auditor requires a command runner")
		}
		if node.Runtime.QMP == "" {
			return Observation{Node: node.Node, Authority: "dead", Evidence: "node has no runtime paths"}, nil
		}
		qmpExists := false
		if info, err := os.Lstat(node.Runtime.QMP); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
				return Observation{}, fmt.Errorf("node %s QMP path is not an owned Unix socket", node.Node)
			}
			qmpExists = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return Observation{}, err
		}
		if qmpExists {
			client := &qmp.Client{Timeout: timeout}
			actualName, nameErr := client.QueryName(ctx, node.Runtime.QMP)
			if nameErr == nil {
				actualUUID, uuidErr := client.QueryUUID(ctx, node.Runtime.QMP)
				if uuidErr != nil {
					return Observation{}, fmt.Errorf("node %s QMP name responded but UUID failed: %w", node.Node, uuidErr)
				}
				if actualName.Name != node.Node || !strings.EqualFold(actualUUID.UUID, node.VMUUID) {
					return Observation{}, fmt.Errorf("node %s QMP identity mismatch: name=%q uuid=%q", node.Node, actualName.Name, actualUUID.UUID)
				}
				return Observation{Node: node.Node, Live: true, Authority: "qmp", Evidence: "matching QMP name and VM UUID"}, nil
			}
		}
		recorded := process.Identity{PID: node.Process.PID, Executable: node.Process.Executable, Started: node.Process.Started, ArgvHash: node.Process.ArgvHash}
		if completeProcess(node.Process) && process.MatchesLive(ctx, runner, recorded, node.Invocation) {
			return Observation{Node: node.Node, Live: true, Authority: "process", Evidence: "matching executable, start time, and argv identity"}, nil
		}
		processEvidence := "recorded process identity is dead"
		if node.Process.PID > 0 {
			switch observeProcess(ctx, runner, node, node.Process.PID) {
			case process.Unknown, process.Ours:
				return Observation{}, fmt.Errorf("node %s recorded PID %d is alive but its identity cannot be verified", node.Node, node.Process.PID)
			case process.Foreign:
				processEvidence = fmt.Sprintf("recorded PID %d now belongs to another process", node.Process.PID)
			}
		}
		pidEvidence := "pidfile absent"
		if info, err := os.Lstat(node.Runtime.PIDFile); err == nil {
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 32 {
				return Observation{}, fmt.Errorf("node %s pidfile is unsafe", node.Node)
			}
			data, err := os.ReadFile(node.Runtime.PIDFile)
			if err != nil {
				return Observation{}, err
			}
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil || pid <= 0 {
				return Observation{}, fmt.Errorf("node %s pidfile is malformed", node.Node)
			}
			switch observeProcess(ctx, runner, node, pid) {
			case process.Dead:
				pidEvidence = fmt.Sprintf("pidfile PID %d is dead", pid)
			case process.Foreign:
				pidEvidence = fmt.Sprintf("pidfile PID %d now belongs to another process", pid)
			default:
				return Observation{}, fmt.Errorf("node %s pidfile references unverified live PID %d", node.Node, pid)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return Observation{}, err
		}
		qmpEvidence := "QMP socket absent"
		if qmpExists {
			qmpEvidence = "QMP socket stale/unresponsive"
		}
		return Observation{Node: node.Node, Authority: "dead", Evidence: qmpEvidence + "; " + pidEvidence + "; " + processEvidence}, nil
	}
}
