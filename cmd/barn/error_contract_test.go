package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestErrorContract pins what a user and a script see for the common
// failures: exit code, JSON class/reason/next, and the text error line.
// Change a message here together with its source.
func TestErrorContract(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BARN_HOME", filepath.Join(home, ".barn"))
	t.Chdir(t.TempDir())
	write := func(name, content string) {
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("empty.yml", "")
	write("small.yml", "all:\n  hosts:\n    10.10.10.10: { nodename: meta, vm_mem: 10 }\n")
	write("typo.yml", "all:\n  hosts:\n    10.10.10.10: { nodename: meta, vm_cpus: 2 }\n")
	write("image.yml", "all:\n  hosts:\n    10.10.10.10: { nodename: meta, vm_image: u99 }\n")

	for _, test := range []struct {
		arguments []string
		code      int
		class     string
		reason    string
		message   string
		next      string
	}{
		{arguments: []string{"status"}, code: exitConflict, class: "conflict", reason: "no_deployment", message: "no deployment state found", next: "barn up"},
		{arguments: []string{"ssh"}, code: exitConflict, class: "conflict", reason: "no_deployment", message: "no deployment state found", next: "barn up"},
		{arguments: []string{"up"}, code: exitUsage, class: "usage", reason: "no_inventory", message: "no inventory found in this directory", next: "barn setup --yes, then barn up"},
		{arguments: []string{"plan"}, code: exitUsage, class: "usage", reason: "no_inventory", message: "no inventory found in this directory", next: "barn init"},
		{arguments: []string{"validate", "-f", "missing.yml"}, code: exitUsage, class: "usage", message: "missing.yml does not exist", next: "barn init, or check -f"},
		{arguments: []string{"validate", "-f", "empty.yml"}, code: exitUsage, class: "usage", message: "inventory is empty"},
		{arguments: []string{"validate", "-f", "small.yml"}, code: exitUsage, class: "usage", message: "vm_mem = 10: must be at least 512 MiB"},
		{arguments: []string{"validate", "-f", "typo.yml"}, code: exitUsage, class: "usage", message: "did you mean vm_cpu?"},
		{arguments: []string{"validate", "-f", "image.yml"}, code: exitUsage, class: "usage", reason: "unknown_image", message: `unknown image "u99"; available: `},
		{arguments: []string{"image", "info", "bogus"}, code: exitUsage, class: "usage", reason: "unknown_image", message: `unknown image "bogus"`},
		{arguments: []string{"destroy", "--delete-persistent", "--force"}, code: exitConflict, class: "conflict", message: "no deployment state found", next: "barn purge"},
	} {
		t.Run(strings.Join(test.arguments, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(append([]string{"--json"}, test.arguments...), &stdout, &stderr); code != test.code {
				t.Fatalf("code=%d want %d stdout=%s stderr=%s", code, test.code, &stdout, &stderr)
			}
			var failure commandFailure
			if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil {
				t.Fatalf("failure JSON: %v\n%s", err, &stdout)
			}
			if failure.Error != test.class || failure.Reason != test.reason || !strings.Contains(failure.Message, test.message) || !strings.HasPrefix(failure.Next, test.next) {
				t.Fatalf("failure = %#v", failure)
			}
			stdout.Reset()
			stderr.Reset()
			run(test.arguments, &stdout, &stderr)
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			if !strings.HasPrefix(lines[0], "error: ") || !strings.Contains(lines[0], test.message) {
				t.Fatalf("text error = %q", stderr.String())
			}
			if test.next != "" && lines[len(lines)-1] != "next: "+failure.Next {
				t.Fatalf("text next = %q", stderr.String())
			}
		})
	}

	// Doing nothing is not a failure.
	for _, arguments := range [][]string{nil, {"destroy"}, {"purge"}} {
		var stdout, stderr bytes.Buffer
		if code := run(arguments, &stdout, &stderr); code != exitOK {
			t.Errorf("run %v code=%d stderr=%s", arguments, code, &stderr)
		}
	}
}
