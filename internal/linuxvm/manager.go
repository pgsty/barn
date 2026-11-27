package linuxvm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/activity"
	"github.com/pgsty/barn/internal/disk"
	"github.com/pgsty/barn/internal/execx"
	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/lock"
	"github.com/pgsty/barn/internal/network/portalloc"
	netpreflight "github.com/pgsty/barn/internal/network/preflight"
	"github.com/pgsty/barn/internal/network/subnet"
	"github.com/pgsty/barn/internal/platform"
	"github.com/pgsty/barn/internal/qmp"
	"github.com/pgsty/barn/internal/spec"
	"github.com/pgsty/barn/internal/sshkeys"
	"github.com/pgsty/barn/internal/state"
	"github.com/pgsty/barn/internal/vm"
)

type ImageResolver func(context.Context, string, string) (image.Entry, string, image.Metadata, error)
type ImageLookup func(context.Context, string, string) (image.Entry, error)
type HostPreflightFunc func(context.Context, platform.Profile, *spec.PrivateNetwork, execx.Runner) (Backend, error)
type NetworkPreflightFunc func(context.Context, platform.Profile, netpreflight.Request, execx.Runner) netpreflight.Report

type NetworkPreflightError struct{ Report netpreflight.Report }

func (e *NetworkPreflightError) Error() string {
	for _, finding := range e.Report.Findings {
		if finding.Severity == netpreflight.Error {
			message := finding.Evidence
			if finding.Fix != "" {
				message += "; fix: " + finding.Fix
			}
			return message
		}
	}
	return "private network preflight failed"
}

type Manager struct {
	retryGuestSetup     bool
	BarnVersion         string
	OperationID         string
	Runner              execx.Runner
	ReadyTimeout        time.Duration
	Parallelism         *int
	Repository          string
	Progress            activity.Reporter
	NoWait              bool
	RollbackFailed      bool
	ResolveImage        ImageResolver
	LookupImage         ImageLookup
	imageSession        *image.CatalogSession
	HostPreflight       HostPreflightFunc
	NetworkPreflight    NetworkPreflightFunc
	NativeProfile       func() (platform.Profile, error)
	LookPath            func(string) (string, error)
	FirmwareLookup      func(platform.Profile, string) (platform.Firmware, error)
	DialSSHAddress      func(string, string) (net.Conn, error)
	Nodes               []string
	ImmediateStop       bool
	allowPartialDestroy bool
	dropFromSpec        bool
}

func (m Manager) report(phase, message string) {
	m.Progress.Report(activity.Event{Phase: phase, Message: message})
}

func (m Manager) runner() execx.Runner {
	if m.Runner != nil {
		return m.Runner
	}
	return execx.OSRunner{Timeout: 15 * time.Second, OutputLimit: 1 << 20}
}

func (m Manager) nativeProfile() (platform.Profile, error) {
	if m.NativeProfile != nil {
		return m.NativeProfile()
	}
	return platform.Native()
}

func (m Manager) lookPath(name string) (string, error) {
	if m.LookPath != nil {
		return m.LookPath(name)
	}
	return exec.LookPath(name)
}

func (m Manager) firmwareForBoot(profile platform.Profile, boot string) (platform.Firmware, error) {
	if m.FirmwareLookup != nil {
		return m.FirmwareLookup(profile, boot)
	}
	return platform.FindFirmwareForBoot(profile, boot)
}

func (m Manager) readyTimeout(resolved spec.Resolved) (time.Duration, error) {
	if m.ReadyTimeout < 0 {
		return 0, errors.New("manager readiness timeout must be positive")
	}
	if m.ReadyTimeout > 0 {
		return m.ReadyTimeout, nil
	}
	return resolved.SSHWaitTimeout()
}

// nodesWithoutCommittedState filters to the nodes that have no committed
// state yet — the only ones whose fixed addresses must still be silent. A
// node this deployment already runs would otherwise probe as a conflict.
func nodesWithoutCommittedState(nodes []spec.Node) []spec.Node {
	root, err := state.ResolveDataRoot()
	if err != nil {
		return nodes
	}
	store := state.Store{Root: root}
	result := make([]spec.Node, 0, len(nodes))
	for _, node := range nodes {
		if _, readErr := store.ReadNode(node.Name); readErr == nil {
			continue
		}
		result = append(result, node)
	}
	return result
}

func selectedNodeNames(resolved spec.Resolved, requested []string) ([]string, error) {
	if len(requested) == 0 {
		result := make([]string, 0, len(resolved.Nodes))
		for _, node := range resolved.Nodes {
			result = append(result, node.Name)
		}
		return result, nil
	}
	known := make(map[string]struct{}, len(resolved.Nodes))
	for _, node := range resolved.Nodes {
		known[node.Name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(requested))
	result := make([]string, 0, len(requested))
	for _, name := range requested {
		if _, ok := known[name]; !ok {
			return nil, UnknownNodeError(name, "deployment", resolved.Nodes)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, failure.New(failure.Usage, fmt.Errorf("node selection repeats %q", name))
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result, nil
}

func nodeNameSet(names []string) map[string]struct{} {
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		result[name] = struct{}{}
	}
	return result
}

func selectedNodeList(names []string, selected map[string]struct{}) []string {
	result := make([]string, 0, len(names))
	for _, name := range names {
		if _, include := selected[name]; include {
			result = append(result, name)
		}
	}
	return result
}

func committedNodeNames(store state.Store, resolved spec.Resolved, names []string) ([]string, error) {
	selected, err := selectedNodeNames(resolved, names)
	if err != nil {
		return nil, err
	}
	explicit := len(names) != 0
	result := make([]string, 0, len(selected))
	for _, name := range selected {
		if _, err := store.ReadNode(name); err == nil {
			result = append(result, name)
		} else if missingPath(err) && !explicit {
			continue
		} else if missingPath(err) {
			return nil, failure.New(failure.Conflict, fmt.Errorf("node %s has not been created", name)).Then("barn up " + name)
		} else {
			return nil, err
		}
	}
	if len(result) == 0 {
		return nil, errors.New("the deployment has no committed nodes")
	}
	return result, nil
}

func (m Manager) preflight(ctx context.Context, profile platform.Profile, resolved spec.Resolved) (Backend, error) {
	if resolved.Private == nil {
		return Backend{}, &CapabilityError{Reason: "host preflight requires a resolved network"}
	}
	layout, layoutErr := subnet.Parse(resolved.Private.CIDR)
	if layoutErr != nil {
		return Backend{}, &CapabilityError{Reason: layoutErr.Error()}
	}
	addresses := make([]string, 0, len(resolved.Nodes))
	for _, node := range nodesWithoutCommittedState(resolved.Nodes) {
		addresses = append(addresses, node.Address)
	}
	request := netpreflight.Request{OS: profile.OS, Arch: profile.Arch, Purpose: netpreflight.Use, Layout: layout, Addresses: addresses}
	var report netpreflight.Report
	if m.NetworkPreflight != nil {
		report = m.NetworkPreflight(ctx, profile, request, m.runner())
	} else {
		report = netpreflight.Run(ctx, request, netpreflight.Probe{Runner: m.runner()})
	}
	if !report.Ready {
		return Backend{}, &NetworkPreflightError{Report: report}
	}
	var backend Backend
	var err error
	if m.HostPreflight != nil {
		backend, err = m.HostPreflight(ctx, profile, resolved.Private, m.runner())
	} else {
		backend, err = PreflightHost(ctx, profile, resolved.Private, m.runner())
	}
	if err != nil {
		return Backend{}, err
	}
	return backend, nil
}

func missingPath(err error) bool {
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	var pathError *os.PathError
	return errors.As(err, &pathError) && errors.Is(pathError.Err, os.ErrNotExist)
}

func (m Manager) openDeployment(create bool) (Deployment, error) {
	return openDeployment(create)
}

func cloneNode(node spec.Node) spec.Node {
	result := node
	result.Aliases = append([]string(nil), node.Aliases...)
	result.Disks = append([]spec.Disk(nil), node.Disks...)
	result.Forwards = append([]spec.Forward(nil), node.Forwards...)
	result.Shares = append([]spec.Share(nil), node.Shares...)
	return result
}

func cloneResolved(value spec.Resolved) spec.Resolved {
	result := value
	if value.Private != nil {
		privateNetwork := *value.Private
		result.Private = &privateNetwork
	}
	result.Nodes = make([]spec.Node, len(value.Nodes))
	for index, node := range value.Nodes {
		result.Nodes[index] = cloneNode(node)
	}
	return result
}

func resolvedNodeSelection(value spec.Resolved, names []string) spec.Resolved {
	selected := nodeNameSet(names)
	result := cloneResolved(value)
	result.Nodes = result.Nodes[:0]
	for _, node := range value.Nodes {
		if _, include := selected[node.Name]; include {
			result.Nodes = append(result.Nodes, cloneNode(node))
		}
	}
	return result
}

func allocateMissingSSHPorts(value spec.Resolved, createNames []string, reserved map[uint16]struct{}, store state.Store) (map[string]uint16, error) {
	return allocateMissingSSHPortsWithProbe(value, createNames, reserved, store, func(bind string, port uint16) bool {
		listener, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(int(port))))
		if err != nil {
			return false
		}
		_ = listener.Close()
		return true
	})
}

func allocateMissingSSHPortsWithProbe(value spec.Resolved, createNames []string, reserved map[uint16]struct{}, store state.Store, probe func(string, uint16) bool) (map[string]uint16, error) {
	if len(createNames) == 0 {
		return allocateSSHPortsWithProbe(value, reserved, probe)
	}
	createSet := nodeNameSet(createNames)
	ports := make(map[string]uint16, len(value.Nodes))
	available := func(bind string, port uint16) bool {
		if _, exists := reserved[port]; exists {
			return false
		}
		return probe(bind, port)
	}
	for _, definition := range value.Nodes {
		if _, create := createSet[definition.Name]; create {
			continue
		}
		node, err := store.ReadNode(definition.Name)
		if missingPath(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		ports[definition.Name] = node.SSHPort
		reserved[node.SSHPort] = struct{}{}
	}
	for index, definition := range value.Nodes {
		if _, create := createSet[definition.Name]; !create {
			continue
		}
		port, err := portalloc.Choose(uint16(2222+index), func(port uint16) bool { return available("127.0.0.1", port) })
		if err != nil {
			return nil, fmt.Errorf("allocate management SSH for recreated node %s: %w", definition.Name, err)
		}
		reserved[port] = struct{}{}
		ports[definition.Name] = port
	}
	return ports, nil
}

func allocateSSHPortsWithProbe(value spec.Resolved, reserved map[uint16]struct{}, probe func(string, uint16) bool) (map[string]uint16, error) {
	selected := make(map[uint16]struct{}, len(value.Nodes))
	available := func(port uint16) bool {
		if _, exists := reserved[port]; exists {
			return false
		}
		if _, exists := selected[port]; exists {
			return false
		}
		return probe("127.0.0.1", port)
	}
	sshPorts := make(map[string]uint16, len(value.Nodes))
	for index, definition := range value.Nodes {
		port, err := portalloc.Choose(uint16(2222+index), available)
		if err != nil {
			return nil, fmt.Errorf("allocate management SSH for %s: %w", definition.Name, err)
		}
		selected[port] = struct{}{}
		sshPorts[definition.Name] = port
	}
	return sshPorts, nil
}

func aliases(value spec.Resolved) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for _, node := range value.Nodes {
		alias := node.Image
		if alias == "" {
			alias = value.Image
		}
		if _, exists := seen[alias]; !exists {
			seen[alias] = struct{}{}
			result = append(result, alias)
		}
	}
	return result
}

func canonicalReferenceImage(value string) string {
	ref, err := image.ParseReference(value)
	if err != nil {
		return value
	}
	return image.CanonicalAlias(ref.Image)
}

func ensureSSHAddressesUnused(nodes []spec.Node) error {
	return ensureSSHAddressesUnusedWithDial(nodes, func(network, address string) (net.Conn, error) {
		return net.DialTimeout(network, address, 500*time.Millisecond)
	})
}

func (m Manager) ensureSSHAddressesUnused(nodes []spec.Node) error {
	if m.DialSSHAddress != nil {
		return ensureSSHAddressesUnusedWithDial(nodes, m.DialSSHAddress)
	}
	return ensureSSHAddressesUnused(nodes)
}

func ensureSSHAddressesUnusedWithDial(nodes []spec.Node, dial func(string, string) (net.Conn, error)) error {
	for _, node := range nodes {
		connection, err := dial("tcp", net.JoinHostPort(node.Address, "22"))
		if err != nil {
			continue
		}
		_ = connection.Close()
		return &CapabilityError{Reason: fmt.Sprintf("private address %s for node %s already accepts SSH before start; stop the conflicting VM or service", node.Address, node.Name)}
	}
	return nil
}

func (m Manager) imageDataRoot() (string, error) {
	return state.ResolveDataRoot()
}

// ensureImageSession opens the local image catalog once per command so every
// node resolves against the same revision. Tests may inject ResolveImage and
// LookupImage instead; the catalog is never read from the network here.
func (m *Manager) ensureImageSession() error {
	if m.imageSession != nil || (m.ResolveImage != nil && m.LookupImage != nil) {
		return nil
	}
	dataRoot, err := m.imageDataRoot()
	if err != nil {
		return err
	}
	service := image.Service{DataRoot: dataRoot, Repository: m.Repository, Runner: m.Runner, Progress: m.Progress}
	session, err := service.OpenCatalog()
	if err != nil {
		return err
	}
	m.imageSession = session
	return nil
}

func (m Manager) resolveOneImage(ctx context.Context, alias, arch string) (image.Entry, string, image.Metadata, error) {
	if m.ResolveImage != nil {
		return m.ResolveImage(ctx, alias, arch)
	}
	if m.imageSession == nil {
		return image.Entry{}, "", image.Metadata{}, errors.New("image catalog was not opened before resolving images")
	}
	return m.imageSession.ResolveArch(ctx, alias, arch)
}

func (m Manager) lookupOneImage(ctx context.Context, alias, arch string) (image.Entry, error) {
	if m.LookupImage != nil {
		return m.LookupImage(ctx, alias, arch)
	}
	if m.imageSession == nil {
		return image.Entry{}, errors.New("image catalog was not opened before resolving images")
	}
	return m.imageSession.LookupArch(ctx, alias, arch)
}

func mergeBootMode(profile platform.Profile, current, alias, candidate string) (string, error) {
	if current != "" && current != candidate {
		return "", errors.New("a deployment does not mix BIOS and UEFI images")
	}
	if profile.RequiresUEFI && candidate != "uefi" {
		return "", &CapabilityError{Reason: fmt.Sprintf("image %s boot=%s is incompatible with required UEFI guest architecture", alias, candidate)}
	}
	return candidate, nil
}

func (m Manager) resolveBases(ctx context.Context, profile platform.Profile, value spec.Resolved) (map[string]BaseImage, string, error) {
	bases := make(map[string]BaseImage)
	boot := ""
	for _, alias := range aliases(value) {
		entry, path, metadata, err := m.resolveOneImage(ctx, alias, profile.Arch)
		if err != nil {
			return nil, "", err
		}
		boot, err = mergeBootMode(profile, boot, alias, entry.Boot)
		if err != nil {
			return nil, "", err
		}
		base := BaseImage{Path: path, Alias: entry.Alias, Release: entry.Release, Digest: entry.SHA256, VirtualSize: metadata.VirtualSize}
		bases[alias] = base
		if _, exists := bases[entry.Alias]; !exists {
			bases[entry.Alias] = base
		}
	}
	return bases, boot, nil
}

type runtimeDecision struct {
	Profile platform.Profile
	Reason  string
}

func selectRuntime(host platform.Profile, value spec.Resolved) (runtimeDecision, error) {
	guestArch, err := platform.GuestArch(value.Arch, host.Arch)
	if err != nil {
		return runtimeDecision{}, err
	}
	imageNames := aliases(value)
	if len(imageNames) == 0 && value.Image != "" {
		imageNames = []string{value.Image}
	}
	hasEL7, hasEL8 := false, false
	for _, name := range imageNames {
		canonical := canonicalReferenceImage(name)
		hasEL7 = hasEL7 || canonical == "el7"
		hasEL8 = hasEL8 || canonical == "el8"
	}
	if hasEL7 && (host.OS != "linux" || host.Arch != "amd64" || guestArch != "amd64") {
		return runtimeDecision{}, &CapabilityError{Reason: "EL7 is deliberately supported only on native Linux/amd64"}
	}
	emulate := guestArch != host.Arch
	reason := "guest architecture differs from the host"
	if hasEL8 && host.OS == "darwin" && host.Arch == "arm64" && guestArch == "arm64" {
		emulate = true
		reason = "stock EL8 arm64 requires a 64K translation granule unavailable through Apple HVF"
	}
	profile, err := platform.ResolveRuntime(host, guestArch, emulate)
	if err != nil {
		return runtimeDecision{}, err
	}
	if !profile.Emulated {
		reason = ""
	}
	return runtimeDecision{Profile: profile, Reason: reason}, nil
}

func (m Manager) resolveRuntimeQEMU(ctx context.Context, profile platform.Profile, backend Backend) (string, Backend, error) {
	qemuPath, err := platform.FindQEMUBinary(profile, m.lookPath)
	if err != nil {
		return "", Backend{}, err
	}
	if !profile.Emulated {
		return qemuPath, backend, nil
	}
	result, err := m.runner().Run(ctx, qemuPath, "--version")
	if err != nil {
		return "", Backend{}, fmt.Errorf("probe selected QEMU version: %w", err)
	}
	version, err := platform.ValidateQEMUVersion(profile, string(result.Stdout)+string(result.Stderr))
	if err != nil {
		return "", Backend{}, err
	}
	if profile.OS != "darwin" {
		return qemuPath, backend, nil
	}
	if backend.DarwinSocket == "" {
		return "", Backend{}, errors.New("selected Darwin QEMU runtime has no socket_vmnet backend")
	}
	help, err := m.runner().Run(ctx, qemuPath, "-machine", "none", "-netdev", "help")
	if err != nil {
		return "", Backend{}, fmt.Errorf("probe selected QEMU network backend: %w", err)
	}
	selected := selectDarwinBackend(version, string(help.Stdout)+string(help.Stderr), backend.DarwinSocket)
	selected.NetworkCIDR, selected.HostAddress, selected.DHCPEnd = backend.NetworkCIDR, backend.HostAddress, backend.DHCPEnd
	if backend.DarwinUseFD {
		selected.DarwinUseFD = true
		selected.ReconnectMS = 0
	}
	return qemuPath, selected, nil
}

func runtimeDriftNodes(store state.Store, resolved spec.Resolved, expected platform.Profile) ([]string, error) {
	drifted := make([]string, 0)
	for _, definition := range resolved.Nodes {
		node, err := store.ReadNode(definition.Name)
		if missingPath(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		arch := invocationGuestArch(node.Invocation.Binary, node.Invocation.Args)
		accel := invocationOption(node.Invocation.Args, "-accel")
		if arch != expected.Arch || accel != expected.Accelerator {
			drifted = append(drifted, definition.Name)
		}
	}
	return drifted, nil
}

func committedNodeUUIDs(store state.Store, resolved spec.Resolved, creating []string) (map[string]string, error) {
	createSet := nodeNameSet(creating)
	result := make(map[string]string)
	for _, definition := range resolved.Nodes {
		if _, create := createSet[definition.Name]; create {
			continue
		}
		node, err := store.ReadNode(definition.Name)
		if missingPath(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result[node.Node] = node.VMUUID
	}
	return result, nil
}

func (m Manager) ensureKeys(ctx context.Context, deploymentValue Deployment) (_ string, _ string, _ string, returnErr error) {
	deploymentLock, err := acquireDeploymentLock(ctx, deploymentValue.Root, false, m.Progress)
	if err != nil {
		return "", "", "", err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, deploymentLock, "deployment key initialization lock")
	}()
	store := state.Store{Root: deploymentValue.Root}
	if deploymentState, err := store.ReadDeployment(); err == nil {
		for _, definition := range deploymentState.Resolved.Nodes {
			if _, err := store.ReadNode(definition.Name); err == nil {
				return sshkeys.EnsureExistingKeys(ctx, m.runner(), deploymentValue.Root)
			} else if !missingPath(err) {
				return "", "", "", err
			}
		}
	} else if !missingPath(err) {
		return "", "", "", err
	}
	return sshkeys.EnsureKeys(ctx, m.runner(), deploymentValue.Root)
}

// preflightBoot resolves startup dependencies before reload/recreate stops a VM.
// Only new/replaced nodes need image downloads; existing disks remain usable offline.
func (m *Manager) preflightBoot(ctx context.Context, requested spec.Resolved, creating []string) (runtimeDecision, string, error) {
	selected, err := selectedNodeNames(requested, m.Nodes)
	if err != nil {
		return runtimeDecision{}, "", err
	}
	profile, err := m.nativeProfile()
	if err != nil {
		return runtimeDecision{}, "", err
	}
	backend, err := m.preflight(ctx, profile, resolvedNodeSelection(requested, selected))
	if err != nil {
		return runtimeDecision{}, "", err
	}
	runtime, err := selectRuntime(profile, requested)
	if err != nil {
		return runtimeDecision{}, "", err
	}
	qemuPath, _, err := m.resolveRuntimeQEMU(ctx, runtime.Profile, backend)
	if err != nil {
		return runtimeDecision{}, "", err
	}
	_, boot, err := m.resolveBootImages(ctx, runtime.Profile, requested, creating)
	if err != nil {
		return runtimeDecision{}, "", err
	}
	if len(creating) != 0 {
		if _, _, err := m.resolveBases(ctx, runtime.Profile, resolvedNodeSelection(requested, creating)); err != nil {
			return runtimeDecision{}, "", err
		}
	}
	if _, err := m.firmwareForBoot(runtime.Profile, boot); err != nil {
		return runtimeDecision{}, "", err
	}
	for _, binary := range []string{"qemu-img", "ssh"} {
		if _, err := m.lookPath(binary); err != nil {
			return runtimeDecision{}, "", err
		}
	}
	return runtime, qemuPath, nil
}

// Reload is the stop/boot cycle that re-reads the desired inventory. The
// drift that up would refuse is refused here before any node stops, so a
// configuration that needs recreate or destroy never takes a healthy lab down.
func (m Manager) Reload(ctx context.Context, requested spec.Resolved) (Status, error) {
	if err := validateResolved(requested); err != nil {
		return Status{}, err
	}
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return Status{}, err
	}
	store := state.Store{Root: deploymentValue.Root}
	persisted, err := store.ReadDeployment()
	if err != nil {
		return Status{}, errors.New("the deployment has no valid state")
	}
	requested, err = applyNodeSelection(persisted.Resolved, requested, m.Nodes)
	if err != nil {
		return Status{}, err
	}
	selected, err := selectedNodeNames(requested, m.Nodes)
	if err != nil {
		return Status{}, err
	}
	runtimeRequested, err := selectedOperationResolved(store, requested, selected, len(m.Nodes) != 0)
	if err != nil {
		return Status{}, err
	}
	hasState := func(name string) bool {
		_, readErr := store.ReadNode(name)
		return readErr == nil
	}
	if err := refuseDrift(diffResolved(persisted.Resolved, requested, hasState)); err != nil {
		return Status{}, err
	}
	creating := make([]string, 0)
	for _, name := range selected {
		if !hasState(name) {
			creating = append(creating, name)
		}
	}
	runtime, qemuPath, err := m.preflightBoot(ctx, runtimeRequested, creating)
	if err != nil {
		return Status{}, err
	}
	if drifted, err := runtimeDriftNodes(store, resolvedNodeSelection(requested, selected), runtime.Profile); err != nil {
		return Status{}, err
	} else if len(drifted) != 0 {
		return Status{}, fmt.Errorf("%w: existing node runtime differs for %s", ErrRecreateRequired, strings.Join(drifted, ", "))
	}
	if err := selectedShareSources(deploymentValue, requested, selected); err != nil {
		return Status{}, err
	}
	shareBinaries, err := selectedShareInvocationBinaries(store, persisted.Resolved, selected)
	if err != nil {
		return Status{}, err
	}
	if selectedHasShares(requested, selected) {
		shareBinaries = append(shareBinaries, qemuPath)
	}
	if err := validatePrivateShareDeviceHelp(ctx, m.runner(), shareBinaries); err != nil {
		return Status{}, err
	}
	if err := selectedShareAccess(deploymentValue, requested, selected); err != nil {
		return Status{}, err
	}
	if err := validatePrivatePersistentDesired(deploymentValue, requested); err != nil {
		return Status{}, err
	}
	if err := m.ensureSSHAddressesUnused(nodesWithoutCommittedState(resolvedNodeSelection(requested, selected).Nodes)); err != nil {
		return Status{}, err
	}
	// Only nodes the deployment already records can stop; a selected node
	// that exists solely in the desired inventory is created on the way up.
	stopper := m
	stopper.Nodes = make([]string, 0, len(selected))
	for _, name := range selected {
		if hasState(name) {
			stopper.Nodes = append(stopper.Nodes, name)
		}
	}
	if len(stopper.Nodes) != 0 {
		if _, err := stopper.Stop(ctx); err != nil {
			return Status{}, err
		}
	}
	return m.Up(ctx, requested)
}

type existingUpPlan struct {
	deployment               *Deployment
	requested                spec.Resolved
	runtimeRequested         spec.Resolved
	createNodes              []string
	hadExistingState         bool
	startExistingAfterCreate bool
	startedExisting          *Status
}

func (m Manager) inspectExistingForUp(ctx context.Context, requested spec.Resolved, selected []string, hostProfile platform.Profile) (existingUpPlan, error) {
	plan := existingUpPlan{requested: requested, runtimeRequested: requested}
	existing, openErr := m.openDeployment(false)
	if openErr != nil {
		if !missingPath(openErr) {
			return existingUpPlan{}, openErr
		}
		plan.createNodes = append([]string(nil), selected...)
		if len(m.Nodes) != 0 {
			plan.runtimeRequested = resolvedNodeSelection(requested, selected)
		}
		return plan, nil
	}
	store := state.Store{Root: existing.Root}
	persisted, err := store.ReadDeployment()
	if missingPath(err) {
		plan.deployment = &existing
		plan.createNodes = append([]string(nil), selected...)
		if len(m.Nodes) != 0 {
			plan.runtimeRequested = resolvedNodeSelection(requested, selected)
		}
		return plan, nil
	}
	if err != nil {
		return existingUpPlan{}, err
	}
	plan.hadExistingState = true
	requested, err = applyNodeSelection(persisted.Resolved, requested, m.Nodes)
	if err != nil {
		return existingUpPlan{}, err
	}
	runtimeRequested, err := selectedOperationResolved(store, requested, selected, len(m.Nodes) != 0)
	if err != nil {
		return existingUpPlan{}, err
	}
	hasState := func(name string) bool {
		_, readErr := store.ReadNode(name)
		return readErr == nil
	}
	if err := refuseDrift(diffResolved(persisted.Resolved, requested, hasState)); err != nil {
		return existingUpPlan{}, err
	}
	selectedSet := nodeNameSet(selected)
	allRunning, allRunnable := true, true
	for _, definition := range requested.Nodes {
		if _, include := selectedSet[definition.Name]; !include {
			continue
		}
		node, err := store.ReadNode(definition.Name)
		if missingPath(err) {
			plan.createNodes = append(plan.createNodes, definition.Name)
			allRunning = false
			continue
		}
		if err != nil {
			return existingUpPlan{}, err
		}
		plan.startExistingAfterCreate = true
		if node.Phase != state.Running && node.Phase != state.Stopped && node.Phase != state.Prepared {
			recovering := m
			recovering.Nodes = []string{node.Node}
			if _, err := recovering.statusFor(ctx, existing, ""); err != nil {
				return existingUpPlan{}, err
			}
			node, err = store.ReadNode(definition.Name)
			if err != nil {
				return existingUpPlan{}, err
			}
			if node.Phase != state.Running && node.Phase != state.Stopped && node.Phase != state.Prepared {
				return existingUpPlan{}, interruptedPhaseError(node)
			}
		}
		allRunning = allRunning && node.Phase == state.Running
		allRunnable = allRunnable && (node.Phase == state.Running || node.Phase == state.Stopped || node.Phase == state.Prepared)
	}
	plan.requested = requested
	plan.runtimeRequested = runtimeRequested
	if len(plan.createNodes) != 0 {
		plan.deployment = &existing
		return plan, nil
	}
	if allRunning || allRunnable {
		// A partial start returns the per-node status together with its error.
		status, err := m.startExisting(ctx, existing, persisted, hostProfile)
		plan.startedExisting = &status
		return plan, err
	}
	return plan, nil
}

func (m Manager) Up(ctx context.Context, requested spec.Resolved) (_ Status, returnErr error) {
	m.retryGuestSetup = true
	m.report("preflight", "Checking the fixed-IP network and QEMU capabilities")
	if err := validateResolved(requested); err != nil {
		return Status{}, err
	}
	selected, err := selectedNodeNames(requested, m.Nodes)
	if err != nil {
		return Status{}, err
	}
	selectedRequested := resolvedNodeSelection(requested, selected)
	for _, node := range selectedRequested.Nodes {
		m.Progress.Report(activity.Event{
			Phase: "guest-ready", Node: node.Name, Address: node.Address, State: "queued",
			Message: "waiting for host preparation",
		})
	}
	hostProfile, err := m.nativeProfile()
	if err != nil {
		return Status{}, err
	}
	backend, err := m.preflight(ctx, hostProfile, selectedRequested)
	if err != nil {
		return Status{}, err
	}
	m.Progress.Report(activity.Event{Phase: "preflight", Message: "Network and QEMU preflight passed", Done: true})

	upPlan, err := m.inspectExistingForUp(ctx, requested, selected, hostProfile)
	if upPlan.startedExisting != nil {
		return *upPlan.startedExisting, err
	}
	if err != nil {
		return Status{}, err
	}
	requested = upPlan.requested
	createNodes := upPlan.createNodes

	if err := m.ensureSSHAddressesUnused(nodesWithoutCommittedState(selectedRequested.Nodes)); err != nil {
		return Status{}, err
	}
	runtime, err := selectRuntime(hostProfile, upPlan.runtimeRequested)
	if err != nil {
		return Status{}, err
	}
	if upPlan.hadExistingState && upPlan.deployment != nil {
		drifted, driftErr := runtimeDriftNodes(state.Store{Root: upPlan.deployment.Root}, resolvedNodeSelection(requested, selected), runtime.Profile)
		if driftErr != nil {
			return Status{}, driftErr
		}
		if len(drifted) != 0 {
			return Status{}, fmt.Errorf("%w: existing node runtime differs for %s", ErrRecreateRequired, strings.Join(drifted, ", "))
		}
	}
	if runtime.Profile.Emulated {
		m.report("runtime", fmt.Sprintf("Using TCG compatibility mode for %s: %s; functional results are valid, performance results are not", runtime.Profile.Arch, runtime.Reason))
	}
	qemuPath, backend, err := m.resolveRuntimeQEMU(ctx, runtime.Profile, backend)
	if err != nil {
		return Status{}, err
	}
	_, boot, err := m.resolveBootImages(ctx, runtime.Profile, upPlan.runtimeRequested, createNodes)
	if err != nil {
		return Status{}, err
	}
	m.report("image-resolve", fmt.Sprintf("Resolving images for %d node(s)", len(createNodes)))
	bases, _, err := m.resolveBases(ctx, runtime.Profile, resolvedNodeSelection(requested, createNodes))
	if err != nil {
		return Status{}, err
	}
	if selectedHasShares(requested, selected) {
		if err := validatePrivateShareDeviceHelp(ctx, m.runner(), []string{qemuPath}); err != nil {
			return Status{}, err
		}
	}
	qemuImg, err := m.lookPath("qemu-img")
	if err != nil {
		return Status{}, err
	}
	sshPath, err := m.lookPath("ssh")
	if err != nil {
		return Status{}, err
	}
	deploymentValue := Deployment{}
	if upPlan.deployment != nil {
		deploymentValue = *upPlan.deployment
	} else {
		deploymentValue, err = m.openDeployment(true)
		if err != nil {
			return Status{}, err
		}
	}
	if err := validatePrivatePersistentDesired(deploymentValue, requested); err != nil {
		return Status{}, err
	}
	allocator, err := waitForLock(ctx, filepath.Join(deploymentValue.Root, "locks", "allocator.lock"), false, m.Progress)
	if err != nil {
		return Status{}, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, allocator, "deployment allocator lock")
	}()
	resolved := cloneResolved(requested)
	sshPorts, err := allocateMissingSSHPorts(resolved, createNodes, make(map[uint16]struct{}), state.Store{Root: deploymentValue.Root})
	if err != nil {
		return Status{}, err
	}
	specHash, err := spec.Hash(resolved)
	if err != nil {
		return Status{}, err
	}
	nodeHashes, err := spec.NodeHashes(resolved)
	if err != nil {
		return Status{}, err
	}
	privateKeyPath, knownHosts, publicKey, err := m.ensureKeys(ctx, deploymentValue)
	if err != nil {
		return Status{}, err
	}
	privateKey, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return Status{}, err
	}
	knownUUIDs := make(map[string]string)
	if upPlan.hadExistingState {
		knownUUIDs, err = committedNodeUUIDs(state.Store{Root: deploymentValue.Root}, resolved, createNodes)
		if err != nil {
			return Status{}, err
		}
	}
	plan, err := Build(resolved, os.Getuid(), knownUUIDs, nil)
	if err != nil {
		return Status{}, err
	}
	generations := make(map[string]uint64, len(resolved.Nodes))
	for _, node := range resolved.Nodes {
		generations[node.Name] = 1
	}
	seedInput := SeedInput{PublicKey: publicKey, PrivateKey: string(privateKey), SpecHashes: nodeHashes, Generation: generations}
	seeds, err := RenderSeeds(resolved, plan, seedInput)
	if err != nil {
		return Status{}, err
	}
	firmware, err := m.firmwareForBoot(runtime.Profile, boot)
	if err != nil {
		return Status{}, err
	}
	prepare := PrepareConfig{
		DeploymentRoot: deploymentValue.Root, Resolved: resolved, SpecHash: specHash, NodeHashes: nodeHashes, Plan: plan, Seeds: seeds, SeedInput: &seedInput, Bases: bases, SSHPorts: sshPorts,
		Profile: runtime.Profile, QEMUBinary: qemuPath, Firmware: firmware, UseUEFI: boot == "uefi", Backend: backend,
		Disks: disk.Manager{QEMUImg: qemuImg, Runner: m.runner()},
	}
	lifecycle := NativeLifecycle{RetryGuestSetup: m.retryGuestSetup, Resolved: resolved, VM: vm.Lifecycle{Runner: m.runner(), QMP: &qmp.Client{Timeout: 5 * time.Second}, SSHUser: resolved.SSHUser}, Deployment: deploymentValue, Shares: shareSourcesByNode(resolved), SSHPath: sshPath, PrivateKey: privateKeyPath, KnownHosts: knownHosts, DarwinSocket: backend.DarwinSocket}
	readyTimeout, err := m.readyTimeout(resolved)
	if err != nil {
		return Status{}, err
	}
	startNodes := selected
	if len(createNodes) != 0 {
		startNodes = append([]string(nil), createNodes...)
	}
	controller := Controller{Deployment: deploymentValue, Prepare: prepare, Lifecycle: lifecycle, Concurrency: m.concurrency(len(resolved.Nodes)), ReadyTimeout: readyTimeout, NoWait: m.NoWait, CreateNodes: createNodes, StartNodes: startNodes, Version: m.BarnVersion, Progress: m.Progress}
	createResult, err := controller.CreateAndStart(ctx)
	if err != nil && m.RollbackFailed {
		err = rollbackCreateFailure(deploymentValue, createResult, err)
	}
	// A node-scoped prepare/start failure does not invalidate committed peers.
	// Only continue after the controller released its lock cleanly, and never
	// replay newly started nodes or continue through cancellation/global errors.
	if partial, partialCreate := err.(*PartialError); upPlan.startExistingAfterCreate && ctx.Err() == nil && (err == nil || partialCreate) {
		return m.convergeExistingPeers(ctx, deploymentValue, hostProfile, selected, createNodes, createResult, partial, err)
	}
	if err != nil {
		status, statusErr := m.statusFor(ctx, deploymentValue, "")
		return statusWithReadiness(status, createResult.Start), errors.Join(err, statusErr)
	}
	if len(createNodes) != 0 {
		status, err := m.statusFor(ctx, deploymentValue, fmt.Sprintf("created and started %d node(s)", len(createNodes)))
		return statusWithReadiness(status, createResult.Start), err
	}
	status, err := m.statusFor(ctx, deploymentValue, "created and started the deployment")
	return statusWithReadiness(status, createResult.Start), err
}

func (m Manager) convergeExistingPeers(ctx context.Context, deploymentValue Deployment, hostProfile platform.Profile, selected, createNodes []string, createResult CreateResult, partial *PartialError, createErr error) (Status, error) {
	deploymentState, readErr := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
	if readErr != nil {
		return Status{}, errors.Join(createErr, readErr)
	}
	peers := m
	peers.Nodes = nil
	creating := nodeNameSet(createNodes)
	for _, name := range selected {
		if _, isNew := creating[name]; !isNew {
			peers.Nodes = append(peers.Nodes, name)
		}
	}
	peerStatus, peerErr := peers.startExisting(ctx, deploymentValue, deploymentState, hostProfile)
	var err error
	switch {
	case partial != nil:
		if peerPartial := IsolatedPartial(peerErr); peerPartial != nil {
			combined := newPartialError(append(append([]NodeFailure(nil), partial.Failures...), peerPartial.Failures...), len(selected))
			combined.RolledBack = partial.RolledBack
			err = combined
		} else if peerErr == nil {
			combined := newPartialError(partial.Failures, len(selected))
			combined.RolledBack = partial.RolledBack
			err = combined
		} else {
			err = errors.Join(createErr, peerErr)
		}
	case IsolatedPartial(peerErr) != nil:
		// Count every selected node, not only the existing peers.
		err = newPartialError(IsolatedPartial(peerErr).Failures, len(selected))
	default:
		err = peerErr
	}
	status, statusErr := m.statusFor(ctx, deploymentValue, "")
	outcomes := append([]StartOutcome(nil), createResult.Start...)
	for _, node := range peerStatus.Nodes {
		outcomes = append(outcomes, StartOutcome{Node: node.Name, Ready: node.Ready, Error: node.Error, Warnings: node.Warnings, Repairs: node.Repairs})
	}
	if err == nil && statusErr == nil {
		status.Message = fmt.Sprintf("converged the deployment: created %d node(s) and started selected existing nodes", len(createResult.Commit.Nodes))
	}
	return statusWithReadiness(status, outcomes), errors.Join(err, statusErr)
}

func (m Manager) startExisting(ctx context.Context, deploymentValue Deployment, deploymentState state.DeploymentState, profile platform.Profile) (_ Status, returnErr error) {
	m.report("preflight", "Checking the existing deployment before start")
	preStartStore := state.Store{Root: deploymentValue.Root}
	selected, selectionErr := committedNodeNames(preStartStore, deploymentState.Resolved, m.Nodes)
	backend, err := m.preflight(ctx, profile, resolvedNodeSelection(deploymentState.Resolved, selected))
	if err != nil {
		return Status{}, err
	}
	if selectionErr != nil {
		return Status{}, selectionErr
	}
	shareBinaries, err := selectedShareInvocationBinaries(preStartStore, deploymentState.Resolved, selected)
	if err != nil {
		return Status{}, err
	}
	if err := validatePrivateShareDeviceHelp(ctx, m.runner(), shareBinaries); err != nil {
		return Status{}, err
	}
	selectedSet := nodeNameSet(selected)
	for _, definition := range deploymentState.Resolved.Nodes {
		if _, include := selectedSet[definition.Name]; !include {
			continue
		}
		m.Progress.Report(activity.Event{
			Phase: "guest-ready", Node: definition.Name, Address: definition.Address, State: "queued",
			Message: "waiting for host preparation",
		})
	}
	if _, err := m.statusFor(ctx, deploymentValue, ""); err != nil {
		return Status{}, err
	}
	startableDefinitions := make([]spec.Node, 0, len(deploymentState.Resolved.Nodes))
	for _, definition := range deploymentState.Resolved.Nodes {
		if _, include := selectedSet[definition.Name]; !include {
			continue
		}
		node, err := preStartStore.ReadNode(definition.Name)
		if err != nil {
			return Status{}, err
		}
		if node.Phase == state.Prepared || node.Phase == state.Stopped {
			startableDefinitions = append(startableDefinitions, definition)
		}
	}
	if err := m.ensureSSHAddressesUnused(startableDefinitions); err != nil {
		return Status{}, err
	}
	deploymentLock, err := acquireDeploymentLock(ctx, deploymentValue.Root, false, m.Progress)
	if err != nil {
		return Status{}, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, deploymentLock, "deployment start lock")
	}()
	return m.startExistingLocked(ctx, deploymentValue, deploymentState, backend, selected)
}

// The caller holds the deployment lock across disk changes and the subsequent boot.
func (m Manager) startExistingLocked(ctx context.Context, deploymentValue Deployment, deploymentState state.DeploymentState, backend Backend, selected []string) (Status, error) {
	preStartStore := state.Store{Root: deploymentValue.Root}
	selectedSet := nodeNameSet(selected)
	sshPath, err := m.lookPath("ssh")
	if err != nil {
		return Status{}, err
	}
	privateKey, knownHosts, _, err := sshkeys.EnsureExistingKeys(ctx, m.runner(), deploymentValue.Root)
	if err != nil {
		return Status{}, err
	}
	lifecycle := NativeLifecycle{RetryGuestSetup: m.retryGuestSetup, Resolved: deploymentState.Resolved, VM: vm.Lifecycle{Runner: m.runner(), QMP: &qmp.Client{Timeout: 5 * time.Second}, SSHUser: deploymentState.Resolved.SSHUser}, Deployment: deploymentValue, Shares: shareSourcesByNode(deploymentState.Resolved), SSHPath: sshPath, PrivateKey: privateKey, KnownHosts: knownHosts, DarwinSocket: backend.DarwinSocket}
	names := make([]string, 0, len(deploymentState.Resolved.Nodes))
	starting := 0
	for _, definition := range deploymentState.Resolved.Nodes {
		if _, include := selectedSet[definition.Name]; !include {
			continue
		}
		node, err := preStartStore.ReadNode(definition.Name)
		if err != nil {
			return Status{}, err
		}
		switch node.Phase {
		case state.Running:
			// Recheck readiness without restarting QEMU so a later up converges
			// a prior partial run; --no-wait skips the check.
			if !m.NoWait {
				names = append(names, node.Node)
			}
		case state.Prepared, state.Stopped:
			names = append(names, node.Node)
			starting++
		default:
			return Status{}, interruptedPhaseError(node)
		}
	}
	if len(names) == 0 {
		m.Progress.Report(activity.Event{Phase: "deployment-state", Message: "All selected nodes are already running", Done: true})
		return m.statusForLocked(ctx, deploymentValue, "already running")
	}
	readyTimeout, err := m.readyTimeout(deploymentState.Resolved)
	if err != nil {
		return Status{}, err
	}
	switch {
	case starting == 0:
		m.report("guest-ready", fmt.Sprintf("Checking guest readiness for %d running node(s)", len(names)))
	case m.NoWait:
		m.report("guest-ready", fmt.Sprintf("Starting %d node(s) without waiting for guest readiness", starting))
	default:
		m.report("guest-ready", fmt.Sprintf("Starting %d node(s) and waiting up to %s for guest readiness", starting, readyTimeout))
	}
	outcomes, err := StartPrepared(ctx, StartConfig{Deployment: deploymentValue, Lifecycle: lifecycle, Nodes: names, NodeAddresses: resolvedNodeAddresses(deploymentState.Resolved), Concurrency: m.concurrency(len(names)), ReadyTimeout: readyTimeout, NoWait: m.NoWait, Progress: m.Progress})
	if err != nil {
		return Status{}, err
	}
	if failures := startFailures(outcomes); len(failures) > 0 {
		status, statusErr := m.statusForLocked(ctx, deploymentValue, "")
		return statusWithReadiness(status, outcomes), errors.Join(newPartialError(failures, len(outcomes)), statusErr)
	}
	readyMessage := fmt.Sprintf("All %d node(s) are ready", len(outcomes))
	if m.NoWait {
		readyMessage = fmt.Sprintf("QEMU is running for %d node(s); guest readiness was skipped", len(outcomes))
	}
	m.Progress.Report(activity.Event{Phase: "guest-ready", Message: readyMessage, Done: true})
	if starting == 0 {
		status, err := m.statusForLocked(ctx, deploymentValue, "already running")
		return statusWithReadiness(status, outcomes), err
	}
	status, err := m.statusForLocked(ctx, deploymentValue, "started the deployment")
	return statusWithReadiness(status, outcomes), err
}

func (m Manager) Start(ctx context.Context) (Status, error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return Status{}, err
	}
	deploymentState, err := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
	if err != nil {
		return Status{}, errors.New("the deployment has no valid state")
	}
	profile, err := m.nativeProfile()
	if err != nil {
		return Status{}, err
	}
	return m.startExisting(ctx, deploymentValue, deploymentState, profile)
}

func (m Manager) Stop(ctx context.Context) (_ Status, returnErr error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return Status{}, err
	}
	deploymentLock, err := acquireDeploymentLock(ctx, deploymentValue.Root, false, m.Progress)
	if err != nil {
		return Status{}, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, deploymentLock, "deployment stop lock")
	}()
	return m.stopLocked(ctx, deploymentValue)
}

func (m Manager) stopLocked(ctx context.Context, deploymentValue Deployment) (Status, error) {
	store := state.Store{Root: deploymentValue.Root}
	deploymentState, err := store.ReadDeployment()
	if err != nil {
		return Status{}, err
	}
	names, err := committedNodeNames(store, deploymentState.Resolved, m.Nodes)
	if err != nil {
		return Status{}, err
	}
	lifecycle := NativeLifecycle{RetryGuestSetup: m.retryGuestSetup, VM: vm.Lifecycle{Runner: m.runner(), QMP: &qmp.Client{Timeout: 5 * time.Second}, SSHUser: deploymentState.Resolved.SSHUser}}
	outcomes, err := StopRunning(ctx, StopConfig{
		Deployment: deploymentValue, Lifecycle: lifecycle, Nodes: names, Concurrency: m.concurrency(len(names)),
		Immediate: m.ImmediateStop, NodeAddresses: resolvedNodeAddresses(deploymentState.Resolved), Progress: m.Progress,
	})
	if err != nil {
		return Status{}, err
	}
	failures := make([]NodeFailure, 0)
	for _, outcome := range outcomes {
		if outcome.Error != "" || !outcome.Stopped {
			message := outcome.Error
			if message == "" {
				message = "node did not stop"
			}
			failures = append(failures, NodeFailure{Node: outcome.Node, Stage: "stop", Error: message})
		}
	}
	if len(failures) > 0 {
		return Status{}, newPartialError(failures, len(outcomes))
	}
	return m.statusForLocked(ctx, deploymentValue, "stopped selected nodes")
}

func (m Manager) Restart(ctx context.Context) (Status, error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return Status{}, err
	}
	deploymentState, err := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
	if err != nil {
		return Status{}, errors.New("the deployment has no valid state")
	}
	profile, err := m.nativeProfile()
	if err != nil {
		return Status{}, err
	}
	selected, selectionErr := committedNodeNames(state.Store{Root: deploymentValue.Root}, deploymentState.Resolved, m.Nodes)
	if _, err := m.preflight(ctx, profile, resolvedNodeSelection(deploymentState.Resolved, selected)); err != nil {
		return Status{}, err
	}
	if selectionErr != nil {
		return Status{}, selectionErr
	}
	if err := selectedShareSources(deploymentValue, deploymentState.Resolved, selected); err != nil {
		return Status{}, err
	}
	shareBinaries, err := selectedShareInvocationBinaries(state.Store{Root: deploymentValue.Root}, deploymentState.Resolved, selected)
	if err != nil {
		return Status{}, err
	}
	if err := validatePrivateShareDeviceHelp(ctx, m.runner(), shareBinaries); err != nil {
		return Status{}, err
	}
	if err := selectedShareAccess(deploymentValue, deploymentState.Resolved, selected); err != nil {
		return Status{}, err
	}
	if _, err := m.Stop(ctx); err != nil {
		return Status{}, err
	}
	return m.Start(ctx)
}
