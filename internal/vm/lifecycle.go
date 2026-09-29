// Package vm runs the shared QEMU/QMP/SSH lifecycle used by product commands.
package vm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/openssh"
	"github.com/pgsty/barn/internal/process"
	"github.com/pgsty/barn/internal/qemu"
	"github.com/pgsty/barn/internal/qmp"
	"github.com/pgsty/barn/internal/state"
)

type Lifecycle struct {
	Runner            execx.Runner
	QMP               QMPClient
	SSHUser           string
	HostKeyAlias      string
	GuestSetupVersion string

	// The hooks below keep process signalling tests deterministic. Production
	// callers leave them nil and use the conservative process package plus the
	// operating system's signal primitive.
	matchesProcess func(context.Context, execx.Runner, process.Identity, qemu.Invocation) bool
	observeProcess func(context.Context, execx.Runner, process.Identity, qemu.Invocation) process.Verdict
	captureProcess func(context.Context, execx.Runner, qemu.Invocation, int) (process.Identity, error)
	processAlive   func(int) bool
	signalProcess  func(int, syscall.Signal) error
	waitProcess    func(context.Context, int, time.Duration) (bool, error)
	quitTimeout    time.Duration
	termTimeout    time.Duration
	killTimeout    time.Duration
}

// QMPClient is the lifecycle subset of QMP. The concrete qmp.Client satisfies
// it; the narrow interface also lets stop safety be tested without a live VM.
type QMPClient interface {
	QueryName(context.Context, string) (qmp.Name, error)
	QueryUUID(context.Context, string) (qmp.UUID, error)
	Powerdown(context.Context, string) error
	Quit(context.Context, string) error
}

var ErrQMPIdentityMismatch = errors.New("QMP identity mismatch")

const (
	defaultProcessPollInterval = 100 * time.Millisecond
	defaultQMPQuitTimeout      = 10 * time.Second
	defaultSIGTERMTimeout      = 10 * time.Second
	defaultSIGKILLTimeout      = 5 * time.Second
	defaultStartAbortTimeout   = 30 * time.Second
	// GracefulGuestShutdownTimeout covers ordinary systemd shutdown on a
	// provisioned Pigsty node. Services such as Vector may use a 90-second
	// stop timeout; forcing QEMU at the former 60-second boundary risks an
	// unclean database/filesystem shutdown.
	GracefulGuestShutdownTimeout = 2 * time.Minute
)

var sshUserPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func (l Lifecycle) sshUser() string {
	if l.SSHUser == "" {
		return "dba"
	}
	return l.SSHUser
}

type ReadyMarker struct {
	Node       string `json:"node"`
	Generation uint64 `json:"generation"`
	SpecHash   string `json:"spec_hash"`
}

type bootstrapErrorMarker struct {
	ExitStatus int    `json:"exit_status"`
	Stage      string `json:"stage"`
	Detail     string `json:"detail,omitempty"`
}

// BootstrapError means SSH succeeded but guest initialization stopped. Waiting
// for SSH again cannot rerun cloud-init or repair the failed initialization.
type BootstrapError struct {
	Stage      string
	Detail     string
	ExitStatus int
}

func (e *BootstrapError) Error() string {
	if e.Stage == "" {
		return fmt.Sprintf("guest bootstrap failed (exit status %d)", e.ExitStatus)
	}
	if detail := strings.TrimSpace(e.Detail); detail != "" {
		return fmt.Sprintf("guest bootstrap failed during %s: %s", e.Stage, detail)
	}
	return fmt.Sprintf("guest bootstrap failed during %s (exit status %d)", e.Stage, e.ExitStatus)
}

func (l Lifecycle) validate() error {
	if l.Runner == nil || l.QMP == nil {
		return errors.New("VM lifecycle requires command runner and QMP client")
	}
	return nil
}

func (l Lifecycle) ValidateIdentity(ctx context.Context, socket, name, uuid string) error {
	if err := l.validate(); err != nil {
		return err
	}
	actualName, err := l.QMP.QueryName(ctx, socket)
	if err != nil {
		return err
	}
	actualUUID, err := l.QMP.QueryUUID(ctx, socket)
	if err != nil {
		return err
	}
	if actualName.Name != name || !strings.EqualFold(actualUUID.UUID, uuid) {
		return fmt.Errorf("%w: name=%q uuid=%q", ErrQMPIdentityMismatch, actualName.Name, actualUUID.UUID)
	}
	return nil
}

func (l Lifecycle) WaitQMP(ctx context.Context, socket, name, uuid string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := l.ValidateIdentity(ctx, socket, name, uuid); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("QMP identity did not become ready: %w", lastErr)
}

func readPID(path string) (int, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > 32 {
		return 0, errors.New("QEMU pidfile is unsafe")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid QEMU pidfile %q", strings.TrimSpace(string(data)))
	}
	return pid, nil
}

func (l Lifecycle) Start(ctx context.Context, invocation qemu.Invocation, socket, pidfile, name, uuid string) (process.Identity, error) {
	return l.start(ctx, invocation, socket, pidfile, name, uuid, nil)
}

func (l Lifecycle) StartWithExtraFiles(ctx context.Context, invocation qemu.Invocation, socket, pidfile, name, uuid string, files []*os.File) (process.Identity, error) {
	if len(files) == 0 {
		return process.Identity{}, errors.New("VM lifecycle extra-file start requires at least one file")
	}
	return l.start(ctx, invocation, socket, pidfile, name, uuid, files)
}

func (l Lifecycle) start(ctx context.Context, invocation qemu.Invocation, socket, pidfile, name, uuid string, files []*os.File) (process.Identity, error) {
	if err := l.validate(); err != nil {
		return process.Identity{}, err
	}
	var runErr error
	if len(files) == 0 {
		_, runErr = l.Runner.Run(ctx, invocation.Binary, invocation.Args...)
	} else if runner, ok := l.Runner.(execx.ExtraFilesRunner); ok {
		_, runErr = runner.RunWithExtraFiles(ctx, invocation.Binary, files, invocation.Args...)
	} else {
		return process.Identity{}, errors.New("VM lifecycle runner cannot pass inherited files")
	}
	if runErr != nil {
		return process.Identity{}, l.compensateStartFailure(ctx, socket, pidfile, name, uuid, invocation, runErr)
	}
	if err := l.WaitQMP(ctx, socket, name, uuid, 15*time.Second); err != nil {
		return process.Identity{}, l.compensateStartFailure(ctx, socket, pidfile, name, uuid, invocation, err)
	}
	pid, err := readPID(pidfile)
	if err != nil {
		return process.Identity{}, l.compensateStartFailure(ctx, socket, pidfile, name, uuid, invocation, err)
	}
	identity, err := l.capture(ctx, invocation, pid)
	if err != nil {
		return process.Identity{}, l.compensateStartFailure(ctx, socket, pidfile, name, uuid, invocation, err)
	}
	if identity.ArgvHash != process.ExpectedArgvHash(invocation) || !l.matchesLive(ctx, identity, invocation) {
		err := errors.New("started QEMU process identity did not match invocation")
		return process.Identity{}, l.compensateStartFailure(ctx, socket, pidfile, name, uuid, invocation, err)
	}
	return identity, nil
}

func (l Lifecycle) compensateStartFailure(ctx context.Context, socket, pidfile, name, uuid string, invocation qemu.Invocation, startErr error) error {
	if abortErr := l.Abort(ctx, socket, pidfile, name, uuid, invocation); abortErr != nil {
		return fmt.Errorf("%w; start compensation failed: %v", startErr, abortErr)
	}
	return fmt.Errorf("%w; start compensation completed", startErr)
}

func (l Lifecycle) capture(ctx context.Context, invocation qemu.Invocation, pid int) (process.Identity, error) {
	if l.captureProcess != nil {
		return l.captureProcess(ctx, l.Runner, invocation, pid)
	}
	return process.Capture(ctx, l.Runner, invocation, pid)
}

func (l Lifecycle) captureAbortIdentity(ctx context.Context, pidfile string, invocation qemu.Invocation) (*process.Identity, error) {
	pid, err := readPID(pidfile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !l.alive(pid) {
		return nil, nil
	}
	identity, err := l.capture(ctx, invocation, pid)
	if err != nil {
		return nil, err
	}
	if identity.ArgvHash != process.ExpectedArgvHash(invocation) {
		return nil, errors.New("QEMU pidfile process does not match the persisted invocation")
	}
	second, err := l.capture(ctx, invocation, pid)
	if err != nil || second != identity {
		return nil, errors.New("QEMU pidfile process identity changed during compensation")
	}
	return &identity, nil
}

func (l Lifecycle) waitForQMPGone(ctx context.Context, socket, name, uuid string, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(defaultProcessPollInterval)
	defer ticker.Stop()
	for {
		identityErr := l.ValidateIdentity(ctx, socket, name, uuid)
		if identityErr != nil {
			if errors.Is(identityErr, ErrQMPIdentityMismatch) {
				return fmt.Errorf("QMP identity changed during start compensation: %w", identityErr)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-timer.C:
			return errors.New("matching QMP remained available after start compensation quit")
		}
	}
}

func (l Lifecycle) abortStarted(ctx context.Context, socket, name, uuid string, identity *process.Identity, invocation qemu.Invocation) error {
	qmpErr := l.ValidateIdentity(ctx, socket, name, uuid)
	if qmpErr == nil {
		if err := l.QMP.Quit(ctx, socket); err != nil {
			return fmt.Errorf("quit matching QMP during start compensation: %w", err)
		}
		if identity == nil {
			return l.waitForQMPGone(ctx, socket, name, uuid, l.duration(l.quitTimeout, defaultQMPQuitTimeout))
		}
		exited, err := l.waitForExit(ctx, identity.PID, l.duration(l.quitTimeout, defaultQMPQuitTimeout))
		if err != nil || exited {
			return err
		}
		identityErr := l.ValidateIdentity(ctx, socket, name, uuid)
		if identityErr == nil {
			return fmt.Errorf("QEMU pid %d remained after start compensation quit while matching QMP was still available", identity.PID)
		}
		if errors.Is(identityErr, ErrQMPIdentityMismatch) {
			return fmt.Errorf("QMP identity changed after start compensation quit: %w", identityErr)
		}
		return l.stopWithSignals(ctx, *identity, invocation, errors.New("QMP disappeared after start compensation quit"))
	}
	if errors.Is(qmpErr, ErrQMPIdentityMismatch) {
		return fmt.Errorf("will not clean up the failed start: QMP identity does not match: %w", qmpErr)
	}
	if identity == nil {
		return nil
	}
	return l.stopWithSignals(ctx, *identity, invocation, fmt.Errorf("QMP unavailable during start compensation: %w", qmpErr))
}

// Abort compensates a failed start from the exact QMP identity and, when QMP
// is unavailable, a stable pidfile process bound to the persisted invocation.
func (l Lifecycle) Abort(ctx context.Context, socket, pidfile, name, uuid string, invocation qemu.Invocation) error {
	if err := l.validate(); err != nil {
		return err
	}
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultStartAbortTimeout)
	defer cancel()
	identity, identityErr := l.captureAbortIdentity(abortCtx, pidfile, invocation)
	if identityErr != nil {
		// Matching QMP remains sufficient authority to quit the just-started VM;
		// retain the pidfile error if QMP cannot establish that authority.
		if qmpErr := l.ValidateIdentity(abortCtx, socket, name, uuid); qmpErr != nil {
			return fmt.Errorf("capture start compensation process: %v; QMP identity: %w", identityErr, qmpErr)
		}
		if err := l.QMP.Quit(abortCtx, socket); err != nil {
			return fmt.Errorf("quit matching QMP with unavailable process evidence: %w", err)
		}
		return l.waitForQMPGone(abortCtx, socket, name, uuid, l.duration(l.quitTimeout, defaultQMPQuitTimeout))
	}
	return l.abortStarted(abortCtx, socket, name, uuid, identity, invocation)
}

// AbortIdentity compensates after the caller captured an exact process but
// failed to persist it. The invocation hash is checked before any signal path.
func (l Lifecycle) AbortIdentity(ctx context.Context, socket, name, uuid string, identity process.Identity, invocation qemu.Invocation) error {
	if err := l.validate(); err != nil {
		return err
	}
	if identity.ArgvHash != process.ExpectedArgvHash(invocation) {
		return errors.New("will not clean up the failed start: the process is not the recorded QEMU invocation")
	}
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultStartAbortTimeout)
	defer cancel()
	return l.abortStarted(abortCtx, socket, name, uuid, &identity, invocation)
}

func (l Lifecycle) Stop(ctx context.Context, socket, name, uuid string, identity process.Identity, invocation qemu.Invocation, guestTimeout time.Duration) error {
	if err := l.validate(); err != nil {
		return err
	}
	if err := l.ValidateIdentity(ctx, socket, name, uuid); err != nil {
		if errors.Is(err, ErrQMPIdentityMismatch) {
			return fmt.Errorf("will not stop: QMP identity does not match: %w", err)
		}
		return l.stopWithSignals(ctx, identity, invocation, fmt.Errorf("QMP identity unavailable: %w", err))
	}
	if !l.matchesLive(ctx, identity, invocation) {
		return errors.New("will not power down: the process identity does not match")
	}
	if err := l.QMP.Powerdown(ctx, socket); err != nil {
		return l.handleQMPFailure(ctx, socket, name, uuid, identity, invocation, "QMP powerdown failed", err)
	}
	exited, err := l.waitForExit(ctx, identity.PID, guestTimeout)
	if err != nil {
		return err
	}
	if exited {
		return nil
	}
	if err := l.ValidateIdentity(ctx, socket, name, uuid); err != nil {
		if errors.Is(err, ErrQMPIdentityMismatch) {
			return fmt.Errorf("guest did not power down; refuse stop with mismatched QMP identity: %w", err)
		}
		return l.stopWithSignals(ctx, identity, invocation, fmt.Errorf("guest did not power down and QMP became unavailable: %w", err))
	}
	if !l.matchesLive(ctx, identity, invocation) {
		return errors.New("guest did not power down; refuse QMP quit without matching process identity")
	}
	if err := l.QMP.Quit(ctx, socket); err != nil {
		return l.handleQMPFailure(ctx, socket, name, uuid, identity, invocation, "QMP quit failed", err)
	}
	exited, err = l.waitForExit(ctx, identity.PID, l.duration(l.quitTimeout, defaultQMPQuitTimeout))
	if err != nil {
		return err
	}
	if exited {
		return nil
	}

	// A successful quit normally closes QMP before the process disappears. Do
	// not assume that happened: prove QMP is unavailable before entering the
	// signal-only fallback. A live or mismatched QMP remains fail-closed.
	if err := l.ValidateIdentity(ctx, socket, name, uuid); err != nil {
		if errors.Is(err, ErrQMPIdentityMismatch) {
			return fmt.Errorf("QEMU remained after QMP quit; refuse stop with mismatched QMP identity: %w", err)
		}
		return l.stopWithSignals(ctx, identity, invocation, fmt.Errorf("QEMU remained after QMP quit and QMP became unavailable: %w", err))
	}
	return fmt.Errorf("QEMU pid %d remained after QMP quit while matching QMP was still available; no signal was sent", identity.PID)
}

func (l Lifecycle) handleQMPFailure(ctx context.Context, socket, name, uuid string, identity process.Identity, invocation qemu.Invocation, operation string, operationErr error) error {
	identityErr := l.ValidateIdentity(ctx, socket, name, uuid)
	if identityErr == nil {
		return fmt.Errorf("%s while matching QMP remained available; no signal was sent: %w", operation, operationErr)
	}
	if errors.Is(identityErr, ErrQMPIdentityMismatch) {
		return fmt.Errorf("%s; refuse stop with mismatched QMP identity: %w", operation, identityErr)
	}
	return l.stopWithSignals(ctx, identity, invocation, fmt.Errorf("%s and QMP became unavailable: %w", operation, operationErr))
}

func (l Lifecycle) stopWithSignals(ctx context.Context, identity process.Identity, invocation qemu.Invocation, reason error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if identity.PID <= 0 || identity.Executable == "" || identity.Started == "" || identity.ArgvHash == "" || invocation.Binary == "" {
		return fmt.Errorf("will not signal QEMU: its recorded process identity or invocation is incomplete (%v)", reason)
	}
	if !l.alive(identity.PID) {
		return nil
	}
	if !l.matchesLive(ctx, identity, invocation) {
		if l.observe(ctx, identity, invocation) == process.Foreign {
			// QMP is gone and the PID now belongs to another process (a reboot
			// or reuse): the recorded QEMU has already exited. Never signal it.
			return nil
		}
		return fmt.Errorf("will not signal QEMU: the process identity does not match (%v)", reason)
	}
	if err := l.signal(identity.PID, syscall.SIGTERM); err != nil {
		if !l.alive(identity.PID) {
			return nil
		}
		return fmt.Errorf("send SIGTERM to verified QEMU pid %d: %w", identity.PID, err)
	}
	exited, err := l.waitForExit(ctx, identity.PID, l.duration(l.termTimeout, defaultSIGTERMTimeout))
	if err != nil {
		return err
	}
	if exited {
		return nil
	}

	// Re-capture every identity field immediately before SIGKILL. This is the
	// critical PID-reuse barrier after the bounded SIGTERM wait.
	if err := ctx.Err(); err != nil {
		return err
	}
	if !l.matchesLive(ctx, identity, invocation) {
		return fmt.Errorf("will not send SIGKILL: the QEMU process identity changed after SIGTERM (%v)", reason)
	}
	if err := l.signal(identity.PID, syscall.SIGKILL); err != nil {
		if !l.alive(identity.PID) {
			return nil
		}
		return fmt.Errorf("send SIGKILL to verified QEMU pid %d: %w", identity.PID, err)
	}
	exited, err = l.waitForExit(ctx, identity.PID, l.duration(l.killTimeout, defaultSIGKILLTimeout))
	if err != nil {
		return err
	}
	if !exited {
		return fmt.Errorf("verified QEMU pid %d remained after bounded SIGKILL wait", identity.PID)
	}
	return nil
}

func (l Lifecycle) matchesLive(ctx context.Context, identity process.Identity, invocation qemu.Invocation) bool {
	if l.matchesProcess != nil {
		return l.matchesProcess(ctx, l.Runner, identity, invocation)
	}
	return process.MatchesLive(ctx, l.Runner, identity, invocation)
}

func (l Lifecycle) observe(ctx context.Context, identity process.Identity, invocation qemu.Invocation) process.Verdict {
	if l.observeProcess != nil {
		return l.observeProcess(ctx, l.Runner, identity, invocation)
	}
	if l.matchesProcess != nil {
		// Tests that fake identity matching must also fake reuse evidence.
		return process.Unknown
	}
	return process.Observe(ctx, l.Runner, identity, invocation)
}

func (l Lifecycle) alive(pid int) bool {
	if l.processAlive != nil {
		return l.processAlive(pid)
	}
	return process.Alive(pid)
}

func (l Lifecycle) signal(pid int, signal syscall.Signal) error {
	if l.signalProcess != nil {
		return l.signalProcess(pid, signal)
	}
	target, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return target.Signal(signal)
}

func (l Lifecycle) waitForExit(ctx context.Context, pid int, timeout time.Duration) (bool, error) {
	if l.waitProcess != nil {
		return l.waitProcess(ctx, pid, timeout)
	}
	if !l.alive(pid) {
		return true, nil
	}
	if timeout <= 0 {
		return false, nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(defaultProcessPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
			if !l.alive(pid) {
				return true, nil
			}
		case <-timer.C:
			return !l.alive(pid), nil
		}
	}
}

func (Lifecycle) duration(configured, fallback time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return fallback
}

func SSHArgsForUser(user, key, knownHosts string, port uint16, command ...string) []string {
	return SSHArgsForInstance(user, key, knownHosts, "", port, command...)
}

// The forwarding port is reusable; trust belongs to the VM instance instead.
func HostKeyAlias(uuid string) string {
	if uuid == "" {
		return ""
	}
	return fmt.Sprintf("barn-%x", sha256.Sum256([]byte(strings.ToLower(uuid))))
}

func SSHArgsForInstance(user, key, knownHosts, alias string, port uint16, command ...string) []string {
	if !sshUserPattern.MatchString(user) {
		return nil
	}
	knownHostsOption, err := openssh.QuoteConfigValue(knownHosts)
	if err != nil {
		return nil
	}
	args := []string{
		"-F", "/dev/null", "-i", key,
		"-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "LogLevel=ERROR",
		"-o", "UserKnownHostsFile=" + knownHostsOption,
		"-o", "ConnectTimeout=5", "-p", strconv.Itoa(int(port)), user + "@127.0.0.1",
	}
	if len(command) > 0 {
		args = append(args, QuoteRemote(command))
	}
	if alias != "" {
		quoted, err := openssh.QuoteConfigValue(alias)
		if err != nil {
			return nil
		}
		args = append([]string{"-o", "HostKeyAlias=" + quoted}, args...)
	}
	return args
}

// QuoteRemote preserves argv boundaries across OpenSSH's string-valued exec
// request. Each argument is single-quoted and embedded quotes use the standard
// close/escaped-quote/reopen sequence.
func QuoteRemote(command []string) string {
	quoted := make([]string, 0, len(command))
	for _, argument := range command {
		quoted = append(quoted, "'"+strings.ReplaceAll(argument, "'", "'\"'\"'")+"'")
	}
	return strings.Join(quoted, " ")
}

func (l Lifecycle) WaitReady(parent context.Context, sshPath, key, knownHosts string, port uint16, expected ReadyMarker, timeout time.Duration) ([]state.GuestWarning, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		args := SSHArgsForInstance(l.sshUser(), key, knownHosts, l.HostKeyAlias, port, "cat", "/var/lib/barn/ready.json")
		if args == nil {
			return nil, fmt.Errorf("invalid SSH user %q", l.sshUser())
		}
		result, err := l.Runner.Run(ctx, sshPath, args...)
		if err == nil {
			var marker struct {
				ReadyMarker
				Warnings     []state.GuestWarning `json:"warnings,omitempty"`
				SetupVersion string               `json:"setup_version"`
			}
			if decodeErr := json.Unmarshal(result.Stdout, &marker); decodeErr == nil && marker.ReadyMarker == expected {
				if l.GuestSetupVersion != "" && marker.SetupVersion != l.GuestSetupVersion {
					marker.Warnings = append(marker.Warnings, state.GuestWarning{Stage: "setup-update", Detail: "guest setup helpers need an update"})
				}
				return marker.Warnings, nil
			} else if decodeErr != nil {
				lastErr = decodeErr
			} else {
				lastErr = fmt.Errorf("guest reports ready marker generation %d, expected %d", marker.Generation, expected.Generation)
			}
		} else {
			lastErr = err
			if permanentSSHError(result.Stderr) {
				return nil, fmt.Errorf("SSH access refused: %s", readinessDetail(err))
			}
		}
		errorArgs := SSHArgsForInstance(l.sshUser(), key, knownHosts, l.HostKeyAlias, port, "cat", "/var/lib/barn/error.json")
		errorResult, errorErr := l.Runner.Run(ctx, sshPath, errorArgs...)
		if errorErr == nil {
			var marker bootstrapErrorMarker
			if decodeErr := json.Unmarshal(errorResult.Stdout, &marker); decodeErr != nil || marker.ExitStatus <= 0 || len(marker.Stage) > 64 {
				return nil, errors.New("guest bootstrap failed and left an unreadable error marker")
			}
			return nil, &BootstrapError{Stage: marker.Stage, Detail: marker.Detail, ExitStatus: marker.ExitStatus}
		}
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return nil, parent.Err()
			}
			return nil, fmt.Errorf("guest did not become ready within %s: %s", timeout, readinessDetail(lastErr))
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("guest did not become ready within %s: %s", timeout, readinessDetail(lastErr))
}

func permanentSSHError(stderr []byte) bool {
	message := strings.ToLower(string(stderr))
	for _, marker := range []string{"host key verification failed", "remote host identification has changed", "permission denied (", "bad configuration option", "bad permissions", "unprotected private key file", "no matching host key type"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// readinessDetail keeps the part of the last probe error a user can act on:
// ssh's final stderr line, not the probe's argument list.
func readinessDetail(err error) string {
	var command *execx.CommandError
	if errors.As(err, &command) {
		if detail := strings.TrimSpace(command.Stderr); detail != "" {
			lines := strings.Split(detail, "\n")
			return strings.TrimSpace(lines[len(lines)-1])
		}
		if command.Cause != nil {
			return command.Cause.Error()
		}
		return fmt.Sprintf("ssh exited with code %d", command.ExitCode)
	}
	if err == nil {
		return "no response from the guest"
	}
	return err.Error()
}
