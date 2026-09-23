package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/farrow/internal/config"
)

func TestFreshUntouchedInitCanUseAnAvailableNetwork(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("FARROW_HOME", t.TempDir())
	path := filepath.Join(cwd, "farrow.yml")
	original, err := config.Template("dual", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	selection, err := resolveSetupSelection("", "", "", cwd)
	if err != nil || !selection.Generated || selection.Publish {
		t.Fatalf("selection=%+v err=%v", selection, err)
	}
	if err := selection.rebaseGenerated("172.31.251.0/24"); err != nil {
		t.Fatal(err)
	}
	if err := publishSetupConfig(selection); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "172.31.251.10") {
		t.Fatalf("rebased inventory=%s %v", data, err)
	}
	backup, err := os.ReadFile(path + ".before-network-change")
	if err != nil || !bytes.Equal(backup, original) {
		t.Fatalf("original not preserved: %s %v", backup, err)
	}
	// Restoring the untouched default and rebasing again reuses the identical
	// backup instead of failing; a different backup is still never replaced.
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishSetupConfig(selection); err != nil {
		t.Fatalf("repeated rebase: %v", err)
	}
	if err := os.WriteFile(path+".before-network-change", []byte("user notes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishSetupConfig(selection); err == nil {
		t.Fatal("a different existing backup was overwritten")
	}
}

func TestSetupPreservesExplicitCustomAndConcurrentlyEditedInventory(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("FARROW_HOME", t.TempDir())
	path := filepath.Join(cwd, "farrow.yml")
	original, err := config.Template("meta", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	explicit, err := resolveSetupSelection("", path, "", cwd)
	if err != nil || explicit.Generated {
		t.Fatalf("explicit file changed: %+v %v", explicit, err)
	}
	selection, err := resolveSetupSelection("", "", "", cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := selection.rebaseGenerated("172.31.251.0/24"); err != nil {
		t.Fatal(err)
	}
	edited := append(original, []byte("# user edits\n")...)
	if err := os.WriteFile(path, edited, 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishSetupConfig(selection); err == nil {
		t.Fatal("concurrent edit overwritten")
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, edited) {
		t.Fatalf("concurrent data lost: %s %v", data, err)
	}
	custom, err := resolveSetupSelection("", "", "", cwd)
	if err != nil || custom.Generated {
		t.Fatalf("custom file eligible for rebase: %+v %v", custom, err)
	}
}

func TestConfigSizeErrorShowsFileLineValueAndExample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yml")
	data := []byte("all:\n  hosts:\n    10.10.10.10: { nodename: meta, vm_mem: 4GBB }\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := config.LoadPath(path)
	if err == nil {
		t.Fatal("invalid memory accepted")
	}
	for _, want := range []string{path, "line 3", "vm_mem", "4GBB", "4GiB"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q: %v", want, err)
		}
	}
}
