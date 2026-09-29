package setup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	darwinnet "github.com/pgsty/barn/internal/network/darwin"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (function doerFunc) Do(request *http.Request) (*http.Response, error) { return function(request) }

func TestDownloadSocketVMNetReleasePublishesVerifiedCache(t *testing.T) {
	t.Parallel()
	const content = "verified archive fixture"
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Length", "24")
		_, _ = io.WriteString(response, content)
	}))
	defer server.Close()
	release := darwinnet.Release{
		Version: "test", Arch: "arm64", ArchiveName: "socket_vmnet-test.tar.gz",
		URL: server.URL + "/socket_vmnet-test.tar.gz", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	verify := func(path, arch string) error {
		if arch != "arm64" {
			return errors.New("wrong architecture")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(data) != content {
			return errors.New("wrong content")
		}
		return nil
	}
	result, err := downloadSocketVMNetRelease(context.Background(), release, "arm64", t.TempDir(), server.Client(), Sources{}, verify)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Downloaded || result.Path == "" {
		t.Fatalf("download result = %#v", result)
	}
	second, err := downloadSocketVMNetRelease(context.Background(), release, "arm64", filepath.Dir(result.Path), doerFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("cache reuse attempted a request")
	}), Sources{}, verify)
	if err != nil {
		t.Fatal(err)
	}
	if second.Downloaded || second.Path != result.Path {
		t.Fatalf("cached result = %#v", second)
	}
}

func TestDownloadSocketVMNetReleaseRejectsPlainHTTP(t *testing.T) {
	t.Parallel()
	release := darwinnet.Release{
		Version: "test", Arch: "arm64", ArchiveName: "fixture.tar.gz",
		URL: "http://example.invalid/fixture.tar.gz", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	_, err := downloadSocketVMNetRelease(context.Background(), release, "arm64", t.TempDir(), nil, Sources{}, func(string, string) error { return nil })
	if err == nil {
		t.Fatal("plain HTTP release accepted")
	}
}

func TestDownloadSocketVMNetReleaseHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.TODO())
	started := make(chan struct{})
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	release := sourcesFixtureRelease("https://example.invalid/socket_vmnet-test.tar.gz")
	cacheDirectory := t.TempDir()
	result := make(chan error, 1)
	go func() {
		_, err := downloadSocketVMNetRelease(ctx, release, "arm64", cacheDirectory, client, Sources{}, func(string, string) error { return errors.New("not cached") })
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("download request did not start")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("download cancellation error = %v", err)
	}
}

func sourcesFixtureRelease(url string) darwinnet.Release {
	return darwinnet.Release{
		Version: "test", Arch: "arm64", ArchiveName: "socket_vmnet-test.tar.gz",
		URL: url, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
}

func contentVerifier(content string) func(string, string) error {
	return func(path, arch string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(data) != content {
			return errors.New("wrong content")
		}
		return nil
	}
}

func TestDownloadSocketVMNetLocalArchiveSourceSkipsNetwork(t *testing.T) {
	t.Parallel()
	const content = "local archive fixture"
	archive := filepath.Join(t.TempDir(), "socket_vmnet-test.tar.gz")
	if err := os.WriteFile(archive, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	release := sourcesFixtureRelease("https://upstream.invalid/socket_vmnet-test.tar.gz")
	deny := doerFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("local archive source attempted a network request")
	})
	result, err := downloadSocketVMNetRelease(context.Background(), release, "arm64", t.TempDir(), deny, Sources{Archive: archive}, contentVerifier(content))
	if err != nil || !result.Downloaded || result.URL != "file://"+archive {
		t.Fatalf("archive source result = %#v, %v", result, err)
	}
	if data, readErr := os.ReadFile(result.Path); readErr != nil || string(data) != content {
		t.Fatalf("published cache = %q, %v", data, readErr)
	}

	// A mismatching explicit archive is an error, never a silent fallback.
	if err := os.WriteFile(archive, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadSocketVMNetRelease(context.Background(), release, "arm64", t.TempDir(), deny, Sources{Archive: archive}, contentVerifier(content)); err == nil {
		t.Fatal("tampered explicit archive accepted")
	}
}

func TestDownloadSocketVMNetRepositoryDirectoryMirror(t *testing.T) {
	t.Parallel()
	const content = "repository mirror fixture"
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "socket_vmnet"), 0o700); err != nil {
		t.Fatal(err)
	}
	mirrorCopy := filepath.Join(repo, "socket_vmnet", "socket_vmnet-test.tar.gz")
	if err := os.WriteFile(mirrorCopy, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	release := sourcesFixtureRelease("https://upstream.invalid/socket_vmnet-test.tar.gz")
	deny := doerFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("repository mirror attempted a network request")
	})
	result, err := downloadSocketVMNetRelease(context.Background(), release, "arm64", t.TempDir(), deny, Sources{Repo: repo}, contentVerifier(content))
	if err != nil || !result.Downloaded || result.URL != "file://"+mirrorCopy {
		t.Fatalf("repo mirror result = %#v, %v", result, err)
	}

	// A corrupt mirror copy fails closed rather than falling through upstream.
	if err := os.WriteFile(mirrorCopy, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadSocketVMNetRelease(context.Background(), release, "arm64", t.TempDir(), deny, Sources{Repo: repo}, contentVerifier(content)); err == nil {
		t.Fatal("corrupt repository mirror accepted")
	}

	// A repository without the file falls through to upstream (denied here).
	empty := t.TempDir()
	if _, err := downloadSocketVMNetRelease(context.Background(), release, "arm64", t.TempDir(), deny, Sources{Repo: empty}, contentVerifier(content)); err == nil || !strings.Contains(err.Error(), "network request") {
		t.Fatalf("empty repo fall-through error = %v", err)
	}
}
