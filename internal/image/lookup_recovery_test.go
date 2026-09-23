package image

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUncachedImageInfoWithoutQEMU(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	info, err := (Service{DataRoot: root}).InfoArch(context.Background(), "u24", "arm64")
	if err != nil || info.Entry.Alias != "u24" || info.Cached || info.Path != "" {
		t.Fatalf("metadata lookup required QEMU or reported a cached image: %#v, %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "images")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata lookup created a cache: %v", err)
	}
}

func TestImageLookupErrorsWithoutQEMU(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	service := Service{DataRoot: t.TempDir()}
	for _, name := range []string{"unknown-fixture", "local-missing"} {
		_, err := service.InfoArch(context.Background(), name, "arm64")
		if err == nil || !strings.Contains(err.Error(), "unknown image") || !strings.Contains(err.Error(), name) {
			t.Errorf("lookup %s lost the alias error: %v", name, err)
		}
	}
	_, err := service.InfoArch(context.Background(), "el7", "arm64")
	if err == nil || strings.Contains(err.Error(), "qemu-img") || !strings.Contains(err.Error(), "arm64") {
		t.Fatalf("unsupported architecture lost its catalog error: %v", err)
	}
}

func TestRegisteredLocalImagePreservesFailureCause(t *testing.T) {
	t.Parallel()
	for _, problem := range []string{"missing", "digest", "architecture", "registry"} {
		t.Run(problem, func(t *testing.T) {
			root := t.TempDir()
			imageDir := filepath.Join(root, "images", "local")
			if err := os.MkdirAll(imageDir, 0o700); err != nil {
				t.Fatal(err)
			}
			alias := LocalAlias{
				Name: "local-fixture", File: "local/fixture.qcow2", Digest: strings.Repeat("a", 64),
				ArtifactSize: 1, VirtualSize: 1, Arch: "arm64", Boot: "uefi", SourceUser: "dba", CreatedAt: time.Now().UTC(),
			}
			data, err := json.Marshal(LocalAliases{Schema: LocalAliasesSchema, Aliases: map[string]LocalAlias{alias.Name: alias}})
			if err != nil {
				t.Fatal(err)
			}
			if problem == "registry" {
				data = []byte("invalid registry")
			}
			if err := os.WriteFile(filepath.Join(root, "images", "local-images.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if problem == "digest" {
				if err := os.WriteFile(filepath.Join(imageDir, "fixture.qcow2"), []byte("x"), 0o444); err != nil {
					t.Fatal(err)
				}
			}
			arch := "arm64"
			if problem == "architecture" {
				arch = "amd64"
			}
			service := Service{DataRoot: root, QEMUImg: "/unused-qemu-img"}
			_, err = service.InfoArch(context.Background(), alias.Name, arch)
			if err == nil || strings.Contains(err.Error(), "unknown image \"") {
				t.Fatalf("registered image %s lost its real error: %v", problem, err)
			}
			switch problem {
			case "missing":
				if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "fixture.qcow2") {
					t.Fatalf("missing image error = %v", err)
				}
			case "digest":
				if !errors.Is(err, ErrIntegrity) {
					t.Fatalf("integrity cause was lost: %v", err)
				}
			case "architecture":
				if !strings.Contains(err.Error(), "arm64") || !strings.Contains(err.Error(), "amd64") {
					t.Fatalf("architecture error = %v", err)
				}
			case "registry":
				if !strings.Contains(err.Error(), "registry") {
					t.Fatalf("registry error = %v", err)
				}
			}
		})
	}
}
