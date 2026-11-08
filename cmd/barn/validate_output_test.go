package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateTableReflectsChecksActuallyPerformed(t *testing.T) {
	t.Setenv("BARN_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("BARN_REPO", "")
	for _, test := range []struct {
		name, inventory, state string
		code                   int
		items                  []string
	}{
		{name: "valid", inventory: "all:\n  hosts:\n    10.10.10.10: { nodename: meta, vm_image: u24 }\n", state: "valid", items: []string{"configuration", "nodes", "images"}},
		{name: "parse failure", inventory: "all: [", state: "failed", code: exitUsage, items: []string{"configuration"}},
		{name: "unknown image", inventory: "all:\n  hosts:\n    10.10.10.10: { nodename: meta, vm_image: u99 }\n", state: "failed", code: exitUsage, items: []string{"configuration", "nodes", "images"}},
		{name: "local image", inventory: "all:\n  hosts:\n    10.10.10.10: { nodename: meta, vm_image: local-fixture }\n", state: "deferred", items: []string{"configuration", "nodes", "images"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "barn.yml")
			if err := os.WriteFile(path, []byte(test.inventory), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"validate", "-f", path}, &stdout, &stderr); code != test.code {
				t.Fatalf("code=%d want=%d stdout=%s stderr=%s", code, test.code, &stdout, &stderr)
			}
			got := stdout.String()
			if strings.Contains(got, "\x1b") || strings.Count(got, "ITEM") != 1 || strings.Contains(got, "[DONE]") != (test.code == exitOK) || strings.Contains(got, "[FAILED]") != (test.code != exitOK) {
				t.Fatalf("invalid plain result: %q", got)
			}
			lines := strings.Split(got, "\n")
			for i, item := range test.items {
				if fields := strings.Fields(lines[i+1]); len(fields) < 2 || fields[0] != item {
					t.Fatalf("missing performed check %q: %s", item, got)
				}
			}
			if fields := strings.Fields(lines[len(test.items)]); fields[1] != test.state {
				t.Fatalf("last state=%q want=%q: %s", fields[1], test.state, got)
			}
			if !strings.HasPrefix(lines[len(test.items)+1], "validate ") {
				t.Fatalf("reported unperformed checks: %s", got)
			}
			if test.name == "local image" && !strings.Contains(got, "local images checked by barn up") {
				t.Fatalf("local image deferred verification missing: %s", got)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != test.inventory {
				t.Fatalf("validation changed the inventory: %s %v", data, err)
			}
			if test.code == exitOK {
				stdout.Reset()
				stderr.Reset()
				if code := run([]string{"validate", "-f", path, "--json"}, &stdout, &stderr); code != exitOK {
					t.Fatalf("structured validation failed: %s %s", &stdout, &stderr)
				}
				var result validateResult
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || !result.Valid || result.Source != path || len(result.Resolved.Nodes) != 1 || strings.Contains(stdout.String(), "[DONE]") {
					t.Fatalf("presentation changed JSON result: %s %v", &stdout, err)
				}
			}
		})
	}
}
