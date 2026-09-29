package macvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/fsutil"
	"github.com/pgsty/farrow/internal/lock"
)

type IPSWSpec struct {
	Version   string `json:"version"`
	Build     string `json:"build"`
	URL       string `json:"url"`
	SizeBytes int64  `json:"size_bytes"`
	// SHA256 may come from independently pinned Apple HTTPS metadata. When
	// absent, a local digest detects later corruption but proves no provenance.
	SHA256 string `json:"sha256,omitempty"`
}

func DefaultIPSW() IPSWSpec {
	return IPSWSpec{Version: "27.0", Build: "26A428", URL: "https://updates.cdn-apple.com/2026FallFCS/afcfc88e-bbe6-44bf-a5da-07c56eebc06c/UniversalMac_27.0_26A428_Restore.ipsw", SizeBytes: 26626436228, SHA256: "2a5d3c695d501022b7fad9adaffcf2627bcb867d993fb5662dcd41bac99a2836"}
}

type DownloadOptions struct {
	Client   *http.Client
	Progress func(downloaded, total int64)
	// AllowHTTP supports local test fixtures. Product downloads require HTTPS.
	AllowHTTP bool
}

type Installer struct {
	SchemaVersion     int       `json:"schema_version"`
	Version           string    `json:"version"`
	Build             string    `json:"build"`
	URL               string    `json:"url"`
	SizeBytes         int64     `json:"size_bytes"`
	SHA256            string    `json:"sha256"`
	ETag              string    `json:"etag,omitempty"`
	LastModified      string    `json:"last_modified,omitempty"`
	Path              string    `json:"path"`
	CreatedAt         time.Time `json:"created_at"`
	VerificationError string    `json:"verification_error,omitempty"`
}

type partialDownload struct {
	SchemaVersion int    `json:"schema_version"`
	URL           string `json:"url"`
	SizeBytes     int64  `json:"size_bytes"`
	ETag          string `json:"etag,omitempty"`
	LastModified  string `json:"last_modified,omitempty"`
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func (spec IPSWSpec) validate(allowHTTP bool) error {
	if err := spec.validateIdentity(); err != nil {
		return err
	}
	u, err := url.Parse(spec.URL)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) {
		return errors.New("IPSW download URL must use HTTPS without embedded credentials")
	}
	if !allowHTTP && !appleDownloadHost(u.Hostname()) {
		return errors.New("IPSW downloads must use an Apple HTTPS host")
	}
	return nil
}

func appleDownloadHost(host string) bool {
	host = strings.ToLower(host)
	return host == "apple.com" || strings.HasSuffix(host, ".apple.com") || strings.HasSuffix(host, ".cdn-apple.com")
}

func (spec IPSWSpec) validateIdentity() error {
	if !safeID(spec.Build) || spec.Version == "" || spec.SizeBytes <= 0 {
		return errors.New("IPSW requires a version, safe build ID and positive expected size")
	}
	if spec.SHA256 != "" && !validSHA256(spec.SHA256) {
		return errors.New("invalid expected IPSW SHA256")
	}
	return nil
}

func (s *Store) installerPaths(build string) (final, partial string, err error) {
	if !safeID(build) {
		return "", "", errors.New("invalid installer build")
	}
	final, err = s.Path("images", "ipsw", build+".ipsw")
	if err != nil {
		return "", "", err
	}
	partial, err = s.Path("images", "ipsw", build+".ipsw.partial")
	if err != nil {
		return "", "", err
	}
	for _, path := range []string{final + ".json", partial + ".json"} {
		if err := noSymlinks(path); err != nil {
			return "", "", err
		}
	}
	return final, partial, nil
}

func (s *Store) lockInstaller(ctx context.Context, build string) (*lock.File, error) {
	if _, err := s.mkdir("runtime"); err != nil {
		return nil, err
	}
	if _, err := s.mkdir("images", "ipsw"); err != nil {
		return nil, err
	}
	path, err := s.imageLockPath("installer", build)
	if err != nil {
		return nil, err
	}
	return lock.Acquire(ctx, path, false)
}

// DownloadIPSW validates every cached file against its saved SHA256, resumes
// only matching HTTP representations, and leaves interrupted data resumable.
// A successful result still requires Apple restore-image API validation.
func (s *Store) DownloadIPSW(ctx context.Context, spec IPSWSpec, options DownloadOptions) (installer *Installer, retErr error) {
	if err := spec.validate(options.AllowHTTP); err != nil {
		return nil, err
	}
	held, err := s.lockInstaller(ctx, spec.Build)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac installer") }()
	final, partial, err := s.installerPaths(spec.Build)
	if err != nil {
		return nil, err
	}
	if cached, err := cachedInstaller(ctx, final, spec); err == nil {
		if options.Progress != nil {
			options.Progress(spec.SizeBytes, spec.SizeBytes)
		}
		return cached, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var meta partialDownload
	offset, err := partialSize(partial)
	if err != nil {
		return nil, err
	}
	if err := readJSON(partial+".json", &meta); err != nil || meta.SchemaVersion != imageSchemaVersion || meta.URL != spec.URL || meta.SizeBytes != spec.SizeBytes || (meta.ETag == "" && meta.LastModified == "") || offset > spec.SizeBytes {
		offset = 0
		meta = partialDownload{SchemaVersion: imageSchemaVersion, URL: spec.URL, SizeBytes: spec.SizeBytes}
	}
	if offset == spec.SizeBytes {
		return finishInstaller(ctx, final, partial, spec, meta)
	}
	client := options.Client
	if client == nil {
		client = http.DefaultClient
	}
	// Clone the client to enforce TLS on redirects without modifying a shared
	// caller client. The transport's TLS certificate validation remains intact.
	copyClient := *client
	previousRedirect := client.CheckRedirect
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" && !(options.AllowHTTP && req.URL.Scheme == "http") {
			return errors.New("IPSW redirect must use HTTPS")
		}
		if !options.AllowHTTP && !appleDownloadHost(req.URL.Hostname()) {
			return errors.New("IPSW redirect must stay on an Apple HTTPS host")
		}
		if previousRedirect != nil {
			return previousRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("too many IPSW redirects")
		}
		return nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.URL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept-Encoding", "identity")
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
			if meta.ETag != "" {
				req.Header.Set("If-Range", meta.ETag)
			} else {
				req.Header.Set("If-Range", meta.LastModified)
			}
		}
		response, err := copyClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("download IPSW: %w", err)
		}
		if response.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset > 0 {
			_ = response.Body.Close()
			offset = 0
			continue // a stale/incompatible partial must never become ready
		}
		if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
			_ = response.Body.Close()
			return nil, fmt.Errorf("download IPSW: HTTP %d", response.StatusCode)
		}
		if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
			_ = response.Body.Close()
			return nil, errors.New("IPSW server returned encoded bytes; cannot safely resume")
		}
		if response.StatusCode == http.StatusOK {
			offset = 0 // server ignored Range or representation changed: truncate
		} else {
			start, end, total, err := parseContentRange(response.Header.Get("Content-Range"))
			if err != nil || start != offset || total != spec.SizeBytes || end != total-1 {
				_ = response.Body.Close()
				return nil, fmt.Errorf("IPSW Content-Range does not match requested offset %d and size %d", offset, spec.SizeBytes)
			}
			if offset > 0 && representationChanged(meta, response) {
				_ = response.Body.Close()
				offset = 0
				continue
			}
		}
		if response.ContentLength >= 0 && response.ContentLength != spec.SizeBytes-offset {
			_ = response.Body.Close()
			return nil, fmt.Errorf("IPSW response length %d differs from expected %d", response.ContentLength, spec.SizeBytes-offset)
		}
		meta = partialDownload{SchemaVersion: imageSchemaVersion, URL: spec.URL, SizeBytes: spec.SizeBytes, ETag: strongETag(response.Header.Get("ETag")), LastModified: response.Header.Get("Last-Modified")}
		if _, err := http.ParseTime(meta.LastModified); err != nil {
			meta.LastModified = ""
		}
		flags := os.O_CREATE | os.O_WRONLY
		if offset == 0 {
			flags |= os.O_TRUNC
		}
		out, err := os.OpenFile(partial, flags, 0o600)
		if err != nil {
			_ = response.Body.Close()
			return nil, err
		}
		if _, err := out.Seek(offset, io.SeekStart); err != nil {
			_ = out.Close()
			_ = response.Body.Close()
			return nil, err
		}
		if err := writeJSON(partial+".json", &meta); err != nil {
			_ = out.Close()
			_ = response.Body.Close()
			return nil, err
		}
		progress := &progressReader{reader: response.Body, ctx: ctx, total: spec.SizeBytes, done: offset, callback: options.Progress}
		if options.Progress != nil {
			options.Progress(offset, spec.SizeBytes)
		}
		written, copyErr := io.Copy(out, io.LimitReader(progress, spec.SizeBytes-offset+1))
		syncErr := out.Sync()
		closeErr := out.Close()
		bodyErr := response.Body.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil || bodyErr != nil {
			return nil, errors.Join(copyErr, syncErr, closeErr, bodyErr)
		}
		if written != spec.SizeBytes-offset {
			return nil, fmt.Errorf("IPSW download incomplete: received %d of %d bytes", offset+written, spec.SizeBytes)
		}
		return finishInstaller(ctx, final, partial, spec, meta)
	}
	return nil, errors.New("IPSW server changed or rejected the partial representation repeatedly; retry the download")
}

func representationChanged(meta partialDownload, response *http.Response) bool {
	if meta.ETag != "" {
		return strongETag(response.Header.Get("ETag")) != meta.ETag
	}
	return response.Header.Get("Last-Modified") != meta.LastModified
}

func strongETag(value string) string {
	if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") && len(value) > 1 {
		return value
	}
	return ""
}

func partialSize(path string) (int64, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("IPSW cache must be a regular file")
	}
	return info.Size(), nil
}

func parseContentRange(value string) (start, end, total int64, err error) {
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, errors.New("invalid Content-Range unit")
	}
	span, totalText, ok := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
	if !ok {
		return 0, 0, 0, errors.New("missing Content-Range total")
	}
	startText, endText, ok := strings.Cut(span, "-")
	if !ok {
		return 0, 0, 0, errors.New("missing Content-Range end")
	}
	start, err = strconv.ParseInt(startText, 10, 64)
	if err != nil {
		return
	}
	end, err = strconv.ParseInt(endText, 10, 64)
	if err != nil {
		return
	}
	total, err = strconv.ParseInt(totalText, 10, 64)
	if err == nil && (start < 0 || end < start || total <= end) {
		err = errors.New("invalid Content-Range bounds")
	}
	return
}

type progressReader struct {
	reader      io.Reader
	ctx         context.Context
	done, total int64
	callback    func(int64, int64)
}

func (r *progressReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	r.done += int64(n)
	if r.callback != nil && n > 0 {
		r.callback(r.done, r.total)
	}
	return n, err
}

func hashFile(ctx context.Context, path string, expectedSize int64) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() != expectedSize {
		return "", fmt.Errorf("IPSW file size/type mismatch: expected regular file of %d bytes", expectedSize)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, &progressReader{reader: f, ctx: ctx})
	if err != nil {
		return "", err
	}
	if n != expectedSize {
		return "", errors.New("IPSW file changed while hashing")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func cachedInstaller(ctx context.Context, path string, spec IPSWSpec) (*Installer, error) {
	if _, err := os.Lstat(path); err != nil {
		return nil, err
	}
	var cached Installer
	err := readJSON(path+".json", &cached)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) || spec.SHA256 == "" {
			return nil, fmt.Errorf("cached IPSW metadata unavailable; inspect with farrow mac image ls, then remove damaged installers with farrow mac image prune --installers --yes: %v", err)
		}
		// Recover a crash between publishing the complete file and metadata only
		// when there is an independently expected digest, never merely a length.
		cached = Installer{SchemaVersion: imageSchemaVersion, Version: spec.Version, Build: spec.Build, URL: spec.URL, SizeBytes: spec.SizeBytes, SHA256: strings.ToLower(spec.SHA256), Path: path, CreatedAt: time.Now().UTC()}
	} else if cached.SchemaVersion != imageSchemaVersion || cached.Build != spec.Build || cached.Version != spec.Version || cached.SizeBytes != spec.SizeBytes || !validSHA256(cached.SHA256) {
		return nil, errors.New("cached IPSW metadata differs from the requested image; inspect with farrow mac image ls, then run farrow mac image prune --installers --yes before retrying")
	}
	digest, err := hashFile(ctx, path, spec.SizeBytes)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			cached.VerificationError = err.Error()
			_ = writeJSON(path+".json", &cached)
		}
		return nil, err
	}
	if digest != strings.ToLower(cached.SHA256) || spec.SHA256 != "" && digest != strings.ToLower(spec.SHA256) {
		cached.VerificationError = "cached IPSW SHA256 mismatch"
		_ = writeJSON(path+".json", &cached)
		return nil, failure.New(failure.Integrity, errors.New("the cached macOS restore image does not match its recorded SHA-256")).Then("farrow mac image prune --installers --yes, then retry")
	}
	cached.VerificationError = ""
	cached.Path = path
	if err := writeJSON(path+".json", &cached); err != nil {
		return nil, err
	}
	return &cached, nil
}

func finishInstaller(ctx context.Context, final, partial string, spec IPSWSpec, meta partialDownload) (*Installer, error) {
	digest, err := hashFile(ctx, partial, spec.SizeBytes)
	if err != nil {
		return nil, err
	}
	if spec.SHA256 != "" && digest != strings.ToLower(spec.SHA256) {
		return nil, failure.New(failure.Integrity, errors.New("the macOS restore image does not match Apple's published SHA-256; the file was kept for inspection")).Then("farrow mac image prune --installers --yes, then retry")
	}
	installer := &Installer{SchemaVersion: imageSchemaVersion, Version: spec.Version, Build: spec.Build, URL: spec.URL, SizeBytes: spec.SizeBytes, SHA256: digest, ETag: meta.ETag, LastModified: meta.LastModified, Path: final, CreatedAt: time.Now().UTC()}
	if err := os.Rename(partial, final); err != nil {
		return nil, err
	}
	if err := fsutil.SyncDir(filepath.Dir(final)); err != nil {
		return nil, err
	}
	if err := writeJSON(final+".json", installer); err != nil {
		return nil, err
	}
	if err := os.Remove(partial + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return installer, nil
}

// CachedInstaller returns the verified cached installer of spec, or
// os.ErrNotExist when none is cached. It never downloads.
func (s *Store) CachedInstaller(ctx context.Context, spec IPSWSpec) (installer *Installer, retErr error) {
	if err := spec.validateIdentity(); err != nil {
		return nil, err
	}
	final, _, err := s.installerPaths(spec.Build)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(final); err != nil {
		return nil, err
	}
	held, err := s.lockInstaller(ctx, spec.Build)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac installer") }()
	return cachedInstaller(ctx, final, spec)
}

// PartialBytes reports how much of spec an interrupted download already holds.
func (s *Store) PartialBytes(spec IPSWSpec) (int64, error) {
	_, partial, err := s.installerPaths(spec.Build)
	if err != nil {
		return 0, err
	}
	size, err := partialSize(partial)
	if err != nil || size > spec.SizeBytes {
		return 0, err
	}
	var meta partialDownload
	if readJSON(partial+".json", &meta) != nil || meta.URL != spec.URL || meta.SizeBytes != spec.SizeBytes {
		return 0, nil
	}
	return size, nil
}

// ImportIPSW brings a caller-owned IPSW in without modifying it. On the same
// APFS volume the cache gets a clone that shares its blocks; elsewhere the
// file is verified and used in place, never copied. The caller must first
// read the build and size through Apple's restore-image API.
func (s *Store) ImportIPSW(ctx context.Context, source string, spec IPSWSpec, options DownloadOptions) (installer *Installer, retErr error) {
	if err := spec.validateIdentity(); err != nil {
		return nil, err
	}
	held, err := s.lockInstaller(ctx, spec.Build)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = lock.JoinRelease(retErr, held, "mac installer import") }()
	final, partial, err := s.installerPaths(spec.Build)
	if err != nil {
		return nil, err
	}
	if cached, err := cachedInstaller(ctx, final, spec); err == nil {
		return cached, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	absSource, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(absSource); err == nil {
		absSource = resolved
	}
	if absSource == partial || absSource == final {
		return nil, errors.New("import source must be outside the managed installer paths")
	}
	info, err := os.Lstat(absSource)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != spec.SizeBytes {
		return nil, errors.New("imported IPSW must be a regular file matching the expected size")
	}
	temp := filepath.Join(filepath.Dir(final), fmt.Sprintf(".import-%d.partial", os.Getpid()))
	_ = os.Remove(temp)
	if err := cloneFile(absSource, temp); err != nil {
		// Another volume: verify and use the file where it is.
		if options.Progress != nil {
			options.Progress(spec.SizeBytes, spec.SizeBytes)
		}
		digest, err := hashFile(ctx, absSource, spec.SizeBytes)
		if err != nil {
			return nil, err
		}
		if spec.SHA256 != "" && digest != strings.ToLower(spec.SHA256) {
			return nil, failure.New(failure.Integrity, errors.New("the local IPSW does not match Apple's published SHA-256")).Then("download the restore image from Apple again, or omit --ipsw")
		}
		return &Installer{SchemaVersion: imageSchemaVersion, Version: spec.Version, Build: spec.Build, SizeBytes: spec.SizeBytes, SHA256: digest, Path: absSource, CreatedAt: time.Now().UTC()}, nil
	}
	defer func() { _ = os.Remove(temp) }()
	if err := os.Chmod(temp, 0o600); err != nil {
		return nil, err
	}
	if options.Progress != nil {
		options.Progress(spec.SizeBytes, spec.SizeBytes)
	}
	// Reuse final verification while preserving any interrupted download.
	return finishInstaller(ctx, final, temp, spec, partialDownload{SchemaVersion: imageSchemaVersion, URL: spec.URL, SizeBytes: spec.SizeBytes})
}
