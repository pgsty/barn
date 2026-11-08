package private

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/persistent"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

func writeDestroyKeyFixtures(t *testing.T, root string) string {
	t.Helper()
	keysDir := filepath.Join(root, "keys")
	if err := os.Mkdir(keysDir, 0o700); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"known_hosts": 0o600, "id_ed25519": 0o600, "id_ed25519.pub": 0o644} {
		if err := os.WriteFile(filepath.Join(keysDir, name), nil, mode); err != nil {
			t.Fatal(err)
		}
	}
	return keysDir
}

func TestPrivateDestroyRemovesOnlyNodesAndPreservesKeysStateCache(t *testing.T) {
	startConfig, nodes := preparedStartFixture(t)
	deploymentValue := startConfig.Deployment
	t.Setenv("BARN_HOME", deploymentValue.Root)
	keysDir := writeDestroyKeyFixtures(t, deploymentValue.Root)
	metaDir, err := deploymentValue.NodeDir("meta")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, ".DS_Store"), []byte("finder"), 0o600); err != nil {
		t.Fatal(err)
	}
	latest := make(map[string]activity.Event)
	manager := Manager{BarnVersion: "test", Progress: func(event activity.Event) {
		if event.Node == "" {
			return
		}
		latest[event.Node] = event
		if event.State == "removed" {
			directory, _ := deploymentValue.NodeDir(event.Node)
			if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("reported removal before node directory was gone: %s: %v", directory, err)
			}
		}
	}}
	result, err := manager.Destroy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != len(nodes) {
		t.Fatalf("destroy result = %#v", result)
	}
	for _, node := range nodes {
		if event := latest[node.Node]; event.State != "removed" || !event.Done || event.Address == "" {
			t.Fatalf("destroy result lost its state or address: %+v", event)
		}
		directory, _ := deploymentValue.NodeDir(node.Node)
		if _, err := os.Lstat(directory); !os.IsNotExist(err) {
			t.Fatalf("destroyed node directory remains: %s: %v", directory, err)
		}
	}
	for _, path := range []string{keysDir, filepath.Join(keysDir, "id_ed25519"), filepath.Join(deploymentValue.Root, "base.qcow2")} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("preserved private artifact missing %s: %v", path, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(deploymentValue.Root, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("full destroy must remove the deployment state document: %v", err)
	}
}

func TestDestroyProgressPreservesFailureAndSelectedScope(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	deployment := startConfig.Deployment
	t.Setenv("BARN_HOME", deployment.Root)
	writeDestroyKeyFixtures(t, deployment.Root)
	directory, _ := deployment.NodeDir("node-1")
	sentinel := filepath.Join(directory, "unexpected-file")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	latest := make(map[string]activity.Event)
	manager := Manager{BarnVersion: "test", Nodes: []string{"node-1"}, Progress: func(event activity.Event) {
		if event.Node != "" {
			latest[event.Node] = event
		}
	}}
	if _, err := manager.DestroyNodes(context.Background()); err == nil {
		t.Fatal("unexpected artifact did not prevent destruction")
	}
	if event := latest["node-1"]; len(latest) != 1 || event.State != "failed" || !strings.Contains(event.Message, "unexpected file") {
		t.Fatalf("destroy progress lost failure or included an unselected peer: %+v", latest)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("failed destroy changed the unexpected file: %q, %v", data, err)
	}
}

func TestPrivatePartialRecreateDestroyPreservesPeerState(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	deploymentValue := startConfig.Deployment
	t.Setenv("BARN_HOME", deploymentValue.Root)
	writeDestroyKeyFixtures(t, deploymentValue.Root)
	manager := Manager{BarnVersion: "test", Nodes: []string{"node-1"}, allowPartialDestroy: true}
	result, err := manager.Destroy(context.Background())
	if err != nil || len(result.Nodes) != 1 || result.Nodes[0].Name != "node-1" || result.Nodes[0].State != state.Absent {
		t.Fatalf("partial destroy result=%#v err=%v", result, err)
	}
	store := state.Store{Root: deploymentValue.Root}
	if _, err := store.ReadNode("meta"); err != nil {
		t.Fatalf("peer state was removed: %v", err)
	}
	if _, err := store.ReadNode("node-1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selected node state remains: %v", err)
	}
	if _, err := store.ReadDeployment(); err != nil {
		t.Fatalf("deployment state was removed: %v", err)
	}
}

func TestPrivateDestroyingControlElectsRemainingPeer(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	deploymentValue := startConfig.Deployment
	t.Setenv("BARN_HOME", deploymentValue.Root)
	writeDestroyKeyFixtures(t, deploymentValue.Root)
	if _, err := (Manager{BarnVersion: "test", Nodes: []string{"meta"}}).DestroyNodes(context.Background()); err != nil {
		t.Fatal(err)
	}
	stateValue, err := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	if len(stateValue.Resolved.Nodes) != 1 || stateValue.Resolved.Nodes[0].Name != "node-1" || !stateValue.Resolved.Nodes[0].Control {
		t.Fatalf("remaining control node = %#v", stateValue.Resolved.Nodes)
	}
}

func TestControllerRecreatesOnlyMissingSelectedNode(t *testing.T) {
	lifecycle := &fakeNodeLifecycle{failStart: map[string]bool{}, failReady: map[string]bool{}}
	controller := controllerFixture(t, &fakePrivateDisks{}, lifecycle)
	t.Setenv("BARN_HOME", controller.Deployment.Root)
	created, err := controller.CreateAndStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Root: controller.Deployment.Root}
	for _, node := range created.Commit.Nodes {
		node.Phase = state.Prepared
		node.Process = state.ProcessIdentity{}
		if err := store.WriteNode(node); err != nil {
			t.Fatal(err)
		}
	}
	writeDestroyKeyFixtures(t, controller.Deployment.Root)
	destroyer := Manager{BarnVersion: "test", Nodes: []string{"node-1"}, allowPartialDestroy: true}
	if _, err := destroyer.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	controller.CreateNodes = []string{"node-1"}
	controller.StartNodes = []string{"node-1"}
	recreated, err := controller.CreateAndStart(context.Background())
	if err != nil || len(recreated.Prepare) != 1 || recreated.Prepare[0].Node != "node-1" || len(recreated.Start) != 1 || !recreated.Start[0].Ready {
		t.Fatalf("partial recreate=%#v err=%v", recreated, err)
	}
	meta, metaErr := store.ReadNode("meta")
	peer, peerErr := store.ReadNode("node-1")
	if metaErr != nil || peerErr != nil || meta.Phase != state.Prepared || peer.Phase != state.Running {
		t.Fatalf("partial recreate states meta=%#v/%v peer=%#v/%v", meta, metaErr, peer, peerErr)
	}
}

func markFixtureDiskPersistent(t *testing.T, deploymentValue state.Store) (state.DeploymentState, state.NodeState) {
	t.Helper()
	deploymentState, err := deploymentValue.ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	deploymentState.Resolved.Nodes[0].Disks[0].Persistent = true
	hash, err := spec.Hash(deploymentState.Resolved)
	if err != nil {
		t.Fatal(err)
	}
	deploymentState.SpecHash = hash
	if err := deploymentValue.WriteDeployment(deploymentState); err != nil {
		t.Fatal(err)
	}
	node, err := deploymentValue.ReadNode(deploymentState.Resolved.Nodes[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	node.SpecHash = hash
	node.DataDisks[0].Persistent = true
	if err := deploymentValue.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	return deploymentState, node
}

func TestPrivateDestroyPreservesAndPrepareReattachesPersistentDisk(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	deploymentValue := startConfig.Deployment
	t.Setenv("BARN_HOME", deploymentValue.Root)
	store := state.Store{Root: deploymentValue.Root}
	deploymentState, originalNode := markFixtureDiskPersistent(t, store)
	originalData, err := os.ReadFile(originalNode.DataDisks[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	writeDestroyKeyFixtures(t, deploymentValue.Root)
	manager := Manager{BarnVersion: "test"}
	if _, err := manager.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, err := persistent.Inventory(deploymentValue.Root)
	if err != nil || len(records) != 1 {
		t.Fatalf("retained records = %#v, %v", records, err)
	}
	if data, err := os.ReadFile(records[0].Path); err != nil || string(data) != string(originalData) {
		t.Fatalf("retained data changed: %q, %v", data, err)
	}

	prepare := privatePrepareConfig(t, deploymentValue.Root, &fakePrivateDisks{})
	prepare.Resolved = deploymentState.Resolved
	prepare.SpecHash = deploymentState.SpecHash
	prepare.Plan, err = Build(prepare.Resolved, os.Getuid(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey := testSSHKeyPair(t)
	prepare.Seeds, err = RenderSeeds(prepare.Resolved, prepare.Plan, SeedInput{PublicKey: publicKey, PrivateKey: privateKey, SpecHashes: prepare.NodeHashes, Generation: map[string]uint64{"meta": 1, "node-1": 1}})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := PrepareNode(context.Background(), prepare, originalNode.Node)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts.Data) != 1 || artifacts.Data[0].Path != records[0].Path {
		t.Fatalf("persistent disk was not reattached: %#v", artifacts.Data)
	}
}

func TestPrivatePersistentValidationAllowsUncreatedDesiredDisk(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	store := state.Store{Root: startConfig.Deployment.Root}
	deploymentState, err := store.ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	deploymentState.Resolved.Nodes[1].Disks = []spec.Disk{{Name: "data", Size: 4 * spec.GiB, Mount: "/data", Filesystem: "ext4", Persistent: true}}
	if err := os.Remove(filepath.Join(startConfig.Deployment.Root, "nodes", deploymentState.Resolved.Nodes[1].Name, "state.json")); err != nil {
		t.Fatal(err)
	}
	meta, err := store.ReadNode(deploymentState.Resolved.Nodes[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := validatePrivatePersistentState(startConfig.Deployment, deploymentState, []state.NodeState{meta})
	if err != nil || len(identities) != 1 || identities[0].Node != deploymentState.Resolved.Nodes[1].Name {
		t.Fatalf("uncreated desired persistent disk = %#v, %v", identities, err)
	}
}

func TestPrivatePersistentValidationAllowsRetainedRemovedNode(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	deploymentValue := startConfig.Deployment
	t.Setenv("BARN_HOME", deploymentValue.Root)
	store := state.Store{Root: deploymentValue.Root}
	markFixtureDiskPersistent(t, store)
	writeDestroyKeyFixtures(t, deploymentValue.Root)
	if _, err := (Manager{BarnVersion: "test", Nodes: []string{"meta"}}).DestroyNodes(context.Background()); err != nil {
		t.Fatal(err)
	}
	stateValue, err := store.ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePrivatePersistentDesired(deploymentValue, stateValue.Resolved); err != nil {
		t.Fatalf("retained disk on removed node rejected: %v", err)
	}
}

func TestPrivateRecreateAllowsAddingUncreatedPersistentDisk(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	store := state.Store{Root: startConfig.Deployment.Root}
	stateValue, err := store.ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	requested := cloneResolved(stateValue.Resolved)
	requested.Nodes[0].Disks = append(requested.Nodes[0].Disks, spec.Disk{Name: "extra", Size: 4 * spec.GiB, Mount: "/extra", Filesystem: "ext4", Persistent: true})
	if err := validatePrivateRecreatePersistent(startConfig.Deployment, stateValue.Resolved, requested); err != nil {
		t.Fatalf("new uncreated persistent disk rejected: %v", err)
	}
}

func TestPrivateRecreateRejectsRemovingActivePersistentDisk(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	deploymentValue := startConfig.Deployment
	store := state.Store{Root: deploymentValue.Root}
	stateValue, _ := markFixtureDiskPersistent(t, store)
	requested := cloneResolved(stateValue.Resolved)
	requested.Nodes[0].Disks[0].Persistent = false
	if err := validatePrivateRecreatePersistent(deploymentValue, stateValue.Resolved, requested); err == nil || !strings.Contains(err.Error(), "active persistent disk") {
		t.Fatalf("active persistent disk removal accepted: %v", err)
	}
}

func TestPrivatePersistentDeleteRequiresDestroyedNodes(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	deploymentValue := startConfig.Deployment
	t.Setenv("BARN_HOME", deploymentValue.Root)
	_, _ = markFixtureDiskPersistent(t, state.Store{Root: deploymentValue.Root})
	manager := Manager{BarnVersion: "test"}
	if _, err := manager.DeletePersistent(context.Background()); err == nil {
		t.Fatal("persistent disks were deleted while nodes existed")
	}
	writeDestroyKeyFixtures(t, deploymentValue.Root)
	if _, err := manager.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	deleted, err := manager.DeletePersistent(context.Background())
	if err != nil || len(deleted) != 1 {
		t.Fatalf("explicit persistent deletion = %#v, %v", deleted, err)
	}
}

func TestPrivatePersistentDeleteIgnoresFinderMetadataInNodesRoot(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	deploymentValue := startConfig.Deployment
	t.Setenv("BARN_HOME", deploymentValue.Root)
	_, _ = markFixtureDiskPersistent(t, state.Store{Root: deploymentValue.Root})
	writeDestroyKeyFixtures(t, deploymentValue.Root)
	if _, err := (Manager{BarnVersion: "test"}).Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploymentValue.Root, "nodes", ".DS_Store"), []byte("finder"), 0o600); err != nil {
		t.Fatal(err)
	}
	deleted, err := (Manager{BarnVersion: "test"}).DeletePersistent(context.Background())
	if err != nil || len(deleted) != 1 {
		t.Fatalf("Finder metadata blocked persistent deletion: %#v, %v", deleted, err)
	}
}

// Destroying the control node elects a peer whose definition gains the control
// role. Its state must follow the new definition hash while keeping the hash
// its guest was seeded with, so integrations keep working and readiness still
// matches the guest marker.
func TestDestroyControlNodeRehashesElectedPeer(t *testing.T) {
	startConfig, _ := preparedStartFixture(t)
	root := startConfig.Deployment.Root
	t.Setenv("BARN_HOME", root)
	writeDestroyKeyFixtures(t, root)
	store := state.Store{Root: root}
	before, err := store.ReadNode("node-1")
	if err != nil {
		t.Fatal(err)
	}
	status, err := (Manager{BarnVersion: "test", Nodes: []string{"meta"}}).DestroyNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.Message, "node-1 is now the control node") {
		t.Fatalf("destroy message does not name the elected control node: %q", status.Message)
	}
	after, err := store.ReadDeployment()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Resolved.Nodes) != 1 || after.Resolved.Nodes[0].Name != "node-1" || !after.Resolved.Nodes[0].Control {
		t.Fatalf("remaining deployment = %#v", after.Resolved.Nodes)
	}
	node, err := store.ReadNode("node-1")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := spec.NodeHash(after.Resolved, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if node.SpecHash != expected || node.GuestSpecHash != before.SpecHash || node.VMUUID != before.VMUUID {
		t.Fatalf("elected node state: spec=%s want %s guest=%s want %s", node.SpecHash, expected, node.GuestSpecHash, before.SpecHash)
	}
	if _, _, _, err := (Manager{BarnVersion: "test"}).integrationSnapshot(context.Background()); err != nil {
		t.Fatalf("integrations rejected the elected control node: %v", err)
	}
	// An inventory that now names node-1 as the control node matches exactly.
	desired := cloneResolved(after.Resolved)
	if diff := diffResolved(after.Resolved, desired, func(string) bool { return true }); len(diff.Changed) != 0 || len(diff.Removed) != 0 {
		t.Fatalf("diff after control election = %#v", diff)
	}
}
