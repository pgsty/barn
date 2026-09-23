package naming

import (
	"strings"
	"testing"
)

func TestValidNodeNameContract(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"a", "meta", "node-1", strings.Repeat("a", 63)} {
		if !ValidNodeName(name) {
			t.Errorf("valid node name %q was rejected", name)
		}
	}
	for _, name := range []string{"", "Meta", "node_1", "-node", "node-", "node.example", "../node", strings.Repeat("a", 64)} {
		if ValidNodeName(name) {
			t.Errorf("invalid node name %q was accepted", name)
		}
	}
}

func TestClosestSuggestsWithinTwoEdits(t *testing.T) {
	t.Parallel()
	candidates := []string{"vm_cpu", "vm_mem", "vm_disk", "vm_disks"}
	for name, want := range map[string]string{"vm_cpus": "vm_cpu", "vm_dsk": "vm_disk", "vm_cpu": "", "vm_network": ""} {
		if got := Closest(name, candidates); got != want {
			t.Errorf("Closest(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestClosestSuggestsOnlyNearNodeNames(t *testing.T) {
	t.Parallel()
	nodes := []string{"pg-meta-1", "pg-test-1", "pg-test-2"}
	for name, want := range map[string]string{"pg-mta-1": "pg-meta-1", "hostname": "", "pg-meta-1": ""} {
		if got := Closest(name, nodes); got != want {
			t.Errorf("Closest(%q) = %q, want %q", name, got, want)
		}
	}
}
