package private

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
	manager := Manager{BarnVersion: "test"}
	result, err := manager.Destroy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != len(nodes) {
		t.Fatalf("destroy result = %#v", result)
	}
	for _, node := range nodes {
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
