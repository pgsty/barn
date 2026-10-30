package macvm

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/pgsty/barn/internal/failure"
)

// Logs returns the last lines of a machine's runtime log: startup, network,
// shutdown and Apple Virtualization errors. It holds no guest secrets.
func (m *Manager) Logs(name string, lines int) (string, error) {
	if lines < 1 {
		return "", failure.New(failure.Usage, errors.New("the line count must be positive"))
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
	return tailLog(f, info.Size(), lines)
}

// Scan backward in fixed chunks, then read only the requested suffix. The
// default tail stays cheap even for a large log, without silently clipping
// an explicitly requested line count or a long diagnostic line.
func tailLog(reader io.ReaderAt, size int64, lines int) (string, error) {
	buffer := make([]byte, 32<<10)
	start := int64(0)
scan:
	for end := size; end > 0; {
		offset := max(int64(0), end-int64(len(buffer)))
		n, err := reader.ReadAt(buffer[:end-offset], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		for i := n - 1; i >= 0; i-- {
			position := offset + int64(i)
			if buffer[i] != '\n' || position == size-1 {
				continue
			}
			lines--
			if lines == 0 {
				start = position + 1
				break scan
			}
		}
		end = offset
	}
	data, err := io.ReadAll(io.NewSectionReader(reader, start, size-start))
	if err != nil || len(data) == 0 {
		return "", err
	}
	if data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	return string(data), nil
}
