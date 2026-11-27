package preflight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/fsutil"
	darwinnet "github.com/pgsty/barn/internal/network/darwin"
	"github.com/pgsty/barn/internal/network/subnet"
)

func parseDarwinInterfaces(output string) []InterfaceAddress {
	result := make([]InterfaceAddress, 0)
	name := ""
	for _, line := range strings.Split(output, "\n") {
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			if colon := strings.IndexByte(line, ':'); colon > 0 {
				name = line[:colon]
			}
			continue
		}
		fields := strings.Fields(line)
		if name == "" || len(fields) < 4 || fields[0] != "inet" || fields[2] != "netmask" {
			continue
		}
		address, err := netip.ParseAddr(fields[1])
		if err != nil || !address.Is4() {
			continue
		}
		maskValue, err := strconv.ParseUint(strings.TrimPrefix(fields[3], "0x"), 16, 32)
		if err != nil {
			continue
		}
		prefix := netip.PrefixFrom(address, bits.OnesCount32(uint32(maskValue)))
		result = append(result, InterfaceAddress{Address: address, Prefix: prefix, Interface: name, Evidence: strings.TrimSpace(line)})
	}
	return result
}

func parseDarwinDestination(value string) (netip.Prefix, error) {
	addressText, bitsText, hasBits := strings.Cut(value, "/")
	parts := strings.Split(addressText, ".")
	if len(parts) < 1 || len(parts) > 4 {
		return netip.Prefix{}, fmt.Errorf("invalid Darwin route destination %q", value)
	}
	for len(parts) < 4 {
		parts = append(parts, "0")
	}
	address, err := netip.ParseAddr(strings.Join(parts, "."))
	if err != nil || !address.Is4() {
		return netip.Prefix{}, fmt.Errorf("invalid Darwin IPv4 route destination %q", value)
	}
	prefixBits := 32
	if hasBits {
		prefixBits, err = strconv.Atoi(bitsText)
		if err != nil || prefixBits < 0 || prefixBits > 32 {
			return netip.Prefix{}, fmt.Errorf("invalid Darwin route prefix %q", value)
		}
	} else if len(strings.Split(addressText, ".")) < 4 {
		prefixBits = len(strings.Split(addressText, ".")) * 8
	}
	return netip.PrefixFrom(address, prefixBits).Masked(), nil
}

func parseDarwinRoutes(output string) []Route {
	result := make([]Route, 0)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] == "default" || fields[0] == "Destination" || fields[0] == "Internet:" {
			continue
		}
		prefix, err := parseDarwinDestination(fields[0])
		if err != nil {
			continue
		}
		flags := fields[2]
		// netstat includes cloned ARP/neighbor host entries in the routing
		// table. They are observations on the connected network, not routes
		// that claim an overlapping subnet.
		if prefix.Bits() == 32 && (strings.Contains(flags, "L") || strings.Contains(flags, "W")) {
			continue
		}
		result = append(result, Route{Prefix: prefix, Interface: fields[3], Kind: "route", Evidence: strings.TrimSpace(line)})
	}
	return result
}

func (p Probe) darwinSharingConflict(ctx context.Context, request Request, installation Installation) (string, error) {
	if (installation.Status == "exact" || installation.Status == "protected") && installation.CIDR == request.Layout.CIDR() && installation.Healthy {
		return "", nil
	}
	if installation.CIDR == request.Layout.CIDR() {
		logResult, err := p.Runner.Run(ctx, "/usr/bin/tail", "-n", "80", darwinnet.LogDir+"/stderr.log")
		logText := string(logResult.Stdout) + string(logResult.Stderr)
		if err == nil && (strings.Contains(logText, "[1009]") || strings.Contains(logText, "VMNET_SHARING_SERVICE_BUSY")) {
			return "macOS vmnet reported the requested subnet busy: another sharing service holds it", nil
		}
	}
	err := darwinnet.CheckSharingSubnet(ctx, p.Runner, p.Runner, request.Layout)
	if errors.Is(err, darwinnet.ErrVMNetSharingBusy) {
		return fmt.Sprintf("macOS Internet Sharing is active and uses %s", request.Layout.CIDR()), nil
	}
	return "", err
}

func plistArguments(data []byte) (mode, interfaceID, gateway, dhcp string, err error) {
	var args []string
	if err = json.Unmarshal(data, &args); err != nil {
		return "", "", "", "", err
	}
	for _, argument := range args {
		switch {
		case strings.HasPrefix(argument, "--vmnet-mode="):
			mode = strings.TrimPrefix(argument, "--vmnet-mode=")
		case strings.HasPrefix(argument, "--vmnet-interface-id="):
			interfaceID = strings.TrimPrefix(argument, "--vmnet-interface-id=")
		case strings.HasPrefix(argument, "--vmnet-gateway="):
			gateway = strings.TrimPrefix(argument, "--vmnet-gateway=")
		case strings.HasPrefix(argument, "--vmnet-dhcp-end="):
			dhcp = strings.TrimPrefix(argument, "--vmnet-dhcp-end=")
		}
	}
	if (mode != "host" && mode != "shared") || interfaceID == "" || gateway == "" || dhcp == "" {
		err = errors.New("launchd ProgramArguments omit required vmnet fields")
	}
	return
}

func darwinInterfaceObserved(interfaces []InterfaceAddress, layout subnet.Layout, marker darwinnet.InterfaceMarker) bool {
	if marker.CIDR != layout.CIDR() || marker.HostAddress != layout.HostAddress() {
		return false
	}
	host, err := netip.ParseAddr(layout.HostAddress())
	if err != nil {
		return false
	}
	for _, address := range interfaces {
		if address.Interface == marker.BSDName && address.Address == host && address.Prefix.Bits() == layout.Prefix().Bits() && address.Prefix.Masked() == layout.Prefix() {
			return true
		}
	}
	return false
}

var darwinRequiredPaths = []struct {
	path   string
	mode   os.FileMode
	kind   string
	group  uint32
	sticky bool
}{
	{darwinnet.DaemonPath, 0o755, "file", 0, false},
	{darwinnet.ClientPath, 0o755, "file", 0, false},
	{darwinnet.PlistPath, 0o644, "file", 0, false},
	{darwinnet.StateDir, 0o700, "directory", 0, false},
	{darwinnet.InterfaceMarkerDir, 0o755, "directory", 0, false},
	{darwinnet.InterfaceMarkerPath, 0o644, "file", 0, false},
}

func (p Probe) collectDarwin(ctx context.Context, request Request) Snapshot {
	snapshot := Snapshot{Addresses: make(map[string]string), Installation: Installation{Status: "absent"}}
	present, socketMetadataOK, socketProblem := p.inspectDarwinFiles(&snapshot.Installation)

	if interfacesResult, err := p.Runner.Run(ctx, "/sbin/ifconfig"); err != nil {
		snapshot.Problems = append(snapshot.Problems, "ifconfig: "+err.Error())
	} else {
		snapshot.Interfaces = parseDarwinInterfaces(string(interfacesResult.Stdout))
	}
	if present == len(darwinRequiredPaths) && snapshot.Installation.Status != "invalid" {
		p.verifyDarwinInstallation(ctx, request, &snapshot, socketMetadataOK, socketProblem)
	}
	// Logs are runtime support, not evidence of network ownership. A directory
	// recreated by launchd with mode 0744 must not hide a verified Barn bridge.
	if snapshot.Installation.Status == "exact" || snapshot.Installation.Status == "protected" {
		if finding := p.darwinLogDirectoryFinding(); finding != nil {
			snapshot.Findings = append(snapshot.Findings, *finding)
		}
	}
	if routes, err := p.Runner.Run(ctx, "/usr/sbin/netstat", "-rn", "-f", "inet"); err != nil {
		snapshot.Problems = append(snapshot.Problems, "netstat IPv4 route table: "+err.Error())
	} else {
		snapshot.Routes = parseDarwinRoutes(string(routes.Stdout))
	}
	p.checkDarwinSharing(ctx, request, &snapshot)
	return snapshot
}

func (p Probe) inspectDarwinFiles(installation *Installation) (present int, socketMetadataOK bool, socketProblem string) {
	for _, target := range darwinRequiredPaths {
		info, err := p.lstat(target.path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				invalidate(installation, target.path+": "+err.Error())
			}
			continue
		}
		present++
		if metadataErr := rootMetadata(info, target.mode, target.kind, target.group, target.sticky); metadataErr != nil {
			invalidate(installation, target.path+": "+metadataErr.Error())
		}
	}
	if present != 0 && present != len(darwinRequiredPaths) && installation.Status != "invalid" {
		installation.Status = "partial"
		installation.Problem = fmt.Sprintf("only %d of %d installed network files are present", present, len(darwinRequiredPaths))
	}
	if present == 0 && installation.Status == "absent" {
		if _, err := p.lstat(darwinnet.LogDir); err == nil {
			installation.Status = "partial"
			installation.Problem = darwinnet.LogDir + " exists but the Barn network is not installed"
		} else if !errors.Is(err, os.ErrNotExist) {
			invalidate(installation, darwinnet.LogDir+": "+err.Error())
		}
	}
	socketExists := false
	if socketInfo, err := p.lstat(darwinnet.SocketPath); err == nil {
		socketExists = true
		if metadataErr := rootMetadata(socketInfo, 0o770, "socket", 20, false); metadataErr != nil {
			socketProblem = darwinnet.SocketPath + ": " + metadataErr.Error()
		} else {
			socketMetadataOK = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		socketProblem = darwinnet.SocketPath + ": " + err.Error()
	}
	if present == 0 && socketExists && installation.Status == "absent" {
		installation.Status = "partial"
		installation.Problem = "a vmnet socket exists but the Barn network is not installed"
	}
	return present, socketMetadataOK, socketProblem
}

func (p Probe) verifyDarwinInstallation(ctx context.Context, request Request, snapshot *Snapshot, socketMetadataOK bool, socketProblem string) {
	arguments, err := p.Runner.Run(ctx, "/usr/bin/plutil", "-extract", "ProgramArguments", "json", "-o", "-", darwinnet.PlistPath)
	if err != nil {
		invalidate(&snapshot.Installation, "cannot read Darwin launch arguments: "+err.Error())
		return
	}
	var exactArgs []string
	decodeErr := json.Unmarshal(arguments.Stdout, &exactArgs)
	mode, interfaceID, gateway, dhcp, parseErr := plistArguments(arguments.Stdout)
	layout, layoutErr := subnet.FromHostAddress(gateway)
	if parseErr != nil || layoutErr != nil || dhcp != layout.DHCPEnd() {
		invalidate(&snapshot.Installation, "Darwin launch arguments do not describe a valid Barn /24")
		return
	}
	snapshot.Installation.Mode = mode
	snapshot.Installation.CIDR = layout.CIDR()
	snapshot.Installation.HostAddress = layout.HostAddress()

	plan, planErr := darwinnet.NewInstallPlanModeNetwork(request.Arch, interfaceID, mode, layout.CIDR())
	if decodeErr != nil || planErr != nil || !slices.Equal(exactArgs, plan.Args) {
		invalidate(&snapshot.Installation, "Darwin launch arguments differ from the pinned network plan")
	} else if expectedPlist, plistErr := plan.Plist(); plistErr != nil {
		invalidate(&snapshot.Installation, "cannot reproduce the pinned Darwin plist: "+plistErr.Error())
	} else if fileErr := p.exactPublicFile(darwinnet.PlistPath, expectedPlist, 1<<20); fileErr != nil {
		invalidate(&snapshot.Installation, darwinnet.PlistPath+": "+fileErr.Error())
	}

	interfaceData, interfaceReadErr := p.readFile(darwinnet.InterfaceMarkerPath)
	interfaceMarker, interfaceParseErr := darwinnet.StrictInterfaceMarker(interfaceData)
	markerOK := interfaceReadErr == nil && interfaceParseErr == nil
	if !markerOK {
		invalidate(&snapshot.Installation, darwinnet.InterfaceMarkerPath+": public interface identity is unreadable or invalid")
	} else {
		// The root-owned public marker records the binary provenance;
		// overlay it so the plan expects the recorded digests.
		plan, planErr = plan.WithRecordedBinaries(interfaceMarker.Source, interfaceMarker.SocketSHA256, interfaceMarker.ClientSHA256)
		if planErr == nil {
			plan, planErr = plan.WithBSDInterface(interfaceMarker.BSDName)
		}
		if planErr != nil || plan.Interface != interfaceMarker {
			invalidate(&snapshot.Installation, darwinnet.InterfaceMarkerPath+": interface identity differs from the pinned network plan")
		} else if expectedInterface, markerErr := plan.InterfaceJSON(); markerErr != nil || !bytes.Equal(expectedInterface, interfaceData) {
			invalidate(&snapshot.Installation, darwinnet.InterfaceMarkerPath+": interface identity bytes are not canonical")
		}
	}

	release, releaseErr := darwinnet.PinnedRelease(request.Arch)
	expectedSocketSHA, expectedClientSHA := release.SocketSHA256, release.ClientSHA256
	if markerOK && interfaceMarker.Source != "" {
		expectedSocketSHA, expectedClientSHA = interfaceMarker.SocketSHA256, interfaceMarker.ClientSHA256
	}
	for _, binary := range []struct{ path, digest string }{{darwinnet.DaemonPath, expectedSocketSHA}, {darwinnet.ClientPath, expectedClientSHA}} {
		data, readErr := p.readFile(binary.path)
		if releaseErr != nil || readErr != nil || len(data) == 0 || len(data) > 64<<20 || fsutil.SHA256Bytes(data) != binary.digest {
			invalidate(&snapshot.Installation, binary.path+": installed digest differs from the recorded provenance")
		}
	}

	protectedState := p.verifyDarwinProtectedState(&snapshot.Installation, plan.State)
	protectedInterfaceState := p.verifyDarwinProtectedInterface(&snapshot.Installation, interfaceData)
	if snapshot.Installation.Status != "invalid" {
		if protectedState || protectedInterfaceState {
			snapshot.Installation.Status = "protected"
		} else {
			snapshot.Installation.Status = "exact"
		}
	}

	interfaceObserved := false
	if markerOK {
		snapshot.Installation.Interface = interfaceMarker.BSDName
		interfaceObserved = darwinInterfaceObserved(snapshot.Interfaces, layout, interfaceMarker)
	}
	var dialErr error
	if socketMetadataOK {
		var connection net.Conn
		connection, dialErr = p.dial("unix", darwinnet.SocketPath, time.Second)
		if dialErr == nil {
			_ = connection.Close()
		}
	} else {
		dialErr = errors.New("runtime socket is absent or unsafe")
	}
	snapshot.Installation.Healthy = dialErr == nil && interfaceObserved
	if !snapshot.Installation.Healthy && snapshot.Installation.Problem == "" {
		if socketProblem != "" {
			snapshot.Installation.Problem = socketProblem
		} else {
			snapshot.Installation.Problem = "socket_vmnet is installed but its socket, host address, or interface is not up"
		}
	}
}

func (p Probe) verifyDarwinProtectedState(installation *Installation, expected darwinnet.NetworkState) bool {
	info, err := p.lstat(darwinnet.StatePath)
	switch {
	case err == nil:
		if metadataErr := rootMetadata(info, 0o600, "file", 0, false); metadataErr != nil {
			invalidate(installation, darwinnet.StatePath+": "+metadataErr.Error())
		} else if data, readErr := p.readFile(darwinnet.StatePath); readErr != nil {
			invalidate(installation, darwinnet.StatePath+": "+readErr.Error())
		} else if state, stateErr := darwinnet.StrictNetworkState(data); stateErr != nil || state != expected {
			invalidate(installation, darwinnet.StatePath+": protected state differs from the pinned network plan")
		}
	case errors.Is(err, os.ErrNotExist):
		invalidate(installation, darwinnet.StatePath+": protected network state is missing")
	case protectedInspectionError(err):
		return true
	default:
		invalidate(installation, darwinnet.StatePath+": "+err.Error())
	}
	return false
}

func (p Probe) verifyDarwinProtectedInterface(installation *Installation, publicBytes []byte) bool {
	info, err := p.lstat(darwinnet.InterfaceStatePath)
	switch {
	case err == nil:
		if metadataErr := rootMetadata(info, 0o600, "file", 0, false); metadataErr != nil {
			invalidate(installation, darwinnet.InterfaceStatePath+": "+metadataErr.Error())
		} else if protectedData, readErr := p.readFile(darwinnet.InterfaceStatePath); readErr != nil || !bytes.Equal(protectedData, publicBytes) {
			invalidate(installation, darwinnet.InterfaceStatePath+": protected interface identity differs from the public root-owned marker")
		}
	case errors.Is(err, os.ErrNotExist):
		invalidate(installation, darwinnet.InterfaceStatePath+": protected interface identity is missing")
	case protectedInspectionError(err):
		return true
	default:
		invalidate(installation, darwinnet.InterfaceStatePath+": "+err.Error())
	}
	return false
}

func (p Probe) checkDarwinSharing(ctx context.Context, request Request, snapshot *Snapshot) {
	sharingBusy, sharingErr := p.darwinSharingConflict(ctx, request, snapshot.Installation)
	snapshot.SharingBusy = sharingBusy
	if sharingErr == nil {
		return
	}
	finding := Finding{
		Code: "vmnet.sharing_probe", Severity: Error, Class: Capability, Evidence: sharingErr.Error(),
		Fix: "inspect the vmnet configuration with: sudo plutil -p /Library/Preferences/SystemConfiguration/com.apple.vmnet.plist",
	}
	if errors.Is(sharingErr, darwinnet.ErrSharingPreferencesUnreadable) {
		switch {
		case snapshot.Installation.Status == "absent":
			finding.Severity = Warning
			finding.Fix = "network installation will check the sharing preferences with your approval before changing the host"
		case (snapshot.Installation.Status == "exact" || snapshot.Installation.Status == "protected") && snapshot.Installation.CIDR == request.Layout.CIDR():
			finding.Severity = Warning
			finding.Fix = "network repair will check the sharing preferences with your approval before restarting the host network"
		}
	}
	snapshot.Findings = append(snapshot.Findings, finding)
}

func (p Probe) darwinLogDirectoryFinding() *Finding {
	info, err := p.lstat(darwinnet.LogDir)
	if errors.Is(err, os.ErrNotExist) {
		info, err = nil, nil
	}
	needsRepair := false
	if err == nil {
		needsRepair, err = darwinnet.LogDirectoryNeedsRepair(info)
	}
	if err != nil {
		return &Finding{
			Code: "installation.log_directory_unsafe", Severity: Error, Class: Integrity,
			Subject: darwinnet.LogDir, Evidence: err.Error(),
			Fix: "inspect with ls -ld " + darwinnet.LogDir + "; restore a real root:wheel directory with mode 0755, then retry",
		}
	}
	if !needsRepair {
		return nil
	}
	evidence := darwinnet.LogDir + " is missing; expected a root:wheel directory with mode 0755"
	command := "sudo install -d -o root -g wheel -m 0755 " + darwinnet.LogDir
	if info != nil {
		evidence = fmt.Sprintf("%s has mode %04o; expected 0755 (owner root:wheel is correct)", darwinnet.LogDir, info.Mode().Perm())
		command = "sudo chmod 0755 " + darwinnet.LogDir
	}
	return &Finding{
		Code: "installation.log_directory", Severity: Error, Class: Capability,
		Subject: darwinnet.LogDir, Evidence: evidence,
		Fix: "run barn up to repair it (sudo may ask for a password), or run: " + command,
	}
}
