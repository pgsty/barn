package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLinkedInventoryResolvesSharesBesideItsTarget(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	inventory := filepath.Join(project, "lab.yml")
	data := `all: {hosts: {10.10.10.10: {vm_shares: [{host: ./src/../code, guest: /src}, {host: ~/shared, guest: /shared}]}}}`
	if err := os.WriteFile(inventory, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "barn.yml")
	if err := os.Symlink(inventory, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	file, _, err := Discover(root, "barn.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := file.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	shares := resolved.Nodes[0].Shares
	if len(shares) != 2 || shares[0].Host != filepath.Join(project, "code") || shares[1].Host != filepath.Join(root, "shared") {
		t.Fatalf("inventory-relative sources = %#v", shares)
	}
}

func TestSharedBuildTreeAllowsOverlappingSources(t *testing.T) {
	for _, readonly := range []bool{true, false} {
		data := fmt.Sprintf(`all: {hosts: {
10.10.10.10: {vm_shares: [{host: ./src, guest: /src, readonly: false}, {host: ./src/build, guest: /build}]},
10.10.10.11: {vm_shares: [{host: ./src, guest: /src, readonly: %t}]}
}}`, readonly)
		if _, err := ParseInventory([]byte(data)); err != nil {
			t.Fatalf("shared build tree readonly=%t: %v", readonly, err)
		}
	}
}
