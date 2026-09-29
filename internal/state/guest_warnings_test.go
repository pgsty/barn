package state

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func warningFixture(t *testing.T) (Store, NodeState) {
	t.Helper()
	store := testStore(t)
	now := time.Now().UTC()
	node := NodeState{Schema: NodeSchema, BarnVersion: "0.7.0", Node: "meta", VMUUID: "original", Phase: Running, Generation: 1, SpecHash: strings.Repeat("a", 64), CreatedAt: now, UpdatedAt: now}
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	return store, node
}

func TestGuestWarningsPreserveLegacyStateAndClearAfterRecovery(t *testing.T) {
	store, node := warningFixture(t)
	path := filepath.Join(store.Root, "nodes", node.Node, "state.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	node.GuestWarnings = []GuestWarning{{Stage: "shares", Detail: "/src: read-only"}}
	if err := store.WriteGuestWarnings(node, node.GuestWarnings); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, current) {
		t.Fatalf("warnings changed the v0.6-compatible node document: %s %v", current, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("cache would block an older destroy: %v %v", entries, err)
	}
	read, err := store.ReadNode(node.Node)
	if err != nil || len(read.GuestWarnings) != 1 {
		t.Fatalf("lost cached warnings: %+v %v", read, err)
	}
	if err := store.WriteGuestWarnings(node, nil); err != nil {
		t.Fatal(err)
	}
	read, err = store.ReadNode(node.Node)
	if err != nil || len(read.GuestWarnings) != 0 {
		t.Fatalf("warning persisted after recovery: %+v %v", read, err)
	}
}

func TestGuestWarningCacheCannotBlockOrContaminateNodeState(t *testing.T) {
	for _, scenario := range []string{"corrupt", "instance", "generation", "spec"} {
		t.Run(scenario, func(t *testing.T) {
			store, node := warningFixture(t)
			if err := store.WriteGuestWarnings(node, []GuestWarning{{Stage: "shares", Detail: "old"}}); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "corrupt":
				path, _ := store.guestWarningsPath(node.Node)
				if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			case "instance":
				node.VMUUID = "replacement"
			case "generation":
				node.Generation++
			case "spec":
				node.SpecHash = strings.Repeat("b", 64)
			}
			if err := store.WriteNode(node); err != nil {
				t.Fatal(err)
			}
			read, err := store.ReadNode(node.Node)
			if err != nil {
				t.Fatalf("diagnostics blocked node access: %v", err)
			}
			if scenario == "corrupt" {
				if len(read.GuestWarnings) != 1 || read.GuestWarnings[0].Stage != "warning-state" {
					t.Fatalf("lost cache diagnostic: %+v", read.GuestWarnings)
				}
			} else if len(read.GuestWarnings) != 0 {
				t.Fatalf("stale warning applied to a new instance/spec: %+v", read.GuestWarnings)
			}
		})
	}
}
