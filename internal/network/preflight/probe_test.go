package preflight

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/pgsty/farrow/internal/execx"
	darwinnet "github.com/pgsty/farrow/internal/network/darwin"
	linuxnet "github.com/pgsty/farrow/internal/network/linux"
	"github.com/pgsty/farrow/internal/network/subnet"
)

type sharingRunner struct{}

func (sharingRunner) Run(_ context.Context, binary string, args ...string) (execx.Result, error) {
	command := binary + " " + strings.Join(args, " ")
	switch {
	case strings.HasPrefix(command, "/usr/bin/tail "):
		return execx.Result{}, errors.New("no Farrow log")
	case command == "/bin/launchctl print system/com.apple.NetworkSharing":
		return execx.Result{Stdout: []byte("active count = 2\n")}, nil
	case strings.Contains(command, "-extract Host_Net_Address"):
		return execx.Result{Stdout: []byte("10.10.10.1\n")}, nil
	case strings.Contains(command, "-extract Host_Net_Mask"):
		return execx.Result{Stdout: []byte("255.255.255.0\n")}, nil
	default:
		return execx.Result{}, errors.New("unexpected command: " + command)
	}
}

type unreadableSharingRunner struct{}

func (unreadableSharingRunner) Run(ctx context.Context, binary string, args ...string) (execx.Result, error) {
	if strings.Contains(strings.Join(args, " "), "Host_Net_Address") {
		return execx.Result{}, errors.New("permission denied")
	}
	return (sharingRunner{}).Run(ctx, binary, args...)
}

func TestParseDarwinInterfacesAndRoutes(t *testing.T) {
	interfaces := parseDarwinInterfaces("en0: flags=1\n\tinet 192.168.0.11 netmask 0xffffff00 broadcast 192.168.0.255\nbridge100: flags=1\n\tinet 10.10.10.1 netmask 0xffffff00 broadcast 10.10.10.255\n")
	if len(interfaces) != 2 || interfaces[1].Interface != "bridge100" || interfaces[1].Prefix.Bits() != 24 {
		t.Fatalf("interfaces=%#v", interfaces)
	}
	routes := parseDarwinRoutes("Routing tables\n\nInternet:\nDestination Gateway Flags Netif Expire\ndefault 192.168.0.1 UGScg en0\n10 192.168.0.1 UGSc en7\n10.10.10/24 link#24 UCS bridge100 !\n10.10.10.128/25 10.0.0.1 UGSc utun9\n10.10.10.10 aa:bb:cc:dd:ee:ff UHLWI bridge100 100\n")
	if len(routes) != 3 || routes[0].Prefix.String() != "10.0.0.0/8" || routes[0].Interface != "en7" || routes[1].Prefix.String() != "10.10.10.0/24" || routes[1].Interface != "bridge100" || routes[2].Prefix.String() != "10.10.10.128/25" || routes[2].Interface != "utun9" {
		t.Fatalf("Darwin full routes=%#v", routes)
	}
}

func TestDarwinInterfaceOwnershipNeverAdoptsForeignExactAddress(t *testing.T) {
	layout := subnet.Default()
	marker := darwinnet.InterfaceMarker{Schema: 1, InterfaceID: "018f4b8e-1234-7abc-9def-0123456789ab", CIDR: layout.CIDR(), HostAddress: layout.HostAddress(), BSDName: "bridge100"}
	foreign := []InterfaceAddress{{Address: netip.MustParseAddr(layout.HostAddress()), Prefix: layout.Prefix(), Interface: "vboxnet0", Evidence: "foreign VirtualBox host-only interface"}}
	if darwinInterfaceObserved(foreign, layout, marker) {
		t.Fatal("foreign vboxnet0 exact .1/24 was adopted as the Farrow interface")
	}
	owned := append(foreign, InterfaceAddress{Address: netip.MustParseAddr(layout.HostAddress()), Prefix: layout.Prefix(), Interface: "bridge100", Evidence: "marker-bound interface"})
	if !darwinInterfaceObserved(owned, layout, marker) {
		t.Fatal("marker-bound exact interface was not observed")
	}
	report := Evaluate(Request{OS: "darwin", Arch: "arm64", Purpose: Use, Layout: layout}, Snapshot{
		Installation: Installation{Status: "protected", CIDR: layout.CIDR(), HostAddress: layout.HostAddress(), Interface: marker.BSDName, Healthy: true},
		Routes:       []Route{{Prefix: layout.Prefix(), Interface: marker.BSDName, Evidence: "owned route"}},
		Interfaces:   owned,
	})
	if report.Ready || report.ExitCode != 6 {
		t.Fatalf("foreign exact interface did not remain a resource conflict: %#v", report)
	}
}

func TestParseLinuxInterfacesAndRoutes(t *testing.T) {
	interfaces := parseLinuxInterfaces("2: eth0    inet 192.168.0.10/24 brd 192.168.0.255 scope global eth0\n3: farrow0    inet 172.31.251.1/24 scope global farrow0\n")
	if len(interfaces) != 2 || interfaces[1].Interface != "farrow0" || interfaces[1].Address.String() != "172.31.251.1" {
		t.Fatalf("interfaces=%#v", interfaces)
	}
	routes := parseLinuxRoutes("default via 192.168.0.1 dev eth0\n172.31.251.0/24 dev farrow0 proto kernel scope link src 172.31.251.1\nlocal 172.31.251.1 dev farrow0 table local proto kernel scope host\nblackhole 172.31.251.128/25 metric 10\nunreachable 172.31.251.64/26 metric 20\nbroadcast 172.31.251.255 dev farrow0 table local\n")
	if len(routes) != 4 || routes[0].Prefix.String() != "172.31.251.0/24" || routes[1].Prefix.String() != "172.31.251.1/32" || routes[1].Kind != "local" || routes[2].Kind != "blackhole" || routes[2].Prefix.String() != "172.31.251.128/25" || routes[3].Kind != "unreachable" {
		t.Fatalf("routes=%#v", routes)
	}
}

func TestSimplifiedLinuxInstallShapesDoNotRequireRetiredLeaseFiles(t *testing.T) {
	t.Parallel()
	for name, targets := range map[string][]linuxPublicTarget{
		"networkmanager": linuxRequiredTargets(true, "/var/lib/farrow"),
		"networkd":       linuxRequiredTargets(false, "/var/lib/farrow"),
	} {
		for _, target := range targets {
			if target.path == linuxnet.TmpfilesPath || target.path == linuxnet.LeaseRoot || target.path == linuxnet.LeaseLockPath {
				t.Fatalf("%s still requires retired lease target %s", name, target.path)
			}
		}
	}
}

func TestPlistArgumentsAndLinuxUnit(t *testing.T) {
	data := []byte(`["/opt/farrow/libexec/socket_vmnet","--vmnet-mode=shared","--vmnet-gateway=172.31.251.1","--vmnet-dhcp-end=172.31.251.8","--vmnet-interface-id=018f4b8e-1234-7abc-9def-0123456789ab"]`)
	mode, _, gateway, dhcp, err := plistArguments(data)
	if err != nil || mode != "shared" || gateway != "172.31.251.1" || dhcp != "172.31.251.8" {
		t.Fatalf("mode=%q gateway=%q dhcp=%q err=%v", mode, gateway, dhcp, err)
	}
	layout, err := linuxNetworkLayout([]byte("[Network]\nAddress=172.31.251.1/24\n"))
	if err != nil || layout.CIDR() != "172.31.251.0/24" {
		t.Fatalf("layout=%#v err=%v", layout, err)
	}
}

func TestActiveNetworkSharingAndLinuxFamily(t *testing.T) {
	if !activeNetworkSharing("active count = 2\nstate = running\n") || activeNetworkSharing("active count = 0\n") {
		t.Fatal("active NetworkSharing count was parsed incorrectly")
	}
	for input, want := range map[string]string{
		"ID=ubuntu\nID_LIKE=debian\n":                "debian",
		"ID=rocky\nID_LIKE=\"rhel centos fedora\"\n": "rpm",
	} {
		family, err := linuxFamily([]byte(input))
		if err != nil || string(family) != want {
			t.Fatalf("family(%q)=%q, %v", input, family, err)
		}
	}
	conflict, problem := (Probe{Runner: sharingRunner{}}).darwinSharingConflict(context.Background(), Request{OS: "darwin", Arch: "arm64", Purpose: Install, Layout: subnet.Default()}, Installation{Status: "absent"})
	if !strings.Contains(conflict, "Internet Sharing") || !strings.Contains(conflict, "10.10.10.0/24") {
		t.Fatalf("sharing conflict=%q", conflict)
	}
	if problem != "" {
		t.Fatalf("sharing problem=%q", problem)
	}
	if conflict, problem := (Probe{Runner: sharingRunner{}}).darwinSharingConflict(context.Background(), Request{OS: "darwin", Arch: "arm64", Purpose: Use, Layout: subnet.Default()}, Installation{Status: "protected", CIDR: subnet.DefaultCIDR, Healthy: true}); conflict != "" || problem != "" {
		t.Fatalf("healthy protected Farrow install was treated as external sharing: %q", conflict)
	}
}

func TestActiveNetworkSharingUnknownSubnetFailsClosed(t *testing.T) {
	conflict, problem := (Probe{Runner: unreadableSharingRunner{}}).darwinSharingConflict(context.Background(), Request{OS: "darwin", Arch: "arm64", Purpose: Install, Layout: subnet.Default()}, Installation{Status: "absent"})
	if conflict != "" || !strings.Contains(problem, "active") || !strings.Contains(problem, "could not be read") {
		t.Fatalf("conflict=%q problem=%q", conflict, problem)
	}
	report := Evaluate(Request{OS: "darwin", Arch: "arm64", Purpose: Install, Layout: subnet.Default()}, Snapshot{Installation: Installation{Status: "absent"}, Problems: []string{problem}})
	if report.Ready || report.ExitCode != 3 || len(report.Findings) == 0 || report.Findings[0].Code != "probe.incomplete" {
		t.Fatalf("report=%#v", report)
	}
}
