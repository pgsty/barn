package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/hostconfig"
)

func TestHostsInventoryPreviewNeedsNoDeployment(t *testing.T) {
	t.Setenv("BARN_HOME", filepath.Join(t.TempDir(), "empty"))
	path := filepath.Join(t.TempDir(), "lab's inventory.yml")
	input := "all: {vars: {vm_hosts: [{address: 10.10.10.30, names: [barn-test-service.example.test]}]}, hosts: {10.10.10.10: {nodename: barn-test-node}}}\n"
	if err := os.WriteFile(path, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := run([]string{"hosts", "install", "-f", path, "--json"}, &out, &stderr)
	if code != 0 {
		t.Fatalf("code=%d out=%s stderr=%s", code, &out, &stderr)
	}
	var report hostconfig.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Applied || !strings.Contains(strings.Join(report.Plan.Lines, "\n"), "barn-test-service.example.test") {
		t.Fatalf("preview=%+v", report)
	}
	out.Reset()
	stderr.Reset()
	if code := run([]string{"hosts", "install", "-f", path}, &out, &stderr); code != 0 {
		t.Fatalf("text preview code=%d out=%s stderr=%s", code, &out, &stderr)
	}
	if !strings.Contains(out.String(), "barn hosts install -f "+shellQuote(path)+" --yes") {
		t.Fatalf("retry lost inventory: %s", &out)
	}
	out.Reset()
	stderr.Reset()
	if code := run([]string{"hosts", "install", "-f", path + ".missing", "--json"}, &out, &stderr); code != exitUsage {
		t.Fatalf("bad inventory code=%d out=%s stderr=%s", code, &out, &stderr)
	}
}
