package macvm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/lock"
)

// A subprocess fixture implements only the runner protocol. No Apple API,
// Keychain entry, network interface, or real VM is touched by these tests.
func TestMacLifecycleRunnerFixture(t *testing.T) {
	if os.Getenv("FARROW_TEST_LIFECYCLE_RUNNER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		os.Exit(90)
	}
	args = args[1:]
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(91)
	}
	inputHash := sha256.Sum256(input)
	report := struct {
		Args      []string `json:"args"`
		StdinHash string   `json:"stdin_hash"`
	}{args, hex.EncodeToString(inputHash[:])}
	data, _ := json.Marshal(report)
	if err := os.WriteFile(os.Getenv("FARROW_TEST_LIFECYCLE_CAPTURE"), data, 0o600); err != nil {
		os.Exit(92)
	}
	value := func(flag string) string {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag {
				return args[i+1]
			}
		}
		return ""
	}
	emit := func(v any) { _ = json.NewEncoder(os.Stdout).Encode(v) }
	switch os.Getenv("FARROW_TEST_LIFECYCLE_MODE") {
	case "fail":
		fmt.Fprintln(os.Stderr, `{"phase":"fixture","message":"public diagnostic only"}`)
		emit(map[string]any{"ok": false, "error": map[string]string{"code": "virtual_machine_limit", "message": "Host macOS VM limit reached"}})
		os.Exit(1)
	case "secret-echo":
		_, _ = os.Stderr.Write(input)
		emit(map[string]any{"error": map[string]string{"code": "keychain:-25244", "message": string(input)}})
		os.Exit(1)
	case "clone":
		if len(args) == 0 || args[0] != "clone" || value("--slot") == "" {
			os.Exit(93)
		}
		for _, file := range []string{"disk.asif", "machine-id.bin", "auxiliary-storage.bin"} {
			if err := os.WriteFile(filepath.Join(value("--slot"), file), []byte("fresh "+file), 0o600); err != nil {
				os.Exit(94)
			}
		}
		emit(map[string]bool{"ok": true})
	case "running":
		if value("--method") == "stop" {
			if err := os.WriteFile(os.Getenv("FARROW_TEST_LIFECYCLE_STOP_MARKER"), []byte("requested"), 0o600); err != nil {
				os.Exit(95)
			}
		}
		emit(RuntimeStatus{OK: true, Instance: value("--instance"), State: "running", PID: os.Getpid()})
	case "shutdown":
		if value("--method") == "stop" {
			data, _ := json.Marshal(args)
			if err := os.WriteFile(os.Getenv("FARROW_TEST_LIFECYCLE_STOP_MARKER"), data, 0o600); err != nil {
				os.Exit(95)
			}
			if os.Getenv("FARROW_TEST_LIFECYCLE_AUTO_STOP") == "1" {
				_ = os.WriteFile(os.Getenv("FARROW_TEST_LIFECYCLE_STATE"), []byte("stopped"), 0o600)
			}
		}
		state, err := os.ReadFile(os.Getenv("FARROW_TEST_LIFECYCLE_STATE"))
		if err != nil {
			os.Exit(97)
		}
		emit(RuntimeStatus{OK: true, Instance: value("--instance"), State: string(state), PID: os.Getpid()})
	default:
		os.Exit(96)
	}
	os.Exit(0)
}

func lifecycleRunner(t *testing.T, mode string) (Runner, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "runner")
	quoted := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "'"
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec "+quoted+" -test.run='^TestMacLifecycleRunnerFixture$' -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(dir, "capture.json")
	t.Setenv("FARROW_TEST_LIFECYCLE_RUNNER", "1")
	t.Setenv("FARROW_TEST_LIFECYCLE_MODE", mode)
	t.Setenv("FARROW_TEST_LIFECYCLE_CAPTURE", capture)
	// Race instrumentation need not wait an extra second before each fixture exits.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	return Runner{Binary: script}, capture
}

func lifecycleSlot(t *testing.T) (*Manager, *lock.File, *Slot, string) {
	t.Helper()
	store := testStore(t)
	held := holdStore(t, store)
	base := testBase(t, store, held, "recovery-base")
	slot := testSlot(t, "mac1", base.ID)
	slot.Initialized, slot.State = true, "running"
	if err := store.SaveSlot(held, slot); err != nil {
		t.Fatal(err)
	}
	dir, err := store.SlotPath(slot.Name)
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{Store: store}, held, slot, dir
}

func holdRunnerLock(t *testing.T, dir string) {
	t.Helper()
	held, err := lock.TryAcquire(filepath.Join(dir, "runner.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := held.Release(); err != nil {
			t.Error(err)
		}
	})
}

func requireRecoveryFile(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != expected {
		t.Fatalf("recovery data changed: %s contents=%q error=%v", path, data, err)
	}
}

func TestStopUnresponsiveRunnerRequiresUnlockedSlot(t *testing.T) {
	m, held, slot, dir := lifecycleSlot(t)
	m.Runner, _ = lifecycleRunner(t, "fail")
	disk := filepath.Join(dir, "disk.asif")
	if err := os.WriteFile(disk, []byte("valuable guest data"), 0o600); err != nil {
		t.Fatal(err)
	}
	holdRunnerLock(t, dir)
	if err := m.stopLocked(context.Background(), held, slot, false); err == nil || !strings.Contains(err.Error(), "cannot establish a safe stopped state") {
		t.Fatalf("unresponsive locked VM reported stopped: %v", err)
	}
	stored, err := m.Store.LoadSlot(slot.Name)
	if err != nil || stored.State != "running" || slot.State != "running" {
		t.Fatalf("failed stop changed state: memory=%s stored=%+v error=%v", slot.State, stored, err)
	}
	requireRecoveryFile(t, disk, "valuable guest data")
}

func TestStopCancellationPreservesRunningStateAndDisk(t *testing.T) {
	m, held, slot, dir := lifecycleSlot(t)
	m.Runner, _ = lifecycleRunner(t, "running")
	marker := filepath.Join(t.TempDir(), "stop-requested")
	t.Setenv("FARROW_TEST_LIFECYCLE_STOP_MARKER", marker)
	disk := filepath.Join(dir, "disk.asif")
	if err := os.WriteFile(disk, []byte("preserve after cancel"), 0o600); err != nil {
		t.Fatal(err)
	}
	holdRunnerLock(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	if err := m.stopLocked(ctx, held, slot, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled shutdown: %v", err)
	}
	stored, err := m.Store.LoadSlot(slot.Name)
	if err != nil || stored.State != "running" {
		t.Fatalf("canceled stop changed state: %+v %v", stored, err)
	}
	requireRecoveryFile(t, disk, "preserve after cancel")
}

func TestDestroyNeverDeletesAnUnresponsiveLockedSlot(t *testing.T) {
	if HostSupported() != nil || os.Geteuid() == 0 {
		t.Skip("public manager requires an ordinary user on Apple Silicon/macOS 27")
	}
	m, held, slot, dir := lifecycleSlot(t)
	m.Runner, _ = lifecycleRunner(t, "fail")
	disk := filepath.Join(dir, "disk.asif")
	if err := os.WriteFile(disk, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}
	holdRunnerLock(t, dir)
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Destroy(context.Background(), slot.Name, false, false); err == nil {
		t.Fatal("destroy accepted an unresponsive live runner")
	}
	requireRecoveryFile(t, disk, "do not delete")
	if _, err := m.Store.LoadSlot(slot.Name); err != nil {
		t.Fatalf("destroy lost slot metadata: %v", err)
	}
}

func TestDestroyCanceledBeforeWorkPreservesSlot(t *testing.T) {
	if HostSupported() != nil || os.Geteuid() == 0 {
		t.Skip("public manager requires an ordinary user on Apple Silicon/macOS 27")
	}
	m, held, slot, dir := lifecycleSlot(t)
	m.Runner, _ = lifecycleRunner(t, "fail")
	disk := filepath.Join(dir, "disk.asif")
	if err := os.WriteFile(disk, []byte("canceled destroy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Destroy(ctx, slot.Name, false, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	requireRecoveryFile(t, disk, "canceled destroy")
	if _, err := m.Store.LoadSlot(slot.Name); err != nil {
		t.Fatalf("canceled destroy lost metadata: %v", err)
	}
}

func TestCloneRecoveryProtectsBootedOrLockedInstances(t *testing.T) {
	files := []string{"disk.asif", "machine-id.bin", "auxiliary-storage.bin"}
	for _, tc := range []struct {
		name        string
		present     int
		bootMarker  bool
		initialized bool
		locked      bool
		wantClone   bool
		wantError   bool
	}{
		{"empty-unbooted", 0, false, false, false, true, false},
		{"partial-disk-only", 1, false, false, false, true, false},
		{"partial-two-files", 2, false, false, false, true, false},
		{"complete-reused", 3, false, false, false, false, false},
		{"boot-requested-partial", 1, true, false, false, false, true},
		{"boot-requested-empty", 0, true, false, false, false, true},
		{"initialized-partial", 2, false, true, false, false, true},
		{"locked-partial", 1, false, false, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, slot, dir := lifecycleSlot(t)
			slot.Initialized = tc.initialized
			var capture string
			m.Runner, capture = lifecycleRunner(t, "clone")
			for _, file := range files[:tc.present] {
				if err := os.WriteFile(filepath.Join(dir, file), []byte("original "+file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.bootMarker {
				if err := os.WriteFile(filepath.Join(dir, "boot-requested"), []byte(slot.InstanceID), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.locked {
				holdRunnerLock(t, dir)
			}
			sentinel := filepath.Join(dir, "user-recovery-note")
			if err := os.WriteFile(sentinel, []byte("retain unrelated recovery material"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := m.cloneSlot(context.Background(), slot)
			if (err != nil) != tc.wantError {
				t.Fatalf("clone recovery error=%v wantError=%v", err, tc.wantError)
			}
			_, calledErr := os.Stat(capture)
			if (calledErr == nil) != tc.wantClone {
				t.Fatalf("clone invocation=%v want=%v", calledErr == nil, tc.wantClone)
			}
			if tc.wantClone {
				for _, file := range files {
					requireRecoveryFile(t, filepath.Join(dir, file), "fresh "+file)
				}
			} else {
				for _, file := range files[:tc.present] {
					requireRecoveryFile(t, filepath.Join(dir, file), "original "+file)
				}
				for _, file := range files[tc.present:] {
					if _, err := os.Stat(filepath.Join(dir, file)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("recovery unexpectedly created %s: %v", file, err)
					}
				}
			}
			requireRecoveryFile(t, sentinel, "retain unrelated recovery material")
		})
	}
}

func TestRunnerNestedErrorsAndSecretStdinBoundary(t *testing.T) {
	runner, capture := lifecycleRunner(t, "fail")
	var progress bytes.Buffer
	runner.Progress = &progress
	secret := "synthetic-secret-only-through-stdin-94b2284a"
	input := map[string]string{"password": secret}
	args := []string{"secret", "set", "--service", "farrow.mac.test", "--account", "c217d75e-bf91-4fc1-bc02-5b9a263bfbaf"}
	err := runner.Call(context.Background(), nil, nil, "probe")
	if err == nil || !strings.Contains(err.Error(), "virtual_machine_limit") || !strings.Contains(err.Error(), "Host macOS VM limit reached") {
		t.Fatalf("nested Apple error was lost: %v", err)
	}
	t.Setenv("FARROW_TEST_LIFECYCLE_MODE", "secret-echo")
	progress.Reset()
	err = runner.Call(context.Background(), input, nil, args...)
	if err == nil || !strings.Contains(err.Error(), "keychain:-25244") || !strings.Contains(err.Error(), "credentials migrate") {
		t.Fatalf("secret ownership error did not include recovery: %v", err)
	}
	data, readErr := os.ReadFile(capture)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var report struct {
		Args      []string `json:"args"`
		StdinHash string   `json:"stdin_hash"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(input); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(encoded.Bytes())
	if report.StdinHash != hex.EncodeToString(wantHash[:]) {
		t.Fatal("secret was not transported as the expected JSON on stdin")
	}
	if strings.Join(report.Args, "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("runner arguments changed: %q", report.Args)
	}
	for name, output := range map[string]string{"argv": strings.Join(report.Args, " "), "stderr": progress.String(), "error": err.Error(), "capture": string(data)} {
		if strings.Contains(output, secret) {
			t.Fatalf("stdin secret leaked to %s", name)
		}
	}
	if progress.Len() != 0 {
		t.Fatal("secret command forwarded runner stderr")
	}
}
