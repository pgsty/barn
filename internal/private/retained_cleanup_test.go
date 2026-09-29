package private

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pgsty/barn/internal/persistent"
	"github.com/pgsty/barn/internal/state"
)

func TestDestroyAfterRemovingPersistentNodePreservesRetainedDisk(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "unexpected artifact"}[unsafe], func(t *testing.T) {
			fixture, _ := preparedStartFixture(t)
			t.Setenv("BARN_HOME", fixture.Deployment.Root)
			store := state.Store{Root: fixture.Deployment.Root}
			_, meta := markFixtureDiskPersistent(t, store)
			writeDestroyKeyFixtures(t, fixture.Deployment.Root)
			if _, err := (Manager{BarnVersion: "test", Nodes: []string{meta.Node}}).DestroyNodes(context.Background()); err != nil {
				t.Fatal(err)
			}
			records, err := persistent.Inventory(fixture.Deployment.Root)
			if err != nil || len(records) != 1 {
				t.Fatalf("retained inventory: %+v %v", records, err)
			}
			before, err := os.Stat(records[0].Path)
			if err != nil {
				t.Fatal(err)
			}
			if unsafe {
				if err := os.WriteFile(filepath.Join(filepath.Dir(records[0].Path), "foreign-file"), []byte("unowned"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			manager := Manager{BarnVersion: "test"}
			_, err = manager.Destroy(context.Background())
			if unsafe && err == nil || !unsafe && err != nil {
				t.Fatalf("destroy with retained disk: %v", err)
			}
			after, err := os.Stat(records[0].Path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("destroy changed a retained disk: %v", err)
			}
			if unsafe {
				if _, err := store.ReadNode("node-1"); err != nil {
					t.Fatalf("unsafe inventory allowed peer deletion: %v", err)
				}
				return
			}
			if deleted, err := manager.DeletePersistent(context.Background()); err != nil || len(deleted) != 1 {
				t.Fatalf("explicit persistent deletion: %+v %v", deleted, err)
			}
		})
	}
}
