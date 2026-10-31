package private

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

func TestSelectedOperationsLeavePeerChangesPending(t *testing.T) {
	fixture, _ := preparedStartFixture(t)
	t.Setenv("BARN_HOME", fixture.Deployment.Root)
	store := state.Store{Root: fixture.Deployment.Root}
	current, err := store.ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	desired := cloneResolved(current.Resolved)
	desired.Nodes[0].Memory *= 2
	desired.Nodes[0].Image = "not-an-installed-image"
	manager := planFixtureManager(t)
	manager.Nodes = []string{"node-1"}
	plan, err := manager.Plan(context.Background(), desired)
	if err != nil || plan.Blocked != "" || plan.Action != "start" || len(plan.Recreate) != 0 {
		t.Fatalf("selected plan blocked by peer drift: %#v, %v", plan, err)
	}
	before, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	startupError := errors.New("fixture reached selected startup")
	manager.LookPath = func(string) (string, error) { return "", startupError }
	if _, err := manager.Up(context.Background(), desired); !errors.Is(err, startupError) {
		t.Fatalf("selected up blocked before startup: %v", err)
	}
	manager.HostPreflight = func(context.Context, platform.Profile, *spec.PrivateNetwork, execx.Runner) (Backend, error) {
		return Backend{}, startupError
	}
	if _, err := manager.Reload(context.Background(), desired); !errors.Is(err, startupError) {
		t.Fatalf("selected reload blocked before preflight: %v", err)
	}
	after, err := store.ReadNode("meta")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("unselected peer changed: %#v, %v", after, err)
	}
	desired.Nodes[1].Memory *= 2
	plan, err = manager.Plan(context.Background(), desired)
	if err != nil || plan.Blocked != "" || plan.Action != "resize" || strings.Join(plan.Resize, ",") != "node-1" {
		t.Fatalf("selected resize plan = %#v, %v", plan, err)
	}
}

func TestSelectionRetainsOmittedPeersAndExcludesUnselectedAdditions(t *testing.T) {
	current := diffFixtureResolved()
	desired := cloneResolved(current)
	desired.Nodes = []spec.Node{desired.Nodes[0], {Name: "new-node", Address: "10.10.10.12", CPUs: 1, Memory: spec.GiB, RootDisk: spec.GiB}}
	desired.Nodes[0].CPUs++
	merged, err := applyNodeSelection(current, desired, []string{"meta"})
	if err != nil || len(merged.Nodes) != 2 {
		t.Fatalf("merge = %#v, %v", merged, err)
	}
	peer, exists := nodeSpec(merged, "node-1")
	if !exists || !sameResolvedNode(peer, current.Nodes[1]) {
		t.Fatal("omitted peer was removed or changed")
	}
	if _, exists := nodeSpec(merged, "new-node"); exists {
		t.Fatal("unselected addition was applied")
	}
}

func TestSelectionPreservesDesiredNodeOrder(t *testing.T) {
	current := diffFixtureResolved()
	current.Nodes = append(current.Nodes, spec.Node{Name: "node-2", Address: "10.10.10.12", CPUs: 1, Memory: spec.GiB, RootDisk: spec.GiB})
	desired := cloneResolved(current)
	desired.Nodes = []spec.Node{desired.Nodes[2], desired.Nodes[0], desired.Nodes[1]}

	merged, err := applyNodeSelection(current, desired, []string{"meta"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"node-2", "meta", "node-1"}
	got := make([]string, 0, len(merged.Nodes))
	for _, node := range merged.Nodes {
		got = append(got, node.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged order = %v, want %v", got, want)
	}
}

func TestSelectionChecksConflictsAgainstAppliedPeers(t *testing.T) {
	current := diffFixtureResolved()
	for _, change := range []string{"address", "control", "alias", "network"} {
		t.Run(change, func(t *testing.T) {
			desired := cloneResolved(current)
			switch change {
			case "address":
				desired.Nodes[0].Address, desired.Nodes[1].Address = current.Nodes[1].Address, current.Nodes[0].Address
			case "control":
				desired.Nodes[0].Control, desired.Nodes[1].Control = false, true
			case "alias":
				current := cloneResolved(current)
				current.Nodes[1].Aliases = []string{"shared"}
				desired.Nodes[0].Aliases = []string{"shared"}
				if _, err := applyNodeSelection(current, desired, []string{"meta"}); err == nil {
					t.Fatal("alias collision accepted")
				}
				return
			case "network":
				desired.SSHUser = "operator"
			}
			if _, err := applyNodeSelection(current, desired, []string{"meta"}); err == nil {
				t.Fatal("conflict with applied peer accepted")
			}
			if _, err := applyNodeSelection(current, desired, []string{"meta", "node-1"}); err != nil {
				t.Fatalf("coordinated changes rejected: %v", err)
			}
		})
	}
}

func TestSequentialRecreatePreservesUnselectedDisksAndIdentity(t *testing.T) {
	lifecycle := &fakeNodeLifecycle{}
	controller := controllerFixture(t, &fakePrivateDisks{}, lifecycle)
	t.Setenv("BARN_HOME", controller.Deployment.Root)
	if _, err := controller.CreateAndStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := state.Store{Root: controller.Deployment.Root}
	current, err := store.ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	desired := cloneResolved(current.Resolved)
	for index := range desired.Nodes {
		desired.Nodes[index].Memory *= 2
		node, err := store.ReadNode(desired.Nodes[index].Name)
		if err != nil {
			t.Fatal(err)
		}
		node.Phase, node.Process = state.Prepared, state.ProcessIdentity{}
		if err := store.WriteNode(node); err != nil {
			t.Fatal(err)
		}
	}
	writeDestroyKeyFixtures(t, controller.Deployment.Root)
	for _, selected := range []string{"node-1", "meta"} {
		current, err := store.ReadDeployment()
		if err != nil {
			t.Fatal(err)
		}
		merged, err := applyNodeSelection(current.Resolved, desired, []string{selected})
		if err != nil {
			t.Fatal(err)
		}
		peerName := "meta"
		if selected == "meta" {
			peerName = "node-1"
		}
		peer, err := store.ReadNode(peerName)
		if err != nil {
			t.Fatal(err)
		}
		peerDisk, err := os.Stat(peer.RootDisk)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (Manager{Nodes: []string{selected}, allowPartialDestroy: true}).Destroy(context.Background()); err != nil {
			t.Fatal(err)
		}
		controller.Prepare.Resolved = merged
		controller.Prepare.SpecHash, err = spec.Hash(merged)
		if err != nil {
			t.Fatal(err)
		}
		controller.Prepare.NodeHashes, err = spec.NodeHashes(merged)
		if err != nil {
			t.Fatal(err)
		}
		controller.Prepare.Plan, err = Build(merged, os.Getuid(), map[string]string{peerName: peer.VMUUID}, nil)
		if err != nil {
			t.Fatal(err)
		}
		publicKey, privateKey := testSSHKeyPair(t)
		controller.Prepare.Seeds, err = RenderSeeds(merged, controller.Prepare.Plan, SeedInput{PublicKey: publicKey, PrivateKey: privateKey, SpecHashes: controller.Prepare.NodeHashes, Generation: map[string]uint64{"meta": 1, "node-1": 1}})
		if err != nil {
			t.Fatal(err)
		}
		controller.CreateNodes, controller.StartNodes = []string{selected}, []string{selected}
		if _, err := controller.CreateAndStart(context.Background()); err != nil {
			t.Fatal(err)
		}
		after, err := store.ReadNode(peerName)
		if err != nil || !reflect.DeepEqual(peer, after) {
			t.Fatalf("peer state changed after recreating %s: %v", selected, err)
		}
		afterDisk, err := os.Stat(peer.RootDisk)
		if err != nil || !os.SameFile(peerDisk, afterDisk) {
			t.Fatalf("peer disk changed after recreating %s: %v", selected, err)
		}
		applied, err := store.ReadDeployment()
		if err != nil {
			t.Fatal(err)
		}
		diff := diffResolved(applied.Resolved, desired, func(string) bool { return true })
		if selected == "node-1" && strings.Join(diff.Resize, ",") != "meta" || selected == "meta" && len(diff.Resize) != 0 {
			t.Fatalf("pending changes after %s = %#v", selected, diff)
		}
	}
}
