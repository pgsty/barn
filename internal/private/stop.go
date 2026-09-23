package private

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pgsty/farrow/internal/execx"
	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/process"
	"github.com/pgsty/farrow/internal/runtimepath"
	"github.com/pgsty/farrow/internal/state"
	"github.com/pgsty/farrow/internal/vm"
)

type StopLifecycle interface {
	Stop(context.Context, state.NodeState, time.Duration) error
}

func (l NativeLifecycle) Stop(ctx context.Context, node state.NodeState, timeout time.Duration) error {
	identityValue := process.Identity{PID: node.Process.PID, Executable: node.Process.Executable, Started: node.Process.Started, ArgvHash: node.Process.ArgvHash}
	return l.VM.Stop(ctx, node.Runtime.QMP, node.Node, node.VMUUID, identityValue, node.Invocation, timeout)
}

type StopConfig struct {
	Deployment     Deployment
	Lifecycle      StopLifecycle
	Nodes          []string
	Concurrency    int
	GuestTimeout   time.Duration
	CleanupRuntime func(state.NodeState) error
	Now            func() time.Time
}

type StopOutcome struct {
	Node    string `json:"node"`
	Stopped bool   `json:"stopped"`
	Error   string `json:"error,omitempty"`
}

func (config StopConfig) now() time.Time {
	if config.Now != nil {
		return config.Now().UTC()
	}
	return time.Now().UTC()
}

// A prepared node may share a runtime path with a different FARROW_HOME.
// Never unlink a listening socket or a pidfile that still has a live owner.
func checkRuntimeUnused(node state.NodeState) error {
	if node.Runtime.QMP != "" {
		connection, err := net.DialTimeout("unix", node.Runtime.QMP, 250*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return fmt.Errorf("node %s runtime socket is still in use: %s", node.Node, node.Runtime.QMP)
		}
	}
	if data, err := os.ReadFile(node.Runtime.PIDFile); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if verdict := observeProcess(context.Background(), execx.OSRunner{Timeout: 5 * time.Second}, node, pid); pid > 0 && verdict != process.Dead && verdict != process.Foreign {
			return fmt.Errorf("node %s runtime pidfile still refers to live process %d", node.Node, pid)
		}
	}
	return nil
}

func cleanupRuntime(node state.NodeState) error {
	if err := checkRuntimeUnused(node); err != nil {
		return err
	}
	for _, pathname := range []string{node.Runtime.QMP, node.Runtime.PIDFile} {
		info, err := os.Lstat(pathname)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("runtime artifact is unsafe: %s", pathname)
		}
		if err := os.Remove(pathname); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(node.Runtime.Directory)
	if errors.Is(err, os.ErrNotExist) {
		return runtimepath.PruneEmptyParents(node.Runtime.Directory, os.Getuid())
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("runtime directory contains unexpected artifacts")
	}
	if err := os.Remove(node.Runtime.Directory); err != nil {
		return err
	}
	return runtimepath.PruneEmptyParents(node.Runtime.Directory, os.Getuid())
}

func loadStoppableNodes(store state.Store, names []string) ([]state.NodeState, error) {
	if len(names) == 0 {
		return nil, errors.New("stop requires at least one node")
	}
	result := make([]state.NodeState, 0, len(names))
	seen := make(map[string]struct{})
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("stop repeats node %s", name)
		}
		seen[name] = struct{}{}
		node, err := store.ReadNode(name)
		if err != nil {
			return nil, err
		}
		switch node.Phase {
		case state.Running:
			if node.Process.PID <= 0 {
				return nil, fmt.Errorf("node %s is recorded running without a process", name)
			}
		case state.Stopping, state.Starting:
			// An interrupted transition: re-drive the stop when the process
			// identity is complete; a dead or identity-less transition is
			// converged by `farrow status` first.
			if !completeProcess(node.Process) {
				return nil, failure.WithNext(fmt.Errorf("node %s was interrupted while %s and its process cannot be identified", name, node.Phase), "farrow status, then retry")
			}
		case state.Stopped, state.Prepared:
			if node.Process.PID != 0 {
				return nil, fmt.Errorf("inactive private node %s retains a process identity", name)
			}
		default:
			return nil, fmt.Errorf("node %s phase %s is not stoppable", name, node.Phase)
		}
		result = append(result, node)
	}
	return result, nil
}

func StopRunning(ctx context.Context, config StopConfig) ([]StopOutcome, error) {
	if config.Deployment.Root == "" || config.Lifecycle == nil {
		return nil, errors.New("stop deployment or lifecycle is incomplete")
	}
	if config.Concurrency <= 0 {
		config.Concurrency = 4
	}
	if config.Concurrency > 16 {
		config.Concurrency = 16
	}
	if config.GuestTimeout <= 0 {
		config.GuestTimeout = vm.GracefulGuestShutdownTimeout
	}
	if config.CleanupRuntime == nil {
		config.CleanupRuntime = cleanupRuntime
	}
	store := state.Store{Root: config.Deployment.Root}
	nodes, err := loadStoppableNodes(store, config.Nodes)
	if err != nil {
		return nil, err
	}
	hasRunning := false
	for index := range nodes {
		switch nodes[index].Phase {
		case state.Running, state.Stopping, state.Starting:
			hasRunning = true
			nodes[index].Phase = state.Stopping
			nodes[index].UpdatedAt = config.now()
		case state.Prepared:
			nodes[index].Phase = state.Stopped
			nodes[index].UpdatedAt = config.now()
		}
	}
	if !hasRunning {
		outcomes := make([]StopOutcome, len(nodes))
		for index, node := range nodes {
			outcomes[index] = StopOutcome{Node: node.Node, Stopped: true}
		}
		return outcomes, nil
	}
	if err := writeNodes(store, nodes); err != nil {
		return nil, err
	}
	outcomes := make([]StopOutcome, len(nodes))
	for index, node := range nodes {
		outcomes[index].Node = node.Node
		if node.Phase == state.Stopped {
			outcomes[index].Stopped = true
		}
	}
	jobs := make(chan int)
	var wait sync.WaitGroup
	for worker := 0; worker < config.Concurrency; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				node := nodes[index]
				if err := config.Lifecycle.Stop(ctx, node, config.GuestTimeout); err != nil {
					outcomes[index].Error = err.Error()
					continue
				}
				if err := config.CleanupRuntime(node); err != nil {
					outcomes[index].Error = err.Error()
					continue
				}
				node.Phase = state.Stopped
				node.Process = state.ProcessIdentity{}
				node.UpdatedAt = config.now()
				if err := store.WriteNode(node); err != nil {
					outcomes[index].Error = err.Error()
					continue
				}
				nodes[index] = node
				outcomes[index].Stopped = true
			}
		}()
	}
	for index, node := range nodes {
		if node.Phase == state.Stopping {
			jobs <- index
		}
	}
	close(jobs)
	wait.Wait()
	return outcomes, nil
}
