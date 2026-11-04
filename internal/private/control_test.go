package private

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/state"
)

func TestControlElectionRecoversInterruptedHashWrite(t *testing.T) {
	for _, direction := range []string{"deployment-first", "node-first"} {
		t.Run(direction, func(t *testing.T) {
			config, _ := preparedStartFixture(t)
			store := state.Store{Root: config.Deployment.Root}
			original, err := store.ReadDeployment()
			if err != nil {
				t.Fatal(err)
			}
			node, err := store.ReadNode("node-1")
			if err != nil {
				t.Fatal(err)
			}
			oldNode := node
			next := original
			next.Resolved = cloneResolved(original.Resolved)
			next.Resolved.Nodes[0].Control = false
			next.Resolved.Nodes[1].Control = true
			next.SpecHash, err = spec.Hash(next.Resolved)
			if err != nil {
				t.Fatal(err)
			}
			if direction == "deployment-first" {
				if err := store.WriteDeployment(next); err != nil {
					t.Fatal(err)
				}
			} else {
				node.GuestSpecHash = node.SpecHash
				node.SpecHash, err = spec.NodeHash(next.Resolved, node.Node)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.WriteNode(node); err != nil {
					t.Fatal(err)
				}
			}
			held, err := acquireDeploymentLock(context.Background(), config.Deployment.Root, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := held.Release(); err != nil {
				t.Fatal(err)
			}
			deployment, err := store.ReadDeployment()
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := store.ReadNode(node.Node)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := spec.NodeHash(deployment.Resolved, node.Node)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.SpecHash != expected || recovered.GuestSpecHash != oldNode.SpecHash || recovered.VMUUID != oldNode.VMUUID || recovered.Phase != oldNode.Phase {
				t.Fatalf("wrong recovered state: %+v", recovered)
			}
			if err := reconcileControlHashes(config.Deployment.Root); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestControlRecoveryDoesNotAcceptUnrelatedDrift(t *testing.T) {
	config, _ := preparedStartFixture(t)
	store := state.Store{Root: config.Deployment.Root}
	node, err := store.ReadNode("node-1")
	if err != nil {
		t.Fatal(err)
	}
	node.SpecHash = strings.Repeat("e", 64)
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	if err := reconcileControlHashes(config.Deployment.Root); err != nil {
		t.Fatal(err)
	}
	after, err := store.ReadNode(node.Node)
	if err != nil {
		t.Fatal(err)
	}
	if after.SpecHash != node.SpecHash {
		t.Fatal("recovery accepted unrelated hash drift")
	}
}

func TestDestroyFinishesMixedPreparedAndDestroyingNodes(t *testing.T) {
	config, nodes := preparedStartFixture(t)
	t.Setenv("BARN_HOME", config.Deployment.Root)
	writeDestroyKeyFixtures(t, config.Deployment.Root)
	store := state.Store{Root: config.Deployment.Root}
	nodes[1].Phase = state.Destroying
	if err := store.WriteNode(nodes[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := (Manager{BarnVersion: "test"}).Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestControlElectionFinalizesCommittedPeerJournal(t *testing.T) {
	deployment, prepare, outcomes := commitFixture(t, &fakePrivateDisks{})
	if _, err := CommitPrepared(deployment, prepare, outcomes, "test"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BARN_HOME", deployment.Root)
	writeDestroyKeyFixtures(t, deployment.Root)
	if _, err := (Manager{BarnVersion: "test", Nodes: []string{"meta"}}).DestroyNodes(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A later lifecycle must not find a prepare journal tied to the old hash.
	if err := FinalizePrepared(deployment, "node-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := (Manager{}).integrationSnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDestroyingNodeWithLivePIDIsNotStoppable(t *testing.T) {
	config, nodes := preparedStartFixture(t)
	store := state.Store{Root: config.Deployment.Root}
	nodes[0].Phase = state.Destroying
	nodes[0].Process.PID = os.Getpid()
	if err := store.WriteNode(nodes[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := loadStoppableNodes(store, []string{nodes[0].Node}); err == nil {
		t.Fatal("accepted destroying node with a live process")
	}
	if _, err := os.Stat(nodes[0].RootDisk); err != nil {
		t.Fatal(err)
	}
}
