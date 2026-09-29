package image

import (
	"path/filepath"
	"testing"
)

func TestRepositorySources(t *testing.T) {
	t.Parallel()
	catalog, err := RepositoryCatalogSource("http://m0/repos/barn/")
	if err != nil || catalog != "http://m0/repos/barn/catalog.json" {
		t.Fatalf("catalog source = %q, %v", catalog, err)
	}
	artifact, err := RepositoryArtifactSource("http://m0/repos/barn", "u24/u24-1-arm64.qcow2")
	if err != nil || artifact != "http://m0/repos/barn/u24/u24-1-arm64.qcow2" {
		t.Fatalf("artifact source = %q, %v", artifact, err)
	}
	flat, err := RepositoryArtifactSource("https://repo.example/barn", "images/d13-1-arm64.qcow2")
	if err != nil || flat != "https://repo.example/barn/images/d13-1-arm64.qcow2" {
		t.Fatalf("flat artifact source = %q, %v", flat, err)
	}
	canonical, err := RepositoryCatalogSource("https://repo.pigsty.io/barn/nested/../.")
	if err != nil || canonical != "https://repo.pigsty.io/barn/catalog.json" {
		t.Fatalf("canonical official catalog source = %q, %v", canonical, err)
	}
	local := filepath.Join(t.TempDir(), "repo")
	localCatalog, err := RepositoryCatalogSource(local)
	if err != nil || localCatalog != filepath.Join(local, CatalogFilename) {
		t.Fatalf("local catalog source = %q, %v", localCatalog, err)
	}
}

func TestPublicDefaultRepository(t *testing.T) {
	t.Parallel()
	const expected = "https://repo.pigsty.io/barn"
	if DefaultRepositoryURL != expected {
		t.Fatalf("default repository = %q, want %q", DefaultRepositoryURL, expected)
	}
	if MirrorRepositoryURL != "https://repo.pigsty.cc/barn" {
		t.Fatalf("mirror repository = %q", MirrorRepositoryURL)
	}
	for _, repository := range []string{
		GlobalRepositoryURL,
		ChinaRepositoryURL,
		GlobalRepositoryURL + "/",
		ChinaRepositoryURL + "/.",
		GlobalRepositoryURL + "/nested/..",
		GlobalRepositoryURL + "/%2e",
		"https://REPO.PIGSTY.IO.:443/barn",
	} {
		if RepositoryAllowsUnsigned(repository) {
			t.Errorf("official repository %q unexpectedly permits an unsigned catalog", repository)
		}
	}
}

func TestResolveRepositoryPrecedence(t *testing.T) {
	t.Setenv("BARN_REPO", "https://environment.example/barn")

	for _, test := range []struct {
		name       string
		repository string
		mirror     bool
		want       string
		explicit   bool
	}{
		{name: "default", want: GlobalRepositoryURL},
		{name: "environment", want: "https://environment.example/barn", explicit: true},
		{name: "mirror beats environment", mirror: true, want: ChinaRepositoryURL, explicit: true},
		{name: "repo beats mirror", repository: "https://command.example/barn/", mirror: true, want: "https://command.example/barn", explicit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "default" {
				t.Setenv("BARN_REPO", "")
			}
			got, explicit, err := ResolveRepository(test.repository, test.mirror)
			if err != nil || got != test.want || explicit != test.explicit {
				t.Fatalf("ResolveRepository(%q, %t) = %q, %t, %v; want %q, %t", test.repository, test.mirror, got, explicit, err, test.want, test.explicit)
			}
		})
	}
}

func TestRepositoryRejectsAmbiguousLocations(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"relative/repo", "ftp://example.com/repo", "https://user@example.com/repo", "https://example.com/repo?q=1"} {
		if _, err := NormalizeRepository(value); err == nil {
			t.Errorf("unsafe repository %q accepted", value)
		}
	}
	if _, err := RepositoryArtifactSource("https://example.com/repo", "../escape.qcow2"); err == nil {
		t.Fatal("unsafe artifact path accepted")
	}
}
