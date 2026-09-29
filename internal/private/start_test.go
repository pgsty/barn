package private

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/process"
	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/vm"
)

type fakeNodeLifecycle struct {
	mu           sync.Mutex
	active       int
	maxActive    int
	failStart    map[string]bool
	failReady    map[string]bool
	warnings     []state.GuestWarning
	waitCalls    int
	abortCalls   []string
	abortError   error
	beforeReturn func(state.NodeState)
}

func (fake *fakeNodeLifecycle) Start(_ context.Context, node state.NodeState) (process.Identity, error) {
	fake.mu.Lock()
	fake.active++
	if fake.active > fake.maxActive {
		fake.maxActive = fake.active
	}
	fake.mu.Unlock()
	defer func() {
		fake.mu.Lock()
		fake.active--
		fake.mu.Unlock()
	}()
	time.Sleep(10 * time.Millisecond)
	if fake.failStart[node.Node] {
		return process.Identity{}, errors.New("injected QEMU start failure")
	}
	if fake.beforeReturn != nil {
		fake.beforeReturn(node)
	}
	pid := 100
	if node.Node != "meta" {
		pid++
	}
	return process.Identity{PID: pid, Executable: node.Invocation.Binary, Started: "test-start", ArgvHash: "test-argv-hash"}, nil
}

func (fake *fakeNodeLifecycle) AbortStart(_ context.Context, node state.NodeState, _ process.Identity) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.abortCalls = append(fake.abortCalls, node.Node)
	return fake.abortError
}

func (fake *fakeNodeLifecycle) WaitReady(_ context.Context, node state.NodeState, _ time.Duration) ([]state.GuestWarning, error) {
	fake.mu.Lock()
	fake.waitCalls++
	fake.mu.Unlock()
	if fake.failReady[node.Node] {
		return nil, errors.New("injected readiness failure")
	}
	return fake.warnings, nil
}

func TestStartPreparedNoWaitStopsAtVerifiedProcessStart(t *testing.T) {
	config, _ := preparedStartFixture(t)
	config.NoWait = true
	fake := &fakeNodeLifecycle{failStart: map[string]bool{}, failReady: map[string]bool{"meta": true, "node-1": true}}
	config.Lifecycle = fake
	outcomes, err := StartPrepared(context.Background(), config)
	if err != nil || len(readyNames(outcomes)) != 0 || len(runningNames(outcomes)) != 2 || !outcomes[0].ReadinessSkipped || len(startFailures(outcomes)) != 0 {
		t.Fatalf("no-wait outcomes=%#v err=%v", outcomes, err)
	}
	fake.mu.Lock()
	waitCalls := fake.waitCalls
	fake.mu.Unlock()
	if waitCalls != 0 {
		t.Fatalf("no-wait invoked guest readiness %d time(s)", waitCalls)
	}
}

func preparedStartFixture(t *testing.T) (StartConfig, []state.NodeState) {
	t.Helper()
	deploymentValue, prepareConfig, outcomes := commitFixture(t, &fakePrivateDisks{})
	committed, err := CommitPrepared(deploymentValue, prepareConfig, outcomes, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range committed.Nodes {
		if err := FinalizePrepared(deploymentValue, node.Node); err != nil {
			t.Fatal(err)
		}
	}
	return StartConfig{
		Deployment: deploymentValue, Nodes: []string{"meta", "node-1"},
		Concurrency: 2, ReadyTimeout: time.Second, SetupRuntime: func(string) error { return nil },
	}, committed.Nodes
}

func TestStartPreparedParallelSuccess(t *testing.T) {
	config, _ := preparedStartFixture(t)
	fake := &fakeNodeLifecycle{failStart: map[string]bool{}, failReady: map[string]bool{}}
	config.Lifecycle = fake
	outcomes, err := StartPrepared(context.Background(), config)
	if err != nil || len(readyNames(outcomes)) != 2 || len(runningNames(outcomes)) != 2 {
		t.Fatalf("start outcomes=%#v err=%v", outcomes, err)
	}
	if fake.maxActive < 2 {
		t.Fatalf("nodes did not start concurrently: max=%d", fake.maxActive)
	}
	store := state.Store{Root: config.Deployment.Root}
	for _, name := range config.Nodes {
		node, err := store.ReadNode(name)
		if err != nil || node.Phase != state.Running || node.Process.PID == 0 {
			t.Fatalf("running state %s = %#v, %v", name, node, err)
		}
	}
}

func TestStartPreparedPreservesRunningPeerOnReadinessFailure(t *testing.T) {
	config, _ := preparedStartFixture(t)
	fake := &fakeNodeLifecycle{failStart: map[string]bool{}, failReady: map[string]bool{"node-1": true}}
	config.Lifecycle = fake
	outcomes, err := StartPrepared(context.Background(), config)
	if err != nil || len(runningNames(outcomes)) != 2 || len(readyNames(outcomes)) != 1 || outcomes[1].Error == "" {
		t.Fatalf("partial readiness outcomes=%#v err=%v", outcomes, err)
	}
	store := state.Store{Root: config.Deployment.Root}
	for _, name := range config.Nodes {
		node, err := store.ReadNode(name)
		if err != nil || node.Phase != state.Running {
			t.Fatalf("readiness failure incorrectly stopped peer/node %s: %#v, %v", name, node, err)
		}
	}
}

func TestStartPreparedRechecksRunningNodesWithoutRestart(t *testing.T) {
	config, _ := preparedStartFixture(t)
	started := &fakeNodeLifecycle{failStart: map[string]bool{}, failReady: map[string]bool{}}
	config.Lifecycle = started
	config.NoWait = true
	if _, err := StartPrepared(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	// Every Start call would now fail, so a running node must only be rechecked.
	recheck := &fakeNodeLifecycle{failStart: map[string]bool{"meta": true, "node-1": true}, failReady: map[string]bool{"node-1": true}}
	config.Lifecycle = recheck
	config.NoWait = false
	outcomes, err := StartPrepared(context.Background(), config)
	if err != nil || len(runningNames(outcomes)) != 2 || len(readyNames(outcomes)) != 1 || outcomes[1].Error == "" {
		t.Fatalf("running readiness outcomes=%#v err=%v", outcomes, err)
	}
	recheck.mu.Lock()
	waitCalls := recheck.waitCalls
	recheck.mu.Unlock()
	if waitCalls != 2 {
		t.Fatalf("running readiness calls = %d, want 2", waitCalls)
	}
	store := state.Store{Root: config.Deployment.Root}
	for _, name := range config.Nodes {
		node, err := store.ReadNode(name)
		if err != nil || node.Phase != state.Running {
			t.Fatalf("recheck changed node %s state: %#v, %v", name, node, err)
		}
	}
}

func TestStartPreparedLeavesFailedStartRecorded(t *testing.T) {
	config, _ := preparedStartFixture(t)
	fake := &fakeNodeLifecycle{failStart: map[string]bool{"node-1": true}, failReady: map[string]bool{}}
	config.Lifecycle = fake
	outcomes, err := StartPrepared(context.Background(), config)
	if err != nil || len(runningNames(outcomes)) != 1 || outcomes[1].Error == "" {
		t.Fatalf("partial start outcomes=%#v err=%v", outcomes, err)
	}
	store := state.Store{Root: config.Deployment.Root}
	meta, metaErr := store.ReadNode("meta")
	if metaErr != nil || meta.Phase != state.Running {
		t.Fatalf("successful peer not running: %#v, %v", meta, metaErr)
	}
	failed, failedErr := store.ReadNode("node-1")
	if failedErr != nil || failed.Phase != state.Starting || failed.Process != (state.ProcessIdentity{}) {
		t.Fatalf("failed start not left in starting phase: %#v, %v", failed, failedErr)
	}
}

func TestStartPreparedCompensatesRunningStateWriteFailure(t *testing.T) {
	config, _ := preparedStartFixture(t)
	config.Nodes = []string{"node-1"}
	statePath := filepath.Join(config.Deployment.Root, "nodes", "node-1", "state.json")
	fake := &fakeNodeLifecycle{failStart: map[string]bool{}, failReady: map[string]bool{}}
	fake.beforeReturn = func(state.NodeState) {
		if err := os.Remove(statePath); err != nil {
			t.Errorf("remove state fixture: %v", err)
			return
		}
		if err := os.Mkdir(statePath, 0o700); err != nil {
			t.Errorf("replace state with directory: %v", err)
		}
	}
	config.Lifecycle = fake
	outcomes, err := StartPrepared(context.Background(), config)
	if err != nil || len(outcomes) != 1 || outcomes[0].Error == "" {
		t.Fatalf("write-failure outcomes=%#v err=%v", outcomes, err)
	}
	if !strings.Contains(outcomes[0].Error, "persist running state for node-1") || !strings.Contains(outcomes[0].Error, "compensation stopped QEMU") {
		t.Fatalf("write-failure message = %q", outcomes[0].Error)
	}
	fake.mu.Lock()
	abortCalls := append([]string(nil), fake.abortCalls...)
	fake.mu.Unlock()
	if len(abortCalls) != 1 || abortCalls[0] != "node-1" {
		t.Fatalf("abort calls = %v", abortCalls)
	}
}

type bootstrapFailureLifecycle struct{ fakeNodeLifecycle }

func (*bootstrapFailureLifecycle) WaitReady(context.Context, state.NodeState, time.Duration) ([]state.GuestWarning, error) {
	return nil, &vm.BootstrapError{Stage: "data-disks", ExitStatus: 1, Detail: "fixture disk failure"}
}
func TestBootstrapFailureKeepsRunningGuestAndRecoveryClassification(t *testing.T) {
	config, _ := preparedStartFixture(t)
	config.Lifecycle = &bootstrapFailureLifecycle{}
	outcomes, err := StartPrepared(context.Background(), config)
	if err != nil || len(outcomes) != 2 {
		t.Fatalf("outcomes=%+v, err=%v", outcomes, err)
	}
	for _, outcome := range outcomes {
		if !outcome.Running || outcome.Ready || !outcome.BootstrapFailed {
			t.Fatalf("lost usable guest or failure classification: %+v", outcome)
		}
	}
	for _, failure := range startFailures(outcomes) {
		if failure.Stage != "bootstrap" {
			t.Fatalf("wrong recovery stage: %+v", failure)
		}
	}
}
