package macvm

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/lock"
)

type NetworkInfo struct {
	Root            string         `json:"root"`
	Configured      bool           `json:"configured"`
	Installed       bool           `json:"installed"`
	Ready           bool           `json:"ready"`
	InstallationID  string         `json:"installation_id,omitempty"`
	Subnet          string         `json:"subnet,omitempty"`
	Socket          string         `json:"socket,omitempty"`
	HelperPath      string         `json:"helper_path,omitempty"`
	ConfigPath      string         `json:"config_path,omitempty"`
	ServiceLabel    string         `json:"service_label,omitempty"`
	LogPath         string         `json:"log_path,omitempty"`
	ExpectedBuildID string         `json:"expected_build_id,omitempty"`
	UpdateRequired  bool           `json:"update_required"`
	Status          *NetworkStatus `json:"status,omitempty"`
	Diagnostic      string         `json:"diagnostic,omitempty"`
}

type NetworkUninstallResult struct {
	Root                 string `json:"root"`
	InstallationID       string `json:"installation_id"`
	Subnet               string `json:"subnet"`
	Uninstalled          bool   `json:"uninstalled"`
	AlreadyAbsent        bool   `json:"already_absent"`
	SharedBinaryRetained bool   `json:"shared_binary_retained"`
	LogRetained          bool   `json:"log_retained"`
	DataPreserved        bool   `json:"data_preserved"`
}

type networkPaths struct{ directory, config, plist, socket, label, log string }

func (m *Manager) networkPaths(config *Config) (networkPaths, error) {
	socket, err := m.networkSocket(config)
	if err != nil {
		return networkPaths{}, err
	}
	id := strings.ToLower(config.InstallationID)
	label := fmt.Sprintf("io.pgsty.farrow.mac-network.%d.%s", os.Getuid(), id)
	directory := filepath.Join("/Library/Application Support/Farrow/mac-network", strconv.Itoa(os.Getuid()), id)
	return networkPaths{directory: directory, config: filepath.Join(directory, "config.json"),
		plist: filepath.Join("/Library/LaunchDaemons", label+".plist"), socket: socket, label: label,
		log: fmt.Sprintf("/var/log/farrow-mac/%d-%s.log", os.Getuid(), id)}, nil
}

// A failed RPC is never proof of absence. Retained log/lock files are not an
// installation; configuration, sockets or a registered service still are.
func networkInstallationPresent(paths networkPaths, stat func(string) (os.FileInfo, error), service func(string) (bool, error)) (bool, error) {
	for _, path := range []string{paths.directory, paths.config, paths.plist, paths.socket} {
		if _, err := stat(path); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("cannot determine whether the Mac network is installed (%s): %w", path, err)
		}
	}
	return service(paths.label)
}

func (m *Manager) networkInstallationExists(ctx context.Context, config *Config) (bool, error) {
	paths, err := m.networkPaths(config)
	if err != nil {
		return false, err
	}
	return networkInstallationPresent(paths, os.Lstat, func(label string) (bool, error) {
		output, err := exec.CommandContext(ctx, "/bin/launchctl", "print", "system/"+label).CombinedOutput()
		if err == nil {
			return true, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 113 {
			return false, nil
		}
		return false, fmt.Errorf("cannot inspect Mac network service %s: %w: %s", label, err, strings.TrimSpace(string(output)))
	})
}

func (m *Manager) networkBuildID(ctx context.Context) (string, error) {
	binary, err := m.networkBinary()
	if err != nil {
		return "", fmt.Errorf("mac network helper is missing from this bundle: %w", err)
	}
	var version struct {
		Name     string `json:"name"`
		BuildID  string `json:"build_id"`
		Protocol int    `json:"protocol"`
	}
	if err = (Runner{Binary: binary}).Call(ctx, nil, &version, "--version"); err != nil {
		return "", err
	}
	decoded, decodeErr := hex.DecodeString(version.BuildID)
	if version.Name != "farrow-mac-network" || version.Protocol != 1 || decodeErr != nil || len(decoded) != 32 {
		return "", errors.New("mac network helper bundle has no valid build identity; reinstall the complete Mac bundle")
	}
	return version.BuildID, nil
}

func (m *Manager) networkInstaller() (string, error) {
	binary, err := m.networkBinary()
	if err != nil {
		return "", err
	}
	for _, path := range []string{filepath.Join(filepath.Dir(binary), "farrow-mac-network-install"), filepath.Join(filepath.Dir(binary), "..", "Resources", "farrow-mac-network-install")} {
		if found, err := executableFile(path); err == nil {
			return found, nil
		}
	}
	return "", errors.New("mac network installer is missing; reinstall the complete Mac bundle")
}

func (m *Manager) runNetworkAdministrator(ctx context.Context, installer string, args ...string) error {
	command := append([]string{"/bin/bash", installer}, args...)
	var cmd *exec.Cmd
	if exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "true").Run() == nil {
		cmd = exec.CommandContext(ctx, "/usr/bin/sudo", append([]string{"-n"}, command...)...)
	} else {
		cmd = exec.CommandContext(ctx, "/usr/bin/osascript", "-")
		cmd.Stdin = strings.NewReader("do shell script " + strconv.Quote(QuoteCommand(command)) + " with administrator privileges\n")
	}
	var diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = &diagnostics, &diagnostics
	if err := cmd.Run(); err != nil {
		return networkAdministratorError(ctx, err, diagnostics.String())
	}
	return nil
}

func networkAdministratorError(ctx context.Context, err error, diagnostics string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	detail := strings.TrimSpace(diagnostics)
	if strings.HasSuffix(detail, "(-128)") {
		return failure.New(failure.Cancelled, errors.New("mac administrator authentication was cancelled; existing VM data was preserved")).Because("mac_authorization_cancelled").Then("Retry the command when ready to authenticate")
	}
	return fmt.Errorf("mac network maintenance did not complete: %w: %s", err, detail)
}

// NetworkInfo reads only the selected data root and its own installation. It
// reports an unavailable helper as a diagnostic, without requesting authority.
func (m *Manager) NetworkInfo(ctx context.Context) (NetworkInfo, error) {
	info := NetworkInfo{Root: m.Store.Root}
	config, err := m.Store.LoadConfig()
	if errors.Is(err, os.ErrNotExist) {
		info.Diagnostic = "mac network is not configured; run farrow mac setup"
		return info, nil
	}
	if err != nil {
		return info, err
	}
	paths, err := m.networkPaths(config)
	if err != nil {
		return info, err
	}
	info.Configured, info.InstallationID, info.Subnet = true, config.InstallationID, config.Network.Subnet
	info.Socket, info.ConfigPath, info.ServiceLabel, info.LogPath = paths.socket, paths.config, paths.label, paths.log
	info.HelperPath = "/Library/PrivilegedHelperTools/io.pgsty.farrow.mac-network"
	info.Installed, err = m.networkInstallationExists(ctx, config)
	if err != nil {
		info.Diagnostic = err.Error()
		return info, nil
	}
	if err = m.requireRunner(); err != nil {
		info.Diagnostic = err.Error()
		return info, nil
	}
	info.ExpectedBuildID, err = m.networkBuildID(ctx)
	if err != nil {
		info.Diagnostic = err.Error()
		return info, nil
	}
	status, err := m.networkStatus(ctx, config)
	if err != nil {
		info.Diagnostic = fmt.Sprintf("%v; run farrow mac setup to install or repair this network; log: %s", err, paths.log)
		return info, nil
	}
	info.Status, info.Ready = &status, true
	info.UpdateRequired = networkNeedsUpgrade(status, info.ExpectedBuildID)
	if err = m.checkNetworkRoutes(ctx, config, &status); err != nil {
		info.Ready, info.Diagnostic = false, err.Error()
		return info, nil
	}
	if info.UpdateRequired {
		info.Diagnostic = "A helper update is available; stop both Mac slots and run farrow mac setup"
	}
	return info, nil
}

type networkRemovalOps struct {
	present func(context.Context, *Config) (bool, error)
	runtime func(context.Context, *Slot) (RuntimeStatus, error)
	status  func(context.Context, *Config) (NetworkStatus, error)
	remove  func(context.Context, *Config) error
}

func (m *Manager) UninstallNetwork(ctx context.Context) (NetworkUninstallResult, error) {
	if err := m.requireRunner(); err != nil {
		return NetworkUninstallResult{Root: m.Store.Root}, err
	}
	return m.uninstallNetwork(ctx, networkRemovalOps{
		present: m.networkInstallationExists, runtime: m.status, status: m.networkStatus,
		remove: func(ctx context.Context, config *Config) error {
			binary, err := m.networkBinary()
			if err != nil {
				return err
			}
			installer, err := m.networkInstaller()
			if err != nil {
				return err
			}
			path, err := m.writeNetworkConfig(ctx, config)
			if err != nil {
				return err
			}
			m.progress("Removing only the Mac network for %s (%s); guest data, other environments and the shared helper executable are preserved. macOS may request administrator authentication.", m.Store.Root, config.InstallationID)
			return m.runNetworkAdministrator(ctx, installer, "--uninstall", "--binary", binary, "--config", path)
		},
	})
}

func (m *Manager) uninstallNetwork(ctx context.Context, ops networkRemovalOps) (result NetworkUninstallResult, retErr error) {
	result = NetworkUninstallResult{Root: m.Store.Root, SharedBinaryRetained: true, LogRetained: true, DataPreserved: true}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, err := m.Store.LoadConfig(); err != nil {
		return result, fmt.Errorf("cannot identify a saved Mac network to uninstall: %w", err)
	}
	// Match lifecycle lock order: both slot operations, then shared state. All
	// locks are nonblocking so a long setup/restore yields an actionable refusal.
	var held []*lock.File
	defer func() {
		for i := len(held) - 1; i >= 0; i-- {
			retErr = lock.JoinRelease(retErr, held[i], "mac network removal")
		}
	}()
	for _, name := range []string{"mac1.operation.lock", "mac2.operation.lock", "state.lock"} {
		path, err := m.Store.Path("runtime", name)
		if err != nil {
			return result, err
		}
		proof, err := lock.TryAcquire(path, false)
		if err != nil {
			return result, fmt.Errorf("mac operation is in progress or its lock is unavailable; finish it before network uninstall: %w", err)
		}
		held = append(held, proof)
	}
	config, err := m.Store.LoadConfig()
	if err != nil {
		return result, err
	}
	result.InstallationID, result.Subnet = config.InstallationID, config.Network.Subnet
	slots, err := m.Store.ListSlots()
	if err != nil {
		return result, err
	}
	for _, slot := range slots {
		if slot.InstanceID == "" {
			continue
		}
		if runtime, err := ops.runtime(ctx, &slot); err == nil && runtime.State != "stopped" {
			return result, fmt.Errorf("%s is %s; stop both Mac slots before network uninstall", slot.Name, runtime.State)
		}
		path, err := m.Store.Path("slots", slot.Name, "runner.lock")
		if err != nil {
			return result, err
		}
		proof, err := lock.TryAcquire(path, false)
		if err != nil {
			return result, fmt.Errorf("%s may still be running; network uninstall refused: %w", slot.Name, err)
		}
		held = append(held, proof)
	}
	present, err := ops.present(ctx, config)
	if err != nil {
		return result, err
	}
	if !present {
		result.AlreadyAbsent = true
		return result, nil
	}
	if status, err := ops.status(ctx, config); err == nil {
		for _, connected := range status.Connected {
			if connected {
				return result, errors.New("mac network still has an attached client; stop both Mac slots before network uninstall")
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// The privileged installer additionally authenticates the root-owned config
	// and acquires an atomic no-connections lease, or an exited-daemon lock.
	if err = ops.remove(ctx, config); err != nil {
		return result, err
	}
	present, err = ops.present(ctx, config)
	if err != nil {
		return result, err
	}
	if present {
		return result, errors.New("network uninstall did not remove this installation; inspect farrow mac network status")
	}
	result.Uninstalled = true
	return result, nil
}
