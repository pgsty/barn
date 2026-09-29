package darwin

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/network/subnet"
)

type Executor struct {
	// InUse, when set, refuses uninstall while the deployment still has
	// live or recorded VMs.
	InUse         func(context.Context) error
	User          execx.Runner
	Root          execx.Runner
	StagingParent string
}

var ErrVMNetSharingBusy error = failure.New(failure.Resource, errors.New("macOS vmnet is busy: another sharing service holds the requested subnet (Internet Sharing, Docker Desktop, UTM, or another VM app)")).Because("vmnet_busy").Then("quit that app, or choose another /24 with barn setup -c <cidr>")

type InstallReport struct {
	Action   string            `json:"action"`
	Plan     InstallPlan       `json:"plan"`
	Targets  map[string]string `json:"targets"`
	Applied  bool              `json:"applied"`
	Checks   map[string]string `json:"checks,omitempty"`
	Warnings []string          `json:"warnings,omitempty"`
}

type UninstallReport struct {
	State       NetworkState      `json:"state"`
	Interface   InterfaceMarker   `json:"interface"`
	RemoveFiles []string          `json:"remove_files"`
	RemoveDirs  []string          `json:"remove_dirs"`
	Applied     bool              `json:"applied"`
	Recovered   bool              `json:"recovered_from_public_evidence,omitempty"`
	Checks      map[string]string `json:"checks,omitempty"`
}

func (e Executor) validate() error {
	if e.User == nil || e.Root == nil {
		return errors.New("darwin network executor requires user and privileged runners")
	}
	return nil
}

func digestFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 4<<20 {
		return "", fmt.Errorf("network file is unsafe: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (e Executor) rootStat(ctx context.Context, path, owner, group, mode, kind string) error {
	result, err := e.Root.Run(ctx, "/usr/bin/stat", "-f", "%Su:%Sg:%Lp:%Sp:%HT", path)
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimSpace(string(result.Stdout)), ":")
	permission := mode
	if mode == "1777" {
		permission = "777"
	}
	if len(parts) != 5 || parts[0] != owner || parts[1] != group || parts[2] != permission || parts[4] != kind || (mode == "1777" && !strings.HasSuffix(parts[3], "t")) {
		return fmt.Errorf("darwin network path metadata mismatch for %s: got %q want owner=%s group=%s mode=%s kind=%s", path, strings.TrimSpace(string(result.Stdout)), owner, group, mode, kind)
	}
	return nil
}

func (e Executor) readInstalled(ctx context.Context) (InstallPlan, bool, error) {
	paths := []string{DaemonPath, ClientPath, PlistPath, StateDir, InterfaceMarkerDir, InterfaceMarkerPath}
	present := 0
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			present++
		} else if !errors.Is(err, os.ErrNotExist) {
			return InstallPlan{}, false, err
		}
	}
	if present == 0 {
		return InstallPlan{}, false, nil
	}
	if present != len(paths) {
		return InstallPlan{}, false, fmt.Errorf("the Barn network is partially installed: %d of %d required files are present", present, len(paths))
	}
	stateResult, err := e.Root.Run(ctx, "/bin/cat", StatePath)
	if err != nil {
		return InstallPlan{}, false, err
	}
	state, err := StrictNetworkState(stateResult.Stdout)
	if err != nil {
		return InstallPlan{}, false, err
	}
	interfaceStateResult, err := e.Root.Run(ctx, "/bin/cat", InterfaceStatePath)
	if err != nil {
		return InstallPlan{}, false, err
	}
	interfaceMarkerResult, err := e.Root.Run(ctx, "/bin/cat", InterfaceMarkerPath)
	if err != nil {
		return InstallPlan{}, false, err
	}
	plan, err := NewInstallPlanModeNetwork(state.Arch, state.InterfaceID, state.Mode, state.CIDR)
	if err == nil {
		plan, err = plan.WithRecordedBinaries(state.Source, state.SocketSHA256, state.ClientSHA256)
	}
	if err != nil || plan.State != state {
		return InstallPlan{}, false, errors.New("installed Darwin network state does not reproduce the pinned plan")
	}
	plan, err = bindInterfaceEvidence(plan, interfaceStateResult.Stdout, interfaceMarkerResult.Stdout)
	if err != nil {
		return InstallPlan{}, false, err
	}
	plist, err := plan.Plist()
	if err != nil {
		return InstallPlan{}, false, err
	}
	actualPlist, err := os.ReadFile(PlistPath)
	if err != nil || !reflect.DeepEqual(actualPlist, plist) {
		return InstallPlan{}, false, errors.New("installed Darwin launchd plist differs from the pinned plan")
	}
	stateJSON, err := plan.StateJSON()
	if err != nil || !reflect.DeepEqual(stateResult.Stdout, stateJSON) {
		return InstallPlan{}, false, errors.New("installed Darwin network state bytes differ from the pinned plan")
	}
	daemonDigest, err := digestFile(DaemonPath)
	if err != nil || daemonDigest != plan.SocketDigest() {
		return InstallPlan{}, false, errors.New("installed socket_vmnet digest mismatch")
	}
	clientDigest, err := digestFile(ClientPath)
	if err != nil || clientDigest != plan.ClientDigest() {
		return InstallPlan{}, false, errors.New("installed socket_vmnet_client digest mismatch")
	}
	for _, target := range []struct{ path, owner, group, mode, kind string }{
		{DaemonPath, "root", "wheel", "755", "Regular File"},
		{ClientPath, "root", "wheel", "755", "Regular File"},
		{PlistPath, "root", "wheel", "644", "Regular File"},
		{StateDir, "root", "wheel", "700", "Directory"},
		{StatePath, "root", "wheel", "600", "Regular File"},
		{InterfaceStatePath, "root", "wheel", "600", "Regular File"},
		{InterfaceMarkerDir, "root", "wheel", "755", "Directory"},
		{InterfaceMarkerPath, "root", "wheel", "644", "Regular File"},
	} {
		if err := e.rootStat(ctx, target.path, target.owner, target.group, target.mode, target.kind); err != nil {
			return InstallPlan{}, false, err
		}
	}
	return plan, true, nil
}

// recoverPlanFromInterfaceEvidence reconstructs only the ownership plan needed
// for uninstall when network.json is missing or unreadable. Recovery remains
// fail-closed: the public marker and root-only twin must be byte-identical,
// the launchd plist must equal one current pinned plan, and both installed
// binaries must match that plan's digests.
func recoverPlanFromInterfaceEvidence(arch string, protectedMarker, publicMarker, actualPlist []byte, socketDigest, clientDigest string) (InstallPlan, error) {
	if !bytes.Equal(protectedMarker, publicMarker) {
		return InstallPlan{}, errors.New("darwin recovery interface evidence differs between protected and public copies")
	}
	marker, err := StrictInterfaceMarker(publicMarker)
	if err != nil {
		return InstallPlan{}, err
	}
	for _, mode := range []string{"host", "shared"} {
		plan, planErr := NewInstallPlanModeNetwork(arch, marker.InterfaceID, mode, marker.CIDR)
		if planErr == nil {
			plan, planErr = plan.WithRecordedBinaries(marker.Source, marker.SocketSHA256, marker.ClientSHA256)
		}
		if planErr == nil {
			plan, planErr = plan.WithBSDInterface(marker.BSDName)
		}
		if planErr != nil || plan.Interface != marker {
			continue
		}
		expectedPlist, plistErr := plan.Plist()
		if plistErr == nil && bytes.Equal(actualPlist, expectedPlist) && socketDigest == plan.SocketDigest() && clientDigest == plan.ClientDigest() {
			return plan, nil
		}
	}
	return InstallPlan{}, errors.New("darwin partial network state does not reproduce a current pinned ownership plan")
}

func (e Executor) recoverInstalledForUninstall(ctx context.Context) (InstallPlan, error) {
	for _, target := range []struct{ path, owner, group, mode, kind string }{
		{DaemonPath, "root", "wheel", "755", "Regular File"},
		{ClientPath, "root", "wheel", "755", "Regular File"},
		{PlistPath, "root", "wheel", "644", "Regular File"},
		{StateDir, "root", "wheel", "700", "Directory"},
		{InterfaceStatePath, "root", "wheel", "600", "Regular File"},
		{InterfaceMarkerDir, "root", "wheel", "755", "Directory"},
		{InterfaceMarkerPath, "root", "wheel", "644", "Regular File"},
	} {
		if err := e.rootStat(ctx, target.path, target.owner, target.group, target.mode, target.kind); err != nil {
			return InstallPlan{}, err
		}
	}
	protected, err := e.Root.Run(ctx, "/bin/cat", InterfaceStatePath)
	if err != nil {
		return InstallPlan{}, err
	}
	public, err := os.ReadFile(InterfaceMarkerPath)
	if err != nil {
		return InstallPlan{}, err
	}
	plist, err := os.ReadFile(PlistPath)
	if err != nil {
		return InstallPlan{}, err
	}
	socketDigest, err := digestFile(DaemonPath)
	if err != nil {
		return InstallPlan{}, err
	}
	clientDigest, err := digestFile(ClientPath)
	if err != nil {
		return InstallPlan{}, err
	}
	return recoverPlanFromInterfaceEvidence(runtime.GOARCH, protected.Stdout, public, plist, socketDigest, clientDigest)
}

func (e Executor) PlanInstall(ctx context.Context, archive, interfaceID, arch string) (InstallReport, error) {
	return e.PlanInstallModeNetwork(ctx, archive, interfaceID, arch, "host", subnet.DefaultCIDR)
}

func (e Executor) PlanInstallMode(ctx context.Context, archive, interfaceID, arch, mode string) (InstallReport, error) {
	return e.PlanInstallModeNetwork(ctx, archive, interfaceID, arch, mode, subnet.DefaultCIDR)
}

// installOrigin selects where the socket_vmnet bytes come from: exactly one
// of the pinned release archive or prebuilt local (Homebrew) binaries.
type installOrigin struct {
	archive  string
	binaries *LocalBinaries
}

func (o installOrigin) provided() bool {
	return o.archive != "" || o.binaries != nil
}

func (e Executor) PlanInstallModeNetwork(ctx context.Context, archive, interfaceID, arch, mode, cidr string) (InstallReport, error) {
	return e.planInstall(ctx, installOrigin{archive: archive}, interfaceID, arch, mode, cidr)
}

// PlanInstallFromHomebrew plans an installation whose binaries are copied
// from a version-matched Homebrew keg; their digests are recorded in the
// network state and public marker for all later verification.
func (e Executor) PlanInstallFromHomebrew(ctx context.Context, binaries LocalBinaries, interfaceID, arch, mode, cidr string) (InstallReport, error) {
	return e.planInstall(ctx, installOrigin{binaries: &binaries}, interfaceID, arch, mode, cidr)
}

func (e Executor) planInstall(ctx context.Context, origin installOrigin, interfaceID, arch, mode, cidr string) (InstallReport, error) {
	if err := e.validate(); err != nil {
		return InstallReport{}, err
	}
	layout, err := subnet.Parse(cidr)
	if err != nil {
		return InstallReport{}, err
	}
	warnings := make([]string, 0, 1)
	if warning := layout.Warning(); warning != "" {
		warnings = append(warnings, warning)
	}
	installedPlan, installed, err := e.readInstalled(ctx)
	if err != nil {
		return InstallReport{}, err
	}
	if installed {
		if installedPlan.State.Mode != mode {
			return InstallReport{}, fmt.Errorf("installed Darwin vmnet mode is %s, requested %s; uninstall before switching", installedPlan.State.Mode, mode)
		}
		if installedPlan.State.CIDR != layout.CIDR() {
			return InstallReport{}, fmt.Errorf("installed Darwin vmnet subnet is %s, requested %s; uninstall before switching", installedPlan.State.CIDR, layout.CIDR())
		}
		if interfaceID != "" && interfaceID != installedPlan.State.InterfaceID {
			return InstallReport{}, errors.New("requested interface ID differs from installed Darwin network state")
		}
		if origin.archive != "" {
			if _, err := VerifyArchive(origin.archive, installedPlan.State.Arch); err != nil {
				return InstallReport{}, err
			}
		}
		return InstallReport{Action: "none", Plan: installedPlan, Targets: darwinTargets(), Checks: map[string]string{"installed": "exact pinned bytes and metadata verified"}, Warnings: warnings}, nil
	}
	if !origin.provided() || interfaceID == "" {
		return InstallReport{}, errors.New("fresh Darwin network install requires --archive and --interface-id")
	}
	recordedSocketSHA, recordedClientSHA := "", ""
	if origin.binaries != nil {
		var verifyErr error
		recordedSocketSHA, recordedClientSHA, verifyErr = VerifyLocalBinaries(*origin.binaries)
		if verifyErr != nil {
			return InstallReport{}, verifyErr
		}
	} else if _, err := VerifyArchive(origin.archive, arch); err != nil {
		return InstallReport{}, err
	}
	plan, err := NewInstallPlanModeNetwork(arch, interfaceID, mode, layout.CIDR())
	if err != nil {
		return InstallReport{}, err
	}
	if origin.binaries != nil {
		plan, err = plan.WithRecordedBinaries(SourceHomebrew, recordedSocketSHA, recordedClientSHA)
		if err != nil {
			return InstallReport{}, err
		}
	}
	if err := e.validateSharedInstallRoot(ctx); err != nil {
		return InstallReport{}, err
	}
	for _, path := range []string{StateDir, InterfaceMarkerDir, LogDir, PlistPath, SocketPath, PIDPath} {
		if _, err := os.Lstat(path); err == nil {
			return InstallReport{}, fmt.Errorf("will not overwrite %s: it exists and Barn did not create it", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return InstallReport{}, err
		}
	}
	return InstallReport{Action: "install", Plan: plan, Targets: darwinTargets(), Warnings: warnings}, nil
}

func (e Executor) validateSharedInstallRoot(ctx context.Context) error {
	if _, err := os.Lstat(InstallRoot); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := e.rootStat(ctx, InstallRoot, "root", "wheel", "755", "Directory"); err != nil {
		return err
	}
	entries, err := e.listRootDir(ctx, InstallRoot)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0] != "libexec" {
		return errors.New("will not install into /opt/barn: it already contains files Barn did not create")
	}
	libexec := filepath.Join(InstallRoot, "libexec")
	if err := e.rootStat(ctx, libexec, "root", "wheel", "755", "Directory"); err != nil {
		return err
	}
	entries, err = e.listRootDir(ctx, libexec)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0] != filepath.Base(HostsHelperPath) {
		return errors.New("will not install into /opt/barn/libexec: it contains files Barn did not create")
	}
	return e.rootStat(ctx, HostsHelperPath, "root", "wheel", "755", "Regular File")
}

func darwinTargets() map[string]string {
	return map[string]string{
		DaemonPath: "root:wheel 0755", ClientPath: "root:wheel 0755",
		PlistPath: "root:wheel 0644", StatePath: "root:wheel 0600",
		InterfaceStatePath: "root:wheel 0600", InterfaceMarkerPath: "root:wheel 0644",
		StateDir: "root:wheel 0700", InterfaceMarkerDir: "root:wheel 0755", LogDir: "root:wheel 0755",
	}
}

func exactHostInterfaces(data []byte, layout subnet.Layout) (map[string]struct{}, error) {
	if len(data) == 0 || len(data) > 1<<20 {
		return nil, errors.New("darwin ifconfig output size is invalid")
	}
	result := make(map[string]struct{})
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	current := ""
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			separator := strings.IndexByte(line, ':')
			if separator <= 0 || !bsdInterfacePattern.MatchString(line[:separator]) {
				current = ""
				continue
			}
			current = line[:separator]
			continue
		}
		if current == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "inet" || fields[1] != layout.HostAddress() {
			continue
		}
		for index := 2; index+1 < len(fields); index++ {
			if fields[index] == "netmask" && isIPv4Slash24Mask(fields[index+1]) {
				result[current] = struct{}{}
				break
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func isIPv4Slash24Mask(value string) bool {
	if strings.HasPrefix(value, "0x") {
		parsed, err := strconv.ParseUint(strings.TrimPrefix(value, "0x"), 16, 32)
		return err == nil && parsed == 0xffffff00
	}
	address, err := netip.ParseAddr(value)
	return err == nil && address.Is4() && address.String() == "255.255.255.0"
}

func newExactHostInterfaces(before, after map[string]struct{}) []string {
	result := make([]string, 0)
	for name := range after {
		if _, existed := before[name]; !existed {
			result = append(result, name)
		}
	}
	sort.Strings(result)
	return result
}

func (e Executor) snapshotExactHostInterfaces(ctx context.Context, layout subnet.Layout) (map[string]struct{}, error) {
	result, err := e.User.Run(ctx, "/sbin/ifconfig")
	if err != nil {
		return nil, err
	}
	return exactHostInterfaces(result.Stdout, layout)
}

func (e Executor) waitNewExactHostInterface(ctx context.Context, layout subnet.Layout, before map[string]struct{}) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		after, err := e.snapshotExactHostInterfaces(ctx, layout)
		if err != nil {
			return "", err
		}
		created := newExactHostInterfaces(before, after)
		switch len(created) {
		case 1:
			return created[0], nil
		case 0:
			// The socket can accept before vmnet has published its BSD interface.
		default:
			return "", fmt.Errorf("socket_vmnet created multiple exact %s host interfaces: %s", layout.CIDR(), strings.Join(created, ", "))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return "", fmt.Errorf("socket_vmnet did not create exactly one new interface with %s/24", layout.HostAddress())
}

func (e Executor) waitSocket(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		info, err := os.Lstat(SocketPath)
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			connection, dialErr := net.DialTimeout("unix", SocketPath, time.Second)
			if dialErr == nil {
				_ = connection.Close()
				return nil
			}
		}
		logResult, _ := e.Root.Run(ctx, "/usr/bin/tail", "-n", "80", filepath.Join(LogDir, "stderr.log"))
		if strings.Contains(string(logResult.Stdout), "[1009]") {
			return ErrVMNetSharingBusy
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("socket_vmnet did not open its socket within 30s; see " + filepath.Join(LogDir, "stderr.log"))
}

func (e Executor) Install(ctx context.Context, archive, interfaceID, arch string, apply bool) (InstallReport, error) {
	return e.InstallModeNetwork(ctx, archive, interfaceID, arch, "host", subnet.DefaultCIDR, apply)
}

func (e Executor) InstallMode(ctx context.Context, archive, interfaceID, arch, mode string, apply bool) (InstallReport, error) {
	return e.InstallModeNetwork(ctx, archive, interfaceID, arch, mode, subnet.DefaultCIDR, apply)
}

func (e Executor) InstallModeNetwork(ctx context.Context, archive, interfaceID, arch, mode, cidr string, apply bool) (InstallReport, error) {
	return e.install(ctx, installOrigin{archive: archive}, interfaceID, arch, mode, cidr, apply)
}

// InstallFromHomebrew copies the formula's binaries into the root-owned
// install root instead of extracting the pinned release archive. Root never
// executes the user-writable brew keg itself: the bytes are staged, digested,
// installed under root ownership, and re-verified before launchd starts them.
func (e Executor) InstallFromHomebrew(ctx context.Context, binaries LocalBinaries, interfaceID, arch, mode, cidr string, apply bool) (InstallReport, error) {
	return e.install(ctx, installOrigin{binaries: &binaries}, interfaceID, arch, mode, cidr, apply)
}

func (e Executor) install(ctx context.Context, origin installOrigin, interfaceID, arch, mode, cidr string, apply bool) (report InstallReport, retErr error) {
	report, retErr = e.planInstall(ctx, origin, interfaceID, arch, mode, cidr)
	if retErr != nil || !apply || report.Action == "none" {
		return report, retErr
	}
	layout, err := subnet.Parse(report.Plan.State.CIDR)
	if err != nil {
		return report, err
	}
	parent := e.StagingParent
	if parent == "" {
		parent = os.TempDir()
	}
	staging, err := os.MkdirTemp(parent, "barn-darwin-network-")
	if err != nil {
		return report, err
	}
	defer func() {
		if err := os.RemoveAll(staging); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove Darwin network staging: %w", err))
		}
	}()
	if err := os.Chmod(staging, 0o700); err != nil {
		return report, err
	}
	if origin.binaries != nil {
		if err := StageVerifiedLocalBinaries(*origin.binaries, staging, report.Plan.State.SocketSHA256, report.Plan.State.ClientSHA256); err != nil {
			return report, err
		}
	} else if err := ExtractVerifiedBinaries(origin.archive, arch, staging); err != nil {
		return report, err
	}
	plist, err := report.Plan.Plist()
	if err != nil {
		return report, err
	}
	state, err := report.Plan.StateJSON()
	if err != nil {
		return report, err
	}
	plistSource := filepath.Join(staging, "launchd.plist")
	stateSource := filepath.Join(staging, "network.json")
	if err := os.WriteFile(plistSource, plist, 0o600); err != nil {
		return report, err
	}
	if err := os.WriteFile(stateSource, state, 0o600); err != nil {
		return report, err
	}
	createdDirs := make([]string, 0, 6)
	bootstrapped := false
	rollback := true
	defer func() {
		if !rollback || retErr == nil {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if rollbackErr := e.rollbackFreshInstall(rollbackCtx, bootstrapped, createdDirs); rollbackErr != nil {
			retErr = fmt.Errorf("%w; scoped partial-install rollback also failed: %v", retErr, rollbackErr)
			return
		}
		retErr = fmt.Errorf("%w; scoped partial install was rolled back", retErr)
	}()
	for _, directory := range []struct{ path, mode string }{
		{InstallRoot, "0755"}, {filepath.Join(InstallRoot, "libexec"), "0755"},
		{StateDir, "0700"}, {InterfaceMarkerDir, "0755"}, {LogDir, "0755"},
	} {
		_, statErr := os.Lstat(directory.path)
		wasMissing := errors.Is(statErr, os.ErrNotExist)
		if statErr != nil && !wasMissing {
			return report, statErr
		}
		_, installErr := e.Root.Run(ctx, "/usr/bin/install", "-d", "-o", "root", "-g", "wheel", "-m", directory.mode, directory.path)
		if wasMissing {
			if _, createdErr := os.Lstat(directory.path); createdErr == nil {
				createdDirs = append(createdDirs, directory.path)
			}
		}
		if installErr != nil {
			return report, installErr
		}
	}
	for _, file := range []struct{ source, target, mode string }{
		{filepath.Join(staging, "socket_vmnet"), DaemonPath, "0755"},
		{filepath.Join(staging, "socket_vmnet_client"), ClientPath, "0755"},
		{plistSource, PlistPath, "0644"}, {stateSource, StatePath, "0600"},
	} {
		if _, err := e.Root.Run(ctx, "/usr/bin/install", "-o", "root", "-g", "wheel", "-m", file.mode, file.source, file.target); err != nil {
			return report, err
		}
	}
	before, err := e.snapshotExactHostInterfaces(ctx, layout)
	if err != nil {
		return report, fmt.Errorf("capture exact Darwin host interfaces before launch: %w", err)
	}
	if _, err := e.Root.Run(ctx, "/bin/launchctl", "bootstrap", "system", PlistPath); err != nil {
		return report, err
	}
	bootstrapped = true
	if _, err := e.Root.Run(ctx, "/bin/launchctl", "enable", "system/"+ServiceID); err != nil {
		return report, err
	}
	if _, err := e.Root.Run(ctx, "/bin/launchctl", "kickstart", "-k", "system/"+ServiceID); err != nil {
		return report, err
	}
	if err := e.waitSocket(ctx); err != nil {
		return report, fmt.Errorf("darwin network did not become ready: %w", err)
	}
	bsdName, err := e.waitNewExactHostInterface(ctx, layout, before)
	if err != nil {
		return report, fmt.Errorf("prove newly-created Darwin host interface ownership: %w", err)
	}
	report.Plan, err = report.Plan.WithBSDInterface(bsdName)
	if err != nil {
		return report, err
	}
	interfaceJSON, err := report.Plan.InterfaceJSON()
	if err != nil {
		return report, err
	}
	interfaceSource := filepath.Join(staging, "network-interface.json")
	if err := os.WriteFile(interfaceSource, interfaceJSON, 0o600); err != nil {
		return report, err
	}
	for _, target := range []struct{ path, mode string }{{InterfaceStatePath, "0600"}, {InterfaceMarkerPath, "0644"}} {
		if _, err := e.Root.Run(ctx, "/usr/bin/install", "-o", "root", "-g", "wheel", "-m", target.mode, interfaceSource, target.path); err != nil {
			return report, err
		}
	}
	verified, installed, err := e.readInstalled(ctx)
	if err != nil {
		return report, fmt.Errorf("darwin network post-install verification failed: %w", err)
	}
	if !installed || verified.State != report.Plan.State || verified.Interface != report.Plan.Interface {
		return report, errors.New("darwin network post-install verification did not reproduce installed ownership state")
	}
	rollback = false
	report.Applied = true
	report.Checks = map[string]string{
		"interface": report.Plan.Interface.BSDName + " newly created with exact " + report.Plan.State.HostAddress + "/24",
		"launchd":   "running", "socket": "root-owned and accepting",
		"state": "exact pinned plan and byte-identical public/protected interface identity",
	}
	return report, nil
}

func (e Executor) rollbackFreshInstall(ctx context.Context, bootstrapped bool, createdDirs []string) error {
	var rollbackErrors []error
	if bootstrapped {
		if _, err := e.Root.Run(ctx, "/bin/launchctl", "bootout", "system/"+ServiceID); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("bootout: %w", err))
		}
	}
	for _, path := range []string{
		SocketPath, PIDPath, DaemonPath, ClientPath, PlistPath, StatePath, InterfaceStatePath,
		InterfaceMarkerPath, filepath.Join(LogDir, "stdout.log"), filepath.Join(LogDir, "stderr.log"),
	} {
		if _, err := e.Root.Run(ctx, "/bin/rm", "-f", path); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	for index := len(createdDirs) - 1; index >= 0; index-- {
		if _, err := e.Root.Run(ctx, "/bin/rmdir", createdDirs[index]); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("remove directory %s: %w", createdDirs[index], err))
		}
	}
	return errors.Join(rollbackErrors...)
}

func (e Executor) listRootDir(ctx context.Context, path string) ([]string, error) {
	result, err := e.Root.Run(ctx, "/bin/ls", "-1A", path)
	if err != nil {
		return nil, err
	}
	entries := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(result.Stdout)), "\n") {
		if line != "" {
			entries = append(entries, line)
		}
	}
	return entries, nil
}

func (e Executor) PlanUninstall(ctx context.Context) (UninstallReport, error) {
	if err := e.validate(); err != nil {
		return UninstallReport{}, err
	}
	plan, installed, err := e.readInstalled(ctx)
	recovered := false
	if err != nil {
		var recoveryErr error
		plan, recoveryErr = e.recoverInstalledForUninstall(ctx)
		if recoveryErr != nil {
			return UninstallReport{}, fmt.Errorf("read installed Darwin network state: %w; recover from interface evidence: %v", err, recoveryErr)
		}
		installed = true
		recovered = true
	}
	if !installed {
		return UninstallReport{}, errors.New("the Barn network is not installed")
	}
	processes, err := e.User.Run(ctx, "/bin/ps", "-axo", "comm=,command=")
	if err != nil {
		return UninstallReport{}, err
	}
	for _, line := range strings.Split(string(processes.Stdout), "\n") {
		if strings.Contains(line, "qemu-system") && strings.Contains(line, SocketPath) {
			return UninstallReport{}, failure.New(failure.Conflict, errors.New("will not remove the Barn network while VMs are using it")).Then("stop the VMs that use it (barn stop for Barn nodes), then retry")
		}
	}
	hostsHelperPresent := false
	if _, err := os.Lstat(HostsHelperPath); err == nil {
		if err := e.rootStat(ctx, HostsHelperPath, "root", "wheel", "755", "Regular File"); err != nil {
			return UninstallReport{}, err
		}
		hostsHelperPresent = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return UninstallReport{}, err
	}
	residueChecks := map[string]map[string]struct{}{
		InstallRoot:                           {"libexec": {}},
		filepath.Join(InstallRoot, "libexec"): {"socket_vmnet": {}, "socket_vmnet_client": {}, "barn-hosts-helper": {}},
		StateDir:                              {"network.json": {}, "network-interface.json": {}},
		InterfaceMarkerDir:                    {"network-interface.json": {}},
		LogDir:                                {"stdout.log": {}, "stderr.log": {}},
	}
	for directory, allowed := range residueChecks {
		entries, err := e.listRootDir(ctx, directory)
		if err != nil {
			return UninstallReport{}, err
		}
		for _, entry := range entries {
			if _, ok := allowed[entry]; !ok {
				return UninstallReport{}, fmt.Errorf("will not remove %s: it contains %s, which Barn did not create", directory, entry)
			}
		}
	}
	for _, path := range []string{filepath.Join(LogDir, "stdout.log"), filepath.Join(LogDir, "stderr.log")} {
		if err := e.rootStat(ctx, path, "root", "wheel", "644", "Regular File"); err != nil {
			return UninstallReport{}, err
		}
	}
	files := []string{
		DaemonPath, ClientPath, PlistPath, StatePath, InterfaceStatePath, InterfaceMarkerPath,
		filepath.Join(LogDir, "stdout.log"), filepath.Join(LogDir, "stderr.log"),
	}
	dirs := []string{StateDir, InterfaceMarkerDir, LogDir}
	if !hostsHelperPresent {
		dirs = append([]string{filepath.Join(InstallRoot, "libexec"), InstallRoot}, dirs...)
	}
	return UninstallReport{State: plan.State, Interface: plan.Interface, RemoveFiles: files, RemoveDirs: dirs, Recovered: recovered}, nil
}

func (e Executor) rootUnlinkIfExists(ctx context.Context, path string) error {
	// The caller cannot distinguish absent from inaccessible below a mode-0700
	// root directory. rm -f on one exact validated path is non-recursive and
	// makes partial-state cleanup idempotent without broadening the target.
	_, err := e.Root.Run(ctx, "/bin/rm", "-f", "--", path)
	return err
}

func (e Executor) Uninstall(ctx context.Context, apply bool) (UninstallReport, error) {
	report, err := e.PlanUninstall(ctx)
	if err != nil || !apply {
		return report, err
	}
	if e.InUse != nil {
		if err := e.InUse(ctx); err != nil {
			return report, fmt.Errorf("will not remove the Barn network: %w", err)
		}
	}
	if _, err := e.PlanUninstall(ctx); err != nil {
		return report, err
	}
	if _, err := e.Root.Run(ctx, "/bin/launchctl", "bootout", "system/"+ServiceID); err != nil {
		return report, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(SocketPath); errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(time.Second)
	}
	for _, path := range append([]string{SocketPath, PIDPath}, report.RemoveFiles...) {
		if err := e.rootUnlinkIfExists(ctx, path); err != nil {
			return report, err
		}
	}
	for _, directory := range report.RemoveDirs {
		if _, err := e.Root.Run(ctx, "/bin/rmdir", directory); err != nil {
			return report, err
		}
	}
	report.Applied = true
	report.Checks = map[string]string{"removed": "launchd, pinned files, public/protected interface identity, logs, state, lock and empty owned directories"}
	return report, nil
}
