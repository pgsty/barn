package preflight

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/pgsty/barn/internal/fsutil"
	linuxnet "github.com/pgsty/barn/internal/network/linux"
	"github.com/pgsty/barn/internal/network/subnet"
	"github.com/pgsty/barn/internal/platform"
)

func parseLinuxInterfaces(output string) []InterfaceAddress {
	result := make([]InterfaceAddress, 0)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[2] != "inet" {
			continue
		}
		prefix, err := netip.ParsePrefix(fields[3])
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		result = append(result, InterfaceAddress{Address: prefix.Addr(), Prefix: prefix, Interface: strings.TrimSuffix(fields[1], ":"), Evidence: strings.TrimSpace(line)})
	}
	return result
}

func parseLinuxRoutes(output string) []Route {
	result := make([]Route, 0)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "default" {
			continue
		}
		prefixField := fields[0]
		kind := "unicast"
		switch fields[0] {
		case "local", "unreachable", "blackhole", "prohibit", "throw", "nat", "anycast", "multicast", "broadcast":
			if len(fields) < 2 {
				continue
			}
			kind = fields[0]
			prefixField = fields[1]
		}
		if kind == "broadcast" || kind == "multicast" || kind == "anycast" {
			continue
		}
		if !strings.Contains(prefixField, "/") {
			prefixField += "/32"
		}
		prefix, err := netip.ParsePrefix(prefixField)
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		device := ""
		for index := range fields {
			if fields[index] == "dev" && index+1 < len(fields) {
				device = fields[index+1]
			}
		}
		result = append(result, Route{Prefix: prefix.Masked(), Interface: device, Kind: kind, Evidence: strings.TrimSpace(line)})
	}
	return result
}

func linuxNetworkLayout(data []byte) (subnet.Layout, error) {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Address=") {
			continue
		}
		prefix, err := netip.ParsePrefix(strings.TrimPrefix(line, "Address="))
		if err != nil || prefix.Bits() != 24 {
			break
		}
		return subnet.FromHostAddress(prefix.Addr().String())
	}
	return subnet.Layout{}, errors.New("linux Barn network unit lacks an exact host .1/24 address")
}

func linuxFamily(data []byte) (linuxnet.Family, error) {
	lower := strings.ToLower(string(data))
	if strings.Contains(lower, "ubuntu") || strings.Contains(lower, "debian") {
		return linuxnet.Debian, nil
	}
	if strings.Contains(lower, "rhel") || strings.Contains(lower, "fedora") || strings.Contains(lower, "rocky") || strings.Contains(lower, "almalinux") || strings.Contains(lower, "centos") {
		return linuxnet.RPM, nil
	}
	return "", errors.New("unsupported Linux distribution family")
}

func fullLinuxMode(info os.FileInfo) uint32 {
	mode := uint32(info.Mode().Perm())
	if info.Mode()&os.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if info.Mode()&os.ModeSetgid != 0 {
		mode |= 0o2000
	}
	return mode
}

func validateLinuxHelper(info os.FileInfo, family linuxnet.Family) error {
	statistics, ok := info.Sys().(*syscall.Stat_t)
	if !ok || statistics.Uid != 0 || !info.Mode().IsRegular() {
		return errors.New("helper is not a root-owned regular non-symlink file")
	}
	mode := fullLinuxMode(info)
	switch family {
	case linuxnet.Debian:
		if mode != 0o4750 {
			return errors.New("debian helper must be setuid root and group-executable (mode 4750)")
		}
		if !platform.CurrentProcessInGroup(statistics.Gid) {
			group, _ := user.LookupGroupId(strconv.FormatUint(uint64(statistics.Gid), 10))
			name := strconv.FormatUint(uint64(statistics.Gid), 10)
			if group != nil {
				name = group.Name
			}
			return fmt.Errorf("debian helper group %s does not include the invoking user", name)
		}
	case linuxnet.RPM:
		group, err := user.LookupGroupId(strconv.FormatUint(uint64(statistics.Gid), 10))
		if err != nil || group.Name != "root" || mode != 0o4755 {
			return errors.New("rpm helper must retain distribution-owned root:root mode-4755")
		}
	default:
		return errors.New("unsupported Linux helper family")
	}
	return nil
}

func (p Probe) safeRootOwnedParents(path string) error {
	for parent := filepath.Dir(filepath.Clean(path)); ; parent = filepath.Dir(parent) {
		info, err := p.lstat(parent)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("unsafe helper parent %s", parent)
		}
		statistics, ok := info.Sys().(*syscall.Stat_t)
		if !ok || statistics.Uid != 0 {
			return fmt.Errorf("helper parent is not root-owned: %s", parent)
		}
		if parent == "/" {
			return nil
		}
	}
}

func exactLinuxNetwork(layout subnet.Layout) string {
	return fmt.Sprintf("[Match]\nName=barn0\n\n[Network]\nAddress=%s/24\nConfigureWithoutCarrier=yes\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n\n[Link]\nRequiredForOnline=no\n", layout.HostAddress())
}

const linuxBridgeMarker = "# BEGIN BARN MANAGED: barn0\nallow barn0\n# END BARN MANAGED: barn0\n"

type linuxPublicTarget struct {
	path   string
	mode   os.FileMode
	kind   string
	sticky bool
}

func linuxRequiredTargets(networkManager bool, stateDir string) []linuxPublicTarget {
	if networkManager {
		return []linuxPublicTarget{
			{linuxnet.PublicStatePath, 0o644, "file", false},
			{linuxnet.BridgeConfPath, 0o644, "file", false},
			{stateDir, 0o700, "directory", false},
		}
	}
	return []linuxPublicTarget{
		{linuxnet.NetDevPath, 0o644, "file", false},
		{linuxnet.NetworkPath, 0o644, "file", false},
		{linuxnet.BridgeConfPath, 0o644, "file", false},
		{stateDir, 0o700, "directory", false},
	}
}

func (p Probe) collectLinux(ctx context.Context, _ Request) Snapshot {
	snapshot := Snapshot{Addresses: make(map[string]string), Installation: Installation{Status: "absent"}}
	stateDir := filepath.Dir(linuxnet.StatePath)
	anchors := 0
	for _, path := range []string{linuxnet.NetDevPath, linuxnet.NetworkPath, linuxnet.PublicStatePath, stateDir} {
		if _, err := p.lstat(path); err == nil {
			anchors++
		} else if !errors.Is(err, os.ErrNotExist) {
			invalidate(&snapshot.Installation, path+": "+err.Error())
		}
	}
	if bridgeConf, err := p.readFile(linuxnet.BridgeConfPath); err == nil && strings.Contains(string(bridgeConf), linuxBridgeMarker) {
		anchors++
	}
	networkdShape := false
	for _, path := range []string{linuxnet.NetDevPath, linuxnet.NetworkPath} {
		if _, err := p.lstat(path); err == nil {
			networkdShape = true
		}
	}
	_, nmErr := p.lstat(linuxnet.PublicStatePath)
	nmShape := nmErr == nil
	if networkdShape && nmShape {
		invalidate(&snapshot.Installation, "mixed systemd-networkd and NetworkManager installation shapes")
	}
	required := linuxRequiredTargets(nmShape && !networkdShape, stateDir)
	present := 0
	if anchors > 0 || snapshot.Installation.Status == "invalid" {
		for _, target := range required {
			info, err := p.lstat(target.path)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					invalidate(&snapshot.Installation, target.path+": "+err.Error())
				}
				continue
			}
			present++
			if metadataErr := rootMetadata(info, target.mode, target.kind, 0, target.sticky); metadataErr != nil {
				invalidate(&snapshot.Installation, target.path+": "+metadataErr.Error())
			}
		}
	}
	if anchors > 0 && present != len(required) && snapshot.Installation.Status != "invalid" {
		snapshot.Installation.Status = "partial"
		snapshot.Installation.Problem = fmt.Sprintf("partial Linux Barn network installation: %d/%d required public paths", present, len(required))
	}
	if addresses, err := p.Runner.Run(ctx, "/usr/sbin/ip", "-4", "-o", "address", "show"); err != nil {
		snapshot.Problems = append(snapshot.Problems, "ip address show: "+err.Error())
	} else {
		snapshot.Interfaces = parseLinuxInterfaces(string(addresses.Stdout))
	}
	if routes, err := p.Runner.Run(ctx, "/usr/sbin/ip", "-4", "route", "show", "table", "all"); err != nil {
		snapshot.Problems = append(snapshot.Problems, "ip route show: "+err.Error())
	} else {
		snapshot.Routes = parseLinuxRoutes(string(routes.Stdout))
	}
	if anchors > 0 && present == len(required) && snapshot.Installation.Status != "invalid" {
		if nmShape && !networkdShape {
			p.verifyLinuxNMInstallation(ctx, &snapshot)
		} else {
			p.verifyLinuxNetworkdInstallation(ctx, &snapshot)
		}
	}
	return snapshot
}

// verifyLinuxOwnedCommon runs the family/helper/manifest checks shared by both
// Linux backends and reports whether the ownership manifest was readable and
// matched. layout carries the parsed network; expectBackend gates the manifest.
func (p Probe) verifyLinuxOwnedCommon(ctx context.Context, snapshot *Snapshot, layout subnet.Layout, expectBackend string) (protectedState bool) {
	bridgeConf, bridgeErr := p.readFile(linuxnet.BridgeConfPath)
	if bridgeErr != nil || len(bridgeConf) > 1<<20 || strings.Count(string(bridgeConf), linuxBridgeMarker) != 1 {
		invalidate(&snapshot.Installation, linuxnet.BridgeConfPath+": exact Barn marker block is missing or duplicated")
	}
	p.verifyLinuxHelperOwnership(ctx, &snapshot.Installation)
	if stateInfo, stateErr := p.lstat(linuxnet.StatePath); stateErr == nil {
		if metadataErr := rootMetadata(stateInfo, 0o600, "file", 0, false); metadataErr != nil {
			invalidate(&snapshot.Installation, linuxnet.StatePath+": "+metadataErr.Error())
		} else if stateData, readErr := p.readFile(linuxnet.StatePath); readErr != nil {
			invalidate(&snapshot.Installation, linuxnet.StatePath+": "+readErr.Error())
		} else if manifest, manifestErr := linuxnet.StrictManifest(stateData); manifestErr != nil || linuxnet.ManifestBackend(manifest) != expectBackend || manifest.CIDR != layout.CIDR() || manifest.HostAddress != layout.HostAddress() || manifest.DHCPEnd != layout.DHCPEnd() || (snapshot.Installation.Family != "" && string(manifest.Family) != snapshot.Installation.Family) || (snapshot.Installation.HelperPath != "" && manifest.HelperPath != snapshot.Installation.HelperPath) {
			invalidate(&snapshot.Installation, linuxnet.StatePath+": protected ownership manifest differs from the installed network")
		} else {
			for path, expectedDigest := range manifest.Files {
				content, readErr := p.readFile(path)
				if readErr != nil || len(content) > 1<<20 || fsutil.SHA256Bytes(content) != expectedDigest {
					invalidate(&snapshot.Installation, path+": content differs from the protected ownership manifest")
				}
			}
		}
	} else if errors.Is(stateErr, os.ErrNotExist) {
		invalidate(&snapshot.Installation, linuxnet.StatePath+": protected ownership manifest is missing")
	} else if protectedInspectionError(stateErr) {
		protectedState = true
	} else {
		invalidate(&snapshot.Installation, linuxnet.StatePath+": "+stateErr.Error())
	}
	return protectedState
}

func (p Probe) verifyLinuxHelperOwnership(ctx context.Context, installation *Installation) {
	osRelease, familyErr := p.readFile("/etc/os-release")
	family, parseFamilyErr := linuxFamily(osRelease)
	if familyErr != nil || parseFamilyErr != nil {
		invalidate(installation, "cannot identify the installed Linux network family")
		return
	}
	installation.Family = string(family)
	helper := ""
	var helperInfo os.FileInfo
	for _, candidate := range []string{"/usr/lib/qemu/qemu-bridge-helper", "/usr/libexec/qemu-bridge-helper"} {
		if candidateInfo, candidateErr := p.lstat(candidate); candidateErr == nil {
			helper, helperInfo = candidate, candidateInfo
			break
		}
	}
	if helper == "" {
		invalidate(installation, "installed Linux network lacks qemu-bridge-helper")
		return
	}
	if helperErr := validateLinuxHelper(helperInfo, family); helperErr != nil {
		invalidate(installation, helper+": "+helperErr.Error())
		return
	}
	if parentErr := p.safeRootOwnedParents(helper); parentErr != nil {
		invalidate(installation, helper+": "+parentErr.Error())
		return
	}
	ownershipOK := false
	if family == linuxnet.Debian {
		packageOwner, packageErr := p.Runner.Run(ctx, "/usr/bin/dpkg-query", "-S", helper)
		override, overrideErr := p.Runner.Run(ctx, "/usr/bin/dpkg-statoverride", "--list", helper)
		groupName := ""
		if statistics, ok := helperInfo.Sys().(*syscall.Stat_t); ok {
			if group, groupErr := user.LookupGroupId(strconv.FormatUint(uint64(statistics.Gid), 10)); groupErr == nil {
				groupName = group.Name
			}
		}
		expectedOverride := "root " + groupName + " 4750 " + helper
		ownershipOK = groupName != "" && packageErr == nil && strings.Contains(string(packageOwner.Stdout), ": "+helper) && overrideErr == nil && strings.TrimSpace(string(override.Stdout)) == expectedOverride
	} else {
		packageOwner, packageErr := p.Runner.Run(ctx, "/usr/bin/rpm", "-qf", helper)
		ownershipOK = packageErr == nil && strings.TrimSpace(string(packageOwner.Stdout)) != ""
	}
	if !ownershipOK {
		invalidate(installation, helper+": package ownership or reversible privilege policy is invalid")
		return
	}
	installation.HelperPath = helper
}

func (p Probe) finalizeLinuxInstallationHealth(ctx context.Context, snapshot *Snapshot, layout subnet.Layout, backend, serviceUnit, serviceLabel string) {
	snapshot.Installation.Mode = "bridge"
	snapshot.Installation.CIDR = layout.CIDR()
	snapshot.Installation.HostAddress = layout.HostAddress()
	snapshot.Installation.Interface = linuxnet.BridgeName
	protectedState := p.verifyLinuxOwnedCommon(ctx, snapshot, layout, backend)
	if snapshot.Installation.Status != "invalid" {
		if protectedState {
			snapshot.Installation.Status = "protected"
		} else {
			snapshot.Installation.Status = "exact"
		}
	}
	addressReady := false
	for _, address := range snapshot.Interfaces {
		if address.Interface == linuxnet.BridgeName && address.Address.String() == layout.HostAddress() && address.Prefix.Bits() == 24 {
			addressReady = true
		}
	}
	active, activeErr := p.Runner.Run(ctx, "/usr/bin/systemctl", "is-active", serviceUnit)
	link, linkErr := p.Runner.Run(ctx, "/usr/sbin/ip", "-d", "link", "show", "dev", linuxnet.BridgeName)
	snapshot.Installation.Healthy = addressReady && activeErr == nil && strings.TrimSpace(string(active.Stdout)) == "active" && linkErr == nil && strings.Contains(string(link.Stdout), "bridge")
	if !snapshot.Installation.Healthy && snapshot.Installation.Problem == "" {
		snapshot.Installation.Problem = fmt.Sprintf("owned Linux network exists but %s, barn0 bridge type, or configured host .1/24 is not ready", serviceLabel)
	}
}

func (p Probe) verifyLinuxNetworkdInstallation(ctx context.Context, snapshot *Snapshot) {
	data, err := p.readFile(linuxnet.NetworkPath)
	layout, layoutErr := linuxNetworkLayout(data)
	if err != nil || layoutErr != nil {
		invalidate(&snapshot.Installation, "cannot parse owned Linux Barn network unit")
		return
	}
	for _, exact := range []struct {
		path    string
		content string
	}{
		{linuxnet.NetDevPath, "[NetDev]\nName=barn0\nKind=bridge\n"},
		{linuxnet.NetworkPath, exactLinuxNetwork(layout)},
	} {
		if fileErr := p.exactPublicFile(exact.path, []byte(exact.content), 1<<20); fileErr != nil {
			invalidate(&snapshot.Installation, exact.path+": "+fileErr.Error())
		}
	}
	p.finalizeLinuxInstallationHealth(ctx, snapshot, layout, linuxnet.BackendNetworkd, "systemd-networkd.service", "networkd")
}

func (p Probe) verifyLinuxNMInstallation(ctx context.Context, snapshot *Snapshot) {
	data, err := p.readFile(linuxnet.PublicStatePath)
	if err != nil {
		invalidate(&snapshot.Installation, linuxnet.PublicStatePath+": "+err.Error())
		return
	}
	state, parseErr := linuxnet.ParsePublicState(data)
	if parseErr != nil {
		invalidate(&snapshot.Installation, linuxnet.PublicStatePath+": "+parseErr.Error())
		return
	}
	layout, layoutErr := subnet.Parse(state.CIDR)
	if layoutErr != nil {
		invalidate(&snapshot.Installation, linuxnet.PublicStatePath+": "+layoutErr.Error())
		return
	}
	p.finalizeLinuxInstallationHealth(ctx, snapshot, layout, linuxnet.BackendNetworkManager, "NetworkManager.service", "NetworkManager")
}
