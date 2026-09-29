package macvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/barn/internal/failure"
)

func lifecycleManager(t *testing.T) *Manager {
	t.Helper()
	requireMac(t)
	m, _ := testManager(t)
	m.Runner.Binary = "/usr/bin/true"
	return m
}

func TestStopShutsDownNormally(t *testing.T) {
	m := lifecycleManager(t)
	machine := testMachine(t, m, "dev", "10.10.30.0/24", true)
	runtime := startFakeRuntime(t, m, machine)
	shutdown := func(context.Context, *Machine) error {
		go func() { time.Sleep(100 * time.Millisecond); runtime.exit() }()
		return nil
	}
	stopped, forced, err := m.stopRuntimeWith(context.Background(), machine, false, shutdown)
	if err != nil || !stopped || forced {
		t.Fatalf("stopped=%v forced=%v err=%v", stopped, forced, err)
	}
	if calls := runtime.calls(); slices.Contains(calls, "stop") || slices.Contains(calls, "stop:force") {
		t.Fatalf("a guest that shut down was also stopped through the VM: %v", calls)
	}
}

func TestStopPowersOffAfterTheGracePeriod(t *testing.T) {
	m := lifecycleManager(t)
	machine := testMachine(t, m, "dev", "10.10.30.0/24", true)
	runtime := startFakeRuntime(t, m, machine)
	runtime.ignoreGraceful = true
	previous := stopGrace
	stopGrace = 300 * time.Millisecond
	t.Cleanup(func() { stopGrace = previous })
	stopped, forced, err := m.stopRuntimeWith(context.Background(), machine, false, func(context.Context, *Machine) error { return nil })
	if err != nil || !stopped || !forced {
		t.Fatalf("stopped=%v forced=%v err=%v", stopped, forced, err)
	}
	if calls := runtime.calls(); !slices.Contains(calls, "stop:force") {
		t.Fatalf("no forced stop: %v", calls)
	}
}

func TestStopUsesTheVMWhenSSHIsUnavailable(t *testing.T) {
	m := lifecycleManager(t)
	for _, initialized := range []bool{false, true} {
		name := map[bool]string{false: "fresh", true: "broken"}[initialized]
		machine := testMachine(t, m, name, map[bool]string{false: "10.10.31.0/24", true: "10.10.32.0/24"}[initialized], initialized)
		runtime := startFakeRuntime(t, m, machine)
		stopped, forced, err := m.stopRuntimeWith(context.Background(), machine, false, func(context.Context, *Machine) error { return errors.New("no SSH") })
		if err != nil || !stopped || forced {
			t.Fatalf("%s: stopped=%v forced=%v err=%v", name, stopped, forced, err)
		}
		if calls := runtime.calls(); !slices.Contains(calls, "stop") {
			t.Fatalf("%s: VM stop not requested: %v", name, calls)
		}
	}
}

func TestForceStopPowersOffAtOnce(t *testing.T) {
	m := lifecycleManager(t)
	machine := testMachine(t, m, "dev", "10.10.30.0/24", true)
	runtime := startFakeRuntime(t, m, machine)
	called := false
	stopped, forced, err := m.stopRuntimeWith(context.Background(), machine, true, func(context.Context, *Machine) error { called = true; return nil })
	if err != nil || !stopped || !forced || called {
		t.Fatalf("stopped=%v forced=%v err=%v ssh=%v", stopped, forced, err, called)
	}
	if calls := runtime.calls(); !slices.Contains(calls, "stop:force") {
		t.Fatalf("calls %v", calls)
	}
}

func TestStopNeverTrustsAnUnresponsiveRunner(t *testing.T) {
	m := lifecycleManager(t)
	machine := testMachine(t, m, "dev", "10.10.30.0/24", true)
	runtime := startFakeRuntime(t, m, machine)
	runtime.mu.Lock()
	_ = runtime.listener.Close()
	runtime.listener = nil
	runtime.mu.Unlock()
	if _, _, err := m.stopRuntime(context.Background(), machine, false); err == nil || !strings.Contains(err.Error(), "cannot be confirmed") {
		t.Fatalf("a locked machine without a socket looked stopped: %v", err)
	}
	runtime.exit()
	stopped, _, err := m.stopRuntime(context.Background(), machine, false)
	if err != nil || stopped {
		t.Fatalf("an exited runner: stopped=%v err=%v", stopped, err)
	}
}

func TestDestroyRemovesOnlyTheMachine(t *testing.T) {
	m := lifecycleManager(t)
	machine := testMachine(t, m, "dev", "10.10.30.0/24", true)
	testMachine(t, m, "keep", "10.10.31.0/24", true)
	runtime := startFakeRuntime(t, m, machine)
	report, err := m.Destroy(context.Background(), []string{"dev", "ghost"})
	if err != nil || len(report.Machines) != 2 || report.Machines[0].Action != "destroyed" || report.Machines[1].Action != "absent" {
		t.Fatalf("report %+v err %v", report, err)
	}
	if !slices.Contains(runtime.calls(), "stop:force") {
		t.Fatalf("destroy did not power off: %v", runtime.calls())
	}
	dir, _ := m.Store.MachinePath("dev")
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("machine directory remains: %v", err)
	}
	if _, err := m.Store.LoadMachine("keep"); err != nil {
		t.Fatalf("another machine was touched: %v", err)
	}
	config, _ := m.Store.LoadConfig()
	if base, err := m.Store.LoadBase(config.DefaultBaseID); err != nil || m.validateBase(base) != nil {
		t.Fatalf("the shared base was touched: %v", err)
	}
}

func TestUpRefusesOptionsThatWouldNotApply(t *testing.T) {
	m := lifecycleManager(t)
	testMachine(t, m, "dev", "10.10.30.0/24", true)
	_, err := m.Up(context.Background(), "dev", UpOptions{CreateOptions: CreateOptions{CPU: 8, MemoryBytes: 16 << 30}})
	class, reason, next := failure.Classify(err)
	if class != failure.Conflict || reason != "mac_configuration_conflict" || next != "barn mac configure dev --cpu 8 --memory 16G" {
		t.Fatalf("err=%v class=%s reason=%s next=%s", err, class, reason, next)
	}
	_, err = m.Up(context.Background(), "dev", UpOptions{CreateOptions: CreateOptions{User: "someone"}})
	if _, _, next := failure.Classify(err); next != "create another machine for these settings: barn mac up NEW-NAME" {
		t.Fatalf("user change next %q (%v)", next, err)
	}
	_, err = m.Up(context.Background(), "dev", UpOptions{CreateOptions: CreateOptions{Setup: SetupOptions{IPSW: "/tmp/new.ipsw"}}})
	if _, _, next := failure.Classify(err); next != "barn mac image update --ipsw /tmp/new.ipsw, then barn mac recreate dev --update" {
		t.Fatalf("ipsw next %q (%v)", next, err)
	}
}

func TestConfigureChangesAStoppedMachine(t *testing.T) {
	m := lifecycleManager(t)
	testMachine(t, m, "dev", "10.10.30.0/24", true)
	share := t.TempDir()
	off := false
	outcome, err := m.Configure(context.Background(), "dev", ConfigureOptions{CPU: 8, MemoryBytes: 16 << 30, Share: []Share{{Name: "src", Path: share}}, Clipboard: &off, Subnet: "auto"})
	if err != nil || outcome.Action != "configured" {
		t.Fatalf("outcome %+v err %v", outcome, err)
	}
	machine, _ := m.Store.LoadMachine("dev")
	if machine.CPU != 8 || machine.MemoryBytes != 16<<30 || len(machine.Shares) != 1 || machine.Clipboard || machine.Network.Subnet != "10.10.20.0/24" {
		t.Fatalf("machine %+v", machine)
	}
	if _, err := m.Configure(context.Background(), "dev", ConfigureOptions{CPU: 64}); err == nil || !strings.Contains(err.Error(), "logical CPUs") {
		t.Fatalf("host limit: %v", err)
	}
	if _, err := m.Configure(context.Background(), "dev", ConfigureOptions{}); err == nil {
		t.Fatal("an empty configure succeeded")
	}
}

func TestConfigureRefusesHardwareChangesWhileRunning(t *testing.T) {
	m := lifecycleManager(t)
	machine := testMachine(t, m, "dev", "10.10.30.0/24", true)
	startFakeRuntime(t, m, machine)
	_, err := m.Configure(context.Background(), "dev", ConfigureOptions{CPU: 8})
	if class, reason, next := failure.Classify(err); class != failure.Conflict || reason != "mac_running" || next != "barn mac stop dev" {
		t.Fatalf("err=%v %s %s %s", err, class, reason, next)
	}
	on := false
	outcome, err := m.Configure(context.Background(), "dev", ConfigureOptions{Clipboard: &on})
	if err != nil || len(outcome.Warnings) != 1 || !strings.Contains(outcome.Warnings[0], "next start") {
		t.Fatalf("clipboard while running: %+v %v", outcome, err)
	}
}

func TestVMLimitNamesTheRunningMachines(t *testing.T) {
	m := lifecycleManager(t)
	startFakeRuntime(t, m, testMachine(t, m, "mac1", "10.10.20.0/24", true))
	startFakeRuntime(t, m, testMachine(t, m, "mac2", "10.10.21.0/24", true))
	err := m.checkVMLimit(context.Background(), "dev")
	if class, reason, next := failure.Classify(err); class != failure.Resource || reason != "mac_vm_limit" || next != "barn mac stop mac2" {
		t.Fatalf("err=%v %s %s %s", err, class, reason, next)
	}
	if err := m.checkVMLimit(context.Background(), "mac1"); err != nil {
		t.Fatalf("a running machine counted against itself: %v", err)
	}
	if _, err := m.Up(context.Background(), "build", UpOptions{}); err == nil {
		t.Fatal("a third machine was started")
	} else if _, reason, _ := failure.Classify(err); reason != "mac_vm_limit" {
		t.Fatalf("err=%v", err)
	}
	if _, err := m.Store.LoadMachine("build"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a machine that could not run was created: %v", err)
	}
}

func TestConnectionRequiresAReadyRunningMachine(t *testing.T) {
	m := lifecycleManager(t)
	testMachine(t, m, "fresh", "10.10.31.0/24", false)
	machine := testMachine(t, m, "dev", "10.10.30.0/24", true)
	if _, err := m.Connection(context.Background(), "fresh"); err == nil {
		t.Fatal("connected to an uninitialized machine")
	} else if _, reason, next := failure.Classify(err); reason != "mac_not_initialized" || next != "barn mac up fresh" {
		t.Fatalf("%v %s %s", err, reason, next)
	}
	if _, err := m.Connection(context.Background(), "dev"); err == nil {
		t.Fatal("connected to a stopped machine")
	} else if _, reason, next := failure.Classify(err); reason != "mac_not_running" || next != "barn mac start dev" {
		t.Fatalf("%v %s %s", err, reason, next)
	}
	startFakeRuntime(t, m, machine)
	connection, err := m.Connection(context.Background(), "dev")
	if err != nil || connection.Host != "10.10.30.10" || connection.HostKeyAlias != machine.HostKeyAlias() {
		t.Fatalf("connection %+v err %v", connection, err)
	}
	if _, err := m.Connection(context.Background(), "ghost"); err == nil {
		t.Fatal("connected to an absent machine")
	} else if _, reason, next := failure.Classify(err); reason != "mac_machine_absent" || next != "barn mac up ghost" {
		t.Fatalf("%v %s %s", err, reason, next)
	}
}

func TestListReportsRuntimeWithoutStartingAnything(t *testing.T) {
	m := lifecycleManager(t)
	running := testMachine(t, m, "mac1", "10.10.20.0/24", true)
	testMachine(t, m, "mac2", "10.10.21.0/24", true)
	testMachine(t, m, "fresh", "10.10.22.0/24", false)
	startFakeRuntime(t, m, running)
	status, err := m.List(context.Background(), false)
	if err != nil || len(status.Machines) != 3 || status.Running != 1 || !status.Prepared || status.Base.Build != "26A428" {
		t.Fatalf("status %+v err %v", status, err)
	}
	states := map[string]string{}
	for _, view := range status.Machines {
		states[view.Name] = view.State
	}
	if states["mac1"] != "running" || states["mac2"] != "stopped" || states["fresh"] != "prepared" {
		t.Fatalf("states %v", states)
	}
	if status.Machines[1].Disk.AllocatedBytes == nil {
		t.Fatal("disk allocation not measured")
	}
}

func TestOpenShowsARunningDesktop(t *testing.T) {
	m := lifecycleManager(t)
	machine := testMachine(t, m, "dev", "10.10.30.0/24", true)
	runtime := startFakeRuntime(t, m, machine)
	outcome, err := m.Open(context.Background(), "dev")
	if err != nil || outcome.Action != "opened" || !outcome.Window {
		t.Fatalf("outcome %+v err %v", outcome, err)
	}
	if !slices.Contains(runtime.calls(), "open") {
		t.Fatalf("calls %v", runtime.calls())
	}
	testMachine(t, m, "fresh", "10.10.31.0/24", false)
	if _, err := m.Open(context.Background(), "fresh"); err == nil {
		t.Fatal("opened a machine that never booted")
	} else if _, _, next := failure.Classify(err); next != "barn mac up fresh --open" {
		t.Fatalf("next %q", next)
	}
}

func TestCloneNeverReplacesABootedMachine(t *testing.T) {
	m := lifecycleManager(t)
	machine := testMachine(t, m, "dev", "10.10.30.0/24", true)
	dir, _ := m.Store.MachinePath("dev")
	if err := os.Remove(filepath.Join(dir, "disk.asif")); err != nil {
		t.Fatal(err)
	}
	err := m.cloneMachine(context.Background(), machine)
	if class, reason, _ := failure.Classify(err); class != failure.Integrity || reason != "mac_machine_damaged" {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "machine-id.bin")); err != nil {
		t.Fatal("recovery files were removed")
	}
}

func TestMachineMemoryIsWholeMiB(t *testing.T) {
	if err := validateHostResources(4, 16<<30, 8, 64<<30); err != nil {
		t.Fatal(err)
	}
	err := validateHostResources(4, 16_000_000_000, 8, 64<<30)
	if class, _, _ := failure.Classify(err); class != failure.Usage {
		t.Fatalf("16e9 bytes of memory: %v", err)
	}
}

func TestInterruptedCreationIsNotAMachine(t *testing.T) {
	m, _ := testManager(t)
	dir, err := m.Store.MachinePath("half")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"password", ".password.tmp-123"} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Store.LoadMachine("half"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a crash before the record left a machine: %v", err)
	}
}

func TestStatelessDirectoryBlocksOnlyItself(t *testing.T) {
	m := lifecycleManager(t)
	testMachine(t, m, "mac1", "10.10.20.0/24", true)
	dir, err := m.Store.MachinePath("zz")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.asif"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.ListMachines(); !errors.Is(err, errStateless) {
		t.Fatalf("ls hid a stateless directory: %v", err)
	} else if _, _, next := failure.Classify(err); next != "barn mac destroy zz" {
		t.Fatalf("next %q", next)
	}
	if name, err := m.Store.DefaultMachine(); err != nil || name != "mac1" {
		t.Fatalf("default machine %q %v", name, err)
	}
	if err := m.checkVMLimit(context.Background(), "mac1"); err != nil {
		t.Fatalf("the stateless directory blocked starting mac1: %v", err)
	}
	report, err := m.Destroy(context.Background(), []string{"zz"})
	if err != nil || len(report.Machines) != 1 || report.Machines[0].Action != "destroyed" {
		t.Fatalf("destroy zz: %+v %v", report, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("zz still exists: %v", err)
	}
}

func TestStopPowersOffWhenNoShutdownRequestIsAccepted(t *testing.T) {
	m := lifecycleManager(t)
	previous := stopGrace
	stopGrace = 300 * time.Millisecond
	t.Cleanup(func() { stopGrace = previous })
	machine := testMachine(t, m, "dev", "10.10.30.0/24", true)
	runtime := startFakeRuntime(t, m, machine)
	runtime.mu.Lock()
	runtime.refuseGraceful = true
	runtime.mu.Unlock()
	shutdown := func(context.Context, *Machine) error { return errors.New("sshd is not up yet") }
	stopped, forced, err := m.stopRuntimeWith(context.Background(), machine, false, shutdown)
	if err != nil || !stopped || !forced {
		t.Fatalf("stopped=%v forced=%v err=%v", stopped, forced, err)
	}
	if calls := runtime.calls(); !slices.Contains(calls, "stop") || !slices.Contains(calls, "stop:force") {
		t.Fatalf("calls %v", calls)
	}
}
