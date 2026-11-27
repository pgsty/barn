package fsutil

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// EnsurePrivateDir creates path with mode, verifies it is a real directory not
// writable by group or others, and normalizes its permission bits.
func EnsurePrivateDir(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("required path is not a real directory: %s", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("required directory is group/world writable: %s mode=%o", path, info.Mode().Perm())
	}
	return os.Chmod(path, mode)
}

// NoSymlinks walks upward from path and rejects any existing symlink component.
func NoSymlinks(path string) error {
	for current := filepath.Clean(path); current != "/" && current != "."; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("state path is a symlink: %s", current)
		}
	}
	return nil
}

// ReadBoundedRegular reads a regular non-symlink file capped at maxBytes (when > 0).
func ReadBoundedRegular(path string, maxBytes int64) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("path must be a regular non-symlink file: %s", path)
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		return nil, nil, fmt.Errorf("file size %d exceeds limit %d: %s", info.Size(), maxBytes, path)
	}
	handle, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = handle.Close() }()
	reader := io.Reader(handle)
	if maxBytes > 0 {
		reader = io.LimitReader(handle, maxBytes+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, err
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, nil, fmt.Errorf("file exceeds %d-byte limit: %s", maxBytes, path)
	}
	return data, info, nil
}

// SHA256Bytes returns the lowercase hexadecimal SHA-256 digest of data.
func SHA256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SHA256File hashes a regular non-symlink file up to maxBytes (when > 0).
func SHA256File(path string, maxBytes int64) (string, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("path must be a regular non-symlink file: %s", path)
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		return "", 0, fmt.Errorf("file size %d exceeds limit %d: %s", info.Size(), maxBytes, path)
	}
	handle, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = handle.Close() }()
	limit := info.Size() + 1
	if maxBytes > 0 && maxBytes+1 < limit {
		limit = maxBytes + 1
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(handle, limit))
	if err != nil {
		return "", written, err
	}
	if written != info.Size() {
		return "", written, fmt.Errorf("file size changed while hashing: %s", path)
	}
	return hex.EncodeToString(hash.Sum(nil)), written, nil
}

// AvailableBytes returns the unprivileged available bytes and existing probe
// directory for the filesystem that contains pathname.
func AvailableBytes(pathname string) (uint64, string, error) {
	probe := pathname
	for {
		info, err := os.Lstat(probe)
		if err == nil {
			if !info.IsDir() {
				probe = filepath.Dir(probe)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return 0, "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return 0, "", err
		}
		probe = parent
	}
	var statistics unix.Statfs_t
	if err := unix.Statfs(probe, &statistics); err != nil {
		return 0, "", err
	}
	return uint64(statistics.Bavail) * uint64(statistics.Bsize), probe, nil
}

// ShellQuote quotes argument for POSIX shells, leaving safe unquoted tokens bare.
func ShellQuote(argument string) string {
	if argument == "" {
		return "''"
	}
	for _, character := range argument {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("_@%+=:,./-", character) {
			continue
		}
		return "'" + strings.ReplaceAll(argument, "'", "'\\''") + "'"
	}
	return argument
}
