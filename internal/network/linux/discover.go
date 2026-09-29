package linux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/pgsty/barn/internal/execx"
)

func hostStatIdentity(info os.FileInfo) (uint32, uint32, uint32, error) {
	statistics, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, errors.New("unsupported Linux stat identity")
	}
	mode := uint32(info.Mode().Perm())
	if info.Mode()&os.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if info.Mode()&os.ModeSetgid != 0 {
		mode |= 0o2000
	}
	return statistics.Uid, statistics.Gid, mode, nil
}

func hostGroupName(gid uint32) (string, error) {
	group, err := user.LookupGroupId(strconv.FormatUint(uint64(gid), 10))
	if err != nil {
		return "", err
	}
	return group.Name, nil
}

func selectAccessGroup(primary string, memberships []string) (string, error) {
	if !accessGroupPattern.MatchString(primary) {
		return "", fmt.Errorf("unsafe primary group name %q", primary)
	}
	for _, name := range memberships {
		if name == "kvm" {
			return name, nil
		}
	}
	return primary, nil
}

type groupIdentity struct {
	Primary uint32
	Groups  []uint32
}

func parseUnixID(label, value string) (uint32, error) {
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q", label, value)
	}
	return uint32(parsed), nil
}

func sudoGroupIdentity(rawUID, rawGID string, account *user.User, rawGroups []string) (groupIdentity, error) {
	uid, err := parseUnixID("SUDO_UID", rawUID)
	if err != nil || uid == 0 {
		return groupIdentity{}, errors.New("debian network install as root requires a non-root SUDO_UID/SUDO_GID; invoke barn as the target user")
	}
	gid, err := parseUnixID("SUDO_GID", rawGID)
	if err != nil {
		return groupIdentity{}, err
	}
	if account == nil {
		return groupIdentity{}, errors.New("sudo invoking-user identity is unavailable")
	}
	accountUID, uidErr := parseUnixID("sudo account uid", account.Uid)
	accountGID, gidErr := parseUnixID("sudo account gid", account.Gid)
	if uidErr != nil || gidErr != nil || accountUID != uid || accountGID != gid {
		return groupIdentity{}, errors.New("SUDO_UID/SUDO_GID do not match the invoking-user account")
	}
	groups := []uint32{gid}
	for _, raw := range rawGroups {
		groupID, parseErr := parseUnixID("supplementary group id", raw)
		if parseErr == nil {
			groups = append(groups, groupID)
		}
	}
	return groupIdentity{Primary: gid, Groups: groups}, nil
}

func invokingGroupIdentity() (groupIdentity, error) {
	if os.Geteuid() != 0 {
		groupIDs, err := os.Getgroups()
		if err != nil {
			return groupIdentity{}, err
		}
		groups := make([]uint32, 0, len(groupIDs)+1)
		groups = append(groups, uint32(os.Getgid()))
		for _, gid := range groupIDs {
			if gid >= 0 {
				groups = append(groups, uint32(gid))
			}
		}
		return groupIdentity{Primary: uint32(os.Getgid()), Groups: groups}, nil
	}
	rawUID := os.Getenv("SUDO_UID")
	account, err := user.LookupId(rawUID)
	if err != nil {
		return groupIdentity{}, errors.New("cannot resolve the non-root SUDO_UID account for Debian network install")
	}
	rawGroups, err := account.GroupIds()
	if err != nil {
		// The primary group is still a valid, least-privilege access boundary.
		rawGroups = nil
	}
	return sudoGroupIdentity(rawUID, os.Getenv("SUDO_GID"), account, rawGroups)
}

func accessGroupForIdentity(identity groupIdentity, lookup func(uint32) (string, error)) (string, error) {
	primary, err := lookup(identity.Primary)
	if err != nil {
		return "", err
	}
	memberships := make([]string, 0, len(identity.Groups)+1)
	memberships = append(memberships, primary)
	for _, gid := range identity.Groups {
		name, lookupErr := lookup(gid)
		if lookupErr == nil {
			memberships = append(memberships, name)
		}
	}
	return selectAccessGroup(primary, memberships)
}

func discoverAccessGroup() (string, error) {
	identity, err := invokingGroupIdentity()
	if err != nil {
		return "", err
	}
	return accessGroupForIdentity(identity, hostGroupName)
}

func safeRootParents(path string) bool {
	clean := filepath.Clean(path)
	for parent := filepath.Dir(clean); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
			return false
		}
		uid, _, _, err := hostStatIdentity(info)
		if err != nil || uid != 0 {
			return false
		}
		if parent == "/" {
			return true
		}
	}
}

func discoverUnitStateMode(ctx context.Context, runner execx.Runner, name string, optional bool) (UnitState, error) {
	result, err := runner.Run(ctx, "/usr/bin/systemctl", "show", name, "--no-pager", "-p", "LoadState", "-p", "UnitFileState", "-p", "ActiveState", "-p", "SubState")
	if err != nil {
		return UnitState{}, err
	}
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(result.Stdout)), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			values[key] = value
		}
	}
	state := UnitState{LoadState: values["LoadState"], UnitFileState: values["UnitFileState"], ActiveState: values["ActiveState"], SubState: values["SubState"]}
	if optional && state.LoadState == "not-found" && state.UnitFileState == "" && state.ActiveState == "inactive" && state.SubState == "dead" {
		return state, nil
	}
	if state.LoadState == "" || state.UnitFileState == "" || state.ActiveState == "" || state.SubState == "" {
		return UnitState{}, fmt.Errorf("incomplete systemd state for %s", name)
	}
	return state, nil
}

func discoverOptionalUnitState(ctx context.Context, runner execx.Runner, name string) (UnitState, error) {
	return discoverUnitStateMode(ctx, runner, name, true)
}

func discoverFamily() (Family, error) {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil || len(data) > 64<<10 {
		return "", errors.New("cannot read bounded /etc/os-release")
	}
	lower := strings.ToLower(string(data))
	if strings.Contains(lower, "ubuntu") || strings.Contains(lower, "debian") {
		return Debian, nil
	}
	if strings.Contains(lower, "rhel") || strings.Contains(lower, "fedora") || strings.Contains(lower, "rocky") || strings.Contains(lower, "almalinux") || strings.Contains(lower, "centos") || strings.Contains(lower, "oracle") {
		return RPM, nil
	}
	return "", errors.New("unsupported Linux distribution family")
}

func discoverBridgeConf() (string, PathState, error) {
	info, err := os.Lstat(BridgeConfPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", PathState{}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", PathState{}, errors.New("existing bridge.conf is not a regular non-symlink file")
	}
	uid, gid, mode, err := hostStatIdentity(info)
	if err != nil || uid != 0 {
		return "", PathState{}, errors.New("existing bridge.conf is not root-owned")
	}
	group, err := hostGroupName(gid)
	if err != nil {
		return "", PathState{}, err
	}
	content, err := os.ReadFile(BridgeConfPath)
	if err != nil || len(content) > 1<<20 {
		return "", PathState{}, errors.New("cannot read bounded bridge.conf")
	}
	return string(content), PathState{Existed: true, Owner: "root", Group: group, Mode: fmt.Sprintf("%04o", mode)}, nil
}

func discoverHelper(ctx context.Context, runner execx.Runner, family Family) (HelperFacts, error) {
	path := ""
	for _, candidate := range []string{"/usr/lib/qemu/qemu-bridge-helper", "/usr/libexec/qemu-bridge-helper"} {
		if _, err := os.Lstat(candidate); err == nil {
			path = candidate
			break
		}
	}
	if path == "" {
		return HelperFacts{}, errors.New("supported qemu-bridge-helper not found")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return HelperFacts{}, err
	}
	uid, gid, mode, err := hostStatIdentity(info)
	if err != nil {
		return HelperFacts{}, err
	}
	group, err := hostGroupName(gid)
	if err != nil {
		return HelperFacts{}, err
	}
	facts := HelperFacts{Path: path, OwnerUID: int(uid), Group: group, Mode: mode, Regular: info.Mode().IsRegular(), Symlink: info.Mode()&os.ModeSymlink != 0, ParentSafe: safeRootParents(path)}
	if family == Debian {
		packageResult, packageErr := runner.Run(ctx, "/usr/bin/dpkg-query", "-S", path)
		facts.PackageOwned = packageErr == nil && strings.Contains(string(packageResult.Stdout), ": "+path)
		overrideResult, overrideErr := runner.Run(ctx, "/usr/bin/dpkg-statoverride", "--list", path)
		if overrideErr == nil && strings.TrimSpace(string(overrideResult.Stdout)) != "" {
			fields := strings.Fields(string(overrideResult.Stdout))
			if len(fields) != 4 || fields[3] != path {
				return HelperFacts{}, errors.New("unexpected dpkg-statoverride output")
			}
			facts.Override = &Override{Owner: fields[0], Group: fields[1], Mode: fields[2]}
		}
	} else {
		facts.PackageOwned = true
	}
	return facts, nil
}

func discoverManifest(ctx context.Context, privileged execx.Runner) (*Manifest, error) {
	stateDir, err := os.Lstat(filepath.Dir(StatePath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !stateDir.IsDir() || stateDir.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("linux network state directory is unsafe")
	}
	result, err := privileged.Run(ctx, "/bin/cat", StatePath)
	if err != nil {
		return nil, fmt.Errorf("read existing linux network state: %w", err)
	}
	manifest, err := StrictManifest(result.Stdout)
	if err != nil {
		return nil, err
	}
	return &manifest, nil
}

func DiscoverFacts(ctx context.Context, runner, privileged execx.Runner) (Facts, error) {
	return discoverFacts(ctx, runner, privileged, true)
}

func discoverFacts(ctx context.Context, runner, privileged execx.Runner, needAccessGroup bool) (Facts, error) {
	if runner == nil || privileged == nil {
		return Facts{}, errors.New("linux network discovery requires user and privileged runners")
	}
	family, err := discoverFamily()
	if err != nil {
		return Facts{}, err
	}
	// The networkd units are optional facts: a NetworkManager-owned host (the
	// whole EL family) does not ship them, and only the networkd backend
	// validates their prestate.
	units := make(map[string]UnitState, len(NetworkdUnitNames))
	for _, name := range NetworkdUnitNames {
		state, err := discoverOptionalUnitState(ctx, runner, name)
		if err != nil {
			return Facts{}, err
		}
		units[name] = state
	}
	bridgeConf, bridgeState, err := discoverBridgeConf()
	if err != nil {
		return Facts{}, err
	}
	helper, err := discoverHelper(ctx, runner, family)
	if err != nil {
		return Facts{}, err
	}
	accessGroup := ""
	if family == Debian && needAccessGroup {
		accessGroup, err = discoverAccessGroup()
		if err != nil {
			return Facts{}, err
		}
	}
	manifest, err := discoverManifest(ctx, privileged)
	if err != nil {
		return Facts{}, err
	}
	if manifest == nil {
		for _, path := range []string{NetDevPath, NetworkPath, PublicStatePath, filepath.Dir(StatePath), StatePath} {
			if _, err := os.Lstat(path); err == nil {
				return Facts{}, fmt.Errorf("will not overwrite %s: it exists and Barn did not create it", path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return Facts{}, err
			}
		}
	}
	qemuDirExists := false
	if info, statErr := os.Lstat(QEMUConfigDir); statErr == nil {
		uid, _, _, identityErr := hostStatIdentity(info)
		if identityErr != nil || uid != 0 || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
			return Facts{}, errors.New("existing /etc/qemu is unsafe")
		}
		qemuDirExists = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Facts{}, statErr
	}
	bridgeExists := false
	if _, err := runner.Run(ctx, "/usr/sbin/ip", "link", "show", "dev", BridgeName); err == nil {
		bridgeExists = true
	}
	networkdActive := units["systemd-networkd.service"].ActiveState == "active"
	nmState, err := discoverOptionalUnitState(ctx, runner, "NetworkManager.service")
	if err != nil {
		return Facts{}, err
	}
	nmActive := nmState.ActiveState == "active"
	// The NetworkManager backend never starts networkd, so the dormant-networkd
	// activation-safety sweep (and its wireless false positives) only applies
	// when networkd is the selected owner.
	var networkdActivation *NetworkdActivationSafety
	if !networkdActive && !nmActive {
		links, err := discoverNetworkdLinks(ctx, runner, "/sys/class/net")
		if err != nil {
			return Facts{}, err
		}
		safety := inspectNetworkdActivation(networkdConfigurationDirectories, links, ownedNetworkdDigests(manifest), true)
		networkdActivation = &safety
	}
	nmConnectionExists := false
	firewalldActive := false
	if nmActive {
		if result, listErr := runner.Run(ctx, NMCLIPath, "-t", "-f", "NAME", "connection", "show"); listErr == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(result.Stdout)), "\n") {
				if line == BridgeName {
					nmConnectionExists = true
				}
			}
		}
		firewalldState, firewalldErr := discoverOptionalUnitState(ctx, runner, "firewalld.service")
		if firewalldErr != nil {
			return Facts{}, firewalldErr
		}
		firewalldActive = firewalldState.ActiveState == "active"
	}
	publicDirExists := false
	if info, statErr := os.Lstat(PublicStateDir); statErr == nil {
		uid, _, _, identityErr := hostStatIdentity(info)
		if identityErr != nil || uid != 0 || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
			return Facts{}, errors.New("existing /etc/barn is unsafe")
		}
		publicDirExists = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Facts{}, statErr
	}
	return Facts{
		Family: family, Systemd: true, NetworkdActive: networkdActive, NetworkdUnits: units, NetworkdActivation: networkdActivation,
		NetworkManagerActive: nmActive, NMConnectionExists: nmConnectionExists, FirewalldActive: firewalldActive,
		PublicDirExisted: publicDirExists, BridgeExists: bridgeExists, BridgeOwned: bridgeExists && manifest != nil,
		BridgeConf: bridgeConf, BridgeConfState: bridgeState, QEMUConfigDirExisted: qemuDirExists, Helper: helper, AccessGroup: accessGroup, ExistingManifest: manifest,
	}, nil
}
