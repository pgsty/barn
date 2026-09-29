package macvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageListReferencesAndPruneDryRun(t *testing.T) {
	m, base := testManager(t)
	testMachine(t, m, "dev", "10.10.30.0/24", false)
	held := holdStore(t, m.Store)
	unused := testBase(t, m.Store, held, "26A428-unused")
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.mkdir("images", "ipsw"); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(m.Store.Root, "images", "ipsw", "26A428.ipsw.partial")
	if err := os.WriteFile(partial, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	images, err := m.Store.ImageList()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]ImageEntry{}
	for _, image := range images {
		found[image.ID] = image
	}
	if found[base.ID].References[0] != "dev" || !found[base.ID].Default || len(found[unused.ID].References) != 0 || found["26A428"].State != "downloading" {
		t.Fatalf("images %+v", images)
	}
	report, err := m.PruneImages(context.Background(), false, false)
	if err != nil || len(report.Candidates) != 1 || report.Candidates[0].ID != unused.ID || len(report.Removed) != 0 {
		t.Fatalf("dry run %+v %v", report, err)
	}
	if _, err := m.Store.LoadBase(unused.ID); err != nil {
		t.Fatal("a dry run removed a base")
	}
	report, err = m.PruneImages(context.Background(), true, true)
	if err != nil || len(report.Removed) != 2 {
		t.Fatalf("apply %+v %v", report, err)
	}
	if _, err := m.Store.LoadBase(unused.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unused base remains: %v", err)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial download remains")
	}
	if _, err := m.Store.LoadBase(base.ID); err != nil {
		t.Fatalf("the default, referenced base was removed: %v", err)
	}
}

func TestBaseMetadataKeepsItsFirstSchema(t *testing.T) {
	m, base := testManager(t)
	path := filepath.Join(m.Store.Root, "images", "base", base.ID, "metadata.json")
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"schema_version": 1`) {
		t.Fatalf("base metadata %s %v", data, err)
	}
}

func TestBaseIdentityCoversHardwareCapacityAndRecipe(t *testing.T) {
	hash := strings.Repeat("a", 64)
	first, err := BaseImageID("26A428", 100<<30, hash, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []func() (string, error){
		func() (string, error) { return BaseImageID("26A429", 100<<30, hash, 1) },
		func() (string, error) { return BaseImageID("26A428", 200<<30, hash, 1) },
		func() (string, error) { return BaseImageID("26A428", 100<<30, strings.Repeat("b", 64), 1) },
		func() (string, error) { return BaseImageID("26A428", 100<<30, hash, 2) },
	} {
		if id, err := other(); err != nil || id == first {
			t.Fatalf("identity collision %s %v", id, err)
		}
	}
	if _, err := BaseImageID("26A428", 1<<30, hash, 1); err == nil {
		t.Fatal("a tiny disk was accepted")
	}
}

func TestImageListToleratesFinderFilesAndEmptyBases(t *testing.T) {
	m, base := testManager(t)
	root, err := m.Store.Path("images", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".DS_Store"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "26A428-interrupted"), 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := m.Store.ImageList()
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, entry := range entries {
		states[entry.ID] = entry.State
	}
	if states[base.ID] != "ready" || states["26A428-interrupted"] != "incomplete" || len(states) != 2 {
		t.Fatalf("entries %v", states)
	}
	report, err := m.PruneImages(context.Background(), false, false)
	if err != nil || len(report.Candidates) != 1 || report.Candidates[0].ID != "26A428-interrupted" {
		t.Fatalf("prune %+v %v", report, err)
	}
}
