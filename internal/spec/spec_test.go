package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQuickDefaults(t *testing.T) {
	t.Parallel()
	got := Quick(true, true)
	if got.SSHUser != "dba" || got.Image != "u24" || got.Network != "private" {
		t.Fatalf("quick identity defaults = %#v", got)
	}
	if len(got.Nodes) != 1 {
		t.Fatalf("quick nodes = %d, want 1", len(got.Nodes))
	}
	node := got.Nodes[0]
	if node.Name != "meta" || node.CPUs != 2 || node.Memory != 4*GiB || node.RootDisk != 64*GiB {
		t.Fatalf("quick node defaults = %#v", node)
	}
	if len(node.Disks) != 1 || node.Disks[0].Size != 64*GiB || node.Disks[0].Mount != "/data" {
		t.Fatalf("quick data disk = %#v", node.Disks)
	}
	if len(node.Forwards) != 4 {
		t.Fatalf("quick forwards = %d, want 4", len(node.Forwards))
	}
}

func TestQuickOptionalDefaults(t *testing.T) {
	t.Parallel()
	got := Quick(false, false).Nodes[0]
	if len(got.Disks) != 0 || len(got.Forwards) != 0 {
		t.Fatalf("optional defaults were not disabled: %#v", got)
	}
}

func TestHashDeterministic(t *testing.T) {
	t.Parallel()
	first, err := Hash(Quick(true, true))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Hash(Quick(true, true))
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 64 {
		t.Fatalf("hashes %q and %q", first, second)
	}
}

func TestEmptySharesPreserveCanonicalHash(t *testing.T) {
	t.Parallel()
	resolved := Quick(true, true)
	canonical, err := CanonicalJSON(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), `"shares"`) {
		t.Fatalf("empty shares changed canonical JSON: %s", canonical)
	}
	hash, err := Hash(resolved)
	if err != nil {
		t.Fatal(err)
	}
	resolved.Nodes[0].Shares = []Share{}
	emptyHash, err := Hash(resolved)
	if err != nil || emptyHash != hash {
		t.Fatalf("non-nil empty shares changed hash: got %s want %s err=%v", emptyHash, hash, err)
	}
}

func TestShareTagIsStableBoundedAndCoversIntent(t *testing.T) {
	t.Parallel()
	share := Share{Host: "/Users/me/pgsty/pigsty", Guest: "/src"}
	if got, want := ShareTag(share), "barn-1c73730fac508a9da296"; got != want {
		t.Fatalf("share tag = %q, want %q", got, want)
	}
	if tag := ShareTag(share); len(tag) > 31 || !strings.HasPrefix(tag, "barn-") {
		t.Fatalf("share tag is not a bounded Barn identifier: %q", tag)
	}
	variants := []Share{
		{Host: share.Host + "-other", Guest: share.Guest},
		{Host: share.Host, Guest: share.Guest + "-other"},
		{Host: share.Host, Guest: share.Guest, Readonly: true},
	}
	for _, variant := range variants {
		if ShareTag(variant) == ShareTag(share) {
			t.Errorf("share intent collision: base=%#v variant=%#v", share, variant)
		}
	}
}

func TestMaterializedForwardPreservesOptionalRequestEvidence(t *testing.T) {
	t.Parallel()
	forward := Forward{Bind: "127.0.0.1", Host: 15432, Guest: 5432, Protocol: "tcp"}
	unchanged := WithMaterializedHost(forward, 15432)
	data, err := json.Marshal(unchanged)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.RequestedHost != 0 || strings.Contains(string(data), "requested_host") {
		t.Fatalf("unchanged allocation retained redundant request evidence: %#v %s", unchanged, data)
	}
	remapped := WithMaterializedHost(forward, 25432)
	data, err = json.Marshal(remapped)
	if err != nil {
		t.Fatal(err)
	}
	if remapped.Host != 25432 || remapped.RequestedHost != 15432 || !strings.Contains(string(data), `"requested_host":15432`) {
		t.Fatalf("remapped allocation lost request evidence: %#v %s", remapped, data)
	}
	var decoded Forward
	if err := json.Unmarshal([]byte(`{"bind":"127.0.0.1","host":25432,"guest":5432,"protocol":"tcp"}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RequestedHost != 0 || RequestedHostPort(decoded) != 25432 {
		t.Fatalf("unchanged port request = %#v", decoded)
	}
}

func TestReuseMaterializedForwardPortsMatchesIntentOneToOne(t *testing.T) {
	t.Parallel()
	persisted := []Forward{
		WithMaterializedHost(Forward{Bind: "127.0.0.1", Host: 15432, Guest: 5432, Protocol: "tcp"}, 25432),
		WithMaterializedHost(Forward{Bind: "127.0.0.1", Host: 16432, Guest: 5432, Protocol: "tcp"}, 26432),
	}
	desired := []Forward{
		{Bind: "127.0.0.1", Host: 16432, Guest: 5432, Protocol: "tcp"},
		{Bind: "127.0.0.1", Host: 15432, Guest: 5432, Protocol: "tcp"},
	}
	got := ReuseMaterializedForwardPorts(desired, persisted)
	if got[0].Host != 26432 || got[0].RequestedHost != 16432 || got[1].Host != 25432 || got[1].RequestedHost != 15432 {
		t.Fatalf("duplicate routes were not paired by request: %#v", got)
	}
	changed := ReuseMaterializedForwardPorts([]Forward{{Bind: "127.0.0.1", Host: 17432, Guest: 5432, Protocol: "tcp"}}, persisted)
	if changed[0].Host != 17432 || changed[0].RequestedHost != 0 {
		t.Fatalf("changed request was silently remapped: %#v", changed)
	}
}
