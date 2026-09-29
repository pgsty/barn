package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	linuxnet "github.com/pgsty/barn/internal/network/linux"
	"github.com/pgsty/barn/internal/platform"
)

func parseLinuxFamily(content string) linuxnet.Family {
	lower := strings.ToLower(content)
	if strings.Contains(lower, "debian") || strings.Contains(lower, "ubuntu") {
		return linuxnet.Debian
	}
	if strings.Contains(lower, "rhel") || strings.Contains(lower, "fedora") || strings.Contains(lower, "rocky") || strings.Contains(lower, "almalinux") || strings.Contains(lower, "centos") {
		return linuxnet.RPM
	}
	return ""
}

func unixMode(info os.FileInfo) uint32 {
	mode := uint32(info.Mode().Perm())
	if info.Mode()&os.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if info.Mode()&os.ModeSetgid != 0 {
		mode |= 0o2000
	}
	return mode
}

func helperCheck(pathname string, family linuxnet.Family) Check {
	info, err := os.Lstat(pathname)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Check{Name: "bridge-helper", Status: Error, Evidence: pathname + " is missing or not a regular file", Fix: "reinstall the distribution QEMU packages; Barn uses only the distribution's qemu-bridge-helper"}
	}
	statistics, ok := info.Sys().(*syscall.Stat_t)
	if !ok || statistics.Uid != 0 {
		return Check{Name: "bridge-helper", Status: Error, Evidence: pathname + " is not root-owned", Fix: "reinstall the distribution QEMU packages to restore its ownership"}
	}
	mode := unixMode(info)
	groupName := strconv.FormatUint(uint64(statistics.Gid), 10)
	if group, lookupErr := user.LookupGroupId(groupName); lookupErr == nil {
		groupName = group.Name
	}
	switch family {
	case linuxnet.Debian:
		if mode == 0o4750 {
			if platform.CurrentProcessInGroup(statistics.Gid) {
				return Check{Name: "bridge-helper", Status: OK, Evidence: fmt.Sprintf("%s root:%s mode=%04o and executable by the invoking user", pathname, groupName, mode)}
			}
			return Check{Name: "bridge-helper", Status: Error, Evidence: fmt.Sprintf("%s is root:%s mode=4750 but the invoking user is not in group %s", pathname, groupName, groupName), Fix: "uninstall the Barn network as its owner, then reinstall it for this user"}
		}
		primaryGroup := strconv.FormatInt(int64(os.Getgid()), 10)
		if group, lookupErr := user.LookupGroupId(primaryGroup); lookupErr == nil {
			primaryGroup = group.Name
		}
		return Check{Name: "bridge-helper", Status: Warn, Evidence: fmt.Sprintf("%s root:%s mode=%04o is not yet usable by this user", pathname, groupName, mode), Fix: fmt.Sprintf("run barn setup; it sets root:%s mode 4750 with dpkg-statoverride and uninstall restores it", primaryGroup)}
	case linuxnet.RPM:
		if mode == 0o4755 {
			return Check{Name: "bridge-helper", Status: Warn, Evidence: fmt.Sprintf("%s root:%s mode=4755 permits every local user to request an allowed bridge attach", pathname, groupName)}
		}
		return Check{Name: "bridge-helper", Status: Error, Evidence: fmt.Sprintf("%s has mode %04o; RPM-family hosts need the packaged mode 4755", pathname, mode), Fix: "reinstall the distribution QEMU packages; Barn does not change helper permissions on RPM-family hosts"}
	default:
		return Check{Name: "bridge-helper", Status: Error, Evidence: "this Linux distribution is outside the Debian/Ubuntu and RHEL/Fedora families", Fix: "use a Debian/Ubuntu or RHEL/Fedora-family host"}
	}
}

func protectedLinuxStateDir() bool {
	info, err := os.Lstat("/var/lib/barn")
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return false
	}
	statistics, ok := info.Sys().(*syscall.Stat_t)
	return ok && statistics.Uid == 0
}

func (p Probe) linuxPrivateChecks(ctx context.Context) []Check {
	checks := make([]Check, 0, 5)
	osRelease, err := os.ReadFile("/etc/os-release")
	if err != nil || len(osRelease) > 64<<10 {
		checks = append(checks, Check{Name: "linux-family", Status: Error, Evidence: "cannot read /etc/os-release", Fix: "make /etc/os-release a readable file under 64 KiB"})
		return checks
	}
	family := parseLinuxFamily(string(osRelease))
	if family == "" {
		checks = append(checks, Check{Name: "linux-family", Status: Error, Evidence: "this Linux distribution is outside the Debian/Ubuntu and RHEL/Fedora families", Fix: "use a Debian/Ubuntu or RHEL/Fedora-family host"})
	} else {
		checks = append(checks, Check{Name: "linux-family", Status: OK, Evidence: string(family)})
	}

	systemctl, systemctlErr := p.lookPath("systemctl")
	if systemctlErr != nil {
		checks = append(checks, Check{Name: "linux-network-owner", Status: Error, Evidence: "systemctl not found; the Barn network on Linux needs systemd", Fix: "use a systemd-based host"})
	} else {
		serviceActive := func(name string) bool {
			result, runErr := p.runner().Run(ctx, systemctl, "is-active", name)
			return runErr == nil && strings.TrimSpace(string(result.Stdout)) == "active"
		}
		switch {
		case serviceActive("NetworkManager.service"):
			checks = append(checks, Check{Name: "linux-network-owner", Status: OK, Evidence: "NetworkManager owns the host network (nmcli backend)"})
		case serviceActive("systemd-networkd.service"):
			checks = append(checks, Check{Name: "linux-network-owner", Status: OK, Evidence: "systemd-networkd owns the host network"})
		default:
			checks = append(checks, Check{Name: "linux-network-owner", Status: Error, Evidence: "neither NetworkManager nor systemd-networkd is active", Fix: "activate the distribution network manager; barn follows the active owner"})
		}
	}

	helperPath := ""
	for _, candidate := range []string{"/usr/lib/qemu/qemu-bridge-helper", "/usr/libexec/qemu-bridge-helper"} {
		if _, err := os.Lstat(candidate); err == nil {
			helperPath = candidate
			break
		}
	}
	if helperPath == "" {
		checks = append(checks, Check{Name: "bridge-helper", Status: Error, Evidence: "qemu-bridge-helper was not found in /usr/lib/qemu or /usr/libexec", Fix: "run barn setup, or reinstall the distribution QEMU packages"})
	} else {
		checks = append(checks, helperCheck(helperPath, family))
	}

	ipBinary, ipErr := p.lookPath("ip")
	stateExists := false
	stateProtected := false
	if stateInfo, stateErr := os.Lstat(linuxnet.StatePath); stateErr == nil && stateInfo.Mode().IsRegular() {
		stateExists = true
	} else if errors.Is(stateErr, os.ErrPermission) && protectedLinuxStateDir() {
		stateExists = true
		stateProtected = true
	}
	if ipErr != nil {
		checks = append(checks, Check{Name: "private-bridge", Status: Error, Evidence: "the ip command was not found", Fix: "install iproute2"})
	} else {
		bridgeResult, bridgeErr := p.runner().Run(ctx, ipBinary, "-json", "link", "show", "dev", linuxnet.BridgeName)
		bridgeExists := bridgeErr == nil && len(strings.TrimSpace(string(bridgeResult.Stdout))) > 2
		if bridgeExists && !stateExists {
			checks = append(checks, Check{Name: "private-bridge", Status: Error, Evidence: "a barn0 bridge exists that Barn did not create", Fix: "Barn will not take it over; if nothing uses it, remove it (sudo ip link delete barn0), then run barn setup"})
		} else if bridgeExists && stateProtected {
			checks = append(checks, Check{Name: "private-bridge", Status: Warn, Evidence: "barn0 exists; its root-only ownership record is verified by barn setup and barn network commands"})
		} else if bridgeExists {
			checks = append(checks, Check{Name: "private-bridge", Status: OK, Evidence: "barn0 and its ownership record exist"})
		} else {
			checks = append(checks, Check{Name: "private-bridge", Status: OK, Evidence: "barn0 is absent"})
		}
	}
	return checks
}
