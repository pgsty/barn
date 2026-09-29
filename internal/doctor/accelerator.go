package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/identity"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/qmp"
)

type boundedOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	original := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = b.buffer.Write(data)
	}
	return original, nil
}

func (b *boundedOutput) String() string { return strings.TrimSpace(b.buffer.String()) }

func acceleratorSmoke(ctx context.Context, qemuPath string, profile platform.Profile) (string, error) {
	parent := "/tmp"
	if runtime.GOOS == "darwin" {
		parent = "/private/tmp"
	}
	root, err := os.MkdirTemp(parent, "barn-accel-smoke.")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.Remove(root)
		return "", err
	}
	qmpPath := filepath.Join(root, "qmp.sock")
	defer func() {
		_ = os.Remove(qmpPath)
		_ = os.Remove(root)
	}()
	uuid, err := identity.NewUUID()
	if err != nil {
		return "", err
	}
	const name = "barn-doctor-accelerator"
	smokeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := []string{
		"-name", name, "-uuid", uuid,
		"-machine", profile.Machine + ",accel=" + profile.Accelerator,
		"-cpu", profile.CPU, "-m", "64",
		"-display", "none", "-nodefaults", "-no-user-config", "-S",
		"-qmp", "unix:" + qmpPath + ",server=on,wait=off",
	}
	command := exec.CommandContext(smokeCtx, qemuPath, args...)
	output := &boundedOutput{limit: 64 << 10}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	finished := false
	defer func() {
		if !finished {
			cancel()
			<-done
		}
	}()
	client := &qmp.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	var identityErr error
	for time.Now().Before(deadline) {
		actualName, nameErr := client.QueryName(smokeCtx, qmpPath)
		actualUUID, uuidErr := client.QueryUUID(smokeCtx, qmpPath)
		if nameErr == nil && uuidErr == nil && actualName.Name == name && strings.EqualFold(actualUUID.UUID, uuid) {
			identityErr = nil
			break
		}
		identityErr = errors.Join(nameErr, uuidErr)
		if identityErr == nil {
			identityErr = fmt.Errorf("qmp identity mismatch: name=%q UUID=%q", actualName.Name, actualUUID.UUID)
		}
		select {
		case waitErr := <-done:
			finished = true
			if detail := output.String(); detail != "" {
				return "", fmt.Errorf("qemu exited before accelerator identity: %v: %s", waitErr, detail)
			}
			return "", fmt.Errorf("qemu exited before accelerator identity: %w", waitErr)
		case <-smokeCtx.Done():
			return "", smokeCtx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if identityErr != nil {
		return "", fmt.Errorf("accelerator QMP identity timeout: %w", identityErr)
	}
	if err := client.Quit(smokeCtx, qmpPath); err != nil {
		return "", err
	}
	select {
	case waitErr := <-done:
		finished = true
		if waitErr != nil {
			return "", fmt.Errorf("accelerator smoke QEMU exit: %w: %s", waitErr, output.String())
		}
	case <-smokeCtx.Done():
		return "", smokeCtx.Err()
	}
	return fmt.Sprintf("started %s with %s and matched QMP name/UUID", profile.Machine, profile.Accelerator), nil
}
