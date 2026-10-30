package private

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

func retiredImageFixture(t *testing.T, pinned bool) (StartConfig, spec.Resolved, []state.NodeState) {
	t.Helper()
	fixture, nodes := preparedStartFixture(t)
	t.Setenv("BARN_HOME", fixture.Deployment.Root)
	store := state.Store{Root: fixture.Deployment.Root}
	applied, err := store.ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	if pinned {
		applied.Resolved.Image = "u24@test"
	}
	applied.SpecHash, err = spec.Hash(applied.Resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteDeployment(applied); err != nil {
		t.Fatal(err)
	}
	for i := range nodes {
		nodes[i].SpecHash, err = spec.NodeHash(applied.Resolved, nodes[i].Node)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.WriteNode(nodes[i]); err != nil {
			t.Fatal(err)
		}
	}
	return fixture, applied.Resolved, nodes
}

func assertCatalogOperationsPreserveFiles(t *testing.T, fixture StartConfig, nodes []state.NodeState) func() {
	t.Helper()
	paths := []string{filepath.Join(fixture.Deployment.Root, "state.json"), filepath.Join(fixture.Deployment.Root, "base.qcow2")}
	for _, node := range nodes {
		paths = append(paths, filepath.Join(fixture.Deployment.Root, "nodes", node.Node, "state.json"), node.RootDisk, node.Seed, node.NVRAM)
		for _, disk := range node.DataDisks {
			paths = append(paths, disk.Path)
		}
	}
	before := make(map[string][]byte)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}
	return func() {
		t.Helper()
		for path, original := range before {
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(original, data) {
				t.Errorf("existing state/artifact changed: %s: %v", path, err)
			}
		}
	}
}

func TestPlanAndReloadUseCommittedImagesAfterCatalogRotation(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		name := "moving-alias"
		if pinned {
			name = "pinned-release"
		}
		t.Run(name, func(t *testing.T) {
			fixture, resolved, nodes := retiredImageFixture(t, pinned)
			defer assertCatalogOperationsPreserveFiles(t, fixture, nodes)()
			manager := privateShareCapabilityManager(t, fixture, &rejectingPrivateShareRunner{})
			manager.LookupImage = func(context.Context, string, string) (image.Entry, error) {
				t.Error("unchanged nodes consulted the active catalog")
				return image.Entry{}, errors.New("retired release")
			}
			manager.ResolveImage = func(context.Context, string, string) (image.Entry, string, image.Metadata, error) {
				t.Error("unchanged nodes tried to download an image")
				return image.Entry{}, "", image.Metadata{}, errors.New("unexpected download")
			}
			for _, selected := range [][]string{nil, {"node-1"}} {
				manager.Nodes = selected
				plan, err := manager.Plan(context.Background(), resolved)
				if err != nil || plan.Action != "start" || len(plan.Create) != 0 || strings.Join(plan.Images, ",") != "u24@test/arm64" {
					t.Fatalf("selected=%v: plan=%#v, %v", selected, plan, err)
				}
				if _, _, err := manager.preflightBoot(context.Background(), resolved, nil); err != nil {
					t.Fatalf("existing-node preflight: %v", err)
				}
			}
			// Reach the real reload stop boundary without running or stopping a VM.
			held, err := acquireDeploymentLock(context.Background(), fixture.Deployment.Root, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = held.Release() }()
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			_, err = manager.Reload(ctx, resolved)
			if _, reason, _ := failure.Classify(err); reason != "deployment_busy" {
				t.Fatalf("reload did not reach the deployment lock: %v", err)
			}
			if manager.imageSession != nil {
				t.Fatal("existing nodes opened an unnecessary catalog session")
			}
		})
	}
}

func TestCatalogRotationResolvesOnlyAddedNodes(t *testing.T) {
	fixture, resolved, nodes := retiredImageFixture(t, true)
	defer assertCatalogOperationsPreserveFiles(t, fixture, nodes)()
	resolved.Nodes = append(resolved.Nodes, spec.Node{Name: "node-2", Image: "u24", Address: "10.10.10.12", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB})
	manager := privateShareCapabilityManager(t, fixture, &rejectingPrivateShareRunner{})
	manager.DialSSHAddress = planFixtureManager(t).DialSSHAddress
	manager.LookupImage = func(_ context.Context, alias, arch string) (image.Entry, error) {
		if alias != "u24" {
			t.Fatalf("looked up retired peer image %q", alias)
		}
		return image.Entry{Alias: alias, Release: "current", Arch: arch, Boot: "uefi"}, nil
	}
	downloadFailure := errors.New("new image download unavailable")
	manager.ResolveImage = func(_ context.Context, alias, _ string) (image.Entry, string, image.Metadata, error) {
		if alias != "u24" {
			t.Fatalf("downloaded retired peer image %q", alias)
		}
		return image.Entry{}, "", image.Metadata{}, downloadFailure
	}
	plan, err := manager.Plan(context.Background(), resolved)
	if err != nil || plan.Action != "converge" || strings.Join(plan.Create, ",") != "node-2" || strings.Join(plan.Images, ",") != "u24@test/arm64,u24@current/arm64" {
		t.Fatalf("expanded plan=%#v, %v", plan, err)
	}
	for _, selected := range [][]string{nil, {"node-2"}} {
		manager.Nodes = selected
		for _, operation := range []func(context.Context, spec.Resolved) (Status, error){manager.Reload, manager.Up} {
			if _, err := operation(context.Background(), resolved); !errors.Is(err, downloadFailure) {
				t.Fatalf("selected=%v: did not reach new image download: %v", selected, err)
			}
		}
	}
	// Replacing one selected node must not resolve a retired unchanged peer.
	resolved.Nodes = resolved.Nodes[:len(nodes)]
	resolved.Nodes[0].Image = "u24"
	manager.Nodes = []string{"meta"}
	if _, err := manager.RecreateResolved(context.Background(), resolved); !errors.Is(err, downloadFailure) {
		t.Fatalf("selected replacement did not reach its image download: %v", err)
	}
}

func TestRetiredImagesStillRequireCatalogForNewOrReplacedNodes(t *testing.T) {
	for _, scenario := range []string{"new", "changed-image", "changed-resources", "explicit-recreate"} {
		t.Run(scenario, func(t *testing.T) {
			fixture, resolved, nodes := retiredImageFixture(t, true)
			defer assertCatalogOperationsPreserveFiles(t, fixture, nodes)()
			manager := privateShareCapabilityManager(t, fixture, &rejectingPrivateShareRunner{})
			retired := errors.New("requested release is absent from the active catalog")
			manager.LookupImage = func(context.Context, string, string) (image.Entry, error) {
				return image.Entry{}, retired
			}
			switch scenario {
			case "new":
				resolved.Nodes = append(resolved.Nodes, spec.Node{Name: "node-2", Address: "10.10.10.12", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB})
			case "changed-image":
				resolved.Nodes[0].Image = "u24@other-missing-release"
			case "changed-resources":
				resolved.Nodes[0].Memory *= 2
			}
			if scenario == "explicit-recreate" {
				manager.Nodes = []string{"meta"}
				if _, err := manager.RecreateResolved(context.Background(), resolved); !errors.Is(err, retired) {
					t.Fatalf("recreate did not validate its image before destruction: %v", err)
				}
			} else if _, err := manager.Plan(context.Background(), resolved); !errors.Is(err, retired) {
				t.Fatalf("new/replaced node reused an old image: %v", err)
			}
		})
	}
}

func TestCatalogRotationStillChecksCommittedBootCompatibility(t *testing.T) {
	fixture, resolved, nodes := retiredImageFixture(t, true)
	store := state.Store{Root: fixture.Deployment.Root}
	for _, node := range nodes {
		node.NVRAM = ""
		node.Invocation.Binary = "/fixture/qemu-system-x86_64"
		node.Invocation.Args = []string{"-machine", "q35", "-accel", "tcg"}
		if err := store.WriteNode(node); err != nil {
			t.Fatal(err)
		}
	}
	manager := planFixtureManager(t)
	manager.NativeProfile = func() (platform.Profile, error) { return platform.Resolve("linux", "amd64") }
	retired := errors.New("committed image is absent from the active catalog")
	manager.LookupImage = func(_ context.Context, alias, arch string) (image.Entry, error) {
		return image.Entry{}, retired
	}
	plan, err := manager.Plan(context.Background(), resolved)
	if err != nil || plan.Action != "recreate" || strings.Join(plan.Images, ",") != "u24@test/amd64" {
		t.Fatalf("existing BIOS plan=%#v, %v", plan, err)
	}
	resolved.Nodes = append(resolved.Nodes, spec.Node{Name: "node-2", Image: "u24", Address: "10.10.10.12", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 8 * spec.GiB})
	manager.LookupImage = func(_ context.Context, alias, arch string) (image.Entry, error) {
		if alias != "u24" {
			t.Fatalf("looked up unexpected image %q", alias)
		}
		return image.Entry{Alias: alias, Release: "current", Arch: arch, Boot: "uefi"}, nil
	}
	if _, err := manager.Plan(context.Background(), resolved); err == nil || !strings.Contains(err.Error(), "does not mix BIOS and UEFI") {
		t.Fatalf("incompatible boot modes accepted: %v", err)
	}
}
