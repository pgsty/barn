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
			release: "20260923.2610.1", url: "",
			sha256: "00d96945c7b7b0f86d5f919658b0fe32829146851ce5f752f2fc50e907e894bc", artifactSize: 763166720, virtualSize: 3221225472, sourceUser: "dba",
		},
		"d12/arm64": {
			release: "20260923.2610.1", url: "",
			sha256: "b7a91965bd9612d51c69396c2cb03b230707c69982b8286327844ce59f6cea00", artifactSize: 724500480, virtualSize: 3221225472, sourceUser: "dba",
		},
		"d13/amd64": {
			release: "20260914.2601.2", url: "",
			sha256: "d7d9535bebb4e3052e67bf280142e55aabf8d62e4e2f42281a12532ac79374c2", artifactSize: 589430784, virtualSize: 3221225472, sourceUser: "dba",
		},
		"d13/arm64": {
			release: "20260914.2601.2", url: "",
			sha256: "400a1d2eb921cad7ecd7240762cafa03003771f8ec99a58826528512703d5079", artifactSize: 604962816, virtualSize: 3221225472, sourceUser: "dba",
		},
		"u22/amd64": {
			release: "20260926.0.0", url: "https://cloud-images.ubuntu.com/releases/jammy/release-20260926/ubuntu-22.04-server-cloudimg-amd64.img",
			sha256: "0c9811a81e6329acacbb5ae4e701a7650f85cd3f19852cd159d79ab8357b210e", artifactSize: 735731200, virtualSize: 2361393152, sourceUser: "ubuntu",
		},
		"u22/arm64": {
			release: "20260926.0.0", url: "https://cloud-images.ubuntu.com/releases/jammy/release-20260926/ubuntu-22.04-server-cloudimg-arm64.img",
			sha256: "04f2ca6af841918df1fa1aaba485f2b5f3d451f90459e48c066bbd70ebed174f", artifactSize: 705299456, virtualSize: 2361393152, sourceUser: "ubuntu",
		},
		"u24/amd64": {
			release: "20260926.0.0", url: "https://cloud-images.ubuntu.com/releases/noble/release-20260926/ubuntu-24.04-server-cloudimg-amd64.img",
			sha256: "6a81c37564db9b1ee84e141922625e1d7c5b389b99bb3c572e0243607d5bb4d2", artifactSize: 625612288, virtualSize: 3758096384, sourceUser: "ubuntu",
		},
		"u24/arm64": {
			release: "20260926.0.0", url: "https://cloud-images.ubuntu.com/releases/noble/release-20260926/ubuntu-24.04-server-cloudimg-arm64.img",
			sha256: "1d6bffe64b848468ac97f821d369a4846d983de1800ccf6b5ec8853e85cefc55", artifactSize: 620224512, virtualSize: 3758096384, sourceUser: "ubuntu",
		},
		"u26/amd64": {
			release: "20260927.0.0", url: "https://cloud-images.ubuntu.com/releases/resolute/release-20260927/ubuntu-26.04-server-cloudimg-amd64.img",
			sha256: "8800651811af9a85465ad1d552add729947bb16488dddb4a9b5305a3d97332b2", artifactSize: 865115136, virtualSize: 3758096384, sourceUser: "ubuntu",
		},
		"u26/arm64": {
			release: "20260927.0.0", url: "https://cloud-images.ubuntu.com/releases/resolute/release-20260927/ubuntu-26.04-server-cloudimg-arm64.img",
			sha256: "63a93bd5a8d76e33b15ceb5daa3657bd79be804748051ab178e643b0f5da22e7", artifactSize: 945530880, virtualSize: 3758096384, sourceUser: "ubuntu",
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
		if entry.Alias == "el7" {
			wantBoot, wantStatus = "bios", "deprecated"
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
		"rocky9": "el9", "rocky": "el9", "rocky10": "el10",
		"debian12": "d12", "bookworm": "d12", "debian13": "d13", "debian": "d13", "trixie": "d13",
		"ubuntu22": "u22", "ubuntu2204": "u22", "jammy": "u22",
		"ubuntu": "u24", "ubuntu24": "u24", "ubuntu2404": "u24", "noble": "u24",
		"ubuntu26": "u26", "ubuntu2604": "u26", "resolute": "u26",
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
	if _, err := embeddedEntry("unknown", "amd64"); err == nil {
		t.Fatal("unknown alias unexpectedly resolved")
	}
	if _, err := embeddedEntry("el7", "arm64"); err == nil {
		t.Fatal("EL7 unexpectedly has an arm64 artifact")
	}
	if _, err := embeddedEntry("u24", "s390x"); err == nil {
		t.Fatal("unsupported architecture unexpectedly resolved")
	}
}
