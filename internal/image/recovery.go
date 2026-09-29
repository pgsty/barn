package image

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/disk"
	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/lock"
	"github.com/pgsty/barn/internal/state"
)

type sourceHTTPError struct {
	Status int
	After  string
}

func (s Store) quarantineCache(ctx context.Context, entry Entry) (returnErr error) {
	lockContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	held, err := lock.Acquire(lockContext, filepath.Join(s.DataRoot, "locks", "allocator.lock"), false)
	if err != nil {
		return err
	}
	defer func() { returnErr = lock.JoinRelease(returnErr, held, "cache recovery allocator lock") }()
	store := state.Store{Root: s.DataRoot}
	nodes, err := os.ReadDir(filepath.Join(s.DataRoot, "nodes"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, node := range nodes {
		if _, err := store.ReadTransaction(node.Name()); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cannot replace cached image while node %s has an unresolved transaction", node.Name())
		}
		recorded, err := store.ReadNode(node.Name())
		if err != nil {
			return fmt.Errorf("cannot establish image references for node %s: %w", node.Name(), err)
		}
		// Catalog metadata may have changed since an overlay was created. Match
		// the logical artifact as well as its digest before moving a backing file.
		if recorded.Image.Digest == entry.SHA256 || recorded.Image.Alias == entry.Alias && recorded.Image.Release == entry.Release {
			return fmt.Errorf("cached image is still referenced by node %s; its backing file was preserved", node.Name())
		}
		base, err := s.manager().Inspect(ctx, recorded.RootDisk)
		if err != nil {
			return fmt.Errorf("cannot establish backing file for node %s: %w", node.Name(), err)
		}
		path, err := s.Path(entry)
		if err != nil {
			return err
		}
		if base.FullBackingFilename == path {
			return fmt.Errorf("cached image is still referenced by node %s; its backing file was preserved", node.Name())
		}
	}
	path, err := s.Path(entry)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || metadata.Uid != uint32(os.Getuid()) || metadata.Nlink != 1 {
		return errors.New("cache recovery requires an owned, unshared regular file")
	}
	quarantined := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UnixNano())
	if err := os.Rename(path, quarantined); err != nil {
		return err
	}
	if err := fsutil.SyncDir(filepath.Dir(path)); err != nil {
		return err
	}
	s.Progress.Report(activity.Event{Phase: "image-recovery", Message: "Preserved damaged cache; fetching a verified copy"})
	return nil
}

func (err *sourceHTTPError) Error() string {
	return fmt.Sprintf("download returned HTTP %d %s", err.Status, http.StatusText(err.Status))
}

func retrySource(err error) bool {
	var response *sourceHTTPError
	if errors.As(err, &response) {
		switch response.Status {
		case 408, 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	var connection *net.OpError
	var dns *net.DNSError
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.Is(err, errImageStalled) || errors.As(err, &connection) || errors.As(err, &dns) && (dns.IsTimeout || dns.IsTemporary)
}

func retryDelay(err error, attempt int) time.Duration {
	delay := time.Duration(1<<attempt) * 500 * time.Millisecond
	var response *sourceHTTPError
	if errors.As(err, &response) {
		if seconds, parseErr := strconv.Atoi(response.After); parseErr == nil && seconds >= 0 {
			// A longer server cooldown is handled by trying the next source.
			delay = max(delay, time.Duration(min(seconds, 31))*time.Second)
		} else if date, parseErr := http.ParseTime(response.After); parseErr == nil {
			delay = max(delay, time.Until(date))
		}
	}
	return delay
}

func officialFallback(repository string) string {
	switch strings.TrimSuffix(repository, "/") {
	case strings.TrimSuffix(DefaultRepositoryURL, "/"):
		return MirrorRepositoryURL
	case strings.TrimSuffix(MirrorRepositoryURL, "/"):
		return DefaultRepositoryURL
	default:
		return ""
	}
}

func (s Store) stageWithRetry(ctx context.Context, source, directory string, entry Entry) (string, int64, error) {
	for attempt := 0; ; attempt++ {
		path, received, err := s.stageSource(ctx, source, directory, entry)
		if err == nil {
			return path, received, nil
		}
		if ctx.Err() != nil {
			return "", received, ctx.Err()
		}
		if attempt >= 2 || !retrySource(err) {
			return path, received, err
		}
		delay := retryDelay(err, attempt)
		if delay > 30*time.Second {
			return path, received, err
		}
		s.Progress.Report(activity.Event{Phase: "image-retry", Message: fmt.Sprintf("Retrying image %s, attempt %d of 3", entry.Alias, attempt+2), Source: displayActivitySource(source)})
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", received, ctx.Err()
		case <-timer.C:
		}
	}
}

// Only fix permissions after proving the bytes and the owned file descriptor.
// Corrupt or shared backing files must never be made usable by skipping checks.
func (s Store) repairCachePermissions(ctx context.Context, entry Entry) error {
	path, err := s.Path(entry)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o222 == 0 {
		return nil
	}
	handle, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()
	opened, err := handle.Stat()
	if err != nil {
		return err
	}
	metadata, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || metadata.Uid != uint32(os.Getuid()) || metadata.Nlink != 1 || !os.SameFile(info, opened) {
		return errors.New("cache permission repair requires an owned, unshared regular file")
	}
	hash := sha256.New()
	size, err := copyWithProgress(hash, handle, MaxArtifactSize+1, s.Progress, activity.Event{Phase: "image-verify", Message: "Checking cache permissions and image bytes", TotalBytes: opened.Size()})
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != entry.SHA256 || entry.ArtifactSize > 0 && size != entry.ArtifactSize {
		return fmt.Errorf("%w: cached image bytes differ from the catalog", ErrIntegrity)
	}
	imageInfo, err := s.manager().Inspect(ctx, path)
	if err != nil {
		return err
	}
	if err := disk.ValidateBase(imageInfo); err != nil {
		return err
	}
	if entry.VirtualSize > 0 && imageInfo.VirtualSize != entry.VirtualSize {
		return fmt.Errorf("%w: cached virtual size differs from the catalog", ErrIntegrity)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, current) {
		return errors.New("cached image changed during permission repair")
	}
	return handle.Chmod(0o444)
}
