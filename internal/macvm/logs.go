package macvm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

func (m *Manager) Logs(name string, lines int) (string, error) {
	if lines < 1 || lines > 10000 {
		return "", errors.New("log line count must be between 1 and 10000")
	}
	name, err := NormalizeSlot(name)
	if err != nil {
		return "", err
	}
	path, err := m.Store.Path("slots", name, "runner.log")
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%s has no runtime log yet; run farrow mac up %s", name, name)
	} else if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("runtime log must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	const maxBytes int64 = 1 << 20
	truncated := info.Size() > maxBytes
	if truncated {
		if _, err = f.Seek(-maxBytes, io.SeekEnd); err != nil {
			return "", err
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if truncated && len(parts) > 1 {
		parts = parts[1:]
	}
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	if len(data) == 0 {
		return "", nil
	}
	return strings.Join(parts, "\n") + "\n", nil
}
