package config

import (
	"errors"
	"fmt"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/image"
	"github.com/pgsty/barn/internal/naming"
	"github.com/pgsty/barn/internal/network/subnet"
	"github.com/pgsty/barn/internal/spec"
	"go.yaml.in/yaml/v3"
)

type Size int64

func (s *Size) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("size must be a quoted or plain string with an explicit unit")
	}
	value, err := ParseSize(node.Value)
	if err != nil {
		return err
	}
	*s = Size(value)
	return nil
}

const maxVirtualCPUs = 256

func (s Size) MarshalYAML() (any, error) {
	value := int64(s)
	for _, unit := range []struct {
		name string
		size int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if value >= unit.size && value%unit.size == 0 {
			return strconv.FormatInt(value/unit.size, 10) + unit.name, nil
		}
	}
	return strconv.FormatInt(value, 10) + "B", nil
}

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("duration must be a string")
	}
	value, err := time.ParseDuration(node.Value)
	if err != nil || value <= 0 {
		return fmt.Errorf("invalid positive duration %q", node.Value)
	}
	*d = Duration(value)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

type NetworkConfig struct {
	Mode        string `yaml:"mode"`
	CIDR        string `yaml:"cidr,omitempty"`
	HostAddress string `yaml:"host_address,omitempty"`
	DHCPEnd     string `yaml:"dhcp_end,omitempty"`
}

type DefaultsConfig struct {
	Image    string `yaml:"image,omitempty"`
	CPUs     int    `yaml:"cpus,omitempty"`
	Memory   Size   `yaml:"memory,omitempty"`
	RootDisk Size   `yaml:"root_disk,omitempty"`
}

type SSHConfig struct {
	User        string   `yaml:"user,omitempty"`
	WaitTimeout Duration `yaml:"wait_timeout,omitempty"`
}

type DiskConfig struct {
	Name       string `yaml:"name"`
	Size       Size   `yaml:"size"`
	Mount      string `yaml:"mount"`
	Filesystem string `yaml:"filesystem,omitempty"`
	Persistent bool   `yaml:"persistent,omitempty"`
}

type ShareConfig struct {
	Host     string `yaml:"host"`
	Guest    string `yaml:"guest"`
	Readonly *bool  `yaml:"readonly,omitempty"`
}

func shareReadonly(share ShareConfig) bool { return share.Readonly == nil || *share.Readonly }

func shareReadonlyYAML(readonly bool) *bool {
	if readonly {
		return nil
	}
	value := false
	return &value
}

type NodeConfig struct {
	Name        string        `yaml:"name"`
	Control     bool          `yaml:"control,omitempty"`
	Address     string        `yaml:"address,omitempty"`
	HostAliases []string      `yaml:"host_aliases,omitempty"`
	Image       string        `yaml:"image,omitempty"`
	CPUs        int           `yaml:"cpus,omitempty"`
	Memory      Size          `yaml:"memory,omitempty"`
	RootDisk    Size          `yaml:"root_disk,omitempty"`
	Disks       []DiskConfig  `yaml:"disks,omitempty"`
	Shares      []ShareConfig `yaml:"shares,omitempty"`
}

type File struct {
	Version  int            `yaml:"version"`
	Name     string         `yaml:"name"`
	Arch     string         `yaml:"arch,omitempty"`
	Network  NetworkConfig  `yaml:"network"`
	Defaults DefaultsConfig `yaml:"defaults,omitempty"`
	SSH      SSHConfig      `yaml:"ssh,omitempty"`
	Nodes    []NodeConfig   `yaml:"nodes"`
}

var (
	sshUser        = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	dnsName        = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
	diskName       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	shareGuestPath = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
	reservedMounts = []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc", "/root", "/run", "/sbin", "/sys", "/usr", "/var/lib/barn"}
)

func safeDiskMount(value string) bool {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == "/" {
		return false
	}
	for _, root := range reservedMounts {
		if guestPathOverlap(value, root) {
			return false
		}
	}
	return true
}

// unsafeGuestPathReason explains why safeDiskMount rejected a guest path.
func unsafeGuestPathReason(value string) string {
	for _, root := range reservedMounts {
		if guestPathOverlap(value, root) {
			return "overlaps the reserved system path " + root
		}
	}
	return "must be a clean absolute path other than /"
}

// label names a node the way the inventory does: by its host key.
func (node NodeConfig) label() string {
	if node.Address == "" {
		return "node " + node.Name
	}
	return fmt.Sprintf("host %s (%s)", node.Address, node.Name)
}

// minMemory is the smallest guest memory Barn boots.
const minMemory = 512 << 20

func safeShareHost(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value && value != "/" && !strings.ContainsAny(value, "\x00\r\n")
}

func safeShareGuest(value string) bool {
	return shareGuestPath.MatchString(value) && pathpkg.IsAbs(value) && pathpkg.Clean(value) == value && safeDiskMount(value)
}

func cleanPathOverlap(first, second, separator string) bool {
	return first == second || strings.HasPrefix(first, second+separator) || strings.HasPrefix(second, first+separator)
}

func hostPathOverlap(first, second string) bool {
	return cleanPathOverlap(first, second, string(filepath.Separator))
}

func guestPathOverlap(first, second string) bool {
	return cleanPathOverlap(first, second, "/")
}

func (f *File) defaults() {
	if f.Arch == "" {
		f.Arch = "native"
	}
	if f.Network.Mode == "" {
		f.Network.Mode = "private"
	}
	if f.Defaults.Image == "" {
		f.Defaults.Image = defaultImage
	}
	if normalized, err := image.CanonicalReference(f.Defaults.Image); err == nil {
		f.Defaults.Image = normalized
	}
	if f.Defaults.CPUs == 0 {
		f.Defaults.CPUs = 1
	}
	if f.Defaults.Memory == 0 {
		f.Defaults.Memory = Size(2 * spec.GiB)
	}
	if f.Defaults.RootDisk == 0 {
		f.Defaults.RootDisk = Size(64 * spec.GiB)
	}
	if f.SSH.User == "" {
		f.SSH.User = "dba"
	}
	if f.SSH.WaitTimeout == 0 {
		f.SSH.WaitTimeout = Duration(180 * time.Second)
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
	for index := range f.Nodes {
		node := &f.Nodes[index]
		if node.Image == "" {
			node.Image = f.Defaults.Image
		} else if normalized, err := image.CanonicalReference(node.Image); err == nil {
			node.Image = normalized
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
	if len(f.Nodes) == 0 || len(f.Nodes) > 20 {
		return errors.New("configuration requires 1..20 nodes")
	}
	layout, err := subnet.Parse(f.Network.CIDR)
	if err != nil {
		return err
	}
	if f.Network.HostAddress != layout.HostAddress() || f.Network.DHCPEnd != layout.DHCPEnd() {
		return fmt.Errorf("network %s requires host address %s and DHCP end %s", layout.CIDR(), layout.HostAddress(), layout.DHCPEnd())
	}
	names := make(map[string]string) // name -> node label
	addresses := make(map[string]struct{})
	allAliases := make(map[string]struct{})
	controls := 0
	type deploymentShareHost struct {
		path     string
		readonly bool
		node     string
	}
	deploymentShareHosts := make([]deploymentShareHost, 0)
	for _, node := range f.Nodes {
		if !naming.ValidNodeName(node.Name) {
			return fmt.Errorf("%s: invalid node name %q: %s", node.label(), node.Name, naming.NodeNameRule)
		}
		if first, exists := names[node.Name]; exists {
			return fmt.Errorf("%s and %s have the same node name; set a distinct nodename", first, node.label())
		}
		names[node.Name] = node.label()
		allAliases[node.Name] = struct{}{}
	}
	for nodeIndex := range f.Nodes {
		node := &f.Nodes[nodeIndex]
		where := node.label()
		if _, err := image.ParseReference(node.Image); err != nil {
			return fmt.Errorf("%s image: %w", where, err)
		}
		if node.Control {
			controls++
		}
		switch {
		case node.CPUs < 1 || node.CPUs > maxVirtualCPUs:
			return fmt.Errorf("%s: %d vCPUs is out of range; use 1 to %d", where, node.CPUs, maxVirtualCPUs)
		case int64(node.Memory) < minMemory:
			return fmt.Errorf("%s: %d MiB memory is below the %d MiB minimum", where, int64(node.Memory)>>20, minMemory>>20)
		case int64(node.RootDisk) <= 0:
			return fmt.Errorf("%s: root disk size must be positive", where)
		}
		if !layout.IsStatic(node.Address) {
			return fmt.Errorf("%s: address must be in %s-%s; %s-%s are reserved for the host and DHCP", where, layout.StaticStart(), layout.StaticEnd(), layout.HostAddress(), layout.DHCPEnd())
		}
		if _, exists := addresses[node.Address]; exists {
			return fmt.Errorf("%s: address %s is used by another node", where, node.Address)
		}
		addresses[node.Address] = struct{}{}
		aliasSeen := make(map[string]struct{})
		for _, alias := range node.HostAliases {
			if !dnsName.MatchString(alias) || strings.Contains(alias, "..") {
				return fmt.Errorf("%s: invalid host alias %q: use lowercase DNS labels separated by dots", where, alias)
			}
			if _, exists := aliasSeen[alias]; exists {
				return fmt.Errorf("%s: duplicate host alias %q", where, alias)
			}
			if _, exists := allAliases[alias]; exists {
				return fmt.Errorf("%s: host alias %q is already a node name or another node's alias", where, alias)
			}
			aliasSeen[alias] = struct{}{}
			allAliases[alias] = struct{}{}
		}
		diskNames := make(map[string]struct{})
		mounts := make(map[string]struct{})
		for _, disk := range node.Disks {
			switch {
			case !diskName.MatchString(disk.Name):
				return fmt.Errorf("%s: invalid disk name %q", where, disk.Name)
			case disk.Size <= 0:
				return fmt.Errorf("%s: disk %s size must be positive", where, disk.Mount)
			case !safeDiskMount(disk.Mount):
				return fmt.Errorf("%s: disk mount %q %s", where, disk.Mount, unsafeGuestPathReason(disk.Mount))
			case !spec.ValidFilesystem(disk.Filesystem):
				return fmt.Errorf("%s: disk %s fs %q must be auto, xfs, or ext4", where, disk.Mount, disk.Filesystem)
			}
			if _, exists := diskNames[disk.Name]; exists {
				return fmt.Errorf("%s: duplicate disk name %q", where, disk.Name)
			}
			if _, exists := mounts[disk.Mount]; exists {
				return fmt.Errorf("%s: duplicate disk mount %q", where, disk.Mount)
			}
			diskNames[disk.Name] = struct{}{}
			mounts[disk.Mount] = struct{}{}
		}
		if len(node.Shares) > spec.MaxSharesPerNode {
			return fmt.Errorf("%s has %d shares; maximum is %d", where, len(node.Shares), spec.MaxSharesPerNode)
		}
		shareHosts := make([]string, 0, len(node.Shares))
		shareGuests := make([]string, 0, len(node.Shares))
		sshDirectory := pathpkg.Join("/home", f.SSH.User, ".ssh")
		for _, share := range node.Shares {
			if !safeShareHost(share.Host) {
				return fmt.Errorf("%s: share host %q must be a clean absolute path other than / (~ and relative paths are not expanded)", where, share.Host)
			}
			if !safeShareGuest(share.Guest) {
				return fmt.Errorf("%s: share guest %q %s", where, share.Guest, unsafeGuestPathReason(share.Guest))
			}
			if guestPathOverlap(share.Guest, sshDirectory) {
				return fmt.Errorf("%s: share guest %q overlaps the SSH directory %q", where, share.Guest, sshDirectory)
			}
			for _, previous := range shareHosts {
				if hostPathOverlap(previous, share.Host) {
					return fmt.Errorf("%s: share hosts %q and %q overlap", where, previous, share.Host)
				}
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
			readonly := shareReadonly(share)
			for _, previous := range deploymentShareHosts {
				if previous.node != node.Name && hostPathOverlap(previous.path, share.Host) && (!previous.readonly || !readonly) {
					return fmt.Errorf("share host %q on %s and %q on %s overlap with read-write access; make both readonly or separate them", previous.path, previous.node, share.Host, node.Name)
				}
			}
			shareHosts = append(shareHosts, share.Host)
			shareGuests = append(shareGuests, share.Guest)
			deploymentShareHosts = append(deploymentShareHosts, deploymentShareHost{path: share.Host, readonly: readonly, node: node.Name})
		}
		if len(node.Shares) == 0 {
			node.Shares = nil
		}
	}
	if controls > 1 {
		return errors.New("at most one control node is allowed")
	}
	if controls == 0 {
		f.Nodes[0].Control = true
	}
	return nil
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
		node := spec.Node{Name: source.Name, Control: source.Control, Address: source.Address, Aliases: append([]string(nil), source.HostAliases...), CPUs: source.CPUs, Memory: int64(source.Memory), RootDisk: int64(source.RootDisk)}
		if source.Image != f.Defaults.Image {
			node.Image = source.Image
		}
		for _, sourceDisk := range source.Disks {
			filesystem := sourceDisk.Filesystem
			if filesystem == "auto" {
				filesystem = ""
			}
			node.Disks = append(node.Disks, spec.Disk{Name: sourceDisk.Name, Size: int64(sourceDisk.Size), Mount: sourceDisk.Mount, Filesystem: filesystem, Persistent: sourceDisk.Persistent})
		}
		for _, sourceShare := range source.Shares {
			node.Shares = append(node.Shares, spec.Share{Host: sourceShare.Host, Guest: sourceShare.Guest, Readonly: shareReadonly(sourceShare)})
		}
		resolved.Nodes = append(resolved.Nodes, node)
	}
	return resolved, nil
}
