package private

import (
	"os"
	"testing"

	"github.com/pgsty/barn/internal/spec"
)

func privateResolved() spec.Resolved {
	return spec.Resolved{
		Schema: 1, Name: "full", Image: "u24", Network: "private", SSHUser: "dba",
		Private: &spec.PrivateNetwork{CIDR: "10.10.10.0/24", HostAddress: "10.10.10.1", DHCPEnd: "10.10.10.8"},
		Nodes: []spec.Node{
			{Name: "meta", Control: true, Address: "10.10.10.10", CPUs: 2, Memory: 4 * spec.GiB, RootDisk: 64 * spec.GiB, Aliases: []string{"admin.example"}},
			{Name: "node-1", Address: "10.10.10.11", CPUs: 1, Memory: 2 * spec.GiB, RootDisk: 32 * spec.GiB},
		},
	}
}

func TestBuildPrivateIntentDeterministicAndShort(t *testing.T) {
	t.Parallel()
	uuids := []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"}
	index := 0
	plan, err := Build(privateResolved(), os.Getuid(), nil, func() (string, error) {
		value := uuids[index]
		index++
		return value, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Control != "meta" || len(plan.Nodes) != 2 {
		t.Fatalf("plan = %#v", plan)
	}
	for _, node := range plan.Nodes {
		if len(node.Runtime.QMP) > 103 || node.ManagementMAC == node.PrivateMAC || node.Runtime.Directory == "" {
			t.Fatalf("node intent = %#v", node)
		}
	}
	if plan.Nodes[0].Name != "meta" || plan.Nodes[1].Name != "node-1" {
		t.Fatalf("plan nodes are not canonical: %#v", plan.Nodes)
	}
	second, err := Build(privateResolved(), os.Getuid(), nil, func() (string, error) {
		index--
		return uuids[index], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Nodes[0].ManagementMAC != second.Nodes[0].ManagementMAC || plan.Nodes[0].PrivateMAC != second.Nodes[0].PrivateMAC || plan.Nodes[0].Runtime != second.Nodes[0].Runtime {
		t.Fatal("deterministic private identities changed")
	}
}

func TestBuildPrivateIntentReusesKnownUUIDs(t *testing.T) {
	t.Parallel()
	first, err := Build(privateResolved(), os.Getuid(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	known := make(map[string]string, len(first.Nodes))
	for _, node := range first.Nodes {
		known[node.Name] = node.VMUUID
	}
	second, err := Build(privateResolved(), os.Getuid(), known, func() (string, error) {
		t.Fatal("UUID source called during known-UUID reentry")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, node := range first.Nodes {
		if second.Nodes[index].Name != node.Name || second.Nodes[index].VMUUID != node.VMUUID {
			t.Fatalf("reentered UUIDs changed: %#v %#v", first.Nodes, second.Nodes)
		}
	}
}

func TestBuildPrivateIntentRejectsInvalidControlAndDuplicateAddress(t *testing.T) {
	t.Parallel()
	resolved := privateResolved()
	resolved.Nodes[0].Control = false
	if _, err := Build(resolved, os.Getuid(), nil, nil); err == nil {
		t.Fatal("plan without control was accepted")
	}
	resolved = privateResolved()
	resolved.Nodes[1].Address = resolved.Nodes[0].Address
	if _, err := Build(resolved, os.Getuid(), nil, nil); err == nil {
		t.Fatal("duplicate private address was accepted")
	}
}

func TestMaterializeExistingForwardPorts(t *testing.T) {
	t.Parallel()
	desired := privateResolved()
	desired.Nodes[0].Forwards = []spec.Forward{{Bind: "127.0.0.1", Host: 15432, Guest: 5432, Protocol: "tcp"}}
	persisted := cloneResolved(desired)
	persisted.Nodes[0].Forwards[0] = spec.WithMaterializedHost(persisted.Nodes[0].Forwards[0], 25432)

	got := materializeExistingForwardPorts(desired, persisted)
	if got.Nodes[0].Forwards[0].Host != 25432 {
		t.Fatalf("materialized host port = %d", got.Nodes[0].Forwards[0].Host)
	}
	if desired.Nodes[0].Forwards[0].Host != 15432 {
		t.Fatal("materialization mutated the desired input")
	}
	desired.Nodes[0].Forwards[0].Host = 16432
	if got := materializeExistingForwardPorts(desired, persisted); got.Nodes[0].Forwards[0].Host != 16432 {
		t.Fatal("changed host request reused a persisted materialized port")
	}
	desired.Nodes[0].Forwards[0].Host = 15432
	desired.Nodes[0].Forwards[0].Guest = 6432
	if got := materializeExistingForwardPorts(desired, persisted); got.Nodes[0].Forwards[0].Host != 15432 {
		t.Fatal("unrelated guest forward reused a persisted host port")
	}
}
