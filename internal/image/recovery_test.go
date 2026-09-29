package image

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/state"
)

func TestCachePermissionRecoveryVerifiesBytes(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	artifact := testHTTPArtifact(t)
	store.Repository = "https://unreachable.invalid"
	entry := artifact.entry("https://unreachable.invalid/image.qcow2")
	path, err := store.Path(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, artifact.data, 0644); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.Pull(context.Background(), entry)
	if err != nil || got != path {
		t.Fatalf("valid writable cache was not repaired offline: %s %v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0444 {
		t.Fatalf("cache mode=%v err=%v", info, err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("damaged"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.repairCachePermissions(context.Background(), entry); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("damaged bytes accepted: %v", err)
	}
}

func TestDamagedUnreferencedCacheIsPreservedAndFetchedAgain(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	artifact := testHTTPArtifact(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "image.qcow2", time.Time{}, strings.NewReader(string(artifact.data)))
	}))
	defer server.Close()
	store.Repository = server.URL
	store.HTTPClient = server.Client()
	entry := artifact.entry(server.URL + "/image.qcow2")
	path, err := store.Path(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("damaged"), 0444); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Pull(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("damaged bytes lost: %v %v", backups, err)
	}
	data, err := os.ReadFile(backups[0])
	if err != nil || string(data) != "damaged" {
		t.Fatalf("backup=%q %v", data, err)
	}
	if _, _, err := store.ValidateCached(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
}

func TestCacheRecoveryPreservesReferencedBackingEvenAfterCatalogDigestChange(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	artifact := testHTTPArtifact(t)
	store.Repository = "https://unreachable.invalid"
	entry := artifact.entry("https://unreachable.invalid/image.qcow2")
	path, err := store.Path(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("damaged"), 0444); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	node := state.NodeState{Schema: state.NodeSchema, BarnVersion: "test", Node: "meta", VMUUID: "instance", Phase: state.Running, Generation: 1, SpecHash: "fixture", CreatedAt: now, UpdatedAt: now, Image: state.Image{Alias: entry.Alias, Release: entry.Release, Digest: strings.Repeat("a", 64)}}
	if err := (state.Store{Root: store.DataRoot}).WriteNode(node); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store.DataRoot, "locks"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.quarantineCache(context.Background(), entry); err == nil || !strings.Contains(err.Error(), "still referenced") {
		t.Fatalf("referenced backing moved: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "damaged" {
		t.Fatalf("backing changed: %q %v", data, err)
	}
}

func TestCancellationDuringRetryPreservesResumePoint(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	artifact := testHTTPArtifact(t)
	var attempts atomic.Int64
	var ranged atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(artifact.data)))
			_, _ = w.Write(artifact.data[:len(artifact.data)/2])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		ranged.Store(r.Header.Get("Range") != "")
		http.ServeContent(w, r, "image.qcow2", time.Time{}, strings.NewReader(string(artifact.data)))
	}))
	defer server.Close()
	store.Repository = server.URL
	store.HTTPClient = server.Client()
	entry := artifact.entry(server.URL + "/image.qcow2")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.Progress = func(event activity.Event) {
		if event.Phase == "image-retry" {
			cancel()
		}
	}
	if _, _, err := store.Pull(ctx, entry); !errors.Is(err, context.Canceled) {
		t.Fatalf("retry cancellation=%v", err)
	}
	staged := resumePartialPath(filepath.Join(store.DataRoot, "images", entry.Alias), entry)
	if info, err := os.Stat(staged); err != nil || info.Size() == 0 {
		t.Fatalf("resume point missing: %v %v", info, err)
	}
	store.Progress = nil
	if _, _, err := store.Pull(context.Background(), entry); err != nil || !ranged.Load() {
		t.Fatalf("repeat command did not resume: ranged=%t err=%v", ranged.Load(), err)
	}
}

func TestRetryPolicyRespectsServerCooldownAndCustomSources(t *testing.T) {
	if retrySource(&sourceHTTPError{Status: 401}) || retrySource(ErrIntegrity) {
		t.Fatal("permanent failure retried")
	}
	if !retrySource(&sourceHTTPError{Status: 503}) || retryDelay(&sourceHTTPError{Status: 429, After: "3600"}, 0) <= 30*time.Second {
		t.Fatal("server cooldown was ignored")
	}
	if officialFallback("https://private.example/images") != "" || officialFallback(DefaultRepositoryURL) != MirrorRepositoryURL {
		t.Fatal("unexpected fallback source")
	}
}

func TestOfficialFailoverRetriesAndStillRejectsWrongMirrorBytes(t *testing.T) {
	// Official endpoint variables are changed only in this non-parallel test.
	previousDefault, previousMirror := DefaultRepositoryURL, MirrorRepositoryURL
	t.Cleanup(func() { DefaultRepositoryURL, MirrorRepositoryURL = previousDefault, previousMirror })
	artifact := testHTTPArtifact(t)
	for _, damaged := range []bool{false, true} {
		store := testImageStore(t)
		var primaryCalls, mirrorCalls atomic.Int64
		primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			primaryCalls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(primary.Close)
		payload := append([]byte(nil), artifact.data...)
		if damaged {
			payload[len(payload)-1] ^= 1
		}
		mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { mirrorCalls.Add(1); _, _ = w.Write(payload) }))
		t.Cleanup(mirror.Close)
		DefaultRepositoryURL, MirrorRepositoryURL = primary.URL, mirror.URL
		store.Repository = primary.URL
		entry := artifact.entry("https://provenance.invalid/never-fetch")
		_, _, err := store.Pull(context.Background(), entry)
		if (err != nil) != damaged || primaryCalls.Load() != 3 || mirrorCalls.Load() != 1 {
			t.Fatalf("damaged=%t primary=%d mirror=%d err=%v", damaged, primaryCalls.Load(), mirrorCalls.Load(), err)
		}
		if damaged {
			path, pathErr := store.Path(entry)
			if pathErr != nil {
				t.Fatal(pathErr)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unverified mirror was published: %v", err)
			}
		}
	}
}
