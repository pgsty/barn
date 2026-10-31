// Package runtimepath owns short, user-scoped QMP and pidfile directories.
// Unix socket paths are limited to ~104 bytes on macOS, so runtime state
// lives under XDG_RUNTIME_DIR or a short UID-scoped /tmp root instead of the
// data root.
package runtimepath

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/naming"
)

func barnScope() string {
	scope := os.Getenv("BARN_HOME")
	if scope == "" {
		if home, err := os.UserHomeDir(); err == nil {
			scope = filepath.Join(home, ".barn")
		}
	}
	if expanded, err := fsutil.UserPath(scope, ""); err == nil {
		scope = expanded
	} else if absolute, err := filepath.Abs(scope); err == nil {
		scope = absolute
	}
	if canonical, err := fsutil.CanonicalPath(scope); err == nil {
		scope = canonical
	} else if absolute, err := filepath.Abs(scope); err == nil {
		scope = absolute
	}
	return scope
}

const scopeHashDigits = 11

func compactBase(root string) string {
	digest := sha256.Sum256([]byte(barnScope()))
	return filepath.Join(root, "b"+fmt.Sprintf("%x", digest[:scopeHashDigits/2+scopeHashDigits%2])[:scopeHashDigits])
}

// scopedBase is the short namespace used below XDG_RUNTIME_DIR. Keep the uid
// argument for callers/tests from the earlier scoped layout; ownership of the
// validated XDG root already supplies the user boundary.
func scopedBase(root string, _ int) string {
	return compactBase(root)
}

func fallbackRoot() string {
	if runtime.GOOS == "darwin" {
		return "/private/tmp"
	}
	return "/tmp"
}

func fallbackBase(uid int) string {
	_ = uid
	return compactBase(fallbackRoot())
}

func xdgBase(uid int) (string, error) {
	canonical, err := xdgRoot(uid)
	if err != nil {
		return "", err
	}
	return scopedBase(canonical, uid), nil
}

func xdgRoot(uid int) (string, error) {
	configured := os.Getenv("XDG_RUNTIME_DIR")
	if configured == "" {
		return "", os.ErrNotExist
	}
	if !filepath.IsAbs(configured) {
		return "", errors.New("XDG_RUNTIME_DIR must be an absolute path")
	}
	configured = filepath.Clean(configured)
	if err := validateOwnedDirectory(configured, uid); err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(configured)
	if err != nil {
		return "", err
	}
	return canonical, nil
}

func managedBase(path string, uid int) bool {
	if isManagedFallbackBase(path, uid) {
		return true
	}
	root, err := xdgRoot(uid)
	if err != nil {
		return false
	}
	// Existing state may have been planned with a different BARN_HOME. Any
	// previously generated namespace directly below the validated XDG root is
	// still safe to reuse; it is identified by the UID and fixed hash shape.
	return path == root || isScopedXDGBase(path, root, uid)
}

func prunableBase(path string, uid int) bool {
	if isManagedFallbackBase(path, uid) {
		return true
	}
	root, err := xdgRoot(uid)
	return err == nil && isScopedXDGBase(path, root, uid)
}

func isScopedXDGBase(path, root string, uid int) bool {
	if filepath.Dir(path) != root {
		return false
	}
	name := filepath.Base(path)
	if len(name) == 1+scopeHashDigits && name[0] == 'b' && isLowerHex(name[1:]) {
		return true
	}
	prefix := fmt.Sprintf("barn-%d-", uid)
	suffix := strings.TrimPrefix(name, prefix)
	if suffix == filepath.Base(path) || len(suffix) != 16 {
		return false
	}
	return isLowerHex(suffix)
}

func isLowerHex(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func isManagedFallbackBase(path string, uid int) bool {
	if filepath.Dir(path) != fallbackRoot() {
		return false
	}
	name := filepath.Base(path)
	if name == "barn-"+fmt.Sprint(uid) {
		return true
	}
	if name == filepath.Base(fallbackBase(uid)) {
		return true
	}
	longPrefix := fmt.Sprintf("barn-%d-", uid)
	if suffix := strings.TrimPrefix(name, longPrefix); suffix != name && len(suffix) == 16 && isLowerHex(suffix) {
		return true
	}
	return false
}

func ownerUID(info os.FileInfo) (int, bool) {
	value, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(value.Uid), true
}

func validateOwnedDirectory(path string, uid int) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	owner, ok := ownerUID(info)
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !ok || owner != uid {
		return fmt.Errorf("runtime parent must be an owner-%d mode-0700 directory: %s", uid, path)
	}
	return nil
}

func selectedBase(uid int) (string, error) {
	if uid < 0 {
		return "", errors.New("runtime UID is invalid")
	}
	if os.Getenv("XDG_RUNTIME_DIR") != "" {
		return xdgBase(uid)
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
// <scoped-base>/barn/<node>, where scoped-base is isolated by BARN_HOME.
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
	allowBaseCreate := managedBase(base, uid)
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
	if prunableBase(base, uid) {
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
