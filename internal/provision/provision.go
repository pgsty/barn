// Package provision runs explicit, bounded guest provisioning scripts over
// Farrow's existing SSH trust channel. It deliberately does not add a plugin
// system, implicit up hook, or shell execution on the host.
package provision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/naming"
	"github.com/pgsty/farrow/internal/vm"
	"golang.org/x/sys/unix"
)

const (
	MaxScriptBytes   = 4 << 20
	MaxParallelism   = 4
	defaultOutputCap = 1 << 20
)

type Script struct {
	Name    string `json:"name"`
	Path    string `json:"-"`
	Size    int64  `json:"size_bytes"`
	SHA256  string `json:"sha256"`
	Content []byte `json:"-"`
}

// LoadScript reads one immutable snapshot of a local script. O_NOFOLLOW and
// identity checks close the final-component symlink and replacement races;
// every target receives its own bytes.Reader over this snapshot.
func LoadScript(path string) (Script, error) {
	if path == "" || strings.ContainsAny(path, "\x00\r\n") {
		return Script{}, errors.New("provision script path must be a non-empty single line")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Script{}, fmt.Errorf("resolve provision script: %w", err)
	}
	before, err := os.Lstat(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return Script{}, failure.New(failure.Usage, fmt.Errorf("provision script %s does not exist", absolute))
	}
	if err != nil {
		return Script{}, fmt.Errorf("inspect provision script: %w", err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return Script{}, errors.New("provision script must be a regular non-symlink file")
	}
	if before.Size() <= 0 {
		return Script{}, errors.New("provision script must not be empty")
	}
	if before.Size() > MaxScriptBytes {
		return Script{}, fmt.Errorf("provision script exceeds %d-byte limit", MaxScriptBytes)
	}

	descriptor, err := unix.Open(absolute, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return Script{}, fmt.Errorf("open provision script without following links: %w", err)
	}
	handle := os.NewFile(uintptr(descriptor), absolute)
	if handle == nil {
		_ = unix.Close(descriptor)
		return Script{}, errors.New("open provision script handle")
	}
	defer func() {
		// The no-follow script handle is read-only; content is already bounded
		// in memory before the script is accepted.
		_ = handle.Close()
	}()
	opened, err := handle.Stat()
	if err != nil {
		return Script{}, fmt.Errorf("stat opened provision script: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return Script{}, errors.New("provision script identity changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(handle, MaxScriptBytes+1))
	if err != nil {
		return Script{}, fmt.Errorf("read provision script: %w", err)
	}
	if len(content) == 0 || len(content) > MaxScriptBytes {
		return Script{}, errors.New("provision script is empty or exceeded its bounded read")
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return Script{}, errors.New("provision script contains a NUL byte")
	}
	after, err := handle.Stat()
	if err != nil {
		return Script{}, fmt.Errorf("restat provision script: %w", err)
	}
	if !os.SameFile(opened, after) || after.Size() != int64(len(content)) || after.ModTime() != opened.ModTime() {
		return Script{}, errors.New("provision script changed while being read")
	}
	digest := sha256.Sum256(content)
	return Script{
		Name: filepath.Base(absolute), Path: absolute, Size: int64(len(content)),
		SHA256: hex.EncodeToString(digest[:]), Content: content,
	}, nil
}

type Target struct {
	Node         string
	User         string
	Port         uint16
	PrivateKey   string
	KnownHosts   string
	HostKeyAlias string
}

func (t Target) validate() error {
	if !naming.ValidNodeName(t.Node) || t.Port == 0 {
		return fmt.Errorf("invalid provision target %q", t.Node)
	}
	if !filepath.IsAbs(t.PrivateKey) || !filepath.IsAbs(t.KnownHosts) {
		return fmt.Errorf("provision target %s has non-absolute SSH artifacts", t.Node)
	}
	if vm.SSHArgsForInstance(t.User, t.PrivateKey, t.KnownHosts, t.HostKeyAlias, t.Port, "true") == nil {
		return fmt.Errorf("provision target %s has an unsafe SSH user or artifact", t.Node)
	}
	return nil
}

type NodeResult struct {
	Node            string `json:"node"`
	Success         bool   `json:"success"`
	ExitCode        int    `json:"exit_code"`
	DurationMS      int64  `json:"duration_ms"`
	Stdout          string `json:"stdout,omitempty"`
	Stderr          string `json:"stderr,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	Error           string `json:"error,omitempty"`
}

type Report struct {
	Schema      int          `json:"schema"`
	OperationID string       `json:"operation_id"`
	Script      Script       `json:"script"`
	Sudo        bool         `json:"sudo"`
	Parallelism int          `json:"parallelism"`
	Results     []NodeResult `json:"results"`
	Successful  int          `json:"successful"`
	Failed      int          `json:"failed"`
	DurationMS  int64        `json:"duration_ms"`
	AuditError  string       `json:"audit_error,omitempty"`
}

type Runner interface {
	Run(context.Context, Target, Script, bool) NodeResult
}

type Executor struct {
	Runner      Runner
	Parallelism int
	OperationID string
}

func (e Executor) Execute(ctx context.Context, script Script, targets []Target, sudo bool) (Report, error) {
	if e.Runner == nil || e.OperationID == "" {
		return Report{}, errors.New("provision executor requires a runner and operation ID")
	}
	if _, ok := ctx.Deadline(); !ok {
		return Report{}, errors.New("provision execution requires a hard deadline")
	}
	if len(script.Content) == 0 || len(script.Content) > MaxScriptBytes || script.Size != int64(len(script.Content)) {
		return Report{}, errors.New("provision script snapshot is invalid")
	}
	digest := sha256.Sum256(script.Content)
	if script.SHA256 != hex.EncodeToString(digest[:]) {
		return Report{}, errors.New("provision script snapshot is invalid")
	}
	if len(targets) == 0 || len(targets) > 20 {
		return Report{}, errors.New("provision requires 1..20 targets")
	}
	if e.Parallelism < 1 || e.Parallelism > MaxParallelism {
		return Report{}, fmt.Errorf("provision parallelism must be 1..%d", MaxParallelism)
	}
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if err := target.validate(); err != nil {
			return Report{}, err
		}
		if _, exists := seen[target.Node]; exists {
			return Report{}, fmt.Errorf("duplicate provision target %q", target.Node)
		}
		seen[target.Node] = struct{}{}
	}

	started := time.Now()
	report := Report{
		Schema: 1, OperationID: e.OperationID,
		Script: Script{Name: script.Name, Size: script.Size, SHA256: script.SHA256},
		Sudo:   sudo, Parallelism: e.Parallelism, Results: make([]NodeResult, len(targets)),
	}
	jobs := make(chan int)
	workers := e.Parallelism
	if workers > len(targets) {
		workers = len(targets)
	}
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wait.Done()
			for index := range jobs {
				report.Results[index] = e.Runner.Run(ctx, targets[index], script, sudo)
			}
		}()
	}
	for index := range targets {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	for _, result := range report.Results {
		if result.Success {
			report.Successful++
		} else {
			report.Failed++
		}
	}
	report.DurationMS = time.Since(started).Milliseconds()
	return report, nil
}

type SSHRunner struct {
	SSHPath     string
	OutputLimit int
}

func (r SSHRunner) Run(ctx context.Context, target Target, script Script, sudo bool) (result NodeResult) {
	result = NodeResult{Node: target.Node, ExitCode: -1}
	started := time.Now()
	defer func() { result.DurationMS = time.Since(started).Milliseconds() }()
	if _, ok := ctx.Deadline(); !ok {
		result.Error = "SSH provision command has no deadline"
		return result
	}
	if err := target.validate(); err != nil {
		result.Error = err.Error()
		return result
	}
	if r.SSHPath == "" || !filepath.IsAbs(r.SSHPath) {
		result.Error = "SSH binary path must be absolute"
		return result
	}
	remote := []string{"/bin/bash", "-se"}
	if sudo {
		remote = []string{"sudo", "-n", "--", "/bin/bash", "-se"}
	}
	args := vm.SSHArgsForInstance(target.User, target.PrivateKey, target.KnownHosts, target.HostKeyAlias, target.Port, remote...)
	if args == nil {
		result.Error = "could not construct safe SSH argv"
		return result
	}
	limit := r.OutputLimit
	if limit <= 0 {
		limit = defaultOutputCap
	}
	stdout, stderr := newLimitedBuffer(limit), newLimitedBuffer(limit)
	command := exec.CommandContext(ctx, r.SSHPath, args...)
	command.Stdin = bytes.NewReader(script.Content)
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	result.StdoutTruncated = stdout.truncated
	result.StderrTruncated = stderr.truncated
	if err == nil {
		result.Success = true
		result.ExitCode = 0
		return result
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
	}
	if ctx.Err() != nil {
		result.Error = "provision deadline exceeded: " + ctx.Err().Error()
	} else {
		result.Error = fmt.Sprintf("SSH provision process failed: %v", err)
	}
	return result
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func newLimitedBuffer(limit int) *limitedBuffer { return &limitedBuffer{remaining: limit} }

func (b *limitedBuffer) String() string { return b.buffer.String() }

func (b *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if len(value) > b.remaining {
		value = value[:b.remaining]
		b.truncated = true
	}
	if len(value) > 0 {
		_, _ = b.buffer.Write(value)
		b.remaining -= len(value)
	}
	return original, nil
}
