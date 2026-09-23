// Package fsutil provides the small, audited filesystem primitives shared by
// configuration, state, and image stores.
package fsutil

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// AtomicWrite writes a same-directory temporary file, fsyncs it, renames it,
// and fsyncs the parent. Existing symlink targets are rejected.
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("atomic write target must be absolute")
	}
	if mode.Perm() != mode {
		return fmt.Errorf("atomic write mode contains non-permission bits: %v", mode)
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("will not overwrite %s: it is a symlink", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat atomic target: %w", err)
	}
	parent := filepath.Dir(path)
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("atomic target parent is not a real directory: %s", parent)
	}
	temp, err := os.CreateTemp(parent, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create atomic temporary file: %w", err)
	}
	tempPath := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod atomic temporary file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write atomic temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync atomic temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close atomic temporary file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish atomic file: %w", err)
	}
	keep = true
	return SyncDir(parent)
}

// AtomicCreate publishes a complete file only if path is still absent. The
// same-directory hard-link step is atomic and fails with os.ErrExist instead
// of replacing a file created by another process while the caller was doing
// preparatory work.
func AtomicCreate(path string, data []byte, mode os.FileMode) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("atomic create target must be absolute")
	}
	if mode.Perm() != mode {
		return fmt.Errorf("atomic create mode contains non-permission bits: %v", mode)
	}
	parent := filepath.Dir(path)
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("atomic create parent is not a real directory: %s", parent)
	}
	temp, err := os.CreateTemp(parent, "."+filepath.Base(path)+".create-")
	if err != nil {
		return fmt.Errorf("create atomic temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if err := temp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod atomic temporary file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write atomic temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync atomic temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close atomic temporary file: %w", err)
	}
	if err := os.Link(tempPath, path); err != nil {
		return fmt.Errorf("publish new atomic file without replacement: %w", err)
	}
	if err := os.Remove(tempPath); err != nil {
		return fmt.Errorf("remove atomic create staging link: %w", err)
	}
	return SyncDir(parent)
}

// CopyToTemp copies source into a new target-directory temporary file. The
// caller owns publishing or removing the returned path.
func CopyToTemp(source, targetDir, pattern string, mode os.FileMode, maxBytes int64) (string, int64, error) {
	inputInfo, err := os.Lstat(source)
	if err != nil || !inputInfo.Mode().IsRegular() {
		return "", 0, fmt.Errorf("copy source must be a regular non-symlink file: %s", source)
	}
	if maxBytes > 0 && inputInfo.Size() > maxBytes {
		return "", 0, fmt.Errorf("copy source size %d exceeds limit %d", inputInfo.Size(), maxBytes)
	}
	input, err := os.Open(source)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		// The source is read-only. Target Write, Sync, and Close errors remain
		// mandatory below because they determine whether bytes can be published.
		_ = input.Close()
	}()
	output, err := os.CreateTemp(targetDir, pattern)
	if err != nil {
		return "", 0, err
	}
	tempPath := output.Name()
	ok := false
	defer func() {
		_ = output.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()
	if err := output.Chmod(mode); err != nil {
		return "", 0, err
	}
	reader := io.Reader(input)
	if maxBytes > 0 {
		reader = io.LimitReader(input, maxBytes+1)
	}
	written, err := io.Copy(output, reader)
	if err != nil {
		return "", written, err
	}
	if maxBytes > 0 && written > maxBytes {
		return "", written, fmt.Errorf("copied bytes %d exceed limit %d", written, maxBytes)
	}
	if err := output.Sync(); err != nil {
		return "", written, err
	}
	if err := output.Close(); err != nil {
		return "", written, err
	}
	ok = true
	return tempPath, written, nil
}

func SyncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

// IsWithin checks lexical containment after making both paths absolute. It
// does not authorize deletion or replace per-component symlink checks.
func IsWithin(root, target string) (bool, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	relative, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return false, err
	}
	return relative != ".." && !filepath.IsAbs(relative) && relative != "." && !startsWithParent(relative), nil
}

func startsWithParent(relative string) bool {
	separator := string(filepath.Separator)
	return len(relative) >= 3 && relative[:3] == ".."+separator
}
