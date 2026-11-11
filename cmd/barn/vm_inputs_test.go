package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/config"
	privatevm "github.com/pgsty/barn/internal/private"
	"github.com/pgsty/barn/internal/spec"
)

func isolatedVMInputs(t *testing.T) string {
	t.Helper()
	input := vmInputs{}
	for _, field := range input.fields() {
		t.Setenv(field.environment, "")
	}
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("BARN_HOME", root)
	t.Setenv("BARN_REPO", "")
	return root
}

func TestVMInputsGenerateBasesAndValidateEffectiveResources(t *testing.T) {
	isolatedVMInputs(t)
	t.Setenv("BARN_SCALE", "3")
	t.Setenv("BARN_CPU", "bad")
	path := filepath.Join(t.TempDir(), "lab.yml")
	var out, errOut bytes.Buffer
	if code := run([]string{"init", "full", "-C", "2", "-M", "4GiB", "-S", "2", "-D", "128", "-o", path}, &out, &errOut); code != exitOK {
		t.Fatalf("init %d: %s", code, &errOut)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"vm_image: u24", "vm_arch: native", "vm_cpu: 2", "vm_mem: 4GiB", "vm_scale: 2", "vm_disk: 128GiB"} {
		if !bytes.Contains(data, []byte(field)) {
			t.Fatalf("missing saved %s: %s", field, data)
		}
	}
	if !strings.Contains(out.String(), "4c8g") {
		t.Fatalf("init summary: %s", &out)
	}
	t.Setenv("BARN_CPU", "")
	t.Setenv("BARN_SCALE", " ")
	out.Reset()
	errOut.Reset()
	if code := run([]string{"validate", "-f", path, "--json"}, &out, &errOut); code != exitOK {
		t.Fatalf("validate %d: %s", code, &errOut)
	}
	var result validateResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Resources) != 4 {
		t.Fatalf("resources: %+v", result.Resources)
	}
	for _, resource := range result.Resources {
		if resource.CPU != 2 || resource.Memory != 4*spec.GiB || resource.Scale != 2 || resource.EffectiveCPU != 4 || resource.EffectiveMemory != 8*spec.GiB || resource.Disk != 128*spec.GiB {
			t.Fatalf("lost bases: %+v", resource)
		}
	}
}

func TestVMInputPrecedenceSelectionAndRetryAreStable(t *testing.T) {
	isolatedVMInputs(t)
	t.Setenv("BARN_CPU", "5")
	t.Setenv("BARN_SCALE", "3")
	inventory := "all:\n  vars: {vm_cpu: 2, vm_mem: 4GiB, vm_scale: 2}\n  hosts:\n    10.10.10.10: {nodename: meta, vm_mem: 6GiB}\n    10.10.10.11: {nodename: peer, vm_cpu: 3, vm_scale: 1}\n"
	path := filepath.Join(t.TempDir(), "lab.yml")
	if err := os.WriteFile(path, []byte(inventory), 0600); err != nil {
		t.Fatal(err)
	}
	var previousHash string
	for range 2 {
		var out, errOut bytes.Buffer
		if code := run([]string{"plan", "-f", path, "-C", "2", "meta", "--json"}, &out, &errOut); code != exitOK {
			t.Fatalf("plan %d: %s", code, &errOut)
		}
		var result struct {
			privatevm.LifecyclePlan
			Resources []config.ResourceResolution `json:"resources"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.CPUs != 6 || result.Memory != 18*spec.GiB || len(result.Resources) != 1 || result.Resources[0].Sources["cpu"] != "CLI --cpu" || result.Resources[0].Sources["scale"] != "env BARN_SCALE" {
			t.Fatalf("precedence/selection: %+v", result)
		}
		if previousHash != "" && previousHash != result.SpecHash {
			t.Fatal("repeated command compounded scale")
		}
		previousHash = result.SpecHash
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"plan", "-f", path, "-C", "2", "meta"}, &out, &errOut); code != exitOK {
		t.Fatalf("plan %d: %s", code, &errOut)
	}
	for _, want := range []string{"SPEC", "6c18g", "--cpu 2 --scale 3 meta", "env BARN_SCALE"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != inventory {
		t.Fatal("runtime override rewrote inventory")
	}
}

func TestVMEnvironmentOnlyAppliesToSupportedCommandMode(t *testing.T) {
	root := isolatedVMInputs(t)
	t.Chdir(t.TempDir())
	t.Setenv("BARN_CPU", "bad")
	for _, arguments := range [][]string{{"up"}, {"init", "-o", "-"}, {"setup", "full"}} {
		var out, errOut bytes.Buffer
		if code := run(arguments, &out, &errOut); code != exitUsage || !strings.Contains(errOut.String(), "BARN_CPU") {
			t.Fatalf("%v: %d %s", arguments, code, &errOut)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("invalid input created state: %v", err)
		}
	}
	for _, arguments := range [][]string{{"up", "--help"}, {"version"}, {"image", "info", "u24", "--json"}} {
		var out, errOut bytes.Buffer
		if code := run(arguments, &out, &errOut); code != exitOK {
			t.Fatalf("unrelated env affected %v: %d %s", arguments, code, &errOut)
		}
	}
	data, err := config.Template("meta", "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "lab.yml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, variable := range []string{"BARN_IMAGE", "BARN_ARCH", "BARN_MEM", "BARN_SCALE", "BARN_DISK"} {
		t.Setenv(variable, "bad")
	}
	selection, err := resolveSetupSelectionInputs("", path, "", vmInputs{}, t.TempDir())
	if err != nil || selection.Resolved.Nodes[0].CPUs != 1 {
		t.Fatalf("existing setup read env: %v", err)
	}
	if _, err := resolveSetupSelectionInputs("", path, "", vmInputs{CPU: "2"}, t.TempDir()); err == nil {
		t.Fatal("setup accepted override for existing file")
	}
	input, err := (vmInputs{Image: "u24", Arch: "native", CPU: "2", Mem: "4G", Scale: "1", Disk: "128G"}).resolve(true, true)
	if err != nil || input.Resources.Disk == nil || *input.Resources.Disk != 128*spec.GiB {
		t.Fatalf("resize disk override: %+v %v", input, err)
	}
}

func TestVMInputSnapshotAndStateFallback(t *testing.T) {
	isolatedVMInputs(t)
	t.Setenv("BARN_CPU", "2")
	input, err := (vmInputs{}).resolve(true, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BARN_CPU", "4")
	t.Setenv("BARN_SCALE", "3")
	retry, err := input.resolve(true, true)
	if err != nil || retry.CPU != "2" || retry.Scale != "" {
		t.Fatalf("retry reread env: %+v %v", retry, err)
	}
	original := quickResolved(true, false)
	original.Nodes[0].CPUs = 4
	resolved, _, err := applyStateResources(original, nil, input)
	if err != nil || resolved.Nodes[0].CPUs != 2 || original.Nodes[0].CPUs != 4 {
		t.Fatalf("state fallback: %+v %v", resolved, err)
	}
	scaled, err := (vmInputs{Scale: "1"}).resolve(false, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := applyStateResources(original, nil, scaled); err == nil || !strings.Contains(err.Error(), "-f") {
		t.Fatalf("state-only scale accepted: %v", err)
	}
}

func TestSetupPreservesResourceBasesAcrossReuseAndRebase(t *testing.T) {
	isolatedVMInputs(t)
	cwd := t.TempDir()
	selection, err := resolveSetupSelectionInputs("dual", "", "", vmInputs{CPU: "2", Mem: "4G", Scale: "2", Disk: "128"}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishSetupConfig(selection); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSetupSelectionInputs("dual", "", "", vmInputs{CPU: "4", Mem: "8G", Scale: "1", Disk: "128"}, cwd); err == nil {
		t.Fatal("same final spec concealed different bases")
	}
	selection, err = resolveSetupSelection("", "", "", cwd)
	if err != nil || !selection.Generated {
		t.Fatalf("recognize: %v", err)
	}
	if err := selection.rebaseGenerated("172.31.251.0/24"); err != nil {
		t.Fatal(err)
	}
	resource := selection.File.Resources[0]
	if resource.CPU != 2 || resource.Memory != 4*spec.GiB || resource.Scale != 2 || resource.Disk != 128*spec.GiB {
		t.Fatalf("rebase changed bases: %+v", resource)
	}
}

func TestVMRootDiskLowerBoundBeforeMutation(t *testing.T) {
	root := isolatedVMInputs(t)
	path := filepath.Join(t.TempDir(), "lab.yml")
	if err := os.WriteFile(path, []byte("all: {hosts: {10.10.10.10: {nodename: meta}}}"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"plan", "validate"} {
		var out, errOut bytes.Buffer
		if code := run([]string{action, "-f", path, "-D", "1GiB"}, &out, &errOut); code != exitUsage || !strings.Contains(errOut.String(), "virtual size") {
			t.Fatalf("%s: %d %s", action, code, &errOut)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "nodes")); !os.IsNotExist(err) {
		t.Fatalf("created nodes: %v", err)
	}
}

func TestVMParameterCommandCapabilities(t *testing.T) {
	root := newRootCommand(&bytes.Buffer{}, &bytes.Buffer{})
	for _, name := range []string{"init", "setup", "validate", "plan", "up", "reload", "recreate", "resize", "start", "stop", "restart", "status", "destroy", "purge"} {
		command, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		input := vmInputs{}
		for _, field := range input.fields() {
			want := slices.Contains([]string{"init", "setup", "validate", "plan", "up", "reload", "recreate", "resize"}, name)
			flag := command.Flags().Lookup(field.name)
			if want && (flag == nil || flag.Shorthand != field.short) || !want && flag != nil {
				t.Fatalf("%s --%s capability mismatch", name, field.name)
			}
		}
	}
}

func TestSpecDisplayUsesEffectiveResourcesAndPreservesPrecision(t *testing.T) {
	for _, test := range []struct {
		cpu    int
		memory int64
		want   string
	}{
		{1, 2 * spec.GiB, "1c2g"}, {8, 16 * spec.GiB, "8c16g"}, {2, 1536 << 20, "2c1.5g"}, {1, 512 << 20, "1c0.5g"}, {2, spec.GiB + 1, "2c1073741825b"}, {0, 0, "-"},
	} {
		if got := compactSpec(test.cpu, test.memory); got != test.want {
			t.Fatalf("got %s want %s", got, test.want)
		}
	}
	applied := quickResolved(false, false)
	desired := quickResolved(false, false)
	desired.Nodes[0].CPUs = 4
	desired.Nodes[0].Memory = 8 * spec.GiB
	var out bytes.Buffer
	printPlanTable(&out, privatevm.LifecyclePlan{Nodes: []string{"meta"}, Resize: []string{"meta"}}, desired, &applied)
	if !strings.Contains(out.String(), "2c4g → 4c8g") {
		t.Fatalf("change display: %s", &out)
	}
	item := &progress{enabled: true, nodes: make(map[string]activity.Event), context: &outputContext{columns: 80}, started: time.Now()}
	item.Report(activity.Event{Node: "meta", Address: "10.10.10.10", CPUs: 4, Memory: 8 * spec.GiB, State: "starting"})
	item.Report(activity.Event{Node: "meta", State: "ready"})
	text := strings.Join(item.renderNodes(80), "\n")
	if !strings.Contains(text, "SPEC") || !strings.Contains(text, "4c8g") {
		t.Fatalf("progress lost spec: %s", text)
	}
}
