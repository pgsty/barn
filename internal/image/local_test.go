package image

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationReimportPreservesLegacyLocalAlias(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	source := filepath.Join(t.TempDir(), "source.qcow2")
	makeQCOW2(t, source, "")
	path, metadata, err := store.Import(context.Background(), source, "")
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(filepath.Dir(path), "source.qcow2")
	if err := os.Rename(path, legacyPath); err != nil {
		t.Fatal(err)
	}
	entry, _, _, err := store.RegisterLocalAlias(context.Background(), "local-legacy", legacyPath, metadata, "arm64", "uefi", "ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.localAliasesPath())
	if err != nil {
		t.Fatal(err)
	}
	path, metadata, err = store.Import(context.Background(), source, "")
	if err != nil {
		t.Fatal(err)
	}
	again, againPath, againMetadata, err := store.RegisterLocalAlias(context.Background(), "local-legacy", path, metadata, "arm64", "uefi", "ubuntu")
	if err != nil || again.File != entry.File || againPath != legacyPath || againMetadata.Digest != metadata.Digest {
		t.Fatalf("legacy reimport = %#v, %s, %#v, %v", again, againPath, againMetadata, err)
	}
	after, err := os.ReadFile(store.localAliasesPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("legacy alias registry changed: %v", err)
	}
}

func TestIntegrationLocalAliasGrowthKeepsRegistryReadable(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	source := filepath.Join(t.TempDir(), "source.qcow2")
	makeQCOW2(t, source, "")
	path, metadata, err := store.Import(context.Background(), source, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.RegisterLocalAlias(context.Background(), "local-0", path, metadata, "arm64", "uefi", "ubuntu"); err != nil {
		t.Fatal(err)
	}
	registry, err := store.readLocalAliases()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 256; i++ {
		alias := registry.Aliases["local-0"]
		alias.Name = fmt.Sprintf("local-%d", i)
		registry.Aliases[alias.Name] = alias
	}
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.localAliasesPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.RegisterLocalAlias(context.Background(), "local-256", path, metadata, "arm64", "uefi", "ubuntu"); err != nil {
		t.Fatal(err)
	}
	names, err := LocalAliasNames(store.DataRoot)
	if err != nil || len(names) != 257 {
		t.Fatalf("registry after 257 aliases = %d names, %v", len(names), err)
	}
	before, err := os.ReadFile(store.localAliasesPath())
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = store.RegisterLocalAlias(context.Background(), "local-too-large", path, metadata, "arm64", "uefi", strings.Repeat("x", maxLocalAliasesBytes))
	if err == nil || !strings.Contains(err.Error(), "existing aliases were preserved") {
		t.Fatalf("oversized registry was published: %v", err)
	}
	after, err := os.ReadFile(store.localAliasesPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed registration modified the existing registry: %v", err)
	}
}

func TestIntegrationNamedLocalImportIsImmutableAndResolvable(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	source := filepath.Join(t.TempDir(), "source.qcow2")
	makeQCOW2(t, source, "")
	path, metadata, err := store.Import(context.Background(), source, "")
	if err != nil {
		t.Fatal(err)
	}
	entry, resolvedPath, resolvedMetadata, err := store.RegisterLocalAlias(context.Background(), "local-u24", path, metadata, "arm64", "uefi", "ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Alias != "local-u24" || entry.SHA256 != metadata.Digest || entry.ArtifactSize != metadata.ArtifactSize || resolvedPath != path || resolvedMetadata.Digest != metadata.Digest {
		t.Fatalf("registered local alias = %#v %s %#v", entry, resolvedPath, resolvedMetadata)
	}
	again, againPath, _, err := store.ResolveLocalAlias(context.Background(), "local-u24", "arm64")
	if err != nil || again.SHA256 != metadata.Digest || againPath != path {
		t.Fatalf("resolved local alias = %#v %s %v", again, againPath, err)
	}
	info, err := os.Lstat(store.localAliasesPath())
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("local alias registry mode = %v, %v", info, err)
	}
	if _, _, _, err := store.RegisterLocalAlias(context.Background(), "local-u24", path, metadata, "arm64", "bios", "ubuntu"); err == nil {
		t.Fatal("local alias metadata was mutated")
	}
	if _, _, _, err := store.RegisterLocalAlias(context.Background(), "u24", path, metadata, "arm64", "uefi", "ubuntu"); err == nil {
		t.Fatal("catalog-shaped local alias was accepted")
	}
	names, err := LocalAliasNames(store.DataRoot)
	if err != nil || len(names) != 1 || names[0] != "local-u24" {
		t.Fatalf("local alias names = %v, %v", names, err)
	}
}

func TestLocalAliasNamesIsSideEffectFreeWhenRegistryIsAbsent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	names, err := LocalAliasNames(root)
	if err != nil || len(names) != 0 {
		t.Fatalf("local alias names = %v, %v", names, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "images")); !os.IsNotExist(err) {
		t.Fatalf("completion lookup created image state: %v", err)
	}
}
