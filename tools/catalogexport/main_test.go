package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/pgsty/barn/internal/image"
)

func TestExportCatalogWritesExactEmbeddedCatalogOnce(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "output", "catalog.json")
	if err := exportCatalog(target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want, err := image.EmbeddedCatalogBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("exported catalog differs from the embedded catalog")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("exported mode = %o, want 600", info.Mode().Perm())
	}
	if err := exportCatalog(target); err == nil {
		t.Fatal("catalog export overwrote an existing file")
	}
	t.Chdir(root)
	if err := exportCatalog("relative/catalog.json"); err != nil {
		t.Fatalf("catalog export rejected a relative path: %v", err)
	}
}
