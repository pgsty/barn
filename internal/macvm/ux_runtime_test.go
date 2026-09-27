package macvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/lock"
)

// This executable fixture implements RPC only. It cannot launch a VM, access
// Keychain, install networking, or make an SSH connection.
func TestUXRuntimeFixture(t *testing.T) {
	if os.Getenv("FARROW_TEST_UX_RUNTIME") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 || args[1] != "rpc" {
		os.Exit(90)
	}
	args = args[1:]
	value := func(flag string) string {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag {
				return args[i+1]
			}
		}
		return ""
	}
	file, err := os.OpenFile(os.Getenv("FARROW_TEST_UX_CAPTURE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(91)
	}
	_ = json.NewEncoder(file).Encode(args)
	_ = file.Close()
	mode := os.Getenv("FARROW_TEST_UX_MODE")
	if mode == "rendezvous" {
		// Both slot probes must enter before either can report a running VM.
		marker := os.Getenv("FARROW_TEST_UX_MARKER")
		if err := os.WriteFile(marker+"."+value("--instance"), nil, 0600); err != nil {
			os.Exit(93)
		}
		until := time.Now().Add(2 * time.Second)
		for {
			arrivals, _ := filepath.Glob(marker + ".*")
			if len(arrivals) == 2 {
				break
			}
			if time.Now().After(until) {
				os.Exit(94)
			}
			time.Sleep(10 * time.Millisecond)
		}
		mode = "running"
	}
	if mode == "slow" {
		_ = os.WriteFile(os.Getenv("FARROW_TEST_UX_MARKER"), []byte("rpc-started"), 0600)
		time.Sleep(10 * time.Second)
	}
	if mode == "unavailable" {
		fmt.Println(`{"ok":false,"error":{"code":"rpc_unavailable","message":"fixture RPC unavailable"}}`)
		os.Exit(1)
	}
	state := mode
	if mode == "shutdown" {
		path := os.Getenv("FARROW_TEST_UX_STATE")
		if value("--method") == "stop" {
			_ = os.WriteFile(path, []byte("stopped"), 0600)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			os.Exit(92)
		}
		state = string(data)
	}
	_ = json.NewEncoder(os.Stdout).Encode(RuntimeStatus{OK: true, Instance: value("--instance"), PID: os.Getpid(), State: state})
	os.Exit(0)
}

func uxRuntimeRunner(t *testing.T, mode string) (Runner, string, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "runner")
	body := "#!/bin/sh\nexec " + QuoteCommand([]string{exe}) + " -test.run='^TestUXRuntimeFixture$' -- \"$@\"\n"
	if err = os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	capture, marker := filepath.Join(dir, "calls.jsonl"), filepath.Join(dir, "rpc-marker")
	t.Setenv("FARROW_TEST_UX_RUNTIME", "1")
	t.Setenv("FARROW_TEST_UX_MODE", mode)
	t.Setenv("FARROW_TEST_UX_CAPTURE", capture)
	t.Setenv("FARROW_TEST_UX_MARKER", marker)
	t.Setenv("FARROW_TEST_UX_STATE", filepath.Join(dir, "runtime-state"))
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	return Runner{Binary: script}, capture, marker
}

func uxRuntimeStore(t *testing.T) (*Manager, *Config, *Slot, *Slot) {
	t.Helper()
	s := testStore(t)
	held := holdStore(t, s)
	config, err := NewConfig(DefaultSubnet)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveConfig(held, config); err != nil {
		t.Fatal(err)
	}
	base := testBase(t, s, held, "ux-runtime-base")
	var slots []*Slot
	for _, name := range []string{"mac1", "mac2"} {
		slot := testSlot(t, name, base.ID)
		slot.Initialized, slot.State = true, "ready"
		slot.LastError = "historical failure"
		if err = s.SaveSlot(held, slot); err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{"runner.lock", "disk.asif", "machine-id.bin", "auxiliary-storage.bin"} {
			if err = os.WriteFile(filepath.Join(s.Root, "slots", name, file), []byte("preserve "+name+" "+file), 0600); err != nil {
				t.Fatal(err)
			}
		}
		slots = append(slots, slot)
	}
	if err = held.Release(); err != nil {
		t.Fatal(err)
	}
	return &Manager{Store: s}, config, slots[0], slots[1]
}

func TestUXRuntimeTruthDoesNotReuseSavedReady(t *testing.T) {
	for _, tc := range []struct{ name, mode, wantState, wantSSH string }{
		{"free-lock-rpc-failure", "unavailable", "stopped", "offline"},
		{"runner-missing", "", "unknown", "unchecked"},
		{"runner-locked", "unavailable", "unresponsive", "unchecked"},
		{"operation-locked", "unavailable", "unknown", "unchecked"},
		{"unsafe-operation-path", "unavailable", "unknown", "unchecked"},
		{"missing-booted-proof", "unavailable", "unknown", "unchecked"},
		{"canceled-inspection", "unavailable", "unknown", "unchecked"},
		{"cached-ready-ssh-missing", "running", "running", "unavailable"},
		{"reported-stopped", "stopped", "stopped", "offline"},
		{"invalid-ready-state", "ready", "unknown", "unchecked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, cfg, slot, _ := uxRuntimeStore(t)
			if tc.mode != "" {
				m.Runner, _, _ = uxRuntimeRunner(t, tc.mode)
			}
			path := filepath.Join(m.Store.Root, "slots", "mac1", "runner.lock")
			if tc.name == "runner-locked" {
				holdRunnerLock(t, filepath.Dir(path))
			}
			if tc.name == "missing-booted-proof" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "operation-locked" {
				p, _ := m.Store.Path("runtime", "mac1.operation.lock")
				held, err := lock.TryAcquire(p, false)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = held.Release() }()
			}
			if tc.name == "unsafe-operation-path" {
				if err := os.Symlink(path, filepath.Join(m.Store.Root, "runtime", "mac1.operation.lock")); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			if tc.name == "canceled-inspection" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			pref, _ := cfg.Preference("mac1")
			view := SlotView{Slot: *slot}
			m.inspectRuntime(ctx, &view, pref)
			if view.State != tc.wantState || view.SSH != tc.wantSSH {
				t.Fatalf("state=%s SSH=%s runtimeError=%s SSHError=%s", view.State, view.SSH, view.RuntimeError, view.SSHError)
			}
			stored, err := m.Store.LoadSlot("mac1")
			if err != nil || !reflect.DeepEqual(stored, slot) {
				t.Fatalf("inspection mutated saved state: %+v %v", stored, err)
			}
		})
	}
}

func TestUXListProbesBothSlotsConcurrentlyAndKeepsOrder(t *testing.T) {
	m, _, first, second := uxRuntimeStore(t)
	m.Runner, _, _ = uxRuntimeRunner(t, "rendezvous")
	result, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Slots) != 2 {
		t.Fatalf("unexpected slots: %+v", result.Slots)
	}
	for i, saved := range []*Slot{first, second} {
		view := result.Slots[i]
		if view.Name != saved.Name || view.State != "running" || view.SSH != "unavailable" {
			t.Fatalf("slot %d was reordered or failed concurrent rendezvous: %+v", i, view)
		}
		stored, err := m.Store.LoadSlot(saved.Name)
		if err != nil || !reflect.DeepEqual(stored, saved) {
			t.Fatalf("List mutated %s: %+v %v", saved.Name, stored, err)
		}
	}
}

func TestUXListConcurrentProbesRespectContextDeadline(t *testing.T) {
	m, _, _, _ := uxRuntimeStore(t)
	m.Runner, _, _ = uxRuntimeRunner(t, "slow")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("canceled list waited %s for probes", elapsed)
	}
	for _, view := range result.Slots {
		if view.State != "unknown" || !strings.Contains(view.RuntimeError, context.DeadlineExceeded.Error()) {
			t.Fatalf("canceled probe reported a definite state: %+v", view)
		}
	}
}

func TestUXStartRequiresRunningRPCOrIndependentStoppedProof(t *testing.T) {
	for _, tc := range []struct {
		mode                          string
		busy, missing, launch, reject bool
	}{
		{"running", false, false, false, false},
		{"stopped", false, false, false, true},
		{"paused", false, false, false, true},
		{"starting", false, false, false, true},
		{"unavailable", false, false, true, false},
		{"unavailable", true, false, false, true},
		{"unavailable", false, true, false, true},
	} {
		t.Run(fmt.Sprintf("%s-busy%t-missing%t", tc.mode, tc.busy, tc.missing), func(t *testing.T) {
			m, _, slot, _ := uxRuntimeStore(t)
			m.Runner, _, _ = uxRuntimeRunner(t, tc.mode)
			dir := filepath.Join(m.Store.Root, "slots", "mac1")
			if tc.busy {
				holdRunnerLock(t, dir)
			}
			if tc.missing {
				if err := os.Remove(filepath.Join(dir, "runner.lock")); err != nil {
					t.Fatal(err)
				}
			}
			launch, err := m.shouldLaunch(context.Background(), slot)
			if launch != tc.launch || (err != nil) != tc.reject {
				t.Fatalf("launch=%t err=%v", launch, err)
			}
		})
	}
}

func TestUXConfigurePreservesIdentityDiskAndOtherSlot(t *testing.T) {
	if HostSupported() != nil || os.Geteuid() == 0 {
		t.Skip("ordinary supported host required; runner remains a fixture")
	}
	m, config, old, other := uxRuntimeStore(t)
	m.Runner, _, _ = uxRuntimeRunner(t, "unavailable")
	result, err := m.Configure(context.Background(), "mac1", 2, 4<<30)
	if err != nil {
		t.Fatal(err)
	}
	want := *old
	want.CPU, want.MemoryBytes, want.UpdatedAt = 2, 4<<30, result.UpdatedAt
	if !reflect.DeepEqual(result, &want) {
		t.Fatalf("unexpected slot mutation: %+v want %+v", result, &want)
	}
	got, err := m.Store.LoadSlot("mac2")
	if err != nil || !reflect.DeepEqual(got, other) {
		t.Fatalf("other slot changed %+v err=%v", got, err)
	}
	after, err := m.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	wantConfig := *config
	wantConfig.Slots = append([]SlotPreference(nil), config.Slots...)
	wantConfig.Slots[0].CPU, wantConfig.Slots[0].MemoryBytes, wantConfig.Slots[0].ResourcesConfigured = 2, 4<<30, true
	if !reflect.DeepEqual(after, &wantConfig) {
		t.Fatalf("unexpected configuration change: %+v", after)
	}
	for _, name := range []string{"mac1", "mac2"} {
		for _, file := range []string{"disk.asif", "machine-id.bin", "auxiliary-storage.bin"} {
			requireRecoveryFile(t, filepath.Join(m.Store.Root, "slots", name, file), "preserve "+name+" "+file)
		}
	}
}

func TestUXConfigureRejectsActiveAndOversizedResources(t *testing.T) {
	if HostSupported() != nil || os.Geteuid() == 0 {
		t.Skip("ordinary supported host required; runner remains a fixture")
	}
	for _, kind := range []string{"running", "locked", "too-many-cpus", "too-much-memory"} {
		t.Run(kind, func(t *testing.T) {
			m, cfg, slot, _ := uxRuntimeStore(t)
			mode := "unavailable"
			if kind == "running" {
				mode = "running"
			}
			m.Runner, _, _ = uxRuntimeRunner(t, mode)
			if kind == "locked" {
				holdRunnerLock(t, filepath.Join(m.Store.Root, "slots", "mac1"))
			}
			cpu, memory := 2, int64(4<<30)
			if kind == "too-many-cpus" {
				cpu = 1 << 30
			}
			if kind == "too-much-memory" {
				memory = 1 << 62
			}
			if _, err := m.Configure(context.Background(), "mac1", cpu, memory); err == nil {
				t.Fatal("accepted unsafe configure")
			}
			got, err := m.Store.LoadSlot("mac1")
			if err != nil || !reflect.DeepEqual(got, slot) {
				t.Fatalf("slot changed %+v err=%v", got, err)
			}
			gotCfg, err := m.Store.LoadConfig()
			if err != nil || !reflect.DeepEqual(gotCfg, cfg) {
				t.Fatalf("config changed %+v err=%v", gotCfg, err)
			}
		})
	}
}

func TestUXConfigureCancellationLeavesResourcesAndLocksUnchanged(t *testing.T) {
	if HostSupported() != nil || os.Geteuid() == 0 {
		t.Skip("ordinary supported host required; runner remains a fixture")
	}
	m, _, slot, _ := uxRuntimeStore(t)
	var marker string
	m.Runner, _, marker = uxRuntimeRunner(t, "slow")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	_, err := m.Configure(ctx, "mac1", 2, 4<<30)
	<-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("configure err=%v", err)
	}
	stored, err := m.Store.LoadSlot("mac1")
	if err != nil || !reflect.DeepEqual(stored, slot) {
		t.Fatalf("canceled operation changed state %+v %v", stored, err)
	}
	for _, name := range []string{"state.lock", "mac1.operation.lock"} {
		path, _ := m.Store.Path("runtime", name)
		proof, err := lock.TryAcquire(path, false)
		if err != nil {
			t.Fatal("leaked lock", err)
		}
		if err = proof.Release(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUXOtherSlotStopDoesNotWaitForGlobalPreparation(t *testing.T) {
	if HostSupported() != nil || os.Geteuid() == 0 {
		t.Skip("ordinary supported host required; runner remains a fixture")
	}
	m, _, first, second := uxRuntimeStore(t)
	var capture string
	m.Runner, capture, _ = uxRuntimeRunner(t, "shutdown")
	if err := os.WriteFile(os.Getenv("FARROW_TEST_UX_STATE"), []byte("running"), 0600); err != nil {
		t.Fatal(err)
	}
	firstOp, err := m.acquireOperation(context.Background(), "mac1", "mac up mac1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = firstOp.Release() }()
	global, err := m.acquireState(context.Background(), "mac image update")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = global.Release() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stopped, err := m.Stop(ctx, "mac2", true)
	if err != nil || stopped.State != "stopped" {
		t.Fatalf("stop blocked behind unrelated locks: %+v %v", stopped, err)
	}
	got, err := m.Store.LoadSlot("mac1")
	if err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("other guest changed %+v %v", got, err)
	}
	if stopped.InstanceID != second.InstanceID || stopped.BaseID != second.BaseID {
		t.Fatal("stop changed identity")
	}
	calls, _ := os.ReadFile(capture)
	if !bytes.Contains(calls, []byte(`"stop"`)) {
		t.Fatal("fixture never received shutdown RPC")
	}
}

func TestUXStopCannotRecreateMissingRunnerProof(t *testing.T) {
	m, _, slot, _ := uxRuntimeStore(t)
	m.Runner, _, _ = uxRuntimeRunner(t, "unavailable")
	path := filepath.Join(m.Store.Root, "slots", slot.Name, "runner.lock")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	held, err := m.acquireOperation(context.Background(), slot.Name, "mac stop "+slot.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	if err = m.stopLocked(context.Background(), held, slot, true); err == nil || !strings.Contains(err.Error(), "runner lock is missing") {
		t.Fatalf("missing proof was accepted: %v", err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing proof was recreated: %v", err)
	}
	stored, err := m.Store.LoadSlot(slot.Name)
	if err != nil || !reflect.DeepEqual(stored, slot) || stored.State != "ready" {
		t.Fatalf("failed stop changed stored runtime state: %+v %v", stored, err)
	}
}

func TestUXSlotOperationTokenCannotChangeOtherSlotOrReferences(t *testing.T) {
	m, _, slot, other := uxRuntimeStore(t)
	held, err := m.acquireOperation(context.Background(), slot.Name, "mac stop "+slot.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	for _, tc := range []string{"other-slot", "instance-identity", "base-reference"} {
		t.Run(tc, func(t *testing.T) {
			changed := *slot
			switch tc {
			case "other-slot":
				changed = *other
			case "instance-identity":
				changed.InstanceID = other.InstanceID
			case "base-reference":
				changed.BaseID = "another-base"
			}
			changed.State = "stopped"
			if err := m.Store.SaveSlot(held, &changed); err == nil {
				t.Fatal("slot operation token authorized an unrelated mutation")
			}
		})
	}
	for _, want := range []*Slot{slot, other} {
		got, err := m.Store.LoadSlot(want.Name)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("rejected write changed %s: %+v %v", want.Name, got, err)
		}
	}
	changed := *slot
	changed.State = "stopped"
	if err := m.Store.SaveSlot(held, &changed); err != nil {
		t.Fatalf("own runtime update failed: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	changed.State = "running"
	if err := m.Store.SaveSlot(held, &changed); err == nil {
		t.Fatal("released token authorized a write")
	}
}

func TestUXLockWaitExplainsOwnerAndCancels(t *testing.T) {
	m, _, _, _ := uxRuntimeStore(t)
	var progress bytes.Buffer
	m.Progress = &progress
	held, err := m.acquireState(context.Background(), "mac image update")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err = m.acquireState(ctx, "mac configure mac2"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if !strings.Contains(progress.String(), "mac image update (pid ") || !strings.Contains(progress.String(), "Ctrl-C cancels") {
		t.Fatal(progress.String())
	}
}

func TestUXResourceArgumentsAndHostBounds(t *testing.T) {
	for _, tc := range []struct {
		bytes int64
		want  string
	}{{16 << 30, "16G"}, {4608 << 20, "4608M"}, {4294967297, "4294967297"}} {
		if got := resourceMemoryArgument(tc.bytes); got != tc.want {
			t.Fatalf("got %s want %s", got, tc.want)
		}
	}
	for _, tc := range []struct {
		cpu    int
		memory int64
		limits string
		reject bool
	}{{8, 16 << 30, "8\n17179869184\n", false}, {9, 16 << 30, "8\n17179869184\n", true}, {8, 17 << 30, "8\n17179869184\n", true}, {8, 16 << 30, "unknown", true}} {
		if err := validateHostResources(tc.cpu, tc.memory, tc.limits); (err != nil) != tc.reject {
			t.Fatalf("limits=%q err=%v", tc.limits, err)
		}
	}
}
