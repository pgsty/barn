package config

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/spec"
	"go.yaml.in/yaml/v3"
)

// decodeTestFile mirrors the strict single-document decoding the retired
// declarative loader performed, so File.Validate/Resolve rules stay covered
// without resurrecting a production YAML entry point.
func decodeTestFile(input string) (File, error) {
	decoder := yaml.NewDecoder(strings.NewReader(input))
	decoder.KnownFields(true)
	var file File
	if err := decoder.Decode(&file); err != nil {
		return File{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return File{}, errors.New("configuration contains multiple YAML documents")
	}
	if err := file.Validate(); err != nil {
		return File{}, err
	}
	return file, nil
}

func TestStrictYAMLResolve(t *testing.T) {
	t.Parallel()
	input := `version: 1
name: full
network:
  mode: private
defaults:
  image: u24
  cpus: 1
  memory: 2GiB
  root_disk: 64GiB
ssh:
  user: dba
nodes:
  - name: meta
    control: true
    address: 10.10.10.10
    cpus: 2
    memory: 4GiB
    disks:
      - name: data
        size: 128GiB
        mount: /data
  - name: node-1
    address: 10.10.10.11
`
	file, err := decodeTestFile(input)
	if err != nil {
		t.Fatal(err)
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
	file, err := decodeTestFile("version: 1\nname: arch\narch: arm64\nnetwork: {mode: private}\nnodes: [{name: meta, address: 10.10.10.10}]\n")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := file.Resolve()
	if err != nil || resolved.Arch != "arm64" {
		t.Fatalf("resolved arch = %q, %v", resolved.Arch, err)
	}
	if _, err := decodeTestFile("version: 1\nname: arch\narch: s390x\nnetwork: {mode: private}\nnodes: [{name: meta, address: 10.10.10.10}]\n"); err == nil {
		t.Fatal("unsupported architecture accepted")
	}
}

func TestChannelReferenceStaysStableInResolvedSpec(t *testing.T) {
	t.Parallel()
	file, err := decodeTestFile("version: 1\nname: channel\nnetwork: {mode: private}\ndefaults: {image: debian13:testing}\nnodes: [{name: meta, address: 10.10.10.10}]\n")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := file.Resolve()
	if err != nil || resolved.Image != "d13:testing" {
		t.Fatalf("resolved channel reference = %q, %v", resolved.Image, err)
	}
}

func TestStrictYAMLRejectsUnknownAndMultipleDocuments(t *testing.T) {
	t.Parallel()
	base := "version: 1\nname: quick\nnetwork: {mode: private}\nnodes: [{name: meta, address: 10.10.10.10}]\n"
	if _, err := decodeTestFile(base + "unknown: true\n"); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := decodeTestFile(base + "---\n" + base); err == nil {
		t.Fatal("multiple documents accepted")
	}
}

func TestPrivateAddressValidation(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		"version: 1\nname: bad\nnetwork: {mode: private}\nnodes: [{name: meta, address: 10.10.10.8}]\n",
		"version: 1\nname: bad\nnetwork: {mode: user}\nnodes: [{name: meta}]\n",
	} {
		if _, err := decodeTestFile(input); err == nil {
			t.Errorf("invalid config accepted:\n%s", input)
		}
	}
}

func TestCustomPrivateNetworkResolvesLayout(t *testing.T) {
	t.Parallel()
	input := `version: 1
name: custom
network:
  mode: private
  cidr: 172.31.251.0/24
nodes:
  - {name: meta, address: 172.31.251.10}
  - {name: node-1, address: 172.31.251.11}
`
	file, err := decodeTestFile(input)
	if err != nil {
		t.Fatal(err)
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
	for _, input := range []string{
		"version: 1\nname: bad\nnetwork: {mode: private, cidr: 8.8.8.0/24}\nnodes: [{name: meta, address: 8.8.8.10}]\n",
		"version: 1\nname: bad\nnetwork: {mode: private, cidr: 172.31.251.0/24, host_address: 172.31.251.2}\nnodes: [{name: meta, address: 172.31.251.10}]\n",
		"version: 1\nname: bad\nnetwork: {mode: private, cidr: 172.31.251.0/24}\nnodes: [{name: meta, address: 10.10.10.10}]\n",
	} {
		if _, err := decodeTestFile(input); err == nil {
			t.Fatalf("accepted unsafe custom layout:\n%s", input)
		}
	}
}

func TestDiskMountTraversalAndSystemPathsAreRejected(t *testing.T) {
	t.Parallel()
	for _, mount := range []string{"/", "/data/../etc", "relative", "/proc/data", "/etc", "/usr/lib/barn", "/var/lib/barn/data"} {
		input := "version: 1\nname: quick\nnetwork: {mode: private}\nnodes:\n  - name: meta\n    address: 10.10.10.10\n    disks:\n      - {name: data, size: 1GiB, mount: " + fmt.Sprintf("%q", mount) + "}\n"
		if _, err := decodeTestFile(input); err == nil {
			t.Errorf("unsafe disk mount %q accepted", mount)
		}
	}
}

func TestShareResolveDefaultsReadonly(t *testing.T) {
	t.Parallel()
	input := `version: 1
name: quick
network: {mode: private}
nodes:
  - name: meta
    address: 10.10.10.10
    shares:
      - {host: /srv/pigsty-ro, guest: /src-ro}
      - {host: /srv/pigsty-rw, guest: /src-rw, readonly: false}
`
	file, err := decodeTestFile(input)
	if err != nil {
		t.Fatal(err)
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
	file, err := decodeTestFile("version: 1\nname: quick\nnetwork: {mode: private}\nnodes: [{name: meta, address: 10.10.10.10, shares: []}]\n")
	if err != nil {
		t.Fatal(err)
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
	for _, host := range []string{"relative", "/", "/srv/../srv/pigsty", "/srv/pigsty\x00bad", "/srv/pigsty\nbad", "/srv/pigsty\rbad"} {
		file := valid()
		file.Nodes[0].Shares[0].Host = host
		if err := file.Validate(); err == nil {
			t.Errorf("unsafe share host %q accepted", host)
		}
	}
	for _, guest := range []string{"relative", "/", "/src/../src", "/src//nested", "/proc/src", "/usr/local/src", "/var", "/var/lib", "/var/lib/barn/src", "/home", "/home/dba", "/home/dba/.ssh", "/home/dba/.ssh/cache", "/src code", "/src:code"} {
		file := valid()
		file.Nodes[0].Shares[0].Guest = guest
		if err := file.Validate(); err == nil {
			t.Errorf("unsafe share guest %q accepted", guest)
		}
	}
	allowed := valid()
	allowed.Nodes[0].Shares[0].Guest = "/home/dba/work"
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

func TestShareOverlapAndLimitValidation(t *testing.T) {
	t.Parallel()
	base := func(shares []ShareConfig, disks []DiskConfig) File {
		return File{Version: 1, Name: "quick", Network: NetworkConfig{Mode: "private"}, Nodes: []NodeConfig{{Name: "meta", Address: "10.10.10.10", Shares: shares, Disks: disks}}}
	}
	tests := []File{
		base([]ShareConfig{{Host: "/srv/a", Guest: "/src-a"}, {Host: "/srv/a", Guest: "/src-b"}}, nil),
		base([]ShareConfig{{Host: "/srv/a", Guest: "/src-a"}, {Host: "/srv/a/nested", Guest: "/src-b"}}, nil),
		base([]ShareConfig{{Host: "/srv/a", Guest: "/src"}, {Host: "/srv/b", Guest: "/src"}}, nil),
		base([]ShareConfig{{Host: "/srv/a", Guest: "/src"}, {Host: "/srv/b", Guest: "/src/nested"}}, nil),
		base([]ShareConfig{{Host: "/srv/a", Guest: "/data/src"}}, []DiskConfig{{Name: "data", Size: Size(spec.GiB), Mount: "/data"}}),
		base([]ShareConfig{{Host: "/srv/a", Guest: "/data"}}, []DiskConfig{{Name: "data", Size: Size(spec.GiB), Mount: "/data/nested"}}),
	}
	for index, file := range tests {
		if err := file.Validate(); err == nil {
			t.Errorf("overlapping share configuration %d accepted: %#v", index, file.Nodes[0])
		}
	}
	tooMany := make([]ShareConfig, spec.MaxSharesPerNode+1)
	for index := range tooMany {
		tooMany[index] = ShareConfig{Host: fmt.Sprintf("/srv/share-%d", index), Guest: fmt.Sprintf("/src-%d", index)}
	}
	atLimit := base(tooMany[:spec.MaxSharesPerNode], nil)
	if err := atLimit.Validate(); err != nil {
		t.Fatalf("exactly %d node shares were rejected: %v", spec.MaxSharesPerNode, err)
	}
	tooManyFile := base(tooMany, nil)
	if err := tooManyFile.Validate(); err == nil {
		t.Fatalf("more than %d node shares accepted", spec.MaxSharesPerNode)
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

func TestCrossNodeOverlappingHostRejectsAnyReadWriteShare(t *testing.T) {
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
		if err := file.Validate(); err == nil {
			t.Errorf("cross-node RO/RW overlap with %q accepted", secondHost)
		}
	}
	reversed := File{
		Version: 1, Name: "full", Network: NetworkConfig{Mode: "private"},
		Nodes: []NodeConfig{
			{Name: "meta", Control: true, Address: "10.10.10.10", Shares: []ShareConfig{{Host: "/srv/pigsty", Guest: "/src", Readonly: &readWrite}}},
			{Name: "node-1", Address: "10.10.10.11", Shares: []ShareConfig{{Host: "/srv/pigsty/nested", Guest: "/src"}}},
		},
	}
	if err := reversed.Validate(); err == nil {
		t.Fatal("cross-node RW/RO overlap was accepted when the RW share appeared first")
	}
}

func TestStrictYAMLRejectsUnknownShareField(t *testing.T) {
	t.Parallel()
	input := "version: 1\nname: quick\nnetwork: {mode: private}\nnodes:\n  - name: meta\n    address: 10.10.10.10\n    shares:\n      - {host: /srv/pigsty, guest: /src, writable: true}\n"
	if _, err := decodeTestFile(input); err == nil {
		t.Fatal("unknown share field accepted")
	}
}
