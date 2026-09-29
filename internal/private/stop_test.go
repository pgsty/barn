package private

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/vm"
)

type fakeStopLifecycle struct {
	mu        sync.Mutex
	active    int
	maxActive int
	fail      map[string]bool
	timeouts  []time.Duration
}

func (fake *fakeStopLifecycle) Stop(_ context.Context, node state.NodeState, timeout time.Duration) error {
	fake.mu.Lock()
	fake.active++
	fake.timeouts = append(fake.timeouts, timeout)
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
	if fake.fail[node.Node] {
		return errors.New("injected stop failure")
	}
	return nil
}

func runningStopFixture(t *testing.T) StartConfig {
	t.Helper()
	config, _ := preparedStartFixture(t)
	config.Lifecycle = &fakeNodeLifecycle{failStart: map[string]bool{}, failReady: map[string]bool{}}
	if _, err := StartPrepared(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestStopRunningParallelAndIdempotent(t *testing.T) {
	startConfig := runningStopFixture(t)
	fake := &fakeStopLifecycle{fail: map[string]bool{}}
	outcomes, err := StopRunning(context.Background(), StopConfig{
		Deployment: startConfig.Deployment, Lifecycle: fake,
		Nodes: startConfig.Nodes, Concurrency: 2, CleanupRuntime: func(state.NodeState) error { return nil },
	})
	if err != nil || len(stoppedNames(outcomes)) != 2 {
		t.Fatalf("stop outcomes=%#v err=%v", outcomes, err)
	}
	if fake.maxActive < 2 {
		t.Fatalf("nodes did not stop concurrently: max=%d", fake.maxActive)
	}
	for _, timeout := range fake.timeouts {
		if timeout != vm.GracefulGuestShutdownTimeout {
			t.Fatalf("default guest shutdown timeout=%s, want %s", timeout, vm.GracefulGuestShutdownTimeout)
		}
	}
	store := state.Store{Root: startConfig.Deployment.Root}
	for _, name := range startConfig.Nodes {
		node, err := store.ReadNode(name)
		if err != nil || node.Phase != state.Stopped || node.Process != (state.ProcessIdentity{}) {
			t.Fatalf("stopped node %s = %#v, %v", name, node, err)
		}
	}
	second, err := StopRunning(context.Background(), StopConfig{
		Deployment: startConfig.Deployment, Lifecycle: fake,
		Nodes: startConfig.Nodes, Concurrency: 2, CleanupRuntime: func(state.NodeState) error { return nil },
	})
	if err != nil || len(stoppedNames(second)) != 2 {
		t.Fatalf("idempotent stop outcomes=%#v err=%v", second, err)
	}
}

func TestStopRunningPreservesPartialFailure(t *testing.T) {
	startConfig := runningStopFixture(t)
	fake := &fakeStopLifecycle{fail: map[string]bool{"node-1": true}}
	outcomes, err := StopRunning(context.Background(), StopConfig{
		Deployment: startConfig.Deployment, Lifecycle: fake,
		Nodes: startConfig.Nodes, Concurrency: 2, CleanupRuntime: func(state.NodeState) error { return nil },
	})
	if err != nil || len(stoppedNames(outcomes)) != 1 || outcomes[1].Error == "" {
		t.Fatalf("partial stop outcomes=%#v err=%v", outcomes, err)
	}
	store := state.Store{Root: startConfig.Deployment.Root}
	meta, metaErr := store.ReadNode("meta")
	if metaErr != nil || meta.Phase != state.Stopped {
		t.Fatalf("successful stop not persisted: %#v, %v", meta, metaErr)
	}
	failed, failedErr := store.ReadNode("node-1")
	if failedErr != nil || failed.Phase != state.Stopping || failed.Process.PID == 0 {
		t.Fatalf("failed stop not retained for retry: %#v, %v", failed, failedErr)
	}
}

func TestStopRunningSelectedNodeKeepsPeer(t *testing.T) {
	startConfig := runningStopFixture(t)
	fake := &fakeStopLifecycle{fail: map[string]bool{}}
	outcomes, err := StopRunning(context.Background(), StopConfig{
		Deployment: startConfig.Deployment, Lifecycle: fake,
		Nodes: []string{"meta"}, Concurrency: 1, CleanupRuntime: func(state.NodeState) error { return nil },
	})
	if err != nil || len(outcomes) != 1 || !outcomes[0].Stopped {
		t.Fatalf("selected stop outcomes=%#v err=%v", outcomes, err)
	}
	store := state.Store{Root: startConfig.Deployment.Root}
	meta, metaErr := store.ReadNode("meta")
	peer, peerErr := store.ReadNode("node-1")
	if metaErr != nil || peerErr != nil || meta.Phase != state.Stopped || peer.Phase != state.Running {
		t.Fatalf("selected stop phases meta=%#v/%v peer=%#v/%v", meta, metaErr, peer, peerErr)
	}
}

func TestStopRunningAcceptsAlreadyStoppedPeer(t *testing.T) {
	startConfig := runningStopFixture(t)
	store := state.Store{Root: startConfig.Deployment.Root}
	peer, err := store.ReadNode("node-1")
	if err != nil {
		t.Fatal(err)
	}
	peer.Phase = state.Stopped
	peer.Process = state.ProcessIdentity{}
	peer.UpdatedAt = time.Now().UTC()
	if err := store.WriteNode(peer); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStopLifecycle{fail: map[string]bool{"node-1": true}}
	outcomes, err := StopRunning(context.Background(), StopConfig{
		Deployment: startConfig.Deployment, Lifecycle: fake,
		Nodes: startConfig.Nodes, Concurrency: 2, CleanupRuntime: func(state.NodeState) error { return nil },
	})
	if err != nil || len(stoppedNames(outcomes)) != 2 || outcomes[1].Error != "" {
		t.Fatalf("mixed stop outcomes=%#v err=%v", outcomes, err)
	}
	meta, err := store.ReadNode("meta")
	if err != nil || meta.Phase != state.Stopped || meta.Process != (state.ProcessIdentity{}) {
		t.Fatalf("running node was not stopped in mixed stop: %#v, %v", meta, err)
	}
}

func TestStatusConvergesInterruptedTransitionAndUnblocksDestroy(t *testing.T) {
	startConfig, nodes := preparedStartFixture(t)
	deploymentValue := startConfig.Deployment
	t.Setenv("BARN_HOME", deploymentValue.Root)
	store := state.Store{Root: deploymentValue.Root}
	// Simulate a CLI killed mid-stop: the node is stranded in a transitional
	// phase with no live runtime behind it.
	stranded := nodes[0]
	stranded.Phase = state.Stopping
	stranded.Process = state.ProcessIdentity{}
	stranded.UpdatedAt = stranded.UpdatedAt.Add(1)
	if err := store.WriteNode(stranded); err != nil {
		t.Fatal(err)
	}
	manager := Manager{BarnVersion: "test"}
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatalf("status over an interrupted transition failed: %v", err)
	}
	for _, node := range status.Nodes {
		if node.Name == stranded.Node && node.State != state.Stopped {
			t.Fatalf("interrupted node did not converge: %#v", node)
		}
	}
	persisted, err := store.ReadNode(stranded.Node)
	if err != nil || persisted.Phase != state.Stopped || persisted.Process.PID != 0 {
		t.Fatalf("converged state = %#v err=%v", persisted, err)
	}
	writeDestroyKeyFixtures(t, deploymentValue.Root)
	if _, err := (Manager{BarnVersion: "test"}).Destroy(context.Background()); err != nil {
		t.Fatalf("destroy after convergence failed: %v", err)
	}
}
