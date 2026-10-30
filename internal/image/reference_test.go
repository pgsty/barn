package image

import "testing"

func TestParseImageReferenceGrammar(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value string
		want  Reference
	}{
		{"", Reference{}},
		{"d13", Reference{Image: "d13"}},
		{" D13:Testing ", Reference{Image: "d13", Channel: "testing"}},
		{"d13@20260914.2601.2", Reference{Image: "d13", Version: "20260914.2601.2"}},
	}
	for _, test := range tests {
		got, err := ParseReference(test.value)
		if err != nil || got != test.want {
			t.Errorf("ParseReference(%q) = %#v, %v; want %#v", test.value, got, err, test.want)
		}
	}
	for _, value := range []string{"d13:stable@1", "d13:stable:next", "d13/path", "d13@", ":stable"} {
		if _, err := ParseReference(value); err == nil {
			t.Errorf("invalid reference %q accepted", value)
		}
	}
}

func TestCatalogResolvesDefaultChannelExactVersionAndAlias(t *testing.T) {
	t.Parallel()
	catalog := EmbeddedCatalog()
	defaultEntry, err := catalog.Entry("", "arm64")
	if err != nil || defaultEntry.Alias != "u24" || defaultEntry.Channel != "stable" || defaultEntry.Release != "20261017.0.1" {
		t.Fatalf("default entry = %#v, %v", defaultEntry, err)
	}
	exact, err := catalog.Entry("ubuntu2404@20261017.0.1", "arm64")
	if err != nil || exact.Alias != "u24" || exact.Channel != "" || exact.SHA256 != defaultEntry.SHA256 {
		t.Fatalf("exact entry = %#v, %v", exact, err)
	}
	if got, err := CanonicalReference("Ubuntu:stable"); err != nil || got != "u24:stable" {
		t.Fatalf("canonical reference = %q, %v", got, err)
	}
	for _, test := range []struct {
		reference string
		arch      string
		release   string
	}{
		{"el10@10.2.20260525.0", "amd64", "10.2.20260525.0"},
		{"rocky@9.8.20260525.2", "arm64", "9.8.20260525.2"},
		{"d12@20261026.2643.1", "amd64", "20261026.2643.1"},
		{"ubuntu22@20261020.0.1", "arm64", "20261020.0.1"},
		{"ubuntu26@20261016.0.1", "amd64", "20261016.0.1"},
	} {
		entry, err := catalog.Entry(test.reference, test.arch)
		if err != nil || entry.Release != test.release || entry.Channel != "" {
			t.Errorf("exact entry %s/%s = %#v, %v", test.reference, test.arch, entry, err)
		}
	}
	for _, test := range []struct {
		reference string
		arch      string
		release   string
	}{
		{"el9@9", "arm64", "9.8.20260525.2"},
		{"el10@10", "amd64", "10.2.20260525.0"},
		{"u24@20261017", "arm64", "20261017.0.1"},
	} {
		entry, err := catalog.Entry(test.reference, test.arch)
		if err != nil || entry.Release != test.release || entry.Channel != "" {
			t.Errorf("prefix entry %s/%s = %#v, %v", test.reference, test.arch, entry, err)
		}
	}
}

func TestNumericVersionPrefixSelectsSemanticLatestOnComponentBoundary(t *testing.T) {
	versions := map[string]CatalogVersion{
		"9.7.20251123.1": {},
		"9.7.20251123.2": {},
		"9.8.20260525.0": {},
		"9.10.2.0":       {},
	}
	for selector, want := range map[string]string{
		"9.7.20251123.1": "9.7.20251123.1",
		"9.7":            "9.7.20251123.2",
		"9":              "9.10.2.0",
	} {
		got, err := resolveVersion(versions, selector)
		if err != nil || got != want {
			t.Errorf("resolveVersion(%q) = %q, %v; want %q", selector, got, err, want)
		}
	}
	if got, err := resolveVersion(map[string]CatalogVersion{"9.7.1": {}, "9.70.1": {}}, "9.7"); err != nil || got != "9.7.1" {
		t.Fatalf("component-boundary resolution = %q, %v", got, err)
	}
	for _, selector := range []string{"8", "9.x"} {
		if _, err := resolveVersion(versions, selector); err == nil {
			t.Errorf("invalid/unmatched selector %q was accepted", selector)
		}
	}
}

func TestCurrentCatalogPinnedImageIdentity(t *testing.T) {
	t.Parallel()
	catalog := EmbeddedCatalog()
	for _, test := range []struct{ reference, arch, sha256 string }{
		{"el7@7.9.20221112.0", "amd64", "284aab2b23d91318f169ff464bce4d53404a15a0618ceb34562838c59af4adea"},
		{"el8@8.10.20240528.3", "amd64", "32beda4ee88880fd989d2887cb2ed34ac39a1423fc85c8591a5cbd850fe0cc71"},
		{"el8@8.10.20240528.3", "arm64", "9dc4c04a521c0aa7072a12ff523403d167d79302c1b5e1b28ceab8b3e9b55de6"},
		{"el9@9.8.20260525.2", "amd64", "97fcb5f117e364a6c26a3536adc2a8f517acabeaf2a0ece30330f16753e7b6dd"},
		{"el9@9.8.20260525.2", "arm64", "28628f3ee93d35b0c21b2e53d568817380f9764ae4b89f0eeb0bde9d503c313b"},
		{"el10@10.2.20260525.0", "amd64", "9fc9e9ff16888bb68ac39b0392e25c9c92684d50c85f1cce6ab549363bbc4b48"},
		{"el10@10.2.20260525.0", "arm64", "457c8375e19496f43a25c4a6169fa11237536c53cef6f85a20ea3c5a751aa0f5"},
		{"d12@20261026.2643.1", "amd64", "57b86ed2748fe41126ce9bdb48af5f289d9a10fb69e1bee4fb363c1649164dad"},
		{"d12@20261026.2643.1", "arm64", "73bf46a766747d7647263c917065ae218f0334e60a78b92c64bc950cbf5cab76"},
		{"d13@20261026.2643.1", "amd64", "2ebcddcc3f2b5ad52f09d021c06b5fc23d984b78c5e9e2c1c94c37891fc0db48"},
		{"d13@20261026.2643.1", "arm64", "c3c2fb7b4ae5218f5c02423c0ca67ddd5ada65e9829f50e7f8f50d86aa6a5dba"},
		{"u22@20261020.0.1", "amd64", "c321f1fee530ceabf505d4b7b06398527d7ace4acc5e8a0ce73f35d4e56511f1"},
		{"u22@20261020.0.1", "arm64", "501de7321ba8b9e63dd930e6d2137b67d52ac722257c56c361979d9ba2946393"},
		{"u24@20261017.0.1", "amd64", "84c5d706bd6077cebd2738fcdc67bdc9e41e7f2c37f62b3b578f0c9448308937"},
		{"u24@20261017.0.1", "arm64", "128cb37a18174c14f1976d752ce27cf9d284802efcb1fc0e730f0abb456cb06f"},
		{"u26@20261016.0.1", "amd64", "87982ec7e107dcd00367aaa5d15a1a08f8d2babb7e58cc3142fd168725b73c10"},
		{"u26@20261016.0.1", "arm64", "ac9634f0ff07d16531868200cef46cda087f3088d4433a5c6510d3e238b3f00f"},
	} {
		entry, err := catalog.Entry(test.reference, test.arch)
		if err != nil || entry.SHA256 != test.sha256 || entry.Channel != "" {
			t.Errorf("pinned image %s/%s = %#v, %v", test.reference, test.arch, entry, err)
		}
	}
}
