package macvm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pgsty/farrow/internal/failure"
)

// RunnerProtocol is the runner command and RPC contract this CLI speaks. The
// CLI and runner may come from different builds as long as it matches.
const RunnerProtocol = 2

// Runner is the Apple API companion. No secret is passed in argv.
type Runner struct {
	Binary   string
	Progress io.Writer
}

type RuntimeStatus struct {
	OK            bool   `json:"ok"`
	Instance      string `json:"instance"`
	State         string `json:"state"`
	PID           int    `json:"pid"`
	WindowVisible bool   `json:"window_visible,omitempty"`
}

type RestoreMetadata struct {
	OK                bool   `json:"ok"`
	Version           string `json:"version"`
	Build             string `json:"build"`
	HardwareModelHash string `json:"hardware_model_sha256"`
	MinimumCPU        int    `json:"minimum_cpu"`
	MinimumMemory     int64  `json:"minimum_memory"`
	DiskSize          int64  `json:"disk_size"`
}

func HostSupported() error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return failure.New(failure.Capability, errors.New("farrow mac requires Apple Silicon and macOS 27 or later")).Because("mac_host_unsupported")
	}
	data, err := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		return err
	}
	major, err := strconv.Atoi(strings.Split(strings.TrimSpace(string(data)), ".")[0])
	if err != nil || major < 27 {
		return failure.New(failure.Capability, errors.New("farrow mac requires macOS 27 or later")).Because("mac_host_unsupported")
	}
	return nil
}

func FindRunner() (string, error) {
	if err := HostSupported(); err != nil {
		return "", err
	}
	if override := os.Getenv("FARROW_MAC_RUNNER"); override != "" {
		if !filepath.IsAbs(override) {
			return "", errors.New("FARROW_MAC_RUNNER must be absolute")
		}
		return executableFile(override)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	for _, path := range runnerCandidates(filepath.Dir(exe)) {
		if found, err := executableFile(path); err == nil {
			return found, nil
		}
	}
	return "", failure.New(failure.Capability, errors.New("the Farrow Mac component is not installed next to this farrow")).
		Because("mac_runner_missing").
		Then("make mac-build in a Farrow source checkout, then use bin/mac/farrow (https://farrow.pgsty.com/docs/start/macos/)")
}

// runnerCandidates covers the source bundle, the installer's version-independent
// component directory, and Homebrew's libexec.
func runnerCandidates(dir string) []string {
	app := filepath.Join("Farrow Mac.app", "Contents", "MacOS", "farrow-mac-runner")
	return []string{
		filepath.Join(dir, app),
		filepath.Join(dir, "libexec", app),
		filepath.Join(dir, "..", "libexec", app),
		filepath.Join(dir, "..", "libexec", "farrow-mac", app),
		filepath.Join(dir, "farrow-mac-runner"),
		filepath.Join(dir, "libexec", "farrow-mac-runner"),
		filepath.Join(dir, "..", "libexec", "farrow-mac-runner"),
	}
}

func executableFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("not an executable file: %s", path)
	}
	return filepath.Clean(path), nil
}

// CheckProtocol refuses a runner from an incompatible build before any VM work.
func (r Runner) CheckProtocol(ctx context.Context) error {
	var probe struct {
		OK       bool   `json:"ok"`
		Protocol int    `json:"protocol_version"`
		Version  string `json:"version"`
	}
	if err := r.Call(ctx, nil, &probe, "probe"); err != nil {
		return err
	}
	if probe.Protocol != RunnerProtocol {
		return failure.New(failure.Capability, fmt.Errorf("the installed Farrow Mac component (%s) speaks protocol %d, but this farrow needs protocol %d", probe.Version, probe.Protocol, RunnerProtocol)).
			Because("mac_runner_protocol").Then("reinstall Farrow so the CLI and its Mac component match")
	}
	return nil
}

func (r Runner) Call(ctx context.Context, input any, result any, args ...string) error {
	if len(args) == 0 {
		return errors.New("mac runner command is required")
	}
	secret := args[0] == "secret"
	var stdin bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&stdin).Encode(input); err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	cmd.Stdin = &stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if r.Progress != nil && !secret {
		cmd.Stderr = io.MultiWriter(r.Progress, &stderr)
	}
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Runner error responses contain public diagnostics only. Never echo stdin.
		var response struct {
			Error   json.RawMessage `json:"error"`
			Message string          `json:"message"`
		}
		_ = json.Unmarshal(stdout.Bytes(), &response)
		if secret {
			// A runner may echo its stdin while failing. Secret commands never
			// forward its stderr or arbitrary error strings to logs or callers.
			var nested struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(response.Error, &nested)
			return fmt.Errorf("legacy Keychain %s failed (%s)", strings.Join(args[1:2], ""), nested.Code)
		}
		detail := response.Message
		if detail == "" {
			var nested struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(response.Error, &nested) == nil {
				detail = nested.Code + ": " + nested.Message
			} else {
				_ = json.Unmarshal(response.Error, &detail)
			}
		}
		if detail == "" {
			detail = strings.TrimSpace(stderr.String())
		}
		if len(detail) > 4096 {
			detail = detail[len(detail)-4096:]
		}
		return fmt.Errorf("mac runner %s: %w: %s", args[0], err, detail)
	}
	if result != nil {
		if err := json.Unmarshal(stdout.Bytes(), result); err != nil {
			return fmt.Errorf("invalid mac runner %s response: %w", args[0], err)
		}
	}
	return nil
}

// RPC talks to a running machine's runner over its private Unix socket. The
// socket lives in a 0700 directory and both ends check the peer's UID.
func (r Runner) RPC(ctx context.Context, socket, instance, method string, force bool) (RuntimeStatus, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return RuntimeStatus{}, err
	}
	defer func() { _ = conn.Close() }()
	if err := checkPeerUID(conn); err != nil {
		return RuntimeStatus{}, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	request, err := json.Marshal(map[string]any{"instance": instance, "method": method, "force": force})
	if err != nil {
		return RuntimeStatus{}, err
	}
	if _, err := conn.Write(append(request, '\n')); err != nil {
		return RuntimeStatus{}, err
	}
	line, err := bufio.NewReader(io.LimitReader(conn, 64<<10)).ReadBytes('\n')
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		if ctx.Err() != nil {
			return RuntimeStatus{}, ctx.Err()
		}
		return RuntimeStatus{}, fmt.Errorf("mac runner did not answer %s: %w", method, err)
	}
	var reply struct {
		RuntimeStatus
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &reply); err != nil {
		return RuntimeStatus{}, fmt.Errorf("invalid mac runner %s reply: %w", method, err)
	}
	if reply.Error != nil {
		return RuntimeStatus{}, fmt.Errorf("mac runner %s: %s: %s", method, reply.Error.Code, reply.Error.Message)
	}
	if !reply.OK || reply.Instance != instance {
		return RuntimeStatus{}, errors.New("mac runner instance identity mismatch")
	}
	return reply.RuntimeStatus, nil
}

func checkPeerUID(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("mac runner connection is not a Unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var uid uint32
	var credErr error
	if err := raw.Control(func(fd uintptr) { uid, credErr = peerUID(fd) }); err != nil {
		return err
	}
	if credErr != nil {
		return credErr
	}
	if int(uid) != os.Getuid() {
		return errors.New("mac runner socket belongs to another user")
	}
	return nil
}

// A short, private runtime directory avoids Unix socket path limits even when
// FARROW_HOME is a deeply nested test or project path. No PID-only signals.
func RuntimeDir(root string, create bool) (string, error) {
	digest := sha256.Sum256([]byte(root))
	path := filepath.Join("/tmp", fmt.Sprintf("farrow-mac-%d-%x", os.Getuid(), digest[:8]))
	if create {
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && !create {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || int(stat.Uid) != os.Getuid() {
		return "", fmt.Errorf("unsafe mac runtime directory: %s", path)
	}
	return path, nil
}

// Launch starts a detached runner and returns once its VM reports running.
// The runner owns its own session so it outlives this command and terminal.
func (r Runner) Launch(ctx context.Context, args []string, input any, logPath, socket, instance string) (RuntimeStatus, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return RuntimeStatus{}, err
	}
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return RuntimeStatus{}, err
	}
	defer func() { _ = log.Close() }()
	cmd := exec.Command(r.Binary, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = reader, log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return RuntimeStatus{}, err
	}
	_ = reader.Close()
	if input != nil {
		err = json.NewEncoder(writer).Encode(input)
	}
	_ = writer.Close()
	// Reap while the CLI lives. The runner is deliberately independent of
	// command cancellation or terminal closure.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	if err != nil {
		return RuntimeStatus{}, err
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(90 * time.Second)
	defer timeout.Stop()
	for {
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		status, rpcErr := r.RPC(probe, socket, instance, "status", false)
		cancel()
		if rpcErr == nil && status.State == "running" {
			return status, nil
		}
		if rpcErr == nil && (status.State == "error" || status.State == "stopped") {
			return status, fmt.Errorf("the VM stopped during startup; inspect %s", logPath)
		}
		select {
		case <-ctx.Done():
			return RuntimeStatus{}, fmt.Errorf("the VM may still be starting in the background; check farrow mac ls: %w", ctx.Err())
		case err := <-exited:
			return RuntimeStatus{}, launchFailure(logPath, err)
		case <-timeout.C:
			return RuntimeStatus{}, fmt.Errorf("the VM did not start within 90 seconds; inspect %s", logPath)
		case <-ticker.C:
		}
	}
}

// launchFailure surfaces the runner's own error object from its log when it
// exits before the VM runs, such as the host-wide macOS VM limit.
func launchFailure(logPath string, exitErr error) error {
	data, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-20; i-- {
		var reply struct {
			OK    *bool `json:"ok"`
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(lines[i]), &reply) == nil && reply.Error != nil {
			if reply.Error.Code == "virtual_machine_limit" {
				return failure.New(failure.Resource, errors.New(reply.Error.Message)).Because("mac_vm_limit").Then("farrow mac stop <another machine>, or quit other macOS VMs")
			}
			return fmt.Errorf("%s: %s", reply.Error.Code, reply.Error.Message)
		}
	}
	return fmt.Errorf("the Mac runner exited before the VM started (%v); inspect %s", exitErr, logPath)
}
