package darwin

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/qmp"
)

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func cleanupIntegrationClose(t *testing.T, name string, closer interface{ Close() error }) {
	t.Helper()
	t.Cleanup(func() {
		if err := closer.Close(); err != nil {
			t.Errorf("close %s: %v", name, err)
		}
	})
}

func waitForPath(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("path did not appear: %s", path)
}

func startNetworkQEMU(t *testing.T, ctx context.Context, binary, qmpSocket string, netArgs []string, extraFiles []*os.File) (*exec.Cmd, *lockedBuffer) {
	t.Helper()
	args := []string{
		"-machine", "virt",
		"-accel", "hvf",
		"-cpu", "host",
		"-display", "none",
		"-nodefaults",
		"-name", "barn-network-probe",
		"-uuid", "018f4b8e-1234-7abc-9def-0123456789ab",
		"-qmp", "unix:" + qmpSocket + ",server=on,wait=off",
		"-S",
	}
	args = append(args, netArgs...)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.ExtraFiles = extraFiles
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start QEMU network probe: %v: %s", err, stderr.String())
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	return cmd, stderr
}

func waitForQMPName(ctx context.Context, socket, expected string, timeout time.Duration) (qmp.Name, error) {
	deadline := time.Now().Add(timeout)
	client := qmp.Client{Timeout: 250 * time.Millisecond}
	var lastErr error
	for time.Now().Before(deadline) {
		name, err := client.QueryName(ctx, socket)
		if err == nil {
			if name.Name != expected {
				return name, fmt.Errorf("unexpected QMP name %q", name.Name)
			}
			return name, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return qmp.Name{}, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return qmp.Name{}, fmt.Errorf("QMP identity did not become ready: %w", lastErr)
}

func quitNetworkQEMU(t *testing.T, ctx context.Context, socket string, cmd *exec.Cmd, stderr *lockedBuffer) {
	t.Helper()
	name, err := waitForQMPName(ctx, socket, "barn-network-probe", 3*time.Second)
	if err != nil {
		t.Fatalf("query real QEMU name: %v: %s", err, stderr.String())
	}
	if name.Name != "barn-network-probe" {
		t.Fatalf("unexpected QMP name %#v", name)
	}
	if err := (&qmp.Client{Timeout: 2 * time.Second}).Quit(ctx, socket); err != nil {
		t.Fatalf("quit real QEMU: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait real QEMU: %v: %s", err, stderr.String())
	}
}

func TestIntegrationQEMUStreamReconnectMS(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("native macOS arm64 probe")
	}
	binary, err := exec.LookPath("qemu-system-aarch64")
	if err != nil {
		t.Skip("qemu-system-aarch64 is not installed")
	}
	dir, err := os.MkdirTemp("/tmp", "barn-stream-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	serverSocket := filepath.Join(dir, "peer.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: serverSocket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	qmpSocket := filepath.Join(dir, "qmp.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, stderr := startNetworkQEMU(t, ctx, binary, qmpSocket, []string{
		"-netdev", "stream,id=private,server=off,addr.type=unix,addr.path=" + serverSocket + ",reconnect-ms=100",
		"-device", "virtio-net-pci,netdev=private,mac=02:11:22:33:44:55",
	}, nil)
	if err := listener.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	first, err := listener.AcceptUnix()
	if err != nil {
		t.Fatalf("accept initial stream connection: %v: %s", err, stderr.String())
	}
	_ = first.Close()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(serverSocket); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	restarted, err := net.ListenUnix("unix", &net.UnixAddr{Name: serverSocket, Net: "unix"})
	if err != nil {
		t.Fatalf("restart fake daemon listener: %v", err)
	}
	cleanupIntegrationClose(t, "restarted stream listener", restarted)
	if err := restarted.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := restarted.AcceptUnix()
	if err != nil {
		t.Fatalf("accept reconnected stream: %v: %s", err, stderr.String())
	}
	_ = second.Close()
	if err := waitForPath(qmpSocket, 3*time.Second); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	quitNetworkQEMU(t, ctx, qmpSocket, cmd, stderr)
}

func TestIntegrationQEMUSocketFD3SurvivesDaemonize(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("native macOS arm64 probe")
	}
	binary, err := exec.LookPath("qemu-system-aarch64")
	if err != nil {
		t.Skip("qemu-system-aarch64 is not installed")
	}
	dir, err := os.MkdirTemp("/tmp", "barn-fd-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	serverSocket := filepath.Join(dir, "peer.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: serverSocket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	cleanupIntegrationClose(t, "daemonize stream listener", listener)
	dialed, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: serverSocket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	cleanupIntegrationClose(t, "daemonize client connection", dialed)
	accepted, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	cleanupIntegrationClose(t, "daemonize server connection", accepted)
	fdFile, err := dialed.File()
	if err != nil {
		t.Fatal(err)
	}
	cleanupIntegrationClose(t, "daemonize inherited file", fdFile)
	qmpSocket := filepath.Join(dir, "qmp.sock")
	pidfile := filepath.Join(dir, "qemu.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary,
		"-machine", "virt", "-accel", "hvf", "-cpu", "host",
		"-display", "none", "-nodefaults", "-S", "-daemonize",
		"-name", "barn-network-probe",
		"-uuid", "018f4b8e-1234-7abc-9def-0123456789ab",
		"-qmp", "unix:"+qmpSocket+",server=on,wait=off",
		"-pidfile", pidfile,
		"-netdev", "socket,id=private,fd=3",
		"-device", "virtio-net-pci,netdev=private,mac=02:11:22:33:44:57",
	)
	cmd.ExtraFiles = []*os.File{fdFile}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("daemonize QEMU with fd=3: %v: %s", err, stderr.String())
	}
	if err := waitForPath(qmpSocket, 3*time.Second); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	pidBytes, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(pid)
	if err != nil || process.Signal(syscall.Signal(0)) != nil {
		t.Fatalf("daemonized QEMU pid %d is not alive", pid)
	}
	client := qmp.Client{Timeout: 2 * time.Second}
	name, err := client.QueryName(ctx, qmpSocket)
	if err != nil || name.Name != "barn-network-probe" {
		t.Fatalf("daemonized QMP identity: %#v %v", name, err)
	}
	if err := client.Quit(ctx, qmpSocket); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := process.Signal(syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("daemonized QEMU pid %d remained after QMP quit", pid)
}

func TestIntegrationQEMUSocketFD3Fallback(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("native macOS arm64 probe")
	}
	binary, err := exec.LookPath("qemu-system-aarch64")
	if err != nil {
		t.Skip("qemu-system-aarch64 is not installed")
	}
	dir, err := os.MkdirTemp("/tmp", "barn-fd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	serverSocket := filepath.Join(dir, "peer.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: serverSocket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	cleanupIntegrationClose(t, "fallback stream listener", listener)
	dialed, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: serverSocket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	cleanupIntegrationClose(t, "fallback client connection", dialed)
	accepted, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	cleanupIntegrationClose(t, "fallback server connection", accepted)
	fdFile, err := dialed.File()
	if err != nil {
		t.Fatal(err)
	}
	cleanupIntegrationClose(t, "fallback inherited file", fdFile)
	qmpSocket := filepath.Join(dir, "qmp.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, stderr := startNetworkQEMU(t, ctx, binary, qmpSocket, []string{
		"-netdev", "socket,id=private,fd=3",
		"-device", "virtio-net-pci,netdev=private,mac=02:11:22:33:44:56",
	}, []*os.File{fdFile})
	if err := waitForPath(qmpSocket, 3*time.Second); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	quitNetworkQEMU(t, ctx, qmpSocket, cmd, stderr)
}
