package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/image"
)

func TestLocalImageInfoAndPullReportIntegrityFailure(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	t.Setenv("BARN_HOME", root)
	t.Setenv("PATH", bin)
	if err := os.WriteFile(filepath.Join(bin, "qemu-img"), []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "images", "local"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "images", "local", "fixture.qcow2")
	if err := os.WriteFile(path, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	alias := image.LocalAlias{
		Name: "local-fixture", File: "local/fixture.qcow2", Digest: strings.Repeat("a", 64),
		ArtifactSize: 1, VirtualSize: 1, Arch: "arm64", Boot: "uefi", SourceUser: "dba", CreatedAt: time.Now().UTC(),
	}
	registry, err := json.Marshal(image.LocalAliases{Schema: image.LocalAliasesSchema, Aliases: map[string]image.LocalAlias{alias.Name: alias}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "images", "local-images.json"), registry, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"info", "pull"} {
		var out, errOut bytes.Buffer
		code := run([]string{"image", action, alias.Name, "--arch", "arm64", "--json"}, &out, &errOut)
		var failure commandFailure
		if err := json.Unmarshal(out.Bytes(), &failure); err != nil {
			t.Fatalf("%s: invalid JSON %q: %v", action, out.String(), err)
		}
		if code != exitIntegrity || failure.Error != "integrity" || !strings.Contains(failure.Message, "digest mismatch") || !strings.Contains(errOut.String(), "digest mismatch") {
			t.Errorf("%s: code=%d stdout=%s stderr=%s", action, code, out.String(), errOut.String())
		}
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "x" {
		t.Fatal("local image validation replaced the registered artifact")
	}
}
