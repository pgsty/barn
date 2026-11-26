package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pgsty/barn/internal/image"
)

func writeUpdateRepository(t *testing.T, root string, revision uint64) {
	t.Helper()
	catalog := image.EmbeddedCatalog()
	catalog.Version = revision
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, image.CatalogFilename), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateCommandActivatesTheConfiguredCatalog(t *testing.T) {
	t.Setenv("BARN_HOME", filepath.Join(t.TempDir(), "barn-home"))
	repository := t.TempDir()
	revision := uint64(image.EmbeddedManifestVersion + 1)
	writeUpdateRepository(t, repository, revision)

	for attempt := 0; attempt < 2; attempt++ {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"update", "--mirror", "--repo", repository}, &stdout, &stderr); code != exitOK {
			t.Fatalf("update attempt %d code=%d stdout=%q stderr=%q", attempt+1, code, stdout.String(), stderr.String())
		}
		if stderr.Len() != 0 || !strings.Contains(stdout.String(), repository) || !strings.Contains(stdout.String(), "revision") {
			t.Fatalf("update attempt %d stdout=%q stderr=%q", attempt+1, stdout.String(), stderr.String())
		}
		if attempt == 0 && !strings.Contains(stdout.String(), "updated image catalog") {
			t.Fatalf("first update did not report activation: %q", stdout.String())
		}
		if attempt == 1 && !strings.Contains(stdout.String(), "is current") {
			t.Fatalf("second update did not report current: %q", stdout.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--json", "update", "--repo", repository}, &stdout, &stderr); code != exitOK {
		t.Fatalf("JSON update code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var result image.CatalogUpdate
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode JSON update: %v\n%s", err, stdout.String())
	}
	if result.Updated || result.ActiveVersion != revision || result.PreviousVersion != revision || result.Repository != repository {
		t.Fatalf("JSON update result = %#v", result)
	}
}

func TestUpdateCommandFailsWhenTheRepositoryIsUnreachable(t *testing.T) {
	t.Setenv("BARN_HOME", filepath.Join(t.TempDir(), "barn-home"))
	repository := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"update", "--repo", repository}, &stdout, &stderr); code != exitRuntime {
		t.Fatalf("update code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), image.CatalogFilename) {
		t.Fatalf("failed update stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestUpdateCommandContract(t *testing.T) {
	command := newUpdateCommand(&bytes.Buffer{}, &bytes.Buffer{})
	if command.Use != "update" || len(command.Aliases) != 0 {
		t.Fatalf("update command contract = use %q aliases=%v", command.Use, command.Aliases)
	}
	flag := command.LocalNonPersistentFlags().Lookup("repo")
	if flag == nil || flag.Shorthand != "r" || !strings.Contains(command.Long, "does not update the Barn executable") {
		t.Fatalf("update command help/flags = flag %#v long %q", flag, command.Long)
	}
	mirror := command.LocalNonPersistentFlags().Lookup("mirror")
	if mirror == nil || mirror.Shorthand != "" {
		t.Fatalf("update --mirror flag = %#v", mirror)
	}
}

func TestImageListUsesTheLocalCatalogWithoutNetwork(t *testing.T) {
	t.Setenv("BARN_HOME", filepath.Join(t.TempDir(), "barn-home"))
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "offline", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"image", "list", "--repo", server.URL}, &stdout, &stderr); code != exitOK {
		t.Fatalf("image list code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "u24") || stderr.Len() != 0 || requests.Load() != 0 {
		t.Fatalf("offline list stdout=%q stderr=%q requests=%d", stdout.String(), stderr.String(), requests.Load())
	}
}

func TestImageUpdateFreshCache(t *testing.T) {
	home := filepath.Join(t.TempDir(), "barn-home")
	t.Setenv("BARN_HOME", home)
	t.Setenv("BARN_ARCH", "")
	repository := t.TempDir()
	writeUpdateRepository(t, repository, image.EmbeddedManifestVersion+1)

	for _, tc := range []struct {
		args []string
		arch string
	}{
		{},
		{args: []string{"d13i"}, arch: "amd64"},
		{args: []string{"d13", "--arch", "arm64", "d13a"}, arch: "arm64"},
	} {
		var stdout, stderr bytes.Buffer
		args := append([]string{"--json", "image", "update", "--repo", repository, "--dry-run"}, tc.args...)
		if code := run(args, &stdout, &stderr); code != exitOK {
			t.Fatalf("%v: exit %d: %s", tc.args, code, stderr.String())
		}
		var report image.ImageUpdateReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if tc.arch == "" {
			if len(report.Images) != 0 {
				t.Fatalf("empty cache without selectors planned images: %+v", report.Images)
			}
		} else if len(report.Images) != 1 || report.Images[0].Alias != "d13" || report.Images[0].Arch != tc.arch || report.Images[0].State != "planned" {
			t.Fatalf("%v: images = %+v", tc.args, report.Images)
		}
		if _, err := os.Stat(home); !os.IsNotExist(err) {
			t.Fatalf("dry-run created its data directory: %v", err)
		}
	}
}

func TestImageUpdateCommandDryRunAndSelectorContracts(t *testing.T) {
	home := filepath.Join(t.TempDir(), "barn-home")
	t.Setenv("BARN_HOME", home)
	repository := t.TempDir()
	revision := uint64(image.EmbeddedManifestVersion + 2)
	writeUpdateRepository(t, repository, revision)

	// Place a retired older d13 arm64 image in local cache.
	d13Dir := filepath.Join(home, "images", "d13")
	if err := os.MkdirAll(d13Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d13Dir, "d13-20261001.1-arm64.qcow2"), []byte("old-d13"), 0o444); err != nil {
		t.Fatal(err)
	}

	// Dry-run previews catalog and image updates strictly read-only.
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--json", "image", "u", "--dry-run", "--repo", repository}, &stdout, &stderr); code != exitOK {
		t.Fatalf("image update dry-run code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, "images", "manifests", "state.json")); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote manifest state.json: %v", err)
	}
	var report image.ImageUpdateReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode image update JSON: %v\n%s", err, stdout.String())
	}
	if !report.DryRun || !report.Catalog.Updated || report.Catalog.ActiveVersion != revision || len(report.Images) != 1 || report.Images[0].Alias != "d13" || report.Images[0].Arch != "arm64" || report.Images[0].State != "planned" {
		t.Fatalf("dry-run report = %#v", report)
	}

	// Text dry-run renders readable catalog, table, and summary.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"image", "update", "-d", "--repo", repository}, &stdout, &stderr); code != exitOK {
		t.Fatalf("text dry-run code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"would update image catalog", "d13", "arm64", "planned", "20261001.1 -> "} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("text dry-run missing %q:\n%s", want, stdout.String())
		}
	}

	// Deprecated image warning is emitted when planned/updated.
	u22Dir := filepath.Join(home, "images", "u22")
	if err := os.MkdirAll(u22Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(u22Dir, "u22-20261001.0.1-arm64.qcow2"), []byte("old-u22"), 0o444); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"image", "update", "--dry-run", "--repo", repository, "u22"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("u22 dry-run code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "deprecated") {
		t.Fatalf("expected deprecated warning on stderr, got %q", stderr.String())
	}

	// Architecture-suffixed alias conflicts with explicit --arch native.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"image", "update", "el10a", "-A", "native", "--dry-run", "--repo", repository}, &stdout, &stderr); code != exitUsage || !strings.Contains(stderr.String(), "conflicts with --arch native") {
		t.Fatalf("arch conflict code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
