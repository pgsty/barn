package hostconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCompanionHelperDigestRequiresInjectedPair(t *testing.T) {
	directory := t.TempDir()
	helper := filepath.Join(directory, "barn-hosts-helper")
	data := []byte("companion helper fixture")
	if err := os.WriteFile(helper, data, 0o755); err != nil {
		t.Fatal(err)
	}
	previous := ExpectedHelperSHA256
	t.Cleanup(func() { ExpectedHelperSHA256 = previous })
	ExpectedHelperSHA256 = ""
	if _, err := CompanionHelperDigest(helper); err == nil {
		t.Fatal("unpaired source build accepted a privileged companion")
	}
	ExpectedHelperSHA256 = digest(data)
	actual, err := CompanionHelperDigest(helper)
	if err != nil {
		t.Fatal(err)
	}
	if actual != ExpectedHelperSHA256 {
		t.Fatalf("digest = %s, want %s", actual, ExpectedHelperSHA256)
	}
	ExpectedHelperSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := CompanionHelperDigest(helper); err == nil {
		t.Fatal("mismatched companion accepted")
	}
}
