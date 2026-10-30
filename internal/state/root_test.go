package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDataRootDoesNotDependOnWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	before, err := ResolveDataRoot()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	after, err := ResolveDataRoot()
	if err != nil || before != after {
		t.Fatalf("data root after cd = %q, %v; before %q", after, err, before)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, broad := range []string{"/", filepath.Clean(home)} {
		t.Setenv("BARN_HOME", broad)
		if _, err := ResolveDataRoot(); err == nil {
			t.Fatalf("broad root %s accepted", broad)
		}
	}
}

func TestDataRootFollowsFinalDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real-state")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "state-alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BARN_HOME", alias)
	resolved, err := ResolveDataRoot()
	if err != nil {
		t.Fatal(err)
	}
	real, err = filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != real {
		t.Fatalf("resolved root = %q, want canonical target %q", resolved, real)
	}
}

func TestDataRootAcceptsUserPathSpellings(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(parent)
	t.Setenv("HOME", parent)
	for _, value := range []string{"./state", "~/state", filepath.Join(parent, "state")} {
		t.Setenv("BARN_HOME", value)
		got, err := ResolveDataRoot()
		if err != nil || got != filepath.Join(parent, "state") {
			t.Fatalf("BARN_HOME=%q: %q, %v", value, got, err)
		}
	}
	for _, value := range []string{".", "~"} {
		t.Setenv("BARN_HOME", value)
		if _, err := ResolveDataRoot(); err == nil {
			t.Fatalf("home directory accepted as data root: %q", value)
		}
	}
}
