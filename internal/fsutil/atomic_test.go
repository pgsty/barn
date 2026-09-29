package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteAndSymlinkRefusal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "state.json")
	if err := AtomicWrite(target, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(target, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "two\n" {
		t.Fatalf("atomic content = %q, err=%v", data, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("atomic mode = %v", info.Mode())
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(link, []byte("bad"), 0o600); err == nil {
		t.Fatal("symlink target unexpectedly overwritten")
	}
}

func TestAtomicCreateDoesNotReplaceConcurrentTarget(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	target := filepath.Join(directory, "barn.yaml")
	if err := AtomicCreate(target, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicCreate(target, []byte("second\n"), 0o600); err == nil {
		t.Fatal("atomic create replaced an existing target")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "first\n" {
		t.Fatalf("target = %q, err=%v", data, err)
	}
}

func TestIsWithin(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inside := filepath.Join(root, "projects", "id")
	if ok, err := IsWithin(root, inside); err != nil || !ok {
		t.Fatalf("inside = %v, %v", ok, err)
	}
	if ok, err := IsWithin(root, filepath.Join(root, "..", "escape")); err != nil || ok {
		t.Fatalf("escape = %v, %v", ok, err)
	}
	if ok, err := IsWithin(root, root); err != nil || ok {
		t.Fatalf("root itself = %v, %v", ok, err)
	}
}
