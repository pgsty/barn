package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInitThroughDirectoryAliasPreservesExistingFiles(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "项目 files")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "project")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(link)
	t.Setenv("PWD", link)
	if _, err := runInit(initOptions{}); err != nil {
		t.Fatalf("init in linked cwd: %v", err)
	}
	target := filepath.Join(real, "barn.yml")
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runInit(initOptions{Template: "full", Output: filepath.Join(link, "barn.yml")}); err == nil {
		t.Fatal("init overwrote existing inventory without --force")
	}
	after, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("existing inventory changed: %v", err)
	}
	linkedFile := filepath.Join(real, "linked.yml")
	if err := os.Symlink(target, linkedFile); err != nil {
		t.Fatal(err)
	}
	if _, err := runInit(initOptions{Template: "full", Output: linkedFile, Force: true}); err == nil {
		t.Fatal("--force followed a linked output file")
	}
	after, err = os.ReadFile(target)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("linked target changed: %v", err)
	}
}

func TestInitCreatesMissingOutputParents(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "generated", "lab", "barn.yml")
	if _, err := runInit(initOptions{Template: "meta", Output: output}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Dir(output)); err != nil || !info.IsDir() {
		t.Fatalf("generated parent = %#v, %v", info, err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal(err)
	}
}
