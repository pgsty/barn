package private

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/farrow/internal/activity"
	"github.com/pgsty/farrow/internal/diagnostics"
	"github.com/pgsty/farrow/internal/disk"
	"github.com/pgsty/farrow/internal/execx"
	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/image"
	"github.com/pgsty/farrow/internal/lock"
	"github.com/pgsty/farrow/internal/network/portalloc"
	netpreflight "github.com/pgsty/farrow/internal/network/preflight"
	"github.com/pgsty/farrow/internal/network/subnet"
	"github.com/pgsty/farrow/internal/openssh"
	"github.com/pgsty/farrow/internal/platform"
	"github.com/pgsty/farrow/internal/process"
	"github.com/pgsty/farrow/internal/qmp"
	"github.com/pgsty/farrow/internal/spec"
	"github.com/pgsty/farrow/internal/sshkeys"
	"github.com/pgsty/farrow/internal/state"
	"github.com/pgsty/farrow/internal/vm"
)

type ImageResolver func(context.Context, string, string) (image.Entry, string, image.Metadata, error)
type ImageLookup func(context.Context, string, string) (image.Entry, error)
type HostPreflightFunc func(context.Context, platform.Profile, *spec.PrivateNetwork, execx.Runner) (Backend, error)
type NetworkPreflightFunc func(context.Context, platform.Profile, netpreflight.Request, execx.Runner) netpreflight.Report

type NetworkPreflightError struct{ Report netpreflight.Report }

var ErrRecreateRequired error = failure.New(failure.Conflict, errors.New("recreate required")).Because("recreate_required")

// ErrNodesRemoved reports nodes present in deployment state but absent from the
// desired configuration. The absence of a node from a configuration never
// implies destruction; removal is an explicit `farrow destroy <node> --force`.
var ErrNodesRemoved error = failure.New(failure.Conflict, errors.New("inventory no longer lists existing node(s)")).Because("nodes_removed")

// resolvedDiff is the node-granular classification of a desired configuration
// against the applied deployment state.
type resolvedDiff struct {
	EnvelopeChanged bool
	Create          []string // desired nodes without committed state: new peers, or the per-node recreate window
	Changed         []string // desired nodes whose definition differs while their state still exists
	Removed         []string // stateful nodes the desired configuration dropped
	Unchanged       []string
}

func diffResolved(persisted, desired spec.Resolved, hasState func(string) bool) resolvedDiff {
	diff := resolvedDiff{}
	if !equalResolvedEnvelope(persisted, desired) {
		diff.EnvelopeChanged = true
	}
	persistedNodes := make(map[string]spec.Node, len(persisted.Nodes))
	for _, node := range persisted.Nodes {
		persistedNodes[node.Name] = node
	}
	desiredNames := make(map[string]struct{}, len(desired.Nodes))
	for _, node := range desired.Nodes {
		desiredNames[node.Name] = struct{}{}
		current, known := persistedNodes[node.Name]
		stateful := known && hasState(node.Name)
		switch {
		case !stateful:
			diff.Create = append(diff.Create, node.Name)
		case sameResolvedNode(current, node):
			diff.Unchanged = append(diff.Unchanged, node.Name)
		default:
			diff.Changed = append(diff.Changed, node.Name)
		}
	}
	for _, node := range persisted.Nodes {
		if _, kept := desiredNames[node.Name]; !kept && hasState(node.Name) {
			diff.Removed = append(diff.Removed, node.Name)
		}
	}
	return diff
}

func describeChanges(current, desired spec.Resolved, selected map[string]struct{}) []string {
	changes := make([]string, 0)
	add := func(label string, before, after any) {
		if !reflect.DeepEqual(before, after) {
			changes = append(changes, fmt.Sprintf("%s: %v -> %v", label, before, after))
		}
	}
	add("default image", current.Image, desired.Image)
	add("guest architecture", current.Arch, desired.Arch)
	add("SSH user", current.SSHUser, desired.SSHUser)
	if !reflect.DeepEqual(current.Private, desired.Private) {
		changes = append(changes, "private network changed")
	}
	before := make(map[string]spec.Node, len(current.Nodes))
	for _, node := range current.Nodes {
		before[node.Name] = node
	}
	for _, node := range desired.Nodes {
		old, exists := before[node.Name]
		if _, include := selected[node.Name]; !exists || !include || sameResolvedNode(old, node) {
			continue
		}
		old, node = alignAutoFilesystems(old, node)
		prefix := node.Name + " "
		add(prefix+"CPU", old.CPUs, node.CPUs)
		add(prefix+"memory (MiB)", old.Memory>>20, node.Memory>>20)
		add(prefix+"root disk (GiB)", old.RootDisk/spec.GiB, node.RootDisk/spec.GiB)
		add(prefix+"image", old.Image, node.Image)
		add(prefix+"address", old.Address, node.Address)
		add(prefix+"control node", old.Control, node.Control)
		for _, item := range []struct {
			name          string
			before, after any
		}{
			{"data disks", old.Disks, node.Disks}, {"shared directories", old.Shares, node.Shares},
			{"port forwards", old.Forwards, node.Forwards}, {"aliases", old.Aliases, node.Aliases},
		} {
			if !reflect.DeepEqual(item.before, item.after) {
				changes = append(changes, prefix+item.name+" changed")
			}
		}
	}
	return changes
}

func equalResolvedEnvelope(first, second spec.Resolved) bool {
	firstHash, firstErr := spec.Hash(envelopeOf(first))
	secondHash, secondErr := spec.Hash(envelopeOf(second))
	return firstErr == nil && secondErr == nil && firstHash == secondHash
}

// sameResolvedNode compares two definitions of one node. A disk requested as
// "auto" keeps whatever filesystem the guest already formatted, so auto never
// differs from a concrete xfs or ext4 on the other side.
func sameResolvedNode(first, second spec.Node) bool {
	first, second = alignAutoFilesystems(first, second)
	firstData, firstErr := spec.CanonicalJSON(spec.Resolved{Nodes: []spec.Node{first}})
	secondData, secondErr := spec.CanonicalJSON(spec.Resolved{Nodes: []spec.Node{second}})
	return firstErr == nil && secondErr == nil && string(firstData) == string(secondData)
}

func alignAutoFilesystems(first, second spec.Node) (spec.Node, spec.Node) {
	first.Disks = append([]spec.Disk(nil), first.Disks...)
	second.Disks = append([]spec.Disk(nil), second.Disks...)
	index := make(map[string]int, len(first.Disks))
	for i, disk := range first.Disks {
		index[disk.Name] = i
	}
	for j, disk := range second.Disks {
		i, ok := index[disk.Name]
		if !ok {
			continue
		}
		if autoFilesystem(first.Disks[i].Filesystem) || autoFilesystem(disk.Filesystem) {
			first.Disks[i].Filesystem = ""
			second.Disks[j].Filesystem = ""
		}
	}
	return first, second
}

// autoFilesystem accepts both spellings of "let the guest choose": the inventory
// says auto, the resolved spec leaves the field empty.
func autoFilesystem(value string) bool { return value == "" || value == "auto" }

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
	FarrowVersion       string
	OperationID         string
	Runner              execx.Runner
	ReadyTimeout        time.Duration
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
	allowPartialDestroy bool
	dropFromSpec        bool
}

func (m Manager) report(phase, message string) {
	m.Progress.Report(activity.Event{Phase: phase, Message: message})
}

type NodeStatus struct {
	Warnings     []state.GuestWarning `json:"warnings,omitempty"`
	Repairs      []string             `json:"repairs,omitempty"`
	hostKeyAlias string               // carries the identity checked in this snapshot into direct SSH
	Ready        bool                 `json:"ready,omitempty"`
	Error        string               `json:"error,omitempty"`
	Image        string               `json:"image,omitempty"`
	CPUs         int                  `json:"cpus,omitempty"`
	Memory       int64                `json:"memory_bytes,omitempty"`
	Name         string               `json:"name"`
	Address      string               `json:"address"`
	State        state.Phase          `json:"state"`
	Runtime      string               `json:"runtime"`
	GuestArch    string               `json:"guest_arch,omitempty"`
	Accel        string               `json:"accelerator,omitempty"`
	SSHHost      string               `json:"ssh_host"`
	SSHPort      uint16               `json:"ssh_port"`
	ProcessID    int                  `json:"pid,omitempty"`
}

func invocationOption(arguments []string, name string) string {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}
	return ""
}

func invocationGuestArch(binary string, arguments []string) string {
	switch filepath.Base(binary) {
	case "qemu-system-aarch64":
		return "arm64"
	case "qemu-system-x86_64":
		return "amd64"
	}
	machine := strings.SplitN(invocationOption(arguments, "-machine"), ",", 2)[0]
	switch machine {
	case "virt":
		return "arm64"
	case "q35":
		return "amd64"
	default:
		return ""
	}
}

type Status struct {
	OperationID string       `json:"operation_id,omitempty"`
	SpecHash    string       `json:"spec_hash"`
	Nodes       []NodeStatus `json:"nodes"`
	Message     string       `json:"message,omitempty"`
}

func appendStatusMessage(current, addition string) string {
	current = strings.TrimSpace(current)
	addition = strings.TrimSpace(addition)
	if current == "" {
		return addition
	}
	if addition == "" {
		return current
	}
	return current + "; " + addition
}

type Connection struct {
	Node         string `json:"node"`
	User         string `json:"user"`
	Host         string `json:"host"`
	Port         uint16 `json:"port"`
	PrivateKey   string `json:"private_key"`
	KnownHosts   string `json:"known_hosts"`
	HostKeyAlias string `json:"host_key_alias,omitempty"`
}

type LifecyclePlan struct {
	Source      string   `json:"source,omitempty"`
	Start       []string `json:"start,omitempty"`
	Changes     []string `json:"changes,omitempty"`
	Images      []string `json:"images"`
	CPUs        int      `json:"cpus"`
	Memory      int64    `json:"memory_bytes"`
	Storage     int64    `json:"disk_bytes"`
	Blocked     string   `json:"blocked,omitempty"`
	Schema      int      `json:"schema"`
	Action      string   `json:"action"`
	SpecHash    string   `json:"spec_hash"`
	Nodes       []string `json:"nodes"`
	Destructive bool     `json:"destructive"`
	Create      []string `json:"create,omitempty"`
	Recreate    []string `json:"recreate,omitempty"`
	Missing     []string `json:"missing,omitempty"`
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

func boundedConcurrency(nodes int) int {
	if nodes < 1 {
		return 1
	}
	if nodes > 4 {
		return 4
	}
	return nodes
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
			return nil, unknownNodeError(name, resolved.Nodes)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("node selection repeats %q", name)
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
			return nil, fmt.Errorf("node %s has no committed state; run `farrow up %s` first", name, name)
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

// materializeExistingForwardPorts preserves the finite host-port choices that
// were committed on first create. Without this normalization, a preferred
// port collision makes the original YAML look like destructive drift forever.
func materializeExistingForwardPorts(desired, existing spec.Resolved) spec.Resolved {
	result := cloneResolved(desired)
	existingNodes := make(map[string]spec.Node, len(existing.Nodes))
	for _, node := range existing.Nodes {
		existingNodes[node.Name] = node
	}
	for nodeIndex := range result.Nodes {
		current, ok := existingNodes[result.Nodes[nodeIndex].Name]
		if !ok {
			continue
		}
		result.Nodes[nodeIndex].Forwards = spec.ReuseMaterializedForwardPorts(result.Nodes[nodeIndex].Forwards, current.Forwards)
	}
	return result
}

func materializeMissingNodePorts(value spec.Resolved, createNames []string, reserved map[uint16]struct{}, store state.Store) (spec.Resolved, map[string]uint16, error) {
	return materializeMissingNodePortsWithProbe(value, createNames, reserved, store, func(bind string, port uint16) bool {
		listener, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(int(port))))
		if err != nil {
			return false
		}
		_ = listener.Close()
		return true
	})
}

func materializeMissingNodePortsWithProbe(value spec.Resolved, createNames []string, reserved map[uint16]struct{}, store state.Store, probe func(string, uint16) bool) (spec.Resolved, map[string]uint16, error) {
	if len(createNames) == 0 {
		return materializePortsWithProbe(value, reserved, probe)
	}
	resolved := cloneResolved(value)
	createSet := nodeNameSet(createNames)
	ports := make(map[string]uint16, len(value.Nodes))
	available := func(bind string, port uint16) bool {
		if _, exists := reserved[port]; exists {
			return false
		}
		return probe(bind, port)
	}
	for _, definition := range resolved.Nodes {
		if _, create := createSet[definition.Name]; create {
			continue
		}
		node, err := store.ReadNode(definition.Name)
		if missingPath(err) {
			continue
		}
		if err != nil {
			return spec.Resolved{}, nil, err
		}
		ports[definition.Name] = node.SSHPort
		reserved[node.SSHPort] = struct{}{}
		for _, forward := range definition.Forwards {
			reserved[forward.Host] = struct{}{}
		}
	}
	for index := range resolved.Nodes {
		definition := &resolved.Nodes[index]
		if _, create := createSet[definition.Name]; !create {
			continue
		}
		port, err := portalloc.Choose(uint16(2222+index), func(port uint16) bool { return available("127.0.0.1", port) })
		if err != nil {
			return spec.Resolved{}, nil, fmt.Errorf("allocate management SSH for recreated node %s: %w", definition.Name, err)
		}
		reserved[port] = struct{}{}
		ports[definition.Name] = port
		for forwardIndex := range definition.Forwards {
			forward := &definition.Forwards[forwardIndex]
			port, err := portalloc.Choose(spec.RequestedHostPort(*forward), func(port uint16) bool { return available(forward.Bind, port) })
			if err != nil {
				return spec.Resolved{}, nil, fmt.Errorf("allocate forward for %s: %w", definition.Name, err)
			}
			reserved[port] = struct{}{}
			*forward = spec.WithMaterializedHost(*forward, port)
		}
	}
	return resolved, ports, nil
}

func materializePortsWithProbe(value spec.Resolved, reserved map[uint16]struct{}, probe func(string, uint16) bool) (spec.Resolved, map[string]uint16, error) {
	resolved := cloneResolved(value)
	selected := make(map[uint16]struct{})
	available := func(bind string) func(uint16) bool {
		return func(port uint16) bool {
			if _, exists := reserved[port]; exists {
				return false
			}
			if _, exists := selected[port]; exists {
				return false
			}
			return probe(bind, port)
		}
	}
	sshPorts := make(map[string]uint16, len(resolved.Nodes))
	for index := range resolved.Nodes {
		preferred := uint16(2222 + index)
		port, err := portalloc.Choose(preferred, available("127.0.0.1"))
		if err != nil {
			return spec.Resolved{}, nil, fmt.Errorf("allocate management SSH for %s: %w", resolved.Nodes[index].Name, err)
		}
		selected[port] = struct{}{}
		sshPorts[resolved.Nodes[index].Name] = port
		for forwardIndex := range resolved.Nodes[index].Forwards {
			forward := &resolved.Nodes[index].Forwards[forwardIndex]
			port, err := portalloc.Choose(spec.RequestedHostPort(*forward), available(forward.Bind))
			if err != nil {
				return spec.Resolved{}, nil, fmt.Errorf("allocate forward for %s: %w", resolved.Nodes[index].Name, err)
			}
			selected[port] = struct{}{}
			*forward = spec.WithMaterializedHost(*forward, port)
		}
	}
	return resolved, sshPorts, nil
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

func (m Manager) resolveBootMode(ctx context.Context, profile platform.Profile, value spec.Resolved) (string, error) {
	boot := ""
	for _, alias := range aliases(value) {
		entry, err := m.lookupOneImage(ctx, alias, profile.Arch)
		if err != nil {
			return "", err
		}
		boot, err = mergeBootMode(profile, boot, alias, entry.Boot)
		if err != nil {
			return "", err
		}
	}
	return boot, nil
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
	lockContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	deploymentLock, err := acquireDeploymentLock(lockContext, deploymentValue.Root, false)
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

func (m Manager) statusForLocked(ctx context.Context, deploymentValue Deployment, message string) (Status, error) {
	store := state.Store{Root: deploymentValue.Root}
	deploymentState, err := store.ReadDeployment()
	if err != nil {
		return Status{}, err
	}
	if deploymentState.Resolved.Network != "private" {
		return Status{}, errors.New("the deployment state is not private")
	}
	selected, err := selectedNodeNames(deploymentState.Resolved, m.Nodes)
	if err != nil {
		return Status{}, err
	}
	selectedSet := nodeNameSet(selected)
	result := Status{OperationID: m.OperationID, SpecHash: deploymentState.SpecHash, Message: message, Nodes: make([]NodeStatus, 0, len(selected))}
	lifecycle := vm.Lifecycle{Runner: m.runner(), QMP: &qmp.Client{Timeout: 5 * time.Second}, SSHUser: deploymentState.Resolved.SSHUser}
	convergenceCandidates := make([]state.NodeState, 0)
	readErrors := make(map[string]error)
	failures := make([]NodeFailure, 0)
	nodes := make(map[string]state.NodeState, len(selected))
	// Compatibility expiry: process-start-v0 in CONTRIBUTING.md#compatibility-expiry.
	// Pre-0.1 development states stored locale- and timezone-dependent ps lstart
	// text. Migrate only an identity that still matches every legacy fact in the
	// caller's current environment; a mismatch remains fail-closed.
	for _, definition := range deploymentState.Resolved.Nodes {
		if _, include := selectedSet[definition.Name]; !include {
			continue
		}
		node, err := store.ReadNode(definition.Name)
		if missingPath(err) {
			continue
		}
		if err != nil {
			readErrors[definition.Name] = err
			continue
		}
		if completeProcess(node.Process) && process.IsLegacyStart(node.Process.Started) {
			recorded := process.Identity{PID: node.Process.PID, Executable: node.Process.Executable, Started: node.Process.Started, ArgvHash: node.Process.ArgvHash}
			if process.MatchesLive(ctx, m.runner(), recorded, node.Invocation) {
				fresh, captureErr := process.Capture(ctx, m.runner(), node.Invocation, node.Process.PID)
				if captureErr != nil || fresh.PID != recorded.PID || fresh.ArgvHash != process.ExpectedArgvHash(node.Invocation) {
					result.Message = appendStatusMessage(result.Message, "kept legacy process identity for "+node.Node+" because native argv binding was unavailable or did not match")
				} else {
					node.Process = recordProcess(fresh)
					node.UpdatedAt = time.Now().UTC()
					if err := store.WriteNode(node); err != nil {
						readErrors[node.Node] = fmt.Errorf("migrate process identity: %w", err)
						continue
					}
					result.Message = appendStatusMessage(result.Message, "migrated legacy process identity for "+node.Node)
				}
			}
		}
		nodes[node.Node] = node
	}
	for _, definition := range deploymentState.Resolved.Nodes {
		node, include := nodes[definition.Name]
		if !include {
			if _, selected := selectedSet[definition.Name]; selected {
				row := NodeStatus{Name: definition.Name, Address: definition.Address, State: state.Absent, Runtime: "absent", Image: definition.Image, CPUs: definition.CPUs, Memory: definition.Memory}
				if readErr := readErrors[definition.Name]; readErr != nil {
					row.State, row.Runtime, row.Error = "unknown", "unknown", readErr.Error()
					failures = append(failures, NodeFailure{Node: definition.Name, Stage: "status", Error: row.Error})
				}
				result.Nodes = append(result.Nodes, row)
			}
			continue
		}
		runtimeState := "inactive"
		nodeErr := func() error {
			if node.Phase == state.Running {
				identity := process.Identity{PID: node.Process.PID, Executable: node.Process.Executable, Started: node.Process.Started, ArgvHash: node.Process.ArgvHash}
				qmpErr := lifecycle.ValidateIdentity(ctx, node.Runtime.QMP, node.Node, node.VMUUID)
				if err := ctx.Err(); err != nil {
					return err
				}
				processMatches := process.MatchesLive(ctx, m.runner(), identity, node.Invocation)
				if err := ctx.Err(); err != nil {
					return err
				}
				switch {
				case qmpErr == nil && processMatches:
					runtimeState = "running"
					if node.Process.Boot == "" {
						// Bind a pre-0.9 record to this boot so a later reuse is provable.
						if boot, err := process.BootID(); err == nil {
							node.Process.Boot = boot
							convergenceCandidates = append(convergenceCandidates, node)
						}
					}
				case qmpErr == nil:
					return fmt.Errorf("node %s has matching QMP but its recorded process identity does not match; recreate --force it", node.Node)
				case errors.Is(qmpErr, vm.ErrQMPIdentityMismatch):
					return fmt.Errorf("node %s has mismatched QMP identity; recreate --force it: %w", node.Node, qmpErr)
				case processMatches:
					return fmt.Errorf("node %s process identity matches but QMP is unavailable; inspect its logs, then run `farrow stop` to converge it (shutdown uses verified process signals, not guest powerdown)", node.Node)
				case node.Runtime.Directory == "" || node.Runtime.QMP == "" || node.Runtime.PIDFile == "":
					return fmt.Errorf("node %s is recorded running with incomplete runtime identity; recreate is required", node.Node)
				case !completeProcess(node.Process):
					return fmt.Errorf("node %s is recorded running with incomplete process identity; recreate is required", node.Node)
				case observeProcess(ctx, m.runner(), node, node.Process.PID) == process.Unknown:
					// A reused PID (after a reboot, or another exit) is proven
					// foreign and converges below; only an unreadable one blocks.
					return fmt.Errorf("node %s recorded PID %d is alive but its identity cannot be read; Farrow will not treat it as stopped", node.Node, node.Process.PID)
				default:
					// ValidateIdentity alone cannot distinguish a wholly stale QMP
					// socket from a live endpoint whose name responded but UUID query
					// failed. Require the stricter runtime auditor to prove the whole
					// QMP/process/pidfile identity dead before converging.
					observation, auditErr := RuntimeIdentityAuditor(m.runner(), time.Second)(ctx, node)
					if auditErr != nil {
						return fmt.Errorf("node %s runtime death audit is inconclusive: %w", node.Node, auditErr)
					}
					if observation.Live || observation.Authority != "dead" {
						return fmt.Errorf("node %s runtime became live during death audit: %s", node.Node, observation.Evidence)
					}
					// This is the safe self-halt case. The death audit proved both QMP
					// and every recorded PID dead, so remove the bounded runtime residue
					// before publishing a startable stopped state.
					if err := cleanupRuntime(node); err != nil {
						return fmt.Errorf("clean dead private runtime for %s: %w", node.Node, err)
					}
					node.Phase = state.Stopped
					node.Process = state.ProcessIdentity{}
					node.UpdatedAt = time.Now().UTC()
					convergenceCandidates = append(convergenceCandidates, node)
				}
			} else if node.Phase == state.Stopping || node.Phase == state.Starting || node.Phase == state.Destroying {
				// An interrupted transition (a killed CLI mid-stop/start/destroy).
				// Prove the runtime dead before converging. A QMP-bound starting
				// process can be adopted above; other live transitions remain visible
				// and are re-driven only with complete process identity.
				observation, auditErr := RuntimeIdentityAuditor(m.runner(), time.Second)(ctx, node)
				if auditErr != nil {
					return fmt.Errorf("node %s was interrupted mid-%s and its runtime death audit is inconclusive: %w", node.Node, node.Phase, auditErr)
				}
				if observation.Live && node.Phase == state.Starting && !completeProcess(node.Process) {
					if observation.Authority != "qmp" {
						return fmt.Errorf("node %s interrupted start lacks matching QMP authority", node.Node)
					}
					identityValue, captureErr := captureRuntimeProcess(ctx, m.runner(), node)
					if captureErr != nil {
						return fmt.Errorf("adopt interrupted private start for %s: %w", node.Node, captureErr)
					}
					node.Process = recordProcess(identityValue)
					node.Phase = state.Running
					node.UpdatedAt = time.Now().UTC()
					convergenceCandidates = append(convergenceCandidates, node)
					result.Message = appendStatusMessage(result.Message, fmt.Sprintf("adopted interrupted start for %s (pid %d)", node.Node, identityValue.PID))
					runtimeState = "running"
				} else if observation.Live {
					runtimeState = "running"
				} else {
					if node.Runtime.Directory != "" {
						if err := cleanupRuntime(node); err != nil {
							return fmt.Errorf("clean dead private runtime for %s: %w", node.Node, err)
						}
					}
					node.Phase = state.Stopped
					node.Process = state.ProcessIdentity{}
					node.UpdatedAt = time.Now().UTC()
					convergenceCandidates = append(convergenceCandidates, node)
				}
			}
			return nil
		}()
		message := ""
		if nodeErr != nil {
			runtimeState, message = "unknown", nodeErr.Error()
			failures = append(failures, NodeFailure{Node: node.Node, Stage: "status", Error: message})
		}
		result.Nodes = append(result.Nodes, NodeStatus{
			Warnings:     node.GuestWarnings,
			hostKeyAlias: vm.HostKeyAlias(node.VMUUID),
			Name:         node.Node, Address: definition.Address, State: node.Phase, Runtime: runtimeState, Error: message,
			Image: node.Image.Alias + "@" + node.Image.Release, CPUs: definition.CPUs, Memory: definition.Memory,
			GuestArch: invocationGuestArch(node.Invocation.Binary, node.Invocation.Args), Accel: invocationOption(node.Invocation.Args, "-accel"),
			SSHHost: "127.0.0.1", SSHPort: node.SSHPort, ProcessID: node.Process.PID,
		})
	}
	for _, node := range convergenceCandidates {
		if err := store.WriteNode(node); err != nil {
			failures = append(failures, NodeFailure{Node: node.Node, Stage: "status", Error: err.Error()})
			for index := range result.Nodes {
				if result.Nodes[index].Name == node.Node {
					result.Nodes[index].Error = err.Error()
				}
			}
		}
	}
	if len(failures) != 0 {
		return result, newPartialError(failures, len(selected))
	}
	return result, nil
}

func (m Manager) statusFor(ctx context.Context, deploymentValue Deployment, message string) (_ Status, returnErr error) {
	lockContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	deploymentLock, err := acquireDeploymentLock(lockContext, deploymentValue.Root, false)
	if err != nil {
		return Status{}, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, deploymentLock, "deployment status lock")
	}()
	return m.statusForLocked(ctx, deploymentValue, message)
}

func (m Manager) Status(ctx context.Context) (Status, error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return Status{}, err
	}
	return m.statusFor(ctx, deploymentValue, "")
}

func defaultCommittedNode(store state.Store, resolved spec.Resolved) (string, error) {
	committed, err := committedNodeNames(store, resolved, nil)
	if err != nil {
		return "", err
	}
	committedSet := nodeNameSet(committed)
	for _, node := range resolved.Nodes {
		if _, exists := committedSet[node.Name]; exists && node.Control {
			return node.Name, nil
		}
	}
	return committed[0], nil
}

func (m Manager) Connection(ctx context.Context, requestedNode string) (Connection, error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return Connection{}, err
	}
	deploymentState, err := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
	if err != nil || deploymentState.Resolved.Network != "private" {
		return Connection{}, errors.New("the deployment has no valid state")
	}
	if requestedNode == "" {
		requestedNode, err = defaultCommittedNode(state.Store{Root: deploymentValue.Root}, deploymentState.Resolved)
		if err != nil {
			return Connection{}, err
		}
	}
	knownNode := false
	for _, node := range deploymentState.Resolved.Nodes {
		knownNode = knownNode || node.Name == requestedNode
	}
	if !knownNode {
		return Connection{}, unknownNodeError(requestedNode, deploymentState.Resolved.Nodes)
	}
	m.Nodes = []string{requestedNode}
	status, err := m.statusFor(ctx, deploymentValue, "")
	if err != nil {
		return Connection{}, err
	}
	port := uint16(0)
	hostKeyAlias := ""
	for _, node := range status.Nodes {
		if node.Name == requestedNode {
			if node.State != state.Running || node.Runtime != "running" {
				return Connection{}, fmt.Errorf("node %s is not running", requestedNode)
			}
			port = node.SSHPort
			hostKeyAlias = node.hostKeyAlias
		}
	}
	privateKey, knownHosts, err := validateSSHArtifacts(deploymentValue)
	if err != nil {
		return Connection{}, err
	}
	for path, mode := range map[string]os.FileMode{privateKey: 0o600, knownHosts: 0o600} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode {
			return Connection{}, fmt.Errorf("SSH artifact is unsafe: %s", path)
		}
	}
	return Connection{Node: requestedNode, User: deploymentState.Resolved.SSHUser, Host: "127.0.0.1", Port: port, PrivateKey: privateKey, KnownHosts: knownHosts, HostKeyAlias: hostKeyAlias}, nil
}

func (m Manager) LogPath(nodeName, source string) (string, error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return "", err
	}
	if source == "events" {
		path := filepath.Join(deploymentValue.Root, "events.jsonl")
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("event log is missing or unsafe: %s", path)
		}
		return path, nil
	}
	deploymentState, err := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
	if err != nil || deploymentState.Resolved.Network != "private" {
		return "", errors.New("the deployment has no valid state")
	}
	if source != "serial" && source != "qemu" {
		return "", fmt.Errorf("unsupported private log source %q", source)
	}
	if nodeName == "" {
		nodeName, err = defaultCommittedNode(state.Store{Root: deploymentValue.Root}, deploymentState.Resolved)
		if err != nil {
			return "", err
		}
	}
	nodeDir, err := deploymentValue.NodeDir(nodeName)
	if err != nil {
		return "", err
	}
	known := false
	for _, node := range deploymentState.Resolved.Nodes {
		known = known || node.Name == nodeName
	}
	if !known {
		return "", unknownNodeError(nodeName, deploymentState.Resolved.Nodes)
	}
	logName := "serial.log"
	if source == "qemu" {
		logName = "qemu.log"
	}
	path := filepath.Join(nodeDir, logName)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s log is missing or unsafe: %s", source, path)
	}
	return path, nil
}

func (m Manager) SSHConfig(ctx context.Context) (string, error) {
	deploymentValue, deploymentState, nodes, err := m.integrationSnapshot(ctx)
	if err != nil {
		return "", err
	}
	privateKey, knownHosts, err := validateSSHArtifacts(deploymentValue)
	if err != nil {
		return "", err
	}
	var output strings.Builder
	quotedIdentity, err := openssh.QuoteConfigValue(privateKey)
	if err != nil {
		return "", err
	}
	quotedKnownHosts, err := openssh.QuoteConfigValue(knownHosts)
	if err != nil {
		return "", err
	}
	for index, definition := range deploymentState.Resolved.Nodes {
		node := nodes[index]
		hostPatterns := []string{definition.Name}
		if definition.Address != "" {
			hostPatterns = append(hostPatterns, definition.Address)
		}
		hostPatterns = append(hostPatterns, definition.Aliases...)
		fmt.Fprintf(&output, "Host %s\n  HostName 127.0.0.1\n  User %s\n  Port %d\n  IdentityFile %s\n  UserKnownHostsFile %s\n  HostKeyAlias %s\n  IdentitiesOnly yes\n  StrictHostKeyChecking accept-new\n\n", strings.Join(hostPatterns, " "), deploymentState.Resolved.SSHUser, node.SSHPort, quotedIdentity, quotedKnownHosts, vm.HostKeyAlias(node.VMUUID))
	}
	return output.String(), nil
}

func (m Manager) RecordEvent(ctx context.Context, action, level, message string) error {
	if m.OperationID == "" {
		return errors.New("event requires an operation ID")
	}
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return err
	}
	event := diagnostics.Event{Schema: 1, Time: time.Now().UTC(), Level: level, Node: "deployment", OperationID: m.OperationID, Action: action, Message: message}
	if err := diagnostics.AppendEvent(ctx, filepath.Join(deploymentValue.Root, "events.jsonl"), event); err != nil {
		return err
	}
	deploymentState, err := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
	if err != nil {
		return nil
	}
	store := state.Store{Root: deploymentValue.Root}
	for _, definition := range deploymentState.Resolved.Nodes {
		node, err := store.ReadNode(definition.Name)
		if err != nil {
			continue
		}
		record := diagnostics.QEMULogRecord{Schema: 1, Time: time.Now().UTC(), Level: level, Node: node.Node, OperationID: m.OperationID, Action: action, Message: message + "; " + execx.Display(node.Invocation.Binary, node.Invocation.Args...)}
		if err := diagnostics.AppendQEMULog(ctx, filepath.Join(filepath.Dir(node.RootDisk), "qemu.log"), record); err != nil {
			return err
		}
	}
	return nil
}

// refuseDrift is the one converge boundary: up and reload apply additions
// and starts, and both report definition drift or removed nodes for the
// explicit recreate/destroy paths instead of acting on them.
func refuseDrift(diff resolvedDiff) error {
	if diff.EnvelopeChanged {
		return fmt.Errorf("%w: deployment-level settings changed; run `farrow plan`, then `farrow recreate`", ErrRecreateRequired)
	}
	if len(diff.Removed) != 0 {
		return fmt.Errorf("%w: %s; Farrow never destroys from absence; run `farrow destroy %s` to remove them, or add them back to the inventory", ErrNodesRemoved, strings.Join(diff.Removed, ", "), strings.Join(diff.Removed, " "))
	}
	if len(diff.Changed) != 0 {
		return fmt.Errorf("%w: node(s) %s changed; run `farrow plan` to review, then `farrow recreate %s`", ErrRecreateRequired, strings.Join(diff.Changed, ", "), strings.Join(diff.Changed, " "))
	}
	return nil
}

func refuseUnselectedDrift(diff resolvedDiff, selected []string) error {
	if diff.EnvelopeChanged || len(diff.Removed) != 0 {
		return refuseDrift(diff)
	}
	selectedSet := nodeNameSet(selected)
	peers := make([]string, 0)
	for _, name := range diff.Changed {
		if _, replacing := selectedSet[name]; !replacing {
			peers = append(peers, name)
		}
	}
	if len(peers) != 0 {
		together := append(append([]string(nil), selected...), peers...)
		return fmt.Errorf("%w: unselected node(s) %s also changed; select them together with `farrow recreate %s`, or revert their changes", ErrRecreateRequired, strings.Join(peers, ", "), strings.Join(together, " "))
	}
	return nil
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
	if err := m.ensureImageSession(); err != nil {
		return runtimeDecision{}, "", err
	}
	boot, err := m.resolveBootMode(ctx, runtime.Profile, requested)
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
	if err != nil || persisted.Resolved.Network != "private" {
		return Status{}, errors.New("the deployment has no valid state")
	}
	selected, err := selectedNodeNames(requested, m.Nodes)
	if err != nil {
		return Status{}, err
	}
	hasState := func(name string) bool {
		_, readErr := store.ReadNode(name)
		return readErr == nil
	}
	if err := refuseDrift(diffResolved(persisted.Resolved, materializeExistingForwardPorts(requested, persisted.Resolved), hasState)); err != nil {
		return Status{}, err
	}
	creating := make([]string, 0)
	for _, name := range selected {
		if !hasState(name) {
			creating = append(creating, name)
		}
	}
	runtime, qemuPath, err := m.preflightBoot(ctx, requested, creating)
	if err != nil {
		return Status{}, err
	}
	if drifted, err := runtimeDriftNodes(store, requested, runtime.Profile); err != nil {
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

func (m Manager) Up(ctx context.Context, requested spec.Resolved) (_ Status, returnErr error) {
	m.retryGuestSetup = true
	m.report("preflight", "Checking the fixed-IP network and QEMU capabilities")
	var err error
	if err := validateResolved(requested); err != nil {
		return Status{}, err
	}
	selected, err := selectedNodeNames(requested, m.Nodes)
	if err != nil {
		return Status{}, err
	}
	selectedRequested := resolvedNodeSelection(requested, selected)
	hostProfile, err := m.nativeProfile()
	if err != nil {
		return Status{}, err
	}
	backend, err := m.preflight(ctx, hostProfile, selectedRequested)
	if err != nil {
		return Status{}, err
	}
	m.Progress.Report(activity.Event{Phase: "preflight", Message: "Network and QEMU preflight passed", Done: true})
	selectedSet := nodeNameSet(selected)
	var reusableDeployment *Deployment
	createNodes := []string(nil)
	hadExistingState := false
	startExistingAfterCreate := false
	if existing, openErr := m.openDeployment(false); openErr == nil {
		persisted, err := (state.Store{Root: existing.Root}).ReadDeployment()
		if missingPath(err) {
			reusableDeployment = &existing
		} else if err != nil {
			return Status{}, err
		} else {
			hadExistingState = true
			requested = materializeExistingForwardPorts(requested, persisted.Resolved)
			store := state.Store{Root: existing.Root}
			hasState := func(name string) bool {
				_, readErr := store.ReadNode(name)
				return readErr == nil
			}
			diff := diffResolved(persisted.Resolved, requested, hasState)
			if err := refuseDrift(diff); err != nil {
				return Status{}, err
			}
			allRunning, allRunnable := true, true
			for _, definition := range requested.Nodes {
				if _, include := selectedSet[definition.Name]; !include {
					continue
				}
				node, err := store.ReadNode(definition.Name)
				if missingPath(err) {
					createNodes = append(createNodes, definition.Name)
					allRunning = false
					continue
				}
				if err != nil {
					return Status{}, err
				}
				startExistingAfterCreate = true
				if node.Phase != state.Running && node.Phase != state.Stopped && node.Phase != state.Prepared {
					recovering := m
					recovering.Nodes = []string{node.Node}
					if _, err := recovering.statusFor(ctx, existing, ""); err != nil {
						return Status{}, err
					}
					node, err = store.ReadNode(definition.Name)
					if err != nil {
						return Status{}, err
					}
				}
				allRunning = allRunning && node.Phase == state.Running
				allRunnable = allRunnable && (node.Phase == state.Running || node.Phase == state.Stopped || node.Phase == state.Prepared)
			}
			if len(createNodes) != 0 {
				reusableDeployment = &existing
			} else if allRunning || allRunnable {
				return m.startExisting(ctx, existing, persisted, hostProfile, backend)
			} else {
				return Status{}, errors.New("the deployment has mixed node phases; run `farrow status` to converge interrupted transitions, then retry")
			}
		}
	} else if !missingPath(openErr) {
		return Status{}, openErr
	}
	if !hadExistingState {
		createNodes = append([]string(nil), selected...)
	}
	if err := m.ensureSSHAddressesUnused(nodesWithoutCommittedState(selectedRequested.Nodes)); err != nil {
		return Status{}, err
	}
	runtime, err := selectRuntime(hostProfile, requested)
	if err != nil {
		return Status{}, err
	}
	if hadExistingState && reusableDeployment != nil {
		drifted, driftErr := runtimeDriftNodes(state.Store{Root: reusableDeployment.Root}, requested, runtime.Profile)
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
	if err := m.ensureImageSession(); err != nil {
		return Status{}, err
	}
	boot, err := m.resolveBootMode(ctx, runtime.Profile, requested)
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
	if reusableDeployment != nil {
		deploymentValue = *reusableDeployment
	} else {
		deploymentValue, err = m.openDeployment(true)
		if err != nil {
			return Status{}, err
		}
	}
	if err := validatePrivatePersistentDesired(deploymentValue, requested); err != nil {
		return Status{}, err
	}
	allocatorContext, cancelAllocator := context.WithTimeout(ctx, 30*time.Second)
	defer cancelAllocator()
	allocator, err := lock.Acquire(allocatorContext, filepath.Join(deploymentValue.Root, "locks", "allocator.lock"), false)
	if err != nil {
		return Status{}, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, allocator, "deployment allocator lock")
	}()
	resolved, sshPorts, err := materializeMissingNodePorts(requested, createNodes, make(map[uint16]struct{}), state.Store{Root: deploymentValue.Root})
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
	if hadExistingState {
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
	controller := Controller{Deployment: deploymentValue, Prepare: prepare, Lifecycle: lifecycle, Concurrency: boundedConcurrency(len(resolved.Nodes)), ReadyTimeout: readyTimeout, NoWait: m.NoWait, CreateNodes: createNodes, StartNodes: startNodes, Version: m.FarrowVersion, Progress: m.Progress}
	createResult, err := controller.CreateAndStart(ctx)
	if err != nil && m.RollbackFailed {
		err = rollbackCreateFailure(deploymentValue, createResult, err)
	}
	// A node-scoped prepare/start failure does not invalidate committed peers.
	// Only continue after the controller released its lock cleanly, and never
	// replay newly started nodes or continue through cancellation/global errors.
	partial, partialCreate := err.(*PartialError)
	if startExistingAfterCreate && ctx.Err() == nil && (err == nil || partialCreate) {
		deploymentState, readErr := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
		if readErr != nil {
			return Status{}, errors.Join(err, readErr)
		}
		peers := m
		peers.Nodes = nil
		creating := nodeNameSet(createNodes)
		for _, name := range selected {
			if _, isNew := creating[name]; !isNew {
				peers.Nodes = append(peers.Nodes, name)
			}
		}
		peerStatus, peerErr := peers.startExisting(ctx, deploymentValue, deploymentState, hostProfile, backend)
		if partialCreate {
			if peerPartial := IsolatedPartial(peerErr); peerPartial != nil {
				combined := newPartialError(append(append([]NodeFailure(nil), partial.Failures...), peerPartial.Failures...), len(selected))
				combined.RolledBack = partial.RolledBack
				err = combined
			} else if peerErr == nil {
				combined := newPartialError(partial.Failures, len(selected))
				combined.RolledBack = partial.RolledBack
				err = combined
			} else {
				err = errors.Join(err, peerErr)
			}
		} else {
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

func (m Manager) startExisting(ctx context.Context, deploymentValue Deployment, deploymentState state.DeploymentState, profile platform.Profile, backend Backend) (_ Status, returnErr error) {
	m.report("preflight", "Checking the existing deployment before start")
	preStartStore := state.Store{Root: deploymentValue.Root}
	selected, selectionErr := committedNodeNames(preStartStore, deploymentState.Resolved, m.Nodes)
	verifiedBackend, err := m.preflight(ctx, profile, resolvedNodeSelection(deploymentState.Resolved, selected))
	if err != nil {
		return Status{}, err
	}
	if selectionErr != nil {
		return Status{}, selectionErr
	}
	backend = verifiedBackend
	shareBinaries, err := selectedShareInvocationBinaries(preStartStore, deploymentState.Resolved, selected)
	if err != nil {
		return Status{}, err
	}
	if err := validatePrivateShareDeviceHelp(ctx, m.runner(), shareBinaries); err != nil {
		return Status{}, err
	}
	selectedSet := nodeNameSet(selected)
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
	lockContext, cancelLock := context.WithTimeout(ctx, 30*time.Second)
	defer cancelLock()
	deploymentLock, err := acquireDeploymentLock(lockContext, deploymentValue.Root, false)
	if err != nil {
		return Status{}, err
	}
	defer func() {
		returnErr = lock.JoinRelease(returnErr, deploymentLock, "deployment start lock")
	}()
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
			return Status{}, fmt.Errorf("node %s phase %s requires `farrow status` convergence before start", node.Node, node.Phase)
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
	outcomes, err := StartPrepared(ctx, StartConfig{Deployment: deploymentValue, Lifecycle: lifecycle, Nodes: names, Concurrency: boundedConcurrency(len(names)), ReadyTimeout: readyTimeout, NoWait: m.NoWait, Progress: m.Progress})
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
	if err != nil || deploymentState.Resolved.Network != "private" {
		return Status{}, errors.New("the deployment has no valid state")
	}
	profile, err := m.nativeProfile()
	if err != nil {
		return Status{}, err
	}
	return m.startExisting(ctx, deploymentValue, deploymentState, profile, Backend{})
}

func (m Manager) Stop(ctx context.Context) (_ Status, returnErr error) {
	deploymentValue, err := m.openDeployment(false)
	if err != nil {
		return Status{}, err
	}
	lockContext, cancelLock := context.WithTimeout(ctx, 30*time.Second)
	defer cancelLock()
	deploymentLock, err := acquireDeploymentLock(lockContext, deploymentValue.Root, false)
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
	outcomes, err := StopRunning(ctx, StopConfig{Deployment: deploymentValue, Lifecycle: lifecycle, Nodes: names, Concurrency: boundedConcurrency(len(names))})
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
	if err != nil || deploymentState.Resolved.Network != "private" {
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

func (m Manager) Plan(ctx context.Context, requested spec.Resolved) (LifecyclePlan, error) {
	var err error
	if err := validateResolved(requested); err != nil {
		return LifecyclePlan{}, err
	}
	selected, err := selectedNodeNames(requested, m.Nodes)
	if err != nil {
		return LifecyclePlan{}, err
	}
	selectedRequested := resolvedNodeSelection(requested, selected)
	profile, err := m.nativeProfile()
	if err != nil {
		return LifecyclePlan{}, err
	}
	runtime, err := selectRuntime(profile, requested)
	if err != nil {
		return LifecyclePlan{}, err
	}
	// Planning is a local catalog/spec comparison. Host checks and downloads
	// belong to up, so users can review a lab before installing QEMU/networking.
	if err := m.ensureImageSession(); err != nil {
		return LifecyclePlan{}, err
	}
	if _, err := m.resolveBootMode(ctx, runtime.Profile, requested); err != nil {
		return LifecyclePlan{}, err
	}
	hash, err := spec.Hash(requested)
	if err != nil {
		return LifecyclePlan{}, err
	}
	selectedSet := nodeNameSet(selected)
	result := LifecyclePlan{Schema: 1, Action: "create", SpecHash: hash, Nodes: append([]string(nil), selected...), Create: append([]string(nil), selected...)}
	for _, alias := range aliases(selectedRequested) {
		entry, err := m.lookupOneImage(ctx, alias, runtime.Profile.Arch)
		if err != nil {
			return LifecyclePlan{}, err
		}
		result.Images = append(result.Images, entry.Alias+"@"+entry.Release+"/"+entry.Arch)
	}
	for _, node := range selectedRequested.Nodes {
		result.CPUs += node.CPUs
		result.Memory += node.Memory
		result.Storage += node.RootDisk
		for _, disk := range node.Disks {
			result.Storage += disk.Size
		}
	}
	deploymentValue, err := m.openDeployment(false)
	if missingPath(err) {
		return result, nil
	}
	if err != nil {
		return LifecyclePlan{}, err
	}
	deploymentState, err := (state.Store{Root: deploymentValue.Root}).ReadDeployment()
	if missingPath(err) {
		return result, nil
	}
	if err != nil {
		return LifecyclePlan{}, err
	}
	requested = materializeExistingForwardPorts(requested, deploymentState.Resolved)
	hash, err = spec.Hash(requested)
	if err != nil {
		return LifecyclePlan{}, err
	}
	result.SpecHash = hash
	store := state.Store{Root: deploymentValue.Root}
	hasState := func(name string) bool {
		_, readErr := store.ReadNode(name)
		return readErr == nil
	}
	diff := diffResolved(deploymentState.Resolved, requested, hasState)
	result.Changes = describeChanges(deploymentState.Resolved, requested, selectedSet)
	if len(m.Nodes) != 0 {
		if err := refuseUnselectedDrift(diff, selected); err != nil {
			result.Blocked = err.Error()
		}
	}
	if len(m.Nodes) == 0 {
		result.Create = append([]string(nil), diff.Create...)
		result.Recreate = append([]string(nil), diff.Changed...)
		result.Missing = append([]string(nil), diff.Removed...)
	} else {
		result.Create = selectedNodeList(diff.Create, selectedSet)
		result.Recreate = selectedNodeList(diff.Changed, selectedSet)
		result.Missing = selectedNodeList(diff.Removed, selectedSet)
	}
	runtimeDrift, err := runtimeDriftNodes(store, requested, runtime.Profile)
	if err != nil {
		return LifecyclePlan{}, err
	}
	if len(runtimeDrift) != 0 && len(m.Nodes) != 0 {
		return LifecyclePlan{}, fmt.Errorf("%w: runtime changes require whole-deployment plan without node selectors", ErrRecreateRequired)
	}
	switch {
	case diff.EnvelopeChanged:
		result.Action = "recreate"
		result.Destructive = true
		return result, nil
	case len(diff.Removed) != 0:
		result.Action = "blocked-removal"
		return result, nil
	case len(diff.Changed) != 0:
		result.Action = "recreate"
		result.Destructive = true
		return result, nil
	case len(runtimeDrift) != 0:
		result.Action = "recreate"
		result.Destructive = true
		result.Recreate = append([]string(nil), runtimeDrift...)
		return result, nil
	}
	allRunning, allStartable := true, true
	for _, definition := range deploymentState.Resolved.Nodes {
		if _, include := selectedSet[definition.Name]; !include {
			continue
		}
		node, err := store.ReadNode(definition.Name)
		if missingPath(err) {
			continue
		}
		if err != nil {
			return LifecyclePlan{}, err
		}
		allRunning = allRunning && node.Phase == state.Running
		allStartable = allStartable && (node.Phase == state.Running || node.Phase == state.Prepared || node.Phase == state.Stopped)
		if node.Phase == state.Prepared || node.Phase == state.Stopped {
			result.Start = append(result.Start, node.Node)
		}
	}
	switch {
	case len(result.Create) != 0:
		result.Action = "converge"
	case allRunning:
		result.Action = "none"
	case allStartable:
		result.Action = "start"
	default:
		result.Action = "repair"
	}
	return result, nil
}

// unknownNodeError names the nodes that do exist so the user can pick one.
func unknownNodeError(name string, nodes []spec.Node) error {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name)
	}
	return fmt.Errorf("the deployment has no node %q; nodes: %s", name, strings.Join(names, ", "))
}
