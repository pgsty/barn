package macvm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSetupDiskRequirementTracksPartialInstaller(t *testing.T) {
	store := testStore(t)
	m := &Manager{Store: store}
	spec := DefaultIPSW()
	needed, _, err := m.setupDiskRequirement(nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := spec.SizeBytes + macRestoreBytes; needed != want {
		t.Fatalf("empty cache needs %d bytes, want %d", needed, want)
	}

	if _, err := store.mkdir("images", "ipsw"); err != nil {
		t.Fatal(err)
	}
	partial, err := store.Path("images", "ipsw", spec.Build+".ipsw.partial")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := partialDownload{SchemaVersion: imageSchemaVersion, URL: spec.URL, SizeBytes: spec.SizeBytes}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial+".json", data, 0o600); err != nil {
		t.Fatal(err)
	}
	needed, _, err = m.setupDiskRequirement(nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := spec.SizeBytes - 1 + macRestoreBytes; needed != want {
		t.Fatalf("partial cache needs %d bytes, want %d", needed, want)
	}
}

func TestSetupDiskRequirementSkipsVerifiedBase(t *testing.T) {
	store := testStore(t)
	m := &Manager{Store: store}
	held := holdStore(t, store)
	base := testBase(t, store, held, "26A428-doctor")
	config, err := NewConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.DefaultBaseID = base.ID
	if err := store.SaveConfig(held, config); err != nil {
		t.Fatal(err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}

	needed, detail, err := m.setupDiskRequirement(config)
	if err != nil {
		t.Fatal(err)
	}
	if needed != 0 || detail == "" {
		t.Fatalf("verified base requirement = %d, %q", needed, detail)
	}
	if _, err := os.Stat(filepath.Join(store.Root, "images", "base", base.ID)); err != nil {
		t.Fatal(err)
	}
}
