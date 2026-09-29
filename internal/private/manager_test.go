package private

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/image"
	netpreflight "github.com/pgsty/barn/internal/network/preflight"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/qemu"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

func singlePrivateResolved() spec.Resolved {
	return spec.Resolved{
		Schema: 1, Name: "private", Image: "u24", Network: "private", SSHUser: "dba",
		Private: &spec.PrivateNetwork{CIDR: "10.10.10.0/24", HostAddress: "10.10.10.1", DHCPEnd: "10.10.10.8"},
		Nodes:   []spec.Node{{Name: "meta", Control: true, Address: "10.10.10.10", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB, Forwards: []spec.Forward{{Bind: "127.0.0.1", Host: 15432, Guest: 5432, Protocol: "tcp"}}}},
	}
}

func TestMaterializePrivatePortsIsDeterministicAndNonColliding(t *testing.T) {
	t.Parallel()
	resolved := singlePrivateResolved()
	resolved.Nodes = append(resolved.Nodes, spec.Node{Name: "node-1", Address: "10.10.10.11", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB})
	materialized, ports, err := materializePortsWithProbe(resolved, map[uint16]struct{}{2222: {}, 15432: {}}, func(string, uint16) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if ports["meta"] != 12222 || ports["node-1"] != 2223 || materialized.Nodes[0].Forwards[0].Host != 25432 {
		t.Fatalf("materialized ports=%v resolved=%#v", ports, materialized.Nodes[0].Forwards)
	}
	if materialized.Nodes[0].Forwards[0].RequestedHost != 15432 {
		t.Fatalf("materialized forward request evidence = %#v", materialized.Nodes[0].Forwards[0])
	}
	if resolved.Nodes[0].Forwards[0].Host != 15432 {
		t.Fatal("port materialization mutated caller-owned resolved spec")
	}
}

func TestResolvedNodeSelectionIsDeepAndOrdered(t *testing.T) {
	t.Parallel()
	resolved := singlePrivateResolved()
	resolved.Nodes = append(resolved.Nodes,
		spec.Node{Name: "node-1", Address: "10.10.10.11", Image: "el9", Aliases: []string{"worker"}, CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB},
		spec.Node{Name: "node-2", Address: "10.10.10.12", Image: "el10", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB},
	)
	selected := resolvedNodeSelection(resolved, []string{"node-2", "node-1"})
	if len(selected.Nodes) != 2 || selected.Nodes[0].Name != "node-1" || selected.Nodes[1].Name != "node-2" {
		t.Fatalf("resolved selection = %#v", selected.Nodes)
	}
	selected.Nodes[0].Aliases[0] = "changed"
	if resolved.Nodes[1].Aliases[0] != "worker" {
		t.Fatal("resolved selection aliases caller-owned node state")
	}
}

func TestMaterializeMissingNodePortsSkipsUnselectedMissingPeersAndReservesExisting(t *testing.T) {
	t.Parallel()
	resolved := singlePrivateResolved()
	resolved.Nodes[0].Forwards = []spec.Forward{{Bind: "127.0.0.1", Host: 15432, Guest: 5432, Protocol: "tcp"}}
	resolved.Nodes = append(resolved.Nodes,
		spec.Node{Name: "node-1", Address: "10.10.10.11", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB, Forwards: []spec.Forward{{Bind: "127.0.0.1", Host: 15432, Guest: 5432, Protocol: "tcp"}}},
		spec.Node{Name: "node-2", Address: "10.10.10.12", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB},
	)
	store := state.Store{Root: t.TempDir()}
	if err := store.WriteNode(state.NodeState{
		Schema: state.NodeSchema, BarnVersion: "test", Node: "meta", SSHPort: 2223,
		VMUUID: "018f4b8e-1234-4abc-9def-0123456789ab", Phase: state.Stopped, Generation: 1,
		SpecHash: strings.Repeat("a", 64), CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	materialized, ports, err := materializeMissingNodePortsWithProbe(resolved, []string{"node-1"}, map[uint16]struct{}{}, store, func(string, uint16) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 || ports["meta"] != 2223 || ports["node-1"] != 12223 {
		t.Fatalf("partial materialized ports = %v", ports)
	}
	if materialized.Nodes[1].Forwards[0].Host != 25432 || materialized.Nodes[1].Forwards[0].RequestedHost != 15432 {
		t.Fatalf("partial materialized forward = %#v", materialized.Nodes[1].Forwards[0])
	}
	if resolved.Nodes[1].Forwards[0].Host != 15432 {
		t.Fatal("partial port materialization mutated caller-owned spec")
	}
}

func TestCommittedNodeUUIDsSkipsDesiredNodesWithoutState(t *testing.T) {
	t.Parallel()
	resolved := singlePrivateResolved()
	resolved.Nodes = append(resolved.Nodes,
		spec.Node{Name: "node-1", Address: "10.10.10.11", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB},
		spec.Node{Name: "node-2", Address: "10.10.10.12", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB},
	)
	store := state.Store{Root: t.TempDir()}
	if err := store.WriteNode(state.NodeState{
		Schema: state.NodeSchema, BarnVersion: "test", Node: "meta", SSHPort: 2222,
		VMUUID: "018f4b8e-1234-4abc-9def-0123456789ab", Phase: state.Stopped, Generation: 1,
		SpecHash: strings.Repeat("a", 64), CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	uuids, err := committedNodeUUIDs(store, resolved, []string{"node-1"})
	if err != nil || len(uuids) != 1 || uuids["meta"] != "018f4b8e-1234-4abc-9def-0123456789ab" {
		t.Fatalf("committed UUIDs = %v, %v", uuids, err)
	}
}

type failAfterSelectedImageRunner struct{}

func (failAfterSelectedImageRunner) Run(context.Context, string, ...string) (execx.Result, error) {
	return execx.Result{}, errors.New("stop after selected image resolution")
}

func TestPrivateSelectedFreshUpResolvesOnlySelectedImageAndAddress(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	t.Setenv("BARN_HOME", root)
	if err := os.MkdirAll(filepath.Join(root, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved := singlePrivateResolved()
	resolved.Nodes[0].Image = "el9"
	resolved.Nodes = append(resolved.Nodes,
		spec.Node{Name: "node-1", Address: "10.10.10.11", Image: "el10", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB},
		spec.Node{Name: "node-2", Address: "10.10.10.12", Image: "u24", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB},
	)
	profile, err := platform.Resolve("darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	resolvedImages := make([]string, 0)
	preflightAddresses := make([]string, 0)
	manager := Manager{
		Nodes: []string{"node-2"}, BarnVersion: "test", Runner: failAfterSelectedImageRunner{},
		NativeProfile: func() (platform.Profile, error) { return profile, nil },
		NetworkPreflight: func(_ context.Context, _ platform.Profile, request netpreflight.Request, _ execx.Runner) netpreflight.Report {
			preflightAddresses = append(preflightAddresses, request.Addresses...)
			return netpreflight.Report{Ready: true}
		},
		HostPreflight: func(context.Context, platform.Profile, *spec.PrivateNetwork, execx.Runner) (Backend, error) {
			return Backend{DarwinSocket: "/fixture/vmnet.sock"}, nil
		},
		DialSSHAddress: func(string, string) (net.Conn, error) { return nil, errors.New("unused") },
		LookPath:       func(name string) (string, error) { return "/fixture/" + name, nil },
		LookupImage: func(_ context.Context, alias, arch string) (image.Entry, error) {
			return image.Entry{Alias: alias, Release: "test", Arch: arch, Boot: "uefi", SHA256: strings.Repeat("a", 64)}, nil
		},
		ResolveImage: func(_ context.Context, alias, arch string) (image.Entry, string, image.Metadata, error) {
			resolvedImages = append(resolvedImages, alias)
			return image.Entry{Alias: alias, Release: "test", Arch: arch, Boot: "uefi", SHA256: strings.Repeat("a", 64)}, "/fixture/base.qcow2", image.Metadata{VirtualSize: 8 * spec.GiB}, nil
		},
	}
	_, err = manager.Up(context.Background(), resolved)
	if err == nil || !strings.Contains(err.Error(), "stop after selected image resolution") {
		t.Fatalf("selected up error = %v", err)
	}
	if strings.Join(resolvedImages, ",") != "u24" {
		t.Fatalf("selected up resolved images = %v", resolvedImages)
	}
	if strings.Join(preflightAddresses, ",") != "10.10.10.12" {
		t.Fatalf("selected up preflight addresses = %v", preflightAddresses)
	}
}

func planFixtureManager(t *testing.T) Manager {
	t.Helper()
	profile, err := platform.Resolve("darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	return Manager{
		NativeProfile: func() (platform.Profile, error) { return profile, nil },
		DialSSHAddress: func(string, string) (net.Conn, error) {
			return nil, errors.New("fixture address unused")
		},
		HostPreflight: func(context.Context, platform.Profile, *spec.PrivateNetwork, execx.Runner) (Backend, error) {
			return Backend{}, nil
		},
		NetworkPreflight: func(context.Context, platform.Profile, netpreflight.Request, execx.Runner) netpreflight.Report {
			return netpreflight.Report{Ready: true}
		},
	}
}

func TestPrivatePlanIsReadOnlyAndSupportsOneNode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	t.Setenv("BARN_HOME", root)
	manager := planFixtureManager(t)
	plan, err := manager.Plan(context.Background(), singlePrivateResolved())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != "create" || len(plan.Nodes) != 1 || plan.Nodes[0] != "meta" {
		t.Fatalf("plan = %#v", plan)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("read-only private plan created the data root: %v", err)
	}
}

func TestPrivatePlanTreatsEmptyDataRootWithoutStateAsCreate(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	manager := planFixtureManager(t)
	plan, err := manager.Plan(context.Background(), singlePrivateResolved())
	if err != nil || plan.Action != "create" || plan.Destructive {
		t.Fatalf("empty-root plan=%#v err=%v", plan, err)
	}
}

func TestPrivatePlanFiltersPendingPeersToSelection(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	resolved := singlePrivateResolved()
	resolved.Nodes = append(resolved.Nodes,
		spec.Node{Name: "node-1", Address: "10.10.10.11", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB},
		spec.Node{Name: "node-2", Address: "10.10.10.12", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB},
	)
	hash, err := spec.Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Root: root}
	if err := store.WriteDeployment(state.DeploymentState{Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: hash, Resolved: resolved, UpdatedAt: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	metaHash, err := spec.NodeHash(resolved, "meta")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteNode(state.NodeState{
		Schema: state.NodeSchema, BarnVersion: "test", Node: "meta", SSHPort: 2222,
		VMUUID: "018f4b8e-1234-4abc-9def-0123456789ab", Phase: state.Stopped, Generation: 1,
		SpecHash: metaHash, Invocation: qemu.Invocation{Binary: "/fixture/qemu-system-aarch64", Args: []string{"-accel", "hvf"}},
		CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	manager := planFixtureManager(t)
	manager.Nodes = []string{"node-1"}
	plan, err := manager.Plan(context.Background(), resolved)
	if err != nil || plan.Action != "converge" || strings.Join(plan.Nodes, ",") != "node-1" || strings.Join(plan.Create, ",") != "node-1" || len(plan.Recreate) != 0 || len(plan.Missing) != 0 {
		t.Fatalf("selected partial plan = %#v, %v", plan, err)
	}
}

func TestPrivateDriftPlansRecreateAndUpReturnsTypedConflict(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	persisted := singlePrivateResolved()
	persistedHash, err := spec.Hash(persisted)
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Root: root}
	if err := store.WriteDeployment(state.DeploymentState{Schema: state.DeploymentSchema, BarnVersion: "test", SpecHash: persistedHash, Resolved: persisted, UpdatedAt: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	metaHash, err := spec.NodeHash(persisted, "meta")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteNode(state.NodeState{
		Schema: state.NodeSchema, BarnVersion: "test", Node: "meta",
		VMUUID: "018f4b8e-1234-4abc-9def-0123456789ab", Phase: state.Stopped, Generation: 1,
		SpecHash: metaHash, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	requested := cloneResolved(persisted)
	requested.Nodes[0].CPUs++
	manager := planFixtureManager(t)
	plan, err := manager.Plan(context.Background(), requested)
	if err != nil || plan.Action != "recreate" || !plan.Destructive {
		t.Fatalf("drift plan=%#v err=%v", plan, err)
	}
	if _, err := manager.Up(context.Background(), requested); !errors.Is(err, ErrRecreateRequired) {
		t.Fatalf("drift up error=%T %v, want ErrRecreateRequired", err, err)
	}
	// Reload is stop + up; it must refuse the same drift before anything
	// stops, and refuse removed nodes the same way up does.
	if _, err := manager.Reload(context.Background(), requested); !errors.Is(err, ErrRecreateRequired) {
		t.Fatalf("drift reload error=%T %v, want ErrRecreateRequired", err, err)
	}
	removed := cloneResolved(persisted)
	removed.Nodes = []spec.Node{{Name: "node-1", Control: true, Address: "10.10.10.11", CPUs: 1, Memory: 512 << 20, RootDisk: 1 << 30}}
	if _, err := manager.Reload(context.Background(), removed); !errors.Is(err, ErrNodesRemoved) {
		t.Fatalf("removed-node reload error=%T %v, want ErrNodesRemoved", err, err)
	}
	if after, err := store.ReadDeployment(); err != nil || after.SpecHash != persistedHash {
		t.Fatalf("drift planning/up/reload mutated deployment: %#v err=%v", after, err)
	}
	if node, err := store.ReadNode("meta"); err != nil || node.Phase != state.Stopped || node.Generation != 1 {
		t.Fatalf("drift reload touched node state: %#v err=%v", node, err)
	}
}

func TestPrivatePlanWorksBeforeHostNetworkSetup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	t.Setenv("BARN_HOME", root)
	profile, err := platform.Resolve("darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	manager := Manager{
		NativeProfile: func() (platform.Profile, error) { return profile, nil },
		NetworkPreflight: func(context.Context, platform.Profile, netpreflight.Request, execx.Runner) netpreflight.Report {
			return netpreflight.Report{Ready: false, ExitCode: 4, Findings: []netpreflight.Finding{{Code: "installation.network_mismatch", Severity: netpreflight.Error, Class: netpreflight.State, Evidence: "installed=10.10.10.0/24 requested=172.31.251.0/24"}}}
		},
		HostPreflight: func(context.Context, platform.Profile, *spec.PrivateNetwork, execx.Runner) (Backend, error) {
			t.Fatal("host backend preflight ran after typed network mismatch")
			return Backend{}, nil
		},
	}
	requested := singlePrivateResolved()
	requested.Private = &spec.PrivateNetwork{CIDR: "172.31.251.0/24", HostAddress: "172.31.251.1", DHCPEnd: "172.31.251.8"}
	for index := range requested.Nodes {
		requested.Nodes[index].Address = "172.31.251.1" + fmt.Sprintf("%d", index)
	}
	if plan, err := manager.Plan(context.Background(), requested); err != nil || plan.Action != "create" {
		t.Fatalf("offline plan = %#v, %v", plan, err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("network mismatch created the data root: %v", err)
	}
}

func TestRestartAndRecreatePreflightBeforeLifecycleMutation(t *testing.T) {
	for _, command := range []string{"restart", "recreate"} {
		t.Run(command, func(t *testing.T) {
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
			profile, err := platform.Resolve("darwin", "arm64")
			if err != nil {
				t.Fatal(err)
			}
			preflightCalls := 0
			manager := Manager{
				NativeProfile: func() (platform.Profile, error) { return profile, nil },
				NetworkPreflight: func(context.Context, platform.Profile, netpreflight.Request, execx.Runner) netpreflight.Report {
					preflightCalls++
					return netpreflight.Report{Ready: false, ExitCode: 4, Findings: []netpreflight.Finding{{Code: "installation.network_mismatch", Severity: netpreflight.Error, Class: netpreflight.State, Evidence: "mismatch"}}}
				},
				HostPreflight: func(context.Context, platform.Profile, *spec.PrivateNetwork, execx.Runner) (Backend, error) {
					t.Fatal("host preflight ran after the typed gate failed")
					return Backend{}, nil
				},
			}
			if command == "restart" {
				_, err = manager.Restart(context.Background())
			} else {
				_, err = manager.Recreate(context.Background())
			}
			var preflightErr *NetworkPreflightError
			if !errors.As(err, &preflightErr) || preflightCalls != 1 {
				t.Fatalf("command=%s calls=%d error=%T %v", command, preflightCalls, err, err)
			}
			if _, err := store.ReadDeployment(); err != nil {
				t.Fatalf("command=%s mutated/destroyed deployment state after failed preflight: %v", command, err)
			}
		})
	}
}

type rejectingPrivateShareRunner struct {
	binaries []string
}

func (runner *rejectingPrivateShareRunner) Run(_ context.Context, binary string, args ...string) (execx.Result, error) {
	if len(args) == 1 && args[0] == "--version" {
		return execx.Result{Stdout: []byte("QEMU emulator version 11.1.0\n")}, nil
	}
	if len(args) == 4 && args[0] == "-machine" && args[1] == "none" && args[2] == "-netdev" && args[3] == "help" {
		return execx.Result{Stdout: []byte("stream\n")}, nil
	}
	if len(args) != 2 || args[0] != "-device" || args[1] != "help" {
		return execx.Result{}, fmt.Errorf("unexpected command %s %v", binary, args)
	}
	runner.binaries = append(runner.binaries, binary)
	return execx.Result{Stdout: []byte("virtio-net-pci\n")}, nil
}

func privateShareFixture(t *testing.T) spec.Share {
	t.Helper()
	source := filepath.Join(t.TempDir(), "share-source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	return spec.Share{Host: canonical, Guest: "/mnt/source", Readonly: true}
}

func persistPrivateShare(t *testing.T, store state.Store, share spec.Share) state.DeploymentState {
	t.Helper()
	deploymentState, err := store.ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	deploymentState.Resolved.Nodes[0].Shares = []spec.Share{share}
	deploymentState.SpecHash, err = spec.Hash(deploymentState.Resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteDeployment(deploymentState); err != nil {
		t.Fatal(err)
	}
	node, err := store.ReadNode(deploymentState.Resolved.Nodes[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	node.SpecHash = deploymentState.SpecHash
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	return deploymentState
}

func privateShareCapabilityManager(t *testing.T, fixture StartConfig, runner execx.Runner) Manager {
	t.Helper()
	profile, err := platform.Resolve("darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	return Manager{
		Runner:        runner,
		NativeProfile: func() (platform.Profile, error) { return profile, nil },
		NetworkPreflight: func(context.Context, platform.Profile, netpreflight.Request, execx.Runner) netpreflight.Report {
			return netpreflight.Report{Ready: true}
		},
		HostPreflight: func(context.Context, platform.Profile, *spec.PrivateNetwork, execx.Runner) (Backend, error) {
			return Backend{DarwinSocket: "/fixture/vmnet.sock"}, nil
		},
		LookPath: func(name string) (string, error) { return "/fixture/" + name, nil },
		FirmwareLookup: func(platform.Profile, string) (platform.Firmware, error) {
			return platform.Firmware{Code: "/fixture/code.fd", Vars: "/fixture/vars.fd"}, nil
		},
		ResolveImage: func(_ context.Context, alias, arch string) (image.Entry, string, image.Metadata, error) {
			return image.Entry{Alias: alias, Release: "test", Arch: arch, Boot: "uefi", SHA256: strings.Repeat("a", 64)}, "/fixture/base.qcow2", image.Metadata{VirtualSize: 8 * spec.GiB}, nil
		},
	}
}

func assertPrivateShareCapabilityFailurePreservesState(t *testing.T, fixture StartConfig, beforeNode state.NodeState, err error) {
	t.Helper()
	var capability *CapabilityError
	if !errors.As(err, &capability) {
		t.Fatalf("error=%T %v, want CapabilityError", err, err)
	}
	store := state.Store{Root: fixture.Deployment.Root}
	afterNode, readErr := store.ReadNode(beforeNode.Node)
	if readErr != nil || afterNode.Phase != beforeNode.Phase || afterNode.SpecHash != beforeNode.SpecHash {
		t.Fatalf("node mutated after failed share preflight: before=%#v after=%#v err=%v", beforeNode, afterNode, readErr)
	}
	if _, readErr := store.ReadDeployment(); readErr != nil {
		t.Fatalf("deployment state disappeared after failed share preflight: %v", readErr)
	}
}

func TestPrivateRestartShareCapabilityPrecedesStop(t *testing.T) {
	fixture, _ := preparedStartFixture(t)
	t.Setenv("BARN_HOME", fixture.Deployment.Root)
	store := state.Store{Root: fixture.Deployment.Root}
	persistPrivateShare(t, store, privateShareFixture(t))
	beforeNode, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	runner := &rejectingPrivateShareRunner{}
	_, err = privateShareCapabilityManager(t, fixture, runner).Restart(context.Background())
	assertPrivateShareCapabilityFailurePreservesState(t, fixture, beforeNode, err)
	if len(runner.binaries) != 1 || runner.binaries[0] != beforeNode.Invocation.Binary {
		t.Fatalf("share probes=%v, want persisted binary %q", runner.binaries, beforeNode.Invocation.Binary)
	}
}

func TestPrivateRecreateShareCapabilityPrecedesDestroy(t *testing.T) {
	for _, test := range []struct {
		name           string
		currentShare   bool
		requestedShare bool
		requestedArch  string
		wantBinary     string
	}{
		{name: "add-share", requestedShare: true, wantBinary: "/fixture/qemu-system-aarch64"},
		{name: "foreign-add-share", requestedShare: true, requestedArch: "amd64", wantBinary: "/fixture/qemu-system-x86_64"},
		{name: "remove-share", currentShare: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, _ := preparedStartFixture(t)
			t.Setenv("BARN_HOME", fixture.Deployment.Root)
			store := state.Store{Root: fixture.Deployment.Root}
			share := privateShareFixture(t)
			deploymentState, err := store.ReadDeployment()
			if err != nil {
				t.Fatal(err)
			}
			if test.currentShare {
				deploymentState = persistPrivateShare(t, store, share)
			}
			requested := cloneResolved(deploymentState.Resolved)
			requested.Arch = test.requestedArch
			if test.requestedShare {
				requested.Nodes[0].Shares = []spec.Share{share}
			} else {
				requested.Nodes[0].Shares = nil
			}
			beforeNode, err := store.ReadNode("meta")
			if err != nil {
				t.Fatal(err)
			}
			runner := &rejectingPrivateShareRunner{}
			_, err = privateShareCapabilityManager(t, fixture, runner).RecreateResolved(context.Background(), requested)
			assertPrivateShareCapabilityFailurePreservesState(t, fixture, beforeNode, err)
			wantBinary := test.wantBinary
			if wantBinary == "" {
				wantBinary = beforeNode.Invocation.Binary
			}
			if len(runner.binaries) != 1 || runner.binaries[0] != wantBinary {
				t.Fatalf("share probes=%v, want %q", runner.binaries, wantBinary)
			}
		})
	}
}

func TestPrivateRecreateRuntimePolicyPrecedesDestroy(t *testing.T) {
	fixture, _ := preparedStartFixture(t)
	t.Setenv("BARN_HOME", fixture.Deployment.Root)
	store := state.Store{Root: fixture.Deployment.Root}
	deploymentState, err := store.ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	beforeNode, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	requested := cloneResolved(deploymentState.Resolved)
	requested.Image = "el7"
	_, err = privateShareCapabilityManager(t, fixture, &rejectingPrivateShareRunner{}).RecreateResolved(context.Background(), requested)
	assertPrivateShareCapabilityFailurePreservesState(t, fixture, beforeNode, err)
}

func TestPrivateRecreateRuntimeDriftRequiresWholeDeployment(t *testing.T) {
	fixture, _ := preparedStartFixture(t)
	t.Setenv("BARN_HOME", fixture.Deployment.Root)
	store := state.Store{Root: fixture.Deployment.Root}
	deploymentState, err := store.ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	beforeNode, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	requested := cloneResolved(deploymentState.Resolved)
	requested.Image = "el8"
	manager := privateShareCapabilityManager(t, fixture, &rejectingPrivateShareRunner{})
	manager.Nodes = []string{"meta"}
	_, err = manager.RecreateResolved(context.Background(), requested)
	if !errors.Is(err, ErrRecreateRequired) {
		t.Fatalf("partial runtime recreate error = %v", err)
	}
	afterNode, readErr := store.ReadNode("meta")
	if readErr != nil || afterNode.Phase != beforeNode.Phase || afterNode.SpecHash != beforeNode.SpecHash {
		t.Fatalf("node mutated after partial runtime recreate: before=%#v after=%#v err=%v", beforeNode, afterNode, readErr)
	}
}

func TestPrivateRecreateSelectedRuntimeInputsPrecedeDestroy(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Manager, *spec.Resolved)
		match  string
	}{
		{
			name: "foreign emulator missing",
			mutate: func(manager *Manager, requested *spec.Resolved) {
				requested.Arch = "amd64"
				manager.LookPath = func(name string) (string, error) {
					if name == "qemu-system-x86_64" {
						return "", errors.New("foreign emulator absent")
					}
					return "/fixture/" + name, nil
				}
			},
			match: "foreign emulator absent",
		},
		{
			name: "firmware missing",
			mutate: func(manager *Manager, _ *spec.Resolved) {
				manager.FirmwareLookup = func(platform.Profile, string) (platform.Firmware, error) {
					return platform.Firmware{}, errors.New("firmware absent")
				}
			},
			match: "firmware absent",
		},
		{
			name: "image unavailable",
			mutate: func(manager *Manager, _ *spec.Resolved) {
				manager.ResolveImage = func(context.Context, string, string) (image.Entry, string, image.Metadata, error) {
					return image.Entry{}, "", image.Metadata{}, errors.New("image unavailable")
				}
			},
			match: "image unavailable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, _ := preparedStartFixture(t)
			t.Setenv("BARN_HOME", fixture.Deployment.Root)
			store := state.Store{Root: fixture.Deployment.Root}
			deploymentState, err := store.ReadDeployment()
			if err != nil {
				t.Fatal(err)
			}
			beforeNode, err := store.ReadNode("meta")
			if err != nil {
				t.Fatal(err)
			}
			requested := cloneResolved(deploymentState.Resolved)
			manager := privateShareCapabilityManager(t, fixture, &rejectingPrivateShareRunner{})
			test.mutate(&manager, &requested)
			_, err = manager.RecreateResolved(context.Background(), requested)
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("recreate error = %v, want %q", err, test.match)
			}
			afterNode, readErr := store.ReadNode("meta")
			if readErr != nil || afterNode.Phase != beforeNode.Phase || afterNode.SpecHash != beforeNode.SpecHash {
				t.Fatalf("node mutated after failed runtime preflight: before=%#v after=%#v err=%v", beforeNode, afterNode, readErr)
			}
		})
	}
}

func TestPrivatePlanDoesNotRequireForeignRuntimeInstallation(t *testing.T) {
	t.Setenv("BARN_HOME", t.TempDir())
	resolved := singlePrivateResolved()
	resolved.Arch = "amd64"
	profile, _ := platform.Resolve("darwin", "arm64")
	manager := Manager{
		NativeProfile: func() (platform.Profile, error) { return profile, nil },
		NetworkPreflight: func(context.Context, platform.Profile, netpreflight.Request, execx.Runner) netpreflight.Report {
			return netpreflight.Report{Ready: true}
		},
		HostPreflight: func(context.Context, platform.Profile, *spec.PrivateNetwork, execx.Runner) (Backend, error) {
			return Backend{DarwinSocket: "/fixture/vmnet.sock"}, nil
		},
		LookPath: func(name string) (string, error) { return "", fmt.Errorf("missing %s", name) },
	}
	if plan, err := manager.Plan(context.Background(), resolved); err != nil || plan.Action != "create" || len(plan.Create) != 1 {
		t.Fatalf("foreign offline plan = %#v, %v", plan, err)
	}
}

func TestBoundedConcurrency(t *testing.T) {
	t.Parallel()
	for nodes, want := range map[int]int{0: 1, 1: 1, 2: 2, 4: 4, 20: 4} {
		if got := boundedConcurrency(nodes); got != want {
			t.Errorf("boundedConcurrency(%d)=%d, want %d", nodes, got, want)
		}
	}
}

func TestManagerUsesResolvedReadinessTimeout(t *testing.T) {
	t.Parallel()
	resolved := singlePrivateResolved()
	resolved.SSHWaitTimeoutNS = int64(750 * time.Millisecond)
	timeout, err := (Manager{}).readyTimeout(resolved)
	if err != nil || timeout != 750*time.Millisecond {
		t.Fatalf("resolved timeout = %s, %v", timeout, err)
	}
	timeout, err = (Manager{ReadyTimeout: 2 * time.Second}).readyTimeout(resolved)
	if err != nil || timeout != 2*time.Second {
		t.Fatalf("manager override timeout = %s, %v", timeout, err)
	}
	resolved.SSHWaitTimeoutNS = -1
	if _, err := (Manager{}).readyTimeout(resolved); err == nil {
		t.Fatal("negative resolved timeout accepted")
	}
}

func TestSelectedNodeNamesRejectUnknownAndDuplicate(t *testing.T) {
	t.Parallel()
	resolved := singlePrivateResolved()
	resolved.Nodes = append(resolved.Nodes, spec.Node{Name: "node-1", Address: "10.10.10.11", CPUs: 1, Memory: spec.GiB, RootDisk: 8 * spec.GiB})
	selected, err := selectedNodeNames(resolved, []string{"node-1"})
	if err != nil || len(selected) != 1 || selected[0] != "node-1" {
		t.Fatalf("selected=%v err=%v", selected, err)
	}
	if _, err := selectedNodeNames(resolved, []string{"missing"}); err == nil {
		t.Fatal("unknown private node selection was accepted")
	}
	if _, err := selectedNodeNames(resolved, []string{"meta", "meta"}); err == nil {
		t.Fatal("duplicate private node selection was accepted")
	}
}

func TestEnsureSSHAddressesUnusedDetectsConflict(t *testing.T) {
	t.Parallel()
	nodes := []spec.Node{{Name: "meta", Address: "10.10.10.10"}}
	if err := ensureSSHAddressesUnusedWithDial(nodes, func(_, address string) (net.Conn, error) {
		if address != "10.10.10.10:22" {
			t.Fatalf("probe address = %s", address)
		}
		left, right := net.Pipe()
		_ = right.Close()
		return left, nil
	}); err == nil {
		t.Fatal("existing SSH listener was not reported as a conflict")
	}
	if err := ensureSSHAddressesUnusedWithDial(nodes, func(string, string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}); err != nil {
		t.Fatalf("unused address rejected: %v", err)
	}
}

func TestInvocationRuntimeObservability(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		binary string
		args   []string
		arch   string
	}{
		{binary: "/opt/homebrew/bin/qemu-system-aarch64", arch: "arm64"},
		{binary: "/usr/bin/qemu-system-x86_64", arch: "amd64"},
		{binary: "/usr/libexec/qemu-kvm", args: []string{"-machine", "q35"}, arch: "amd64"},
		{binary: "/usr/libexec/qemu-kvm", args: []string{"-machine", "virt,gic-version=max"}, arch: "arm64"},
	} {
		if got := invocationGuestArch(test.binary, test.args); got != test.arch {
			t.Errorf("guest arch for %s %v = %q, want %q", test.binary, test.args, got, test.arch)
		}
	}
	if got := invocationOption([]string{"-machine", "q35", "-accel", "tcg,thread=multi"}, "-accel"); got != "tcg,thread=multi" {
		t.Fatalf("accelerator option = %q", got)
	}
}

type runtimeProbeRunner struct {
	version string
	netdev  string
}

func (runner runtimeProbeRunner) Run(_ context.Context, _ string, args ...string) (execx.Result, error) {
	if len(args) == 1 && args[0] == "--version" {
		return execx.Result{Stdout: []byte(runner.version)}, nil
	}
	if len(args) == 4 && args[0] == "-machine" && args[1] == "none" && args[2] == "-netdev" && args[3] == "help" {
		return execx.Result{Stdout: []byte(runner.netdev)}, nil
	}
	return execx.Result{}, fmt.Errorf("unexpected runtime probe %v", args)
}

func TestResolveRuntimeQEMUValidatesSelectedBinaryAndBackend(t *testing.T) {
	t.Parallel()
	host, _ := platform.Resolve("darwin", "arm64")
	foreign, _ := platform.ResolveRuntime(host, "amd64", false)
	backend := Backend{DarwinSocket: "/fixture/vmnet.sock", NetworkCIDR: "10.10.10.0/24", HostAddress: "10.10.10.1", DHCPEnd: "10.10.10.8"}
	manager := Manager{
		Runner:   runtimeProbeRunner{version: "QEMU emulator version 11.1.0\n", netdev: "stream\n"},
		LookPath: func(string) (string, error) { return "/fixture/qemu-system-x86_64", nil },
	}
	path, selected, err := manager.resolveRuntimeQEMU(context.Background(), foreign, backend)
	if err != nil || path != "/fixture/qemu-system-x86_64" || selected.ReconnectMS != 1000 || selected.DarwinUseFD || selected.NetworkCIDR != backend.NetworkCIDR {
		t.Fatalf("selected runtime QEMU = %q %#v %v", path, selected, err)
	}
	manager.Runner = runtimeProbeRunner{version: "QEMU emulator version 8.1.0\n", netdev: "stream\n"}
	if _, _, err := manager.resolveRuntimeQEMU(context.Background(), foreign, backend); err == nil || !strings.Contains(err.Error(), "minimum") {
		t.Fatalf("old selected QEMU error = %v", err)
	}
	manager.Runner = runtimeProbeRunner{version: "QEMU emulator version 11.1.0\n", netdev: "tap\n"}
	if _, selected, err := manager.resolveRuntimeQEMU(context.Background(), foreign, backend); err != nil || !selected.DarwinUseFD || selected.ReconnectMS != 0 {
		t.Fatalf("selected QEMU FD fallback = %#v %v", selected, err)
	}
}

func TestRuntimeDriftUsesPersistedInvocation(t *testing.T) {
	fixture, _ := preparedStartFixture(t)
	store := state.Store{Root: fixture.Deployment.Root}
	deploymentState, err := store.ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	node, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	node.Invocation = qemu.Invocation{Binary: "/opt/homebrew/bin/qemu-system-aarch64", Args: []string{"-machine", "virt", "-accel", "hvf"}}
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	host, _ := platform.Resolve("darwin", "arm64")
	if drifted, err := runtimeDriftNodes(store, deploymentState.Resolved, host); err != nil || len(drifted) != 0 {
		t.Fatalf("native runtime drift = %v, %v", drifted, err)
	}
	tcg, _ := platform.ResolveRuntime(host, "arm64", true)
	if drifted, err := runtimeDriftNodes(store, deploymentState.Resolved, tcg); err != nil || len(drifted) != len(deploymentState.Resolved.Nodes) || drifted[0] != "meta" {
		t.Fatalf("TCG runtime drift = %v, %v", drifted, err)
	}
}

func TestSelectRuntimeCompatibilityPolicy(t *testing.T) {
	t.Parallel()
	darwinArm, _ := platform.Resolve("darwin", "arm64")
	linuxAMD, _ := platform.Resolve("linux", "amd64")
	linuxArm, _ := platform.Resolve("linux", "arm64")
	cases := []struct {
		name     string
		host     platform.Profile
		resolved spec.Resolved
		arch     string
		accel    string
		wantErr  bool
	}{
		{name: "primary native", host: darwinArm, resolved: spec.Resolved{Image: "el9"}, arch: "arm64", accel: "hvf"},
		{name: "EL8 Apple TCG", host: darwinArm, resolved: spec.Resolved{Image: "el8"}, arch: "arm64", accel: "tcg,thread=multi"},
		{name: "EL8 channel keeps policy", host: darwinArm, resolved: spec.Resolved{Image: "el8:stable"}, arch: "arm64", accel: "tcg,thread=multi"},
		{name: "explicit foreign arch", host: darwinArm, resolved: spec.Resolved{Image: "el9", Arch: "amd64"}, arch: "amd64", accel: "tcg,thread=single"},
		{name: "EL8 Linux native", host: linuxAMD, resolved: spec.Resolved{Image: "el8"}, arch: "amd64", accel: "kvm"},
		{name: "EL8 Linux arm native", host: linuxArm, resolved: spec.Resolved{Image: "el8"}, arch: "arm64", accel: "kvm"},
		{name: "explicit arm on Linux amd64", host: linuxAMD, resolved: spec.Resolved{Image: "el9", Arch: "arm64"}, arch: "arm64", accel: "tcg,thread=multi"},
		{name: "EL7 Linux native", host: linuxAMD, resolved: spec.Resolved{Image: "el7"}, arch: "amd64", accel: "kvm"},
		{name: "EL7 Linux arm refused", host: linuxArm, resolved: spec.Resolved{Image: "el7", Arch: "amd64"}, wantErr: true},
		{name: "EL7 macOS refused", host: darwinArm, resolved: spec.Resolved{Image: "el7", Arch: "amd64"}, wantErr: true},
		{name: "EL7 pinned alias refused", host: darwinArm, resolved: spec.Resolved{Image: "c7@7.9.20221112.0", Arch: "amd64"}, wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			decision, err := selectRuntime(test.host, test.resolved)
			if (err != nil) != test.wantErr {
				t.Fatalf("select runtime = %#v, %v", decision, err)
			}
			if err == nil && (decision.Profile.Arch != test.arch || decision.Profile.Accelerator != test.accel) {
				t.Fatalf("select runtime = %#v", decision)
			}
		})
	}
}

func TestResolveBasesKeepsBareAndPinnedReferencesDistinct(t *testing.T) {
	t.Parallel()
	manager := Manager{ResolveImage: func(_ context.Context, reference, arch string) (image.Entry, string, image.Metadata, error) {
		release := "new"
		digest := strings.Repeat("a", 64)
		if strings.Contains(reference, "@old") {
			release = "old"
			digest = strings.Repeat("b", 64)
		}
		return image.Entry{Alias: "d13", Release: release, Arch: arch, Boot: "uefi", SHA256: digest}, "/fixture/" + release + ".qcow2", image.Metadata{VirtualSize: 4 * spec.GiB}, nil
	}}
	profile, _ := platform.Resolve("linux", "arm64")
	resolved := spec.Resolved{
		Image: "d13",
		Nodes: []spec.Node{
			{Name: "current"},
			{Name: "pinned", Image: "d13@old"},
		},
	}
	bases, _, err := manager.resolveBases(context.Background(), profile, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if bases["d13"].Release != "new" || bases["d13"].Digest != strings.Repeat("a", 64) {
		t.Fatalf("bare base was overwritten: %#v", bases["d13"])
	}
	if bases["d13@old"].Release != "old" || bases["d13@old"].Digest != strings.Repeat("b", 64) {
		t.Fatalf("pinned base was lost: %#v", bases["d13@old"])
	}
}
