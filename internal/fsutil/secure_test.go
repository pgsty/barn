package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStrictJSONRoundTripAndRejection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	type doc struct {
		Schema int    `json:"schema"`
		Name   string `json:"name"`
	}
	want := doc{Schema: 1, Name: "meta"}
	if err := WriteJSONAtomic(path, want, 0o600); err != nil {
		t.Fatalf("WriteJSONAtomic: %v", err)
	}
	var got doc
	if err := ReadStrictJSON(path, 1<<20, &got); err != nil {
		t.Fatalf("ReadStrictJSON: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if err := DecodeStrictJSON([]byte(`{"schema":1,"extra":true}`), &got); err == nil {
		t.Fatal("accepted unknown field")
	}
	if err := DecodeStrictJSON([]byte(`{"schema":1} {"schema":2}`), &got); err == nil {
		t.Fatal("accepted trailing JSON")
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := ReadStrictJSON(link, 1<<20, &got); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestSecureHelpers(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	priv := filepath.Join(dir, "priv")
	if err := EnsurePrivateDir(priv, 0o700); err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}
	if err := NoSymlinks(filepath.Join(priv, "child")); err != nil {
		t.Fatalf("NoSymlinks on clean path: %v", err)
	}
	link := filepath.Join(priv, "sym")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := NoSymlinks(filepath.Join(link, "nested")); err == nil {
		t.Fatal("NoSymlinks accepted symlink ancestor")
	}
	file := filepath.Join(priv, "blob")
	data := []byte("barn-data")
	if err := AtomicWrite(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, size, err := SHA256File(file, 1024)
	if err != nil || size != int64(len(data)) || digest != SHA256Bytes(data) {
		t.Fatalf("SHA256File = %q, %d, %v", digest, size, err)
	}
	if _, _, err := SHA256File(file, 2); err == nil {
		t.Fatal("SHA256File accepted oversized file")
	}
	if free, probe, err := AvailableBytes(filepath.Join(priv, "future", "file")); err != nil || free == 0 || probe == "" {
		t.Fatalf("AvailableBytes = %d, %q, %v", free, probe, err)
	}
	if got := ShellQuote("safe/path-1.yml"); got != "safe/path-1.yml" {
		t.Fatalf("ShellQuote bare = %q", got)
	}
	if got := ShellQuote("team's lab.yml"); got != `'team'\''s lab.yml'` {
		t.Fatalf("ShellQuote escaped = %q", got)
	}
	if got := ShellQuote(""); got != "''" {
		t.Fatalf("ShellQuote empty = %q", got)
	}
}
