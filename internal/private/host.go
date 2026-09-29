package private

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pgsty/barn/internal/execx"
	darwinnet "github.com/pgsty/barn/internal/network/darwin"
	linuxnet "github.com/pgsty/barn/internal/network/linux"
	"github.com/pgsty/barn/internal/network/subnet"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/spec"
)

type CapabilityError struct {
	Reason string
	Cause  error
}

func (e *CapabilityError) Error() string { return e.Reason }
func (e *CapabilityError) Unwrap() error { return e.Cause }

func rootOwned(path string, mode os.FileMode, kind string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	statistics, ok := info.Sys().(*syscall.Stat_t)
	if !ok || statistics.Uid != 0 || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode {
		return nil, fmt.Errorf("%s is not a root-owned non-symlink mode-%04o %s", path, mode, kind)
	}
	switch kind {
	case "file":
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", path)
		}
	case "directory":
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", path)
		}
	case "socket":
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s is not a Unix socket", path)
		}
	default:
		return nil, fmt.Errorf("unsupported ownership check kind %q", kind)
	}
	return info, nil
}

func expectedPrivateLayout(expected *spec.PrivateNetwork) (subnet.Layout, error) {
	if expected == nil {
		return subnet.Layout{}, errors.New("host preflight requires an expected network")
	}
	layout, err := subnet.Parse(expected.CIDR)
	if err != nil {
		return subnet.Layout{}, err
	}
	if expected.HostAddress != layout.HostAddress() || expected.DHCPEnd != layout.DHCPEnd() {
		return subnet.Layout{}, errors.New("expected private network does not match its /24 layout")
	}
	return layout, nil
}

func qemuVersionPreflight(ctx context.Context, profile platform.Profile, runner execx.Runner) (string, platform.Version, error) {
	if profile.QEMUBinary == "" {
		return "", platform.Version{}, errors.New("host profile has no QEMU binary")
	}
	qemuPath, err := platform.FindQEMUBinary(profile, exec.LookPath)
	if err != nil {
		return "", platform.Version{}, err
	}
	result, err := runner.Run(ctx, qemuPath, "--version")
	if err != nil {
		return "", platform.Version{}, fmt.Errorf("probe QEMU version: %w", err)
	}
	version, err := platform.ValidateQEMUVersion(profile, string(result.Stdout)+string(result.Stderr))
	if err != nil {
		return "", platform.Version{}, err
	}
	return qemuPath, version, nil
}

func darwinHostPreflight(ctx context.Context, profile platform.Profile, expected *spec.PrivateNetwork, runner execx.Runner, qemuPath string, qemuVersion platform.Version) (Backend, error) {
	layout, err := expectedPrivateLayout(expected)
	if err != nil {
		return Backend{}, err
	}
	if _, err := rootOwned(darwinnet.DaemonPath, 0o755, "file"); err != nil {
		return Backend{}, err
	}
	if _, err := rootOwned(darwinnet.PlistPath, 0o644, "file"); err != nil {
		return Backend{}, err
	}
	if _, err := rootOwned(darwinnet.StateDir, 0o700, "directory"); err != nil {
		return Backend{}, err
	}
	if _, err := rootOwned(darwinnet.SocketPath, 0o770, "socket"); err != nil {
		return Backend{}, err
	}
	result, err := runner.Run(ctx, "/usr/bin/plutil", "-extract", "ProgramArguments", "json", "-o", "-", darwinnet.PlistPath)
	if err != nil {
		return Backend{}, fmt.Errorf("read socket_vmnet launch arguments: %w", err)
	}
	var args []string
	if err := json.Unmarshal(result.Stdout, &args); err != nil {
		return Backend{}, fmt.Errorf("decode socket_vmnet launch arguments: %w", err)
	}
	interfaceID := ""
	mode := ""
	for _, arg := range args {
		if strings.HasPrefix(arg, "--vmnet-interface-id=") {
			if interfaceID != "" {
				return Backend{}, errors.New("socket_vmnet launch arguments repeat interface ID")
			}
			interfaceID = strings.TrimPrefix(arg, "--vmnet-interface-id=")
		}
		if strings.HasPrefix(arg, "--vmnet-mode=") {
			if mode != "" {
				return Backend{}, errors.New("socket_vmnet launch arguments repeat mode")
			}
			mode = strings.TrimPrefix(arg, "--vmnet-mode=")
		}
	}
	plan, err := darwinnet.NewInstallPlanModeNetwork(profile.Arch, interfaceID, mode, layout.CIDR())
	if err != nil || !reflect.DeepEqual(args, plan.Args) {
		return Backend{}, errors.New("socket_vmnet launch arguments do not match the pinned plan")
	}
	connection, err := net.DialTimeout("unix", darwinnet.SocketPath, time.Second)
	if err != nil {
		return Backend{}, fmt.Errorf("connect socket_vmnet stream: %w", err)
	}
	_ = connection.Close()
	ifconfig, err := runner.Run(ctx, "/sbin/ifconfig")
	if err != nil || !strings.Contains(string(ifconfig.Stdout), plan.State.HostAddress) {
		return Backend{}, fmt.Errorf("socket_vmnet host address %s is absent; inspect for a conflicting vmnet sharing service or subnet", plan.State.HostAddress)
	}
	netdevResult, err := runner.Run(ctx, qemuPath, "-machine", "none", "-netdev", "help")
	if err != nil {
		return Backend{}, err
	}
	backend := selectDarwinBackend(qemuVersion, string(netdevResult.Stdout)+string(netdevResult.Stderr), darwinnet.SocketPath)
	backend.NetworkCIDR, backend.HostAddress, backend.DHCPEnd = layout.CIDR(), layout.HostAddress(), layout.DHCPEnd()
	return backend, nil
}

func selectDarwinBackend(version platform.Version, netdevHelp, socket string) Backend {
	streamReconnectMS := version.AtLeast(platform.Version{Major: 10, Minor: 2}) && strings.Contains(netdevHelp, "stream")
	if !streamReconnectMS {
		return Backend{DarwinSocket: socket, DarwinUseFD: true}
	}
	return Backend{DarwinSocket: socket, ReconnectMS: 1000}
}

func linuxMode(info os.FileInfo) uint32 {
	mode := uint32(info.Mode().Perm())
	if info.Mode()&os.ModeSetuid != 0 {
		mode |= 0o4000
	}
	return mode
}

func linuxHostFamily(data []byte) (linuxnet.Family, error) {
	lower := strings.ToLower(string(data))
	if strings.Contains(lower, "ubuntu") || strings.Contains(lower, "debian") {
		return linuxnet.Debian, nil
	}
	if strings.Contains(lower, "rhel") || strings.Contains(lower, "fedora") || strings.Contains(lower, "rocky") || strings.Contains(lower, "almalinux") || strings.Contains(lower, "centos") {
		return linuxnet.RPM, nil
	}
	return "", errors.New("unsupported Linux distribution family")
}

func validateLinuxHelperPolicy(family linuxnet.Family, mode uint32, group string) error {
	switch family {
	case linuxnet.Debian:
		if mode != 0o4750 || group == "" {
			return errors.New("debian qemu-bridge-helper must be setuid root and group-executable (mode 4750)")
		}
	case linuxnet.RPM:
		if mode != 0o4755 || group != "root" {
			return errors.New("rpm qemu-bridge-helper must retain distribution-owned root:root mode-4755")
		}
	default:
		return errors.New("unsupported Linux qemu-bridge-helper family")
	}
	return nil
}

func validateRootOwnedHelperParents(path string) error {
	for parent := filepath.Dir(filepath.Clean(path)); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("qemu-bridge-helper parent is unsafe: %s", parent)
		}
		statistics, ok := info.Sys().(*syscall.Stat_t)
		if !ok || statistics.Uid != 0 {
			return fmt.Errorf("qemu-bridge-helper parent is not root-owned: %s", parent)
		}
		if parent == "/" {
			return nil
		}
	}
}

func requireExactRootFile(path, content string, mode os.FileMode) error {
	if _, err := rootOwned(path, mode, "file"); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != content {
		return fmt.Errorf("owned Linux network file content mismatch: %s", path)
	}
	return nil
}

func linuxHostPreflight(ctx context.Context, expected *spec.PrivateNetwork, runner execx.Runner) (Backend, error) {
	layout, err := expectedPrivateLayout(expected)
	if err != nil {
		return Backend{}, err
	}
	helper := ""
	for _, candidate := range []string{"/usr/lib/qemu/qemu-bridge-helper", "/usr/libexec/qemu-bridge-helper"} {
		if _, err := os.Lstat(candidate); err == nil {
			helper = candidate
			break
		}
	}
	if helper == "" {
		return Backend{}, errors.New("supported qemu-bridge-helper is absent")
	}
	helperInfo, err := os.Lstat(helper)
	if err != nil || !helperInfo.Mode().IsRegular() || helperInfo.Mode()&os.ModeSymlink != 0 {
		return Backend{}, errors.New("qemu-bridge-helper is not a regular non-symlink file")
	}
	statistics, ok := helperInfo.Sys().(*syscall.Stat_t)
	if !ok || statistics.Uid != 0 {
		return Backend{}, errors.New("qemu-bridge-helper is not root-owned")
	}
	if err := validateRootOwnedHelperParents(helper); err != nil {
		return Backend{}, err
	}
	group, err := user.LookupGroupId(strconv.FormatUint(uint64(statistics.Gid), 10))
	if err != nil {
		return Backend{}, errors.New("qemu-bridge-helper group identity is unavailable")
	}
	osRelease, err := os.ReadFile("/etc/os-release")
	if err != nil || len(osRelease) > 64<<10 {
		return Backend{}, errors.New("cannot read bounded /etc/os-release")
	}
	family, err := linuxHostFamily(osRelease)
	if err != nil {
		return Backend{}, err
	}
	if err := validateLinuxHelperPolicy(family, linuxMode(helperInfo), group.Name); err != nil {
		return Backend{}, err
	}
	if family == linuxnet.Debian {
		if !platform.CurrentProcessInGroup(statistics.Gid) {
			return Backend{}, fmt.Errorf("qemu-bridge-helper group %s does not include the invoking user", group.Name)
		}
		packageOwner, packageErr := runner.Run(ctx, "/usr/bin/dpkg-query", "-S", helper)
		if packageErr != nil || !strings.Contains(string(packageOwner.Stdout), ": "+helper) {
			return Backend{}, errors.New("qemu-bridge-helper is not owned by a Debian package")
		}
		override, overrideErr := runner.Run(ctx, "/usr/bin/dpkg-statoverride", "--list", helper)
		if overrideErr != nil || strings.TrimSpace(string(override.Stdout)) != "root "+group.Name+" 4750 "+helper {
			return Backend{}, errors.New("qemu-bridge-helper dpkg override does not match Barn policy")
		}
	} else {
		packageOwner, packageErr := runner.Run(ctx, "/usr/bin/rpm", "-qf", helper)
		if packageErr != nil || strings.TrimSpace(string(packageOwner.Stdout)) == "" {
			return Backend{}, errors.New("qemu-bridge-helper is not owned by an RPM package")
		}
	}
	backend := linuxnet.BackendNetworkd
	if publicInfo, publicErr := os.Lstat(linuxnet.PublicStatePath); publicErr == nil {
		backend = linuxnet.BackendNetworkManager
		if !publicInfo.Mode().IsRegular() || publicInfo.Mode()&os.ModeSymlink != 0 {
			return Backend{}, errors.New("NetworkManager public state is not a regular non-symlink file")
		}
		if _, err := rootOwned(linuxnet.PublicStateDir, 0o755, "directory"); err != nil {
			return Backend{}, err
		}
		if _, err := rootOwned(linuxnet.PublicStatePath, 0o644, "file"); err != nil {
			return Backend{}, err
		}
		publicData, err := os.ReadFile(linuxnet.PublicStatePath)
		if err != nil {
			return Backend{}, err
		}
		publicState, err := linuxnet.ParsePublicState(publicData)
		if err != nil || publicState.CIDR != layout.CIDR() || publicState.HostAddress != layout.HostAddress() || publicState.DHCPEnd != layout.DHCPEnd() {
			return Backend{}, errors.New("NetworkManager public state does not match the requested network")
		}
		for _, stale := range []string{linuxnet.NetDevPath, linuxnet.NetworkPath, linuxnet.NetworkManagerPath} {
			if _, err := os.Lstat(stale); err == nil {
				return Backend{}, fmt.Errorf("NetworkManager backend has stale networkd path: %s", stale)
			} else if !errors.Is(err, os.ErrNotExist) {
				return Backend{}, err
			}
		}
	} else if !errors.Is(publicErr, os.ErrNotExist) {
		return Backend{}, publicErr
	}
	if backend == linuxnet.BackendNetworkd {
		if err := requireExactRootFile(linuxnet.NetDevPath, "[NetDev]\nName=barn0\nKind=bridge\n", 0o644); err != nil {
			return Backend{}, err
		}
		wantNetwork := fmt.Sprintf("[Match]\nName=barn0\n\n[Network]\nAddress=%s/24\nConfigureWithoutCarrier=yes\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n\n[Link]\nRequiredForOnline=no\n", layout.HostAddress())
		if err := requireExactRootFile(linuxnet.NetworkPath, wantNetwork, 0o644); err != nil {
			return Backend{}, err
		}
	}
	if _, err := rootOwned(linuxnet.BridgeConfPath, 0o644, "file"); err != nil {
		return Backend{}, err
	}
	bridgeConf, err := os.ReadFile(linuxnet.BridgeConfPath)
	if err != nil || !strings.Contains(string(bridgeConf), "# BEGIN BARN MANAGED: barn0\nallow barn0\n# END BARN MANAGED: barn0\n") {
		return Backend{}, errors.New("qemu bridge.conf lacks the exact Barn marker block")
	}
	if _, err := rootOwned("/var/lib/barn", 0o700, "directory"); err != nil {
		return Backend{}, err
	}
	service := "systemd-networkd.service"
	if backend == linuxnet.BackendNetworkManager {
		service = "NetworkManager.service"
	}
	active, err := runner.Run(ctx, "/usr/bin/systemctl", "is-active", service)
	if err != nil || strings.TrimSpace(string(active.Stdout)) != "active" {
		return Backend{}, fmt.Errorf("%s is not active", service)
	}
	if backend == linuxnet.BackendNetworkManager {
		connections, err := runner.Run(ctx, linuxnet.NMCLIPath, "-t", "-f", "NAME", "connection", "show")
		if err != nil {
			return Backend{}, err
		}
		found := false
		for _, name := range strings.Split(strings.TrimSpace(string(connections.Stdout)), "\n") {
			found = found || name == linuxnet.BridgeName
		}
		if !found {
			return Backend{}, errors.New("NetworkManager has no barn0 connection")
		}
	}
	link, err := runner.Run(ctx, "/usr/sbin/ip", "-d", "link", "show", "dev", linuxnet.BridgeName)
	if err != nil || !strings.Contains(string(link.Stdout), "bridge") {
		return Backend{}, errors.New("barn0 is absent or not a bridge")
	}
	address, err := runner.Run(ctx, "/usr/sbin/ip", "-4", "-o", "address", "show", "dev", linuxnet.BridgeName)
	if err != nil || !strings.Contains(string(address.Stdout), layout.HostAddress()+"/24") {
		return Backend{}, fmt.Errorf("barn0 does not own %s/24", layout.HostAddress())
	}
	return Backend{LinuxBridgeHelper: helper, NetworkCIDR: layout.CIDR(), HostAddress: layout.HostAddress(), DHCPEnd: layout.DHCPEnd()}, nil
}

func PreflightHost(ctx context.Context, profile platform.Profile, expected *spec.PrivateNetwork, runner execx.Runner) (Backend, error) {
	if runner == nil {
		return Backend{}, &CapabilityError{Reason: "host preflight requires a command runner"}
	}
	if profile.OS != "darwin" && profile.OS != "linux" {
		return Backend{}, &CapabilityError{Reason: fmt.Sprintf("host preflight does not support %s", profile.OS)}
	}
	qemuPath, qemuVersion, err := qemuVersionPreflight(ctx, profile, runner)
	if err != nil {
		return Backend{}, &CapabilityError{Reason: err.Error(), Cause: err}
	}
	var backend Backend
	switch profile.OS {
	case "darwin":
		backend, err = darwinHostPreflight(ctx, profile, expected, runner, qemuPath, qemuVersion)
	case "linux":
		backend, err = linuxHostPreflight(ctx, expected, runner)
	}
	if err != nil {
		return Backend{}, &CapabilityError{Reason: err.Error()}
	}
	return backend, nil
}
