package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pgsty/farrow/internal/activity"
	"github.com/pgsty/farrow/internal/disk"
	"github.com/pgsty/farrow/internal/execx"
	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/fsutil"
	"github.com/pgsty/farrow/internal/lock"
	"github.com/pgsty/farrow/internal/webclient"
)

const (
	MetadataSchema        = 1
	MaxArtifactSize int64 = 8 << 30
)

// ErrIntegrity identifies image bytes that fail verification.
var ErrIntegrity error = failure.New(failure.Integrity, errors.New("image integrity check failed")).Because("image_integrity")

var (
	digestPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	localImageFilename = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,191}\.(?:qcow2|img)$`)
)

// Metadata describes bytes verified during this operation. It is returned to
// callers but is deliberately not written as a sidecar next to every image.
type Metadata struct {
	Schema       int       `json:"schema"`
	Digest       string    `json:"digest"`
	Format       string    `json:"format"`
	VirtualSize  int64     `json:"virtual_size"`
	ArtifactSize int64     `json:"artifact_size"`
	Source       string    `json:"source"`
	ImportedAt   time.Time `json:"imported_at"`
}

type Store struct {
	DataRoot   string
	Repository string
	QEMUImg    string
	Runner     execx.Runner
	HTTPClient *http.Client
	Progress   activity.Reporter
}

const byteProgressInterval = 250 * time.Millisecond

type reportingReader struct {
	reader   io.Reader
	reporter activity.Reporter
	event    activity.Event
	current  int64
	last     time.Time
}

func (reader *reportingReader) Read(buffer []byte) (int, error) {
	read, err := reader.reader.Read(buffer)
	reader.current += int64(read)
	now := time.Now()
	if read > 0 && (reader.last.IsZero() || now.Sub(reader.last) >= byteProgressInterval) {
		reader.event.CurrentBytes = reader.current
		reader.reporter.Report(reader.event)
		reader.last = now
	}
	return read, err
}

func displayActivitySource(source string) string {
	parsed, err := url.Parse(source)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return source
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func copyWithProgress(destination io.Writer, source io.Reader, limit int64, reporter activity.Reporter, event activity.Event) (int64, error) {
	event.StartBytes = event.CurrentBytes
	if event.StartedAt.IsZero() {
		event.StartedAt = time.Now()
	}
	reporter.Report(event)
	// event.CurrentBytes seeds the counter so a resumed transfer reports absolute
	// progress against TotalBytes rather than restarting the bar at zero.
	tracked := &reportingReader{reader: io.LimitReader(source, limit), reporter: reporter, event: event, current: event.CurrentBytes, last: time.Now()}
	written, err := io.Copy(destination, tracked)
	event.CurrentBytes = tracked.current
	event.Done = err == nil
	reporter.Report(event)
	return written, err
}

// errSourceGone marks a source that answered "this artifact is not here". It is
// distinguished from a transport failure because the remedy is a newer catalog
// or another repository, not a retry against the same source.
var errSourceGone = errors.New("image source no longer publishes this artifact")

// errImageStalled marks a transfer that stopped delivering bytes. It is a
// distinct cause so the message can tell a stalled source apart from a refused
// or cancelled one.
var errImageStalled = errors.New("image source stopped sending data")

// imageStallTimeout bounds inactivity, not total duration. A multi-gigabyte
// artifact on a constrained link must be allowed to take as long as it honestly
// needs; only a source that has genuinely stopped is a failure.
const imageStallTimeout = 2 * time.Minute

// stallReader re-arms a watchdog on every byte received. Paired with a
// cancellable request context it replaces an overall client deadline, which
// would abort a healthy but slow download partway through.
type stallReader struct {
	reader  io.Reader
	timer   *time.Timer
	timeout time.Duration
}

func (reader *stallReader) Read(buffer []byte) (int, error) {
	read, err := reader.reader.Read(buffer)
	if read > 0 {
		reader.timer.Reset(reader.timeout)
	}
	return read, err
}

func (s Store) lockPath() string { return filepath.Join(s.DataRoot, "locks", "images.lock") }

// imagesRoot is the dedicated image subtree of the data root: family
// directories, local imports, the local-alias registry, and the signed
// manifests all live under it, never at the data-root top level.
func (s Store) imagesRoot() string { return filepath.Join(s.DataRoot, "images") }

func ensurePrivateDir(pathname string) error {
	if err := os.MkdirAll(pathname, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(pathname)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("farrow path is unsafe: %s", pathname)
	}
	return os.Chmod(pathname, 0o700)
}

func (s Store) validate() error {
	if s.DataRoot == "" || !filepath.IsAbs(s.DataRoot) || s.QEMUImg == "" || s.Runner == nil {
		return errors.New("image store data root, qemu-img, and runner are required")
	}
	if _, err := NormalizeRepository(s.Repository); err != nil {
		return err
	}
	for _, directory := range []string{s.DataRoot, s.imagesRoot(), filepath.Dir(s.lockPath())} {
		if err := ensurePrivateDir(directory); err != nil {
			return err
		}
	}
	return nil
}

func validStoreFile(filename string) bool {
	if filename == "" || path.Clean(filename) != filename || strings.HasPrefix(filename, "/") || strings.Contains(filename, "\\") {
		return false
	}
	parts := strings.Split(filename, "/")
	return len(parts) == 2 && catalogName.MatchString(parts[0]) && localImageFilename.MatchString(parts[1])
}

func localStoreFile(entry Entry) string {
	if entry.CacheFile != "" {
		return entry.CacheFile
	}
	// Backward-compatible fallback for private local aliases and tests created
	// before repository and cache paths became separate fields.
	return entry.File
}

func (s Store) Path(entry Entry) (string, error) {
	filename := localStoreFile(entry)
	if !validStoreFile(filename) {
		return "", fmt.Errorf("invalid local image path %q", filename)
	}
	return filepath.Join(s.imagesRoot(), filepath.FromSlash(filename)), nil
}

func (s Store) ensureImageDirectory(entry Entry) (string, error) {
	target, err := s.Path(entry)
	if err != nil {
		return "", err
	}
	directory := filepath.Dir(target)
	if filepath.Dir(directory) != filepath.Clean(s.imagesRoot()) {
		return "", errors.New("image path must have exactly one family directory")
	}
	if err := ensurePrivateDir(directory); err != nil {
		return "", err
	}
	return directory, nil
}

func digestFileWithProgress(pathname string, reporter activity.Reporter, event activity.Event) (string, int64, error) {
	info, err := os.Lstat(pathname)
	if err != nil || !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("image must be a regular non-symlink file: %s", pathname)
	}
	if info.Size() > MaxArtifactSize {
		return "", 0, fmt.Errorf("image size %d exceeds limit %d", info.Size(), MaxArtifactSize)
	}
	handle, err := os.Open(pathname)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		// Digesting is read-only; the accepted byte count and hash are complete
		// before this descriptor is released.
		_ = handle.Close()
	}()
	hash := sha256.New()
	event.TotalBytes = info.Size()
	written, err := copyWithProgress(hash, handle, MaxArtifactSize+1, reporter, event)
	if err != nil {
		return "", written, err
	}
	if written > MaxArtifactSize {
		return "", written, errors.New("image exceeded size limit while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), written, nil
}

func digestFile(pathname string) (string, int64, error) {
	return digestFileWithProgress(pathname, nil, activity.Event{})
}

func (s Store) manager() disk.Manager { return disk.Manager{QEMUImg: s.QEMUImg, Runner: s.Runner} }

func validateEntryIdentity(entry Entry) error {
	if !digestPattern.MatchString(entry.SHA256) || entry.Format != "qcow2" || !validStoreFile(localStoreFile(entry)) {
		return errors.New("catalog image digest, format, or local cache file is invalid")
	}
	if entry.ArtifactSize < 0 || entry.ArtifactSize > MaxArtifactSize || entry.VirtualSize < 0 {
		return errors.New("catalog image size is invalid")
	}
	return nil
}

func (s Store) ValidateCached(ctx context.Context, entry Entry) (string, Metadata, error) {
	if err := validateEntryIdentity(entry); err != nil {
		return "", Metadata{}, err
	}
	pathname, err := s.Path(entry)
	if err != nil {
		return "", Metadata{}, err
	}
	info, err := os.Lstat(pathname)
	if err != nil {
		return "", Metadata{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o222 != 0 {
		return "", Metadata{}, errors.New("local image is not an immutable regular file")
	}
	actual, size, err := digestFileWithProgress(pathname, s.Progress, activity.Event{
		Phase:   "image-verify",
		Message: fmt.Sprintf("Verifying cached image %s %s (%s)", entry.Alias, entry.Release, entry.Arch),
	})
	if err != nil {
		return "", Metadata{}, fmt.Errorf("local image digest mismatch: got %s want %s: %w", actual, entry.SHA256, err)
	}
	if actual != entry.SHA256 {
		return "", Metadata{}, fmt.Errorf("%w: local image digest mismatch: got %s want %s", ErrIntegrity, actual, entry.SHA256)
	}
	if entry.ArtifactSize > 0 && size != entry.ArtifactSize {
		return "", Metadata{}, fmt.Errorf("local image size mismatch: got %d want %d", size, entry.ArtifactSize)
	}
	imageInfo, err := s.manager().Inspect(ctx, pathname)
	if err != nil {
		return "", Metadata{}, err
	}
	if err := disk.ValidateBase(imageInfo); err != nil {
		return "", Metadata{}, err
	}
	if entry.VirtualSize > 0 && imageInfo.VirtualSize != entry.VirtualSize {
		return "", Metadata{}, fmt.Errorf("local image virtual size mismatch: got %d want %d", imageInfo.VirtualSize, entry.VirtualSize)
	}
	metadata := Metadata{Schema: MetadataSchema, Digest: actual, Format: "qcow2", VirtualSize: imageInfo.VirtualSize, ArtifactSize: size, Source: "local:" + pathname, ImportedAt: info.ModTime().UTC()}
	return pathname, metadata, nil
}

func (s Store) publish(ctx context.Context, tempPath string, entry Entry, source string, artifactSize int64) (string, Metadata, error) {
	actual, size, err := digestFileWithProgress(tempPath, s.Progress, activity.Event{
		Phase:   "image-verify",
		Message: fmt.Sprintf("Verifying downloaded image %s %s (%s)", entry.Alias, entry.Release, entry.Arch),
	})
	if err != nil {
		return "", Metadata{}, err
	}
	if actual != entry.SHA256 || size != artifactSize || (entry.ArtifactSize > 0 && size != entry.ArtifactSize) {
		return "", Metadata{}, errors.New("copied or downloaded image digest/size does not match catalog")
	}
	imageInfo, err := s.manager().Inspect(ctx, tempPath)
	if err != nil {
		return "", Metadata{}, err
	}
	if err := disk.ValidateBase(imageInfo); err != nil {
		return "", Metadata{}, err
	}
	if entry.VirtualSize > 0 && imageInfo.VirtualSize != entry.VirtualSize {
		return "", Metadata{}, errors.New("copied or downloaded image virtual size does not match catalog")
	}
	if err := os.Chmod(tempPath, 0o444); err != nil {
		return "", Metadata{}, err
	}
	target, _ := s.Path(entry)
	if err := os.Rename(tempPath, target); err != nil {
		return "", Metadata{}, err
	}
	if err := fsutil.SyncDir(filepath.Dir(target)); err != nil {
		return "", Metadata{}, err
	}
	metadata := Metadata{Schema: MetadataSchema, Digest: actual, Format: "qcow2", VirtualSize: imageInfo.VirtualSize, ArtifactSize: size, Source: source, ImportedAt: time.Now().UTC()}
	return target, metadata, nil
}

func (s Store) Import(ctx context.Context, source, expectedDigest string) (_ string, _ Metadata, returnErr error) {
	if err := s.validate(); err != nil {
		return "", Metadata{}, err
	}
	absSource, err := filepath.Abs(source)
	if err != nil {
		return "", Metadata{}, err
	}
	if _, err := os.Lstat(absSource); errors.Is(err, os.ErrNotExist) {
		return "", Metadata{}, failure.New(failure.Usage, fmt.Errorf("no such file: %s", absSource))
	}
	digest, sourceSize, err := digestFile(absSource)
	if err != nil {
		return "", Metadata{}, err
	}
	if expectedDigest != "" && digest != expectedDigest {
		return "", Metadata{}, fmt.Errorf("%w: local image digest %s does not match expected %s", ErrIntegrity, digest, expectedDigest)
	}
	if !qcow2File(absSource) {
		return "", Metadata{}, failure.New(failure.Usage, fmt.Errorf("%s is not a qcow2 image", absSource))
	}
	basename := filepath.Base(absSource)
	if !localImageFilename.MatchString(basename) {
		return "", Metadata{}, failure.New(failure.Usage, fmt.Errorf("%s: image file names may use letters, digits and ._+- and must end in .qcow2 or .img", basename))
	}
	entry := Entry{Alias: "local", Release: "local-" + digest[:12], CacheFile: "local/" + basename, SHA256: digest, Format: "qcow2", ArtifactSize: sourceSize}
	lockContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	imageLock, err := lock.Acquire(lockContext, s.lockPath(), false)
	if err != nil {
		return "", Metadata{}, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, imageLock, "image import lock")
	}()
	directory, err := s.ensureImageDirectory(entry)
	if err != nil {
		return "", Metadata{}, err
	}
	target, _ := s.Path(entry)
	if _, err := os.Lstat(target); err == nil {
		pathname, metadata, err := s.ValidateCached(ctx, entry)
		if errors.Is(err, ErrIntegrity) {
			// The cache is keyed by basename: a different file with the same name.
			return "", Metadata{}, failure.New(failure.Usage, fmt.Errorf("the image cache already holds a different %s", entry.CacheFile)).Because("image_name_taken").Then("rename the file to import it alongside, or remove the old one with farrow image prune --yes")
		}
		return pathname, metadata, err
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", Metadata{}, err
	}
	tempPath, copied, err := fsutil.CopyToTemp(absSource, directory, ".import-*.partial", 0o600, MaxArtifactSize)
	if err != nil {
		return "", Metadata{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	if copied != sourceSize {
		return "", Metadata{}, errors.New("local image changed while importing")
	}
	pathname, metadata, err := s.publish(ctx, tempPath, entry, "local:"+absSource, copied)
	if err == nil {
		keep = true
	}
	return pathname, metadata, err
}

// qcow2File reports whether pathname starts with the qcow2 magic, so a wrong
// file is rejected before it is copied into the cache.
func qcow2File(pathname string) bool {
	handle, err := os.Open(pathname)
	if err != nil {
		return false
	}
	defer func() { _ = handle.Close() }()
	magic := make([]byte, 4)
	_, err = io.ReadFull(handle, magic)
	return err == nil && string(magic) == "QFI\xfb"
}

func (s Store) httpClient() *http.Client {
	// No overall client deadline: image transfers are bounded by the per-read
	// stall watchdog instead, so a slow link finishes rather than failing at an
	// arbitrary wall-clock limit with nothing to show for it. The response-header
	// wait stays bounded so an unresponsive server still fails fast.
	client := *webclient.New(0)
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport.ResponseHeaderTimeout = 60 * time.Second
	}
	if s.HTTPClient != nil {
		client = *s.HTTPClient
	}
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 5 || (request.URL.Scheme != "http" && request.URL.Scheme != "https") {
			return errors.New("image redirect scheme is invalid or exceeds five hops")
		}
		if len(via) > 0 && via[0].URL.Scheme == "https" && request.URL.Scheme != "https" {
			return errors.New("image redirect attempts an HTTPS downgrade")
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		return nil
	}
	return &client
}

// resumePartialPath names the staging file after the artifact digest so an
// interrupted transfer can continue instead of restarting from zero. Prune still
// recognises it: the name keeps the .download- prefix and .partial suffix.
func resumePartialPath(directory string, entry Entry) string {
	if entry.ArtifactSize <= 0 || !digestPattern.MatchString(entry.SHA256) {
		return ""
	}
	return filepath.Join(directory, ".download-"+entry.SHA256+".partial")
}

// openStaging returns the staging handle, the byte offset already on disk, and
// whether the file is safe to keep when the transfer fails. A resumable staging
// file survives a transient failure; an anonymous one never does.
func (s Store) openStaging(directory string, entry Entry) (*os.File, string, int64, error) {
	resumable := resumePartialPath(directory, entry)
	if resumable == "" {
		handle, err := os.CreateTemp(directory, ".download-*.partial")
		if err != nil {
			return nil, "", 0, err
		}
		if err := handle.Chmod(0o600); err != nil {
			_ = handle.Close()
			_ = os.Remove(handle.Name())
			return nil, "", 0, err
		}
		return handle, handle.Name(), 0, nil
	}
	info, err := os.Lstat(resumable)
	switch {
	case err == nil && (!info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > entry.ArtifactSize):
		// Anything that is not our own private regular file of a plausible length
		// is not evidence of our own interrupted transfer. Start clean.
		if err := os.Remove(resumable); err != nil {
			return nil, "", 0, err
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return nil, "", 0, err
	}
	handle, err := os.OpenFile(resumable, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, "", 0, err
	}
	offset, err := handle.Seek(0, io.SeekEnd)
	if err != nil {
		_ = handle.Close()
		return nil, "", 0, err
	}
	return handle, resumable, offset, nil
}

func (s Store) stageHTTP(ctx context.Context, source, directory string, entry Entry) (string, int64, error) {
	parsed, err := url.Parse(source)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", 0, errors.New("repository image URL must be absolute HTTP(S)")
	}
	displaySource := displayActivitySource(source)
	handle, stagingPath, resumeFrom, err := s.openStaging(directory, entry)
	if err != nil {
		return "", 0, err
	}
	// A staging file that contradicts the catalog is poisoned and must go. One
	// that merely lost its connection is kept, because it is the resume point.
	poisoned := false
	complete := false
	defer func() {
		_ = handle.Close()
		if poisoned || (!complete && resumePartialPath(directory, entry) != stagingPath) {
			_ = os.Remove(stagingPath)
		}
	}()

	if entry.ArtifactSize > 0 && resumeFrom == entry.ArtifactSize {
		// The bytes are already here from an interrupted run; publish verifies the
		// digest, so reusing them costs nothing and saves the whole transfer.
		s.Progress.Report(activity.Event{
			Phase: "image-download", Message: fmt.Sprintf("Reusing the complete staged download of %s %s (%s)", entry.Alias, entry.Release, entry.Arch),
			Source: displaySource, CurrentBytes: resumeFrom, TotalBytes: resumeFrom, Done: true,
		})
		complete = true
		return stagingPath, resumeFrom, nil
	}

	s.Progress.Report(activity.Event{
		Phase: "image-source", Message: fmt.Sprintf("Connecting to an image source for %s %s (%s)", entry.Alias, entry.Release, entry.Arch), Source: displaySource,
	})
	downloadContext, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchdog := time.AfterFunc(imageStallTimeout, func() { cancel(errImageStalled) })
	defer watchdog.Stop()

	request, err := http.NewRequestWithContext(downloadContext, http.MethodGet, source, nil)
	if err != nil {
		return "", 0, err
	}
	if resumeFrom > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}
	response, err := s.httpClient().Do(request)
	if err != nil {
		return "", resumeFrom, stallAwareError(downloadContext, err)
	}
	defer func() {
		// The bounded response is read-only and its bytes are digest-verified;
		// closing only affects HTTP connection reuse.
		_ = response.Body.Close()
	}()
	switch response.StatusCode {
	case http.StatusOK:
		// The source ignored the range request, so the body starts at byte zero and
		// the staged prefix has to go. If the reset itself fails the offset is no
		// longer something we can reason about, so discard the file rather than
		// leave a staging point nobody can trust.
		if resumeFrom > 0 {
			if err := handle.Truncate(0); err != nil {
				poisoned = true
				return "", 0, err
			}
			if _, err := handle.Seek(0, io.SeekStart); err != nil {
				poisoned = true
				return "", 0, err
			}
			resumeFrom = 0
		}
	case http.StatusPartialContent:
		if !resumeMatchesRange(response.Header.Get("Content-Range"), resumeFrom, entry.ArtifactSize) {
			poisoned = true
			return "", resumeFrom, errors.New("image source answered the resume request with a mismatched byte range")
		}
		s.Progress.Report(activity.Event{
			Phase: "image-download", Source: displaySource, CurrentBytes: resumeFrom, TotalBytes: entry.ArtifactSize,
			Message: fmt.Sprintf("Resuming the interrupted download of %s %s (%s)", entry.Alias, entry.Release, entry.Arch),
		})
	case http.StatusNotFound, http.StatusGone:
		return "", resumeFrom, fmt.Errorf("%w: %s", errSourceGone, response.Status)
	default:
		return "", resumeFrom, &sourceHTTPError{Status: response.StatusCode, After: response.Header.Get("Retry-After")}
	}
	if response.Request != nil && response.Request.URL != nil {
		displaySource = displayActivitySource(response.Request.URL.String())
	}
	remaining := entry.ArtifactSize - resumeFrom
	if response.ContentLength > MaxArtifactSize || (entry.ArtifactSize > 0 && response.ContentLength >= 0 && response.ContentLength != remaining) {
		poisoned = true
		return "", resumeFrom, errors.New("download Content-Length differs from catalog policy")
	}

	total := entry.ArtifactSize
	if total <= 0 && response.ContentLength >= 0 {
		total = resumeFrom + response.ContentLength
	}
	body := &stallReader{reader: response.Body, timer: watchdog, timeout: imageStallTimeout}
	received, err := copyWithProgress(handle, body, MaxArtifactSize+1-resumeFrom, s.Progress, activity.Event{
		Phase:        "image-download",
		Message:      fmt.Sprintf("Downloading image %s %s (%s)", entry.Alias, entry.Release, entry.Arch),
		Source:       displaySource,
		CurrentBytes: resumeFrom,
		TotalBytes:   total,
	})
	written := resumeFrom + received
	if err != nil {
		return "", written, fmt.Errorf("download image: %w", stallAwareError(downloadContext, err))
	}
	if written > MaxArtifactSize || (response.ContentLength >= 0 && received != response.ContentLength) || (entry.ArtifactSize > 0 && written != entry.ArtifactSize) {
		poisoned = true
		return "", written, errors.New("downloaded image size differs from catalog policy")
	}
	if err := handle.Sync(); err != nil {
		return "", written, err
	}
	if err := handle.Close(); err != nil {
		return "", written, err
	}
	complete = true
	return stagingPath, written, nil
}

// resumeMatchesRange proves a 206 answers exactly the range that was asked for,
// against the total size the catalog promised.
func resumeMatchesRange(header string, resumeFrom, artifactSize int64) bool {
	if resumeFrom <= 0 || artifactSize <= 0 {
		return false
	}
	return header == fmt.Sprintf("bytes %d-%d/%d", resumeFrom, artifactSize-1, artifactSize)
}

// stallAwareError reports a watchdog abort as a stall rather than as the generic
// context cancellation the transport surfaces.
func stallAwareError(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); errors.Is(cause, errImageStalled) {
		return fmt.Errorf("%w after %s of no progress", errImageStalled, imageStallTimeout)
	}
	return reachError(err)
}

// unreachableError names the host and the short network reason instead of
// Go's `Get "<url>": dial tcp <addr>: connect: ...` chain, which it still wraps.
type unreachableError struct {
	host   string
	reason string
	cause  error
}

func (e *unreachableError) Error() string { return "cannot reach " + e.host + ": " + e.reason }
func (e *unreachableError) Unwrap() error { return e.cause }

// reachError rewrites a failed HTTP round trip for people. Cancellation and
// non-transport errors pass through unchanged.
func reachError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || errors.Is(err, context.Canceled) {
		return err
	}
	host := urlErr.URL
	if parsed, parseErr := url.Parse(urlErr.URL); parseErr == nil && parsed.Host != "" {
		host = parsed.Host
	}
	reason := urlErr.Err.Error()
	var dnsErr *net.DNSError
	var syscallErr *os.SyscallError
	switch {
	case errors.As(err, &dnsErr):
		reason = "cannot resolve " + dnsErr.Name
	case errors.As(err, &syscallErr):
		reason = syscallErr.Err.Error()
	case urlErr.Timeout():
		reason = "timed out"
	}
	return failure.WithNext(&unreachableError{host: host, reason: reason, cause: err}, "check the network, or use --mirror / --repo <dir>")
}

func (s Store) stageSource(ctx context.Context, source, directory string, entry Entry) (string, int64, error) {
	parsed, _ := url.Parse(source)
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		return s.stageHTTP(ctx, source, directory, entry)
	}
	if !filepath.IsAbs(source) {
		return "", 0, errors.New("local repository artifact path must be absolute")
	}
	if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
		// A local repository without the file answers like an HTTP 404.
		return "", 0, fmt.Errorf("%w: no such file %s", errSourceGone, source)
	}
	s.Progress.Report(activity.Event{
		Phase: "image-copy", Message: fmt.Sprintf("Copying image %s %s (%s) from the local repository", entry.Alias, entry.Release, entry.Arch), Source: source,
	})
	tempPath, copied, err := fsutil.CopyToTemp(source, directory, ".repository-*.partial", 0o600, MaxArtifactSize)
	if err == nil {
		s.Progress.Report(activity.Event{
			Phase: "image-copy", Message: fmt.Sprintf("Copied image %s %s (%s) from the local repository", entry.Alias, entry.Release, entry.Arch), Source: source,
			CurrentBytes: copied, TotalBytes: copied, Done: true,
		})
	}
	return tempPath, copied, err
}

func (s Store) Pull(ctx context.Context, entry Entry) (_ string, _ Metadata, returnErr error) {
	if err := s.validate(); err != nil {
		return "", Metadata{}, err
	}
	if err := validateEntryIdentity(entry); err != nil {
		return "", Metadata{}, err
	}
	if entry.Upstream != "" {
		upstream, err := url.Parse(entry.Upstream)
		if err != nil || upstream.Scheme != "https" || upstream.Host == "" || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
			return "", Metadata{}, errors.New("catalog upstream image URL must be empty or absolute HTTPS")
		}
	}
	if s.Repository == "" && entry.Upstream == "" {
		return "", Metadata{}, errors.New("catalog image has neither a repository nor an upstream source")
	}
	lockContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	imageLock, err := lock.Acquire(lockContext, s.lockPath(), false)
	if err != nil {
		return "", Metadata{}, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, imageLock, "image pull lock")
	}()
	directory, err := s.ensureImageDirectory(entry)
	if err != nil {
		return "", Metadata{}, err
	}
	if err := s.repairCachePermissions(ctx, entry); err != nil {
		if !errors.Is(err, ErrIntegrity) {
			return "", Metadata{}, err
		}
		if quarantineErr := s.quarantineCache(ctx, entry); quarantineErr != nil {
			return "", Metadata{}, errors.Join(err, quarantineErr)
		}
	}
	if target, metadata, err := s.ValidateCached(ctx, entry); err == nil {
		s.Progress.Report(activity.Event{
			Phase: "image-ready", Message: fmt.Sprintf("Using cached image %s %s (%s)", entry.Alias, entry.Release, entry.Arch), Done: true,
		})
		return target, metadata, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		if errors.Is(err, ErrIntegrity) {
			if quarantineErr := s.quarantineCache(ctx, entry); quarantineErr != nil {
				return "", Metadata{}, errors.Join(err, quarantineErr)
			}
		}
		if target, pathErr := s.Path(entry); pathErr == nil {
			if _, statErr := os.Lstat(target); statErr == nil {
				return "", Metadata{}, fmt.Errorf("%w; remove the conflicting cache file %s or run farrow image prune --yes", err, target)
			}
		}
	}
	type candidate struct {
		kind   string
		source string
	}
	candidates := make([]candidate, 0, 1)
	if s.Repository != "" {
		source, sourceErr := RepositoryArtifactSource(s.Repository, entry.File)
		if sourceErr != nil {
			return "", Metadata{}, sourceErr
		}
		candidates = append(candidates, candidate{kind: "repository", source: source})
		if fallback := officialFallback(s.Repository); fallback != "" {
			mirrorSource, sourceErr := RepositoryArtifactSource(fallback, entry.File)
			if sourceErr != nil {
				return "", Metadata{}, sourceErr
			}
			candidates = append(candidates, candidate{kind: "mirror", source: mirrorSource})
		}
	} else if entry.Upstream != "" {
		// Upstream remains a provenance field and a compatibility source for a
		// Store deliberately constructed without a repository. Normal command
		// paths resolve a repository; official repositories can fail over to their mirror.
		candidates = append(candidates, candidate{kind: "upstream", source: entry.Upstream})
	}
	failures := make([]string, 0, len(candidates))
	next := ""
	gone := len(candidates) > 0
	for index, candidate := range candidates {
		tempPath, copied, stageErr := s.stageWithRetry(ctx, candidate.source, directory, entry)
		if stageErr != nil {
			if err := ctx.Err(); err != nil {
				return "", Metadata{}, err
			}
			gone = gone && errors.Is(stageErr, errSourceGone)
			message := fmt.Sprintf("Image %s source %s failed", entry.Alias, candidate.kind)
			if index+1 < len(candidates) {
				message += "; trying the next source"
			}
			s.Progress.Report(activity.Event{Phase: "image-fallback", Message: message, Source: displayActivitySource(candidate.source)})
			failures = append(failures, candidate.kind+": "+stageErr.Error())
			if _, _, hint := failure.Classify(stageErr); next == "" {
				next = hint
			}
			continue
		}
		pathname, metadata, publishErr := s.publish(ctx, tempPath, entry, candidate.kind+":"+candidate.source, copied)
		if publishErr == nil {
			s.Progress.Report(activity.Event{
				Phase: "image-ready", Message: fmt.Sprintf("Image %s %s (%s) is ready", entry.Alias, entry.Release, entry.Arch), Done: true,
			})
			return pathname, metadata, nil
		}
		if err := ctx.Err(); err != nil {
			return "", Metadata{}, err
		}
		_ = os.Remove(tempPath)
		gone = false
		message := fmt.Sprintf("Image %s source %s failed verification: %v", entry.Alias, candidate.kind, publishErr)
		if index+1 < len(candidates) {
			message += "; trying the next source"
		}
		s.Progress.Report(activity.Event{Phase: "image-fallback", Message: message, Source: displayActivitySource(candidate.source)})
		failures = append(failures, candidate.kind+": "+publishErr.Error())
	}
	message := fmt.Errorf("download image %s %s: %s", entry.Alias, entry.Release, strings.Join(failures, "; "))
	if gone {
		if candidates[0].kind == "repository" {
			return "", Metadata{}, failure.WithNext(fmt.Errorf("%w; the selected repository does not carry this catalog artifact", message), "farrow update, choose another repository with --mirror / --repo, or farrow image import a local copy")
		}
		// A Store without a repository is a compatibility-only upstream path.
		return "", Metadata{}, failure.WithNext(fmt.Errorf("%w; the pinned artifact is no longer published upstream", message), "farrow update, point --repo at a repository that carries it, or farrow image import a local copy")
	}
	return "", Metadata{}, failure.WithNext(message, next)
}
