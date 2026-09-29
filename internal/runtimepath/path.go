// Package runtimepath owns short, user-scoped QMP and pidfile directories.
// Unix socket paths are limited to ~104 bytes on macOS, so runtime state
// lives under XDG_RUNTIME_DIR or a short UID-scoped /tmp root instead of the
// data root.
package runtimepath

import (
	"errors"
	"fmt"
	"github.com/pgsty/barn/internal/naming"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

func fallbackBase(uid int) string {
	root := "/tmp"
	if runtime.GOOS == "darwin" {
		root = "/private/tmp"
	}
	return filepath.Join(root, fmt.Sprintf("barn-%d", uid))
}

func ownerUID(info os.FileInfo) (int, bool) {
	value, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(value.Uid), true
}

func validateOwnedDirectory(path string, uid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, ok := ownerUID(info)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || !ok || owner != uid {
		return fmt.Errorf("runtime parent must be a real owner-%d mode-0700 directory: %s", uid, path)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return fmt.Errorf("runtime parent must have a canonical non-symlink path: %s", path)
	}
	return nil
}

func selectedBase(uid int) (string, error) {
	if uid < 0 {
		return "", errors.New("runtime UID is invalid")
	}
	if configured := os.Getenv("XDG_RUNTIME_DIR"); configured != "" {
		if !filepath.IsAbs(configured) || filepath.Clean(configured) != configured {
			return "", errors.New("XDG_RUNTIME_DIR must be a clean absolute path")
		}
		if err := validateOwnedDirectory(configured, uid); err != nil {
			return "", err
		}
		return configured, nil
	}
	return fallbackBase(uid), nil
}

func maxSocketPath() int {
	if runtime.GOOS == "darwin" {
		return 103
	}
	return 107
}

// Directory returns the node's runtime location without creating it:
// <base>/barn/<node>, where base is XDG_RUNTIME_DIR or the UID /tmp root.
func Directory(node string, uid int) (string, error) {
	base, err := selectedBase(uid)
	if err != nil {
		return "", err
	}
	directory := filepath.Join(base, "barn", node)
	if err := Validate(directory, node, uid); err != nil {
		return "", err
	}
	return directory, nil
}

// Validate proves a persisted runtime path belongs to the node and keeps its
// QMP socket under the platform path limit.
func Validate(directory, node string, uid int) error {
	if !naming.ValidNodeName(node) || uid < 0 || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("runtime directory identity is invalid")
	}
	barnDir := filepath.Dir(directory)
	base := filepath.Dir(barnDir)
	if filepath.Base(directory) != node || filepath.Base(barnDir) != "barn" || base == "/" || base == "." {
		return errors.New("runtime directory does not match the node identity")
	}
	if len(filepath.Join(directory, "qmp.sock")) > maxSocketPath() {
		return errors.New("runtime QMP socket path exceeds the platform limit")
	}
	return nil
}

func ensureOne(path string, uid int, allowCreate bool) error {
	if err := validateOwnedDirectory(path, uid); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) || !allowCreate {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return validateOwnedDirectory(path, uid)
}

// Ensure creates only the validated, user-owned runtime chain.
func Ensure(directory string, uid int) error {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("runtime directory must be a clean absolute path")
	}
	nodeDir := directory
	barnDir := filepath.Dir(nodeDir)
	base := filepath.Dir(barnDir)
	if filepath.Base(barnDir) != "barn" || !naming.ValidNodeName(filepath.Base(nodeDir)) {
		return errors.New("runtime directory has an invalid managed layout")
	}
	allowBaseCreate := base == fallbackBase(uid)
	if err := ensureOne(base, uid, allowBaseCreate); err != nil {
		return err
	}
	for _, path := range []string{barnDir, nodeDir} {
		if err := ensureOne(path, uid, true); err != nil {
			return err
		}
	}
	return nil
}

// PruneEmptyParents removes only empty managed parents after the node runtime
// directory has gone. It never removes XDG_RUNTIME_DIR itself.
func PruneEmptyParents(directory string, uid int) error {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("runtime directory must be a clean absolute path")
	}
	barnDir := filepath.Dir(directory)
	base := filepath.Dir(barnDir)
	if filepath.Base(barnDir) != "barn" || !naming.ValidNodeName(filepath.Base(directory)) {
		return errors.New("runtime directory has an invalid managed layout")
	}
	candidates := []string{barnDir}
	if base == fallbackBase(uid) {
		candidates = append(candidates, base)
	}
	for _, path := range candidates {
		if err := validateOwnedDirectory(path, uid); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := os.Remove(path); errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
