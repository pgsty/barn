package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/spec"
)

func writeTestFile(t *testing.T, directory, name, content string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestSkippedExternalHostsNeedNoVMAddress(t *testing.T) {
	data := `all:
  children:
    external:
      vars: {vm_skip: true}
      hosts:
        build.example.com: {}
        localhost: {}
        '2001:db8::1': {}
    lab:
      hosts:
        10.10.10.10: {nodename: meta}
`
	file, err := ParseInventory([]byte(data))
	if err != nil || len(file.Nodes) != 1 || file.Nodes[0].Name != "meta" {
		t.Fatalf("mixed inventory = %#v, %v", file.Nodes, err)
	}
	for _, input := range []string{
		strings.Replace(data, "vm_skip: true", "vm_skip: false", 1),
		strings.Replace(data, "build.example.com: {}", "build.example.com: {vm_skip: false}", 1),
	} {
		if _, err := ParseInventory([]byte(input)); err == nil || !strings.Contains(err.Error(), "IPv4") {
			t.Fatalf("managed hostname must still require a VM address: %v", err)
		}
	}
}

const fullInventory = `---
all:
  vars:
    version: v3.6.0
    admin_ip: 10.10.10.10
    region: default
    vm_image: u24
    node_tune: tiny
  children:
    infra:
      hosts:
        10.10.10.10:
          infra_seq: 1
          nodename: meta
          vm_cpu: 4
          vm_mem: 8192
          vm_alias: [i.pigsty, a.pigsty]
    etcd:
      hosts:
        10.10.10.10: { etcd_seq: 1 }
      vars: { etcd_cluster: etcd }
    pg-meta:
      hosts:
        10.10.10.10: { pg_seq: 1, pg_role: primary }
      vars:
        pg_cluster: pg-meta
        pg_conf: crit.yml
    pg-test:
      hosts:
        10.10.10.11: { pg_seq: 1, pg_role: primary }
        10.10.10.12: { pg_seq: 2, pg_role: replica, vm_mem: "4GiB" }
        10.10.10.13: { pg_seq: 3 }
      vars:
        pg_cluster: pg-test
        vm_disks: [{ path: /data, size: 64 }]
`

func mustParseInventory(t *testing.T, text string) File {
	t.Helper()
	file, err := ParseInventory([]byte(text))
	if err != nil {
		t.Fatalf("parse inventory: %v", err)
	}
	return file
}

func TestParseInventoryFullLab(t *testing.T) {
	file := mustParseInventory(t, fullInventory)
	if file.Network.Mode != "private" || file.Network.CIDR != "10.10.10.0/24" {
		t.Fatalf("derived network is wrong: %+v", file.Network)
	}
	if file.SSH.User != "dba" {
		t.Fatalf("default ssh user, got %q", file.SSH.User)
	}
	if len(file.Nodes) != 4 {
		t.Fatalf("expected 4 nodes, got %d", len(file.Nodes))
	}

	meta := file.Nodes[0]
	if meta.Name != "meta" || !meta.Control || meta.Address != "10.10.10.10" {
		t.Fatalf("meta identity/control: %+v", meta)
	}
	if meta.CPUs != 4 || int64(meta.Memory) != 8192<<20 || int64(meta.RootDisk) != 64*spec.GiB {
		t.Fatalf("meta resources: %+v", meta)
	}
	if len(meta.HostAliases) != 2 || meta.HostAliases[0] != "i.pigsty" {
		t.Fatalf("meta aliases: %v", meta.HostAliases)
	}
	if len(meta.Disks) != 1 || meta.Disks[0].Mount != "/data" || int64(meta.Disks[0].Size) != 128*spec.GiB || meta.Disks[0].Filesystem != "auto" {
		t.Fatalf("meta default data disk: %+v", meta.Disks)
	}

	nodeOne := file.Nodes[1]
	if nodeOne.Name != "pg-test-1" || nodeOne.Control {
		t.Fatalf("pg-derived name: %+v", nodeOne)
	}
	if nodeOne.CPUs != 1 || int64(nodeOne.Memory) != 2048<<20 {
		t.Fatalf("node-1 defaults: %+v", nodeOne)
	}
	if len(nodeOne.Disks) != 1 || int64(nodeOne.Disks[0].Size) != 64*spec.GiB || nodeOne.Disks[0].Filesystem != "auto" {
		t.Fatalf("group-level vm_disks: %+v", nodeOne.Disks)
	}

	if int64(file.Nodes[2].Memory) != 4*spec.GiB {
		t.Fatalf("string-size vm_mem: %+v", file.Nodes[2])
	}
	if file.Nodes[3].Name != "pg-test-3" {
		t.Fatalf("pg-derived fallback for bare host: %+v", file.Nodes[3])
	}
}

func TestParseInventoryBareHostGetsFullDefaults(t *testing.T) {
	file := mustParseInventory(t, `
all:
  children:
    nodes:
      hosts:
        10.10.10.13: {}
`)
	node := file.Nodes[0]
	if node.Name != "node-13" || node.CPUs != 1 || int64(node.Memory) != 2048<<20 || int64(node.RootDisk) != 64*spec.GiB {
		t.Fatalf("bare host defaults: %+v", node)
	}
	if len(node.Disks) != 1 || node.Disks[0].Mount != "/data" || node.Disks[0].Filesystem != "auto" {
		t.Fatalf("bare host default disk: %+v", node.Disks)
	}
	if node.Image != "u24" || !node.Control {
		t.Fatalf("bare host image/control: %+v", node)
	}
}

func TestParseInventoryScaleMultipliesCPUAndMemoryOnly(t *testing.T) {
	file := mustParseInventory(t, `
all:
  vars:
    vm_scale: 2
    vm_mem: 3GiB
    vm_disk: 80
  children:
    nodes:
      hosts:
        10.10.10.10: {vm_cpu: 3, vm_disks: [{path: /data, size: 16}]}
        10.10.10.11: {}
`)
	if file.Nodes[0].CPUs != 6 || int64(file.Nodes[0].Memory) != 6*spec.GiB || int64(file.Nodes[0].RootDisk) != 80*spec.GiB {
		t.Fatalf("scaled explicit resources = %+v", file.Nodes[0])
	}
	if len(file.Nodes[0].Disks) != 1 || int64(file.Nodes[0].Disks[0].Size) != 16*spec.GiB {
		t.Fatalf("vm_scale changed data disk = %+v", file.Nodes[0].Disks)
	}
	if file.Nodes[1].CPUs != 2 || int64(file.Nodes[1].Memory) != 6*spec.GiB || int64(file.Nodes[1].RootDisk) != 80*spec.GiB {
		t.Fatalf("scaled defaults = %+v", file.Nodes[1])
	}
}

func TestParseInventoryScaleUsesNormalInheritanceAndBounds(t *testing.T) {
	file := mustParseInventory(t, `
all:
  vars: {vm_scale: 2}
  children:
    parent:
      vars: {vm_scale: 3}
      children:
        child:
          hosts: {10.10.10.10: {vm_scale: 4}}
`)
	if file.Nodes[0].CPUs != 4 || int64(file.Nodes[0].Memory) != 8*spec.GiB {
		t.Fatalf("host vm_scale did not win: %+v", file.Nodes[0])
	}
	for name, text := range map[string]string{
		"negative":  `all: {vars: {vm_scale: -1}, hosts: {10.10.10.10: {}}}`,
		"fraction":  `all: {vars: {vm_scale: 1.5}, hosts: {10.10.10.10: {}}}`,
		"boolean":   `all: {vars: {vm_scale: true}, hosts: {10.10.10.10: {}}}`,
		"null":      `all: {vars: {vm_scale: null}, hosts: {10.10.10.10: {}}}`,
		"overflow":  `all: {vars: {vm_scale: 2, vm_mem: 8796093022207}, hosts: {10.10.10.10: {}}}`,
		"zero":      `all: {vars: {vm_scale: 0}, hosts: {10.10.10.10: {}}}`,
		"string":    `all: {vars: {vm_scale: "2"}, hosts: {10.10.10.10: {}}}`,
		"cpu limit": `all: {vars: {vm_scale: 2, vm_cpu: 200}, hosts: {10.10.10.10: {}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseInventory([]byte(text)); err == nil || !strings.Contains(err.Error(), "vm_scale") {
				t.Fatalf("invalid scale accepted: %v", err)
			}
		})
	}
}

func TestParseInventoryVMHostsPublishesServicesWithoutVMDrift(t *testing.T) {
	base := `all:
  vars:
    vm_hosts:
      - {address: 10.10.10.30, names: [pg-dns-service, pg-meta.pigsty]}
      - {address: 10.10.10.10, names: [repo.pigsty, META]}
      - {address: 10.10.10.10, names: [repo.pigsty]}
  hosts:
    10.10.10.10: {nodename: meta}
`
	file := mustParseInventory(t, base)
	entries, err := file.HostEntries()
	if err != nil || len(entries) != 2 || entries[0].Address != "10.10.10.10" || strings.Join(entries[0].Names, ",") != "meta,repo.pigsty" {
		t.Fatalf("normalized entries = %#v, %v", entries, err)
	}
	first, err := file.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	file = mustParseInventory(t, strings.ReplaceAll(base, "repo.pigsty", "new.pigsty"))
	second, err := file.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("DNS edit changed VM spec: %#v -> %#v", first, second)
	}
}

func TestParseInventoryVMHostsIsGlobalAndRejectsAmbiguity(t *testing.T) {
	for name, text := range map[string]string{
		"group scope":        `all: {children: {nodes: {vars: {vm_hosts: []}, hosts: {10.10.10.10: {}}}}}`,
		"later host":         `all: {hosts: {10.10.10.10: {}, 10.10.10.11: {vm_hosts: []}}}`,
		"skipped host":       `all: {hosts: {10.10.10.10: {}, external: {vm_skip: true, vm_hosts: []}}}`,
		"null":               `all: {vars: {vm_hosts: null}, hosts: {10.10.10.10: {}}}`,
		"reserved host":      `all: {vars: {vm_hosts: [{address: 10.10.10.1, names: [service]}]}, hosts: {10.10.10.10: {}}}`,
		"reserved dhcp":      `all: {vars: {vm_hosts: [{address: 10.10.10.2, names: [service]}]}, hosts: {10.10.10.10: {}}}`,
		"different subnet":   `all: {vars: {vm_hosts: [{address: 10.10.20.30, names: [service]}]}, hosts: {10.10.10.10: {}}}`,
		"node name conflict": `all: {vars: {vm_hosts: [{address: 10.10.10.30, names: [NODE-10]}]}, hosts: {10.10.10.10: {}}}`,
		"missing names":      `all: {vars: {vm_hosts: [{address: 10.10.10.30}]}, hosts: {10.10.10.10: {}}}`,
		"unknown key":        `all: {vars: {vm_hosts: [{address: 10.10.10.30, names: [service], alias: extra}]}, hosts: {10.10.10.10: {}}}`,
		"bad label":          `all: {vars: {vm_hosts: [{address: 10.10.10.30, names: [bad.-name]}]}, hosts: {10.10.10.10: {}}}`,
		"non-string name":    `all: {vars: {vm_hosts: [{address: 10.10.10.30, names: [42]}]}, hosts: {10.10.10.10: {}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseInventory([]byte(text)); err == nil || !strings.Contains(err.Error(), "vm_hosts") {
				t.Fatalf("invalid vm_hosts accepted: %v", err)
			}
		})
	}
}

func TestParseInventoryDiskFilesystemSelection(t *testing.T) {
	file := mustParseInventory(t, `
all:
  children:
    nodes:
      hosts:
        10.10.10.10: { vm_disks: [{ path: /auto, size: 8, fs: auto }] }
        10.10.10.11: { vm_disks: [{ path: /xfs, size: 8, fs: xfs }] }
        10.10.10.12: { vm_disks: [{ path: /ext4, size: 8, fs: ext4 }] }
`)
	want := []string{"auto", "xfs", "ext4"}
	for index, node := range file.Nodes {
		if len(node.Disks) != 1 || node.Disks[0].Filesystem != want[index] {
			t.Fatalf("node %d filesystem = %+v, want %s", index, node.Disks, want[index])
		}
	}
}

func TestParseInventoryNormalizesFriendlyDiskPathsAndKeepsStableNames(t *testing.T) {
	file := mustParseInventory(t, `
all:
  children:
    nodes:
      hosts:
        10.10.10.10:
          vm_disks:
            - {path: "/data//build/../源码 files/$build", size: "8G"}
`)
	if len(file.Nodes) != 1 || len(file.Nodes[0].Disks) != 1 {
		t.Fatalf("friendly disk was not parsed: %#v", file.Nodes)
	}
	disk := file.Nodes[0].Disks[0]
	if disk.Mount != "/data/源码 files/$build" || int64(disk.Size) != 8*spec.GiB || !strings.HasPrefix(disk.Name, "disk-") {
		t.Fatalf("friendly disk = %#v", disk)
	}
	resolved, err := file.Resolve()
	if err != nil || resolved.Nodes[0].Disks[0].Mount != disk.Mount || resolved.Nodes[0].Disks[0].Name != disk.Name {
		t.Fatalf("resolved friendly disk = %#v, %v", resolved.Nodes[0].Disks, err)
	}
	for path, want := range map[string]string{"/data": "data", "/data/pg": "data-pg"} {
		if got, err := diskNameForPath(path); err != nil || got != want {
			t.Fatalf("disk name for %q = %q, %v; want %q", path, got, err, want)
		}
	}
}

func TestParseInventoryAllowsMoreThanTwentyStaticNodes(t *testing.T) {
	var text strings.Builder
	text.WriteString("all:\n  children:\n    nodes:\n      hosts:\n")
	for address := 10; address < 42; address++ {
		fmt.Fprintf(&text, "        10.10.10.%d: {}\n", address)
	}
	file := mustParseInventory(t, text.String())
	if len(file.Nodes) != 32 {
		t.Fatalf("node count = %d, want 32", len(file.Nodes))
	}
	if _, err := file.Resolve(); err != nil {
		t.Fatalf("32-node inventory did not resolve: %v", err)
	}
}

func TestParseInventoryVersionPrefixPreservesNaturalScalars(t *testing.T) {
	file := mustParseInventory(t, `
all:
  vars:
    vm_image: el9
    vm_version: 9.7
  children:
    nodes:
      hosts:
        10.10.10.10: {}
        10.10.10.11: { vm_version: 9 }
        10.10.10.12: { vm_version: 9.10 }
`)
	if file.Nodes[0].Image != "el9@9.7" || file.Nodes[1].Image != "el9@9" || file.Nodes[2].Image != "el9@9.10" {
		t.Fatalf("version-prefix image references = %#v", file.Nodes)
	}
}

func TestParseInventoryRejectsInvalidOrConflictingVersionSelectors(t *testing.T) {
	for name, text := range map[string]string{
		"non-numeric": `
all:
  vars: { vm_image: el9, vm_version: 9.x }
  children: { nodes: { hosts: { 10.10.10.10: {} } } }
`,
		"wrong type": `
all:
  vars: { vm_image: el9, vm_version: true }
  children: { nodes: { hosts: { 10.10.10.10: {} } } }
`,
		"combined selector": `
all:
  vars: { vm_image: "el9:stable", vm_version: 9 }
  children: { nodes: { hosts: { 10.10.10.10: {} } } }
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseInventory([]byte(text)); err == nil || !strings.Contains(err.Error(), "vm_version") {
				t.Fatalf("invalid vm_version error = %v", err)
			}
		})
	}
}

func TestParseInventoryVersionConflictPreservesTrailingZero(t *testing.T) {
	_, err := ParseInventory([]byte(`
all:
  children:
    one:
      hosts: { 10.10.10.10: {} }
      vars: { vm_image: el9, vm_version: 9.10 }
    two:
      hosts: { 10.10.10.10: {} }
      vars: { vm_image: el9, vm_version: 9.1 }
`))
	if err == nil || !strings.Contains(err.Error(), "conflicting values") || !strings.Contains(err.Error(), "vm_version") {
		t.Fatalf("version spelling conflict error = %v", err)
	}
}

func TestParseInventoryDeploymentArchitecture(t *testing.T) {
	file := mustParseInventory(t, `
all:
  vars: { vm_arch: amd64 }
  children:
    nodes:
      hosts:
        10.10.10.10: {}
        10.10.10.11: {}
`)
	resolved, err := file.Resolve()
	if err != nil || file.Arch != "amd64" || resolved.Arch != "amd64" {
		t.Fatalf("resolved architecture = file:%q resolved:%q err:%v", file.Arch, resolved.Arch, err)
	}
	for name, text := range map[string]string{
		"empty": `
all:
  vars: { vm_arch: "" }
  children:
    nodes:
      hosts:
        10.10.10.10: {}
`,
		"unsupported": `
all:
  vars: { vm_arch: s390x }
  children:
    nodes:
      hosts:
        10.10.10.10: {}
`,
		"partial": `
all:
  children:
    nodes:
      hosts:
        10.10.10.10: { vm_arch: amd64 }
        10.10.10.11: {}
`,
		"mixed": `
all:
  children:
    nodes:
      hosts:
        10.10.10.10: { vm_arch: amd64 }
        10.10.10.11: { vm_arch: arm64 }
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseInventory([]byte(text)); err == nil || !strings.Contains(err.Error(), "vm_arch") {
				t.Fatalf("invalid vm_arch inventory error = %v", err)
			}
		})
	}
}

func TestParseInventoryArchitectureAliasesAreEquivalentAcrossHosts(t *testing.T) {
	for _, values := range []string{"amd64, x86_64, x64", "arm64, aarch64, a64"} {
		aliases := strings.Split(values, ", ")
		file := mustParseInventory(t, fmt.Sprintf(`all:
  hosts:
    10.10.10.10: { vm_arch: %s }
    10.10.10.11: { vm_arch: %s }
    10.10.10.12: { vm_arch: %s }
`, aliases[0], aliases[1], aliases[2]))
		resolved, err := file.Resolve()
		if err != nil || resolved.Arch != aliases[0] {
			t.Fatalf("architecture aliases %s: %q, %v", values, resolved.Arch, err)
		}
	}
}

func TestParseInventoryExplicitEmptyDisksOverrides(t *testing.T) {
	file := mustParseInventory(t, `
all:
  children:
    nodes:
      hosts:
        10.10.10.10: { vm_disks: [] }
`)
	if len(file.Nodes[0].Disks) != 0 {
		t.Fatalf("explicit empty vm_disks must remove the default disk: %+v", file.Nodes[0].Disks)
	}
}

func TestParseInventorySiblingGroupConflictFails(t *testing.T) {
	_, err := ParseInventory([]byte(`
all:
  children:
    one:
      hosts: { 10.10.10.10: {} }
      vars: { vm_cpu: 2 }
    two:
      hosts: { 10.10.10.10: {} }
      vars: { vm_cpu: 4 }
`))
	if err == nil || !strings.Contains(err.Error(), "conflicting values") {
		t.Fatalf("expected sibling conflict error, got %v", err)
	}
}

func TestParseInventoryDeeperGroupWins(t *testing.T) {
	file := mustParseInventory(t, `
all:
  vars: { vm_cpu: 1 }
  children:
    parent:
      vars: { vm_cpu: 2 }
      children:
        child:
          hosts: { 10.10.10.10: {} }
          vars: { vm_cpu: 8 }
`)
	if file.Nodes[0].CPUs != 8 {
		t.Fatalf("deeper group must win: %+v", file.Nodes[0])
	}
}

func TestParseInventorySkipAndForeignHosts(t *testing.T) {
	file := mustParseInventory(t, `
all:
  children:
    nodes:
      hosts:
        10.10.10.10: {}
        10.10.10.20: { vm_skip: true }
`)
	if len(file.Nodes) != 1 || file.Nodes[0].Address != "10.10.10.10" {
		t.Fatalf("vm_skip host must be excluded: %+v", file.Nodes)
	}
}

func TestParseInventoryUnknownVMKeyFails(t *testing.T) {
	_, err := ParseInventory([]byte(`
all:
  children:
    nodes:
      hosts:
        10.10.10.10: { vm_cpus: 4 }
`))
	if err == nil || !strings.Contains(err.Error(), "unknown variable vm_cpus") || !strings.Contains(err.Error(), "did you mean vm_cpu?") {
		t.Fatalf("expected strict vm_* namespace error, got %v", err)
	}
}

func TestParseInventoryRejectsDuplicateMappingKeys(t *testing.T) {
	for name, text := range map[string]string{
		"children": `
all:
  children: { one: { hosts: { 10.10.10.10: {} } } }
  children: { two: { hosts: { 10.10.10.11: {} } } }
`,
		"host": `
all:
  children:
    nodes:
      hosts:
        10.10.10.10: { nodename: meta }
        10.10.10.10: { nodename: other }
`,
		"vm_cpu": `
all:
  children:
    nodes:
      hosts:
        10.10.10.10:
          vm_cpu: 2
          vm_cpu: 8
`,
		"nodename": `
all:
  children:
    nodes:
      hosts:
        10.10.10.10:
          nodename: meta
          nodename: other
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseInventory([]byte(text))
			if err == nil || !strings.Contains(err.Error(), "duplicate key") || !strings.Contains(err.Error(), "line") {
				t.Fatalf("duplicate mapping error = %v", err)
			}
		})
	}
}

func TestParseInventoryExpandsYAMLMergeKeysWithAnsiblePrecedence(t *testing.T) {
	file := mustParseInventory(t, `
defaults: &defaults
  vm_cpu: 8
  vm_mem: 8192
  opaque_pigsty_value: kept
all:
  children:
    nodes:
      hosts:
        10.10.10.10:
          <<: *defaults
          nodename: meta
`)
	if file.Nodes[0].CPUs != 8 || int64(file.Nodes[0].Memory) != 8192<<20 {
		t.Fatalf("merged resources = %+v", file.Nodes[0])
	}

	file = mustParseInventory(t, `
first: &first { vm_cpu: 4 }
second: &second { vm_cpu: 8, vm_mem: 6144 }
all:
  children:
    nodes:
      hosts:
        10.10.10.10:
          <<: [*first, *second]
          vm_mem: 2048
`)
	if file.Nodes[0].CPUs != 4 || int64(file.Nodes[0].Memory) != 2048<<20 {
		t.Fatalf("merge sequence/explicit precedence = %+v", file.Nodes[0])
	}
}

func TestParseInventoryRejectsMergeAndGroupAliasCycles(t *testing.T) {
	for name, text := range map[string]string{
		"merge-self": `
all:
  children:
    nodes:
      hosts:
        10.10.10.10: &host
          <<: *host
`,
		"group-self": `
all: &all
  children:
    loop: *all
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseInventory([]byte(text))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cycle") {
				t.Fatalf("alias cycle error = %v", err)
			}
		})
	}
}

func TestParseInventoryTemplateValueFails(t *testing.T) {
	_, err := ParseInventory([]byte(`
all:
  children:
    nodes:
      hosts:
        10.10.10.10: { vm_image: "{{ default_image }}" }
`))
	if err == nil || !strings.Contains(err.Error(), "template expression") {
		t.Fatalf("expected literal-value error, got %v", err)
	}
}

func TestParseInventorySplitSubnetFails(t *testing.T) {
	_, err := ParseInventory([]byte(`
all:
  children:
    nodes:
      hosts:
        10.10.10.10: {}
        10.10.20.10: {}
`))
	if err == nil || !strings.Contains(err.Error(), "share one /24") {
		t.Fatalf("expected split-subnet error, got %v", err)
	}
}

func TestParseInventoryAdminUserContract(t *testing.T) {
	if _, err := ParseInventory([]byte(`
all:
  vars: { node_admin_username: root }
  children:
    nodes:
      hosts: { 10.10.10.10: {} }
`)); err != nil {
		t.Fatalf("custom admin user must parse: %v", err)
	}
	_, err := ParseInventory([]byte(`
all:
  vars: { node_admin_uid: 1000 }
  children:
    nodes:
      hosts: { 10.10.10.10: {} }
`))
	if err == nil || !strings.Contains(err.Error(), "fixed UID") {
		t.Fatalf("expected fixed-UID error, got %v", err)
	}
}

func TestParseInventoryPigstyVarsAreOpaque(t *testing.T) {
	file := mustParseInventory(t, `
all:
  vars:
    pg_version: 18
    repo_modules: infra,node,pgsql
    node_packages: [openssh-server]
  children:
    pg-meta:
      hosts: { 10.10.10.10: { pg_seq: 1, pg_role: primary } }
      vars:
        pg_cluster: pg-meta
        pg_users: [{ name: dbuser_meta, password: DBUser.Meta }]
`)
	if file.Nodes[0].Name != "pg-meta-1" {
		t.Fatalf("expected opaque handling plus pg naming, got %+v", file.Nodes[0])
	}
}

func TestDetectFormat(t *testing.T) {
	if format, err := DetectFormat([]byte(fullInventory)); err != nil || format != "inventory" {
		t.Fatalf("inventory detection: %q %v", format, err)
	}
	// Only a Pigsty-compatible inventory is a configuration. Any other document
	// shape, including the retired version:/nodes: sketch, is simply not one.
	for _, unrecognized := range []string{
		"version: 1\nname: full\nnetwork: {mode: private}\nnodes: [{name: meta}]\n",
		"just: text\n",
	} {
		if format, err := DetectFormat([]byte(unrecognized)); err == nil {
			t.Fatalf("DetectFormat(%q) = %q, want an unrecognized-format error", unrecognized, format)
		}
	}
}

func TestDiscoverRejectsANonInventoryDocument(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "barn.yaml", "version: 1\nname: x\nnetwork: {mode: user}\nnodes: [{name: meta}]\n")
	_, _, err := Discover(directory, "")
	if err == nil || !strings.Contains(err.Error(), "unrecognized configuration") {
		t.Fatalf("Discover error = %v, want an unrecognized-configuration failure", err)
	}
}

func TestDiscoverPigstyYML(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "pigsty.yml", fullInventory)
	file, path, err := Discover(directory, "")
	if err != nil || !strings.HasSuffix(path, "pigsty.yml") || len(file.Nodes) != 4 {
		t.Fatalf("pigsty.yml discovery: path=%q nodes=%d err=%v", path, len(file.Nodes), err)
	}
}

func TestDiscoverPrefersBarnYMLOverPigstyYML(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "pigsty.yml", fullInventory)
	writeTestFile(t, directory, "barn.yml", "all:\n  vars: {admin_ip: 10.10.10.10}\n  children:\n    nodes:\n      hosts:\n        10.10.10.10: {nodename: meta}\n")
	file, path, err := Discover(directory, "")
	if err != nil || !strings.HasSuffix(path, "barn.yml") || len(file.Nodes) != 1 {
		t.Fatalf("barn.yml priority: path=%q nodes=%d err=%v", path, len(file.Nodes), err)
	}
}

func TestDiscoverMissingConfig(t *testing.T) {
	_, _, err := Discover(t.TempDir(), "")
	if !errors.Is(err, ErrNoConfig) {
		t.Fatalf("expected ErrNoConfig, got %v", err)
	}
}

func TestScaleIsAppliedOnceAndMatchesExplicitResources(t *testing.T) {
	for _, values := range []struct{ vars, explicit string }{
		{"vm_scale: 3", "vm_cpu: 3, vm_mem: 6144"},
		{"vm_scale: 2, vm_cpu: 3", "vm_cpu: 6, vm_mem: 4096"},
		{"vm_scale: 2, vm_mem: 1024", "vm_cpu: 2, vm_mem: 2048"},
	} {
		file := mustParseInventory(t, "all: {vars: {"+values.vars+"}, hosts: {10.10.10.10: {}}}")
		explicit := mustParseInventory(t, "all: {vars: {"+values.explicit+"}, hosts: {10.10.10.10: {}}}")
		first, err := file.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		second, err := file.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		want, err := explicit.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(first, want) {
			t.Fatalf("scale is not equivalent to explicit resources: %#v, %#v, %#v", first, second, want)
		}
	}
}

func TestScaleSiblingConflictCanBeOverriddenAtHost(t *testing.T) {
	input := `all:
  children:
    left:
      vars: {vm_scale: 2}
      hosts: {10.10.10.10: {}}
    right:
      vars: {vm_scale: 3}
      hosts: {10.10.10.10: {%s}}
`
	if _, err := ParseInventory([]byte(fmt.Sprintf(input, ""))); err == nil {
		t.Fatal("conflicting sibling scales accepted")
	}
	file := mustParseInventory(t, fmt.Sprintf(input, "vm_scale: 1"))
	if file.Nodes[0].CPUs != 1 || int64(file.Nodes[0].Memory) != 2*spec.GiB {
		t.Fatalf("host scale did not replace sibling scales: %#v", file.Nodes[0])
	}
}
