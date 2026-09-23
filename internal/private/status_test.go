package private

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pgsty/farrow/internal/execx"
	"github.com/pgsty/farrow/internal/process"
	"github.com/pgsty/farrow/internal/qemu"
	"github.com/pgsty/farrow/internal/state"
	"github.com/pgsty/farrow/internal/vm"
)

func statusFixture(t *testing.T) (StartConfig, state.Store) {
	t.Helper()
	config, _ := preparedStartFixture(t)
	t.Setenv("FARROW_HOME", config.Deployment.Root)
	return config, state.Store{Root: config.Deployment.Root}
}

func TestSelectedStatusConnectionAndStopIgnoreDegradedPeer(t *testing.T) {
	_, store := statusFixture(t)
	meta, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	meta.Phase = state.Running
	meta.Process = state.ProcessIdentity{}
	meta.Runtime = state.RuntimePaths{}
	if err := store.WriteNode(meta); err != nil {
		t.Fatal(err)
	}
	peer, err := store.ReadNode("node-1")
	if err != nil {
		t.Fatal(err)
	}
	peer.Phase = state.Stopped
	peer.Process = state.ProcessIdentity{}
	if err := store.WriteNode(peer); err != nil {
		t.Fatal(err)
	}

	selected := Manager{FarrowVersion: "test", Nodes: []string{"node-1"}}
	status, err := selected.Status(context.Background())
	if err != nil || len(status.Nodes) != 1 || status.Nodes[0].Name != "node-1" {
		t.Fatalf("selected status = %#v, %v", status, err)
	}
	if status, err := (Manager{FarrowVersion: "test"}).Status(context.Background()); err == nil || !strings.Contains(err.Error(), "meta") || len(status.Nodes) != 2 || status.Nodes[0].Error == "" || status.Nodes[1].Error != "" {
		t.Fatalf("whole status = %#v, %v, want both nodes with degraded meta", status, err)
	}
	if _, err := (Manager{FarrowVersion: "test"}).Connection(context.Background(), "node-1"); err == nil || !strings.Contains(err.Error(), "node-1 is not running") || strings.Contains(err.Error(), "meta") {
		t.Fatalf("selected connection error = %v", err)
	}
	if status, err := selected.Stop(context.Background()); err != nil || len(status.Nodes) != 1 || status.Nodes[0].Name != "node-1" {
		t.Fatalf("selected idempotent stop = %#v, %v", status, err)
	}
}

func TestStatusReportsDesiredNodeWithoutCommittedStateAsAbsent(t *testing.T) {
	config, _ := statusFixture(t)
	if err := os.Remove(filepath.Join(config.Deployment.Root, "nodes", "node-1", "state.json")); err != nil {
		t.Fatal(err)
	}
	status, err := (Manager{FarrowVersion: "test"}).Status(context.Background())
	if err != nil || len(status.Nodes) != 2 {
		t.Fatalf("partial deployment status = %#v, %v", status, err)
	}
	if status.Nodes[1].Name != "node-1" || status.Nodes[1].State != state.Absent || status.Nodes[1].Runtime != "absent" || status.Nodes[1].SSHPort != 0 {
		t.Fatalf("missing desired node status = %#v", status.Nodes[1])
	}
	selected, err := (Manager{FarrowVersion: "test", Nodes: []string{"node-1"}}).Status(context.Background())
	if err != nil || len(selected.Nodes) != 1 || selected.Nodes[0].State != state.Absent {
		t.Fatalf("selected missing node status = %#v, %v", selected, err)
	}
}

func TestDefaultConnectionSkipsMissingControlNode(t *testing.T) {
	config, _ := statusFixture(t)
	if err := os.Remove(filepath.Join(config.Deployment.Root, "nodes", "meta", "state.json")); err != nil {
		t.Fatal(err)
	}
	_, err := (Manager{FarrowVersion: "test"}).Connection(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "node-1 is not running") || strings.Contains(err.Error(), "meta") {
		t.Fatalf("default partial connection error = %v", err)
	}
}

func startStatusProcess(t *testing.T) (*exec.Cmd, qemu.Invocation) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep is not installed")
	}
	command := exec.Command(sleep, "121")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})
	return command, qemu.Invocation{Binary: sleep, Args: []string{"121"}}
}

func shortRuntimeBase(t *testing.T, prefix string) string {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(canonical) })
	return canonical
}

func legacyIdentityForProcess(t *testing.T, command *exec.Cmd, invocation qemu.Invocation) state.ProcessIdentity {
	t.Helper()
	ps, err := exec.LookPath("ps")
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(ps, "-ww", "-p", strconv.Itoa(command.Process.Pid), "-o", "lstart=").Output()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(append([]string{invocation.Binary}, invocation.Args...), " ")
	digest := sha256.Sum256([]byte(joined))
	return state.ProcessIdentity{
		PID: command.Process.Pid, Executable: invocation.Binary,
		Started: strings.TrimSpace(string(output)), ArgvHash: hex.EncodeToString(digest[:]),
	}
}

func TestStatusMigratesMatchingLegacyIdentityBeforeRuntimeError(t *testing.T) {
	t.Setenv("LC_ALL", "C")
	t.Setenv("TZ", "UTC")
	_, store := statusFixture(t)
	command, invocation := startStatusProcess(t)
	node, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	base := shortRuntimeBase(t, "farrow-legacy-status-")
	node.Phase = state.Running
	node.Invocation = invocation
	node.Runtime = state.RuntimePaths{Directory: filepath.Join(base, "farrow", node.Node), QMP: filepath.Join(base, "farrow", node.Node, "qmp.sock"), PIDFile: filepath.Join(base, "farrow", node.Node, "qemu.pid")}
	node.Process = legacyIdentityForProcess(t, command, invocation)
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	_, statusErr := (Manager{FarrowVersion: "test", Nodes: []string{"meta"}}).Status(context.Background())
	if statusErr == nil || !strings.Contains(statusErr.Error(), "QMP is unavailable") {
		t.Fatalf("status error = %v", statusErr)
	}
	migrated, err := store.ReadNode("meta")
	if err != nil || process.IsLegacyStart(migrated.Process.Started) || migrated.Process.Started == node.Process.Started {
		t.Fatalf("migrated process = %#v, %v", migrated.Process, err)
	}
}

func TestStatusAdoptsQMPBoundInterruptedStart(t *testing.T) {
	_, store := statusFixture(t)
	command, invocation := startStatusProcess(t)
	node, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	base := shortRuntimeBase(t, "farrow-adopt-status-")
	directory := filepath.Join(base, "farrow", node.Node)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	node.Phase = state.Starting
	node.Invocation = invocation
	node.Process = state.ProcessIdentity{}
	node.Runtime = state.RuntimePaths{Directory: directory, QMP: filepath.Join(directory, "qmp.sock"), PIDFile: filepath.Join(directory, "qemu.pid")}
	if err := os.WriteFile(node.Runtime.PIDFile, []byte(fmt.Sprintf("%d\n", command.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	serveQMPIdentity(t, node.Runtime.QMP, node.Node, node.VMUUID)
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	status, err := (Manager{FarrowVersion: "test", Nodes: []string{"meta"}}).Status(context.Background())
	if err != nil || len(status.Nodes) != 1 || status.Nodes[0].State != state.Running || status.Nodes[0].ProcessID != command.Process.Pid || !strings.Contains(status.Message, "adopted interrupted start") {
		t.Fatalf("adopted status = %#v, %v", status, err)
	}
	adopted, err := store.ReadNode("meta")
	if err != nil || adopted.Phase != state.Running || adopted.Process.PID != command.Process.Pid || process.IsLegacyStart(adopted.Process.Started) {
		t.Fatalf("adopted state = %#v, %v", adopted, err)
	}
	manager := Manager{FarrowVersion: "test"}
	writeRecoveryKeys(t, store.Root)
	if _, _, _, err := manager.ensureKeys(context.Background(), Deployment{Root: store.Root}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Root, "keys", "known_hosts"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	connection, err := manager.Connection(context.Background(), "meta")
	if err != nil || connection.HostKeyAlias != vm.HostKeyAlias(node.VMUUID) {
		t.Fatalf("direct SSH lost verified instance identity: %+v %v", connection, err)
	}
}

func TestStatusCleansDeadInterruptedRuntimeBeforeConvergence(t *testing.T) {
	_, store := statusFixture(t)
	node, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	base := shortRuntimeBase(t, "farrow-dead-status-")
	directory := filepath.Join(base, "farrow", node.Node)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	node.Phase = state.Starting
	node.Process = state.ProcessIdentity{}
	node.Runtime = state.RuntimePaths{Directory: directory, QMP: filepath.Join(directory, "qmp.sock"), PIDFile: filepath.Join(directory, "qemu.pid")}
	if err := os.WriteFile(node.Runtime.PIDFile, []byte("999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	status, err := (Manager{FarrowVersion: "test", Nodes: []string{"meta"}}).Status(context.Background())
	if err != nil || len(status.Nodes) != 1 || status.Nodes[0].State != state.Stopped {
		t.Fatalf("dead convergence = %#v, %v", status, err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead runtime directory remains: %v", err)
	}
	stopped, err := store.ReadNode("meta")
	if err != nil || stopped.Phase != state.Stopped || stopped.Process != (state.ProcessIdentity{}) {
		t.Fatalf("stopped state = %#v, %v", stopped, err)
	}
}

func TestStatusCleansDeadRunningRuntimeBeforeSelfHalt(t *testing.T) {
	_, store := statusFixture(t)
	node, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	base := shortRuntimeBase(t, "farrow-dead-running-")
	directory := filepath.Join(base, "farrow", node.Node)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	node.Phase = state.Running
	node.Runtime = state.RuntimePaths{Directory: directory, QMP: filepath.Join(directory, "qmp.sock"), PIDFile: filepath.Join(directory, "qemu.pid")}
	node.Process = state.ProcessIdentity{PID: 999999999, Executable: node.Invocation.Binary, Started: "kinfo:1.000000", ArgvHash: process.ExpectedArgvHash(node.Invocation)}
	listener, err := net.Listen("unix", node.Runtime.QMP)
	if err != nil {
		t.Fatal(err)
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node.Runtime.PIDFile, []byte("999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	status, err := (Manager{FarrowVersion: "test", Nodes: []string{"meta"}}).Status(context.Background())
	if err != nil || len(status.Nodes) != 1 || status.Nodes[0].State != state.Stopped {
		t.Fatalf("running self-halt = %#v, %v", status, err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead running runtime directory remains: %v", err)
	}
}

// After a host reboot, a node recorded running can find its PID taken by an
// unrelated process. Its start time proves it is not the recorded QEMU, so
// status converges the node to stopped instead of blocking every command.
func TestStatusConvergesARunningNodeWhosePIDWasReused(t *testing.T) {
	_, store := statusFixture(t)
	node, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	stranger := exec.Command("sleep", "30")
	if err := stranger.Start(); err != nil {
		t.Skip("sleep is unavailable")
	}
	t.Cleanup(func() { _ = stranger.Process.Kill(); _, _ = stranger.Process.Wait() })
	base := shortRuntimeBase(t, "farrow-reused-pid-")
	directory := filepath.Join(base, "farrow", node.Node)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := stranger.Process.Pid
	node.Phase = state.Running
	node.Runtime = state.RuntimePaths{Directory: directory, QMP: filepath.Join(directory, "qmp.sock"), PIDFile: filepath.Join(directory, "qemu.pid")}
	node.Process = state.ProcessIdentity{PID: pid, Executable: node.Invocation.Binary, Started: "kinfo:1.000000", ArgvHash: process.ExpectedArgvHash(node.Invocation)}
	if err := os.WriteFile(node.Runtime.PIDFile, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	status, err := (Manager{FarrowVersion: "test", Nodes: []string{"meta"}}).Status(context.Background())
	if err != nil || len(status.Nodes) != 1 || status.Nodes[0].State != state.Stopped {
		t.Fatalf("reused PID = %#v, %v", status, err)
	}
	if !process.Alive(pid) {
		t.Fatal("status signalled the unrelated process")
	}
}

// Ctrl-C during stop can leave a node "stopping" while its verified QEMU keeps
// running. Status settles it as running, so up and start stop pointing at
// status in a loop.
func TestStatusResumesAnInterruptedStopWhoseProcessStillRuns(t *testing.T) {
	_, store := statusFixture(t)
	node, err := store.ReadNode("meta")
	if err != nil {
		t.Fatal(err)
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep is unavailable")
	}
	survivor := exec.Command(sleep, "31")
	if err := survivor.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = survivor.Process.Kill(); _, _ = survivor.Process.Wait() })
	node.Invocation = qemu.Invocation{Binary: sleep, Args: []string{"31"}}
	var identity process.Identity
	for attempt := 0; attempt < 100; attempt++ {
		if identity, err = process.Capture(context.Background(), execx.OSRunner{}, node.Invocation, survivor.Process.Pid); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	base := shortRuntimeBase(t, "farrow-interrupted-stop-")
	directory := filepath.Join(base, "farrow", node.Node)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	node.Phase = state.Stopping
	node.Runtime = state.RuntimePaths{Directory: directory, QMP: filepath.Join(directory, "qmp.sock"), PIDFile: filepath.Join(directory, "qemu.pid")}
	node.Process = recordProcess(identity)
	if err := store.WriteNode(node); err != nil {
		t.Fatal(err)
	}
	status, err := (Manager{FarrowVersion: "test", Nodes: []string{"meta"}}).Status(context.Background())
	if err != nil || len(status.Nodes) != 1 || status.Nodes[0].State != state.Running {
		t.Fatalf("interrupted stop = %#v, %v", status, err)
	}
	if settled, err := store.ReadNode("meta"); err != nil || settled.Phase != state.Running {
		t.Fatalf("settled state = %v, %v", settled.Phase, err)
	}
}

func TestAppendStatusMessageDoesNotLeadWithSeparator(t *testing.T) {
	if got := appendStatusMessage("", "adopted"); got != "adopted" {
		t.Fatalf("message = %q", got)
	}
	if got := appendStatusMessage("status", "adopted"); got != "status; adopted" {
		t.Fatalf("message = %q", got)
	}
}
