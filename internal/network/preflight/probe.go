package preflight

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"net"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pgsty/farrow/internal/execx"
	darwinnet "github.com/pgsty/farrow/internal/network/darwin"
	linuxnet "github.com/pgsty/farrow/internal/network/linux"
	"github.com/pgsty/farrow/internal/network/subnet"
	"github.com/pgsty/farrow/internal/platform"
)

type DialFunc func(network, address string, timeout time.Duration) (net.Conn, error)

type Probe struct {
	Runner   execx.Runner
	Lstat    func(string) (os.FileInfo, error)
	ReadFile func(string) ([]byte, error)
	Dial     DialFunc
}

func (p Probe) lstat(path string) (os.FileInfo, error) {
	if p.Lstat != nil {
		return p.Lstat(path)
	}
	return os.Lstat(path)
}

func (p Probe) readFile(path string) ([]byte, error) {
	if p.ReadFile != nil {
		return p.ReadFile(path)
	}
	return os.ReadFile(path)
}

func (p Probe) dial(network, address string, timeout time.Duration) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(network, address, timeout)
	}
	return net.DialTimeout(network, address, timeout)
}

func (p Probe) Collect(ctx context.Context, request Request) Snapshot {
	snapshot := Snapshot{Addresses: make(map[string]string)}
	if p.Runner == nil {
		snapshot.Problems = append(snapshot.Problems, "network preflight requires a command runner")
		return snapshot
	}
	switch request.OS {
	case "darwin":
		snapshot = p.collectDarwin(ctx, request)
	case "linux":
		snapshot = p.collectLinux(ctx, request)
	default:
		snapshot.Problems = append(snapshot.Problems, "network preflight does not support "+request.OS)
	}
	if snapshot.Addresses == nil {
		snapshot.Addresses = make(map[string]string)
	}
	var addressMu sync.Mutex
	var addressWG sync.WaitGroup
	addressSlots := make(chan struct{}, 32)
	for _, address := range request.Addresses {
		address := address
		addressWG.Add(1)
		go func() {
			defer addressWG.Done()
			select {
			case addressSlots <- struct{}{}:
				defer func() { <-addressSlots }()
			case <-ctx.Done():
				return
			}
			connection, err := p.dial("tcp", net.JoinHostPort(address, "22"), 250*time.Millisecond)
			if err != nil {
				return
			}
			_ = connection.Close()
			addressMu.Lock()
			snapshot.Addresses[address] = address + " already answers on port 22"
			addressMu.Unlock()
		}()
	}
	addressWG.Wait()
	return snapshot
}

func Run(ctx context.Context, request Request, probe Probe) Report {
	return Evaluate(request, probe.Collect(ctx, request))
}

func rootMetadata(info os.FileInfo, mode os.FileMode, kind string, group uint32, sticky bool) error {
	statistics, ok := info.Sys().(*syscall.Stat_t)
	if !ok || statistics.Uid != 0 || statistics.Gid != group || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode || (sticky && info.Mode()&os.ModeSticky == 0) {
		stickyText := ""
		if sticky {
			stickyText = " sticky"
		}
		return fmt.Errorf("not a root-owned group-%d non-symlink mode-%04o%s %s", group, mode, stickyText, kind)
	}
	switch kind {
	case "file":
		if !info.Mode().IsRegular() {
			return errors.New("not a regular file")
		}
	case "directory":
		if !info.IsDir() {
			return errors.New("not a directory")
		}
	case "socket":
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("not a Unix socket")
		}
	}
	return nil
}

func invalidate(installation *Installation, problem string) {
	installation.Status = "invalid"
	if installation.Problem == "" {
		installation.Problem = problem
	}
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest[:])
}

func (p Probe) exactPublicFile(path string, expected []byte, max int) error {
	data, err := p.readFile(path)
	if err != nil {
		return err
	}
	if len(data) > max || !bytes.Equal(data, expected) {
		return errors.New("content differs from the pinned plan")
	}
	return nil
}

func protectedInspectionError(err error) bool {
	return errors.Is(err, os.ErrPermission)
}

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

func activeNetworkSharing(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 4 && fields[0] == "active" && fields[1] == "count" && fields[2] == "=" {
			count, err := strconv.Atoi(fields[3])
			return err == nil && count > 0
		}
	}
	return false
}

func (p Probe) darwinSharingConflict(ctx context.Context, request Request, installation Installation) (string, string) {
	if (installation.Status == "exact" || installation.Status == "protected") && installation.CIDR == request.Layout.CIDR() && installation.Healthy {
		return "", ""
	}
	if installation.CIDR == request.Layout.CIDR() {
		logResult, err := p.Runner.Run(ctx, "/usr/bin/tail", "-n", "80", darwinnet.LogDir+"/stderr.log")
		logText := string(logResult.Stdout) + string(logResult.Stderr)
		if err == nil && (strings.Contains(logText, "[1009]") || strings.Contains(logText, "VMNET_SHARING_SERVICE_BUSY")) {
			return "macOS vmnet reported the requested subnet busy: another sharing service holds it", ""
		}
	}
	sharing, err := p.Runner.Run(ctx, "/bin/launchctl", "print", "system/com.apple.NetworkSharing")
	if err != nil || !activeNetworkSharing(string(sharing.Stdout)) {
		return "", ""
	}
	const vmnetPlist = "/Library/Preferences/SystemConfiguration/com.apple.vmnet.plist"
	hostResult, hostErr := p.Runner.Run(ctx, "/usr/bin/plutil", "-extract", "Host_Net_Address", "raw", "-o", "-", vmnetPlist)
	maskResult, maskErr := p.Runner.Run(ctx, "/usr/bin/plutil", "-extract", "Host_Net_Mask", "raw", "-o", "-", vmnetPlist)
	if hostErr != nil || maskErr != nil {
		readErr := hostErr
		if readErr == nil {
			readErr = maskErr
		}
		return "", fmt.Sprintf("macOS Internet Sharing is active, but the subnet it uses could not be read: %v", readErr)
	}
	if strings.TrimSpace(string(maskResult.Stdout)) != "255.255.255.0" {
		return "", "macOS Internet Sharing is active, but the subnet it uses is not a /24 Farrow can check"
	}
	layout, err := subnet.FromHostAddress(strings.TrimSpace(string(hostResult.Stdout)))
	if err != nil {
		return "", "macOS Internet Sharing is active, but the subnet it uses could not be parsed"
	}
	if layout.CIDR() != request.Layout.CIDR() {
		return "", ""
	}
	return fmt.Sprintf("macOS Internet Sharing is active and uses %s", layout.CIDR()), ""
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

func (p Probe) collectDarwin(ctx context.Context, request Request) Snapshot {
	snapshot := Snapshot{Addresses: make(map[string]string), Installation: Installation{Status: "absent"}}
	required := []struct {
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
	present := 0
	for _, target := range required {
		info, err := p.lstat(target.path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				invalidate(&snapshot.Installation, target.path+": "+err.Error())
			}
			continue
		}
		present++
		if metadataErr := rootMetadata(info, target.mode, target.kind, target.group, target.sticky); metadataErr != nil {
			invalidate(&snapshot.Installation, target.path+": "+metadataErr.Error())
		}
	}
	if present != 0 && present != len(required) && snapshot.Installation.Status != "invalid" {
		snapshot.Installation.Status = "partial"
		snapshot.Installation.Problem = fmt.Sprintf("only %d of %d installed network files are present", present, len(required))
	}
	if present == 0 && snapshot.Installation.Status == "absent" {
		if _, err := p.lstat(darwinnet.LogDir); err == nil {
			snapshot.Installation.Status = "partial"
			snapshot.Installation.Problem = darwinnet.LogDir + " exists but the Farrow network is not installed"
		} else if !errors.Is(err, os.ErrNotExist) {
			invalidate(&snapshot.Installation, darwinnet.LogDir+": "+err.Error())
		}
	}
	socketExists, socketMetadataOK := false, false
	socketProblem := ""
	if socketInfo, socketErr := p.lstat(darwinnet.SocketPath); socketErr == nil {
		socketExists = true
		if metadataErr := rootMetadata(socketInfo, 0o770, "socket", 20, false); metadataErr != nil {
			socketProblem = darwinnet.SocketPath + ": " + metadataErr.Error()
		} else {
			socketMetadataOK = true
		}
	} else if !errors.Is(socketErr, os.ErrNotExist) {
		socketProblem = darwinnet.SocketPath + ": " + socketErr.Error()
	}
	if present == 0 && socketExists && snapshot.Installation.Status == "absent" {
		snapshot.Installation.Status = "partial"
		snapshot.Installation.Problem = "a vmnet socket exists but the Farrow network is not installed"
	}
	interfacesResult, interfacesErr := p.Runner.Run(ctx, "/sbin/ifconfig")
	if interfacesErr != nil {
		snapshot.Problems = append(snapshot.Problems, "ifconfig: "+interfacesErr.Error())
	} else {
		snapshot.Interfaces = parseDarwinInterfaces(string(interfacesResult.Stdout))
	}
	if present == len(required) && snapshot.Installation.Status != "invalid" {
		arguments, err := p.Runner.Run(ctx, "/usr/bin/plutil", "-extract", "ProgramArguments", "json", "-o", "-", darwinnet.PlistPath)
		if err != nil {
			invalidate(&snapshot.Installation, "cannot read Darwin launch arguments: "+err.Error())
		} else {
			var exactArgs []string
			decodeErr := json.Unmarshal(arguments.Stdout, &exactArgs)
			mode, interfaceID, gateway, dhcp, parseErr := plistArguments(arguments.Stdout)
			layout, layoutErr := subnet.FromHostAddress(gateway)
			if parseErr != nil || layoutErr != nil || dhcp != layout.DHCPEnd() {
				invalidate(&snapshot.Installation, "Darwin launch arguments do not describe a valid Farrow /24")
			} else {
				protectedState := false
				protectedInterfaceState := false
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
				if interfaceReadErr != nil || interfaceParseErr != nil {
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
				if interfaceReadErr == nil && interfaceParseErr == nil && interfaceMarker.Source != "" {
					expectedSocketSHA, expectedClientSHA = interfaceMarker.SocketSHA256, interfaceMarker.ClientSHA256
				}
				for _, binary := range []struct{ path, digest string }{{darwinnet.DaemonPath, expectedSocketSHA}, {darwinnet.ClientPath, expectedClientSHA}} {
					data, readErr := p.readFile(binary.path)
					if releaseErr != nil || readErr != nil || len(data) == 0 || len(data) > 64<<20 || digestBytes(data) != binary.digest {
						invalidate(&snapshot.Installation, binary.path+": installed digest differs from the recorded provenance")
					}
				}
				if stateInfo, stateErr := p.lstat(darwinnet.StatePath); stateErr == nil {
					if metadataErr := rootMetadata(stateInfo, 0o600, "file", 0, false); metadataErr != nil {
						invalidate(&snapshot.Installation, darwinnet.StatePath+": "+metadataErr.Error())
					} else if stateData, readErr := p.readFile(darwinnet.StatePath); readErr != nil {
						invalidate(&snapshot.Installation, darwinnet.StatePath+": "+readErr.Error())
					} else if state, stateErr := darwinnet.StrictNetworkState(stateData); stateErr != nil || state != plan.State {
						invalidate(&snapshot.Installation, darwinnet.StatePath+": protected state differs from the pinned network plan")
					}
				} else if errors.Is(stateErr, os.ErrNotExist) {
					invalidate(&snapshot.Installation, darwinnet.StatePath+": protected network state is missing")
				} else if protectedInspectionError(stateErr) {
					protectedState = true
				} else {
					invalidate(&snapshot.Installation, darwinnet.StatePath+": "+stateErr.Error())
				}
				if interfaceStateInfo, interfaceStateErr := p.lstat(darwinnet.InterfaceStatePath); interfaceStateErr == nil {
					if metadataErr := rootMetadata(interfaceStateInfo, 0o600, "file", 0, false); metadataErr != nil {
						invalidate(&snapshot.Installation, darwinnet.InterfaceStatePath+": "+metadataErr.Error())
					} else if protectedData, readErr := p.readFile(darwinnet.InterfaceStatePath); readErr != nil || !bytes.Equal(protectedData, interfaceData) {
						invalidate(&snapshot.Installation, darwinnet.InterfaceStatePath+": protected interface identity differs from the public root-owned marker")
					}
				} else if errors.Is(interfaceStateErr, os.ErrNotExist) {
					invalidate(&snapshot.Installation, darwinnet.InterfaceStatePath+": protected interface identity is missing")
				} else if protectedInspectionError(interfaceStateErr) {
					protectedInterfaceState = true
				} else {
					invalidate(&snapshot.Installation, darwinnet.InterfaceStatePath+": "+interfaceStateErr.Error())
				}
				if snapshot.Installation.Status != "invalid" {
					if protectedState || protectedInterfaceState {
						snapshot.Installation.Status = "protected"
					} else {
						snapshot.Installation.Status = "exact"
					}
				}
				interfaceObserved := false
				if interfaceParseErr == nil && interfaceReadErr == nil {
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
		}
	}
	// Logs are runtime support, not evidence of network ownership. A directory
	// recreated by launchd with mode 0744 must not hide a verified Farrow bridge.
	if snapshot.Installation.Status == "exact" || snapshot.Installation.Status == "protected" {
		if finding := p.darwinLogDirectoryFinding(); finding != nil {
			snapshot.Findings = append(snapshot.Findings, *finding)
		}
	}
	routes, routeErr := p.Runner.Run(ctx, "/usr/sbin/netstat", "-rn", "-f", "inet")
	if routeErr != nil {
		snapshot.Problems = append(snapshot.Problems, "netstat IPv4 route table: "+routeErr.Error())
	} else {
		snapshot.Routes = parseDarwinRoutes(string(routes.Stdout))
	}
	sharingBusy, sharingProblem := p.darwinSharingConflict(ctx, request, snapshot.Installation)
	snapshot.SharingBusy = sharingBusy
	if sharingProblem != "" {
		snapshot.Findings = append(snapshot.Findings, Finding{
			Code: "vmnet.sharing_probe", Severity: Error, Class: Capability,
			Evidence: sharingProblem,
			Fix:      "inspect the vmnet configuration with: sudo plutil -p /Library/Preferences/SystemConfiguration/com.apple.vmnet.plist",
		})
	}
	return snapshot
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
		Fix: "run farrow up to repair it (sudo may ask for a password), or run: " + command,
	}
}

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
	return subnet.Layout{}, errors.New("linux Farrow network unit lacks an exact host .1/24 address")
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
	if !ok || statistics.Uid != 0 || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
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
	return fmt.Sprintf("[Match]\nName=farrow0\n\n[Network]\nAddress=%s/24\nConfigureWithoutCarrier=yes\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n\n[Link]\nRequiredForOnline=no\n", layout.HostAddress())
}

const linuxBridgeMarker = "# BEGIN FARROW MANAGED: farrow0\nallow farrow0\n# END FARROW MANAGED: farrow0\n"

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

func (p Probe) collectLinux(ctx context.Context, request Request) Snapshot {
	snapshot := Snapshot{Addresses: make(map[string]string), Installation: Installation{Status: "absent"}}
	stateDir := filepath.Dir(linuxnet.StatePath)
	anchorPaths := []string{linuxnet.NetDevPath, linuxnet.NetworkPath, linuxnet.NetworkManagerPath, linuxnet.TmpfilesPath, linuxnet.PublicStatePath, stateDir, linuxnet.LeaseRoot}
	anchors := 0
	for _, path := range anchorPaths {
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
	nmShape := false
	if _, err := p.lstat(linuxnet.PublicStatePath); err == nil {
		nmShape = true
	}
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
		snapshot.Installation.Problem = fmt.Sprintf("partial Linux Farrow network installation: %d/%d required public paths", present, len(required))
	}
	addresses, addressErr := p.Runner.Run(ctx, "/usr/sbin/ip", "-4", "-o", "address", "show")
	if addressErr != nil {
		snapshot.Problems = append(snapshot.Problems, "ip address show: "+addressErr.Error())
	} else {
		snapshot.Interfaces = parseLinuxInterfaces(string(addresses.Stdout))
	}
	routes, routeErr := p.Runner.Run(ctx, "/usr/sbin/ip", "-4", "route", "show", "table", "all")
	if routeErr != nil {
		snapshot.Problems = append(snapshot.Problems, "ip route show: "+routeErr.Error())
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
		invalidate(&snapshot.Installation, linuxnet.BridgeConfPath+": exact Farrow marker block is missing or duplicated")
	}
	osRelease, familyErr := p.readFile("/etc/os-release")
	family, parseFamilyErr := linuxFamily(osRelease)
	if familyErr != nil || parseFamilyErr != nil {
		invalidate(&snapshot.Installation, "cannot identify the installed Linux network family")
	} else {
		snapshot.Installation.Family = string(family)
		helper := ""
		var helperInfo os.FileInfo
		for _, candidate := range []string{"/usr/lib/qemu/qemu-bridge-helper", "/usr/libexec/qemu-bridge-helper"} {
			if candidateInfo, candidateErr := p.lstat(candidate); candidateErr == nil {
				helper, helperInfo = candidate, candidateInfo
				break
			}
		}
		if helper == "" {
			invalidate(&snapshot.Installation, "installed Linux network lacks qemu-bridge-helper")
		} else if helperErr := validateLinuxHelper(helperInfo, family); helperErr != nil {
			invalidate(&snapshot.Installation, helper+": "+helperErr.Error())
		} else if parentErr := p.safeRootOwnedParents(helper); parentErr != nil {
			invalidate(&snapshot.Installation, helper+": "+parentErr.Error())
		} else {
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
				invalidate(&snapshot.Installation, helper+": package ownership or reversible privilege policy is invalid")
			} else {
				snapshot.Installation.HelperPath = helper
			}
		}
	}
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
				if readErr != nil || len(content) > 1<<20 || digestBytes(content) != expectedDigest {
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

func (p Probe) verifyLinuxNetworkdInstallation(ctx context.Context, snapshot *Snapshot) {
	data, err := p.readFile(linuxnet.NetworkPath)
	layout, layoutErr := linuxNetworkLayout(data)
	if err != nil || layoutErr != nil {
		invalidate(&snapshot.Installation, "cannot parse owned Linux Farrow network unit")
		return
	}
	snapshot.Installation.Mode = "bridge"
	snapshot.Installation.CIDR = layout.CIDR()
	snapshot.Installation.HostAddress = layout.HostAddress()
	snapshot.Installation.Interface = linuxnet.BridgeName
	for _, exact := range []struct {
		path    string
		content string
	}{
		{linuxnet.NetDevPath, "[NetDev]\nName=farrow0\nKind=bridge\n"},
		{linuxnet.NetworkPath, exactLinuxNetwork(layout)},
	} {
		if fileErr := p.exactPublicFile(exact.path, []byte(exact.content), 1<<20); fileErr != nil {
			invalidate(&snapshot.Installation, exact.path+": "+fileErr.Error())
		}
	}
	if managerInfo, managerErr := p.lstat(linuxnet.NetworkManagerPath); managerErr == nil {
		if metadataErr := rootMetadata(managerInfo, 0o644, "file", 0, false); metadataErr != nil {
			invalidate(&snapshot.Installation, linuxnet.NetworkManagerPath+": "+metadataErr.Error())
		} else if fileErr := p.exactPublicFile(linuxnet.NetworkManagerPath, []byte("[keyfile]\nunmanaged-devices=interface-name:farrow0\n"), 1<<20); fileErr != nil {
			invalidate(&snapshot.Installation, linuxnet.NetworkManagerPath+": "+fileErr.Error())
		}
	} else if !errors.Is(managerErr, os.ErrNotExist) {
		invalidate(&snapshot.Installation, linuxnet.NetworkManagerPath+": "+managerErr.Error())
	}
	protectedState := p.verifyLinuxOwnedCommon(ctx, snapshot, layout, linuxnet.BackendNetworkd)
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
	active, activeErr := p.Runner.Run(ctx, "/usr/bin/systemctl", "is-active", "systemd-networkd.service")
	link, linkErr := p.Runner.Run(ctx, "/usr/sbin/ip", "-d", "link", "show", "dev", linuxnet.BridgeName)
	snapshot.Installation.Healthy = addressReady && activeErr == nil && strings.TrimSpace(string(active.Stdout)) == "active" && linkErr == nil && strings.Contains(string(link.Stdout), "bridge")
	if !snapshot.Installation.Healthy && snapshot.Installation.Problem == "" {
		snapshot.Installation.Problem = "owned Linux network exists but networkd, farrow0 bridge type, or configured host .1/24 is not ready"
	}
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
	snapshot.Installation.Mode = "bridge"
	snapshot.Installation.CIDR = layout.CIDR()
	snapshot.Installation.HostAddress = layout.HostAddress()
	snapshot.Installation.Interface = linuxnet.BridgeName
	// The unmanaged drop-in belongs to the networkd backend; its presence under
	// the NetworkManager backend is stale foreign state.
	if _, managerErr := p.lstat(linuxnet.NetworkManagerPath); managerErr == nil {
		invalidate(&snapshot.Installation, linuxnet.NetworkManagerPath+": stale networkd-backend drop-in present under the NetworkManager backend")
	} else if !errors.Is(managerErr, os.ErrNotExist) {
		invalidate(&snapshot.Installation, linuxnet.NetworkManagerPath+": "+managerErr.Error())
	}
	protectedState := p.verifyLinuxOwnedCommon(ctx, snapshot, layout, linuxnet.BackendNetworkManager)
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
	active, activeErr := p.Runner.Run(ctx, "/usr/bin/systemctl", "is-active", "NetworkManager.service")
	link, linkErr := p.Runner.Run(ctx, "/usr/sbin/ip", "-d", "link", "show", "dev", linuxnet.BridgeName)
	snapshot.Installation.Healthy = addressReady && activeErr == nil && strings.TrimSpace(string(active.Stdout)) == "active" && linkErr == nil && strings.Contains(string(link.Stdout), "bridge")
	if !snapshot.Installation.Healthy && snapshot.Installation.Problem == "" {
		snapshot.Installation.Problem = "owned Linux network exists but NetworkManager, farrow0 bridge type, or configured host .1/24 is not ready"
	}
}
