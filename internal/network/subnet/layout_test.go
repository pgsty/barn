package subnet

import (
	"strings"
	"testing"
)

func TestLayoutAndRebase(t *testing.T) {
	defaultLayout := Default()
	custom, err := Parse("172.31.251.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if custom.HostAddress() != "172.31.251.1" || custom.DHCPEnd() != "172.31.251.8" || custom.StaticEnd() != "172.31.251.254" {
		t.Fatalf("custom layout = %#v", custom)
	}
	rebased, err := custom.RebaseStatic("10.10.10.13", defaultLayout)
	if err != nil || rebased != "172.31.251.13" {
		t.Fatalf("rebase=%q err=%v", rebased, err)
	}
	if custom.Warning() == "" || !strings.Contains(custom.Warning(), "non-default Farrow network") || defaultLayout.Warning() != "" {
		t.Fatal("warning policy mismatch")
	}
	addresses := custom.StaticAddresses()
	if len(addresses) != 246 || addresses[0] != "172.31.251.9" || addresses[len(addresses)-1] != "172.31.251.254" {
		t.Fatalf("static addresses = len %d first=%q last=%q", len(addresses), addresses[0], addresses[len(addresses)-1])
	}
}

func TestRejectsUnsafeOrNonCanonicalNetworks(t *testing.T) {
	for _, value := range []string{"10.10.10.1/24", "10.10.10.0/25", "8.8.8.0/24", "169.254.1.0/24", "2001:db8::/64", "invalid"} {
		if _, err := Parse(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}
