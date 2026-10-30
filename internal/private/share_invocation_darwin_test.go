package private

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/qemu"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

type preparedShareLifecycle struct {
	*fakeNodeLifecycle
	native NativeLifecycle
}

func (l preparedShareLifecycle) PrepareStart(node state.NodeState) (state.NodeState, error) {
	return l.native.PrepareStart(node)
}

func TestDarwinShareInvocationIsPersistedBeforeStart(t *testing.T) {
	config, nodes := preparedStartFixture(t)
	share := privateShareFixture(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(share.Host, link); err != nil {
		t.Fatal(err)
	}
	share.Host = link
	node := nodes[0]
	node.Invocation.Args = append(node.Invocation.Args, "-fsdev", "local,id=share0,path=/dev/fd/3,security_model=mapped-xattr")
	node.Invocation.InheritedFiles = []qemu.InheritedFile{{FD: 3, Kind: "share", ID: spec.ShareTag(share)}}
	store := state.Store{Root: config.Deployment.Root}
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	native := NativeLifecycle{Deployment: config.Deployment, Shares: map[string][]spec.Share{node.Node: {share}}}
	fake := &fakeNodeLifecycle{beforeReturn: func(starting state.NodeState) {
		persisted, err := store.ReadNode(starting.Node)
		if err != nil || !slices.Equal(starting.Invocation.Args, persisted.Invocation.Args) || persisted.Phase != state.Starting {
			t.Errorf("actual invocation was not persisted before launch: %v", err)
		}
		if strings.Contains(strings.Join(starting.Invocation.Args, " "), "/dev/fd/") {
			t.Error("directory FD path reached Darwin launch")
		}
	}}
	config.Nodes = []string{node.Node}
	config.Lifecycle = preparedShareLifecycle{fake, native}
	outcomes, err := StartPrepared(context.Background(), config)
	if err != nil || len(readyNames(outcomes)) != 1 {
		t.Fatalf("start = %#v, %v", outcomes, err)
	}
	started, err := store.ReadNode(node.Node)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := native.Start(context.Background(), started); err == nil || !strings.Contains(err.Error(), "changed before startup") {
		t.Fatalf("retargeted source reached QEMU with stale argv: %v", err)
	}
}
