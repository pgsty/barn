package private

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/qemu"
	"github.com/pgsty/barn/internal/state"
)

func TestManagementPortRecoveryKeepsGuestAndExplicitForwards(t *testing.T) {
	config, nodes := preparedStartFixture(t)
	node := nodes[0]
	node.Forwards = append(node.Forwards, qemu.Forward{Bind: "127.0.0.1", Host: 12222, Guest: 5432})
	for i, arg := range node.Invocation.Args {
		if strings.HasPrefix(arg, "user,id=mgmt,") {
			node.Invocation.Args[i] += ",hostfwd=tcp:127.0.0.1:12222-:5432"
		}
	}
	reserved, err := reservedNodePorts(state.Store{Root: config.Deployment.Root})
	if err != nil {
		t.Fatal(err)
	}
	reserved[12222] = struct{}{}
	repaired, notices, err := repairManagementPort(node, reserved, func(_ string, port uint16) bool { return port != 2222 })
	if err != nil || repaired.SSHPort != 22222 || len(notices) != 1 {
		t.Fatalf("repair=%+v notices=%v err=%v", repaired, notices, err)
	}
	if repaired.VMUUID != node.VMUUID || repaired.RootDisk != node.RootDisk || !reflect.DeepEqual(repaired.DataDisks, node.DataDisks) || repaired.Forwards[1] != node.Forwards[1] {
		t.Fatal("repair changed guest or explicit forwards")
	}
	if !strings.Contains(strings.Join(repaired.Invocation.Args, " "), "hostfwd=tcp:127.0.0.1:22222-:22,hostfwd=tcp:127.0.0.1:12222-:5432") {
		t.Fatal("port and invocation disagree")
	}
	if node.SSHPort != 2222 || !strings.Contains(strings.Join(node.Invocation.Args, " "), "hostfwd=tcp:127.0.0.1:2222-:22") {
		t.Fatal("mutated input")
	}
	node.Phase = state.Running
	unchanged, notices, err := repairManagementPort(node, reserved, func(string, uint16) bool { t.Fatal("probed running guest port"); return false })
	if err != nil || len(notices) != 0 || !reflect.DeepEqual(node, unchanged) {
		t.Fatal("changed running guest")
	}
}

type recoveringPortLifecycle struct{ fakeNodeLifecycle }

func (*recoveringPortLifecycle) PortAvailable(_ string, port uint16) bool { return port != 2222 }

func TestStartPersistsRecoveredPortBeforeLaunching(t *testing.T) {
	config, original := preparedStartFixture(t)
	store := state.Store{Root: config.Deployment.Root}
	config.NoWait = true
	config.Lifecycle = &recoveringPortLifecycle{fakeNodeLifecycle: fakeNodeLifecycle{beforeReturn: func(started state.NodeState) {
		persisted, err := store.ReadNode(started.Node)
		if err != nil || persisted.SSHPort != started.SSHPort || !reflect.DeepEqual(persisted.Invocation, started.Invocation) {
			t.Errorf("launch and saved state disagree: %v", err)
		}
	}}}
	outcomes, err := StartPrepared(context.Background(), config)
	if err != nil || len(startFailures(outcomes)) != 0 || len(outcomes[0].Repairs) != 1 {
		t.Fatalf("outcomes=%+v err=%v", outcomes, err)
	}
	changed, err := store.ReadNode("meta")
	if err != nil || changed.SSHPort != 12222 || changed.VMUUID != original[0].VMUUID || changed.RootDisk != original[0].RootDisk {
		t.Fatalf("persisted=%+v err=%v", changed, err)
	}
	peer, err := store.ReadNode("node-1")
	if err != nil || peer.SSHPort != original[1].SSHPort {
		t.Fatalf("changed peer: %+v %v", peer, err)
	}
}

func TestUnrecoverableNodeDoesNotPreventHealthyPeerStart(t *testing.T) {
	config, nodes := preparedStartFixture(t)
	store := state.Store{Root: config.Deployment.Root}
	nodes[0].Invocation.Args = []string{"-netdev", "user,id=unknown"}
	if err := store.WriteNode(nodes[0]); err != nil {
		t.Fatal(err)
	}
	config.Lifecycle = &recoveringPortLifecycle{}
	outcomes, err := StartPrepared(context.Background(), config)
	if err != nil || outcomes[0].Running || !outcomes[1].Ready || len(startFailures(outcomes)) != 1 {
		t.Fatalf("outcomes=%+v err=%v", outcomes, err)
	}
}
