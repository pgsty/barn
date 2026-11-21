package image

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

type embeddedGolden struct {
	release      string
	url          string
	sha256       string
	artifactSize int64
	virtualSize  int64
	sourceUser   string
}

func embeddedEntries(t *testing.T) []Entry {
	t.Helper()
	catalog := EmbeddedCatalog()
	entries := make([]Entry, 0, len(formalMatrix))
	for _, key := range formalMatrix {
		imageName, arch, _ := strings.Cut(key, "/")
		entry, err := catalog.Entry(imageName, arch)
		if err != nil {
			t.Fatalf("invalid embedded image matrix entry %s: %v", key, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestEmbeddedFormalGuestMatrixExact(t *testing.T) {
	t.Parallel()
	expected := map[string]embeddedGolden{
		"el7/amd64": {
			release: "7.9.20221112.0", url: "https://cloud.centos.org/centos/7/images/CentOS-7-x86_64-GenericCloud-2211.qcow2",
			sha256: "284aab2b23d91318f169ff464bce4d53404a15a0618ceb34562838c59af4adea", artifactSize: 902889472, virtualSize: 8589934592, sourceUser: "centos",
		},
		"el8/amd64": {
			release: "8.10.20240528.3", url: "",
			sha256: "32beda4ee88880fd989d2887cb2ed34ac39a1423fc85c8591a5cbd850fe0cc71", artifactSize: 2098659328, virtualSize: 10737418240, sourceUser: "dba",
		},
		"el8/arm64": {
			release: "8.10.20240528.3", url: "",
			sha256: "9dc4c04a521c0aa7072a12ff523403d167d79302c1b5e1b28ceab8b3e9b55de6", artifactSize: 1958019072, virtualSize: 10737418240, sourceUser: "dba",
		},
		"el9/amd64": {
			release: "9.8.20260525.2", url: "",
			sha256: "97fcb5f117e364a6c26a3536adc2a8f517acabeaf2a0ece30330f16753e7b6dd", artifactSize: 655622144, virtualSize: 10737418240, sourceUser: "dba",
		},
		"el9/arm64": {
			release: "9.8.20260525.2", url: "",
			sha256: "28628f3ee93d35b0c21b2e53d568817380f9764ae4b89f0eeb0bde9d503c313b", artifactSize: 529727488, virtualSize: 10737418240, sourceUser: "dba",
		},
		"el10/amd64": {
			release: "10.2.20260525.0", url: "https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base-10.2-20260525.0.x86_64.qcow2",
			sha256: "9fc9e9ff16888bb68ac39b0392e25c9c92684d50c85f1cce6ab549363bbc4b48", artifactSize: 544997376, virtualSize: 10737418240, sourceUser: "rocky",
		},
		"el10/arm64": {
			release: "10.2.20260525.0", url: "https://dl.rockylinux.org/pub/rocky/10/images/aarch64/Rocky-10-GenericCloud-Base-10.2-20260525.0.aarch64.qcow2",
			sha256: "457c8375e19496f43a25c4a6169fa11237536c53cef6f85a20ea3c5a751aa0f5", artifactSize: 469368832, virtualSize: 10737418240, sourceUser: "rocky",
		},
		"d12/amd64": {
			release: "20261026.2643.1", url: "",
			sha256: "57b86ed2748fe41126ce9bdb48af5f289d9a10fb69e1bee4fb363c1649164dad", artifactSize: 758841344, virtualSize: 3221225472, sourceUser: "dba",
		},
		"d12/arm64": {
			release: "20261026.2643.1", url: "",
			sha256: "73bf46a766747d7647263c917065ae218f0334e60a78b92c64bc950cbf5cab76", artifactSize: 749142016, virtualSize: 3221225472, sourceUser: "dba",
		},
		"d13/amd64": {
			release: "20261103.2651.1", url: "",
			sha256: "3d23da279db3b16c4057d35ba7bce5a4c4a56c94467c50f62e39257e58b45584", artifactSize: 539885568, virtualSize: 3221225472, sourceUser: "dba",
		},
		"d13/arm64": {
			release: "20261103.2651.1", url: "",
			sha256: "24e2fb8bf997e3e0b4e4c429081a161eed3611f7bc5c2a75ddbbca43f98fb474", artifactSize: 595853312, virtualSize: 3221225472, sourceUser: "dba",
		},
		"u22/amd64": {
			release: "20261020.0.1", url: "",
			sha256: "c321f1fee530ceabf505d4b7b06398527d7ace4acc5e8a0ce73f35d4e56511f1", artifactSize: 734920704, virtualSize: 2361393152, sourceUser: "dba",
		},
		"u22/arm64": {
			release: "20261020.0.1", url: "",
			sha256: "501de7321ba8b9e63dd930e6d2137b67d52ac722257c56c361979d9ba2946393", artifactSize: 705560576, virtualSize: 2361393152, sourceUser: "dba",
		},
		"u24/amd64": {
			release: "20261017.0.1", url: "",
			sha256: "84c5d706bd6077cebd2738fcdc67bdc9e41e7f2c37f62b3b578f0c9448308937", artifactSize: 626393088, virtualSize: 3758096384, sourceUser: "dba",
		},
		"u24/arm64": {
			release: "20261017.0.1", url: "",
			sha256: "128cb37a18174c14f1976d752ce27cf9d284802efcb1fc0e730f0abb456cb06f", artifactSize: 621608960, virtualSize: 3758096384, sourceUser: "dba",
		},
		"u26/amd64": {
			release: "20261016.0.1", url: "",
			sha256: "87982ec7e107dcd00367aaa5d15a1a08f8d2babb7e58cc3142fd168725b73c10", artifactSize: 864288768, virtualSize: 3758096384, sourceUser: "dba",
		},
		"u26/arm64": {
			release: "20261016.0.1", url: "",
			sha256: "ac9634f0ff07d16531868200cef46cda087f3088d4433a5c6510d3e238b3f00f", artifactSize: 945618944, virtualSize: 3758096384, sourceUser: "dba",
		},
	}

	entries := embeddedEntries(t)
	if len(entries) != len(expected) {
		t.Fatalf("EmbeddedEntries count = %d, want %d", len(entries), len(expected))
	}

	wantOrder := append([]string(nil), formalMatrix...)
	gotOrder := make([]string, 0, len(entries))
	perAlias := make(map[string]map[string]bool)
	for _, entry := range entries {
		key := entry.Alias + "/" + entry.Arch
		gotOrder = append(gotOrder, key)
		want, ok := expected[key]
		if !ok {
			t.Errorf("unexpected embedded entry %s", key)
			continue
		}
		if entry.Release != want.release || entry.Upstream != want.url || entry.SHA256 != want.sha256 || entry.ArtifactSize != want.artifactSize || entry.VirtualSize != want.virtualSize || entry.SourceUser != want.sourceUser {
			t.Errorf("%s metadata mismatch:\n got %#v\nwant %#v", key, entry, want)
		}
		wantBoot, wantStatus := "uefi", "supported"
		switch entry.Alias {
		case "el7":
			wantBoot, wantStatus = "bios", "deprecated"
		case "el8", "u22":
			wantStatus = "deprecated"
		}
		if entry.Format != "qcow2" || entry.Boot != wantBoot || entry.Status != wantStatus || strings.TrimSpace(entry.Provenance) == "" {
			t.Errorf("%s incomplete policy fields: %#v", key, entry)
		}
		parsed, err := url.Parse(entry.Upstream)
		if err != nil || hasMovingReleasePath(parsed.Path) || strings.Contains(strings.ToLower(entry.Upstream), "latest") {
			t.Errorf("%s has a moving or invalid URL: %q", key, entry.Upstream)
		}
		if entry.Upstream != "" && parsed.Host != "dl.rockylinux.org" && parsed.Host != "cloud.debian.org" && parsed.Host != "cloud-images.ubuntu.com" && parsed.Host != "cloud.centos.org" {
			t.Errorf("%s is not on an expected distribution-owned host: %q", key, parsed.Host)
		}
		if entry.ArtifactSize <= 0 || entry.VirtualSize <= 0 || entry.ArtifactSize > entry.VirtualSize {
			t.Errorf("%s has invalid byte sizes: %#v", key, entry)
		}
		if perAlias[entry.Alias] == nil {
			perAlias[entry.Alias] = make(map[string]bool)
		}
		perAlias[entry.Alias][entry.Arch] = true
		resolved, err := embeddedEntry(entry.Alias, entry.Arch)
		if err != nil || resolved.SHA256 != entry.SHA256 || resolved.Release != entry.Release || resolved.Arch != entry.Arch {
			t.Errorf("Embedded(%s, %s) = %#v, %v", entry.Alias, entry.Arch, resolved, err)
		}
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Errorf("EmbeddedEntries order = %v, want %v", gotOrder, wantOrder)
	}
	if arches := perAlias["el7"]; len(arches) != 1 || !arches["amd64"] {
		t.Errorf("el7 architecture set = %v", arches)
	}
	for _, alias := range formalAliases {
		if alias == "el7" {
			continue
		}
		arches := perAlias[alias]
		if len(arches) != 2 || !arches["amd64"] || !arches["arm64"] {
			t.Errorf("%s architecture set = %v", alias, arches)
		}
	}
}

func TestEmbeddedFriendlyAliases(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"c7": "el7", "centos7": "el7", "centos79": "el7",
		"rocky8": "el8",
		"rocky9": "el9",
		"e":      "el10", "el": "el10", "rocky": "el10", "rocky10": "el10",
		"debian12": "d12", "bookworm": "d12",
		"d": "d13", "debian13": "d13", "debian": "d13", "trixie": "d13",
		"ubuntu22": "u22", "ubuntu2204": "u22", "jammy": "u22",
		"ubuntu24": "u24", "ubuntu2404": "u24", "noble": "u24",
		"u": "u26", "ubuntu": "u26", "ubuntu26": "u26", "ubuntu2604": "u26", "resolute": "u26",
	}
	for alias, canonical := range cases {
		if got := CanonicalAlias("  " + strings.ToUpper(alias) + "  "); got != canonical {
			t.Errorf("CanonicalAlias(%q) = %q, want %q", alias, got, canonical)
		}
		entry, err := embeddedEntry(alias, "amd64")
		if err != nil || entry.Alias != canonical || entry.Arch != "amd64" {
			t.Errorf("Embedded(%q, amd64) = %#v, %v", alias, entry, err)
		}
	}
	archCases := []struct {
		alias string
		image string
		arch  string
	}{
		{"el7x", "el7", "amd64"}, {"el7i", "el7", "amd64"}, {"c7x", "el7", "amd64"}, {"c7i", "el7", "amd64"},
		{"el8a", "el8", "arm64"}, {"el8x", "el8", "amd64"}, {"el8i", "el8", "amd64"},
		{"el9a", "el9", "arm64"}, {"el9x", "el9", "amd64"}, {"el9i", "el9", "amd64"},
		{"el10a", "el10", "arm64"}, {"el10x", "el10", "amd64"}, {"el10i", "el10", "amd64"},
		{"ea", "el10", "arm64"}, {"ex", "el10", "amd64"}, {"ei", "el10", "amd64"},
		{"ela", "el10", "arm64"}, {"elx", "el10", "amd64"}, {"eli", "el10", "amd64"},
		{"d12a", "d12", "arm64"}, {"d12x", "d12", "amd64"}, {"d12i", "d12", "amd64"},
		{"d13a", "d13", "arm64"}, {"d13x", "d13", "amd64"}, {"d13i", "d13", "amd64"},
		{"da", "d13", "arm64"}, {"dx", "d13", "amd64"}, {"di", "d13", "amd64"},
		{"u22a", "u22", "arm64"}, {"u22x", "u22", "amd64"}, {"u22i", "u22", "amd64"},
		{"u24a", "u24", "arm64"}, {"u24x", "u24", "amd64"}, {"u24i", "u24", "amd64"},
		{"u26a", "u26", "arm64"}, {"u26x", "u26", "amd64"}, {"u26i", "u26", "amd64"},
		{"ua", "u26", "arm64"}, {"ux", "u26", "amd64"}, {"ui", "u26", "amd64"},
	}
	for _, tc := range archCases {
		gotImage, gotArch := ResolveAlias(" " + strings.ToUpper(tc.alias) + " ")
		if gotImage != tc.image || gotArch != tc.arch {
			t.Errorf("ResolveAlias(%q) = (%q, %q), want (%q, %q)", tc.alias, gotImage, gotArch, tc.image, tc.arch)
		}
		entry, err := embeddedEntry(tc.alias, tc.arch)
		if err != nil || entry.Alias != tc.image || entry.Arch != tc.arch {
			t.Errorf("Embedded(%q, %s) = %#v, %v", tc.alias, tc.arch, entry, err)
		}
		otherArch := "amd64"
		if tc.arch == "amd64" {
			otherArch = "arm64"
		}
		if _, err := embeddedEntry(tc.alias, otherArch); err == nil || !strings.Contains(err.Error(), "conflicts with requested architecture") {
			t.Errorf("Embedded(%q, %s) expected arch conflict, got %v", tc.alias, otherArch, err)
		}
	}
	if _, err := embeddedEntry("unknown", "amd64"); err == nil {
		t.Fatal("unknown alias unexpectedly resolved")
	}
	if _, err := embeddedEntry("el7a", "arm64"); err == nil {
		t.Fatal("el7a unexpectedly resolved")
	}
	if _, err := embeddedEntry("el7", "arm64"); err == nil {
		t.Fatal("EL7 unexpectedly has an arm64 artifact")
	}
	if _, err := embeddedEntry("u24", "s390x"); err == nil {
		t.Fatal("unsupported architecture unexpectedly resolved")
	}
}

func TestFutureCatalogCanAdvanceFloatingFamilyAliases(t *testing.T) {
	t.Parallel()
	catalog := EmbeddedCatalog()
	u26 := catalog.Images["u26"]
	u28 := CatalogImage{
		Aliases:  []string{"ubuntu", "u"},
		Boot:     u26.Boot,
		Channels: map[string]string{"stable": "20280420.0.1"},
		Versions: map[string]CatalogVersion{
			"20280420.0.1": u26.Versions[u26.Channels["stable"]],
		},
	}
	u24 := catalog.Images["u24"]
	u24.Aliases = []string{"ubuntu24", "ubuntu2404", "noble"}
	catalog.Images["u24"] = u24
	catalog.Images["u28"] = u28
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query string
		arch  string
	}{
		{"u", "arm64"},
		{"ubuntu", "amd64"},
		{"ua", "arm64"},
		{"ux", "amd64"},
		{"ui", "amd64"},
	} {
		entry, err := catalog.Entry(tc.query, tc.arch)
		if err != nil || entry.Alias != "u28" || entry.Arch != tc.arch {
			t.Fatalf("future catalog %s/%s = %#v, %v; want u28/%s", tc.query, tc.arch, entry, err, tc.arch)
		}
	}
}
