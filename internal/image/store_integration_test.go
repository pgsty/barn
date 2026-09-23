package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/activity"
	"github.com/pgsty/farrow/internal/execx"
	"github.com/pgsty/farrow/internal/failure"
)

func makeQCOW2(t *testing.T, path string, backing string) {
	t.Helper()
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img is not installed")
	}
	args := []string{"create", "-f", "qcow2"}
	if backing != "" {
		args = append(args, "-F", "qcow2", "-b", backing)
	}
	args = append(args, path, "64M")
	result := exec.Command(qemuImg, args...)
	if output, err := result.CombinedOutput(); err != nil {
		t.Fatalf("create qcow2: %v: %s", err, output)
	}
}

func testImageStore(t *testing.T) Store {
	t.Helper()
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img is not installed")
	}
	return Store{DataRoot: filepath.Join(t.TempDir(), "data"), QEMUImg: qemuImg, Runner: execx.OSRunner{Timeout: 10 * time.Second}}
}

func TestIntegrationImportAndValidateCache(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	source := filepath.Join(t.TempDir(), "source.qcow2")
	makeQCOW2(t, source, "")
	digest, _, err := digestFile(source)
	if err != nil {
		t.Fatal(err)
	}
	path, metadata, err := store.Import(context.Background(), source, digest)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Digest != digest || metadata.VirtualSize != 64<<20 {
		t.Fatalf("metadata = %#v", metadata)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("cached mode = %v", info.Mode())
	}
	relative, err := filepath.Rel(filepath.Join(store.DataRoot, "images"), path)
	if err != nil {
		t.Fatal(err)
	}
	entry := Entry{File: filepath.ToSlash(relative), SHA256: digest, Format: "qcow2", ArtifactSize: metadata.ArtifactSize, VirtualSize: metadata.VirtualSize}
	if _, _, err := store.ValidateCached(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationImportRejectsBacking(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	overlay := filepath.Join(dir, "overlay.qcow2")
	makeQCOW2(t, base, "")
	makeQCOW2(t, overlay, base)
	if _, _, err := store.Import(context.Background(), overlay, ""); err == nil {
		t.Fatal("backed image unexpectedly imported")
	}
}

func TestIntegrationHTTPSPull(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	source := filepath.Join(t.TempDir(), "source.qcow2")
	makeQCOW2(t, source, "")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256(data)
	digest := hex.EncodeToString(digestBytes[:])
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = writer.Write(data)
	}))
	defer server.Close()
	store.HTTPClient = server.Client()
	var eventsMu sync.Mutex
	var events []activity.Event
	store.Progress = func(event activity.Event) {
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
	}
	entry := Entry{Alias: "test", Release: "1", Arch: "arm64", File: "test/test-1-arm64.qcow2", Upstream: server.URL + "/image.qcow2", SHA256: digest, Format: "qcow2", ArtifactSize: int64(len(data)), VirtualSize: 64 << 20}
	path, _, err := store.Pull(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	var downloaded, verified, ready bool
	for _, event := range events {
		switch event.Phase {
		case "image-download":
			if event.Done && event.Source == server.URL+"/image.qcow2" && event.CurrentBytes == int64(len(data)) && event.TotalBytes == int64(len(data)) {
				downloaded = true
			}
		case "image-verify":
			verified = verified || event.Done
		case "image-ready":
			ready = ready || event.Done
		}
	}
	if !downloaded || !verified || !ready {
		t.Fatalf("progress stages download=%t verify=%t ready=%t events=%+v", downloaded, verified, ready, events)
	}
}

func TestPullHonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()
	store := Store{
		DataRoot: filepath.Join(t.TempDir(), "data"), QEMUImg: "qemu-img",
		Runner: execx.OSRunner{Timeout: time.Minute}, HTTPClient: server.Client(),
	}
	entry := Entry{
		Alias: "cancel", Release: "1", Arch: "arm64", File: "cancel/image.qcow2",
		Upstream: server.URL + "/image.qcow2", SHA256: strings.Repeat("a", 64), Format: "qcow2", ArtifactSize: 1,
	}
	ctx, cancel := context.WithCancel(context.TODO())
	result := make(chan error, 1)
	go func() {
		_, _, err := store.Pull(ctx, entry)
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("pull cancellation error = %v", err)
	}
}

func TestHTTPRepositoryPrecedesUpstreamAndKeepsReadableName(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	source := filepath.Join(t.TempDir(), "source.qcow2")
	makeQCOW2(t, source, "")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256(data)
	repository := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/u24/u24-1-arm64.qcow2" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = writer.Write(data)
	}))
	defer repository.Close()
	var upstreamHits atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamHits.Add(1)
		http.Error(writer, "unexpected upstream request", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	store.Repository = repository.URL
	store.HTTPClient = repository.Client()
	entry := Entry{Alias: "u24", Release: "1", Arch: "arm64", File: "u24/u24-1-arm64.qcow2", Upstream: upstream.URL + "/image.qcow2", SHA256: hex.EncodeToString(digestBytes[:]), Format: "qcow2", ArtifactSize: int64(len(data)), VirtualSize: 64 << 20}
	pathname, metadata, err := store.Pull(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if pathname != filepath.Join(store.DataRoot, "images", "u24", "u24-1-arm64.qcow2") || !strings.HasPrefix(metadata.Source, "repository:") || upstreamHits.Load() != 0 {
		t.Fatalf("repository pull = path %q metadata %#v upstream hits %d", pathname, metadata, upstreamHits.Load())
	}
}

func TestMissingRepositoryArtifactDoesNotRequestUpstream(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	source := filepath.Join(t.TempDir(), "source.qcow2")
	makeQCOW2(t, source, "")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256(data)
	repository := httptest.NewServer(http.NotFoundHandler())
	defer repository.Close()
	var upstreamHits atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamHits.Add(1)
		writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = writer.Write(data)
	}))
	defer upstream.Close()
	store.Repository = repository.URL
	store.HTTPClient = upstream.Client()
	entry := Entry{Alias: "u24", Release: "1", Arch: "arm64", File: "u24/u24-1-arm64.qcow2", Upstream: upstream.URL + "/image.qcow2", SHA256: hex.EncodeToString(digestBytes[:]), Format: "qcow2", ArtifactSize: int64(len(data)), VirtualSize: 64 << 20}
	_, _, err = store.Pull(context.Background(), entry)
	if err == nil || !strings.Contains(err.Error(), "does not carry this catalog artifact") {
		t.Fatalf("missing repository artifact error = %v", err)
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("repository failure requested catalog upstream %d time(s)", upstreamHits.Load())
	}
}

func TestRepositoryPullEnforcesFinalCatalogDigestWithoutUpstreamFallback(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	final := testHTTPArtifact(t)
	repository := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		// These bytes model the uncustomized provenance artifact. The catalog
		// identity describes the final repository qcow2 instead.
		_, _ = writer.Write([]byte("uncustomized upstream bytes"))
	}))
	defer repository.Close()
	var upstreamHits atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		writer.Header().Set("Content-Length", strconv.Itoa(len(final.data)))
		_, _ = writer.Write(final.data)
	}))
	defer upstream.Close()
	store.Repository = repository.URL
	store.HTTPClient = upstream.Client()
	entry := final.entry(upstream.URL + "/provenance.qcow2")

	if _, _, err := store.Pull(context.Background(), entry); err == nil || !strings.Contains(err.Error(), "catalog") {
		t.Fatalf("repository bytes that differ from the final catalog identity were accepted: %v", err)
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("digest failure fell back to catalog upstream %d time(s)", upstreamHits.Load())
	}
}

func TestHTTPSPullRejectsManifestArtifactSizeMismatch(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	source := filepath.Join(t.TempDir(), "source.qcow2")
	makeQCOW2(t, source, "")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256(data)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = writer.Write(data)
	}))
	defer server.Close()
	store.HTTPClient = server.Client()
	entry := Entry{File: "test/test-1-arm64.qcow2", Upstream: server.URL + "/image.qcow2", SHA256: hex.EncodeToString(digestBytes[:]), Format: "qcow2", ArtifactSize: int64(len(data) + 1)}
	if _, _, err := store.Pull(context.Background(), entry); err == nil {
		t.Fatal("manifest artifact-size mismatch unexpectedly accepted")
	}
}

func TestHTTPSPullRejectsChecksumAndDowngrade(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	source := filepath.Join(t.TempDir(), "source.qcow2")
	makeQCOW2(t, source, "")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = writer.Write(data)
	}))
	defer server.Close()
	store.HTTPClient = server.Client()
	badDigest := strings.Repeat("0", 64)
	entry := Entry{File: "test/test-1-arm64.qcow2", Upstream: server.URL + "/image.qcow2", SHA256: badDigest, Format: "qcow2"}
	if _, _, err := store.Pull(context.Background(), entry); err == nil {
		t.Fatal("checksum mismatch unexpectedly accepted")
	}
	badPath, _ := store.Path(entry)
	if _, err := os.Stat(badPath); !os.IsNotExist(err) {
		t.Fatalf("checksum failure left valid cache path: %v", err)
	}

	plain := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write(data)
	}))
	defer plain.Close()
	redirect := httptest.NewTLSServer(http.RedirectHandler(plain.URL+"/image.qcow2", http.StatusFound))
	defer redirect.Close()
	store.HTTPClient = redirect.Client()
	digestBytes := sha256.Sum256(data)
	entry = Entry{File: "test/test-2-arm64.qcow2", Upstream: redirect.URL, SHA256: hex.EncodeToString(digestBytes[:]), Format: "qcow2"}
	if _, _, err := store.Pull(context.Background(), entry); err == nil {
		t.Fatal("HTTPS-to-HTTP redirect unexpectedly accepted")
	}
}

// httpArtifact is one in-memory qcow2 plus the identity a catalog entry needs.
type httpArtifact struct {
	data   []byte
	digest string
}

func testHTTPArtifact(t *testing.T) httpArtifact {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source.qcow2")
	makeQCOW2(t, source, "")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return httpArtifact{data: data, digest: hex.EncodeToString(sum[:])}
}

func (a httpArtifact) entry(upstream string) Entry {
	return Entry{
		Alias: "test", Release: "1", Arch: "arm64", File: "test/test-1-arm64.qcow2",
		Upstream: upstream, SHA256: a.digest, Format: "qcow2",
		ArtifactSize: int64(len(a.data)), VirtualSize: 64 << 20,
	}
}

func TestIntegrationInterruptedDownloadResumesWithRange(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	artifact := testHTTPArtifact(t)
	var attempts atomic.Int64
	var sawRange atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if attempts.Add(1) == 1 {
			// First attempt dies halfway through, exactly like a dropped link.
			half := len(artifact.data) / 2
			writer.Header().Set("Content-Length", strconv.Itoa(len(artifact.data)))
			_, _ = writer.Write(artifact.data[:half])
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		if request.Header.Get("Range") != "" {
			sawRange.Store(true)
		}
		http.ServeContent(writer, request, "image.qcow2", time.Time{}, strings.NewReader(string(artifact.data)))
	}))
	defer server.Close()
	store.HTTPClient = server.Client()
	entry := artifact.entry(server.URL + "/image.qcow2")

	staged := resumePartialPath(filepath.Join(store.DataRoot, "images", entry.Alias), entry)
	path, _, err := store.Pull(context.Background(), entry)
	if err != nil {
		t.Fatalf("resumed download failed: %v", err)
	}
	if !sawRange.Load() || attempts.Load() != 2 {
		t.Fatalf("automatic retry attempts=%d range=%t", attempts.Load(), sawRange.Load())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("resume point survived a published download: %v", err)
	}
}

func TestIntegrationResumeRestartsWhenSourceIgnoresRange(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	artifact := testHTTPArtifact(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// A source that answers 200 to a Range request must restart cleanly
		// rather than append a second copy onto the existing prefix.
		writer.Header().Set("Content-Length", strconv.Itoa(len(artifact.data)))
		_, _ = writer.Write(artifact.data)
	}))
	defer server.Close()
	store.HTTPClient = server.Client()
	entry := artifact.entry(server.URL + "/image.qcow2")

	directory := filepath.Join(store.DataRoot, "images", entry.Alias)
	if err := ensurePrivateDir(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resumePartialPath(directory, entry), artifact.data[:64], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Pull(context.Background(), entry); err != nil {
		t.Fatalf("range-ignoring source failed: %v", err)
	}
}

func TestIntegrationCompleteStagedDownloadIsReusedWithoutNetwork(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	artifact := testHTTPArtifact(t)
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	store.HTTPClient = server.Client()
	entry := artifact.entry(server.URL + "/image.qcow2")

	directory := filepath.Join(store.DataRoot, "images", entry.Alias)
	if err := ensurePrivateDir(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resumePartialPath(directory, entry), artifact.data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Pull(context.Background(), entry); err != nil {
		t.Fatalf("complete staged download was not reused: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("reused download still contacted the source %d time(s)", requests.Load())
	}
}

func TestIntegrationStagedDownloadThatFailsVerificationIsDiscarded(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	artifact := testHTTPArtifact(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	store.HTTPClient = server.Client()
	entry := artifact.entry(server.URL + "/image.qcow2")

	directory := filepath.Join(store.DataRoot, "images", entry.Alias)
	if err := ensurePrivateDir(directory); err != nil {
		t.Fatal(err)
	}
	// Right length, wrong bytes: the resume point must not survive the digest
	// check, or every later attempt would keep reusing the same poison.
	corrupt := append([]byte(nil), artifact.data...)
	corrupt[len(corrupt)-1] ^= 0xff
	staged := resumePartialPath(directory, entry)
	if err := os.WriteFile(staged, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Pull(context.Background(), entry); err == nil {
		t.Fatal("corrupt staged download was accepted")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("corrupt resume point survived: %v", err)
	}
}

func TestIntegrationRotatedAwayUpstreamExplainsTheRemedy(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	artifact := testHTTPArtifact(t)
	// Distribution mirrors prune dated artifacts. A pinned catalog entry that has
	// aged out must say what fixes it, not just report a status line.
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	store.HTTPClient = server.Client()

	_, _, err := store.Pull(context.Background(), artifact.entry(server.URL+"/image.qcow2"))
	if err == nil {
		t.Fatal("missing upstream artifact was accepted")
	}
	_, _, next := failure.Classify(err)
	for _, want := range []string{"no longer published upstream", "farrow update", "--repo", "farrow image import"} {
		if !strings.Contains(err.Error()+" next: "+next, want) {
			t.Errorf("rotated-away upstream error is missing %q:\n%v next: %s", want, err, next)
		}
	}
}

func TestIntegrationTransportFailureIsNotReportedAsRotatedAway(t *testing.T) {
	t.Parallel()
	store := testImageStore(t)
	artifact := testHTTPArtifact(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	store.HTTPClient = server.Client()

	_, _, err := store.Pull(context.Background(), artifact.entry(server.URL+"/image.qcow2"))
	if err == nil {
		t.Fatal("failing upstream was accepted")
	}
	if strings.Contains(err.Error(), "no longer published upstream") {
		t.Errorf("a retryable transport failure was reported as a rotated-away artifact:\n%v", err)
	}
}
