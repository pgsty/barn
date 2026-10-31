package private

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/qemu"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

func resizeNodeState(t *testing.T, resolved spec.Resolved, root string) state.NodeState {
	t.Helper()
	nodeHash, err := spec.NodeHash(resolved, "meta")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1, 0).UTC()
	return state.NodeState{
		Schema: state.NodeSchema, BarnVersion: "test", Node: "meta",
		VMUUID: "018f4b8e-1234-4abc-9def-0123456789ab", Phase: state.Stopped,
		Generation: 1, SpecHash: nodeHash, GuestSpecHash: nodeHash,
		RootDisk: root + "/root.qcow2", Seed: root + "/seed.iso", SSHPort: 2222,
		Runtime:    state.RuntimePaths{Directory: root + "/runtime", QMP: root + "/runtime/qmp.sock", PIDFile: root + "/runtime/qemu.pid"},
		Invocation: qemu.Invocation{Binary: "/fixture/qemu", Args: []string{"-smp", "1", "-m", "2048M"}},
		CreatedAt:  now, UpdatedAt: now,
	}
}

func TestCommitResizePreservesRunningInvocationAndGuestMarker(t *testing.T) {
	root := t.TempDir()
	resolved := singlePrivateResolved()
	currentHash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Root: root}
	if err := store.WriteDeployment(state.DeploymentState{Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: currentHash, Resolved: resolved, UpdatedAt: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	node := resizeNodeState(t, resolved, root)
	node.Phase = state.Running
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	target := resolved.Nodes[0]
	target.CPUs = 2
	target.Memory = 4 * spec.GiB
	next, err := qemu.ConfigureResources(node.Invocation, target.CPUs, target.Memory)
	if err != nil {
		t.Fatal(err)
	}
	updated, deployment, err := commitResize(store, state.DeploymentState{Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: currentHash, Resolved: resolved, UpdatedAt: time.Unix(1, 0).UTC()}, node, target, next, true, "test", time.Unix(2, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if updated.VMUUID != node.VMUUID || updated.Invocation.Binary != node.Invocation.Binary || updated.PendingInvocation.Binary == "" {
		t.Fatalf("resize changed VM identity or pending invocation: %#v", updated)
	}
	if updated.GuestSpecHash != node.GuestSpecHash || updated.SpecHash == node.SpecHash {
		t.Fatalf("guest/spec hashes after resize = %q/%q, before %q", updated.GuestSpecHash, updated.SpecHash, node.SpecHash)
	}
	if deployment.Resolved.Nodes[0].CPUs != 2 || deployment.Resolved.Nodes[0].Memory != 4*spec.GiB {
		t.Fatalf("deployment resources = %#v", deployment.Resolved.Nodes[0])
	}
	readBack, err := store.ReadNode("meta")
	if err != nil || readBack.PendingInvocation.Binary == "" {
		t.Fatalf("persisted pending invocation = %#v, %v", readBack, err)
	}
}

func TestResizeStoppedNodePatchesInvocationWithoutStartingIt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	resolved := singlePrivateResolved()
	hash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Root: root}
	if err := store.WriteDeployment(state.DeploymentState{Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: hash, Resolved: resolved, UpdatedAt: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteNode(resizeNodeState(t, resolved, root)); err != nil {
		t.Fatal(err)
	}
	desired := cloneResolved(resolved)
	desired.Nodes[0].Memory = 4 * spec.GiB
	status, err := (Manager{BarnVersion: "test"}).Resize(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.Message, "while stopped") {
		t.Fatalf("resize status = %#v", status)
	}
	node, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	resources, err := qemu.InvocationResources(node.Invocation)
	if err != nil || resources.Memory != 4*spec.GiB || node.Phase != state.Stopped || node.PendingInvocation.Binary != "" {
		t.Fatalf("stopped resize state = %#v resources=%#v err=%v", node, resources, err)
	}
	deployment, err := store.ReadDeployment()
	if err != nil || deployment.Resolved.Nodes[0].Memory != 4*spec.GiB {
		t.Fatalf("stopped resize deployment = %#v err=%v", deployment, err)
	}
}
