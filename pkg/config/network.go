package config

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

// The bridge a fleet.yml without `network:` gets.
const (
	DefaultBridgeName = "incusbr0"
	DefaultIPv4CIDR   = "10.0.100.0/24"
)

var bridgeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,14}$`)

// HostNetwork is the bridge the named host uses: the fleet-level `network:`, with each field the host's
// own `network:` sets taking its place. An unknown host (or "") gets the fleet-level one.
func (f *FleetConfig) HostNetwork(host string) NetworkConfig {
	n := f.Network
	if h, ok := f.Hosts[host]; ok && h.Network != nil {
		if s := strings.TrimSpace(h.Network.BridgeName); s != "" {
			n.BridgeName = s
		}
		if s := strings.TrimSpace(h.Network.IPv4CIDR); s != "" {
			n.IPv4CIDR = s
		}
	}
	return n
}

// LocalNetwork is the bridge of the host a daemon (or a local apply) runs on, when that is known: the host
// it was told it is (hostName, from `serve --host-name`), else the only host fleet.yml declares, else the
// fleet-level `network:` when it declares none. With more than one host and no (known) host name it cannot
// tell which one it is on, and ok is false.
func (f *FleetConfig) LocalNetwork(hostName string) (n NetworkConfig, ok bool) {
	if _, known := f.Hosts[hostName]; known && hostName != "" {
		return f.HostNetwork(hostName), true
	}
	switch len(f.Hosts) {
	case 0:
		return f.Network, true
	case 1:
		for name := range f.Hosts {
			return f.HostNetwork(name), true
		}
	}
	return NetworkConfig{}, false
}

// Subnet is the bridge's IPv4 subnet (the CIDR with its host bits cleared).
func (n NetworkConfig) Subnet() (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(n.IPv4CIDR))
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("ipv4_cidr %q is not an IPv4 CIDR", n.IPv4CIDR)
	}
	if p.Bits() > 30 {
		return netip.Prefix{}, fmt.Errorf("ipv4_cidr %q leaves no room for instances (at most /30)", n.IPv4CIDR)
	}
	return p.Masked(), nil
}

// BridgeAddress is the bridge's own ipv4.address in Incus form (address/bits): the CIDR as written when it
// names a host address (10.0.101.254/24), otherwise the subnet's first address (10.0.101.0/24 gives
// 10.0.101.1/24).
func (n NetworkConfig) BridgeAddress() (string, error) {
	sub, err := n.Subnet()
	if err != nil {
		return "", err
	}
	p, _ := netip.ParsePrefix(strings.TrimSpace(n.IPv4CIDR))
	addr := p.Addr()
	if addr == sub.Addr() {
		addr = addr.Next()
	}
	return netip.PrefixFrom(addr, sub.Bits()).String(), nil
}

// Bridge is the bridge's name and its ipv4.address (see BridgeAddress), both checked: the name goes into
// host commands, so it must be a plain interface name.
func (n NetworkConfig) Bridge() (name, address string, err error) {
	name = strings.TrimSpace(n.BridgeName)
	if !bridgeNameRe.MatchString(name) {
		return "", "", fmt.Errorf("bridge_name %q is not a usable interface name", n.BridgeName)
	}
	if address, err = n.BridgeAddress(); err != nil {
		return "", "", err
	}
	return name, address, nil
}

// ValidateHostNetworks checks each host's `network:` override and refuses two hosts whose bridge subnets
// overlap: instance addresses would clash, and the subnets could not be routed to each other. A host
// without an override has the fleet-level subnet, so two such hosts overlap too. A fleet-level CIDR that
// does not parse is left to the plan, which blocks on it.
func (f *FleetConfig) ValidateHostNetworks() error {
	names := make([]string, 0, len(f.Hosts))
	for name := range f.Hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	type subnet struct {
		host string
		p    netip.Prefix
	}
	var seen []subnet
	for _, name := range names {
		if o := f.Hosts[name].Network; o != nil {
			if b := strings.TrimSpace(o.BridgeName); b != "" && !bridgeNameRe.MatchString(b) {
				return fmt.Errorf("hosts.%s.network.bridge_name %q is not a usable interface name", name, o.BridgeName)
			}
			if strings.TrimSpace(o.IPv4CIDR) != "" {
				if _, err := o.Subnet(); err != nil {
					return fmt.Errorf("hosts.%s.network: %w", name, err)
				}
			}
		}
		p, err := f.HostNetwork(name).Subnet()
		if err != nil {
			continue
		}
		for _, s := range seen {
			if s.p.Overlaps(p) {
				return fmt.Errorf("hosts %s and %s have overlapping bridge subnets (%s and %s); give each host its own with hosts.<name>.network.ipv4_cidr", s.host, name, s.p, p)
			}
		}
		seen = append(seen, subnet{name, p})
	}
	return nil
}
