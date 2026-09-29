package macvm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pgsty/barn/internal/failure"
)

// Logs returns the last lines of a machine's runtime log: startup, network,
// shutdown and Apple Virtualization errors. It holds no guest secrets.
func (m *Manager) Logs(name string, lines int) (string, error) {
	if lines < 1 || lines > 10000 {
		return "", failure.New(failure.Usage, errors.New("the line count must be between 1 and 10000"))
	}
	if _, err := m.Machine(name); err != nil {
		return "", err
	}
	path, err := m.runnerLogPath(name)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", failure.New(failure.Conflict, fmt.Errorf("%s has not started yet, so it has no runtime log", name)).Then("barn mac up " + name)
	} else if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("the runtime log must be a regular file")
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
	if len(data) == 0 {
		return "", nil
	}
	parts := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if truncated && len(parts) > 1 {
		parts = parts[1:]
	}
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n") + "\n", nil
}
