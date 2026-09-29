package image

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestIntegrationEmbeddedMatchesLocalCorpus independently checks the embedded
// constants against a locally retained source corpus. It is opt-in because it
// hashes all 17 in-scope artifacts and requires qemu-img; release evidence should run:
//
// BARN_IMAGE_CORPUS=/path/to/image go test ./internal/image -run LocalCorpus
func TestIntegrationEmbeddedMatchesLocalCorpus(t *testing.T) {
	root := os.Getenv("BARN_IMAGE_CORPUS")
	if root == "" {
		t.Skip("BARN_IMAGE_CORPUS is not set")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := os.Open(filepath.Join(root, "manifest.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handle.Close(); err != nil {
			t.Errorf("close image corpus manifest: %v", err)
		}
	})
	reader := csv.NewReader(handle)
	reader.Comma = '\t'
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 2 {
		t.Fatal("manifest.tsv is empty")
	}
	header := make(map[string]int)
	for index, name := range rows[0] {
		header[name] = index
	}
	for _, name := range []string{"alias", "version", "arch", "file", "bytes", "sha256", "source_url"} {
		if _, ok := header[name]; !ok {
			t.Fatalf("manifest.tsv is missing %q", name)
		}
	}
	formal := make(map[string]bool, len(formalAliases))
	for _, alias := range formalAliases {
		formal[alias] = true
	}
	checked := 0
	seen := make(map[string]bool, len(formalMatrix))
	for _, row := range rows[1:] {
		alias := row[header["alias"]]
		if !formal[alias] {
			continue
		}
		arch := row[header["arch"]]
		entry, entryErr := embeddedEntry(alias, arch)
		if entryErr != nil || entry.Release != row[header["version"]] {
			continue
		}
		key := alias + "/" + arch
		if seen[key] {
			t.Fatalf("manifest.tsv has duplicate formal row %s", key)
		}
		seen[key] = true
		t.Run(key, func(t *testing.T) {
			if entry.Release != row[header["version"]] || entry.Upstream != row[header["source_url"]] {
				t.Fatalf("embedded source identity differs from manifest.tsv: %#v", entry)
			}
			artifactSize, err := strconv.ParseInt(row[header["bytes"]], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, filepath.FromSlash(row[header["file"]]))
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("unsafe corpus artifact %s: %v", path, err)
			}
			if info.Size() != artifactSize || entry.ArtifactSize != artifactSize {
				t.Fatalf("artifact bytes: file=%d TSV=%d embedded=%d", info.Size(), artifactSize, entry.ArtifactSize)
			}
			digest, err := hashFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if digest != row[header["sha256"]] || entry.SHA256 != digest {
				t.Fatalf("artifact SHA-256: file=%s TSV=%s embedded=%s", digest, row[header["sha256"]], entry.SHA256)
			}
			output, err := exec.Command(qemuImg, "info", "--output=json", "--force-share", "-f", "qcow2", path).Output()
			if err != nil {
				t.Fatal(err)
			}
			var imageInfo map[string]any
			if err := json.Unmarshal(output, &imageInfo); err != nil {
				t.Fatal(err)
			}
			virtualSize, ok := imageInfo["virtual-size"].(float64)
			if imageInfo["format"] != "qcow2" || !ok || int64(virtualSize) != entry.VirtualSize {
				t.Fatalf("qemu-img metadata differs from embedded entry: %s", output)
			}
			if _, ok := imageInfo["backing-filename"]; ok {
				t.Fatal("corpus artifact has a backing file")
			}
			if _, ok := imageInfo["full-backing-filename"]; ok {
				t.Fatal("corpus artifact has a full backing file")
			}
			if dirty, ok := imageInfo["dirty-flag"].(bool); ok && dirty {
				t.Fatal("corpus artifact has a dirty qcow2 header")
			}
			formatSpecific, _ := json.Marshal(imageInfo["format-specific"])
			lower := strings.ToLower(string(formatSpecific))
			if strings.Contains(lower, "data-file") || strings.Contains(lower, "encrypt") || strings.Contains(lower, `"corrupt":true`) {
				t.Fatalf("unsafe qcow2 format metadata: %s", formatSpecific)
			}
		})
		checked++
	}
	if checked != len(formalMatrix) {
		t.Fatalf("checked %d formal corpus rows, want %d", checked, len(formalMatrix))
	}
	for _, key := range formalMatrix {
		if !seen[key] {
			t.Errorf("manifest.tsv is missing formal row %s", key)
		}
	}
}

func hashFile(path string) (string, error) {
	handle, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		// The corpus hash helper reads only; io.Copy below determines whether the
		// full fixture was accepted.
		_ = handle.Close()
	}()
	hash := sha256.New()
	if _, err := io.Copy(hash, handle); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
