package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pgsty/barn/internal/failure"
)

// ErrNoConfig marks an inventory directory with no discoverable configuration.
var ErrNoConfig error = failure.New(failure.Usage, errors.New("no inventory found in this directory")).Because("no_inventory").Then("barn init, or pass -f <file>")

// DiscoveryNames are the configuration filenames probed in the working
// directory, in order: an explicit barn.yml wins over the Pigsty inventory
// it would otherwise share. The .yaml spellings are filename conveniences —
// the content format is the same inventory in every case.
var DiscoveryNames = []string{"barn.yml", "barn.yaml", "pigsty.yml", "pigsty.yaml"}

func readBounded(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, failure.New(failure.Usage, fmt.Errorf("inventory %s does not exist", path)).Then("barn init, or check -f")
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("configuration must be a regular non-symlink file: %s", path)
	}
	handle, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		// The configuration handle is read-only; accepted bytes are already
		// bounded in memory, so closing cannot change the parsed result.
		_ = handle.Close()
	}()
	data, err := io.ReadAll(io.LimitReader(handle, maxInventoryBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxInventoryBytes {
		return nil, fmt.Errorf("configuration exceeds the 4 MiB limit: %s", path)
	}
	return data, nil
}

// LoadPath parses one configuration file. The content decides the format: a
// Pigsty-compatible inventory (all:) parses and anything else is rejected.
func LoadPath(path string) (File, error) {
	if path == "" || !filepath.IsAbs(path) {
		return File{}, errors.New("configuration path must be absolute")
	}
	data, err := readBounded(path)
	if err != nil {
		return File{}, failure.New(failure.Usage, err)
	}
	if _, err := DetectFormat(data); err != nil {
		return File{}, failure.New(failure.Usage, fmt.Errorf("%s: %w", path, err))
	}
	file, err := ParseInventory(data)
	if err != nil {
		return File{}, failure.New(failure.Usage, fmt.Errorf("%s: %w", path, err))
	}
	return file, nil
}

// Discover resolves the inventory configuration: the explicit -f path when
// given, otherwise the first DiscoveryNames hit in the working directory.
// A missing configuration returns ErrNoConfig.
func Discover(cwd, explicit string) (File, string, error) {
	if explicit != "" {
		absolute, err := filepath.Abs(explicit)
		if err != nil {
			return File{}, "", err
		}
		file, err := LoadPath(absolute)
		return file, absolute, err
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return File{}, "", err
		}
	}
	for _, name := range DiscoveryNames {
		candidate := filepath.Join(cwd, name)
		if _, err := os.Lstat(candidate); err == nil {
			file, err := LoadPath(candidate)
			return file, candidate, err
		} else if !errors.Is(err, os.ErrNotExist) {
			return File{}, "", err
		}
	}
	return File{}, "", ErrNoConfig
}
