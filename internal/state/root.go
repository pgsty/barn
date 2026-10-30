package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pgsty/barn/internal/fsutil"
)

func canonicalIfExisting(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if canonical, err := filepath.EvalSymlinks(abs); err == nil {
		return canonical
	}
	return abs
}

// canonicalWithMissing resolves every existing ancestor and then restores the
// not-yet-created suffix, so a symlinked parent of a missing data root is
// still inspected.
func canonicalWithMissing(path string) (string, error) {
	abs, err := filepath.Abs(path)
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

func unsafeRoot(root, home string) bool {
	clean := filepath.Clean(root)
	for _, unsafe := range []string{"/", filepath.Clean(home)} {
		if unsafe != "." && unsafe != "" && clean == unsafe {
			return true
		}
	}
	return false
}

// ResolveDataRoot implements the complete precedence: BARN_HOME, then
// ~/.barn. It resolves directory aliases before returning the root, refuses
// broad roots, and does not create the directory. An unsupported layout (a
// projects/ registry) is a hard error.
func ResolveDataRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	home = canonicalIfExisting(home)
	root := os.Getenv("BARN_HOME")
	if root == "" {
		root = filepath.Join(home, ".barn")
	}
	root, err = fsutil.UserPath(root, "")
	if err != nil {
		return "", err
	}
	comparisonRoot, err := canonicalWithMissing(root)
	if err != nil {
		return "", err
	}
	if unsafeRoot(comparisonRoot, home) {
		return "", fmt.Errorf("unsafe broad Barn data root: %s", root)
	}
	finalSymlink := false
	if info, err := os.Lstat(root); err == nil {
		finalSymlink = info.Mode()&os.ModeSymlink != 0
		if finalSymlink {
			if target, statErr := os.Stat(root); statErr != nil {
				return "", statErr
			} else if !target.IsDir() {
				return "", fmt.Errorf("barn data root is not a directory: %s", root)
			}
		} else if !info.IsDir() {
			return "", fmt.Errorf("barn data root is not a directory: %s", root)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		// A missing final entry is fine; setup will create it. The canonical
		// parent returned by canonicalWithMissing still protects root checks.
	} else {
		return "", err
	}
	if _, err := os.Lstat(filepath.Join(comparisonRoot, "projects")); err == nil {
		return "", fmt.Errorf("%s holds an unsupported multiple-deployment layout; preserve this directory and choose a new empty absolute BARN_HOME for setup/up", comparisonRoot)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if finalSymlink {
		return comparisonRoot, nil
	}
	return root, nil
}
