package image

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRepositoryBuildThroughSharedDirectoryAliases(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(parent)
	t.Setenv("HOME", parent)
	root, images := filepath.Join(parent, "repo"), filepath.Join(parent, "artifacts")
	for _, directory := range []string{root, images} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o775); err != nil {
			t.Fatal(err)
		}
	}
	writeRepoFixture(t, parent, 1, "debian13")
	for link, target := range map[string]string{
		filepath.Join(root, RepoFilename):  filepath.Join(parent, RepoFilename),
		filepath.Join(root, RepoImagesDir): images,
		filepath.Join(parent, "linked"):    root,
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(images, "d13-1-arm64.qcow2"), []byte("fixture-qcow"), 0o600); err != nil {
		t.Fatal(err)
	}
	builder := RepoBuilder{QEMUImg: "/fake/qemu-img", Runner: repoRunner{}}
	if _, err := ScanRepository("./linked"); err != nil {
		t.Fatal(err)
	}
	result, err := builder.Build(context.Background(), "~/linked")
	if err != nil || result.Path != filepath.Join(root, CatalogFilename) {
		t.Fatalf("linked repository build = %#v, %v", result, err)
	}
	if _, err := builder.Verify(context.Background(), "./linked"); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, CatalogFilename)
	saved := filepath.Join(parent, "saved-catalog.json")
	if err := os.Rename(output, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(saved, output); err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Build(context.Background(), "./linked"); err == nil {
		t.Fatal("catalog output symlink was followed for writing")
	}
}

func TestCatalogAcceptsLargeDevelopmentImages(t *testing.T) {
	catalog := EmbeddedCatalog()
	for imageName, image := range catalog.Images {
		for versionName, version := range image.Versions {
			for arch, variant := range version.Variants {
				variant.ArtifactSize = 12 << 30
				variant.VirtualSize = 64 << 30
				version.Variants[arch] = variant
			}
			image.Versions[versionName] = version
		}
		catalog.Images[imageName] = image
	}
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, entry := range catalog.Entries() {
		if err := validateEntryIdentity(entry); err != nil {
			t.Fatal(err)
		}
	}
}

type developmentTransport func(*http.Request) (*http.Response, error)

func (f developmentTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDownloadStopsAtDeclaredSize(t *testing.T) {
	for _, size := range []int64{64, 12 << 30} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			body := strings.NewReader(strings.Repeat("x", 4096))
			length := int64(-1)
			if size > 8<<30 {
				length = size
			}
			store := Store{HTTPClient: &http.Client{Transport: developmentTransport(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, ContentLength: length, Body: io.NopCloser(body), Request: request}, nil
			})}}
			entry := Entry{SHA256: strings.Repeat("a", 64), ArtifactSize: size}
			_, written, err := store.stageHTTP(context.Background(), "https://repo.example/image.qcow2", t.TempDir(), entry)
			if err == nil || !strings.Contains(err.Error(), "size differs") {
				t.Fatalf("wrong length accepted: %d, %v", written, err)
			}
			wantRead := min(size+1, 4096)
			if written != wantRead || int64(4096-body.Len()) != wantRead {
				t.Fatalf("read %d bytes, wrote %d, want %d", 4096-body.Len(), written, wantRead)
			}
		})
	}
}
