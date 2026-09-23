package diagnostics

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"golang.org/x/sys/unix"
)

const maxEventBytes = 64 << 10
const maxDiagnosticLogBytes int64 = 8 << 20
const retainedDiagnosticLogBytes int64 = 4 << 20

var eventNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

type Event struct {
	Schema      int       `json:"schema"`
	Time        time.Time `json:"time"`
	Level       string    `json:"level"`
	Node        string    `json:"node"`
	OperationID string    `json:"operation_id"`
	Action      string    `json:"action"`
	Phase       string    `json:"phase,omitempty"`
	Message     string    `json:"message,omitempty"`
}

type QEMULogRecord struct {
	Schema      int       `json:"schema"`
	Time        time.Time `json:"time"`
	Level       string    `json:"level"`
	Node        string    `json:"node"`
	OperationID string    `json:"operation_id"`
	Action      string    `json:"action"`
	Message     string    `json:"message"`
	Argv        []string  `json:"argv,omitempty"`
}

func validateEvent(event Event) error {
	if event.Schema != 1 || event.Time.IsZero() || len(event.OperationID) < 8 {
		return errors.New("event schema, time, or operation identity is invalid")
	}
	if event.Level != "error" && event.Level != "warn" && event.Level != "info" && event.Level != "debug" {
		return fmt.Errorf("invalid event level %q", event.Level)
	}
	if !eventNamePattern.MatchString(event.Node) || !eventNamePattern.MatchString(event.Action) {
		return errors.New("event node or action is invalid")
	}
	return nil
}

func lockEventFile(ctx context.Context, descriptor int) error {
	for {
		if err := unix.Flock(descriptor, unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return nil
		} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func appendJSONLine(ctx context.Context, pathname, basename string, value any) (returnErr error) {
	if pathname == "" || !filepath.IsAbs(pathname) || filepath.Base(pathname) != basename {
		return fmt.Errorf("diagnostic path must be an absolute %s path", basename)
	}
	parent := filepath.Dir(pathname)
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || parentInfo.Mode().Perm() != 0o700 {
		return errors.New("diagnostic parent must be an owned mode-0700 directory")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxEventBytes {
		return errors.New("diagnostic record exceeds 64 KiB limit")
	}
	descriptor, err := unix.Open(pathname, unix.O_RDWR|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	handle := os.NewFile(uintptr(descriptor), pathname)
	if handle == nil {
		return errors.Join(errors.New("open diagnostic file handle"), unix.Close(descriptor))
	}
	locked := false
	defer func() {
		if locked {
			if err := unix.Flock(descriptor, unix.LOCK_UN); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("unlock diagnostic file: %w", err))
			}
		}
		if err := handle.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close diagnostic file: %w", err))
		}
	}()
	info, err := handle.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("diagnostic target is not a regular file")
	}
	if err := handle.Chmod(0o600); err != nil {
		return err
	}
	if err := lockEventFile(ctx, descriptor); err != nil {
		return err
	}
	locked = true
	info, err = handle.Stat()
	if err != nil {
		return err
	}
	if info.Size()+int64(len(data)) > maxDiagnosticLogBytes {
		keep := retainedDiagnosticLogBytes
		if info.Size() < keep {
			keep = info.Size()
		}
		tail := make([]byte, keep)
		if keep > 0 {
			read, err := handle.ReadAt(tail, info.Size()-keep)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			tail = tail[:read]
			if newline := bytes.IndexByte(tail, '\n'); newline >= 0 {
				tail = tail[newline+1:]
			} else {
				tail = nil
			}
		}
		if err := handle.Truncate(0); err != nil {
			return err
		}
		if len(tail) > 0 {
			if _, err := handle.Write(tail); err != nil {
				return err
			}
		}
	}
	writer := bufio.NewWriterSize(handle, len(data))
	if _, err := writer.Write(data); err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	return handle.Sync()
}

// AppendEvent writes one bounded JSON line under an exclusive file lock.
// O_NOFOLLOW and strict parent/file modes keep the append target scoped.
func AppendEvent(ctx context.Context, pathname string, event Event) error {
	if err := validateEvent(event); err != nil {
		return err
	}
	return appendJSONLine(ctx, pathname, "events.jsonl", event)
}

func AppendQEMULog(ctx context.Context, pathname string, record QEMULogRecord) error {
	if record.Schema != 1 || record.Time.IsZero() || len(record.OperationID) < 8 || !eventNamePattern.MatchString(record.Node) || !eventNamePattern.MatchString(record.Action) {
		return errors.New("QEMU log record identity/action is invalid")
	}
	if record.Level != "error" && record.Level != "warn" && record.Level != "info" && record.Level != "debug" {
		return fmt.Errorf("invalid QEMU log level %q", record.Level)
	}
	return appendJSONLine(ctx, pathname, "qemu.log", record)
}
