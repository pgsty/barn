package runtimepath

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func shortRuntimeRoot(t *testing.T) string {
	t.Helper()
	parent := "/tmp"
	if runtime.GOOS == "darwin" {
		parent = "/private/tmp"
	}
	root, err := os.MkdirTemp(parent, "rp.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func TestXDGDirectoryAndEnsure(t *testing.T) {
	root := shortRuntimeRoot(t)
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", root)
	directory, err := Directory("meta", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(filepath.Dir(directory)) != root {
		t.Fatalf("runtime directory = %s", directory)
	}
	if err := Ensure(directory, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(directory); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("runtime directory metadata = %#v, %v", info, err)
	}
	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	if err := PruneEmptyParents(directory, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "barn")); !os.IsNotExist(err) {
		t.Fatalf("empty managed parents remain: %v", err)
	}
	if _, err := os.Lstat(root); err != nil {
		t.Fatalf("XDG runtime root was removed: %v", err)
	}
}

func TestRejectsUnsafeXDGAndWrongIdentity(t *testing.T) {
	root := shortRuntimeRoot(t)
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", root)
	if _, err := Directory("meta", os.Getuid()); err == nil {
		t.Fatal("group-readable XDG runtime root accepted")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := Directory("meta", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(directory, "other", os.Getuid()); err == nil {
		t.Fatal("runtime path accepted for another node")
	}
}

func TestEnsureSharedParentsIsConcurrentAndIdempotent(t *testing.T) {
	root := shortRuntimeRoot(t)
	t.Setenv("XDG_RUNTIME_DIR", root)
	directories := make([]string, 0, 2)
	for _, node := range []string{"meta", "node-1"} {
		directory, err := Directory(node, os.Getuid())
		if err != nil {
			t.Fatal(err)
		}
		directories = append(directories, directory)
	}
	errorsCh := make(chan error, 16)
	var wait sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			errorsCh <- Ensure(directories[index%len(directories)], os.Getuid())
		}(worker)
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}
