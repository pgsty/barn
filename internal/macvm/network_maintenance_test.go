package macvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgsty/farrow/internal/lock"
)

func TestNetworkUpgradeIsExplicitAndProtectsAttachedGuests(t *testing.T) {
	expected := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, build                         string
		sources                             int
		attached, explicit, upgrade, reject bool
	}{
		{"current", expected, 2, true, true, false, false},
		{"new-package-idle-setup", "old", 2, false, true, true, false},
		{"new-package-active-setup", "old", 2, true, true, false, true},
		{"daily-up-compatible", "old", 2, true, false, false, false},
		{"daily-up-incompatible", "old", 0, false, false, false, true},
		{"unversioned-helper-setup", "", 2, false, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := NetworkStatus{BuildID: tc.build, Connected: []bool{tc.attached, false}, SlotSources: make([]NetworkSlotSource, tc.sources)}
			upgrade, err := networkUpgradeDecision(status, expected, tc.explicit)
			if upgrade != tc.upgrade || (err != nil) != tc.reject {
				t.Fatalf("upgrade=%t err=%v", upgrade, err)
			}
		})
	}
}

func TestNetworkPresenceRequiresAllFilesAndServiceAbsent(t *testing.T) {
	paths := networkPaths{directory: "config-directory", config: "config", plist: "plist", socket: "socket", label: "only-this-label"}
	for _, present := range []string{"", paths.directory, paths.config, paths.plist, paths.socket} {
		serviceCalled := false
		found, err := networkInstallationPresent(paths, func(path string) (os.FileInfo, error) {
			if path == present {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}, func(label string) (bool, error) {
			serviceCalled = true
			if label != paths.label {
				t.Fatal(label)
			}
			return false, nil
		})
		if err != nil || found != (present != "") || serviceCalled != (present == "") {
			t.Fatalf("present=%q found=%t service=%t err=%v", present, found, serviceCalled, err)
		}
	}
	for _, registered := range []bool{false, true} {
		found, err := networkInstallationPresent(paths, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }, func(string) (bool, error) { return registered, nil })
		if err != nil || found != registered {
			t.Fatalf("registered=%t found=%t err=%v", registered, found, err)
		}
	}
	if _, err := networkInstallationPresent(paths, func(string) (os.FileInfo, error) { return nil, os.ErrPermission }, func(string) (bool, error) { t.Fatal("probed through unknown file state"); return false, nil }); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	unknown := errors.New("launchctl failed")
	if _, err := networkInstallationPresent(paths, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }, func(string) (bool, error) { return false, unknown }); !errors.Is(err, unknown) {
		t.Fatal(err)
	}
}

func networkRemovalFixture(t *testing.T, withSlot bool) (*Manager, *Config) {
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
	if withSlot {
		base := testBase(t, s, held, "network-preserved-base")
		if err = s.SaveSlot(held, testSlot(t, "mac1", base.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if err = held.Release(); err != nil {
		t.Fatal(err)
	}
	return &Manager{Store: s}, config
}

func TestNetworkRemovalPreservesGuestDataAndChecksAbsence(t *testing.T) {
	m, config := networkRemovalFixture(t, true)
	configPath, _ := m.Store.Path("config.json")
	statePath, _ := m.Store.Path("slots", "mac1", "state.json")
	beforeConfig, _ := os.ReadFile(configPath)
	beforeSlot, _ := os.ReadFile(statePath)
	present := true
	removed := 0
	result, err := m.uninstallNetwork(context.Background(), networkRemovalOps{
		present: func(context.Context, *Config) (bool, error) { return present, nil },
		runtime: func(context.Context, *Slot) (RuntimeStatus, error) { return RuntimeStatus{}, os.ErrNotExist },
		status: func(context.Context, *Config) (NetworkStatus, error) {
			return NetworkStatus{Connected: []bool{false, false}}, nil
		},
		remove: func(_ context.Context, c *Config) error {
			if c.InstallationID != config.InstallationID {
				t.Fatal("other installation")
			}
			removed++
			present = false
			return nil
		},
	})
	if err != nil || !result.Uninstalled || !result.DataPreserved || !result.SharedBinaryRetained || removed != 1 {
		t.Fatalf("%+v removed=%d err=%v", result, removed, err)
	}
	afterConfig, _ := os.ReadFile(configPath)
	afterSlot, _ := os.ReadFile(statePath)
	if string(beforeConfig) != string(afterConfig) || string(beforeSlot) != string(afterSlot) {
		t.Fatal("uninstall modified guest configuration")
	}
	if _, err := m.Store.LoadBase("network-preserved-base"); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkRemovalRejectsOperationsRunnersAndConnections(t *testing.T) {
	for _, kind := range []string{"mac1.operation.lock", "mac2.operation.lock", "state.lock", "runner", "runtime", "connected", "unknown-presence", "incomplete-removal", "absent"} {
		t.Run(kind, func(t *testing.T) {
			m, _ := networkRemovalFixture(t, true)
			var proof *lock.File
			var err error
			if strings.HasSuffix(kind, ".lock") || kind == "runner" {
				path := filepath.Join(m.Store.Root, "runtime", kind)
				if kind == "runner" {
					path = filepath.Join(m.Store.Root, "slots", "mac1", "runner.lock")
				}
				proof, err = lock.TryAcquire(path, false)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = proof.Release() }()
			}
			removed := 0
			result, err := m.uninstallNetwork(context.Background(), networkRemovalOps{
				present: func(context.Context, *Config) (bool, error) {
					if kind == "unknown-presence" {
						return false, os.ErrPermission
					}
					return kind != "absent", nil
				},
				runtime: func(context.Context, *Slot) (RuntimeStatus, error) {
					if kind == "runtime" {
						return RuntimeStatus{State: "running"}, nil
					}
					return RuntimeStatus{}, os.ErrNotExist
				},
				status: func(context.Context, *Config) (NetworkStatus, error) {
					return NetworkStatus{Connected: []bool{kind == "connected", false}}, nil
				},
				remove: func(context.Context, *Config) error { removed++; return nil },
			})
			if kind == "absent" {
				if err != nil || !result.AlreadyAbsent || removed != 0 {
					t.Fatalf("%+v removed=%d err=%v", result, removed, err)
				}
				return
			}
			if err == nil {
				t.Fatal("unsafe operation accepted")
			}
			wantRemove := 0
			if kind == "incomplete-removal" {
				wantRemove = 1
			}
			if removed != wantRemove {
				t.Fatalf("privileged calls=%d wanted=%d err=%v", removed, wantRemove, err)
			}
		})
	}
}

func TestNetworkBuildIdentityRejectsUnversionedBundle(t *testing.T) {
	for _, build := range []string{"", "not-a-sha", strings.Repeat("a", 64)} {
		dir := t.TempDir()
		script := "#!/bin/sh\nprintf '%s\\n' '{\"name\":\"farrow-mac-network\",\"protocol\":1,\"build_id\":\"" + build + "\"}'\n"
		if err := os.WriteFile(filepath.Join(dir, "farrow-mac-network"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		m := &Manager{Runner: Runner{Binary: filepath.Join(dir, "farrow-mac-runner")}}
		got, err := m.networkBuildID(context.Background())
		if (err == nil) != (len(build) == 64) || err == nil && got != build {
			t.Fatalf("build=%q got=%q err=%v", build, got, err)
		}
	}
}

func TestNetworkInfoWithoutSetupIsReadOnly(t *testing.T) {
	s := testStore(t)
	m := &Manager{Store: s}
	info, err := m.NetworkInfo(context.Background())
	if err != nil || info.Configured || info.Installed || info.Root != s.Root || info.Diagnostic == "" {
		t.Fatalf("%+v err=%v", info, err)
	}
	if _, err = os.Stat(s.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("network status created state: %v", err)
	}
}
