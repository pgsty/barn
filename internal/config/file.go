package config

import (
	"errors"
	"fmt"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/hostconfig"
	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/naming"
	"github.com/pgsty/barn/internal/network/subnet"
	"github.com/pgsty/barn/internal/spec"
)

const (
	maxVirtualCPUs = 256
	// minMemory is the smallest guest memory Barn boots.
	minMemory = 512 << 20
)

// FormatSize renders a byte size using the largest exact binary unit.
func FormatSize(value int64) string {
	for _, unit := range []struct {
		name string
		size int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if value >= unit.size && value%unit.size == 0 {
			return strconv.FormatInt(value/unit.size, 10) + unit.name
		}
	}
	return strconv.FormatInt(value, 10) + "B"
}

type NetworkConfig struct {
	Mode        string
	CIDR        string
	HostAddress string
	DHCPEnd     string
}

type DefaultsConfig struct {
	Image    string
	CPUs     int
	Memory   int64
	RootDisk int64
}

type SSHConfig struct {
	User        string
	WaitTimeout time.Duration
}

type DiskConfig struct {
	Name       string
	Size       int64
	Mount      string
	Filesystem string
	Persistent bool
}

type ShareConfig struct {
	Host     string
	Guest    string
	Readonly *bool
}

func shareReadonly(share ShareConfig) bool { return share.Readonly == nil || *share.Readonly }

type NodeConfig struct {
	Name        string
	Control     bool
	Address     string
	HostAliases []string
	Image       string
	ImageArch   string
	ImageRaw    string
	CPUs        int
	Memory      int64
	RootDisk    int64
	Disks       []DiskConfig
	Shares      []ShareConfig
}

type File struct {
	Resources     []ResourceResolution
	directory     string
	archFromImage bool
	Version       int
	Name          string
	Arch          string
	Network       NetworkConfig
	Defaults      DefaultsConfig
	SSH           SSHConfig
	Nodes         []NodeConfig
	Hosts         []hostconfig.Entry
}

var (
	sshUser  = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	dnsName  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
	diskName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

func safeDiskMount(value string) bool                { return spec.SafeDiskMount(value) }
func unsafeGuestPathReason(value string) string      { return spec.UnsafeGuestPathReason(value) }
func normalizeGuestPath(value string) (string, bool) { return spec.NormalizeGuestPath(value) }

// label names a node the way the inventory does: by its host key.
func (node NodeConfig) label() string {
	if node.Address == "" {
		return "node " + node.Name
	}
	return fmt.Sprintf("host %s (%s)", node.Address, node.Name)
}

func safeShareHost(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value && value != "/" && !strings.ContainsAny(value, "\x00\r\n")
}

func safeShareGuest(value string) bool {
	return safeDiskMount(value)
}

func cleanPathOverlap(first, second, separator string) bool {
	return first == second || strings.HasPrefix(first, second+separator) || strings.HasPrefix(second, first+separator)
}

func guestPathOverlap(first, second string) bool {
	return cleanPathOverlap(first, second, "/")
}

func (f *File) defaults() {
	if f.Arch == "" {
		f.Arch = "native"
	}
	// Generated inventories materialize vm_arch: native as their default, so an
	// architecture-suffixed vm_image (such as el10a) may specialize native in YAML;
	// concrete vm_arch values and explicit CLI/env --arch native still conflict.
	defaultArch := f.Arch == "native" || f.archFromImage
	if f.Network.Mode == "" {
		f.Network.Mode = "private"
	}
	if f.Defaults.Image == "" {
		f.Defaults.Image = defaultImage
	}
	var defaultImageArch, defaultImageRaw string
	if resolved, err := image.ResolveReference(f.Defaults.Image); err == nil {
		defaultImageRaw = f.Defaults.Image
		defaultImageArch = resolved.Arch
		f.Defaults.Image = resolved.Reference
	}
	if f.Defaults.CPUs == 0 {
		f.Defaults.CPUs = 1
	}
	if f.Defaults.Memory == 0 {
		f.Defaults.Memory = 2 * spec.GiB
	}
	if f.Defaults.RootDisk == 0 {
		f.Defaults.RootDisk = 64 * spec.GiB
	}
	if f.SSH.User == "" {
		f.SSH.User = "dba"
	}
	if f.SSH.WaitTimeout == 0 {
		f.SSH.WaitTimeout = 180 * time.Second
	}
	if f.Network.Mode == "private" {
		if f.Network.CIDR == "" {
			f.Network.CIDR = subnet.DefaultCIDR
		}
		if layout, err := subnet.Parse(f.Network.CIDR); err == nil {
			if f.Network.HostAddress == "" {
				f.Network.HostAddress = layout.HostAddress()
			}
			if f.Network.DHCPEnd == "" {
				f.Network.DHCPEnd = layout.DHCPEnd()
			}
		}
	}
	var firstImpliedArch string
	for index := range f.Nodes {
		node := &f.Nodes[index]
		if node.Image == "" {
			node.Image = f.Defaults.Image
			if node.ImageArch == "" && defaultImageArch != "" {
				node.ImageArch = defaultImageArch
				node.ImageRaw = defaultImageRaw
			}
		} else if resolved, err := image.ResolveReference(node.Image); err == nil {
			if node.ImageArch == "" && resolved.Arch != "" {
				node.ImageArch = resolved.Arch
				node.ImageRaw = node.Image
			}
			node.Image = resolved.Reference
		}
		if firstImpliedArch == "" && node.ImageArch != "" {
			firstImpliedArch = node.ImageArch
		}
		if node.CPUs == 0 {
			node.CPUs = f.Defaults.CPUs
		}
		if node.Memory == 0 {
			node.Memory = f.Defaults.Memory
		}
		if node.RootDisk == 0 {
			node.RootDisk = f.Defaults.RootDisk
		}
		for diskIndex := range node.Disks {
			if node.Disks[diskIndex].Filesystem == "" {
				node.Disks[diskIndex].Filesystem = "auto"
			}
		}
	}
	if defaultArch && firstImpliedArch != "" {
		f.Arch = firstImpliedArch
		f.archFromImage = true
	}
}

// CheckArchOverride ensures an explicit CLI/environment architecture override
// does not contradict any node's architecture-specific vm_image that remains
// in effect.
func (f File) CheckArchOverride(nodes []string, imageOverride, archOverride, archSource string) error {
	if archOverride == "" {
		return nil
	}
	source := strings.TrimSpace(strings.TrimPrefix(archSource, "CLI "))
	if source == "" {
		source = "--arch"
	}
	for index := range f.Nodes {
		node := &f.Nodes[index]
		replaced := imageOverride != "" && (len(nodes) == 0 || slices.Contains(nodes, node.Name))
		if replaced || node.ImageArch == "" || node.ImageArch == archOverride {
			continue
		}
		raw := node.ImageRaw
		if raw == "" {
			raw = node.Image
		}
		return failure.New(failure.Usage, fmt.Errorf("%s vm_image %q targets %s, which conflicts with %s %s", node.label(), raw, node.ImageArch, source, archOverride))
	}
	return nil
}

func (f *File) Validate() error {
	f.defaults()
	if f.Version != 1 {
		return fmt.Errorf("configuration version must be 1, got %d", f.Version)
	}
	if !naming.ValidNodeName(f.Name) {
		return fmt.Errorf("invalid deployment name %q", f.Name)
	}
	if f.Arch != "native" && f.Arch != "amd64" && f.Arch != "arm64" {
		return errors.New("arch must be native, amd64, or arm64")
	}
	if f.Network.Mode != "private" {
		return fmt.Errorf("unsupported network mode %q", f.Network.Mode)
	}
	if !sshUser.MatchString(f.SSH.User) {
		return fmt.Errorf("invalid SSH user %q", f.SSH.User)
	}
	if _, err := image.ParseReference(f.Defaults.Image); err != nil {
		return fmt.Errorf("invalid default image reference: %w", err)
	}
	if len(f.Nodes) == 0 {
		return errors.New("configuration requires at least one node")
	}
	layout, err := subnet.Parse(f.Network.CIDR)
	if err != nil {
		return err
	}
	if f.Network.HostAddress != layout.HostAddress() || f.Network.DHCPEnd != layout.DHCPEnd() {
		return fmt.Errorf("network %s requires host address %s and DHCP end %s", layout.CIDR(), layout.HostAddress(), layout.DHCPEnd())
	}
	if err := f.validateNodes(layout); err != nil {
		return err
	}
	if len(f.Hosts) != 0 {
		if _, err := f.HostEntries(); err != nil {
			return err
		}
	}
	return nil
}

func (f *File) validateNodes(layout subnet.Layout) error {
	names := make(map[string]string, len(f.Nodes)) // name -> node label
	allAliases := make(map[string]struct{}, len(f.Nodes))
	for nodeIndex := range f.Nodes {
		node := &f.Nodes[nodeIndex]
		if !naming.ValidNodeName(node.Name) {
			return fmt.Errorf("%s: invalid node name %q: %s", node.label(), node.Name, naming.NodeNameRule)
		}
		if first, exists := names[node.Name]; exists {
			return fmt.Errorf("%s and %s have the same node name; set a distinct nodename", first, node.label())
		}
		names[node.Name] = node.label()
		allAliases[node.Name] = struct{}{}
	}
	addresses := make(map[string]struct{}, len(f.Nodes))
	controls := 0
	var impliedArch, impliedOwner, impliedRaw string
	for nodeIndex := range f.Nodes {
		node := &f.Nodes[nodeIndex]
		where := node.label()
		if _, err := image.ParseReference(node.Image); err != nil {
			return fmt.Errorf("%s image: %w", where, err)
		}
		if node.ImageArch != "" {
			raw := node.ImageRaw
			if raw == "" {
				raw = node.Image
			}
			if impliedArch == "" {
				impliedArch, impliedOwner, impliedRaw = node.ImageArch, where, raw
			} else if impliedArch != node.ImageArch {
				return fmt.Errorf("%s and %s declare different vm_arch values (%s vs %s); barn uses one guest architecture per deployment", impliedOwner, where, impliedArch, node.ImageArch)
			}
		}
		if node.Control {
			controls++
		}
		switch {
		case node.CPUs < 1 || node.CPUs > maxVirtualCPUs:
			return fmt.Errorf("%s: %d vCPUs is out of range; use 1 to %d", where, node.CPUs, maxVirtualCPUs)
		case node.Memory < minMemory:
			return fmt.Errorf("%s: %d MiB memory is below the %d MiB minimum", where, node.Memory>>20, minMemory>>20)
		case node.RootDisk <= 0:
			return fmt.Errorf("%s: root disk size must be positive", where)
		}
		if !layout.IsStatic(node.Address) {
			return fmt.Errorf("%s: address must be in %s-%s; %s-%s are reserved for the host and DHCP", where, layout.StaticStart(), layout.StaticEnd(), layout.HostAddress(), layout.DHCPEnd())
		}
		if _, exists := addresses[node.Address]; exists {
			return fmt.Errorf("%s: address %s is used by another node", where, node.Address)
		}
		addresses[node.Address] = struct{}{}
		if err := validateNodeAliases(node, where, allAliases); err != nil {
			return err
		}
		mounts, err := validateNodeDisks(node, where)
		if err != nil {
			return err
		}
		if err := f.validateNodeShares(node, where, mounts); err != nil {
			return err
		}
	}
	if impliedArch != "" && !f.archFromImage && f.Arch != "native" && f.Arch != impliedArch {
		return fmt.Errorf("%s vm_image %q targets %s, which conflicts with vm_arch %s", impliedOwner, impliedRaw, impliedArch, f.Arch)
	}
	if controls > 1 {
		return errors.New("at most one control node is allowed")
	}
	if controls == 0 {
		f.Nodes[0].Control = true
	}
	return nil
}

func validateNodeAliases(node *NodeConfig, where string, allAliases map[string]struct{}) error {
	seen := make(map[string]struct{}, len(node.HostAliases))
	for _, alias := range node.HostAliases {
		if !dnsName.MatchString(alias) || strings.Contains(alias, "..") {
			return fmt.Errorf("%s: invalid host alias %q: use lowercase DNS labels separated by dots", where, alias)
		}
		if _, exists := seen[alias]; exists {
			return fmt.Errorf("%s: duplicate host alias %q", where, alias)
		}
		if _, exists := allAliases[alias]; exists {
			return fmt.Errorf("%s: host alias %q is already a node name or another node's alias", where, alias)
		}
		seen[alias] = struct{}{}
		allAliases[alias] = struct{}{}
	}
	return nil
}

func validateNodeDisks(node *NodeConfig, where string) (map[string]struct{}, error) {
	diskNames := make(map[string]struct{}, len(node.Disks))
	mounts := make(map[string]struct{}, len(node.Disks))
	for diskIndex := range node.Disks {
		disk := &node.Disks[diskIndex]
		cleaned, ok := normalizeGuestPath(disk.Mount)
		if !ok {
			return nil, fmt.Errorf("%s: disk mount %q %s", where, disk.Mount, unsafeGuestPathReason(disk.Mount))
		}
		disk.Mount = cleaned
		switch {
		case !diskName.MatchString(disk.Name):
			return nil, fmt.Errorf("%s: invalid disk name %q", where, disk.Name)
		case disk.Size <= 0:
			return nil, fmt.Errorf("%s: disk %s size must be positive", where, disk.Mount)
		case !safeDiskMount(disk.Mount):
			return nil, fmt.Errorf("%s: disk mount %q %s", where, disk.Mount, unsafeGuestPathReason(disk.Mount))
		case !spec.ValidFilesystem(disk.Filesystem):
			return nil, fmt.Errorf("%s: disk %s fs %q must be auto, xfs, or ext4", where, disk.Mount, disk.Filesystem)
		}
		if _, exists := diskNames[disk.Name]; exists {
			return nil, fmt.Errorf("%s: duplicate disk name %q", where, disk.Name)
		}
		if _, exists := mounts[disk.Mount]; exists {
			return nil, fmt.Errorf("%s: duplicate disk mount %q", where, disk.Mount)
		}
		diskNames[disk.Name] = struct{}{}
		mounts[disk.Mount] = struct{}{}
	}
	return mounts, nil
}

func (f *File) validateNodeShares(node *NodeConfig, where string, mounts map[string]struct{}) error {
	shareGuests := make([]string, 0, len(node.Shares))
	sshDirectory := pathpkg.Join("/home", f.SSH.User, ".ssh")
	for shareIndex := range node.Shares {
		share := &node.Shares[shareIndex]
		host, err := fsutil.UserPath(share.Host, f.directory)
		if err != nil || !safeShareHost(host) {
			return fmt.Errorf("%s: share host %q must name a directory other than / on a single line", where, share.Host)
		}
		share.Host = host
		cleanedGuest, ok := normalizeGuestPath(share.Guest)
		if !ok {
			return fmt.Errorf("%s: share guest %q %s", where, share.Guest, unsafeGuestPathReason(share.Guest))
		}
		share.Guest = cleanedGuest
		if !safeShareGuest(share.Guest) {
			return fmt.Errorf("%s: share guest %q %s", where, share.Guest, unsafeGuestPathReason(share.Guest))
		}
		if guestPathOverlap(share.Guest, sshDirectory) {
			return fmt.Errorf("%s: share guest %q overlaps the SSH directory %q", where, share.Guest, sshDirectory)
		}
		for _, previous := range shareGuests {
			if guestPathOverlap(previous, share.Guest) {
				return fmt.Errorf("%s: share guests %q and %q overlap", where, previous, share.Guest)
			}
		}
		for mount := range mounts {
			if guestPathOverlap(mount, share.Guest) {
				return fmt.Errorf("%s: share guest %q overlaps disk mount %q", where, share.Guest, mount)
			}
		}
		shareGuests = append(shareGuests, share.Guest)
	}
	if len(node.Shares) == 0 {
		node.Shares = nil
	}
	return nil
}

// HostEntries adds inventory-only service names to the VM names. Keeping this
// view outside spec.Resolved lets DNS edits leave VM identity and drift intact.
func (f File) HostEntries() ([]hostconfig.Entry, error) {
	layout, err := subnet.Parse(f.Network.CIDR)
	if err != nil {
		return nil, err
	}
	entries := make([]hostconfig.Entry, 0, len(f.Nodes)+len(f.Hosts))
	for _, node := range f.Nodes {
		names := append([]string{node.Name}, node.HostAliases...)
		entries = append(entries, hostconfig.Entry{Address: node.Address, Names: names})
	}
	for _, entry := range f.Hosts {
		if _, err := layout.Last(entry.Address); err != nil {
			return nil, fmt.Errorf("vm_hosts address %q must be in %s", entry.Address, layout.CIDR())
		}
		entries = append(entries, entry)
	}
	result, err := hostconfig.Normalize(entries)
	if err != nil {
		return nil, fmt.Errorf("vm_hosts: %w", err)
	}
	return result, nil
}

func (f File) Resolve() (spec.Resolved, error) {
	f.defaults()
	if err := f.Validate(); err != nil {
		return spec.Resolved{}, failure.New(failure.Usage, err)
	}
	resolved := spec.Resolved{Schema: 1, Name: f.Name, Image: f.Defaults.Image, Network: f.Network.Mode, SSHUser: f.SSH.User, SSHWaitTimeoutNS: int64(f.SSH.WaitTimeout)}
	if f.Arch != "native" {
		resolved.Arch = f.Arch
	}
	if f.Network.Mode == "private" {
		resolved.Private = &spec.PrivateNetwork{CIDR: f.Network.CIDR, HostAddress: f.Network.HostAddress, DHCPEnd: f.Network.DHCPEnd}
	}
	for _, source := range f.Nodes {
		node := spec.Node{Name: source.Name, Control: source.Control, Address: source.Address, Aliases: append([]string(nil), source.HostAliases...), CPUs: source.CPUs, Memory: source.Memory, RootDisk: source.RootDisk}
		if source.Image != f.Defaults.Image {
			node.Image = source.Image
		}
		for _, sourceDisk := range source.Disks {
			filesystem := sourceDisk.Filesystem
			if filesystem == "auto" {
				filesystem = ""
			}
			node.Disks = append(node.Disks, spec.Disk{Name: sourceDisk.Name, Size: sourceDisk.Size, Mount: sourceDisk.Mount, Filesystem: filesystem, Persistent: sourceDisk.Persistent})
		}
		for _, sourceShare := range source.Shares {
			node.Shares = append(node.Shares, spec.Share{Host: sourceShare.Host, Guest: sourceShare.Guest, Readonly: shareReadonly(sourceShare)})
		}
		resolved.Nodes = append(resolved.Nodes, node)
	}
	return resolved, nil
}
