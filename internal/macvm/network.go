package macvm

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"

	"github.com/pgsty/barn/internal/failure"
)

// Each machine gets its own private /24. The runner creates it with vmnet when
// the machine starts and it disappears when the machine stops; the host is .1
// and the guest's MAC has a DHCP reservation for .10. No root is involved.
const (
	gatewayHost = 1
	guestHost   = 10
)

// ValidateSubnet limits machine networks to private IPv4 /24 networks.
func ValidateSubnet(value string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(value)
	if err != nil || !p.Addr().Is4() || p.Bits() != 24 || !p.Addr().IsPrivate() || p != p.Masked() {
		return netip.Prefix{}, failure.New(failure.Usage, fmt.Errorf("subnet %q must be a canonical private IPv4 /24 network such as 10.10.20.0/24", value))
	}
	return p, nil
}

func subnetAddress(p netip.Prefix, host byte) netip.Addr {
	b := p.Addr().As4()
	b[3] = host
	return netip.AddrFrom4(b)
}

// NetworkFor derives a machine's gateway and reserved guest address.
func NetworkFor(p netip.Prefix) MachineNetwork {
	return MachineNetwork{Subnet: p.String(), Gateway: subnetAddress(p, gatewayHost).String(), Address: subnetAddress(p, guestHost).String()}
}

func (n MachineNetwork) Validate() error {
	p, err := ValidateSubnet(n.Subnet)
	if err != nil {
		return err
	}
	if n != NetworkFor(p) {
		return errors.New("machine network must use its subnet's .1 gateway and .10 guest address")
	}
	return nil
}

func validMAC(value string) bool {
	mac, err := net.ParseMAC(value)
	return err == nil && len(mac) == 6 && mac[0]&3 == 2 && mac.String() == value
}

// newMAC returns a random locally administered unicast address.
func newMAC() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[0] = b[0]&0xfc | 0x02
	return net.HardwareAddr(b).String(), nil
}

// candidateSubnets starts at the historical mac1 network and stays inside
// 10.10.0.0/16 next to the Linux lab default, which uses 10.10.10.0/24.
func candidateSubnets() []string {
	candidates := make([]string, 0, 40)
	for third := 20; third < 60; third++ {
		candidates = append(candidates, fmt.Sprintf("10.10.%d.0/24", third))
	}
	return candidates
}

// SelectSubnet picks the first candidate that overlaps neither a host route
// nor another machine's network. An explicit subnet is checked, never replaced.
func SelectSubnet(explicit string, occupied []netip.Prefix) (string, error) {
	if explicit != "" {
		if err := CheckSubnet(explicit, occupied); err != nil {
			return "", err
		}
		return explicit, nil
	}
	for _, candidate := range candidateSubnets() {
		if CheckSubnet(candidate, occupied) == nil {
			return candidate, nil
		}
	}
	return "", failure.New(failure.Resource, errors.New("no free private /24 is available for a new machine network; choose one with --subnet after checking LAN, VPN and VM routes"))
}

func CheckSubnet(value string, occupied []netip.Prefix) error {
	p, err := ValidateSubnet(value)
	if err != nil {
		return err
	}
	for _, other := range occupied {
		if !other.IsValid() || !other.Addr().Is4() || other.Bits() == 0 {
			continue
		}
		if p.Overlaps(other) {
			return failure.New(failure.Resource, fmt.Errorf("subnet %s overlaps %s already in use on this host", p, other))
		}
	}
	return nil
}

// ParseDarwinRoutes parses netstat -rn -f inet destinations, including Darwin's
// abbreviated network notation. Header lines and the default route are ignored.
// Broad VPN split-default routes (0/1, 128/1) are deliberately retained.
func ParseDarwinRoutes(output string) ([]netip.Prefix, error) {
	table, err := ParseDarwinRouteTable(output)
	if err != nil {
		return nil, err
	}
	var routes []netip.Prefix
	seen := map[netip.Prefix]bool{}
	for _, route := range table {
		if !seen[route.Prefix] {
			routes = append(routes, route.Prefix)
			seen[route.Prefix] = true
		}
	}
	return routes, nil
}

// DarwinRoute keeps the interface so diagnostics can name who owns a subnet.
type DarwinRoute struct {
	Prefix    netip.Prefix
	Interface string
	Gateway   string
}

func ParseDarwinRouteTable(output string) ([]DarwinRoute, error) {
	var routes []DarwinRoute
	interfaceColumn := 3
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		dest := fields[0]
		if dest == "Destination" {
			found := false
			for i, field := range fields {
				if field == "Netif" || field == "NetIface" {
					interfaceColumn = i
					found = true
					break
				}
			}
			if !found {
				return nil, errors.New("IPv4 route table has no interface column")
			}
			continue
		}
		if dest == "default" || dest == "Routing" || strings.HasSuffix(dest, ":") {
			continue
		}
		if dest[0] < '0' || dest[0] > '9' || strings.Contains(dest, ":") {
			continue
		}
		p, err := parseRouteDestination(dest)
		if err != nil {
			return nil, fmt.Errorf("invalid IPv4 route destination %q: %w", dest, err)
		}
		if p.Bits() == 0 {
			continue
		}
		if len(fields) <= interfaceColumn {
			return nil, fmt.Errorf("IPv4 route %s has no interface", dest)
		}
		routes = append(routes, DarwinRoute{Prefix: p, Interface: fields[interfaceColumn], Gateway: fields[1]})
	}
	return routes, scanner.Err()
}

func parseRouteDestination(value string) (netip.Prefix, error) {
	address, mask, hasMask := strings.Cut(value, "/")
	parts := strings.Split(address, ".")
	if len(parts) > 4 {
		return netip.Prefix{}, errors.New("too many address octets")
	}
	var octets [4]byte
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || n > 255 {
			return netip.Prefix{}, errors.New("invalid address octet")
		}
		octets[i] = byte(n)
	}
	bits := len(parts) * 8
	if hasMask {
		var err error
		bits, err = strconv.Atoi(mask)
		if err != nil || bits < 0 || bits > 32 {
			return netip.Prefix{}, errors.New("invalid prefix length")
		}
	}
	return netip.PrefixFrom(netip.AddrFrom4(octets), bits).Masked(), nil
}

// hostRoutes reads the live IPv4 routing table.
func hostRoutes(ctx context.Context) ([]DarwinRoute, error) {
	output, err := exec.CommandContext(ctx, "/usr/sbin/netstat", "-rn", "-f", "inet").Output()
	if err != nil {
		return nil, fmt.Errorf("read host IPv4 routes: %w", err)
	}
	return ParseDarwinRouteTable(string(output))
}

// allocateNetwork chooses a subnet that no route and no other machine uses.
func (m *Manager) allocateNetwork(ctx context.Context, requested, self string) (MachineNetwork, error) {
	routes, err := m.routes(ctx)
	if err != nil {
		return MachineNetwork{}, err
	}
	occupied := make([]netip.Prefix, 0, len(routes))
	for _, route := range routes {
		occupied = append(occupied, route.Prefix)
	}
	machines, err := m.Store.ListMachines()
	if err != nil {
		return MachineNetwork{}, err
	}
	for _, other := range machines {
		if other.Name == self {
			continue
		}
		if p, err := netip.ParsePrefix(other.Network.Subnet); err == nil {
			occupied = append(occupied, p)
		}
	}
	selected, err := SelectSubnet(requested, occupied)
	if err != nil {
		return MachineNetwork{}, err
	}
	p, err := ValidateSubnet(selected)
	if err != nil {
		return MachineNetwork{}, err
	}
	return NetworkFor(p), nil
}

// checkNetworkFree runs before a machine starts: its private network does not
// exist yet, so any overlapping route belongs to something else (a VPN, a LAN,
// another VM tool) and would make the guest unreachable.
func (m *Manager) checkNetworkFree(ctx context.Context, machine *Machine) error {
	routes, err := m.routes(ctx)
	if err != nil {
		return err
	}
	return m.checkRoutesFree(machine, routes)
}

func (m *Manager) checkRoutesFree(machine *Machine, routes []DarwinRoute) error {
	subnet, err := ValidateSubnet(machine.Network.Subnet)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if route.Prefix.IsValid() && route.Prefix.Addr().Is4() && route.Prefix.Bits() != 0 && route.Prefix.Overlaps(subnet) {
			next := fmt.Sprintf("barn mac configure %s --subnet auto", machine.Name)
			return failure.New(failure.Resource, fmt.Errorf("%s network %s overlaps route %s on %s", machine.Name, subnet, route.Prefix, route.Interface)).
				Because("mac_subnet_in_use").Then(next)
		}
	}
	return nil
}

func (m *Manager) routes(ctx context.Context) ([]DarwinRoute, error) {
	if m.hostRoutes != nil {
		return m.hostRoutes(ctx)
	}
	return hostRoutes(ctx)
}
