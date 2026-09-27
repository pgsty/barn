package macvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pgsty/farrow/internal/lock"
	"golang.org/x/sys/unix"
)

func TestSetupUXRunnerFixture(t *testing.T) {
	if os.Getenv("FARROW_TEST_SETUP_UX") != "1" {
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
	_, _ = io.Copy(io.Discard, os.Stdin)
	f, err := os.OpenFile(os.Getenv("FARROW_TEST_SETUP_CALLS"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(91)
	}
	_ = json.NewEncoder(f).Encode(args)
	_ = f.Close()
	metadata := RestoreMetadata{OK: true, Version: "27.1", Build: "26B100", HardwareModelHash: strings.Repeat("a", 64), MinimumCPU: 2, MinimumMemory: 4 << 30}
	if raw := os.Getenv("FARROW_TEST_SETUP_METADATA"); raw != "" {
		if json.Unmarshal([]byte(raw), &metadata) != nil {
			os.Exit(92)
		}
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
	case "metadata":
		_ = json.NewEncoder(os.Stdout).Encode(metadata)
	case "discover":
		_ = json.NewEncoder(os.Stdout).Encode(restoreDiscovery{RestoreMetadata: metadata, URL: "https://updates.cdn-apple.com/never-requested-fixture.ipsw"})
	case "hardware":
		hash := strings.Repeat("a", 64)
		if os.Getenv("FARROW_TEST_SETUP_HARDWARE") == "tampered" {
			hash = strings.Repeat("b", 64)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "supported": os.Getenv("FARROW_TEST_SETUP_HARDWARE") == "supported", "hardware_model_sha256": hash})
	case "restore":
		for _, name := range []string{"disk.asif", "hardware-model.bin", "machine-id.bin", "auxiliary-storage.bin", "base.json"} {
			data := []byte("fixture " + name)
			if name == "base.json" {
				data = []byte(`{"ready":true,"first_boot":false}`)
			}
			if os.WriteFile(filepath.Join(value("--base"), name), data, 0o600) != nil {
				os.Exit(93)
			}
		}
		_ = json.NewEncoder(os.Stdout).Encode(metadata)
	default:
		os.Exit(94)
	}
	os.Exit(0)
}

func setupUXManager(t *testing.T, config bool) (*Manager, *lock.File, string) {
	t.Helper()
	store := testStore(t)
	held := holdStore(t, store)
	if config {
		cfg, err := NewConfig(DefaultSubnet)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveConfig(held, cfg); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	runner := filepath.Join(dir, "runner")
	quoted := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "'"
	if err := os.WriteFile(runner, []byte("#!/bin/sh\nexec "+quoted+" -test.run='^TestSetupUXRunnerFixture$' -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls.jsonl")
	t.Setenv("FARROW_TEST_SETUP_UX", "1")
	t.Setenv("FARROW_TEST_SETUP_CALLS", calls)
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	return &Manager{Store: store, Runner: Runner{Binary: runner}}, held, calls
}

func readySetupUXBase(t *testing.T, m *Manager, held *lock.File, disk int64) *BaseImage {
	t.Helper()
	id, err := BaseImageID("26B100", disk, strings.Repeat("a", 64), 1)
	if err != nil {
		t.Fatal(err)
	}
	base := &BaseImage{SchemaVersion: 1, ID: id, Version: "27.1", Build: "26B100", HardwareModelHash: strings.Repeat("a", 64), InstallerSHA256: strings.Repeat("b", 64), State: "ready", DiskBytes: disk, RecipeVersion: 1, CreatedAt: time.Now().UTC()}
	if err := m.Store.SaveBase(held, base); err != nil {
		t.Fatal(err)
	}
	dir, _ := m.Store.BasePath(id)
	for _, name := range []string{"disk.asif", "hardware-model.bin", "machine-id.bin", "auxiliary-storage.bin", "base.json"} {
		data := []byte("fixture " + name)
		if name == "base.json" {
			data = []byte(`{"ready":true,"first_boot":false}`)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := m.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultBaseID = id
	if err := m.Store.SaveConfig(held, cfg); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestSetupUXCapacityFlowsToNewSlotsWithoutChangingExistingPreferences(t *testing.T) {
	m, held, _ := setupUXManager(t, true)
	cfg, _ := m.Store.LoadConfig()
	cfg.Slots[0].ResourcesConfigured = true
	if err := m.Store.SaveConfig(held, cfg); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "local.ipsw")
	if err := os.WriteFile(source, []byte("Apple metadata fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	networkCalls := 0
	base, err := m.setupWithChecks(context.Background(), held, SetupOptions{IPSW: source, DiskBytes: 200 << 30}, func(context.Context, *Config) error { networkCalls++; return nil }, func(string) (uint64, error) { return 100 << 30, nil })
	if err != nil || networkCalls != 1 || base.DiskBytes != 200<<30 {
		t.Fatalf("setup base=%+v network calls=%d error=%v", base, networkCalls, err)
	}
	for _, test := range []struct {
		name string
		disk int64
	}{{"mac1", DefaultDiskBytes}, {"mac2", 200 << 30}} {
		options, err := m.setupOptionsForSlot(test.name, SetupOptions{})
		if err != nil || options.DiskBytes != test.disk {
			t.Fatalf("%s disk=%d expected=%d error=%v", test.name, options.DiskBytes, test.disk, err)
		}
	}
	after, _ := m.Store.LoadConfig()
	if !reflect.DeepEqual(cfg.Slots, after.Slots) || cfg.Network != after.Network || cfg.InstallationID != after.InstallationID {
		t.Fatal("preparing a new capacity changed existing preferences or identity")
	}
	override, err := m.setupOptionsForSlot("mac2", SetupOptions{DiskBytes: 160 << 30})
	if err != nil || override.DiskBytes != 160<<30 {
		t.Fatalf("explicit capacity lost: %+v %v", override, err)
	}
}

func TestSetupUXUpdateReusesCompatibleBaseBeforeSpaceAndDownload(t *testing.T) {
	m, held, calls := setupUXManager(t, true)
	base := readySetupUXBase(t, m, held, 200<<30)
	var progress bytes.Buffer
	m.Progress = &progress
	spaceCalls, networkCalls := 0, 0
	result, err := m.setupWithChecks(context.Background(), held, SetupOptions{Update: true}, func(context.Context, *Config) error { networkCalls++; return nil }, func(string) (uint64, error) { spaceCalls++; return 0, nil })
	if err != nil || result.ID != base.ID || result.DiskBytes != 200<<30 || spaceCalls != 0 || networkCalls != 1 {
		t.Fatalf("base=%+v space calls=%d network calls=%d error=%v", result, spaceCalls, networkCalls, err)
	}
	data, err := os.ReadFile(calls)
	if err != nil || string(data) != "[\"discover\"]\n" {
		t.Fatalf("no-op update called more than discovery: %s %v", data, err)
	}
	if !strings.Contains(progress.String(), "no download or restore needed") {
		t.Fatalf("missing no-op explanation: %s", &progress)
	}
	cache, _ := m.Store.Path("images", "ipsw")
	if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-op update touched installer cache: %v", err)
	}
}

func TestSetupUXImageUpdateKeepsCompatibleHelperWithRunningGuests(t *testing.T) {
	status := NetworkStatus{BuildID: "previous", Connected: []bool{true, true}, SlotSources: make([]NetworkSlotSource, 2)}
	if upgrade, err := networkUpgradeDecision(status, "current", (SetupOptions{Update: true}).upgradesNetwork()); err != nil || upgrade {
		t.Fatalf("image update required stopping running guests for a compatible helper: upgrade=%t error=%v", upgrade, err)
	}
	if _, err := networkUpgradeDecision(status, "current", (SetupOptions{}).upgradesNetwork()); err == nil {
		t.Fatal("explicit setup allowed a helper replacement under running guests")
	}
}

func TestSetupUXUpdateDoesNotReuseWrongHardwareOrCapacity(t *testing.T) {
	for _, kind := range []string{"hardware", "capacity"} {
		t.Run(kind, func(t *testing.T) {
			m, held, _ := setupUXManager(t, true)
			readySetupUXBase(t, m, held, 200<<30)
			options := SetupOptions{Update: true}
			if kind == "hardware" {
				t.Setenv("FARROW_TEST_SETUP_METADATA", `{"hardware_model_sha256":"`+strings.Repeat("c", 64)+`"}`)
			} else {
				options.DiskBytes = 160 << 30
			}
			networkCalls := 0
			_, err := m.setupWithChecks(context.Background(), held, options, func(context.Context, *Config) error { networkCalls++; return nil }, func(string) (uint64, error) { return 0, nil })
			if err == nil || !strings.Contains(err.Error(), "50 GiB") || networkCalls != 0 {
				t.Fatalf("incompatible base reused or requested authorization: calls=%d error=%v", networkCalls, err)
			}
		})
	}
}

func TestSetupUXUpdateReusesDifferentCatalogModelOnlyWhenVerified(t *testing.T) {
	for _, mode := range []string{"supported", "tampered", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			m, held, _ := setupUXManager(t, true)
			base := readySetupUXBase(t, m, held, 200<<30)
			t.Setenv("FARROW_TEST_SETUP_METADATA", `{"hardware_model_sha256":"`+strings.Repeat("c", 64)+`"}`)
			t.Setenv("FARROW_TEST_SETUP_HARDWARE", mode)
			spaceCalls := 0
			plan, err := m.preflightSetup(context.Background(), SetupOptions{Update: true}, func(string) (uint64, error) { spaceCalls++; return 0, nil })
			if mode == "supported" {
				if err != nil || plan.readyBase == nil || plan.readyBase.ID != base.ID || spaceCalls != 0 {
					t.Fatalf("supported cached model was not reused: %+v %v", plan, err)
				}
			} else if err == nil || plan.readyBase != nil {
				t.Fatalf("unverified model reused: %+v %v", plan, err)
			}
			if mode == "tampered" && spaceCalls != 0 {
				t.Fatal("tampered cached identity continued to download preflight")
			}
		})
	}
}

func TestSetupUXUpdateRejectsConflictingCachedIdentityBeforeNetwork(t *testing.T) {
	for _, field := range []string{"version", "build", "hardware", "capacity", "recipe"} {
		t.Run(field, func(t *testing.T) {
			m, held, _ := setupUXManager(t, true)
			base := readySetupUXBase(t, m, held, 200<<30)
			switch field {
			case "version":
				base.Version = "27.2"
			case "build":
				base.Build = "26C100"
			case "hardware":
				base.HardwareModelHash = strings.Repeat("c", 64)
			case "capacity":
				base.DiskBytes = 160 << 30
			case "recipe":
				base.RecipeVersion = 2
			}
			path, err := m.Store.Path("images", "base", base.ID, "metadata.json")
			if err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(path, base); err != nil {
				t.Fatal(err)
			}
			networkCalls, spaceCalls := 0, 0
			_, err = m.setupWithChecks(context.Background(), held, SetupOptions{Update: true, DiskBytes: 200 << 30}, func(context.Context, *Config) error { networkCalls++; return nil }, func(string) (uint64, error) { spaceCalls++; return 100 << 30, nil })
			if err == nil || !strings.Contains(err.Error(), "cached base metadata") || networkCalls != 0 || spaceCalls != 0 {
				t.Fatalf("conflicting cache passed preflight: network=%d space=%d error=%v", networkCalls, spaceCalls, err)
			}
		})
	}
}

func TestSetupUXPreflightRejectsBeforeNetworkAndConfig(t *testing.T) {
	for _, kind := range []string{"small-disk", "invalid-subnet", "missing-ipsw", "directory-ipsw", "pipe-ipsw", "empty-ipsw", "unsupported-ipsw", "free-space"} {
		t.Run(kind, func(t *testing.T) {
			m, held, _ := setupUXManager(t, false)
			options := SetupOptions{}
			switch kind {
			case "small-disk":
				options.DiskBytes = 31 << 30
			case "invalid-subnet":
				options.Subnet = "8.8.8.0/24"
			case "missing-ipsw":
				options.IPSW = filepath.Join(t.TempDir(), "absent.ipsw")
			case "directory-ipsw":
				options.IPSW = t.TempDir()
			case "pipe-ipsw":
				options.IPSW = filepath.Join(t.TempDir(), "pipe.ipsw")
				if err := unix.Mkfifo(options.IPSW, 0o600); err != nil {
					t.Fatal(err)
				}
			case "empty-ipsw", "unsupported-ipsw":
				options.IPSW = filepath.Join(t.TempDir(), "image.ipsw")
				data := []byte{}
				if kind == "unsupported-ipsw" {
					data = []byte("not macOS 27")
					t.Setenv("FARROW_TEST_SETUP_METADATA", `{"version":"26.0"}`)
				}
				if err := os.WriteFile(options.IPSW, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			networkCalls := 0
			_, err := m.setupWithChecks(context.Background(), held, options, func(context.Context, *Config) error { networkCalls++; return nil }, func(string) (uint64, error) { return 0, nil })
			if err == nil || networkCalls != 0 {
				t.Fatalf("preflight failed to stop authorization: calls=%d err=%v", networkCalls, err)
			}
			if _, err := m.Store.LoadConfig(); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid options wrote configuration: %v", err)
			}
		})
	}
}

func TestSetupUXPendingSubnetRequiresAbsentInstallationAndArtifacts(t *testing.T) {
	for _, kind := range []string{"canceled-first-auth", "installed", "unknown-installation", "base-artifact", "slot-artifact", "ready-base", "route-conflict"} {
		t.Run(kind, func(t *testing.T) {
			m, held, _ := setupUXManager(t, true)
			cfg, _ := m.Store.LoadConfig()
			switch kind {
			case "base-artifact":
				if _, err := m.Store.mkdir("images", "base", "partial"); err != nil {
					t.Fatal(err)
				}
			case "slot-artifact":
				dir, err := m.Store.mkdir("slots", "mac1")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "disk.asif"), []byte("orphan guest"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "ready-base":
				readySetupUXBase(t, m, held, DefaultDiskBytes)
				cfg, _ = m.Store.LoadConfig()
			}
			before, _ := os.ReadFile(filepath.Join(m.Store.Root, "config.json"))
			changed, err := m.reselectPendingSubnet(context.Background(), held, cfg, "10.10.24.0/24", func(context.Context, *Config) (bool, error) {
				if kind == "unknown-installation" {
					return false, errors.New("permission denied")
				}
				return kind == "installed", nil
			}, func(_ context.Context, requested string) (string, error) {
				if kind == "route-conflict" {
					return "", errors.New("route conflict")
				}
				return requested, nil
			})
			if kind == "canceled-first-auth" {
				if err != nil || changed.Network.Subnet != "10.10.24.0/24" || changed.Network.Gateway != "10.10.24.1" || changed.Slots[0].IP != "10.10.24.10" || changed.Slots[1].IP != "10.10.24.11" {
					t.Fatalf("unused subnet did not change safely: %+v %v", changed, err)
				}
				if changed.InstallationID != cfg.InstallationID || changed.Network.NetworkID != cfg.Network.NetworkID || changed.Slots[0].MAC != cfg.Slots[0].MAC || changed.Slots[1].MAC != cfg.Slots[1].MAC {
					t.Fatal("subnet correction changed installation identity")
				}
			} else {
				if err == nil {
					t.Fatal("changed an established or uncertain configuration")
				}
				after, _ := os.ReadFile(filepath.Join(m.Store.Root, "config.json"))
				if !bytes.Equal(before, after) {
					t.Fatal("rejected subnet correction changed configuration")
				}
			}
		})
	}
}
