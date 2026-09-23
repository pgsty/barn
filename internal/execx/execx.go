// Package execx runs bounded external commands without invoking a shell.
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const defaultOutputLimit = 1 << 20

// Result is the bounded output and process metadata from one invocation.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Duration time.Duration
}

// Runner is implemented by OSRunner and by deterministic test fakes.
type Runner interface {
	Run(ctx context.Context, binary string, args ...string) (Result, error)
}

// ExtraFilesRunner is the narrow extension used by the Darwin private-network
// FD fallback. File index zero is inherited by the child as descriptor 3.
type ExtraFilesRunner interface {
	RunWithExtraFiles(ctx context.Context, binary string, files []*os.File, args ...string) (Result, error)
}

// OSRunner executes argv directly. Timeout zero means use only the caller's
// context. OutputLimit zero selects a conservative default.
type OSRunner struct {
	Timeout     time.Duration
	OutputLimit int
}

// CommandError preserves controlled stderr and the exit code without exposing
// an unbounded command output. Its message names the program, not the argv;
// callers that need the full invocation read Binary and Args.
type CommandError struct {
	Binary   string
	Args     []string
	ExitCode int
	Signal   string
	Timeout  time.Duration // non-zero when the runner's own timeout expired
	Stderr   string
	Cause    error
}

func (e *CommandError) Error() string {
	var message string
	switch {
	case e.Timeout > 0:
		message = fmt.Sprintf("%s timed out after %s", e.Name(), e.Timeout)
	case e.Signal != "":
		message = fmt.Sprintf("%s was killed by %s", e.Name(), e.Signal)
	case e.ExitCode >= 0:
		message = fmt.Sprintf("%s exited with status %d", e.Name(), e.ExitCode)
	default:
		message = fmt.Sprintf("%s failed: %v", e.Name(), e.Cause)
	}
	if lines := e.StderrTail(1); len(lines) != 0 {
		message += ": " + lines[0]
	}
	return message
}

func (e *CommandError) Unwrap() error { return e.Cause }

// Name is the program the user would recognize: the binary's base name, or the
// program run through sudo.
func (e *CommandError) Name() string {
	name := filepath.Base(e.Binary)
	if name != "sudo" {
		return name
	}
	for index, arg := range e.Args {
		if arg == "--" && index+1 < len(e.Args) {
			return filepath.Base(e.Args[index+1]) + " (via sudo)"
		}
		if !strings.HasPrefix(arg, "-") {
			return filepath.Base(arg) + " (via sudo)"
		}
	}
	return name
}

// StderrTail returns up to limit last non-empty stderr lines with control
// characters removed and each line bounded, for display under an error.
func (e *CommandError) StderrTail(limit int) []string {
	var lines []string
	for _, line := range strings.Split(e.Stderr, "\n") {
		line = strings.TrimSpace(strings.Map(func(r rune) rune {
			if r == '\t' {
				return ' '
			}
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, stripANSI(line)))
		if line == "" {
			continue
		}
		if len(line) > maxStderrLine {
			line = line[:maxStderrLine] + "…"
		}
		lines = append(lines, line)
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines
}

const maxStderrLine = 240

func stripANSI(value string) string {
	if !strings.Contains(value, "\x1b") {
		return value
	}
	var out strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != 0x1b {
			out.WriteByte(value[index])
			continue
		}
		// Skip ESC [ ... final byte (CSI) or a lone two-byte escape.
		index++
		if index < len(value) && value[index] == '[' {
			for index+1 < len(value) && (value[index+1] < 0x40 || value[index+1] > 0x7e) {
				index++
			}
			index++
		}
	}
	return out.String()
}

// Run executes binary with args as an argv slice. It never invokes a shell.
func (r OSRunner) Run(ctx context.Context, binary string, args ...string) (Result, error) {
	return r.run(ctx, binary, nil, args...)
}

func (r OSRunner) RunWithExtraFiles(ctx context.Context, binary string, files []*os.File, args ...string) (Result, error) {
	if len(files) == 0 || len(files) > 16 {
		return Result{}, errors.New("external command extra-file count must be 1..16")
	}
	for _, file := range files {
		if file == nil {
			return Result{}, errors.New("external command extra file is nil")
		}
	}
	return r.run(ctx, binary, files, args...)
}

func (r OSRunner) run(ctx context.Context, binary string, files []*os.File, args ...string) (Result, error) {
	if binary == "" {
		return Result{}, errors.New("external command binary is empty")
	}
	parent := ctx
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	limit := r.OutputLimit
	if limit <= 0 {
		limit = defaultOutputLimit
	}
	stdout := newLimitedBuffer(limit)
	stderr := newLimitedBuffer(limit)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.ExtraFiles = files
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	started := time.Now()
	err := cmd.Run()
	result := Result{
		Stdout:   bytes.Clone(stdout.Bytes()),
		Stderr:   bytes.Clone(stderr.Bytes()),
		ExitCode: 0,
		Duration: time.Since(started),
	}
	if err == nil {
		return result, nil
	}

	result.ExitCode = -1
	commandErr := &CommandError{
		Binary: binary,
		Args:   append([]string(nil), args...),
		Stderr: strings.TrimSpace(string(result.Stderr)),
		Cause:  err,
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			commandErr.Signal = unix.SignalName(status.Signal())
		}
	}
	// Only a kill by the runner's own deadline is a timeout; a command that
	// failed by itself just before the deadline keeps its exit status.
	if r.Timeout > 0 && commandErr.Signal != "" && errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil {
		commandErr.Timeout = r.Timeout
		commandErr.Signal = ""
	}
	commandErr.ExitCode = result.ExitCode
	return result, commandErr
}

// Display returns a human-readable representation only. The returned string
// must never be fed to a shell for execution.
func Display(binary string, args ...string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, strconv.Quote(binary))
	for _, arg := range args {
		parts = append(parts, strconv.Quote(arg))
	}
	return strings.Join(parts, " ")
}

type limitedBuffer struct {
	buf       bytes.Buffer
	remaining int
}

func newLimitedBuffer(limit int) *limitedBuffer { return &limitedBuffer{remaining: limit} }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if b.remaining > 0 {
		keep := len(p)
		if keep > b.remaining {
			keep = b.remaining
		}
		_, _ = b.buf.Write(p[:keep])
		b.remaining -= keep
	}
	return original, nil
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }
