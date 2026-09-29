package private

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pgsty/barn/internal/state"
)

func commitFixture(t *testing.T, disks DiskOps) (Deployment, PrepareConfig, []PrepareOutcome) {
	t.Helper()
	root := t.TempDir()
	deploymentValue := Deployment{Root: root}
	config := privatePrepareConfig(t, deploymentValue.Root, disks)
	// privatePrepareConfig creates a fresh intent; rebuild it for the current
	// owner before preparing or committing state.
	var err error
	config.Plan, err = Build(config.Resolved, os.Getuid(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey := testSSHKeyPair(t)
	config.Seeds, err = RenderSeeds(config.Resolved, config.Plan, SeedInput{
		PublicKey:  publicKey,
		PrivateKey: privateKey,
		SpecHashes: config.NodeHashes, Generation: map[string]uint64{"meta": 1, "node-1": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcomes := prepareAll(context.Background(), config, 2)
	return deploymentValue, config, outcomes
}

func TestCommitPreparedStateAndFinalization(t *testing.T) {
	deploymentValue, config, outcomes := commitFixture(t, &fakePrivateDisks{})
	result, err := CommitPrepared(deploymentValue, config, outcomes, "test-version")
	if err != nil || len(result.Nodes) != 2 || len(result.Failed) != 0 || result.Deployment.Resolved.Network != "private" {
		t.Fatalf("commit result = %#v, %v", result, err)
	}
	store := state.Store{Root: deploymentValue.Root}
	for _, node := range result.Nodes {
		persisted, err := store.ReadNode(node.Node)
		if err != nil || persisted.Phase != state.Prepared || persisted.VMUUID != node.VMUUID {
			t.Fatalf("persisted node %s = %#v, %v", node.Node, persisted, err)
		}
		journalPath := filepath.Join(filepath.Dir(node.RootDisk), "private-prepare.json")
		journal, err := ReadPrepareJournal(journalPath)
		if err != nil || !journal.StateCommitted {
			t.Fatalf("committed journal %s = %#v, %v", node.Node, journal, err)
		}
	}
	// State commit is idempotent before the finalizer removes the journal.
	second, err := CommitPrepared(deploymentValue, config, outcomes, "test-version")
	if err != nil || len(second.Nodes) != 2 {
		t.Fatalf("idempotent commit = %#v, %v", second, err)
	}
	for _, node := range result.Nodes {
		if err := FinalizePrepared(deploymentValue, node.Node); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(node.RootDisk), "private-prepare.json")); !os.IsNotExist(err) {
			t.Fatalf("finalized journal remains for %s: %v", node.Node, err)
		}
	}
}

func TestCommitPreparedPreservesSuccessfulNodeOnPeerFailure(t *testing.T) {
	deploymentValue, config, outcomes := commitFixture(t, &fakePrivateDisks{failSubstring: "node-1/root.qcow2"})
	result, err := CommitPrepared(deploymentValue, config, outcomes, "test-version")
	if err != nil || len(result.Nodes) != 1 || len(result.Failed) != 1 || result.Nodes[0].Node != "meta" || result.Failed[0] != "node-1" {
		t.Fatalf("partial commit = %#v, %v", result, err)
	}
	store := state.Store{Root: deploymentValue.Root}
	if _, err := store.ReadNode("meta"); err != nil {
		t.Fatalf("successful peer state missing: %v", err)
	}
	if _, err := store.ReadNode("node-1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed peer gained state: %v", err)
	}
	partial, err := ReadPrepareJournal(filepath.Join(deploymentValue.Root, "nodes", "node-1", "private-prepare.json"))
	if err != nil || partial.Prepared || partial.StateCommitted {
		t.Fatalf("failed peer journal = %#v, %v", partial, err)
	}
}
