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

	"github.com/pgsty/farrow/internal/failure"
	"github.com/pgsty/farrow/internal/image"
	"github.com/pgsty/farrow/internal/naming"
	"github.com/pgsty/farrow/internal/network/subnet"
	"github.com/pgsty/farrow/internal/spec"
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
	reservedMounts = []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc", "/root", "/run", "/sbin", "/sys", "/usr", "/var/lib/farrow"}
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
	privateLayout := subnet.Layout{}
	if f.Network.Mode == "private" {
		var err error
		privateLayout, err = subnet.Parse(f.Network.CIDR)
		if err != nil {
			return err
		}
		if f.Network.HostAddress != privateLayout.HostAddress() || f.Network.DHCPEnd != privateLayout.DHCPEnd() {
			return fmt.Errorf("private v1 requires host %s and DHCP end %s for %s", privateLayout.HostAddress(), privateLayout.DHCPEnd(), privateLayout.CIDR())
		}
	}
	names := make(map[string]struct{})
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
			return fmt.Errorf("invalid node name %q", node.Name)
		}
		if _, exists := names[node.Name]; exists {
			return fmt.Errorf("duplicate node name %q", node.Name)
		}
		names[node.Name] = struct{}{}
		allAliases[node.Name] = struct{}{}
	}
	for nodeIndex := range f.Nodes {
		node := &f.Nodes[nodeIndex]
		if _, err := image.ParseReference(node.Image); err != nil {
			return fmt.Errorf("node %s image reference: %w", node.Name, err)
		}
		if node.Control {
			controls++
		}
		if node.CPUs < 1 || node.CPUs > maxVirtualCPUs || int64(node.Memory) < 512<<20 || int64(node.RootDisk) <= 0 {
			return fmt.Errorf("invalid CPU/memory/root resources for node %s", node.Name)
		}
		if f.Network.Mode == "private" {
			if !privateLayout.IsStatic(node.Address) {
				return fmt.Errorf("private node %s address must be in %s-%s", node.Name, privateLayout.StaticStart(), privateLayout.StaticEnd())
			}
			if _, exists := addresses[node.Address]; exists {
				return fmt.Errorf("duplicate private address %s", node.Address)
			}
			addresses[node.Address] = struct{}{}
		} else if node.Address != "" {
			return fmt.Errorf("user-network node %s must not set address", node.Name)
		}
		aliasSeen := make(map[string]struct{})
		for _, alias := range node.HostAliases {
			if !dnsName.MatchString(alias) || strings.Contains(alias, "..") {
				return fmt.Errorf("invalid host alias %q", alias)
			}
			if _, exists := aliasSeen[alias]; exists {
				return fmt.Errorf("duplicate host alias %q", alias)
			}
			if _, exists := allAliases[alias]; exists {
				return fmt.Errorf("host alias %q collides within deployment", alias)
			}
			aliasSeen[alias] = struct{}{}
			allAliases[alias] = struct{}{}
		}
		diskNames := make(map[string]struct{})
		mounts := make(map[string]struct{})
		for _, disk := range node.Disks {
			if !diskName.MatchString(disk.Name) || disk.Size <= 0 || !safeDiskMount(disk.Mount) {
				return fmt.Errorf("invalid disk %q on node %s", disk.Name, node.Name)
			}
			if !spec.ValidFilesystem(disk.Filesystem) {
				return fmt.Errorf("unsupported filesystem %q", disk.Filesystem)
			}
			if _, exists := diskNames[disk.Name]; exists {
				return fmt.Errorf("duplicate disk name %q", disk.Name)
			}
			if _, exists := mounts[disk.Mount]; exists {
				return fmt.Errorf("duplicate mount %q", disk.Mount)
			}
			diskNames[disk.Name] = struct{}{}
			mounts[disk.Mount] = struct{}{}
		}
		if len(node.Shares) > spec.MaxSharesPerNode {
			return fmt.Errorf("node %s has %d shares; maximum is %d", node.Name, len(node.Shares), spec.MaxSharesPerNode)
		}
		shareHosts := make([]string, 0, len(node.Shares))
		shareGuests := make([]string, 0, len(node.Shares))
		sshDirectory := pathpkg.Join("/home", f.SSH.User, ".ssh")
		for _, share := range node.Shares {
			if !safeShareHost(share.Host) {
				return fmt.Errorf("invalid share host %q on node %s", share.Host, node.Name)
			}
			if !safeShareGuest(share.Guest) {
				return fmt.Errorf("invalid share guest %q on node %s", share.Guest, node.Name)
			}
			if guestPathOverlap(share.Guest, sshDirectory) {
				return fmt.Errorf("share guest %q overlaps SSH directory %q on node %s", share.Guest, sshDirectory, node.Name)
			}
			for _, previous := range shareHosts {
				if hostPathOverlap(previous, share.Host) {
					return fmt.Errorf("overlapping share hosts %q and %q on node %s", previous, share.Host, node.Name)
				}
			}
			for _, previous := range shareGuests {
				if guestPathOverlap(previous, share.Guest) {
					return fmt.Errorf("overlapping share guests %q and %q on node %s", previous, share.Guest, node.Name)
				}
			}
			for mount := range mounts {
				if guestPathOverlap(mount, share.Guest) {
					return fmt.Errorf("share guest %q overlaps data disk mount %q on node %s", share.Guest, mount, node.Name)
				}
			}
			readonly := shareReadonly(share)
			for _, previous := range deploymentShareHosts {
				if previous.node != node.Name && hostPathOverlap(previous.path, share.Host) && (!previous.readonly || !readonly) {
					return fmt.Errorf("cross-node share hosts %q on %s and %q on %s overlap with read-write access", previous.path, previous.node, share.Host, node.Name)
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
