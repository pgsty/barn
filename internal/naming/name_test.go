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

func TestClosestSuggestsOnlyNearNames(t *testing.T) {
	t.Parallel()
	nodes := []string{"pg-meta-1", "pg-test-1", "pg-test-2"}
	for name, want := range map[string]string{"pg-mta-1": "pg-meta-1", "pg-tset-1": "pg-test-1", "hostname": "", "pg-meta-1": "", "vm_cpus": ""} {
		if got := Closest(name, nodes); got != want {
			t.Errorf("Closest(%q) = %q, want %q", name, got, want)
		}
	}
}
