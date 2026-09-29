package private

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/runtimepath"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

func TestSelectedRecreateRefusesPeerDriftBeforeDeletingDisks(t *testing.T) {
	fixture, nodes := preparedStartFixture(t)
	t.Setenv("BARN_HOME", fixture.Deployment.Root)
	store := state.Store{Root: fixture.Deployment.Root}
	current, err := store.ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	requested := cloneResolved(current.Resolved)
	for i := range requested.Nodes {
		requested.Nodes[i].Memory *= 2
	}
	_, err = (Manager{Nodes: []string{"node-1"}}).RecreateResolved(context.Background(), requested)
	if !errors.Is(err, ErrRecreateRequired) || !strings.Contains(err.Error(), "barn recreate node-1 meta") {
		t.Fatalf("recreate: %v", err)
	}
	for _, node := range nodes {
		if _, err := os.Stat(node.RootDisk); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReadNode(node.Node); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDestroyAndStopReadStateOnlyAfterAcquiringLock(t *testing.T) {
	fixture, _ := preparedStartFixture(t)
	t.Setenv("BARN_HOME", fixture.Deployment.Root)
	held, err := acquireDeploymentLock(context.Background(), fixture.Deployment.Root, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	// A concurrent writer has not yet finished publishing state. Readers must
	// wait for its lock instead of acting on this intermediate snapshot.
	if err := os.WriteFile(filepath.Join(fixture.Deployment.Root, "state.json"), []byte("unfinished"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []func(context.Context) (Status, error){(Manager{}).Destroy, (Manager{}).Stop} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, err := operation(ctx)
		cancel()
		if _, reason, _ := failure.Classify(err); reason != "deployment_busy" {
			t.Fatalf("read uncommitted state before lock: %v", err)
		}
	}
}

func TestDestroyPreservesUnrelatedListeningRuntime(t *testing.T) {
	fixture, nodes := preparedStartFixture(t)
	t.Setenv("BARN_HOME", fixture.Deployment.Root)
	writeDestroyKeyFixtures(t, fixture.Deployment.Root)
	node := nodes[0]
	if err := runtimepath.Ensure(node.Runtime.Directory, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", node.Runtime.QMP)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close(); _ = cleanupRuntime(node) }()
	if _, err := (Manager{}).Destroy(context.Background()); err == nil || !strings.Contains(err.Error(), "still in use") {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := os.Lstat(node.Runtime.QMP); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(node.RootDisk); err != nil {
		t.Fatal(err)
	}
}

func TestReloadPreflightsBeforeStoppingExistingNodes(t *testing.T) {
	for _, failure := range []string{"host unavailable", "image unavailable"} {
		t.Run(failure, func(t *testing.T) {
			fixture, nodes := preparedStartFixture(t)
			t.Setenv("BARN_HOME", fixture.Deployment.Root)
			store := state.Store{Root: fixture.Deployment.Root}
			current, err := store.ReadDeployment()
			if err != nil {
				t.Fatal(err)
			}
			nodes[0].Phase = state.Running // no real process: any attempt to stop is a test failure
			if err := store.WriteNode(nodes[0]); err != nil {
				t.Fatal(err)
			}
			requested := cloneResolved(current.Resolved)
			requested.Nodes = append(requested.Nodes, spec.Node{Name: "node-2", Address: "10.10.10.12", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB})
			manager := privateShareCapabilityManager(t, fixture, &rejectingPrivateShareRunner{})
			if failure == "host unavailable" {
				manager.HostPreflight = func(context.Context, platform.Profile, *spec.PrivateNetwork, execx.Runner) (Backend, error) {
					return Backend{}, errors.New(failure)
				}
			} else {
				manager.ResolveImage = func(context.Context, string, string) (image.Entry, string, image.Metadata, error) {
					return image.Entry{}, "", image.Metadata{}, errors.New(failure)
				}
			}
			if _, err := manager.Reload(context.Background(), requested); err == nil || !strings.Contains(err.Error(), failure) {
				t.Fatalf("reload: %v", err)
			}
			after, err := store.ReadNode(nodes[0].Node)
			if err != nil || after.Phase != state.Running {
				t.Fatalf("node changed: %#v, %v", after, err)
			}
		})
	}
}

func TestPlanMixedRunningStoppedAndInvalidImage(t *testing.T) {
	fixture, nodes := preparedStartFixture(t)
	t.Setenv("BARN_HOME", fixture.Deployment.Root)
	store := state.Store{Root: fixture.Deployment.Root}
	current, err := store.ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	nodes[0].Phase, nodes[1].Phase = state.Running, state.Stopped
	for _, node := range nodes {
		if err := store.WriteNode(node); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := planFixtureManager(t).Plan(context.Background(), current.Resolved)
	if err != nil || plan.Action != "start" || strings.Join(plan.Start, ",") != "node-1" {
		t.Fatalf("plan: %#v, %v", plan, err)
	}
	current.Resolved.Nodes = append(current.Resolved.Nodes, spec.Node{Name: "node-2", Address: "10.10.10.12", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB})
	plan, err = planFixtureManager(t).Plan(context.Background(), current.Resolved)
	if err != nil || plan.Action != "converge" || strings.Join(plan.Start, ",") != "node-1" || strings.Join(plan.Create, ",") != "node-2" {
		t.Fatalf("expansion with stopped peer: %#v, %v", plan, err)
	}
	selected := planFixtureManager(t)
	selected.Nodes = []string{"meta"}
	plan, err = selected.Plan(context.Background(), current.Resolved)
	if err != nil || plan.Action != "none" || len(plan.Create) != 0 {
		t.Fatalf("unselected new peer: %#v, %v", plan, err)
	}
	current.Resolved.Image = "nonexistent-test-image"
	if _, err := planFixtureManager(t).Plan(context.Background(), current.Resolved); err == nil {
		t.Fatal("unknown image accepted")
	}
}
