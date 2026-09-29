// Package subnet defines Barn's one host-global private IPv4 /24 layout.
package subnet

import (
	"errors"
	"fmt"
	"net/netip"
)

const DefaultCIDR = "10.10.10.0/24"

type Layout struct {
	prefix netip.Prefix
}

func Parse(cidr string) (Layout, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 24 || prefix != prefix.Masked() {
		return Layout{}, fmt.Errorf("the Barn network must be a canonical IPv4 /24 such as 10.10.10.0/24, got %q", cidr)
	}
	if !prefix.Addr().IsPrivate() {
		return Layout{}, fmt.Errorf("the Barn network must be private (RFC1918: 10/8, 172.16/12, or 192.168/16), got %q", cidr)
	}
	return Layout{prefix: prefix}, nil
}

func Default() Layout {
	layout, err := Parse(DefaultCIDR)
	if err != nil {
		panic(err)
	}
	return layout
}

func FromHostAddress(host string) (Layout, error) {
	address, err := netip.ParseAddr(host)
	if err != nil || !address.Is4() {
		return Layout{}, fmt.Errorf("private host address must be IPv4: %q", host)
	}
	value := address.As4()
	if value[3] != 1 {
		return Layout{}, fmt.Errorf("private host address must be the /24 .1 address: %q", host)
	}
	value[3] = 0
	return Parse(netip.PrefixFrom(netip.AddrFrom4(value), 24).String())
}

func (l Layout) valid() bool { return l.prefix.IsValid() }

func (l Layout) CIDR() string {
	if !l.valid() {
		return ""
	}
	return l.prefix.String()
}

func (l Layout) Prefix() netip.Prefix { return l.prefix }

func (l Layout) Address(last byte) string {
	if !l.valid() {
		return ""
	}
	value := l.prefix.Addr().As4()
	value[3] = last
	return netip.AddrFrom4(value).String()
}

func (l Layout) HostAddress() string  { return l.Address(1) }
func (l Layout) DHCPEnd() string      { return l.Address(8) }
func (l Layout) StaticStart() string  { return l.Address(9) }
func (l Layout) StaticEnd() string    { return l.Address(254) }
func (l Layout) ProbeAddress() string { return l.Address(9) }

func (l Layout) StaticAddresses() []string {
	if !l.valid() {
		return nil
	}
	addresses := make([]string, 0, 246)
	for last := 9; last <= 254; last++ {
		addresses = append(addresses, l.Address(byte(last)))
	}
	return addresses
}

func (l Layout) IsDefault() bool { return l.CIDR() == DefaultCIDR }

func (l Layout) Last(address string) (byte, error) {
	parsed, err := netip.ParseAddr(address)
	if err != nil || !parsed.Is4() || !l.prefix.Contains(parsed) {
		return 0, fmt.Errorf("address %q is outside %s", address, l.CIDR())
	}
	return parsed.As4()[3], nil
}

func (l Layout) IsStatic(address string) bool {
	last, err := l.Last(address)
	return err == nil && last >= 9 && last <= 254
}

func (l Layout) RebaseStatic(address string, source Layout) (string, error) {
	if !l.valid() || !source.valid() {
		return "", errors.New("cannot rebase with an invalid private layout")
	}
	last, err := source.Last(address)
	if err != nil || last < 9 || last > 254 {
		return "", fmt.Errorf("private node address %q is not in %s static pool", address, source.CIDR())
	}
	return l.Address(last), nil
}

func (l Layout) Warning() string {
	if l.IsDefault() {
		return ""
	}
	return fmt.Sprintf("using the non-default Barn network %s (host %s, DHCP up to %s, nodes %s-%s); the installed network and every node address must use it", l.CIDR(), l.HostAddress(), l.DHCPEnd(), l.StaticStart(), l.StaticEnd())
}
