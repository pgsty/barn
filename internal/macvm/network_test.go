package macvm

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

func TestNetworkStatusRecognizesAnchorWithoutAttachedGuests(t *testing.T) {
	for _, test := range []struct {
		name, wire string
		active     bool
	}{
		{"anchor-only", `{"network_active":true,"connected":[false,false]}`, true},
		{"old-helper-active", `{"connected":[false,true]}`, true},
		{"anchor-and-guest", `{"network_active":true,"connected":[true,false]}`, true},
		{"inactive", `{"network_active":false,"connected":[false,false]}`, false},
		{"old-helper-inactive", `{"connected":[false,false]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var status NetworkStatus
			if err := json.Unmarshal([]byte(test.wire), &status); err != nil {
				t.Fatal(err)
			}
			if status.hasActiveNetwork() != test.active {
				t.Fatalf("active=%t expected=%t", status.hasActiveNetwork(), test.active)
			}
			routes, err := ParseDarwinRouteTable(macLiveRoutesFixture)
			if err != nil {
				t.Fatal(err)
			}
			err = CheckLiveSubnet(NetworkConfig{Subnet: DefaultSubnet, Gateway: "10.10.20.1"}, routes, macBridgeFixture, status.hasActiveNetwork())
			if (err == nil) != test.active {
				t.Fatalf("own bridge exemption differs from anchor/interface state: %v", err)
			}
		})
	}
	var absent *NetworkStatus
	if absent.hasActiveNetwork() {
		t.Fatal("unverified/missing status claimed an active network")
	}
	var status NetworkStatus
	if err := json.Unmarshal([]byte(`{"network_active":1,"connected":[false,false]}`), &status); err == nil {
		t.Fatal("accepted numeric substitute for protocol boolean")
	}
}

func TestRouteSelectionCoversLANVPNAndLinux(t *testing.T) {
	routes, err := ParseDarwinRoutes(`Routing tables
Internet:
Destination Gateway Flags Netif Expire
default 192.168.1.1 UGScg en0
10.10.10 link#20 UCS bridge100
10.10.20/24 10.5.0.1 UGSc utun3
192.168.1  link#4 UCS en0
127 127.0.0.1 UCS lo0
10.10.10.10 aa:bb:cc:dd:ee:ff UHLWI bridge100
`)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := SelectSubnet("", routes)
	if err != nil || selected != "10.10.21.0/24" {
		t.Fatalf("selected=%s err=%v", selected, err)
	}
	if err := CheckSubnet(DefaultSubnet, routes); err == nil {
		t.Fatal("saved subnet conflict was ignored")
	}
	if _, err := SelectSubnet(DefaultSubnet, routes); err == nil {
		t.Fatal("explicit conflict was ignored")
	}
	for _, s := range []string{"10.10.20.1/24", "8.8.8.0/24", "10.10.0.0/16", "fd00::/64", "bad"} {
		if _, err := ValidateSubnet(s); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}

const macBridgeFixture = `lo0: flags=8049<UP,LOOPBACK,RUNNING> mtu 16384
    inet 127.0.0.1 netmask 0xff000000
bridge100: flags=8a63<UP,BROADCAST,RUNNING> mtu 1500
    inet 10.10.10.1 netmask 0xffffff00 broadcast 10.10.10.255
bridge101: flags=8a63<UP,BROADCAST,RUNNING> mtu 1500
    inet 10.10.20.1 netmask 0xffffff00 broadcast 10.10.20.255
    inet6 fe80::1%bridge101 prefixlen 64
utun6: flags=8051<UP,POINTOPOINT,RUNNING> mtu 1380
    inet 100.99.0.15 --> 100.99.0.15 netmask 0xffffffff
`

const macLiveRoutesFixture = `Routing tables
Internet:
Destination Gateway Flags Netif Expire
default 192.168.0.1 UGScg en15
10.10.10/24 link#19 UC bridge100 !
10.10.20/24 link#33 UC bridge101 !
10.10.20.1 10.10.20.1 UH lo0
10.10.20.10 02:cc:bb:aa:00:00 UHLWI bridge101 1200
10.10.20.11 02:cc:bb:aa:00:01 UHLWI bridge101 1200
10.10.20.255 ff:ff:ff:ff:ff:ff UHLWbI bridge101 !
100.64/10 link#27 UCS utun6
192.168.0 link#32 UCS en15 !
`

func TestLiveNetworkExemptsOnlyItsProvenBridgeAndGateway(t *testing.T) {
	network := NetworkConfig{Subnet: DefaultSubnet, Gateway: "10.10.20.1"}
	routes, err := ParseDarwinRouteTable(macLiveRoutesFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLiveSubnet(network, routes, macBridgeFixture, true); err != nil {
		t.Fatal(err)
	}
	if err := CheckLiveSubnet(network, routes, macBridgeFixture, false); err == nil {
		t.Fatal("exempted a bridge without active helper association")
	}
	for _, extra := range []string{
		"10.10.20/24 link#28 UCS utun7\n",
		"10.10.20.10/32 link#28 UCS utun7\n",
		"10.10/16 link#28 UCS utun7\n",
		"0/1 link#28 UCS utun7\n",
		"10/8 link#33 UCS bridge101\n",
		"10.10.20.10 10.10.20.10 UH lo0\n",
		"10.10.20/24 link#34 UCS bridge102\n",
		"10.10.20.1/32 link#4 UCS en0\n",
	} {
		t.Run(strings.Fields(extra)[0]+"-"+strings.Fields(extra)[3], func(t *testing.T) {
			changed, err := ParseDarwinRouteTable(macLiveRoutesFixture + extra)
			if err != nil {
				t.Fatal(err)
			}
			if err := CheckLiveSubnet(network, changed, macBridgeFixture, true); err == nil {
				t.Fatalf("ignored post-setup conflict: %s", extra)
			}
		})
	}
}

func TestLiveBridgeIdentityFailsClosed(t *testing.T) {
	network := NetworkConfig{Subnet: DefaultSubnet, Gateway: "10.10.20.1"}
	routes, _ := ParseDarwinRouteTable(macLiveRoutesFixture)
	for _, interfaces := range []string{
		"",
		strings.ReplaceAll(macBridgeFixture, "10.10.20.1 netmask 0xffffff00", "10.10.20.1 netmask 0xffff0000"),
		strings.ReplaceAll(macBridgeFixture, "bridge101:", "en0:"),
		macBridgeFixture + "bridge102: flags=8a63<UP,BROADCAST,RUNNING> mtu 1500\n    inet 10.10.20.1 netmask 0xffffff00\n",
	} {
		if err := CheckLiveSubnet(network, routes, interfaces, true); err == nil {
			t.Fatal("accepted missing or ambiguous bridge identity")
		}
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
		t.Fatal("accepted table without interface column")
	}
}

func TestSplitDefaultVPNRoutesAreConflicts(t *testing.T) {
	routes, err := ParseDarwinRoutes("0/1 10.5.0.1 UGSc utun3\n128.0/1 10.5.0.1 UGSc utun3\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SelectSubnet("", routes); err == nil {
		t.Fatal("ignored split default VPN")
	}
	if selected, err := SelectSubnet("", []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}); err != nil || selected != DefaultSubnet {
		t.Fatalf("default route must not consume every subnet: %s %v", selected, err)
	}
}

func TestRouteParsingFailsClosedOnMalformedIPv4(t *testing.T) {
	if _, err := ParseDarwinRoutes("10.10.300 link#3 UCS en0\n"); err == nil {
		t.Fatal("silently ignored malformed route")
	}
	p, err := parseRouteDestination("10.10")
	if err != nil || p.String() != "10.10.0.0/16" {
		t.Fatalf("p=%s err=%v", p, err)
	}
}
