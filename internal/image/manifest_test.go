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
			release: "9.9.20261112.1", url: "",
			sha256: "0c0b039e543cadca889407aa87abd75910d5ef7d77bd7563eb7efeb622757995", artifactSize: 659292160, virtualSize: 10737418240, sourceUser: "dba",
		},
		"el9/arm64": {
			release: "9.9.20261112.1", url: "",
			sha256: "0b8ef97d585f61d1348fefd4f4eaff3f70fb743024d9d5e083d6a4ab8e1b6bee", artifactSize: 538378240, virtualSize: 10737418240, sourceUser: "dba",
		},
		"el10/amd64": {
			release: "10.3.20261112.1", url: "",
			sha256: "bf47bd169e19ae086d466f00e50178850f62b24293dfc9ec7428cf5f080b74e7", artifactSize: 857079808, virtualSize: 10737418240, sourceUser: "dba",
		},
		"el10/arm64": {
			release: "10.3.20261112.1", url: "",
			sha256: "995e2e8cc0991dc35e7584f86d39cf9b15e083133aa59a468c3eb144f8e9e7f8", artifactSize: 690552832, virtualSize: 10737418240, sourceUser: "dba",
		},
		"d12/amd64": {
			release: "20261117.2665.1", url: "",
			sha256: "30d8067534fcefc45091824c8fb3da97ac3d7803f88fe12e79319ffaa96241e7", artifactSize: 759627776, virtualSize: 3221225472, sourceUser: "dba",
		},
		"d12/arm64": {
			release: "20261117.2665.1", url: "",
			sha256: "dceb3d3da442ccaf941fd5f4425fa31dcb6181919da70f9f0b15003f11f3a751", artifactSize: 711000064, virtualSize: 3221225472, sourceUser: "dba",
		},
		"d13/amd64": {
			release: "20261123.2671.1", url: "",
			sha256: "27f92ad31cb6119fb75c31a794697d06b784055179860043432b4884e6b420e4", artifactSize: 543883264, virtualSize: 3221225472, sourceUser: "dba",
		},
		"d13/arm64": {
			release: "20261123.2671.1", url: "",
			sha256: "fcc690489358b0a59472365488458c9df78a322c46b41d6a65afec85fe999d0c", artifactSize: 587923456, virtualSize: 3221225472, sourceUser: "dba",
		},
		"u22/amd64": {
			release: "20261109.0.1", url: "",
			sha256: "aaf993c0e192891e7e74a38394baaf4ec188e74f6d0393def5951640c4e8b192", artifactSize: 721682432, virtualSize: 2361393152, sourceUser: "dba",
		},
		"u22/arm64": {
			release: "20261109.0.1", url: "",
			sha256: "1d343b80f9a5a0dc6a0bceae9ab8f7b768f7e6faaa10a33bdb83c22b34d08019", artifactSize: 693829632, virtualSize: 2361393152, sourceUser: "dba",
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
			release: "20261108.0.1", url: "",
			sha256: "cd35b7f4ff9cd15a5adcd181cd7078b0c84d71d7cb8e249373ff99ae8c503c8e", artifactSize: 865927168, virtualSize: 3758096384, sourceUser: "dba",
		},
		"u26/arm64": {
			release: "20261108.0.1", url: "",
			sha256: "5049ade4e59f2139b489ecd76afd4aedaaa3d539caaec6300b427927416c616b", artifactSize: 947978240, virtualSize: 3758096384, sourceUser: "dba",
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
