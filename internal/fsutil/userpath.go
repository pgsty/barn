package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// UserPath interprets a user-selected path without requiring it to exist.
// Inventory paths use the inventory's directory; CLI paths use the cwd.
func UserPath(path, directory string) (string, error) {
	if path == "" || strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("path must be a non-empty single line")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
	}
	if !filepath.IsAbs(path) && directory != "" {
		path = filepath.Join(directory, path)
	}
	return filepath.Abs(path)
}

// CanonicalPath resolves existing directory aliases and preserves any missing
// suffix without creating it. Callers still validate the selected object's type.
func CanonicalPath(path string) (string, error) {
	abs, err := UserPath(path, "")
	if err != nil {
		return "", err
	}
	probe := abs
	missing := make([]string, 0)
	for {
		canonical, evalErr := filepath.EvalSymlinks(probe)
		if evalErr == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				canonical = filepath.Join(canonical, missing[index])
			}
			return canonical, nil
		}
		if !errors.Is(evalErr, os.ErrNotExist) {
			return "", evalErr
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", evalErr
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}

// ResolveParent follows directory aliases while leaving the final entry alone:
// creating a file must not replace an existing file or follow its symlink.
func ResolveParent(path string) (string, error) {
	absolute, err := UserPath(path, "")
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

// EnsureParent is ResolveParent for a file the user asked Barn to create. It
// creates missing parent directories with private permissions, follows
// directory aliases, and still leaves the final file entry untouched so the
// atomic create/write helpers can protect an existing file or symlink.
func EnsureParent(path string) (string, error) {
	absolute, err := UserPath(path, "")
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(absolute)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(canonical, filepath.Base(absolute)), nil
}
