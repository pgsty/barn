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

func TestPrunePreservesInterruptedRecreateBase(t *testing.T) {
	m, oldBase := testManager(t)
	old := testMachine(t, m, "dev", "10.10.30.0/24", false)
	held := holdStore(t, m.Store)
	newBase := testBase(t, m.Store, held, "new-default")
	cfg, err := m.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultBaseID = newBase.ID
	if err := m.Store.SaveConfig(held, cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.BeginRecreate(held, old); err != nil {
		t.Fatal(err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	_, err = m.PruneImages(context.Background(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.LoadBase(oldBase.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := m.Store.LoadMachine("dev")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.BaseID != oldBase.ID {
		t.Fatalf("restored unexpected base %s", recovered.BaseID)
	}
	if _, err := os.Stat(filepath.Join(m.Store.Root, "slots", "dev", "disk.asif")); err != nil {
		t.Fatal(err)
	}
}

func TestPruneRefusesUnreadableRecreateJournal(t *testing.T) {
	m, _ := testManager(t)
	held := holdStore(t, m.Store)
	unused := testBase(t, m.Store, held, "unused")
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	path, err := m.Store.recreateJournalPath("dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PruneImages(context.Background(), false, true); err == nil {
		t.Fatal("prune ignored malformed recovery record")
	}
	if _, err := m.Store.LoadBase(unused.ID); err != nil {
		t.Fatal("prune removed base before validating recovery record")
	}
}
