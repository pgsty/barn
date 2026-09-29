package hostconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/failure"
)

func fixtureEntries() []Entry {
	return []Entry{
		{Address: "10.10.10.10", Names: []string{"meta", "admin.example.test"}},
		{Address: "10.10.10.11", Names: []string{"node-1"}},
	}
}

func TestReconcileInstallUpdateUninstallPreservesUnownedBytes(t *testing.T) {
	t.Parallel()
	legacy := "127.0.0.1 localhost\n10.9.9.9 legacy # pigsty dns\n"
	before := []byte(legacy)
	after, lines, changed, err := ReconcileContent(before, ActionInstall, fixtureEntries())
	if err != nil || !changed || len(lines) != 2 {
		t.Fatalf("install changed=%t lines=%v err=%v", changed, lines, err)
	}
	if !strings.HasPrefix(string(after), string(before)) || !strings.Contains(string(after), "# barn:begin") {
		t.Fatalf("install did not preserve prefix or add block:\n%s", after)
	}
	idempotent, _, changed, err := ReconcileContent(after, ActionInstall, fixtureEntries())
	if err != nil || changed || string(idempotent) != string(after) {
		t.Fatalf("idempotent install changed=%t err=%v", changed, err)
	}
	updatedEntries := fixtureEntries()
	updatedEntries[0].Names = append(updatedEntries[0].Names, "grafana.example.test")
	updated, _, changed, err := ReconcileContent(after, ActionInstall, updatedEntries)
	if err != nil || !changed || !strings.Contains(string(updated), "grafana.example.test") {
		t.Fatalf("update changed=%t err=%v data=%s", changed, err, updated)
	}
	removed, _, changed, err := ReconcileContent(updated, ActionUninstall, nil)
	if err != nil || !changed || string(removed) != string(before) {
		t.Fatalf("uninstall changed=%t err=%v\nwant=%q\ngot =%q", changed, err, before, removed)
	}
}

func TestReconcileRejectsMalformedMarkersAndConflictingNames(t *testing.T) {
	t.Parallel()
	malformed := []byte("127.0.0.1 localhost\n# barn:begin\n10.10.10.10 meta\n")
	if _, _, _, err := ReconcileContent(malformed, ActionInstall, fixtureEntries()); err == nil {
		t.Fatal("unterminated marker block was accepted")
	}
	legacyMarker := []byte("127.0.0.1 localhost\n# barn:11111111-1111-4111-8111-111111111111:begin\n10.10.10.10 meta\n# barn:11111111-1111-4111-8111-111111111111:end\n")
	if _, _, _, err := ReconcileContent(legacyMarker, ActionInstall, fixtureEntries()); err == nil {
		t.Fatal("pre-simplification per-project marker was accepted")
	}
	duplicate := []byte("# barn:begin\n10.10.10.20 other\n# barn:end\n# barn:begin\n10.10.10.21 more\n# barn:end\n")
	if _, _, _, err := ReconcileContent(duplicate, ActionInstall, fixtureEntries()); err == nil {
		t.Fatal("duplicate Barn hosts block was accepted")
	}
	entries := fixtureEntries()
	entries[1].Names = []string{"meta"}
	if _, _, _, err := ReconcileContent([]byte("127.0.0.1 localhost\n"), ActionInstall, entries); err == nil {
		t.Fatal("one hostname mapped to multiple addresses")
	}
	entries = fixtureEntries()
	entries[0].Names = []string{"bad host"}
	if _, _, _, err := ReconcileContent([]byte("127.0.0.1 localhost\n"), ActionInstall, entries); err == nil {
		t.Fatal("unsafe hostname was accepted")
	}
	entries = fixtureEntries()
	entries[0].Address = "192.0.2.10"
	if _, _, _, err := ReconcileContent([]byte("127.0.0.1 localhost\n"), ActionInstall, entries); err == nil {
		t.Fatal("non-private address was accepted")
	}
}

func TestReconcileRejectsNameConflictOutsideOwnedBlock(t *testing.T) {
	t.Parallel()
	entries := fixtureEntries()
	before := []byte("127.0.0.1 localhost\n192.0.2.50 meta # user managed\n")
	_, _, _, err := ReconcileContent(before, ActionInstall, entries)
	if err == nil {
		t.Fatalf("conflicting existing name was accepted:\n%s", before)
	}
	if class, _, next := failure.Classify(err); class != failure.Conflict || !strings.Contains(next, "192.0.2.50 meta") {
		t.Fatalf("conflict = %q next=%q: %v", class, next, err)
	}
	same := []byte("127.0.0.1 localhost\n10.10.10.10 meta\n")
	if _, _, _, err := ReconcileContent(same, ActionInstall, entries); err != nil {
		t.Fatalf("same-address existing name should be unambiguous: %v", err)
	}
	owned, _, err := renderBlock([]Entry{{Address: "10.10.10.20", Names: []string{"meta"}}})
	if err != nil {
		t.Fatal(err)
	}
	replaced := append([]byte("127.0.0.1 localhost\n"), owned...)
	after, _, changed, err := ReconcileContent(replaced, ActionInstall, entries)
	if err != nil || !changed || strings.Contains(string(after), "10.10.10.20") {
		t.Fatalf("owned-block name was not replaced: changed=%t err=%v\n%s", changed, err, after)
	}
}

func TestApplyHelperIsDigestBoundAndAtomic(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	target := filepath.Join(directory, "hosts")
	before := []byte("127.0.0.1 localhost\n10.9.9.9 legacy # pigsty dns\n")
	if err := os.WriteFile(target, before, 0o644); err != nil {
		t.Fatal(err)
	}
	after, _, _, err := ReconcileContent(before, ActionInstall, fixtureEntries())
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(directory, "staging")
	if err := os.WriteFile(staging, after, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyHelper(target, staging, ActionInstall, digest(before), digest(after), false); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(target)
	if err != nil || string(actual) != string(after) {
		t.Fatalf("applied hosts mismatch: %v\n%s", err, actual)
	}
	info, err := os.Lstat(target)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("target mode = %v, %v", info, err)
	}
}

func TestApplyHelperRefusesStaleTargetAndSymlink(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	target := filepath.Join(directory, "hosts")
	before := []byte("127.0.0.1 localhost\n")
	if err := os.WriteFile(target, before, 0o644); err != nil {
		t.Fatal(err)
	}
	after, _, _, err := ReconcileContent(before, ActionInstall, fixtureEntries())
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(directory, "staging")
	if err := os.WriteFile(staging, after, 0o600); err != nil {
		t.Fatal(err)
	}
	changed := []byte("127.0.0.1 localhost\n192.0.2.1 concurrent\n")
	if err := os.WriteFile(target, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyHelper(target, staging, ActionInstall, digest(before), digest(after), false); err == nil {
		t.Fatal("stale reviewed digest was accepted")
	}
	actual, _ := os.ReadFile(target)
	if string(actual) != string(changed) {
		t.Fatalf("stale apply changed target: %q", actual)
	}
	link := filepath.Join(directory, "hosts-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ApplyHelper(link, staging, ActionInstall, digest(changed), digest(after), false); err == nil {
		t.Fatal("symlink target was accepted")
	}
}

func TestInstalledHelperValidationRejectsUserOwnedAndSymlinkPaths(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	helper := filepath.Join(directory, "barn-hosts-helper")
	if err := os.WriteFile(helper, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateInstalledHelper(helper); err == nil {
		t.Fatal("user-owned helper was accepted for privileged execution")
	}
	link := filepath.Join(directory, "helper-link")
	if err := os.Symlink(helper, link); err != nil {
		t.Fatal(err)
	}
	if err := validateInstalledHelper(link); err == nil {
		t.Fatal("symlink helper was accepted for privileged execution")
	}
	if err := validateInstalledHelper(filepath.Join(directory, "..", filepath.Base(directory), "barn-hosts-helper")); err == nil {
		t.Fatal("non-canonical helper path was accepted")
	}
}
