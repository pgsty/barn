package private

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/state"
)

func TestBoundedConcurrency(t *testing.T) {
	t.Parallel()
	defaultParallelism := max(1, runtime.NumCPU()-2)
	for _, test := range []struct {
		nodes, parallelism, want int
	}{
		{0, 0, 1}, {1, 0, 1}, {2, 0, min(2, defaultParallelism)},
		{128, 0, min(128, defaultParallelism)},
		{20, 1, 1}, {20, 8, 8}, {20, 17, 17}, {20, 64, 20},
	} {
		if got := boundedConcurrency(test.nodes, test.parallelism); got != test.want {
			t.Errorf("nodes=%d parallelism=%d: got %d, want %d", test.nodes, test.parallelism, got, test.want)
		}
	}
}

func TestManagerConcurrencyDistinguishesAutomaticAndAllNodes(t *testing.T) {
	t.Parallel()
	defaultParallelism := max(1, runtime.NumCPU()-2)
	for _, nodes := range []int{0, 1, 2, 32, defaultParallelism + 3} {
		if got := (Manager{}).concurrency(nodes); got != min(max(1, nodes), defaultParallelism) {
			t.Errorf("automatic concurrency for %d nodes = %d", nodes, got)
		}
		for _, parallelism := range []int{0, 1, 8, 32} {
			manager := Manager{Parallelism: &parallelism}
			want := max(1, nodes)
			if parallelism != 0 {
				want = min(want, parallelism)
			}
			if got := manager.concurrency(nodes); got != want {
				t.Errorf("%d nodes, --parallel %d: got %d, want %d", nodes, parallelism, got, want)
			}
		}
	}
}

type readinessBarrier struct {
	fakeNodeLifecycle
	mu        sync.Mutex
	active    int
	maxActive int
	entered   chan string
	release   chan struct{}
}

func (barrier *readinessBarrier) WaitReady(ctx context.Context, node state.NodeState, _ time.Duration) ([]state.GuestWarning, error) {
	barrier.mu.Lock()
	barrier.active++
	barrier.maxActive = max(barrier.maxActive, barrier.active)
	barrier.mu.Unlock()
	defer func() {
		barrier.mu.Lock()
		barrier.active--
		barrier.mu.Unlock()
	}()
	barrier.entered <- node.Node
	select {
	case <-barrier.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestStartPreparedConcurrencyIncludesReadiness(t *testing.T) {
	for _, parallelism := range []int{0, 1, 3, 17, 64} {
		t.Run(fmt.Sprint(parallelism), func(t *testing.T) {
			config, nodes := preparedStartFixture(t)
			store := state.Store{Root: config.Deployment.Root}
			for index := len(config.Nodes); index < 18; index++ {
				node := nodes[0]
				node.Node = fmt.Sprintf("node-%d", index)
				if err := store.WriteNode(node); err != nil {
					t.Fatal(err)
				}
				config.Nodes = append(config.Nodes, node.Node)
			}
			barrier := &readinessBarrier{entered: make(chan string, len(config.Nodes)), release: make(chan struct{})}
			manager := Manager{Parallelism: &parallelism}
			config.Concurrency, config.Lifecycle = manager.concurrency(len(config.Nodes)), barrier
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var outcomes []StartOutcome
			var err error
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				outcomes, err = StartPrepared(ctx, config)
			}()
			var release sync.Once
			defer func() {
				release.Do(func() { close(barrier.release) })
				<-finished
			}()
			want := min(parallelism, len(config.Nodes))
			if parallelism == 0 {
				want = len(config.Nodes)
			}
			for count := 0; count < want; count++ {
				select {
				case <-barrier.entered:
				case <-ctx.Done():
					t.Fatalf("only %d of %d workers reached readiness together", count, want)
				}
			}
			release.Do(func() { close(barrier.release) })
			<-finished
			if err != nil || len(readyNames(outcomes)) != len(config.Nodes) {
				t.Fatalf("outcomes=%+v, err=%v", outcomes, err)
			}
			if barrier.maxActive != want {
				t.Fatalf("concurrent readiness checks=%d, want %d", barrier.maxActive, want)
			}
		})
	}
}

func TestControllerSerialOverride(t *testing.T) {
	disks := &fakePrivateDisks{}
	lifecycle := &fakeNodeLifecycle{}
	controller := controllerFixture(t, disks, lifecycle)
	controller.Concurrency = 1
	result, err := controller.CreateAndStart(context.Background())
	if err != nil || len(readyNames(result.Start)) != 2 {
		t.Fatalf("result=%+v, err=%v", result, err)
	}
	if disks.maxActive != 1 || lifecycle.maxActive != 1 {
		t.Fatalf("serial override: concurrent disk operations=%d, starts=%d", disks.maxActive, lifecycle.maxActive)
	}
}
