package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/config"
	"github.com/pgsty/barn/internal/image"
)

func TestInitAndValidateUseTheSameImageAndArchitecture(t *testing.T) {
	t.Setenv("BARN_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("BARN_REPO", "")
	var out, errOut bytes.Buffer
	if code := run([]string{"init", "dual", "-I", "ubuntu24", "-A", "x64", "-o", "-"}, &out, &errOut); code != exitOK {
		t.Fatalf("init: %d %s %s", code, &out, &errOut)
	}
	file, err := config.ParseInventory(out.Bytes())
	if err != nil || file.Arch != "amd64" || !strings.Contains(out.String(), "vm_image: u24") {
		t.Fatalf("init output: %s %v", &out, err)
	}
	path := filepath.Join(t.TempDir(), "pigsty.yml")
	if err := os.WriteFile(path, []byte(overrideInventory), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"validate", "-f", path, "-I", "u24", "-A", "x86_64", "--json"}, &out, &errOut); code != exitOK {
		t.Fatalf("validate: %d %s %s", code, &out, &errOut)
	}
	var result validateResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Resolved.Arch != "amd64" {
		t.Fatalf("validate output: %s %v", &out, err)
	}
	for _, node := range result.Resolved.Nodes {
		if node.Image != "u24" && !(node.Image == "" && result.Resolved.Image == "u24") {
			t.Fatalf("validate retained inventory image: %+v", node)
		}
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"validate", "-f", path, "-I", "el7", "-A", "arm64"}, &out, &errOut); code != exitUsage || !strings.Contains(errOut.String(), "arm64") {
		t.Fatalf("validate missed unavailable architecture: %d %s %s", code, &out, &errOut)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != overrideInventory {
		t.Fatalf("validate changed inventory: %s %v", data, err)
	}
}

func TestImageInfoArchitectureAliasesAndLegacyShorthand(t *testing.T) {
	t.Setenv("BARN_HOME", t.TempDir())
	t.Setenv("BARN_REPO", "")
	for _, test := range []struct{ flag, value, want string }{
		{"-A", "x64", "amd64"}, {"--arch", "a64", "arm64"},
		{"-a", "amd64", "amd64"}, {"-A", "native", runtime.GOARCH},
	} {
		var out, errOut bytes.Buffer
		if code := run([]string{"image", "info", "u24", test.flag, test.value, "--json"}, &out, &errOut); code != exitOK {
			t.Fatalf("info %s %s: %d %s %s", test.flag, test.value, code, &out, &errOut)
		}
		var info image.Info
		if err := json.Unmarshal(out.Bytes(), &info); err != nil || info.Entry.Arch != test.want {
			t.Fatalf("info ignored architecture: %s %v", &out, err)
		}
	}
}

func TestArchitectureCommandsRejectInvalidChoicesBeforeMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("BARN_HOME", root)
	for _, args := range [][]string{
		{"init", "--arch="}, {"init", "-A", "s390x"}, {"init", "--image="},
		{"setup", "--arch="}, {"setup", "-A", "s390x"},
		{"validate", "--arch="}, {"validate", "-A", "s390x"},
		{"image", "info", "--arch="}, {"image", "info", "-a", " "},
		{"image", "import", "missing.qcow2", "-A", "arm64"},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != exitUsage {
			t.Fatalf("%v: %d %s %s", args, code, &out, &errOut)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("invalid choice created state: %v", err)
	}
}

func TestNamedImageImportRecordsTheGuestArchitecture(t *testing.T) {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img is not installed")
	}
	t.Setenv("BARN_HOME", t.TempDir())
	t.Setenv("BARN_REPO", "")
	source := filepath.Join(t.TempDir(), "guest.qcow2")
	if output, err := exec.Command(qemuImg, "create", "-f", "qcow2", source, "64M").CombinedOutput(); err != nil {
		t.Fatalf("create qcow2 fixture: %s %v", output, err)
	}
	guest, alias := "amd64", "x64"
	if runtime.GOARCH == "amd64" {
		guest, alias = "arm64", "a64"
	}
	var out, errOut bytes.Buffer
	args := []string{"image", "import", source, "--name", "local-foreign", "--boot", "uefi", "--source-user", "dba", "-A", alias, "--json"}
	if code := run(args, &out, &errOut); code != exitOK {
		t.Fatalf("import: %d %s %s", code, &out, &errOut)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "info", "local-foreign", "-A", guest, "--json"}, &out, &errOut); code != exitOK {
		t.Fatalf("foreign image not found: %d %s %s", code, &out, &errOut)
	}
	var info image.Info
	if err := json.Unmarshal(out.Bytes(), &info); err != nil || info.Entry.Arch != guest || !info.Cached {
		t.Fatalf("wrong imported metadata: %s %v", &out, err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"image", "info", "local-foreign", "-A", "native"}, &out, &errOut); code == exitOK {
		t.Fatal("foreign image was also labelled as native")
	}
}
