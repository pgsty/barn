package macvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pgsty/farrow/internal/fsutil"
)

type NetworkStatus struct {
	BuildID        string              `json:"build_id"`
	OK             bool                `json:"ok"`
	UID            int                 `json:"uid"`
	InstallationID string              `json:"installation_id"`
	Subnet         string              `json:"subnet"`
	Socket         string              `json:"socket"`
	PID            int                 `json:"pid"`
	NetworkActive  bool                `json:"network_active"`
	Connected      []bool              `json:"connected"`
	SlotSources    []NetworkSlotSource `json:"slot_sources"`
}

// New helpers keep a network anchor alive even with no attached guest. Older
// helpers omit network_active and expose active interfaces through Connected.
func (status *NetworkStatus) hasActiveNetwork() bool {
	if status == nil {
		return false
	}
	if status.NetworkActive {
		return true
	}
	for _, connected := range status.Connected {
		if connected {
			return true
		}
	}
	return false
}

func (m *Manager) networkSocket(config *Config) (string, error) {
	if err := config.Validate(); err != nil {
		return "", err
	}
	return fmt.Sprintf("/var/run/farrow-mac/%d-%s.sock", os.Getuid(), strings.ToLower(config.InstallationID)), nil
}

func (m *Manager) networkBinary() (string, error) {
	return executableFile(filepath.Join(filepath.Dir(m.Runner.Binary), "farrow-mac-network"))
}

func (m *Manager) networkStatus(ctx context.Context, config *Config) (NetworkStatus, error) {
	var status NetworkStatus
	binary, err := m.networkBinary()
	if err != nil {
		return status, err
	}
	socket, err := m.networkSocket(config)
	if err != nil {
		return status, err
	}
	err = (Runner{Binary: binary}).Call(ctx, nil, &status, "status", "--socket", socket)
	if err == nil && (!status.OK || status.UID != os.Getuid() || status.InstallationID != config.InstallationID || status.Subnet != config.Network.Subnet || status.Socket != socket || len(status.Connected) != 2) {
		return status, errors.New("mac network helper identity/configuration mismatch")
	}
	return status, err
}

func (m *Manager) checkNetworkRoutes(ctx context.Context, config *Config, status *NetworkStatus) error {
	output, err := exec.CommandContext(ctx, "/usr/sbin/netstat", "-rn", "-f", "inet").Output()
	if err != nil {
		return fmt.Errorf("read host IPv4 routes: %w", err)
	}
	routes, err := ParseDarwinRouteTable(string(output))
	if err != nil {
		return err
	}
	active := status.hasActiveNetwork()
	var interfaces []byte
	if active {
		interfaces, err = exec.CommandContext(ctx, "/sbin/ifconfig").Output()
		if err != nil {
			return fmt.Errorf("identify active Mac network bridge: %w", err)
		}
	}
	return CheckLiveSubnet(config.Network, routes, string(interfaces), active)
}

// Ordinary existing-slot operations never request administrator authentication.
func (m *Manager) ensureNetwork(ctx context.Context, config *Config) error {
	return m.ensureNetworkMode(ctx, config, false, false)
}

// First-time up/init may install a missing helper, but do not upgrade an existing
// service as a side effect of creating another slot.
func (m *Manager) ensureNetworkForPreparation(ctx context.Context, config *Config) error {
	return m.ensureNetworkMode(ctx, config, true, false)
}

func (m *Manager) ensureNetworkForSetup(ctx context.Context, config *Config) error {
	return m.ensureNetworkMode(ctx, config, true, true)
}

func networkNeedsUpgrade(status NetworkStatus, expected string) bool {
	return status.BuildID != expected || len(status.SlotSources) != 2
}

func networkUpgradeDecision(status NetworkStatus, expected string, explicit bool) (bool, error) {
	if !networkNeedsUpgrade(status, expected) {
		return false, nil
	}
	if !explicit {
		if len(status.SlotSources) != 2 {
			return false, errors.New("the Mac network helper lacks first SSH identity verification; stop both Mac slots, then run farrow mac setup to upgrade it")
		}
		return false, nil
	}
	for _, connected := range status.Connected {
		if connected {
			return false, errors.New("the Mac network helper needs an update; stop both Mac slots, then run farrow mac setup again (running guests were preserved)")
		}
	}
	return true, nil
}

func (m *Manager) ensureNetworkMode(ctx context.Context, config *Config, allowInstall, allowUpgrade bool) error {
	expected, err := m.networkBuildID(ctx)
	if err != nil {
		return err
	}
	var current *NetworkStatus
	status, statusErr := m.networkStatus(ctx, config)
	if statusErr == nil {
		if err = m.checkNetworkRoutes(ctx, config, &status); err != nil {
			return err
		}
		upgrade, err := networkUpgradeDecision(status, expected, allowUpgrade)
		if err != nil {
			return err
		}
		if !upgrade {
			if networkNeedsUpgrade(status, expected) {
				m.progress("A Mac network helper update is available; the compatible installed helper remains in use. Stop both Mac slots and run farrow mac setup when convenient.")
			}
			return nil
		}
		current = &status
	} else if !allowInstall {
		return fmt.Errorf("mac network helper is unavailable; run farrow mac network status, then farrow mac setup to restore it: %w", statusErr)
	}
	binary, err := m.networkBinary()
	if err != nil {
		return fmt.Errorf("mac network helper is missing from the bundle: %w", err)
	}
	installer, err := m.networkInstaller()
	if err != nil {
		return err
	}
	if err = m.checkNetworkRoutes(ctx, config, current); err != nil {
		return err
	}
	path, err := m.writeNetworkConfig(ctx, config)
	if err != nil {
		return err
	}
	m.progress("Installing the dedicated Mac network helper for %s; macOS may request administrator authentication", config.Network.Subnet)
	if err = m.runNetworkAdministrator(ctx, installer, "--binary", binary, "--config", path); err != nil {
		return err
	}
	status, err = m.networkStatus(ctx, config)
	if err != nil {
		return fmt.Errorf("network helper was installed but is not ready: %w", err)
	}
	if networkNeedsUpgrade(status, expected) {
		return errors.New("network helper installation returned an unexpected build; inspect farrow mac network status")
	}
	return m.checkNetworkRoutes(ctx, config, &status)
}

func (m *Manager) writeNetworkConfig(ctx context.Context, config *Config) (string, error) {
	type reservation struct {
		Name string `json:"name"`
		MAC  string `json:"mac"`
		IP   string `json:"ip"`
	}
	helperConfig := struct {
		SchemaVersion  int           `json:"schema_version"`
		UID            int           `json:"uid"`
		InstallationID string        `json:"installation_id"`
		Subnet         string        `json:"subnet"`
		Slots          []reservation `json:"slots"`
	}{SchemaVersion: 1, UID: os.Getuid(), InstallationID: config.InstallationID, Subnet: config.Network.Subnet}
	for _, pref := range config.Slots {
		helperConfig.Slots = append(helperConfig.Slots, reservation{Name: pref.Name, MAC: pref.MAC, IP: pref.IP})
	}
	data, err := json.MarshalIndent(helperConfig, "", "  ")
	if err != nil {
		return "", err
	}
	path, err := m.Store.Path("network-helper.json")
	if err != nil {
		return "", err
	}
	if err = fsutil.AtomicWrite(path, append(data, '\n'), 0600); err != nil {
		return "", err
	}
	binary, err := m.networkBinary()
	if err != nil {
		return "", err
	}
	var validated map[string]any
	if err = (Runner{Binary: binary}).Call(ctx, nil, &validated, "validate-config", "--config", path); err != nil {
		return "", err
	}
	return path, nil
}
