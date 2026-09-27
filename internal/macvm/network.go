package macvm

import (
	"bufio"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

const DefaultSubnet = "10.10.20.0/24"

// ValidateSubnet limits the first product to private /24 IPv4 networks.
func ValidateSubnet(value string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(value)
	if err != nil || !p.Addr().Is4() || p.Bits() != 24 || !p.Addr().IsPrivate() || p != p.Masked() {
		return netip.Prefix{}, fmt.Errorf("mac subnet %q must be a canonical private IPv4 /24 network", value)
	}
	return p, nil
}

func subnetAddress(p netip.Prefix, host byte) netip.Addr {
	b := p.Addr().As4()
	b[3] = host
	return netip.AddrFrom4(b)
}

// SelectSubnet never changes existing configuration: callers use this only
// before first setup and check the saved subnet with CheckSubnet thereafter.
func SelectSubnet(explicit string, occupied []netip.Prefix) (string, error) {
	if explicit != "" {
		if err := CheckSubnet(explicit, occupied); err != nil {
			return "", err
		}
		return explicit, nil
	}
	for _, candidate := range []string{DefaultSubnet, "10.10.21.0/24", "10.88.20.0/24", "172.29.20.0/24", "192.168.242.0/24"} {
		if CheckSubnet(candidate, occupied) == nil {
			return candidate, nil
		}
	}
	return "", errors.New("no conflict-free mac subnet is available; choose a private /24 with --subnet after checking LAN, VPN and Linux VM routes")
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
			return fmt.Errorf("mac subnet %s conflicts with route %s; do not change a saved subnet while instances exist", p, other)
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

// DarwinRoute keeps interface identity: equal routes on a VM bridge and a VPN
// must remain distinct when checking an already active Farrow network.
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

// CheckLiveSubnet recognizes only an active, identity-checked helper's bridge.
// Its own /24, neighbor /32 and the gateway's lo0 /32 routes are expected.
// Broader routes and routes on every other interface still conflict, including
// duplicates installed by a VPN after the original setup selected the subnet.
func CheckLiveSubnet(network NetworkConfig, routes []DarwinRoute, ifconfig string, helperConnected bool) error {
	subnet, err := ValidateSubnet(network.Subnet)
	if err != nil {
		return err
	}
	if network.Gateway != subnetAddress(subnet, 1).String() {
		return errors.New("saved mac gateway does not match subnet")
	}
	bridge := ""
	if helperConnected {
		bridge, err = macBridgeInterface(ifconfig, network.Gateway)
		if err != nil {
			return err
		}
	}
	for _, route := range routes {
		p := route.Prefix
		if !p.IsValid() || !p.Addr().Is4() || p.Bits() == 0 || !p.Overlaps(subnet) {
			continue
		}
		if bridge != "" {
			if route.Interface == bridge && (p == subnet || p.Bits() == 32 && subnet.Contains(p.Addr())) {
				continue
			}
			if route.Interface == "lo0" && p.Bits() == 32 && p.Addr().String() == network.Gateway {
				continue
			}
		}
		return fmt.Errorf("saved mac subnet %s conflicts with route %s on %s; resolve the LAN/VPN/VM route conflict without changing the saved slot addresses", subnet, p, route.Interface)
	}
	return nil
}

func macBridgeInterface(output, gateway string) (string, error) {
	current, matched := "", ""
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' && strings.HasSuffix(fields[0], ":") {
			current = strings.TrimSuffix(fields[0], ":")
			continue
		}
		if len(fields) < 4 || fields[0] != "inet" || fields[1] != gateway || fields[2] != "netmask" || fields[3] != "0xffffff00" {
			continue
		}
		if !strings.HasPrefix(current, "bridge") {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(current, "bridge"))
		if err != nil || index < 0 {
			continue
		}
		if matched != "" && matched != current {
			return "", fmt.Errorf("multiple bridges have saved mac gateway %s; cannot identify the active Mac network", gateway)
		}
		matched = current
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if matched == "" {
		return "", fmt.Errorf("active Mac network has no identifiable bridge for %s/24; retry after checking helper and interface state", gateway)
	}
	return matched, nil
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
