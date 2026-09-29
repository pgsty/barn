package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitCustomPathAndFreshPlan(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("BARN_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	if code := run([]string{"init", "full", "-o", "lab.yml"}, &out, &errOut); code != 0 {
		t.Fatalf("init: %d %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "barn up -f lab.yml") {
		t.Fatalf("unusable next command: %s", out.String())
	}
	data, err := os.ReadFile("lab.yml")
	if err != nil || !strings.Contains(string(data), "vm_image: u24") {
		t.Fatalf("default inventory: %s %v", data, err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"plan", "-f", "lab.yml"}, &out, &errOut); code != 0 {
		t.Fatalf("plan: %d %s", code, errOut.String())
	}
	for _, want := range []string{"create", "u24@", "8 vCPU", "16 GiB RAM", "data:        /data 128 GiB (auto fs) on each node"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("plan missing %q: %s", want, out.String())
		}
	}
}

func TestCorruptDeploymentIsNotReportedAsAbsent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BARN_HOME", root)
	path := filepath.Join(root, "state.json")
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"status", "stop", "ssh"} {
		var out, errOut bytes.Buffer
		if code := run([]string{command, "--json"}, &out, &errOut); code != exitIntegrity {
			t.Fatalf("%s: code %d %s %s", command, code, out.String(), errOut.String())
		}
		if strings.Contains(out.String(), "no deployment") || !strings.Contains(out.String(), "state.json") {
			t.Fatalf("%s hides state corruption: %s", command, out.String())
		}
	}
}

func TestPresentationFlagsRespectOptionValues(t *testing.T) {
	for _, args := range [][]string{{"validate", "-f", "--json"}, {"init", "-o", "--yaml"}, {"image", "import", "image.qcow2", "--name", "--verbose"}} {
		var out bytes.Buffer
		got, _, _, err := prepareOutput(args, &out, &out)
		if err != nil || strings.Join(got, "\x00") != strings.Join(args, "\x00") {
			t.Fatalf("%v parsed as %v: %v", args, got, err)
		}
	}
}

func TestImageImportDigestMismatchIsIntegrityFailure(t *testing.T) {
	t.Setenv("BARN_HOME", t.TempDir())
	// Digest rejection must happen before QEMU runs, even on hosts without it.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "qemu-img"), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	path := filepath.Join(t.TempDir(), "bad.qcow2")
	if err := os.WriteFile(path, []byte("not the expected image"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run([]string{"image", "import", path, "--sha256", strings.Repeat("0", 64), "--json"}, &out, &errOut)
	if code != exitIntegrity || strings.Contains(out.String(), "\"metadata\"") {
		t.Fatalf("import: %d %s %s", code, out.String(), errOut.String())
	}
}

func TestUnattendedFirstUpOffersCompleteSetupRecipe(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("BARN_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	if code := run([]string{"up"}, &out, &errOut); code != exitUsage || !strings.Contains(errOut.String(), "barn setup --yes") {
		t.Fatalf("incomplete first-use guidance: code=%d, %s", code, errOut.String())
	}
	if _, err := os.Stat("barn.yml"); !os.IsNotExist(err) {
		t.Fatalf("unattended up silently created inventory: %v", err)
	}
}
