package hostshare

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pgsty/barn/internal/qemu"
	"github.com/pgsty/barn/internal/spec"
)

func testDirs(t *testing.T) (string, string) {
	t.Helper()
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return work, root
}

func TestOpenRejectsSymlinksAndProtectedPaths(t *testing.T) {
	work, root := testDirs(t)
	source := filepath.Join(work, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Validate(root, []spec.Share{{Host: source, Guest: "/mnt/source", Readonly: true}}); err != nil {
		t.Fatalf("valid source: %v", err)
	}
	link := filepath.Join(work, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if err := Validate(root, []spec.Share{{Host: link, Guest: "/mnt/link", Readonly: true}}); err == nil {
		t.Fatal("symlink source was accepted")
	}
	if err := Validate(root, []spec.Share{{Host: root, Guest: "/mnt/data", Readonly: true}}); err == nil {
		t.Fatal("data root source was accepted")
	}
	if err := Validate(root, []spec.Share{{Host: filepath.Join(root, "nodes"), Guest: "/mnt/nodes", Readonly: true}}); err == nil {
		t.Fatal("data root descendant source was accepted")
	}
}

func TestBundleLayoutValidation(t *testing.T) {
	work, root := testDirs(t)
	source := filepath.Join(work, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	shares := []spec.Share{{Host: source, Guest: "/mnt/source", Readonly: true}}
	bundle, err := Open(root, shares)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := bundle.Close(); err != nil {
			t.Errorf("close share bundle: %v", err)
		}
	})
	tag := spec.ShareTag(shares[0])
	invocation := qemu.Invocation{InheritedFiles: []qemu.InheritedFile{{FD: 3, Kind: "share", ID: tag}}}
	if err := bundle.ValidateInvocation(invocation, 0); err != nil {
		t.Fatal(err)
	}
	wrong := qemu.Invocation{InheritedFiles: []qemu.InheritedFile{{FD: 4, Kind: "share", ID: tag}}}
	if err := bundle.ValidateInvocation(wrong, 0); err == nil {
		t.Fatal("wrong descriptor layout was accepted")
	}
}

func TestUserSelectedProjectModesArePreserved(t *testing.T) {
	work, root := testDirs(t)
	for _, mode := range []os.FileMode{0o755, 0o775, 0o777, 0o555} {
		if err := os.Chmod(work, mode); err != nil {
			t.Fatal(err)
		}
		if err := Validate(root, []spec.Share{{Host: work, Guest: "/project"}}); err != nil {
			t.Fatalf("mode %o: %v", mode, err)
		}
		info, err := os.Stat(work)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("changed project mode: %v %v", info, err)
		}
	}
	if err := os.Chmod(work, 0o700); err != nil {
		t.Fatal(err)
	}
}
