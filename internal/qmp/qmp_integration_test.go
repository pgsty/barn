package qmp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/platform"
)

func TestIntegrationRealQEMUHandshakeIdentityAndQuit(t *testing.T) {
	profile, err := platform.Native()
	if err != nil {
		t.Skip(err)
	}
	binary, err := exec.LookPath(profile.QEMUBinary)
	if err != nil {
		t.Skip(profile.QEMUBinary + " is not installed")
	}
	dir, err := os.MkdirTemp("/tmp", "barn-qmp-int-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove QMP integration directory: %v", err)
		}
	})
	socket := filepath.Join(dir, "qmp.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary,
		"-machine", "none",
		"-nodefaults",
		"-display", "none",
		"-name", "barn-qmp-test",
		"-uuid", "018f4b8e-1234-7abc-9def-0123456789ab",
		"-qmp", "unix:"+socket+",server=on,wait=off",
		"-S",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start real QEMU: %v: %s", err, stderr.String())
	}
	defer func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	client := Client{Timeout: 2 * time.Second}
	var name Name
	deadline := time.Now().Add(5 * time.Second)
	for {
		// bind creates the socket before QEMU starts listening. Wait for a
		// real handshake instead of treating the directory entry as readiness.
		name, err = client.QueryName(ctx, socket)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("query-name: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("QMP did not become ready: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if name.Name != "barn-qmp-test" {
		t.Fatalf("query-name = %#v", name)
	}
	uuid, err := client.QueryUUID(ctx, socket)
	if err != nil {
		t.Fatalf("query-uuid: %v", err)
	}
	if uuid.UUID != "018f4b8e-1234-7abc-9def-0123456789ab" {
		t.Fatalf("query-uuid = %#v", uuid)
	}
	if err := client.Quit(ctx, socket); err != nil {
		t.Fatalf("quit: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait after quit: %v", err)
	}
}
