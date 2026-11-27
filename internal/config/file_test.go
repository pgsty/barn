package config

import (
	"fmt"
	"testing"

	"github.com/pgsty/barn/internal/spec"
)

func TestFormatSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		bytes int64
		want  string
	}{
		{2 * spec.GiB, "2GiB"},
		{64 * spec.GiB, "64GiB"},
		{512 << 20, "512MiB"},
		{1 << 40, "1TiB"},
		{1536, "1536B"},
	}
	for _, tc := range cases {
		if got := FormatSize(tc.bytes); got != tc.want {
			t.Errorf("FormatSize(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

func TestResolveDefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	file := File{
		Version:  1,
		Name:     "full",
		Network:  NetworkConfig{Mode: "private"},
		Defaults: DefaultsConfig{Image: "u24", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 64 * spec.GiB},
		SSH:      SSHConfig{User: "dba"},
		Nodes: []NodeConfig{
			{
				Name: "meta", Control: true, Address: "10.10.10.10", CPUs: 2, Memory: 4 * spec.GiB,
				Disks: []DiskConfig{{Name: "data", Size: 128 * spec.GiB, Mount: "/data"}},
			},
			{Name: "node-1", Address: "10.10.10.11"},
		},
	}
	resolved, err := file.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Network != "private" || resolved.Private.DHCPEnd != "10.10.10.8" || len(resolved.Nodes) != 2 || resolved.Nodes[0].Memory != 4*spec.GiB || resolved.Nodes[0].Disks[0].Size != 128*spec.GiB {
		t.Fatalf("resolved = %#v", resolved)
	}
}

func TestExplicitGuestArchitectureResolves(t *testing.T) {
	t.Parallel()
	file := File{
		Version: 1, Name: "arch", Arch: "arm64",
		Network: NetworkConfig{Mode: "private"},
		Nodes:   []NodeConfig{{Name: "meta", Address: "10.10.10.10"}},
	}
	resolved, err := file.Resolve()
	if err != nil || resolved.Arch != "arm64" {
		t.Fatalf("resolved arch = %q, %v", resolved.Arch, err)
	}
	bad := file
	bad.Arch = "s390x"
	if err := bad.Validate(); err == nil {
		t.Fatal("unsupported architecture accepted")
	}
}

func TestChannelReferenceStaysStableInResolvedSpec(t *testing.T) {
	t.Parallel()
	file := File{
		Version:  1,
		Name:     "channel",
		Network:  NetworkConfig{Mode: "private"},
		Defaults: DefaultsConfig{Image: "debian13:testing"},
		Nodes:    []NodeConfig{{Name: "meta", Address: "10.10.10.10"}},
	}
	resolved, err := file.Resolve()
	if err != nil || resolved.Image != "d13:testing" {
		t.Fatalf("resolved channel reference = %q, %v", resolved.Image, err)
	}
}

func TestPrivateAddressValidation(t *testing.T) {
	t.Parallel()
	for _, file := range []File{
		{Version: 1, Name: "bad", Network: NetworkConfig{Mode: "private"}, Nodes: []NodeConfig{{Name: "meta", Address: "10.10.10.8"}}},
		{Version: 1, Name: "bad", Network: NetworkConfig{Mode: "user"}, Nodes: []NodeConfig{{Name: "meta"}}},
	} {
		if err := file.Validate(); err == nil {
			t.Errorf("invalid config accepted: %#v", file)
		}
	}
}

func TestCustomPrivateNetworkResolvesLayout(t *testing.T) {
	t.Parallel()
	file := File{
		Version: 1, Name: "custom",
		Network: NetworkConfig{Mode: "private", CIDR: "172.31.251.0/24"},
		Nodes: []NodeConfig{
			{Name: "meta", Address: "172.31.251.10"},
			{Name: "node-1", Address: "172.31.251.11"},
		},
	}
	resolved, err := file.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Private.HostAddress != "172.31.251.1" || resolved.Private.DHCPEnd != "172.31.251.8" {
		t.Fatalf("custom private = %#v", resolved.Private)
	}
}

func TestCustomPrivateNetworkRequiresCoordinatedLayout(t *testing.T) {
	t.Parallel()
	for _, file := range []File{
		{Version: 1, Name: "bad", Network: NetworkConfig{Mode: "private", CIDR: "8.8.8.0/24"}, Nodes: []NodeConfig{{Name: "meta", Address: "8.8.8.10"}}},
		{Version: 1, Name: "bad", Network: NetworkConfig{Mode: "private", CIDR: "172.31.251.0/24", HostAddress: "172.31.251.2"}, Nodes: []NodeConfig{{Name: "meta", Address: "172.31.251.10"}}},
		{Version: 1, Name: "bad", Network: NetworkConfig{Mode: "private", CIDR: "172.31.251.0/24"}, Nodes: []NodeConfig{{Name: "meta", Address: "10.10.10.10"}}},
	} {
		if err := file.Validate(); err == nil {
			t.Fatalf("accepted unsafe custom layout: %#v", file)
		}
	}
}

func TestDiskMountTraversalAndSystemPathsAreRejected(t *testing.T) {
	t.Parallel()
	for _, mount := range []string{"/", "/data/../etc", "relative", "/proc/data", "/etc", "/usr/lib/barn", "/var/lib/barn/data"} {
		file := File{
			Version: 1, Name: "quick", Network: NetworkConfig{Mode: "private"},
			Nodes: []NodeConfig{{Name: "meta", Address: "10.10.10.10", Disks: []DiskConfig{{Name: "data", Size: spec.GiB, Mount: mount}}}},
		}
		if err := file.Validate(); err == nil {
			t.Errorf("unsafe disk mount %q accepted", mount)
		}
	}
}

func TestShareResolveDefaultsReadonly(t *testing.T) {
	t.Parallel()
	readWrite := false
	file := File{
		Version: 1, Name: "quick", Network: NetworkConfig{Mode: "private"},
		Nodes: []NodeConfig{{
			Name: "meta", Address: "10.10.10.10",
			Shares: []ShareConfig{
				{Host: "/srv/pigsty-ro", Guest: "/src-ro"},
				{Host: "/srv/pigsty-rw", Guest: "/src-rw", Readonly: &readWrite},
			},
		}},
	}
	resolved, err := file.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	shares := resolved.Nodes[0].Shares
	if len(shares) != 2 || !shares[0].Readonly || shares[1].Readonly {
		t.Fatalf("resolved share readonly defaults = %#v", shares)
	}
}

func TestEmptyShareListNormalizesToNil(t *testing.T) {
	t.Parallel()
	file := File{
		Version: 1, Name: "quick", Network: NetworkConfig{Mode: "private"},
		Nodes: []NodeConfig{{Name: "meta", Address: "10.10.10.10", Shares: []ShareConfig{}}},
	}
	resolved, err := file.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Nodes[0].Shares != nil {
		t.Fatalf("resolved empty shares were not normalized: %#v", resolved.Nodes[0].Shares)
	}
}

func TestShareStructuralPathValidation(t *testing.T) {
	t.Parallel()
	valid := func() File {
		return File{Version: 1, Name: "quick", Network: NetworkConfig{Mode: "private"}, Nodes: []NodeConfig{{Name: "meta", Address: "10.10.10.10", Shares: []ShareConfig{{Host: "/srv/pigsty", Guest: "/src"}}}}}
	}
	for _, host := range []string{"", "/", "/srv/pigsty\x00bad", "/srv/pigsty\nbad", "/srv/pigsty\rbad"} {
		file := valid()
		file.Nodes[0].Shares[0].Host = host
		if err := file.Validate(); err == nil {
			t.Errorf("unsafe share host %q accepted", host)
		}
	}
	for _, guest := range []string{"relative", "/", "/proc/src", "/usr/local/src", "/var", "/var/lib", "/var/lib/barn/src", "/home", "/home/dba", "/home/dba/.ssh", "/home/dba/.ssh/cache", "/src\tcode", "/src\ncode"} {
		file := valid()
		file.Nodes[0].Shares[0].Guest = guest
		if err := file.Validate(); err == nil {
			t.Errorf("unsafe share guest %q accepted", guest)
		}
	}
	normalized := valid()
	normalized.Nodes[0].Shares[0].Guest = "/src//nested/../code"
	if err := normalized.Validate(); err != nil {
		t.Fatalf("normalizable guest path was rejected: %v", err)
	}
	if got := normalized.Nodes[0].Shares[0].Guest; got != "/src/code" {
		t.Fatalf("guest path was not normalized: %q", got)
	}
	allowed := valid()
	allowed.Nodes[0].Shares[0].Guest = "/home/dba/源码 files/$build"
	if err := allowed.Validate(); err != nil {
		t.Fatalf("non-overlapping SSH-user work path was rejected: %v", err)
	}
	custom := valid()
	custom.SSH.User = "operator"
	custom.Nodes[0].Shares[0].Guest = "/home/operator"
	if err := custom.Validate(); err == nil {
		t.Fatal("share masking the custom SSH user's .ssh ancestor was accepted")
	}
}

func TestShareOverlapValidationAllowsManySources(t *testing.T) {
	t.Parallel()
	base := func(shares []ShareConfig, disks []DiskConfig) File {
		return File{Version: 1, Name: "quick", Network: NetworkConfig{Mode: "private"}, Nodes: []NodeConfig{{Name: "meta", Address: "10.10.10.10", Shares: shares, Disks: disks}}}
	}
	tests := []File{
		base([]ShareConfig{{Host: "/srv/a", Guest: "/src"}, {Host: "/srv/b", Guest: "/src"}}, nil),
		base([]ShareConfig{{Host: "/srv/a", Guest: "/src"}, {Host: "/srv/b", Guest: "/src/nested"}}, nil),
		base([]ShareConfig{{Host: "/srv/a", Guest: "/data/src"}}, []DiskConfig{{Name: "data", Size: spec.GiB, Mount: "/data"}}),
		base([]ShareConfig{{Host: "/srv/a", Guest: "/data"}}, []DiskConfig{{Name: "data", Size: spec.GiB, Mount: "/data/nested"}}),
	}
	for index, file := range tests {
		if err := file.Validate(); err == nil {
			t.Errorf("overlapping share configuration %d accepted: %#v", index, file.Nodes[0])
		}
	}
	shares := make([]ShareConfig, 16)
	for index := range shares {
		shares[index] = ShareConfig{Host: fmt.Sprintf("/srv/share-%d", index), Guest: fmt.Sprintf("/src-%d", index)}
	}
	many := base(shares, nil)
	if err := many.Validate(); err != nil {
		t.Fatalf("independent source trees rejected: %v", err)
	}
}

func TestSameReadonlyHostShareIsAllowedAcrossFourNodes(t *testing.T) {
	t.Parallel()
	file := File{
		Version: 1, Name: "full", Network: NetworkConfig{Mode: "private"},
		Nodes: []NodeConfig{
			{Name: "meta", Control: true, Address: "10.10.10.10", Shares: []ShareConfig{{Host: "/srv/pigsty", Guest: "/src"}}},
			{Name: "node-1", Address: "10.10.10.11", Shares: []ShareConfig{{Host: "/srv/pigsty", Guest: "/src"}}},
			{Name: "node-2", Address: "10.10.10.12", Shares: []ShareConfig{{Host: "/srv/pigsty", Guest: "/src"}}},
			{Name: "node-3", Address: "10.10.10.13", Shares: []ShareConfig{{Host: "/srv/pigsty", Guest: "/src"}}},
		},
	}
	resolved, err := file.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Nodes) != 4 {
		t.Fatalf("cross-node shares were not preserved: %#v", resolved.Nodes)
	}
	for _, node := range resolved.Nodes {
		if len(node.Shares) != 1 || !node.Shares[0].Readonly {
			t.Errorf("node %s did not retain its default-readonly share: %#v", node.Name, node.Shares)
		}
	}
	nested := file
	nested.Nodes = append([]NodeConfig(nil), file.Nodes...)
	for index := 1; index < len(nested.Nodes); index++ {
		nested.Nodes[index].Shares = []ShareConfig{{Host: fmt.Sprintf("/srv/pigsty/node-%d", index), Guest: "/src"}}
	}
	if _, err := nested.Resolve(); err != nil {
		t.Fatalf("nested cross-node readonly hosts were rejected: %v", err)
	}
}

func TestCrossNodeOverlappingHostAllowsReadWriteShares(t *testing.T) {
	t.Parallel()
	readWrite := false
	for _, secondHost := range []string{"/srv/pigsty", "/srv/pigsty/nested", "/srv"} {
		file := File{
			Version: 1, Name: "full", Network: NetworkConfig{Mode: "private"},
			Nodes: []NodeConfig{
				{Name: "meta", Control: true, Address: "10.10.10.10", Shares: []ShareConfig{{Host: "/srv/pigsty", Guest: "/src"}}},
				{Name: "node-1", Address: "10.10.10.11", Shares: []ShareConfig{{Host: secondHost, Guest: "/src", Readonly: &readWrite}}},
			},
		}
		if err := file.Validate(); err != nil {
			t.Errorf("cross-node RO/RW overlap with %q: %v", secondHost, err)
		}
	}
	reversed := File{
		Version: 1, Name: "full", Network: NetworkConfig{Mode: "private"},
		Nodes: []NodeConfig{
			{Name: "meta", Control: true, Address: "10.10.10.10", Shares: []ShareConfig{{Host: "/srv/pigsty", Guest: "/src", Readonly: &readWrite}}},
			{Name: "node-1", Address: "10.10.10.11", Shares: []ShareConfig{{Host: "/srv/pigsty/nested", Guest: "/src"}}},
		},
	}
	if err := reversed.Validate(); err != nil {
		t.Fatalf("cross-node RW/RO overlap: %v", err)
	}
}
