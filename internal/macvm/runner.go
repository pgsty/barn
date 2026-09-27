package macvm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Runner is the public Apple API companion. No secret is passed in argv.
type Runner struct {
	Binary   string
	Progress io.Writer
}

type RuntimeStatus struct {
	OK       bool   `json:"ok"`
	Instance string `json:"instance"`
	State    string `json:"state"`
	PID      int    `json:"pid"`
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
	for _, path := range []string{
		filepath.Join(filepath.Dir(exe), "Farrow Mac.app", "Contents", "MacOS", "farrow-mac-runner"),
		filepath.Join(filepath.Dir(exe), "libexec", "Farrow Mac.app", "Contents", "MacOS", "farrow-mac-runner"),
		filepath.Join(filepath.Dir(exe), "..", "libexec", "Farrow Mac.app", "Contents", "MacOS", "farrow-mac-runner"),
		filepath.Join(filepath.Dir(exe), "farrow-mac-runner"),
		filepath.Join(filepath.Dir(exe), "libexec", "farrow-mac-runner"),
		filepath.Join(filepath.Dir(exe), "..", "libexec", "farrow-mac-runner"),
	} {
		if found, err := executableFile(path); err == nil {
			return found, nil
		}
	}
	return "", failure.New(failure.Capability, errors.New("farrow-mac-runner is missing; install the macOS arm64 bundle or run make mac-build in the source checkout")).Because("mac_runner_missing")
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
		// Companion error responses contain public diagnostics only. Never echo stdin.
		var response struct {
			Error   json.RawMessage `json:"error"`
			Message string          `json:"message"`
		}
		_ = json.Unmarshal(stdout.Bytes(), &response)
		if secret {
			// A companion may echo its stdin while failing. Secret commands never
			// forward its stderr or arbitrary error strings to logs or callers.
			var nested struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(response.Error, &nested)
			return secretCommandError(args, nested.Code)
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
			if secret {
				return secretCommandError(args, "invalid_response")
			}
			return fmt.Errorf("invalid mac runner %s response: %w", args[0], err)
		}
	}
	return nil
}

func (r Runner) RPC(ctx context.Context, socket, instance, method string, force bool) (RuntimeStatus, error) {
	args := []string{"rpc", "--socket", socket, "--instance", instance, "--method", method}
	if force {
		args = append(args, "--force")
	}
	var result RuntimeStatus
	err := r.Call(ctx, nil, &result, args...)
	if err == nil && (!result.OK || result.Instance != instance) {
		err = errors.New("mac runner instance identity mismatch")
	}
	return result, err
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
	// Reap while the CLI lives. The runner belongs to its own session and is
	// deliberately independent of command cancellation or terminal closure.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	if err != nil {
		return RuntimeStatus{}, err
	}
	ticker := time.NewTicker(500 * time.Millisecond)
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
			return status, fmt.Errorf("VM startup ended in %s; inspect %s", status.State, logPath)
		}
		select {
		case <-ctx.Done():
			return RuntimeStatus{}, fmt.Errorf("runner may continue in background; inspect mac ls: %w", ctx.Err())
		case err := <-exited:
			return RuntimeStatus{}, fmt.Errorf("mac runner exited before its control socket was ready (%v); inspect %s", err, logPath)
		case <-timeout.C:
			return RuntimeStatus{}, fmt.Errorf("mac runner startup timed out; inspect %s", logPath)
		case <-ticker.C:
		}
	}
}

func secretService(root string) string {
	sum := sha256.Sum256([]byte(root))
	return fmt.Sprintf("farrow.mac.%x", sum[:12])
}

func (r Runner) SetPassword(ctx context.Context, root, instance, password string) error {
	owner, err := r.credentialRunner(ctx, root, instance)
	if err != nil {
		return err
	}
	if owner.Binary == r.Binary {
		owner, err = r.preserveCredentialRunner(ctx, root, instance)
		if err != nil {
			return err
		}
	}
	return owner.setPasswordAtService(ctx, secretService(root), instance, password)
}

func (r Runner) setPasswordAtService(ctx context.Context, service, instance, password string) error {
	return r.Call(ctx, map[string]string{"password": password}, nil, "secret", "set", "--service", service, "--account", instance)
}

func (r Runner) Password(ctx context.Context, root, instance string) (string, error) {
	owner, err := r.credentialRunner(ctx, root, instance)
	if err != nil {
		return "", err
	}
	return owner.passwordAtService(ctx, secretService(root), instance)
}

func (r Runner) passwordAtService(ctx context.Context, service, instance string) (string, error) {
	var result struct {
		Password string `json:"password"`
	}
	// Secrets must not be echoed through a progress writer.
	r.Progress = nil
	err := r.Call(ctx, nil, &result, "secret", "get", "--service", service, "--account", instance)
	if err == nil && result.Password == "" {
		err = secretCommandError([]string{"secret", "get"}, "invalid_response")
	}
	if err != nil {
		return "", err
	}
	return result.Password, err
}

func (r Runner) DeletePassword(ctx context.Context, root, instance string) error {
	owner, err := r.credentialRunner(ctx, root, instance)
	if err != nil {
		return err
	}
	record, err := readCredentialRunnerRecord(root, instance)
	if err != nil {
		return err
	}
	if record != nil && record.MigrationFrom != "" {
		original, err := retainedCredentialRunner(ctx, root, record.MigrationFrom)
		if err != nil {
			return err
		}
		// Destroy/reset after an interrupted publication must also remove the
		// encrypted recovery item before dropping the only source reference.
		if err := original.deletePasswordAtService(ctx, secretService(root)+".migration", instance); err != nil {
			return err
		}
	}
	if err := owner.deletePasswordAtService(ctx, secretService(root), instance); err != nil {
		return err
	}
	return saveCredentialRunnerRecord(root, instance, credentialRunnerRecord{})
}

func (r Runner) deletePasswordAtService(ctx context.Context, service, instance string) error {
	return r.Call(ctx, nil, nil, "secret", "delete", "--service", service, "--account", instance)
}
