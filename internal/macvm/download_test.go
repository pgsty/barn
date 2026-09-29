package macvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pgsty/farrow/internal/failure"
)

func fixtureSpec(url, body string) IPSWSpec {
	hash := sha256.Sum256([]byte(body))
	return IPSWSpec{Version: "27.0", Build: "26A428", URL: url, SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(hash[:])}
}

func seedPartial(t *testing.T, s *Store, spec IPSWSpec, body, etag string) string {
	t.Helper()
	if _, err := s.mkdir("images", "ipsw"); err != nil {
		t.Fatal(err)
	}
	_, partial, err := s.installerPaths(spec.Build)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(partial+".json", &partialDownload{SchemaVersion: imageSchemaVersion, URL: spec.URL, SizeBytes: spec.SizeBytes, ETag: etag}); err != nil {
		t.Fatal(err)
	}
	return partial
}

func assertInstaller(t *testing.T, installer *Installer, err error, body string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if installer.SizeBytes != int64(len(body)) || !validSHA256(installer.SHA256) {
		t.Fatalf("installer=%+v", installer)
	}
	data, err := os.ReadFile(installer.Path)
	if err != nil || string(data) != body {
		t.Fatalf("cache data=%q err=%v", data, err)
	}
}

func TestDownloadResumesInterruptedResponse(t *testing.T) {
	body := "abcdefghijklmnopqrstuvwxyz"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		w.Header().Set("ETag", `"same"`)
		if call == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, body[:7])
			return
		}
		if r.Header.Get("Range") != "bytes=7-" || r.Header.Get("If-Range") != `"same"` {
			t.Errorf("resume headers=%v", r.Header)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)-7))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 7-%d/%d", len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = fmt.Fprint(w, body[7:])
	}))
	defer server.Close()
	s := testStore(t)
	spec := fixtureSpec(server.URL, body)
	options := DownloadOptions{AllowHTTP: true}
	if _, err := s.DownloadIPSW(context.Background(), spec, options); err == nil {
		t.Fatal("short response marked ready")
	}
	installer, err := s.DownloadIPSW(context.Background(), spec, options)
	assertInstaller(t, installer, err, body)
	if calls.Load() != 2 {
		t.Fatalf("requests=%d", calls.Load())
	}
	if _, err := os.Stat(installer.Path + ".partial"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial remains: %v", err)
	}
}

func TestDownloadIgnoredRangeTruncatesInsteadOfAppending(t *testing.T) {
	body := "new complete representation"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			t.Error("test expected resume request")
		}
		w.Header().Set("ETag", `"new"`)
		_, _ = fmt.Fprint(w, body)
	}))
	defer server.Close()
	s := testStore(t)
	spec := fixtureSpec(server.URL, body)
	seedPartial(t, s, spec, "old stale", `"old"`)
	installer, err := s.DownloadIPSW(context.Background(), spec, DownloadOptions{AllowHTTP: true})
	assertInstaller(t, installer, err, body)
}

func TestDownloadChangedETagAnd416RestartSafely(t *testing.T) {
	for _, status := range []int{http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			body := "abcdefghijklmnop"
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("ETag", `"changed"`)
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 4-%d/%d", len(body)-1, len(body)))
					w.WriteHeader(status)
					if status == http.StatusPartialContent {
						_, _ = fmt.Fprint(w, body[4:])
					}
					return
				}
				if r.Header.Get("Range") != "" {
					t.Errorf("restart retained Range: %s", r.Header.Get("Range"))
				}
				_, _ = fmt.Fprint(w, body)
			}))
			defer server.Close()
			s := testStore(t)
			spec := fixtureSpec(server.URL, body)
			seedPartial(t, s, spec, body[:4], `"original"`)
			installer, err := s.DownloadIPSW(context.Background(), spec, DownloadOptions{AllowHTTP: true})
			assertInstaller(t, installer, err, body)
			if calls.Load() != 2 {
				t.Fatalf("requests=%d", calls.Load())
			}
		})
	}
}

func TestDownloadRejectsWrongRangeWithoutMutatingPartial(t *testing.T) {
	body := "abcdefghijklmno"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"same"`)
		w.Header().Set("Content-Range", "bytes 2-14/15")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = fmt.Fprint(w, body[2:])
	}))
	defer server.Close()
	s := testStore(t)
	spec := fixtureSpec(server.URL, body)
	partial := seedPartial(t, s, spec, body[:4], `"same"`)
	if _, err := s.DownloadIPSW(context.Background(), spec, DownloadOptions{AllowHTTP: true}); err == nil {
		t.Fatal("accepted wrong byte offset")
	}
	if data, err := os.ReadFile(partial); err != nil || string(data) != body[:4] {
		t.Fatalf("partial mutated: %q %v", data, err)
	}
}

func TestDownloadCancellationRetainsResumableBytes(t *testing.T) {
	body := strings.Repeat("abcd", 1<<18)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("ETag", `"same"`)
		offset := 0
		if value := r.Header.Get("Range"); value != "" {
			_, _ = fmt.Sscanf(value, "bytes=%d-", &offset)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(body)-1, len(body)))
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = fmt.Fprint(w, body[offset:])
	}))
	defer server.Close()
	s := testStore(t)
	spec := fixtureSpec(server.URL, body)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := s.DownloadIPSW(ctx, spec, DownloadOptions{AllowHTTP: true, Progress: func(done, total int64) {
		if done > 0 {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	_, partial, _ := s.installerPaths(spec.Build)
	info, err := os.Stat(partial)
	if err != nil || info.Size() <= 0 {
		t.Fatalf("cancelled bytes not retained: %v", err)
	}
	installer, err := s.DownloadIPSW(context.Background(), spec, DownloadOptions{AllowHTTP: true})
	assertInstaller(t, installer, err, body)
	if calls.Load() > 2 {
		t.Fatalf("extra requests=%d", calls.Load())
	}
}

func TestDownloaderCacheIntegrityAndConcurrentReuse(t *testing.T) {
	body := strings.Repeat("cache", 100)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = fmt.Fprint(w, body) }))
	defer server.Close()
	s := testStore(t)
	spec := fixtureSpec(server.URL, body)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			installer, err := s.DownloadIPSW(context.Background(), spec, DownloadOptions{AllowHTTP: true})
			if err != nil {
				t.Error(err)
			} else if installer.SHA256 != spec.SHA256 {
				t.Error("digest mismatch")
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("cache downloaded %d times", calls.Load())
	}
	final, _, _ := s.installerPaths(spec.Build)
	if err := os.WriteFile(final, []byte(strings.Repeat("x", len(body))), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.DownloadIPSW(context.Background(), spec, DownloadOptions{AllowHTTP: true})
	if class, _, next := failure.Classify(err); class != failure.Integrity || !strings.Contains(next, "prune --installers") {
		t.Fatalf("cache corruption=%v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("silently overwrote corrupt cache")
	}
}

func TestCachePublishCrashRecoveryRequiresPinnedDigest(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(strconv.FormatBool(pinned), func(t *testing.T) {
			body := "complete before metadata"
			s := testStore(t)
			if _, err := s.mkdir("images", "ipsw"); err != nil {
				t.Fatal(err)
			}
			spec := fixtureSpec("https://updates.cdn-apple.com/fixture.ipsw", body)
			if !pinned {
				spec.SHA256 = ""
			}
			final, _, _ := s.installerPaths(spec.Build)
			if err := os.WriteFile(final, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			installer, err := s.DownloadIPSW(context.Background(), spec, DownloadOptions{})
			if pinned {
				assertInstaller(t, installer, err, body)
			} else if err == nil || !strings.Contains(err.Error(), "metadata") {
				t.Fatalf("unpinned cache accepted or redownloaded: %v", err)
			}
		})
	}
}

func TestImportPreservesSourceAndChecksDigest(t *testing.T) {
	body := "local IPSW validated by Apple metadata API"
	s := testStore(t)
	spec := fixtureSpec("", body)
	spec.Version = "27.1"
	spec.Build = "26B100"
	spec.SHA256 = ""
	source := filepath.Join(t.TempDir(), "local.ipsw")
	if err := os.WriteFile(source, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	installer, err := s.ImportIPSW(context.Background(), source, spec, DownloadOptions{})
	assertInstaller(t, installer, err, body)
	if data, err := os.ReadFile(source); err != nil || string(data) != body {
		t.Fatalf("source mutated: %q %v", data, err)
	}
	spec.Build = "26B101"
	spec.SHA256 = strings.Repeat("f", 64)
	if _, err := s.ImportIPSW(context.Background(), source, spec, DownloadOptions{}); err == nil {
		t.Fatal("accepted wrong pinned digest")
	}
}

func TestVerifiedInstallerCacheSurvivesSourceURLChange(t *testing.T) {
	body := "same official build from local import and Apple download"
	s := testStore(t)
	source := filepath.Join(t.TempDir(), "local.ipsw")
	if err := os.WriteFile(source, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := fixtureSpec("", body)
	spec.Version, spec.Build = "27.1", "26B100"
	imported, err := s.ImportIPSW(context.Background(), source, spec, DownloadOptions{})
	assertInstaller(t, imported, err, body)
	spec.URL = "https://updates.cdn-apple.com/never-download-cached-fixture.ipsw"
	requests := 0
	options := DownloadOptions{Client: &http.Client{Transport: fixtureRoundTripper(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("network is disabled for this cache reuse test")
	})}}
	reused, err := s.DownloadIPSW(context.Background(), spec, options)
	assertInstaller(t, reused, err, body)
	if reused.Path != imported.Path {
		t.Fatal("source URL change created another cache")
	}
	spec.SHA256 = strings.Repeat("f", 64)
	if _, err := s.DownloadIPSW(context.Background(), spec, options); err == nil {
		t.Fatal("source URL independence bypassed expected digest verification")
	}
	if requests != 0 {
		t.Fatalf("cache verification attempted %d network requests", requests)
	}
}

type fixtureRoundTripper func(*http.Request) (*http.Response, error)

func (f fixtureRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDownloadRejectsHTTPUnlessFixtureOptIn(t *testing.T) {
	s := testStore(t)
	if _, err := s.DownloadIPSW(context.Background(), fixtureSpec("http://example.invalid/a", "test"), DownloadOptions{}); err == nil {
		t.Fatal("accepted plaintext network download")
	}
	if _, err := os.Stat(s.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid spec mutated state: %v", err)
	}
}

func TestProductDownloadsStayOnAppleHosts(t *testing.T) {
	for _, address := range []string{"https://example.com/a.ipsw", "https://apple.com.attacker.invalid/a.ipsw", "https://evilapple.com/a.ipsw"} {
		if err := fixtureSpec(address, "body").validate(false); err == nil {
			t.Errorf("accepted non-Apple URL %s", address)
		}
	}
	if err := DefaultIPSW().validate(false); err != nil {
		t.Fatal(err)
	}
}
