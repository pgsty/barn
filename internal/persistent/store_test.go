package persistent

import (
	"os"
	"path/filepath"
	"testing"
)

func persistentFixture(t *testing.T) (string, Identity, string) {
	t.Helper()
	root := t.TempDir()
	nodeDir := filepath.Join(root, "nodes", "meta")
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(nodeDir, "data.qcow2")
	if err := os.WriteFile(source, []byte("retained-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := Identity{Node: "meta", Name: "data", Serial: "disk-serial", Size: 64 << 30, Mount: "/data", Filesystem: "xfs"}
	return root, identity, source
}

func TestPreserveValidateAndExplicitDelete(t *testing.T) {
	t.Parallel()
	root, identity, source := persistentFixture(t)
	record, err := Preserve(root, identity, source)
	if err != nil {
		t.Fatal(err)
	}
	if record.Path == source || record.OwnerUID != os.Geteuid() {
		t.Fatalf("record = %#v", record)
	}
	if _, err := os.Lstat(source); !os.IsNotExist(err) {
		t.Fatalf("node-local source remains: %v", err)
	}
	for _, directory := range []string{root, filepath.Join(root, "disks", "meta"), filepath.Dir(record.Path)} {
		if err := os.WriteFile(filepath.Join(directory, ".DS_Store"), []byte("finder"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found, err := ValidateDesired(root, []Identity{identity})
	if err != nil || found[key(identity.Node, identity.Name)].Path != record.Path {
		t.Fatalf("validated = %#v, %v", found, err)
	}
	deleted, err := DeleteAll(root)
	if err != nil || len(deleted) != 1 {
		t.Fatalf("deleted = %#v, %v", deleted, err)
	}
	if records, err := Inventory(root); err != nil || len(records) != 0 {
		t.Fatalf("inventory after delete = %#v, %v", records, err)
	}
}

func TestValidateDesiredRejectsIncompatibleSemantics(t *testing.T) {
	t.Parallel()
	root, identity, source := persistentFixture(t)
	if _, err := Preserve(root, identity, source); err != nil {
		t.Fatal(err)
	}
	checks := []func(*Identity){
		func(value *Identity) { value.Size++ },
		func(value *Identity) { value.Mount = "/different" },
		func(value *Identity) { value.Serial = "different" },
	}
	for _, mutate := range checks {
		candidate := identity
		mutate(&candidate)
		if _, err := ValidateDesired(root, []Identity{candidate}); err == nil {
			t.Fatalf("incompatible identity was accepted: %#v", candidate)
		}
	}
}

func TestInventoryFailsClosedOnSymlinkAndUnexpectedArtifact(t *testing.T) {
	t.Parallel()
	t.Run("symlink disk", func(t *testing.T) {
		root, identity, source := persistentFixture(t)
		record, err := Preserve(root, identity, source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(record.Path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "state.json"), record.Path); err != nil {
			t.Fatal(err)
		}
		if _, err := Inventory(root); err == nil {
			t.Fatal("symlink retained disk was accepted")
		}
		if _, err := DeleteAll(root); err == nil {
			t.Fatal("unsafe retained store was deleted")
		}
	})
	t.Run("unexpected file", func(t *testing.T) {
		root, identity, source := persistentFixture(t)
		record, err := Preserve(root, identity, source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(record.Path), "manual.txt"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Inventory(root); err == nil {
			t.Fatal("unexpected retained artifact was accepted")
		}
	})
}

func TestPreserveRejectsForeignHardLinkedSource(t *testing.T) {
	t.Parallel()
	root, identity, source := persistentFixture(t)
	other := filepath.Join(filepath.Dir(source), "other.qcow2")
	if err := os.Link(source, other); err != nil {
		t.Fatal(err)
	}
	if _, err := Preserve(root, identity, source); err == nil {
		t.Fatal("hard-linked source was accepted")
	}
}

func TestPreserveNormalizesOlderQEMUImgModesWithoutChangingData(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o640, 0o644} {
		root, identity, source := persistentFixture(t)
		if err := os.Chmod(source, mode); err != nil {
			t.Fatal(err)
		}
		if err := ValidateSource(root, source); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(source)
		if err != nil || before.Mode().Perm() != mode {
			t.Fatal("read-only source validation changed mode")
		}
		record, err := Preserve(root, identity, source)
		if err != nil {
			t.Fatalf("mode %o cannot be retained: %v", mode, err)
		}
		after, err := os.Stat(record.Path)
		if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o lost identity/permissions: %v %v", mode, after, err)
		}
		data, err := os.ReadFile(record.Path)
		if err != nil || string(data) != "retained-data" {
			t.Fatalf("disk data changed: %q %v", data, err)
		}
	}
}

func TestSourcePermissionCompatibilityRejectsWritableOrLinkedDisks(t *testing.T) {
	root, _, source := persistentFixture(t)
	if err := os.Chmod(source, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSource(root, source); err == nil {
		t.Fatal("world-writable disk accepted")
	}
	if err := os.Chmod(source, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, source+".link"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSource(root, source); err == nil {
		t.Fatal("hardlinked disk accepted")
	}
}
