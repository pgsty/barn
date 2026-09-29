// Package lock implements bounded advisory file locks for Barn's fixed lock
// order: cache/global, deployment allocator, deployment, then node.
package lock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type File struct {
	mu       sync.Mutex
	handle   *os.File
	path     string
	shared   bool
	recorded bool
}

// ErrBusy reports a lock another process holds right now.
var ErrBusy = errors.New("lock is held by another process")

// Acquire waits until the lock is free or ctx ends.
func Acquire(ctx context.Context, path string, shared bool) (*File, error) {
	handle, err := openLock(path)
	if err != nil {
		return nil, err
	}
	for {
		held, err := tryLock(handle, path, shared)
		if !errors.Is(err, ErrBusy) {
			return held, err
		}
		select {
		case <-ctx.Done():
			_ = handle.Close()
			return nil, fmt.Errorf("lock %s: %w", path, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// TryAcquire takes the lock only if it is free now, and returns ErrBusy
// otherwise.
func TryAcquire(path string, shared bool) (*File, error) {
	handle, err := openLock(path)
	if err != nil {
		return nil, err
	}
	held, err := tryLock(handle, path, shared)
	if errors.Is(err, ErrBusy) {
		_ = handle.Close()
	}
	return held, err
}

func openLock(path string) (*os.File, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("lock path must be absolute")
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("lock path %s is a symlink", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}

// tryLock closes handle on every outcome except success and ErrBusy.
func tryLock(handle *os.File, path string, shared bool) (*File, error) {
	operation := unix.LOCK_EX | unix.LOCK_NB
	if shared {
		operation = unix.LOCK_SH | unix.LOCK_NB
	}
	err := unix.Flock(int(handle.Fd()), operation)
	switch {
	case err == nil:
		return &File{handle: handle, path: path, shared: shared}, nil
	case errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN):
		return nil, ErrBusy
	default:
		_ = handle.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
}

// maxOwnerBytes bounds the holder description kept in a lock file.
const maxOwnerBytes = 1024

// Record writes a short description of the exclusive holder into the lock
// file, so a waiting process can say what it waits for. Release clears it.
func (f *File) Record(owner []byte) error {
	if len(owner) > maxOwnerBytes {
		return errors.New("lock owner description is too long")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.handle == nil || f.shared {
		return errors.New("only a live exclusive lock records its owner")
	}
	if err := f.handle.Truncate(0); err != nil {
		return err
	}
	if _, err := f.handle.WriteAt(owner, 0); err != nil {
		return err
	}
	f.recorded = true
	return nil
}

// Owner returns what the current exclusive holder of path recorded. The
// kernel drops a crashed holder's lock, so a stale description is only read
// by a caller that did not first find the lock busy.
func Owner(path string) []byte {
	handle, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = handle.Close() }()
	owner, _ := io.ReadAll(io.LimitReader(handle, maxOwnerBytes))
	return owner
}

func (f *File) Path() string { return f.path }

// ValidateExclusive proves that the token still owns an unreleased exclusive
// lock for exactly path. Locked helpers use this instead of relying on a
// naming convention that callers could accidentally violate.
func (f *File) ValidateExclusive(path string) error {
	if f == nil {
		return errors.New("exclusive lock token is nil")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.handle == nil {
		return errors.New("exclusive lock token has been released")
	}
	if f.shared {
		return errors.New("lock token is shared, not exclusive")
	}
	if path == "" || f.path != path {
		return fmt.Errorf("lock token path mismatch: held %q, required %q", f.path, path)
	}
	if _, err := f.handle.Stat(); err != nil {
		return fmt.Errorf("exclusive lock token is not live: %w", err)
	}
	return nil
}

func (f *File) Release() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.handle == nil {
		return nil
	}
	if f.recorded {
		// A stale description is harmless; never let it block the unlock.
		_ = f.handle.Truncate(0)
	}
	unlockErr := unix.Flock(int(f.handle.Fd()), unix.LOCK_UN)
	closeErr := f.handle.Close()
	f.handle = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// JoinRelease preserves the operation error while making a failed unlock or
// close visible to the caller. Lock release is part of every state mutation's
// durability boundary, not best-effort cleanup.
func JoinRelease(current error, held *File, description string) error {
	if err := held.Release(); err != nil {
		return errors.Join(current, fmt.Errorf("release %s: %w", description, err))
	}
	return current
}
