package private

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

func diffFixtureResolved() spec.Resolved {
	return spec.Resolved{
		Schema: 1, Name: "barn", Image: "u24", Network: "private", SSHUser: "dba",
		Private: &spec.PrivateNetwork{CIDR: "10.10.10.0/24", HostAddress: "10.10.10.1", DHCPEnd: "10.10.10.8"},
		Nodes: []spec.Node{
			{Name: "meta", Control: true, Address: "10.10.10.10", CPUs: 2, Memory: 4 * spec.GiB, RootDisk: 64 * spec.GiB},
			{Name: "node-1", Address: "10.10.10.11", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 64 * spec.GiB},
		},
	}
}

func TestDiffResolvedClassifiesNodeChanges(t *testing.T) {
	persisted := diffFixtureResolved()
	stateful := func(string) bool { return true }

	unchanged := diffResolved(persisted, persisted, stateful)
	if unchanged.EnvelopeChanged || len(unchanged.Create) != 0 || len(unchanged.Changed) != 0 || len(unchanged.Removed) != 0 || len(unchanged.Unchanged) != 2 {
		t.Fatalf("identical diff = %#v", unchanged)
	}

	added := diffFixtureResolved()
	added.Nodes = append(added.Nodes, spec.Node{Name: "node-2", Address: "10.10.10.12", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 64 * spec.GiB})
	grow := diffResolved(persisted, added, func(name string) bool { return name != "node-2" })
	if grow.EnvelopeChanged || len(grow.Create) != 1 || grow.Create[0] != "node-2" || len(grow.Changed) != 0 || len(grow.Removed) != 0 {
		t.Fatalf("additive diff = %#v", grow)
	}

	resized := diffFixtureResolved()
	resized.Nodes[1].Memory = 8 * spec.GiB
	changed := diffResolved(persisted, resized, stateful)
	if len(changed.Changed) != 1 || changed.Changed[0] != "node-1" || len(changed.Create) != 0 {
		t.Fatalf("changed diff = %#v", changed)
	}

	// The per-node recreate window: same definition change, but node-1's state
	// was already destroyed, so it is a creation with the new definition.
	window := diffResolved(persisted, resized, func(name string) bool { return name != "node-1" })
	if len(window.Create) != 1 || window.Create[0] != "node-1" || len(window.Changed) != 0 {
		t.Fatalf("recreate-window diff = %#v", window)
	}

	shrunk := diffFixtureResolved()
	shrunk.Nodes = shrunk.Nodes[:1]
	removed := diffResolved(persisted, shrunk, stateful)
	if len(removed.Removed) != 1 || removed.Removed[0] != "node-1" {
		t.Fatalf("removal diff = %#v", removed)
	}

	envelope := diffFixtureResolved()
	envelope.SSHUser = "admin"
	if !diffResolved(persisted, envelope, stateful).EnvelopeChanged {
		t.Fatalf("SSH user change must be an envelope change")
	}
	architecture := diffFixtureResolved()
	architecture.Arch = "amd64"
	if !diffResolved(persisted, architecture, stateful).EnvelopeChanged {
		t.Fatalf("guest architecture change must be an envelope change")
	}
}

func diffTestDeployment(t *testing.T) Deployment {
	t.Helper()
	root := t.TempDir()
	return Deployment{Root: root}
}

func deploymentStateFor(t *testing.T, resolved spec.Resolved) state.DeploymentState {
	t.Helper()
	hash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	return state.DeploymentState{Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: hash, Resolved: resolved, UpdatedAt: time.Unix(1, 0).UTC()}
}

func TestEnsureDeploymentStateAcceptsAdditionsRefusesRemovals(t *testing.T) {
	deploymentValue := diffTestDeployment(t)
	store := state.Store{Root: deploymentValue.Root}
	persisted := diffFixtureResolved()
	if err := ensureDeploymentState(store, deploymentStateFor(t, persisted)); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	added := diffFixtureResolved()
	added.Nodes = append(added.Nodes, spec.Node{Name: "node-2", Address: "10.10.10.12", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 64 * spec.GiB})
	if err := ensureDeploymentState(store, deploymentStateFor(t, added)); err != nil {
		t.Fatalf("additive update refused: %v", err)
	}
	current, err := store.ReadDeployment()
	if err != nil || len(current.Resolved.Nodes) != 3 {
		t.Fatalf("additive update not persisted: %#v %v", current, err)
	}

	shrunk := diffFixtureResolved()
	shrunk.Nodes = shrunk.Nodes[:1]
	if err := ensureDeploymentState(store, deploymentStateFor(t, shrunk)); err == nil {
		t.Fatal("node removal must never ride a state commit")
	}

	// A changed definition is refused while the node has committed state...
	resized := diffFixtureResolved()
	resized.Nodes = append(resized.Nodes, added.Nodes[2])
	resized.Nodes[1].Memory = 8 * spec.GiB
	nodeState := state.NodeState{
		Schema: state.NodeSchema, BarnVersion: "test",
		Node: "node-1", VMUUID: "018f4b8e-1234-4abc-9def-0123456789ab", Phase: state.Stopped, Generation: 1,
		SpecHash: "deadbeef", CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}
	if err := store.WriteNode(nodeState); err != nil {
		t.Fatal(err)
	}
	if err := ensureDeploymentState(store, deploymentStateFor(t, resized)); err == nil {
		t.Fatal("definition change with live node state must be refused")
	}
	// ...and accepted once that state is gone (the per-node recreate window).
	nodeDir, _ := deploymentValue.NodeDir("node-1")
	if err := os.Remove(filepath.Join(nodeDir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := ensureDeploymentState(store, deploymentStateFor(t, resized)); err != nil {
		t.Fatalf("recreate-window update refused: %v", err)
	}
}
