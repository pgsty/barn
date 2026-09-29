package config

import (
	"fmt"
	"strings"

	"github.com/pgsty/barn/internal/failure"
	"github.com/pgsty/barn/internal/network/subnet"
)

// Built-in templates are deliberately generic topologies — nodes, addresses,
// and vm_* knobs only. Pigsty-specific service topology belongs to Pigsty's
// own conf templates, which carry the same vm_* variables.

type templateNode struct {
	LastOctet int
	Name      string
}

var templateProfiles = map[string][]templateNode{
	"meta": {{10, "meta"}},
	"dual": {{10, "meta"}, {11, "node-1"}},
	"trio": {{10, "meta"}, {11, "node-1"}, {12, "node-2"}},
	"full": {{10, "meta"}, {11, "node-1"}, {12, "node-2"}, {13, "node-3"}},
}

// TemplateNames lists the built-in lab templates in size order.
func TemplateNames() []string { return []string{"meta", "dual", "trio", "full"} }

func ValidTemplate(name string) bool {
	_, exists := templateProfiles[name]
	return exists
}

// Template renders a built-in lab as a Pigsty-compatible inventory. The
// optional cidr rebases the /24 while keeping each node's last octet.
func Template(name, cidr string) ([]byte, error) {
	nodes, exists := templateProfiles[name]
	if !exists {
		return nil, failure.New(failure.Usage, fmt.Errorf("unknown lab template %q; available: %s", name, strings.Join(TemplateNames(), ", ")))
	}
	if cidr == "" {
		cidr = subnet.DefaultCIDR
	}
	layout, err := subnet.Parse(cidr)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(layout.CIDR(), ".0/24")
	var output strings.Builder
	fmt.Fprintf(&output, "---\n")
	fmt.Fprintf(&output, "# Barn lab %q: %d fixed-IP node(s) on %s.\n", name, len(nodes), layout.CIDR())
	fmt.Fprintf(&output, "# Start: barn up; connect: barn ssh.\n")
	fmt.Fprintf(&output, "# Optional per-host settings: vm_cpu: 2, vm_mem: 4096, vm_disk: 64, vm_image: %s.\n", defaultImage)
	fmt.Fprintf(&output, "# Each node also gets a %d GiB /data disk; set vm_disks to change it, or vm_disks: [] for none.\n", defaultDataGiB)
	fmt.Fprintf(&output, "# Other Pigsty/Ansible variables can stay in this inventory.\n")
	fmt.Fprintf(&output, "all:\n")
	fmt.Fprintf(&output, "  vars:\n")
	fmt.Fprintf(&output, "    admin_ip: %s.%d\n", prefix, nodes[0].LastOctet)
	fmt.Fprintf(&output, "  children:\n")
	fmt.Fprintf(&output, "    nodes:\n")
	fmt.Fprintf(&output, "      hosts:\n")
	for _, node := range nodes {
		fmt.Fprintf(&output, "        %s.%d: { nodename: %s }\n", prefix, node.LastOctet, node.Name)
	}
	data := []byte(output.String())
	if _, err := ParseInventory(data); err != nil {
		return nil, fmt.Errorf("internal template %q does not round-trip: %w", name, err)
	}
	return data, nil
}
