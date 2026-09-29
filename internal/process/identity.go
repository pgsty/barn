// Package process captures conservative QEMU process identity. QMP remains the
// primary runtime identity; these facts are the fallback and audit record.
package process

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/qemu"
)

type Identity struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
	Started    string `json:"started"`
	ArgvHash   string `json:"argv_hash"`
}

var numericStartPattern = regexp.MustCompile(`^(?:procstat:[0-9]+|kinfo:[0-9]+\.[0-9]{6})$`)

// ExpectedArgvHash binds one native, NUL-delimited process argv to the exact
// typed invocation Barn persisted before launch; no locale rendering or
// shell parsing is involved.
func ExpectedArgvHash(invocation qemu.Invocation) string {
	parts := make([]string, 0, len(invocation.Args)+1)
	parts = append(parts, invocation.Binary)
	parts = append(parts, invocation.Args...)
	return hashArgv(parts)
}

// Compatibility expiry: process-start-v0 in CONTRIBUTING.md#compatibility-expiry.
// IsLegacyStart reports the pre-0.1 process birth encoding produced by
// `ps -o lstart`. Known-but-malformed numeric prefixes fail closed instead of
// being reinterpreted as locale-dependent legacy text.
func IsLegacyStart(started string) bool {
	return started != "" && !numericStartPattern.MatchString(started) &&
		!strings.HasPrefix(started, "procstat:") && !strings.HasPrefix(started, "kinfo:")
}

func hashArgv(argv []string) string {
	var encoded bytes.Buffer
	for _, argument := range argv {
		encoded.WriteString(argument)
		encoded.WriteByte(0)
	}
	digest := sha256.Sum256(encoded.Bytes())
	return hex.EncodeToString(digest[:])
}

func observeLegacyCommand(ctx context.Context, runner execx.Runner, invocation qemu.Invocation, pid int) (string, string, error) {
	if runner == nil || pid <= 0 {
		return "", "", errors.New("process runner and positive PID are required")
	}
	psPath, err := exec.LookPath("ps")
	if err != nil {
		return "", "", err
	}
	result, err := runner.Run(ctx, psPath, "-ww", "-p", strconv.Itoa(pid), "-o", "command=")
	if err != nil {
		return "", "", err
	}
	commandLine := strings.TrimSpace(string(result.Stdout))
	fields := strings.Fields(commandLine)
	if len(fields) == 0 {
		return "", "", errors.New("process command line is empty")
	}
	executable := fields[0]
	hash := sha256.Sum256([]byte(commandLine))
	if filepath.Base(executable) != filepath.Base(invocation.Binary) {
		return "", "", fmt.Errorf("process executable %q does not match QEMU %q", executable, invocation.Binary)
	}
	return executable, hex.EncodeToString(hash[:]), nil
}

func captureLegacy(ctx context.Context, runner execx.Runner, invocation qemu.Invocation, pid int) (Identity, error) {
	executable, argvHash, err := observeLegacyCommand(ctx, runner, invocation, pid)
	if err != nil {
		return Identity{}, err
	}
	psPath, err := exec.LookPath("ps")
	if err != nil {
		return Identity{}, err
	}
	result, err := runner.Run(ctx, psPath, "-ww", "-p", strconv.Itoa(pid), "-o", "lstart=")
	if err != nil {
		return Identity{}, err
	}
	started := strings.TrimSpace(string(result.Stdout))
	if started == "" {
		return Identity{}, errors.New("process start time is empty")
	}
	return Identity{PID: pid, Executable: executable, Started: started, ArgvHash: argvHash}, nil
}

// Capture records a locale- and timezone-independent process birth identity.
func Capture(ctx context.Context, runner execx.Runner, invocation qemu.Invocation, pid int) (Identity, error) {
	if runner == nil || pid <= 0 {
		return Identity{}, errors.New("process runner and positive PID are required")
	}
	argv, err := processArgv(pid)
	if err != nil {
		return Identity{}, err
	}
	if len(argv) == 0 || argv[0] == "" {
		return Identity{}, errors.New("process argv is empty")
	}
	executable := argv[0]
	if filepath.Base(executable) != filepath.Base(invocation.Binary) {
		return Identity{}, fmt.Errorf("process executable %q does not match QEMU %q", executable, invocation.Binary)
	}
	argvHash := hashArgv(argv)
	started, err := processStarted(pid)
	if err != nil {
		return Identity{}, err
	}
	if !numericStartPattern.MatchString(started) {
		return Identity{}, errors.New("process start identity is malformed")
	}
	return Identity{PID: pid, Executable: executable, Started: started, ArgvHash: argvHash}, nil
}

func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

func MatchesLive(ctx context.Context, runner execx.Runner, identity Identity, invocation qemu.Invocation) bool {
	if !Alive(identity.PID) || identity.Executable == "" || identity.Started == "" || identity.ArgvHash == "" {
		return false
	}
	var current Identity
	var err error
	if IsLegacyStart(identity.Started) {
		current, err = captureLegacy(ctx, runner, invocation, identity.PID)
	} else {
		current, err = Capture(ctx, runner, invocation, identity.PID)
	}
	return err == nil && current == identity
}

// Verdict is what a recorded QEMU PID proves about the process holding it now.
type Verdict int

const (
	Dead    Verdict = iota // no process holds the PID
	Ours                   // the recorded process: executable, start time, and argv match
	Foreign                // another process reused the PID after a reboot or an exit
	Unknown                // a live process whose identity could not be read
)

// Observe classifies a recorded PID. A different start time or executable
// proves reuse (any PID's start time is readable, even after a reboot); only
// an unreadable live process stays Unknown, and callers keep refusing to act
// on it.
func Observe(ctx context.Context, runner execx.Runner, identity Identity, invocation qemu.Invocation) Verdict {
	if !Alive(identity.PID) {
		return Dead
	}
	if MatchesLive(ctx, runner, identity, invocation) {
		return Ours
	}
	if identity.Started != "" && !IsLegacyStart(identity.Started) {
		if started, err := processStarted(identity.PID); err == nil && started != identity.Started {
			return Foreign
		}
	}
	if argv, err := processArgv(identity.PID); err == nil && len(argv) != 0 && identity.Executable != "" && filepath.Base(argv[0]) != filepath.Base(identity.Executable) {
		return Foreign
	}
	if !Alive(identity.PID) {
		return Dead
	}
	return Unknown
}

func parseDarwinProcArgs(data []byte) ([]string, error) {
	if len(data) < 4 {
		return nil, errors.New("darwin process arguments lack argc")
	}
	argc := int(int32(binary.LittleEndian.Uint32(data[:4])))
	if argc <= 0 || argc > 4096 {
		return nil, errors.New("darwin process argc is invalid")
	}
	data = data[4:]
	executableEnd := bytes.IndexByte(data, 0)
	if executableEnd < 0 {
		return nil, errors.New("darwin process arguments lack executable terminator")
	}
	data = data[executableEnd+1:]
	for len(data) > 0 && data[0] == 0 {
		data = data[1:]
	}
	argv := make([]string, 0, argc)
	for len(argv) < argc {
		end := bytes.IndexByte(data, 0)
		if end < 0 {
			return nil, errors.New("darwin process arguments end before argc")
		}
		argv = append(argv, string(data[:end]))
		data = data[end+1:]
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("darwin process argv has no executable")
	}
	return argv, nil
}
