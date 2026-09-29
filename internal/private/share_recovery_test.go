package private

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

func TestMissingSharePrepareIsScopedAndRetryable(t *testing.T) {
	config := privatePrepareConfig(t, t.TempDir(), &fakePrivateDisks{})
	share := privateShareFixture(t)
	if err := os.Remove(share.Host); err != nil {
		t.Fatal(err)
	}
	config.Resolved.Nodes[0].Shares = []spec.Share{share}
	outcomes := PrepareSelected(context.Background(), config, []string{"meta", "node-1"}, 2)
	if len(PreparedNames(outcomes)) != 1 || PreparedNames(outcomes)[0] != "node-1" {
		t.Fatalf("missing source must fail only meta: %+v", outcomes)
	}
	for _, want := range []string{share.Host, share.Guest, "restore"} {
		if !strings.Contains(outcomes[0].Error, want) {
			t.Fatalf("missing actionable detail %q: %s", want, outcomes[0].Error)
		}
	}
	if _, err := os.Stat(filepath.Join(config.DeploymentRoot, "nodes", "meta")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bad source created node artifacts: %v", err)
	}
	if _, err := os.Stat(share.Host); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source created: %v", err)
	}
	if err := os.Mkdir(share.Host, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareNode(context.Background(), config, "meta"); err != nil {
		t.Fatal(err)
	}
}

type unavailableSharePeerRunner struct{}

func (unavailableSharePeerRunner) Run(_ context.Context, _ string, args ...string) (execx.Result, error) {
	if len(args) == 2 && args[0] == "-device" && args[1] == "help" {
		return execx.Result{Stdout: []byte("virtio-9p-pci")}, nil
	}
	return execx.Result{}, errors.New("independent peer reached its runner")
}

func TestStartMissingShareDoesNotAbortPeer(t *testing.T) {
	fixture, _ := preparedStartFixture(t)
	t.Setenv("BARN_HOME", fixture.Deployment.Root)
	writeRecoveryKeys(t, fixture.Deployment.Root)
	share := privateShareFixture(t)
	persistPrivateShare(t, state.Store{Root: fixture.Deployment.Root}, share)
	if err := os.Remove(share.Host); err != nil {
		t.Fatal(err)
	}
	manager := privateShareCapabilityManager(t, fixture, unavailableSharePeerRunner{})
	manager.DialSSHAddress = func(string, string) (net.Conn, error) { return nil, errors.New("unused") }
	_, err := manager.Start(context.Background())
	var partial *PartialError
	if !errors.As(err, &partial) || partial.Total != 2 || len(partial.Failures) != 2 {
		t.Fatalf("expected two independent outcomes, got %T %v", err, err)
	}
	if !strings.Contains(partial.Failures[0].Error, share.Host) || strings.Contains(partial.Failures[1].Error, share.Host) {
		t.Fatalf("share failure leaked to peer: %+v", partial.Failures)
	}
}

func TestUpAttemptsExistingPeersAfterNewNodePrepareFailure(t *testing.T) {
	fixture, _ := preparedStartFixture(t)
	t.Setenv("BARN_HOME", fixture.Deployment.Root)
	if err := os.MkdirAll(filepath.Join(fixture.Deployment.Root, "locks"), 0700); err != nil {
		t.Fatal(err)
	}
	writeRecoveryKeys(t, fixture.Deployment.Root)
	persisted, err := (state.Store{Root: fixture.Deployment.Root}).ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	share := privateShareFixture(t)
	if err := os.Remove(share.Host); err != nil {
		t.Fatal(err)
	}
	requested := cloneResolved(persisted.Resolved)
	third := cloneNode(requested.Nodes[1])
	third.Name, third.Address = "node-2", "10.10.10.12"
	third.Shares = []spec.Share{share}
	requested.Nodes = append(requested.Nodes, third)
	manager := privateShareCapabilityManager(t, fixture, unavailableSharePeerRunner{})
	manager.BarnVersion = "test"
	manager.HostPreflight = func(context.Context, platform.Profile, *spec.PrivateNetwork, execx.Runner) (Backend, error) {
		return Backend{DarwinSocket: "/fixture/vmnet.sock", ReconnectMS: 1000}, nil
	}
	manager.DialSSHAddress = func(string, string) (net.Conn, error) { return nil, errors.New("unused") }
	_, err = manager.Up(context.Background(), requested)
	var partial *PartialError
	if !errors.As(err, &partial) || partial.Total != 3 || len(partial.Failures) != 3 {
		t.Fatalf("new prepare failure must not skip two existing peers: %T %v", err, err)
	}
	if partial.Failures[2].Node != "node-2" || partial.Failures[2].Stage != "prepare" || !strings.Contains(partial.Failures[2].Error, share.Host) {
		t.Fatalf("lost original prepare cause: %+v", partial.Failures)
	}
}

func TestPartialResultsDoNotHideGlobalFailures(t *testing.T) {
	partial := &PartialError{Total: 1, Failures: []NodeFailure{{Node: "meta", Stage: "prepare", Error: "missing share"}}}
	if IsolatedPartial(errors.Join(partial, nil)) != partial {
		t.Fatal("lost sole node failure")
	}
	if IsolatedPartial(errors.Join(partial, errors.New("state identity mismatch"))) != nil {
		t.Fatal("global failure hidden by node failure")
	}
}
