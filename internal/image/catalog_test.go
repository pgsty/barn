package image

import (
	"bytes"
	"strings"
	"testing"
)

func TestEmbeddedCatalogRoundTrip(t *testing.T) {
	t.Parallel()
	data, err := EmbeddedCatalogBytes()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := strictCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := catalog.Entry("ubuntu2404", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if entry.SHA256 != "1d6bffe64b848468ac97f821d369a4846d983de1800ccf6b5ec8853e85cefc55" || entry.ArtifactSize != 620224512 || entry.VirtualSize != 3758096384 || catalog.Version != EmbeddedManifestVersion || catalog.Defaults.Image != "u24" || entry.Channel != "stable" {
		t.Fatalf("entry/catalog = %#v %#v", entry, catalog)
	}
	if len(catalog.Images) != 9 || len(catalog.Entries()) != 45 {
		t.Fatalf("embedded catalog matrix = %d images / %d entries", len(catalog.Images), len(catalog.Entries()))
	}
	for _, alias := range formalAliases {
		record, ok := catalog.Images[alias]
		wantVersions := 1
		switch alias {
		case "d13":
			wantVersions = 2
		case "d12", "u22", "u24", "u26":
			wantVersions = 3
		case "el9":
			wantVersions = 4
		case "el10":
			wantVersions = 3
		}
		if !ok || len(record.Versions) != wantVersions || record.Channels["stable"] == "" {
			t.Errorf("catalog image %s = %#v", alias, record)
			continue
		}
		for _, version := range record.Versions {
			want := 2
			if alias == "el7" {
				want = 1
			}
			if len(version.Variants) != want {
				t.Errorf("catalog image %s architectures = %v", alias, version.Variants)
			}
		}
	}
}

func TestEmbeddedCatalogSupportPolicy(t *testing.T) {
	t.Parallel()
	catalog := EmbeddedCatalog()
	for _, test := range []struct {
		reference string
		arch      string
		status    string
	}{
		{"d12", "amd64", "supported"},
		{"d13", "arm64", "supported"},
		{"el8", "amd64", "supported"},
		{"el9@9.8", "arm64", "supported"},
		{"el9@9.7", "amd64", "supported"},
		{"el9@9.6", "arm64", "deprecated"},
		{"el9@9.3", "amd64", "deprecated"},
		{"el10@10.2", "arm64", "supported"},
		{"el10@10.1", "amd64", "supported"},
		{"el10@10.0", "arm64", "deprecated"},
		{"el7", "amd64", "deprecated"},
		{"u22", "arm64", "supported"},
		{"u24", "amd64", "supported"},
		{"u26", "arm64", "supported"},
	} {
		entry, err := catalog.Entry(test.reference, test.arch)
		if err != nil || entry.Status != test.status {
			t.Errorf("catalog status %s/%s = %q, %v; want %q", test.reference, test.arch, entry.Status, err, test.status)
		}
	}
}

func TestCatalogRejectsUnknownAndTrailing(t *testing.T) {
	t.Parallel()
	data, _ := EmbeddedCatalogBytes()
	data = bytes.Replace(data, []byte(`"schema": 3`), []byte(`"schema": 3, "unknown": true`), 1)
	if _, err := strictCatalog(data); err == nil {
		t.Fatal("unknown manifest field accepted")
	}
	valid, _ := EmbeddedCatalogBytes()
	if _, err := strictCatalog(append(valid, []byte(`{}`)...)); err == nil {
		t.Fatal("trailing manifest JSON accepted")
	}
}

func TestCatalogRejectsIncompleteArtifactAndMovingURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Artifact)
	}{
		{name: "artifact size", mutate: func(artifact *Artifact) { artifact.ArtifactSize = 0 }},
		{name: "virtual size", mutate: func(artifact *Artifact) { artifact.VirtualSize = 0 }},
		{name: "unsafe file", mutate: func(artifact *Artifact) { artifact.File = "../image.qcow2" }},
		{name: "credential URL", mutate: func(artifact *Artifact) { artifact.Upstream = "https://user:secret@example.test/image.qcow2" }},
		{name: "query URL", mutate: func(artifact *Artifact) { artifact.Upstream = "https://example.test/image.qcow2?token=secret" }},
		{name: "fragment URL", mutate: func(artifact *Artifact) { artifact.Upstream = "https://example.test/image.qcow2#release" }},
		{name: "latest URL", mutate: func(artifact *Artifact) {
			artifact.Upstream = "https://cloud.debian.org/images/cloud/trixie/latest/image.qcow2"
		}},
		{name: "current URL", mutate: func(artifact *Artifact) {
			artifact.Upstream = "https://cloud-images.ubuntu.com/noble/current/image.img"
		}},
		{name: "release symlink URL", mutate: func(artifact *Artifact) {
			artifact.Upstream = "https://cloud-images.ubuntu.com/minimal/releases/noble/release/image.img"
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			catalog := EmbeddedCatalog()
			record := catalog.Images["u24"]
			version := record.Versions["20260801.0.0"]
			artifact := version.Variants["arm64"]
			test.mutate(&artifact)
			version.Variants["arm64"] = artifact
			record.Versions["20260801.0.0"] = version
			catalog.Images["u24"] = record
			if err := catalog.Validate(); err == nil {
				t.Fatal("invalid catalog artifact unexpectedly accepted")
			}
		})
	}
}

func TestCatalogRejectsReservedLocalAliasNamespace(t *testing.T) {
	t.Parallel()
	for _, mutate := range []func(*Catalog){
		func(catalog *Catalog) {
			catalog.Images["local"] = catalog.Images["u24"]
			delete(catalog.Images, "u24")
		},
		func(catalog *Catalog) {
			catalog.Images["local-custom"] = catalog.Images["u24"]
			delete(catalog.Images, "u24")
		},
		func(catalog *Catalog) {
			record := catalog.Images["u24"]
			record.Aliases = append(record.Aliases, "local")
			catalog.Images["u24"] = record
		},
		func(catalog *Catalog) {
			record := catalog.Images["u24"]
			record.Aliases = append(record.Aliases, "local-custom")
			catalog.Images["u24"] = record
		},
	} {
		catalog := EmbeddedCatalog()
		catalog.Defaults.Image = "d13"
		mutate(&catalog)
		if err := catalog.Validate(); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("catalog reserved namespace error = %v", err)
		}
	}
}

func TestResolveVersionIsDeterministicUnderMapOrder(t *testing.T) {
	t.Parallel()
	versions := map[string]CatalogVersion{"9.7": {}, "9.8": {}, "9.10": {}}
	for attempt := 0; attempt < 200; attempt++ {
		got, err := resolveVersion(versions, "9")
		if err != nil || got != "9.10" {
			t.Fatalf("attempt %d: resolveVersion = %q, %v; want 9.10", attempt, got, err)
		}
	}
	// A clear winner stays a clear winner: an equal-ranking pair further down the
	// list is irrelevant to the answer and must not turn into a sporadic error.
	shadowed := map[string]CatalogVersion{"9.7": {}, "9.7.0": {}, "9.8": {}}
	for attempt := 0; attempt < 200; attempt++ {
		got, err := resolveVersion(shadowed, "9")
		if err != nil || got != "9.8" {
			t.Fatalf("attempt %d: resolveVersion = %q, %v; want 9.8", attempt, got, err)
		}
	}
	// Only a tie for first place is genuinely ambiguous, and it must name the
	// same two versions every time.
	tied := map[string]CatalogVersion{"9.7": {}, "9.7.0": {}}
	for attempt := 0; attempt < 200; attempt++ {
		got, err := resolveVersion(tied, "9")
		if err == nil || !strings.Contains(err.Error(), `ambiguous between semantically equal versions "9.7" and "9.7.0"`) {
			t.Fatalf("attempt %d: resolveVersion = %q, %v; want a stable ambiguity error", attempt, got, err)
		}
	}
}

func TestCatalogRejectsIdenticallyRankedVersions(t *testing.T) {
	t.Parallel()
	catalog := EmbeddedCatalog()
	record := catalog.Images["el9"]
	versions := make(map[string]CatalogVersion, len(record.Versions)+1)
	for release, version := range record.Versions {
		versions[release] = version
	}
	// A trailing zero ranks identically; reject the pair before resolution can
	// report a runtime ambiguity.
	stable := record.Channels["stable"]
	versions[stable+".0"] = versions[stable]
	record.Versions = versions
	catalog.Images["el9"] = record
	err := catalog.Validate()
	if err == nil || !strings.Contains(err.Error(), "rank identically") {
		t.Fatalf("Validate accepted identically ranked versions: %v", err)
	}
}
