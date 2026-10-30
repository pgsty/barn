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
	if filepath.Dir(filepath.Dir(filepath.Dir(directory))) != root {
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

func TestFallbackSeparatesBarnHomes(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	firstRoot := filepath.Join(t.TempDir(), "barn-one")
	secondRoot := filepath.Join(t.TempDir(), "barn-two")
	t.Setenv("BARN_HOME", firstRoot)
	first, err := Directory("meta", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if err := Ensure(first, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	if err := PruneEmptyParents(first, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BARN_HOME", secondRoot)
	second, err := Directory("meta", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("different BARN_HOME values share runtime path %q", first)
	}
	if err := Ensure(second, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	if err := PruneEmptyParents(second, os.Getuid()); err != nil {
		t.Fatal(err)
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

func TestXDGDirectoryFollowsSafeAliasAndNormalizesPath(t *testing.T) {
	realRoot := shortRuntimeRoot(t)
	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "runtime-alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", aliasParent+"/./runtime-alias/../runtime-alias")
	directory, err := Directory("meta", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(scopedBase(canonical, os.Getuid()), "barn", "meta")
	if directory != want {
		t.Fatalf("runtime directory = %q, want %q", directory, want)
	}
	if err := Ensure(directory, os.Getuid()); err != nil {
		t.Fatal(err)
	}
}

func TestXDGSeparatesBarnHomes(t *testing.T) {
	root := shortRuntimeRoot(t)
	t.Setenv("XDG_RUNTIME_DIR", root)
	firstRoot := filepath.Join(t.TempDir(), "barn-one")
	secondRoot := filepath.Join(t.TempDir(), "barn-two")
	t.Setenv("BARN_HOME", firstRoot)
	first, err := Directory("meta", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BARN_HOME", secondRoot)
	second, err := Directory("meta", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("different BARN_HOME values share XDG runtime path %q", first)
	}
}

func TestEnsureAcceptsPreviousXDGNamespaceAfterBarnHomeChanges(t *testing.T) {
	root := shortRuntimeRoot(t)
	t.Setenv("XDG_RUNTIME_DIR", root)
	t.Setenv("BARN_HOME", filepath.Join(t.TempDir(), "barn-one"))
	previous, err := Directory("meta", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BARN_HOME", filepath.Join(t.TempDir(), "barn-two"))
	if err := Ensure(previous, os.Getuid()); err != nil {
		t.Fatalf("previous persisted runtime namespace was rejected: %v", err)
	}
}

func TestFallbackExpandsBarnHomeBeforeHashing(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BARN_HOME", "~/barn-test")
	first, err := Directory("meta", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	second, err := Directory("meta", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("BARN_HOME=~/... changed with working directory: %q -> %q", first, second)
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
