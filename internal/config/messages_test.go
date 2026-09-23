package config

import (
	"strings"
	"testing"
)

// Each rejected inventory names the host, the value, and the rule.
func TestInventoryErrorsStateTheRule(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct{ hosts, want string }{
		"memory":       {`10.10.10.10: {vm_mem: 10}`, `line 1: host 10.10.10.10 vm_mem = 10: must be at least 512 MiB (plain integers are MiB`},
		"cpu type":     {`10.10.10.10: {vm_cpu: four}`, `line 1: host 10.10.10.10 vm_cpu = "four": must be an integer`},
		"cpu range":    {`10.10.10.10: {vm_cpu: 0}`, `vm_cpu = 0: must be between 1 and 256`},
		"unknown key":  {`10.10.10.10: {vm_cpus: 4}`, `unknown variable vm_cpus in group n host 10.10.10.10; did you mean vm_cpu?`},
		"node name":    {`10.10.10.10: {nodename: Meta_1}`, `invalid node name "Meta_1": use lowercase letters`},
		"derived name": {`10.10.10.10: {pg_cluster: pg_test, pg_seq: 1}`, `"pg_test-1" derived from pg_cluster and pg_seq is invalid`},
		"duplicate":    {`{10.10.10.10: {nodename: a}, 10.10.10.11: {nodename: a}}`, `host 10.10.10.10 (a) and host 10.10.10.11 (a) have the same node name`},
		"mount":        {`10.10.10.10: {vm_disks: [{path: /etc}]}`, `disk mount "/etc" overlaps the reserved system path /etc`},
		"share":        {`10.10.10.10: {vm_shares: [{host: ./x, guest: /mnt/x}]}`, `share host "./x" must be a clean absolute path`},
		"address":      {`10.10.10.3: {}`, `address must be in 10.10.10.9-10.10.10.254; 10.10.10.1-10.10.10.8 are reserved`},
		"public":       {`8.8.8.10: {}`, `host 8.8.8.10: `},
	} {
		hosts := test.hosts
		if !strings.HasPrefix(hosts, "{") {
			hosts = "{" + hosts + "}"
		}
		_, err := ParseInventory([]byte(`all: {children: {n: {hosts: ` + hosts + `}}}`))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want %q", name, err, test.want)
		}
	}
	for data, want := range map[string]string{"": "inventory is empty", "all:\n  children: [\n": "invalid YAML at line 2: "} {
		if _, err := ParseInventory([]byte(data)); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("ParseInventory(%q) = %v, want prefix %q", data, err, want)
		}
	}
}
