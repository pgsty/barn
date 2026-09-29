package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/fsutil"
	darwinnet "github.com/pgsty/barn/internal/network/darwin"
	"github.com/pgsty/barn/internal/webclient"
)

const maxSocketVMNetArchive = 4 << 20

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type DownloadResult struct {
	Path       string `json:"path"`
	URL        string `json:"url"`
	SHA256     string `json:"sha256"`
	Downloaded bool   `json:"downloaded"`
}

func defaultHTTPClient() *http.Client {
	return webclient.NewWithRedirectPolicy(2*time.Minute, func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many socket_vmnet redirects")
		}
		if request.URL.Scheme != "https" {
			return errors.New("socket_vmnet redirect is not HTTPS")
		}
		return nil
	})
}

func ensureCacheDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("setup cache directory must be a clean absolute path")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("setup cache is not a real directory: %s", path)
	}
	return os.Chmod(path, 0o700)
}

func DefaultCacheDirectory() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "barn", "downloads"), nil
}

// Sources widens where the pinned socket_vmnet archive may come from. The
// SHA-256 embedded in the binary is enforced identically for every source,
// so any mirror — a local file, a private repository, the upstream release —
// is equally trustworthy. This matters most on networks where GitHub is slow
// or unreachable.
type Sources struct {
	// Archive is an explicit local tarball (BARN_VMNET_ARCHIVE). When set it
	// must verify; a mismatch is an error, never a silent fallback.
	Archive string
	// Repo is the resolved image-repository URL or absolute local directory
	// (--repo / --mirror / BARN_REPO / default). The archive is expected under
	// <repo>/socket_vmnet/<archive-name>. A missing mirror copy falls through
	// to upstream; a corrupt one is an error.
	Repo string
	// Progress receives byte-level download events.
	Progress activity.Reporter
}

// SourcesFromEnvironment resolves the local archive override and accepts the
// repository already selected by the command. The environment fallback is
// retained for direct internal callers.
func SourcesFromEnvironment(repo string) Sources {
	if repo == "" {
		repo = os.Getenv("BARN_REPO")
	}
	return Sources{Archive: os.Getenv("BARN_VMNET_ARCHIVE"), Repo: repo}
}

// SocketVMNetCached reports whether the pinned archive is already present and
// fully verified in the user cache, so setup plans can say "cached" instead
// of announcing a download.
func SocketVMNetCached(arch string) bool {
	release, err := darwinnet.PinnedRelease(arch)
	if err != nil {
		return false
	}
	cacheDirectory, err := DefaultCacheDirectory()
	if err != nil {
		return false
	}
	target := filepath.Join(cacheDirectory, release.SHA256+"-"+release.ArchiveName)
	_, err = darwinnet.VerifyArchive(target, arch)
	return err == nil
}

// DownloadPinnedSocketVMNet obtains the pinned release archive from the first
// working source — an explicit local archive, the configured repository
// mirror, then the embedded upstream URL — bounds and digest-verifies it, and
// atomically publishes a user-owned cache entry. An already verified cache
// entry is reused without touching the network.
func DownloadPinnedSocketVMNet(ctx context.Context, arch, cacheDirectory string, client HTTPDoer, sources Sources) (DownloadResult, error) {
	release, err := darwinnet.PinnedRelease(arch)
	if err != nil {
		return DownloadResult{}, err
	}
	return downloadSocketVMNetRelease(ctx, release, arch, cacheDirectory, client, sources, func(path, arch string) error {
		_, err := darwinnet.VerifyArchive(path, arch)
		return err
	})
}

func downloadSocketVMNetRelease(ctx context.Context, release darwinnet.Release, arch, cacheDirectory string, client HTTPDoer, sources Sources, verify func(string, string) error) (DownloadResult, error) {
	if verify == nil {
		return DownloadResult{}, errors.New("socket_vmnet verifier is missing")
	}
	if release.ArchiveName == "" || filepath.Base(release.ArchiveName) != release.ArchiveName || len(release.SHA256) != 64 {
		return DownloadResult{}, errors.New("embedded socket_vmnet identity is invalid")
	}
	if parsed, parseErr := url.Parse(release.URL); parseErr != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return DownloadResult{}, errors.New("embedded socket_vmnet URL is invalid")
	}
	if cacheDirectory == "" {
		defaultDirectory, err := DefaultCacheDirectory()
		if err != nil {
			return DownloadResult{}, err
		}
		cacheDirectory = defaultDirectory
	}
	if err := ensureCacheDirectory(cacheDirectory); err != nil {
		return DownloadResult{}, err
	}
	target := filepath.Join(cacheDirectory, release.SHA256+"-"+release.ArchiveName)
	result := DownloadResult{Path: target, URL: release.URL, SHA256: release.SHA256}
	if verifyErr := verify(target, arch); verifyErr == nil {
		return result, nil
	} else if info, statErr := os.Lstat(target); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return result, fmt.Errorf("cached socket_vmnet archive is unsafe: %w", verifyErr)
		}
		if removeErr := os.Remove(target); removeErr != nil {
			return result, fmt.Errorf("remove invalid owned socket_vmnet cache entry: %w", removeErr)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return result, statErr
	}
	// 1. Explicit local archive: user intent, so a mismatch is an error.
	if sources.Archive != "" {
		if !filepath.IsAbs(sources.Archive) {
			return result, errors.New("BARN_VMNET_ARCHIVE must be an absolute path")
		}
		if err := verify(sources.Archive, arch); err != nil {
			return result, failure.New(failure.Integrity, fmt.Errorf("BARN_VMNET_ARCHIVE %s does not match the pinned socket_vmnet v%s archive: %w", sources.Archive, release.Version, err))
		}
		if err := publishVerifiedCopy(sources.Archive, cacheDirectory, target, arch, verify); err != nil {
			return result, err
		}
		result.URL = "file://" + sources.Archive
		result.Downloaded = true
		return result, nil
	}
	// 2. Repository mirror: <repo>/socket_vmnet/<archive-name>. A missing
	//    mirror copy falls through to upstream; a corrupt one fails closed.
	if sources.Repo != "" {
		if filepath.IsAbs(sources.Repo) {
			candidate := filepath.Join(sources.Repo, "socket_vmnet", release.ArchiveName)
			if _, statErr := os.Lstat(candidate); statErr == nil {
				if err := verify(candidate, arch); err != nil {
					return result, failure.New(failure.Integrity, fmt.Errorf("repository copy %s does not match the pinned socket_vmnet v%s archive: %w", candidate, release.Version, err))
				}
				if err := publishVerifiedCopy(candidate, cacheDirectory, target, arch, verify); err != nil {
					return result, err
				}
				result.URL = "file://" + candidate
				result.Downloaded = true
				return result, nil
			}
		} else {
			mirror := strings.TrimSuffix(sources.Repo, "/") + "/socket_vmnet/" + release.ArchiveName
			fetched, fetchErr := fetchBoundedArchive(ctx, client, mirror, release, cacheDirectory, target, arch, sources.Progress, verify)
			if fetchErr == nil {
				return fetched, nil
			}
			// The mirror simply not carrying the file is a fall-through; a
			// digest mismatch on a present file is not.
			var invalid *archiveVerificationError
			if errors.As(fetchErr, &invalid) {
				return result, fetchErr
			}
		}
	}
	// 3. The embedded upstream URL.
	fetched, err := fetchBoundedArchive(ctx, client, release.URL, release, cacheDirectory, target, arch, sources.Progress, verify)
	if err != nil {
		// Any mirror works: the SHA-256 is pinned in the binary.
		return result, failure.WithNext(err, fmt.Sprintf("put %s under <repo>/socket_vmnet/ and pass --repo, or download it by hand and set BARN_VMNET_ARCHIVE=/absolute/path/to/%s", release.ArchiveName, release.ArchiveName))
	}
	return fetched, nil
}

// publishVerifiedCopy copies an already verified source archive into the
// cache atomically and re-verifies the published bytes.
func publishVerifiedCopy(source, cacheDirectory, target, arch string, verify func(string, string) error) error {
	temporary, _, err := fsutil.CopyToTemp(source, cacheDirectory, ".socket-vmnet-*.partial", 0o600, maxSocketVMNetArchive)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(temporary)
		}
	}()
	if err := verify(temporary, arch); err != nil {
		return fmt.Errorf("verify staged socket_vmnet copy: %w", err)
	}
	if err := os.Rename(temporary, target); err != nil {
		return err
	}
	keep = true
	return nil
}

// progressBody reports byte progress while the response body is copied.
type progressBody struct {
	reader  io.Reader
	report  activity.Reporter
	event   activity.Event
	written int64
	last    time.Time
}

func (body *progressBody) Read(data []byte) (int, error) {
	read, err := body.reader.Read(data)
	body.written += int64(read)
	if now := time.Now(); now.Sub(body.last) >= 100*time.Millisecond || err == io.EOF {
		body.last = now
		event := body.event
		event.CurrentBytes = body.written
		body.report.Report(event)
	}
	return read, err
}

func fetchBoundedArchive(ctx context.Context, client HTTPDoer, fetchURL string, release darwinnet.Release, cacheDirectory, target, arch string, progress activity.Reporter, verify func(string, string) error) (DownloadResult, error) {
	if client == nil {
		client = defaultHTTPClient()
	}
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return DownloadResult{}, err
		}
		result, err := fetchBoundedArchiveAttempt(ctx, client, fetchURL, release, cacheDirectory, target, arch, progress, verify)
		if err == nil || attempt == 3 || !retryArchiveDownload(err) {
			return result, err
		}
		delay := time.Duration(attempt) * 500 * time.Millisecond
		var response *archiveHTTPError
		if errors.As(err, &response) {
			if seconds, parseErr := strconv.Atoi(response.After); parseErr == nil && seconds >= 0 {
				delay = max(delay, time.Duration(min(seconds, 31))*time.Second)
			} else if date, parseErr := http.ParseTime(response.After); parseErr == nil {
				delay = max(delay, time.Until(date))
			}
		}
		if delay > 30*time.Second {
			return result, err // Let the caller try its next pinned source.
		}
		progress.Report(activity.Event{Phase: "download-retry", Message: fmt.Sprintf("Retrying socket_vmnet download (%d/3): %v", attempt+1, err)})
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
		}
	}
}

type archiveHTTPError struct {
	Host   string
	Status int
	After  string
}

func (err *archiveHTTPError) Error() string {
	return fmt.Sprintf("download socket_vmnet from %s: HTTP %d %s", err.Host, err.Status, http.StatusText(err.Status))
}

type archiveVerificationError struct{ error }

func (err *archiveVerificationError) Unwrap() error { return err.error }

func retryArchiveDownload(err error) bool {
	var invalid *archiveVerificationError
	if errors.As(err, &invalid) {
		return false
	}
	var response *archiveHTTPError
	if errors.As(err, &response) {
		switch response.Status {
		case 408, 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	var connection *net.OpError
	var temporary net.Error
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.As(err, &connection) || errors.As(err, &temporary) && temporary.Timeout()
}

func fetchBoundedArchiveAttempt(ctx context.Context, client HTTPDoer, fetchURL string, release darwinnet.Release, cacheDirectory, target, arch string, progress activity.Reporter, verify func(string, string) error) (DownloadResult, error) {
	result := DownloadResult{Path: target, URL: fetchURL, SHA256: release.SHA256}
	parsed, err := url.Parse(fetchURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return result, fmt.Errorf("socket_vmnet source URL must be HTTPS: %s", fetchURL)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return result, err
	}
	request.Header.Set("User-Agent", "barn-setup/"+release.Version)
	response, err := client.Do(request)
	if err != nil {
		return result, fmt.Errorf("download socket_vmnet from %s: %w", parsed.Host, err)
	}
	defer func() {
		// The bounded response is read-only and the archive is SHA-256 verified;
		// closing only affects HTTP connection reuse.
		_ = response.Body.Close()
	}()
	if response.Request != nil && response.Request.URL != nil && response.Request.URL.Scheme != "https" {
		return result, errors.New("socket_vmnet response URL is not HTTPS")
	}
	if response.StatusCode != http.StatusOK {
		return result, &archiveHTTPError{Host: parsed.Host, Status: response.StatusCode, After: response.Header.Get("Retry-After")}
	}
	if response.ContentLength > maxSocketVMNetArchive {
		return result, fmt.Errorf("socket_vmnet download exceeds the %d-byte limit", maxSocketVMNetArchive)
	}
	temporary, err := os.CreateTemp(cacheDirectory, ".socket-vmnet-*.partial")
	if err != nil {
		return result, err
	}
	temporaryPath := temporary.Name()
	published := false
	defer func() {
		_ = temporary.Close()
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return result, err
	}
	body := &progressBody{
		reader: io.LimitReader(response.Body, maxSocketVMNetArchive+1),
		report: progress,
		event: activity.Event{
			Phase: "download", Message: "Downloading socket_vmnet v" + release.Version,
			Source: parsed.Host, TotalBytes: max(response.ContentLength, 0), StartedAt: time.Now(),
		},
	}
	written, err := io.Copy(temporary, body)
	if err != nil {
		return result, fmt.Errorf("download socket_vmnet from %s: %w", parsed.Host, err)
	}
	if written > maxSocketVMNetArchive {
		return result, fmt.Errorf("socket_vmnet download exceeds the %d-byte limit", maxSocketVMNetArchive)
	}
	if response.ContentLength >= 0 && written != response.ContentLength {
		return result, fmt.Errorf("socket_vmnet download from %s ended after %d of %d bytes: %w", parsed.Host, written, response.ContentLength, io.ErrUnexpectedEOF)
	}
	if err := temporary.Sync(); err != nil {
		return result, err
	}
	if err := temporary.Close(); err != nil {
		return result, err
	}
	if err := verify(temporaryPath, arch); err != nil {
		return result, failure.New(failure.Integrity, &archiveVerificationError{fmt.Errorf("downloaded copy from %s does not match the pinned socket_vmnet v%s archive: %w", parsed.Host, release.Version, err)})
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return result, err
	}
	published = true
	result.Downloaded = true
	return result, nil
}
