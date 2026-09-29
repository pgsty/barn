package macvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseShare(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]Share{
		"/tmp/src":             {Name: "src", Path: "/tmp/src"},
		"code=/tmp/src":        {Name: "code", Path: "/tmp/src"},
		"/tmp/docs:ro":         {Name: "docs", Path: "/tmp/docs", ReadOnly: true},
		"docs=/tmp/docs:rw":    {Name: "docs", Path: "/tmp/docs"},
		"~/Projects":           {Name: "Projects", Path: filepath.Join(home, "Projects")},
		"/tmp/a=b/c":           {Name: "c", Path: "/tmp/a=b/c"},
		"work=/tmp/x/../src/":  {Name: "work", Path: "/tmp/src"},
		"My.Data=/tmp/my data": {Name: "My.Data", Path: "/tmp/my data"},
	}
	for spec, expected := range cases {
		share, err := ParseShare(spec)
		if err != nil || share != expected {
			t.Errorf("%q: got %+v %v, want %+v", spec, share, err, expected)
		}
	}
	for _, spec := range []string{"", ":ro", "bad name=/tmp/x", "=/tmp/x"} {
		if _, err := ParseShare(spec); err == nil {
			t.Errorf("accepted %q", spec)
		}
	}
}

func TestShareSourceMustBeARealDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := checkShareSource(Share{Name: "ok", Path: dir}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for path, message := range map[string]string{file: "not a directory", link: "symlink", filepath.Join(dir, "missing"): "does not exist"} {
		if err := checkShareSource(Share{Name: "x", Path: path}); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestMergeShares(t *testing.T) {
	current := []Share{{Name: "src", Path: "/a"}, {Name: "docs", Path: "/b", ReadOnly: true}}
	merged, err := mergeShares(current, []Share{{Name: "src", Path: "/c"}, {Name: "new", Path: "/d"}}, []string{"docs"})
	if err != nil || len(merged) != 2 || merged[0].Path != "/c" || merged[1].Name != "new" {
		t.Fatalf("merged %+v %v", merged, err)
	}
	if current[0].Path != "/a" {
		t.Fatal("merge changed its input")
	}
	if _, err := mergeShares(current, nil, []string{"missing"}); err == nil {
		t.Fatal("removing an unknown share succeeded")
	}
	var many []Share
	for i := 0; i <= maxShares; i++ {
		many = append(many, Share{Name: "s" + string(rune('a'+i)), Path: "/x"})
	}
	if _, err := mergeShares(nil, many, nil); err == nil {
		t.Fatal("too many shares accepted")
	}
}
