package macvm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pgsty/farrow/internal/lock"
)

// This runner only touches temporary fixture files. In particular it never
// accesses Keychain, the host network, or Apple virtualization APIs.
func TestMacPreferencesRunnerFixture(t *testing.T) {
	if os.Getenv("FARROW_TEST_PREFERENCES_RUNNER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(90)
	}
	args = args[1:]
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		os.Exit(91)
	}
	capture, err := os.OpenFile(os.Getenv("FARROW_TEST_PREFERENCES_CAPTURE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(92)
	}
	if json.NewEncoder(capture).Encode(args) != nil || capture.Close() != nil {
		os.Exit(93)
	}
	value := func(flag string) string {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag {
				return args[i+1]
			}
		}
		return ""
	}
	switch args[0] {
	case "secret":
		if len(args) < 2 || (args[1] != "set" && args[1] != "delete") {
			os.Exit(94)
		}
	case "clone":
		if os.Getenv("FARROW_TEST_PREFERENCES_CLONE_FAIL") == "1" {
			os.Exit(95)
		}
		for _, name := range []string{"disk.asif", "machine-id.bin", "auxiliary-storage.bin"} {
			if os.WriteFile(filepath.Join(value("--slot"), name), []byte("fixture "+name), 0o600) != nil {
				os.Exit(96)
			}
		}
	case "rpc":
		_ = json.NewEncoder(os.Stdout).Encode(RuntimeStatus{OK: true, Instance: value("--instance"), State: "stopped"})
		os.Exit(0)
	default:
		os.Exit(97)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]bool{"ok": true})
	os.Exit(0)
}

func preferencesRunner(t *testing.T) (Runner, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "runner")
	quoted := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "'"
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec "+quoted+" -test.run='^TestMacPreferencesRunnerFixture$' -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(dir, "calls.jsonl")
	t.Setenv("FARROW_TEST_PREFERENCES_RUNNER", "1")
	t.Setenv("FARROW_TEST_PREFERENCES_CAPTURE", capture)
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	return Runner{Binary: script}, capture
}

func preferencesManager(t *testing.T) (*Manager, *lock.File, *BaseImage, string) {
	t.Helper()
	store := testStore(t)
	held := holdStore(t, store)
	base := *testBase(t, store, held, "preference-default")
	base.ID, base.DiskBytes = "preference-large", 160<<30
	if err := store.SaveBase(held, &base); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"disk.asif", "hardware-model.bin", "auxiliary-storage.bin", "machine-id.bin", "base.json"} {
		path, err := store.Path("images", "base", base.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		data := []byte("fixture " + name)
		if name == "base.json" {
			data = []byte(`{"ready":true,"first_boot":false,"disk_size":171798691840}`)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config, err := NewConfig(DefaultSubnet)
	if err != nil {
		t.Fatal(err)
	}
	config.DefaultBaseID = base.ID
	if err := store.SaveConfig(held, config); err != nil {
		t.Fatal(err)
	}
	runner, capture := preferencesRunner(t)
	return &Manager{Store: store, Runner: runner}, held, &base, capture
}

func TestInitPersistsActualResourcesAndReusesPreferences(t *testing.T) {
	m, held, base, capture := preferencesManager(t)
	before, err := m.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	slot, err := m.initLocked(context.Background(), held, "mac1", InitOptions{User: "farrow", CPU: 6, MemoryBytes: 12 << 30}, base)
	if err != nil {
		t.Fatal(err)
	}
	config, err := m.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	pref, err := config.Preference("mac1")
	if err != nil || pref.CPU != 6 || pref.MemoryBytes != 12<<30 || pref.DiskBytes != base.DiskBytes {
		t.Fatalf("created resources not persisted: %+v error=%v", pref, err)
	}
	if config.Network != before.Network || config.DefaultBaseID != base.ID || config.Slots[1] != before.Slots[1] || config.Slots[0].IP != before.Slots[0].IP || config.Slots[0].MAC != before.Slots[0].MAC {
		t.Fatalf("resource changes affected network, other slot, or default base: %+v", config)
	}
	callsBefore, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	// Conflicting creation options are rejected before changing data or identity.
	again, err := m.initLocked(context.Background(), held, "mac1", InitOptions{SetupOptions: SetupOptions{DiskBytes: 32 << 30, Update: true}, User: "other", CPU: 2, MemoryBytes: 4 << 30}, nil)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("conflicting init did not explain the conflict: %+v %v", again, err)
	}
	again, err = m.initLocked(context.Background(), held, "mac1", InitOptions{}, nil)
	if err != nil || !reflect.DeepEqual(again, slot) {
		t.Fatalf("plain init changed existing instance: %+v %v", again, err)
	}
	after, err := m.Store.LoadConfig()
	if err != nil || !reflect.DeepEqual(config, after) {
		t.Fatalf("repeated init changed preferences: %+v error=%v", after, err)
	}
	callsAfter, err := os.ReadFile(capture)
	if err != nil || string(callsBefore) != string(callsAfter) {
		t.Fatalf("repeated init called runner: %q error=%v", callsAfter, err)
	}
	if err := m.Store.DeleteSlot(held, "mac1"); err != nil {
		t.Fatal(err)
	}
	setup, err := m.setupOptionsForSlot("mac1", SetupOptions{})
	if err != nil || setup.DiskBytes != base.DiskBytes {
		t.Fatalf("destroy lost disk preference: %+v error=%v", setup, err)
	}
	requested := SetupOptions{DiskBytes: 192 << 30, IPSW: "/local/restore.ipsw", Update: true}
	setup, err = m.setupOptionsForSlot("mac1", requested)
	if err != nil || setup != requested {
		t.Fatalf("explicit disk did not override preference: %+v error=%v", setup, err)
	}
	recreated, err := m.initLocked(context.Background(), held, "mac1", InitOptions{User: "farrow"}, base)
	if err != nil || recreated.CPU != 6 || recreated.MemoryBytes != 12<<30 || recreated.DiskBytes != 160<<30 || recreated.InstanceID == slot.InstanceID {
		t.Fatalf("recreation lost resources or reused identity: %+v error=%v", recreated, err)
	}
}

func TestInitFailureDoesNotClaimSuccessfulResourcePreferences(t *testing.T) {
	m, held, base, _ := preferencesManager(t)
	before, err := m.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FARROW_TEST_PREFERENCES_CLONE_FAIL", "1")
	if _, err := m.initLocked(context.Background(), held, "mac1", InitOptions{User: "farrow", CPU: 6, MemoryBytes: 12 << 30}, base); err == nil {
		t.Fatal("clone failure reported successful initialization")
	}
	after, err := m.Store.LoadConfig()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed creation changed preferences: %+v error=%v", after, err)
	}
	slot, err := m.Store.LoadSlot("mac1")
	if err != nil || slot.State != "failed" || slot.CPU != 6 || slot.MemoryBytes != 12<<30 {
		t.Fatalf("failed instance not retained for recovery: %+v error=%v", slot, err)
	}
}

func TestInitDiskPreferenceForFreshInstallationIsReadOnly(t *testing.T) {
	m := &Manager{Store: testStore(t)}
	options, err := m.setupOptionsForSlot("mac1", SetupOptions{})
	if err != nil || options.DiskBytes != DefaultDiskBytes {
		t.Fatalf("fresh installation disk default: %+v error=%v", options, err)
	}
	if _, err := os.Stat(m.Store.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading resource defaults created state: %v", err)
	}
}

func TestResetPreservesActualResourcesDespiteStalePreferences(t *testing.T) {
	if HostSupported() != nil || os.Geteuid() == 0 {
		t.Skip("public manager requires an ordinary user on Apple Silicon/macOS 27")
	}
	m, held, base, _ := preferencesManager(t)
	slot, err := m.initLocked(context.Background(), held, "mac1", InitOptions{User: "farrow", CPU: 6, MemoryBytes: 12 << 30}, base)
	if err != nil {
		t.Fatal(err)
	}
	config, err := m.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.Slots[0].CPU, config.Slots[0].MemoryBytes, config.Slots[0].DiskBytes = 2, 4<<30, 64<<30
	config.DefaultBaseID = "preference-default" // Ordinary reset must keep its original base.
	if err := m.Store.SaveConfig(held, config); err != nil {
		t.Fatal(err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	recreated, err := m.Destroy(context.Background(), "mac1", true, false)
	if err != nil || recreated.CPU != slot.CPU || recreated.MemoryBytes != slot.MemoryBytes || recreated.DiskBytes != slot.DiskBytes || recreated.BaseID != slot.BaseID || recreated.InstanceID == slot.InstanceID {
		t.Fatalf("reset changed actual resources or reused identity: %+v error=%v", recreated, err)
	}
	config, err = m.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	pref, err := config.Preference("mac1")
	if err != nil || pref.CPU != slot.CPU || pref.MemoryBytes != slot.MemoryBytes || pref.DiskBytes != slot.DiskBytes {
		t.Fatalf("reset did not persist actual resources: %+v error=%v", pref, err)
	}
}

func TestResetCapacityMismatchPreservesGuestBeforeAnyRunnerCall(t *testing.T) {
	if HostSupported() != nil || os.Geteuid() == 0 {
		t.Skip("public manager requires an ordinary user on Apple Silicon/macOS 27")
	}
	for _, update := range []bool{false, true} {
		t.Run(map[bool]string{false: "original-base", true: "updated-base"}[update], func(t *testing.T) {
			m, held, base, capture := preferencesManager(t)
			slot := testSlot(t, "mac1", base.ID)
			slot.DiskBytes = 200 << 30
			if update {
				slot.BaseID, slot.DiskBytes = "preference-default", DefaultDiskBytes
			}
			if err := m.Store.SaveSlot(held, slot); err != nil {
				t.Fatal(err)
			}
			dir, err := m.Store.SlotPath(slot.Name)
			if err != nil {
				t.Fatal(err)
			}
			disk := filepath.Join(dir, "disk.asif")
			if err := os.WriteFile(disk, []byte("valuable guest content"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := held.Release(); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Destroy(context.Background(), "mac1", true, update); err == nil || !strings.Contains(err.Error(), "matching capacity") {
				t.Fatalf("capacity mismatch was not rejected clearly: %v", err)
			}
			if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("capacity mismatch contacted runner before rejection: %v", err)
			}
			stored, err := m.Store.LoadSlot(slot.Name)
			if err != nil || !reflect.DeepEqual(stored, slot) {
				t.Fatalf("capacity mismatch changed slot state: %+v error=%v", stored, err)
			}
			requireRecoveryFile(t, disk, "valuable guest content")
		})
	}
}

func TestResetIncompleteBasePreservesGuestBeforeAnyRunnerCall(t *testing.T) {
	if HostSupported() != nil || os.Geteuid() == 0 {
		t.Skip("public manager requires an ordinary user on Apple Silicon/macOS 27")
	}
	for _, broken := range []string{"disk.asif", "hardware-model.bin", "auxiliary-storage.bin", "machine-id.bin", "base.json", "failed", "restoring", "symlink", "directory", "empty", "booted", "native-not-ready", "invalid-json"} {
		t.Run(broken, func(t *testing.T) {
			m, held, base, capture := preferencesManager(t)
			slot := testSlot(t, "mac1", base.ID)
			slot.DiskBytes = base.DiskBytes
			if err := m.Store.SaveSlot(held, slot); err != nil {
				t.Fatal(err)
			}
			dir, err := m.Store.SlotPath(slot.Name)
			if err != nil {
				t.Fatal(err)
			}
			disk := filepath.Join(dir, "disk.asif")
			if err := os.WriteFile(disk, []byte("preserve guest despite broken base"), 0o600); err != nil {
				t.Fatal(err)
			}
			basePath, err := m.Store.BasePath(base.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch broken {
			case "failed", "restoring":
				base.State = broken
				err = writeJSON(filepath.Join(basePath, "metadata.json"), base)
			case "symlink":
				if err = os.Remove(filepath.Join(basePath, "disk.asif")); err == nil {
					err = os.Symlink(disk, filepath.Join(basePath, "disk.asif"))
				}
			case "directory":
				if err = os.Remove(filepath.Join(basePath, "hardware-model.bin")); err == nil {
					err = os.Mkdir(filepath.Join(basePath, "hardware-model.bin"), 0o700)
				}
			case "empty":
				err = os.WriteFile(filepath.Join(basePath, "auxiliary-storage.bin"), nil, 0o600)
			case "booted":
				err = os.WriteFile(filepath.Join(basePath, "base.json"), []byte(`{"ready":true,"first_boot":true}`), 0o600)
			case "native-not-ready":
				err = os.WriteFile(filepath.Join(basePath, "base.json"), []byte(`{"ready":false,"first_boot":false}`), 0o600)
			case "invalid-json":
				err = os.WriteFile(filepath.Join(basePath, "base.json"), []byte(`{`), 0o600)
			default:
				err = os.Remove(filepath.Join(basePath, broken))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := held.Release(); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Destroy(context.Background(), "mac1", true, false); err == nil || !strings.Contains(err.Error(), "cannot reset") {
				t.Fatalf("incomplete base did not reject reset: %v", err)
			}
			if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("incomplete base contacted runner before rejection: %v", err)
			}
			stored, err := m.Store.LoadSlot(slot.Name)
			if err != nil || !reflect.DeepEqual(stored, slot) {
				t.Fatalf("incomplete base changed slot state: %+v error=%v", stored, err)
			}
			requireRecoveryFile(t, disk, "preserve guest despite broken base")
		})
	}
}
