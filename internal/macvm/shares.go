package macvm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pgsty/farrow/internal/failure"
)

// Host directories are shared read-write or read-only with VirtioFS. A macOS
// guest mounts every share automatically under /Volumes/My Shared Files/<name>.
const (
	GuestShareRoot = "/Volumes/My Shared Files"
	maxShares      = 8
)

var shareNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ParseShare reads "[name=]PATH[:ro|:rw]". The name defaults to the last path
// component; a leading ~/ expands to the caller's home directory.
func ParseShare(spec string) (Share, error) {
	usage := func(detail string) error {
		return failure.New(failure.Usage, fmt.Errorf("invalid share %q: %s; use [name=]PATH[:ro]", spec, detail))
	}
	text := strings.TrimSpace(spec)
	share := Share{}
	if name, rest, ok := strings.Cut(text, "="); ok && !strings.Contains(name, "/") {
		if name == "" {
			return Share{}, usage("the share name before = is empty")
		}
		share.Name, text = name, rest
	}
	switch {
	case strings.HasSuffix(text, ":ro"):
		share.ReadOnly, text = true, strings.TrimSuffix(text, ":ro")
	case strings.HasSuffix(text, ":rw"):
		text = strings.TrimSuffix(text, ":rw")
	}
	if text == "" {
		return Share{}, usage("the host path is empty")
	}
	if text == "~" || strings.HasPrefix(text, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return Share{}, err
		}
		text = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(text, "~"), "/"))
	}
	path, err := filepath.Abs(text)
	if err != nil {
		return Share{}, err
	}
	share.Path = filepath.Clean(path)
	if share.Name == "" {
		share.Name = filepath.Base(share.Path)
	}
	if !shareNamePattern.MatchString(share.Name) {
		return Share{}, usage("the share name must use letters, digits, '.', '_' or '-'")
	}
	return share, nil
}

// checkShareSource verifies a host directory right before the VM uses it.
// Farrow never creates, changes or deletes a shared host directory.
func checkShareSource(share Share) error {
	info, err := os.Lstat(share.Path)
	if errors.Is(err, os.ErrNotExist) {
		return failure.New(failure.Usage, fmt.Errorf("shared directory %s does not exist: %s", share.Name, share.Path))
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return failure.New(failure.Usage, fmt.Errorf("shared directory %s is a symlink; share its target instead: %s", share.Name, share.Path))
	}
	if !info.IsDir() {
		return failure.New(failure.Usage, fmt.Errorf("shared path %s is not a directory: %s", share.Name, share.Path))
	}
	return nil
}

func validateShares(shares []Share) error {
	if len(shares) > maxShares {
		return fmt.Errorf("at most %d shared directories are supported", maxShares)
	}
	seen := map[string]bool{}
	for _, share := range shares {
		if !shareNamePattern.MatchString(share.Name) || !filepath.IsAbs(share.Path) || filepath.Clean(share.Path) != share.Path {
			return fmt.Errorf("invalid shared directory %q", share.Name)
		}
		if seen[share.Name] {
			return fmt.Errorf("duplicate shared directory name %q", share.Name)
		}
		seen[share.Name] = true
	}
	return nil
}

// mergeShares adds or replaces shares by name, then removes the named ones.
func mergeShares(current, add []Share, remove []string) ([]Share, error) {
	result := append([]Share(nil), current...)
	for _, share := range add {
		replaced := false
		for i := range result {
			if result[i].Name == share.Name {
				result[i], replaced = share, true
			}
		}
		if !replaced {
			result = append(result, share)
		}
	}
	for _, name := range remove {
		index := -1
		for i := range result {
			if result[i].Name == name {
				index = i
			}
		}
		if index < 0 {
			return nil, failure.New(failure.Usage, fmt.Errorf("no shared directory named %q", name))
		}
		result = append(result[:index], result[index+1:]...)
	}
	if err := validateShares(result); err != nil {
		return nil, failure.New(failure.Usage, err)
	}
	return result, nil
}
