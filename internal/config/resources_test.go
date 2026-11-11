package config

import (
	"strings"
	"testing"

	"github.com/pgsty/barn/internal/spec"
)

func TestResourceOverridesResolveBeforeMultiplicationAndPreservePeers(t *testing.T) {
	inventory := []byte("all: {vars: {vm_cpu: 2, vm_mem: 4GiB, vm_scale: 2}, hosts: {10.10.10.10: {nodename: meta}, 10.10.10.11: {nodename: peer, vm_scale: 1}}}")
	cpu, scale := int64(4), int64(3)
	file, err := ParseInventory(inventory, InventoryOptions{Resources: ResourceOverrides{CPU: &cpu, Scale: &scale}, Nodes: []string{"meta"}})
	if err != nil {
		t.Fatal(err)
	}
	if file.Nodes[0].CPUs != 12 || int64(file.Nodes[0].Memory) != 12*spec.GiB || file.Nodes[1].CPUs != 2 || int64(file.Nodes[1].Memory) != 4*spec.GiB || int64(file.Nodes[0].RootDisk) != 64*spec.GiB {
		t.Fatalf("incorrect resolution: %+v", file.Nodes)
	}
	// The override can reduce an otherwise excessive effective CPU count,
	// while invalid inventory types and unknown fields remain errors.
	scale = 1
	input := []byte("all: {vars: {vm_cpu: 256, vm_scale: 2}, hosts: {10.10.10.10: {}}}")
	if _, err := ParseInventory(input); err == nil {
		t.Fatal("excessive CPUs accepted")
	}
	if _, err := ParseInventory(input, InventoryOptions{Resources: ResourceOverrides{Scale: &scale}}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"vm_cpu: wrong", "vm_cup: 2", "vm_cpu: null"} {
		input := []byte("all: {vars: {" + bad + "}, hosts: {10.10.10.10: {}}}")
		if _, err := ParseInventory(input, InventoryOptions{Resources: ResourceOverrides{CPU: &cpu}}); err == nil {
			t.Fatalf("override hid %s", bad)
		}
	}
}

func TestResourceOverrideBounds(t *testing.T) {
	cpu, scale, memory := int64(256), int64(2), int64(1<<62)
	base := ResourceResolution{Node: "meta", CPU: 1, Memory: 2 * spec.GiB, Scale: 1, Disk: 64 * spec.GiB}
	for _, test := range []struct {
		overrides ResourceOverrides
		want      string
	}{
		{ResourceOverrides{CPU: &cpu, Scale: &scale}, "effective vm_cpu"},
		{ResourceOverrides{Memory: &memory, Scale: &scale}, "overflows"},
	} {
		if _, err := ResolveResources(base, test.overrides); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("bounds: %v", err)
		}
	}
}
