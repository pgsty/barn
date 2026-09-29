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
