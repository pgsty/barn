package macvm

import (
	"context"
	"net/netip"
	"testing"

	"github.com/pgsty/farrow/internal/failure"
)

const liveRoutesFixture = `Routing tables
Internet:
Destination Gateway Flags Netif Expire
default 192.168.0.1 UGScg en15
10.10.10/24 link#19 UC bridge100 !
10.10.20/24 10.5.0.1 UGSc utun3
100.64/10 link#27 UCS utun6
192.168.0 link#32 UCS en15 !
127 127.0.0.1 UCS lo0
10.10.10.10 aa:bb:cc:dd:ee:ff UHLWI bridge100
`

func TestSubnetSelectionAvoidsLANVPNAndLinux(t *testing.T) {
	routes, err := ParseDarwinRoutes(liveRoutesFixture)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := SelectSubnet("", routes)
	if err != nil || selected != "10.10.21.0/24" {
		t.Fatalf("selected=%s err=%v", selected, err)
	}
	if _, err := SelectSubnet("10.10.20.0/24", routes); err == nil {
		t.Fatal("an explicit conflicting subnet was accepted")
	}
	for _, s := range []string{"10.10.20.1/24", "8.8.8.0/24", "10.10.0.0/16", "fd00::/64", "bad"} {
		if _, err := ValidateSubnet(s); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}

func TestMachineNetworkShape(t *testing.T) {
	network := NetworkFor(netip.MustParsePrefix("10.10.42.0/24"))
	if network.Gateway != "10.10.42.1" || network.Address != "10.10.42.10" || network.Validate() != nil {
		t.Fatalf("network %+v", network)
	}
	network.Address = "10.10.42.11"
	if network.Validate() == nil {
		t.Fatal("a non-.10 guest address was accepted")
	}
	for i := 0; i < 100; i++ {
		mac, err := newMAC()
		if err != nil || !validMAC(mac) {
			t.Fatalf("generated %q %v", mac, err)
		}
	}
	for _, mac := range []string{"01:00:00:00:00:01", "00:11:22:33:44:55", "5E:40:07:08:6C:4E", "bad"} {
		if validMAC(mac) {
			t.Errorf("accepted %q", mac)
		}
	}
}

func TestAllocationSkipsOtherMachinesAndKeepsItsOwn(t *testing.T) {
	m, _ := testManager(t)
	testMachine(t, m, "mac1", "10.10.20.0/24", false)
	testMachine(t, m, "mac2", "10.10.21.0/24", false)
	network, err := m.allocateNetwork(context.Background(), "", "dev")
	if err != nil || network.Subnet != "10.10.22.0/24" {
		t.Fatalf("allocated %+v %v", network, err)
	}
	if _, err := m.allocateNetwork(context.Background(), "10.10.21.0/24", "dev"); err == nil {
		t.Fatal("another machine's subnet was assigned")
	}
	if network, err := m.allocateNetwork(context.Background(), "10.10.21.0/24", "mac2"); err != nil || network.Address != "10.10.21.10" {
		t.Fatalf("a machine could not keep its own subnet: %+v %v", network, err)
	}
}

func TestStartRefusesAnOccupiedSubnetWithTheRightNext(t *testing.T) {
	m, _ := testManager(t)
	machine := testMachine(t, m, "dev", "10.10.20.0/24", false)
	m.hostRoutes = func(context.Context) ([]DarwinRoute, error) {
		return ParseDarwinRouteTable(liveRoutesFixture)
	}
	err := m.checkNetworkFree(context.Background(), machine)
	class, reason, next := failure.Classify(err)
	if class != failure.Resource || reason != "mac_subnet_in_use" || next != "farrow mac configure dev --subnet auto" {
		t.Fatalf("err=%v class=%s reason=%s next=%s", err, class, reason, next)
	}
}

func TestDarwinRouteParserRetainsDuplicateDestinationsAndHeaderLayout(t *testing.T) {
	routes, err := ParseDarwinRouteTable(`Destination Gateway Flags Refs Use Netif Expire
10.10.20/24 link#33 UCS 0 0 bridge101 !
10.10.20/24 link#28 UCS 0 0 utun7 !
`)
	if err != nil || len(routes) != 2 || routes[0].Interface != "bridge101" || routes[1].Interface != "utun7" {
		t.Fatalf("routes=%+v err=%v", routes, err)
	}
	if _, err := ParseDarwinRouteTable("Destination Gateway Flags\n10.10.20 link#4 UCS\n"); err == nil {
		t.Fatal("accepted a table without an interface column")
	}
}

func TestSplitDefaultVPNRoutesAreConflicts(t *testing.T) {
	routes, err := ParseDarwinRoutes("0/1 10.5.0.1 UGSc utun3\n128.0/1 10.5.0.1 UGSc utun3\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SelectSubnet("", routes); err == nil {
		t.Fatal("ignored a split default VPN")
	}
	if selected, err := SelectSubnet("", []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}); err != nil || selected != "10.10.20.0/24" {
		t.Fatalf("a default route must not consume every subnet: %s %v", selected, err)
	}
}

func TestRouteParsingFailsClosedOnMalformedIPv4(t *testing.T) {
	if _, err := ParseDarwinRoutes("10.10.300 link#3 UCS en0\n"); err == nil {
		t.Fatal("silently ignored a malformed route")
	}
	p, err := parseRouteDestination("10.10")
	if err != nil || p.String() != "10.10.0.0/16" {
		t.Fatalf("p=%s err=%v", p, err)
	}
}
