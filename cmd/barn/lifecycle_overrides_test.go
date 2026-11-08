package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/config"
	privatevm "github.com/pgsty/barn/internal/private"
	"github.com/pgsty/barn/internal/spec"
)

const overrideInventory = `all:
  vars: { vm_image: el9, vm_version: 9, vm_arch: arm64 }
  children:
    nodes:
      hosts:
        10.10.10.10: { nodename: meta }
        10.10.10.11: { nodename: node1, vm_image: d13, vm_version: 13 }
`

func TestLifecycleOverridesReplaceInheritedAndNodeImagesWithoutMutatingSource(t *testing.T) {
	t.Parallel()
	file, err := config.ParseInventory([]byte(overrideInventory))
	if err != nil {
		t.Fatal(err)
	}
	original, err := file.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	originalHash, _ := spec.Hash(original)
	options, err := normalizeLifecycleOverrides(lifecycleOptions{Image: "ubuntu24:stable", Arch: "x64"})
	if err != nil || options.Image != "u24:stable" || options.Arch != "amd64" {
		t.Fatalf("normalized options: %+v, %v", options, err)
	}
	for _, selected := range [][]string{nil, {"meta"}} {
		resolved := applyLifecycleOverrides(original, selected, options)
		if resolved.Arch != "amd64" || resolved.Image != original.Image || resolved.Nodes[0].Image != "u24:stable" {
			t.Fatalf("wrong override: %+v", resolved)
		}
		if selected == nil && resolved.Nodes[1].Image != "u24:stable" || selected != nil && resolved.Nodes[1].Image != original.Nodes[1].Image {
			t.Fatalf("wrong selected image scope: %+v", resolved.Nodes)
		}
		if repeated := applyLifecycleOverrides(resolved, selected, options); !reflect.DeepEqual(repeated, resolved) {
			t.Fatal("host-setup retry changed override semantics")
		}
	}
	if got, _ := spec.Hash(original); got != originalHash {
		t.Fatal("override mutated the source specification")
	}
	native := applyLifecycleOverrides(original, nil, lifecycleOptions{Arch: "native"})
	if native.Arch != "" {
		t.Fatalf("native did not reset inventory architecture: %q", native.Arch)
	}
	// Writing the equivalent vm_* values must not introduce a second round of
	// drift caused by a different default-image envelope or redundant images.
	persistedText := strings.ReplaceAll(overrideInventory, "vm_image: el9, vm_version: 9, vm_arch: arm64", "vm_image: u24, vm_arch: amd64")
	persistedText = strings.ReplaceAll(persistedText, ", vm_image: d13, vm_version: 13", "")
	persistedFile, err := config.ParseInventory([]byte(persistedText))
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := persistedFile.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	overridden := applyLifecycleOverrides(original, nil, lifecycleOptions{Image: "u24", Arch: "amd64"})
	if !reflect.DeepEqual(persisted, overridden) {
		t.Fatalf("saved config and override differ:\nsaved=%+v\noverride=%+v", persisted, overridden)
	}
}

func TestPlanUsesImageAndArchOverridesWithoutWritingInventory(t *testing.T) {
	t.Setenv("BARN_HOME", filepath.Join(t.TempDir(), "barn"))
	t.Setenv("BARN_REPO", "")
	path := filepath.Join(t.TempDir(), "inventory.yml")
	if err := os.WriteFile(path, []byte(overrideInventory), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"plan", "-f", path, "-I", "u24", "-A", "x86_64", "--json"}, &out, &errOut); code != exitOK {
		t.Fatalf("plan failed: %d %s %s", code, &out, &errOut)
	}
	var plan privatevm.LifecyclePlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Images) != 1 || !strings.HasPrefix(plan.Images[0], "u24@") || !strings.HasSuffix(plan.Images[0], "/amd64") || len(plan.Create) != 2 {
		t.Fatalf("plan ignored image/arch override: %+v", plan)
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != overrideInventory {
		t.Fatalf("inventory changed: %q, %v", content, err)
	}
	for _, want := range []string{"vm_image", "vm_version", "vm_arch"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("missing persistence hint %q: %s", want, &errOut)
		}
	}
}

func TestUpRejectsInvalidOverridesBeforePreparingHost(t *testing.T) {
	root := filepath.Join(t.TempDir(), "barn")
	t.Setenv("BARN_HOME", root)
	for _, args := range [][]string{
		{"-I", "el9/name"}, {"--image="}, {"-I", " "},
		{"-A", "s390x"}, {"--arch="}, {"-A", " "},
	} {
		var out, errOut bytes.Buffer
		if code := run(append([]string{"up"}, args...), &out, &errOut); code != exitUsage {
			t.Fatalf("up %v: %d %s %s", args, code, &out, &errOut)
		}
		if !strings.Contains(errOut.String(), "--image") && !strings.Contains(errOut.String(), "--arch") {
			t.Fatalf("missing flag error for %v: %s", args, &errOut)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("invalid overrides created state: %v", err)
	}
}

func TestScopedPlanFollowUpPreservesSelectorsAndOverrides(t *testing.T) {
	t.Setenv("BARN_HOME", filepath.Join(t.TempDir(), "barn"))
	t.Setenv("BARN_REPO", "")
	path := filepath.Join(t.TempDir(), "inventory.yml")
	if err := os.WriteFile(path, []byte(overrideInventory), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"plan", "-f", path, "-I", "u24", "-A", "x86_64", "node1"}, &out, &errOut); code != exitOK {
		t.Fatalf("plan failed: %d %s %s", code, &out, &errOut)
	}
	want := "barn up -f " + shellQuote(path) + " --image u24 --arch amd64 node1"
	if !strings.Contains(out.String(), "apply: "+want+")") {
		t.Fatalf("scoped plan expanded its follow-up scope; want %q in:\n%s", want, &out)
	}
}
