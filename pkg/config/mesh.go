package config

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// A fleet of several hosts can join them with a WireGuard mesh (fleet.yml `mesh:`): each host gets one
// interface with a tunnel address, a peer for every other host, and a route over the mesh to every other
// host's bridge subnet, so an instance's bridge address on one host answers from every other. Each host's
// daemon converges its own side through the normal plan and apply (engine.PlanMesh). Its private key is
// generated on the host and never leaves it; fleet.yml holds only the public keys.

// Defaults for fleet.yml's `mesh:`.
const (
	DefaultMeshInterface  = "wg0"
	DefaultMeshListenPort = 51820
)

// MeshConfig is fleet.yml's `mesh:`. Absent, or with enabled false, nothing about a mesh is planned.
type MeshConfig struct {
	Enabled bool `yaml:"enabled"`
	// Interface is the WireGuard interface on every host (default wg0).
	Interface string `yaml:"interface,omitempty"`
	// ListenPort is the UDP port every host listens on (default 51820).
	ListenPort int `yaml:"listen_port,omitempty"`
	// Subnet is the range the hosts' tunnel addresses come from, e.g. 10.255.0.0/24. It must not overlap
	// any host's bridge subnet.
	Subnet string `yaml:"subnet"`
}

// HostMeshConfig is a host's own part of the mesh, `hosts.<name>.mesh`.
type HostMeshConfig struct {
	// PublicKey is the host's WireGuard public key (base64), as its daemon's plan reports it. Only the
	// public key: the private key is generated on the host and never leaves it.
	PublicKey string `yaml:"public_key,omitempty"`
	// Address is the host's tunnel address, inside mesh.subnet. Empty: derived from the subnet and the
	// sorted host names (see MeshPeer).
	Address string `yaml:"address,omitempty"`
	// Endpoint is where the other hosts reach this one: host or host:port. Empty: hosts.<name>.address,
	// with mesh.listen_port.
	Endpoint string `yaml:"endpoint,omitempty"`
}

// MeshPeer is one host's resolved place in the mesh.
type MeshPeer struct {
	Host      string
	PublicKey string       // empty while fleet.yml does not have it yet
	Address   netip.Addr   // the tunnel address
	Endpoint  string       // host:port; empty when the host has neither mesh.endpoint nor a usable address
	Bridge    netip.Prefix // the host's bridge subnet
}

var (
	meshIfaceRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,14}$`)
	// A hostname or an IPv4 address, as an endpoint's host part.
	meshHostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252}[A-Za-z0-9])?$`)
)

// MeshEnabled reports whether fleet.yml asks for a mesh.
func (f *FleetConfig) MeshEnabled() bool { return f.Mesh != nil && f.Mesh.Enabled }

// MeshInterface is the mesh's interface name (mesh.interface, default wg0).
func (m *MeshConfig) MeshInterface() string {
	if s := strings.TrimSpace(m.Interface); s != "" {
		return s
	}
	return DefaultMeshInterface
}

// Port is the mesh's UDP port (mesh.listen_port, default 51820).
func (m *MeshConfig) Port() int {
	if m.ListenPort != 0 {
		return m.ListenPort
	}
	return DefaultMeshListenPort
}

// MeshSubnet is mesh.subnet, parsed (an IPv4 prefix, host bits cleared).
func (m *MeshConfig) MeshSubnet() (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(m.Subnet))
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("mesh.subnet %q is not an IPv4 CIDR (for example 10.255.0.0/24)", m.Subnet)
	}
	if p.Bits() > 30 {
		return netip.Prefix{}, fmt.Errorf("mesh.subnet %q is too small (at most /30)", m.Subnet)
	}
	return p.Masked(), nil
}

// ValidWireGuardKey reports whether s is a WireGuard key: 32 bytes, base64.
func ValidWireGuardKey(s string) bool {
	b, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(b) == 32 && len(s) == 44
}

// MeshPeers is every host's place in the mesh, in sorted host order. A host's tunnel address is its
// hosts.<name>.mesh.address, else the subnet's (i+1)th address for the ith host in sorted order, so the
// same fleet.yml gives every daemon the same addresses. It checks the mesh's shape (ValidateMesh calls it):
// the interface and port, the subnet and its overlap with every bridge subnet, each address (inside the
// subnet, not its network or broadcast address, unique) and each public key's form. A missing public key
// or endpoint is not an error here: the plan reports it (a host's key exists only once its daemon has
// made it).
func (f *FleetConfig) MeshPeers() ([]MeshPeer, error) {
	m := f.Mesh
	if m == nil {
		return nil, nil
	}
	if !meshIfaceRe.MatchString(m.MeshInterface()) {
		return nil, fmt.Errorf("mesh.interface %q is not a usable interface name", m.Interface)
	}
	if p := m.Port(); p < 1 || p > 65535 {
		return nil, fmt.Errorf("mesh.listen_port %d is not a port", m.ListenPort)
	}
	subnet, err := m.MeshSubnet()
	if err != nil {
		return nil, err
	}
	names := f.HostNames()
	if len(names) == 0 {
		return nil, fmt.Errorf("mesh: fleet.yml has no hosts: to join")
	}
	broadcast := lastAddr(subnet)
	var peers []MeshPeer
	used := map[netip.Addr]string{}
	for i, name := range names {
		h := f.Hosts[name]
		var hm HostMeshConfig
		if h.Mesh != nil {
			hm = *h.Mesh
		}
		p := MeshPeer{Host: name, PublicKey: strings.TrimSpace(hm.PublicKey)}
		if p.PublicKey != "" && !ValidWireGuardKey(p.PublicKey) {
			return nil, fmt.Errorf("hosts.%s.mesh.public_key is not a WireGuard public key (44 characters of base64)", name)
		}
		if s := strings.TrimSpace(hm.Address); s != "" {
			a, err := netip.ParseAddr(strings.TrimSuffix(s, "/32"))
			if err != nil || !a.Is4() {
				return nil, fmt.Errorf("hosts.%s.mesh.address %q is not an IPv4 address", name, hm.Address)
			}
			p.Address = a
		} else {
			a := subnet.Addr()
			for j := 0; j <= i; j++ {
				a = a.Next()
			}
			p.Address = a
		}
		if !subnet.Contains(p.Address) || p.Address == subnet.Addr() || p.Address == broadcast {
			return nil, fmt.Errorf("host %s's tunnel address %s is not a usable address of mesh.subnet %s", name, p.Address, subnet)
		}
		if other, dup := used[p.Address]; dup {
			return nil, fmt.Errorf("hosts %s and %s have the same tunnel address %s; give one its own hosts.<name>.mesh.address", other, name, p.Address)
		}
		used[p.Address] = name
		if p.Bridge, err = f.HostNetwork(name).Subnet(); err != nil {
			return nil, fmt.Errorf("hosts.%s.network: %w", name, err)
		}
		if p.Bridge.Overlaps(subnet) {
			return nil, fmt.Errorf("mesh.subnet %s overlaps host %s's bridge subnet %s; pick a range no bridge uses", subnet, name, p.Bridge)
		}
		if p.Endpoint, err = meshEndpoint(name, hm.Endpoint, h.Address, m.Port()); err != nil {
			return nil, err
		}
		peers = append(peers, p)
	}
	return peers, nil
}

// meshEndpoint is host:port for a peer: mesh.endpoint (with the listen port when it names none), else the
// host's address. "" when neither is usable (no address, or `auto`, which only the provider resolves).
func meshEndpoint(name, endpoint, address string, port int) (string, error) {
	e := strings.TrimSpace(endpoint)
	if e == "" {
		a := strings.TrimSpace(address)
		if a == "" || a == "auto" {
			return "", nil
		}
		if !meshHostRe.MatchString(a) {
			return "", fmt.Errorf("hosts.%s.address %q cannot be a mesh endpoint; set hosts.%s.mesh.endpoint", name, address, name)
		}
		return net.JoinHostPort(a, strconv.Itoa(port)), nil
	}
	host, p, err := net.SplitHostPort(e)
	if err != nil {
		host, p = e, strconv.Itoa(port)
	}
	if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 || !meshHostRe.MatchString(host) {
		return "", fmt.Errorf("hosts.%s.mesh.endpoint %q is not host or host:port", name, endpoint)
	}
	return net.JoinHostPort(host, p), nil
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	for i := p.Bits(); i < 32; i++ {
		b[i/8] |= 1 << (7 - uint(i%8))
	}
	return netip.AddrFrom4(b)
}

// ValidateMesh checks fleet.yml's `mesh:` when it is enabled (see MeshPeers). A disabled or absent one is
// not looked at, so a half-written section can wait under enabled: false.
func (f *FleetConfig) ValidateMesh() error {
	if !f.MeshEnabled() {
		return nil
	}
	_, err := f.MeshPeers()
	return err
}

// MeshMissing is what the mesh still lacks in fleet.yml before any host can bring its interface up: the
// hosts without a public key, and those without an endpoint. Every host needs both once mesh.enabled;
// they are reported by the plan rather than refused when fleet.yml is loaded, because a host's key only
// exists once its daemon has applied the mesh once (and generated it).
func MeshMissing(peers []MeshPeer) (noKey, noEndpoint []string) {
	for _, p := range peers {
		if p.PublicKey == "" {
			noKey = append(noKey, p.Host)
		}
		if p.Endpoint == "" {
			noEndpoint = append(noEndpoint, p.Host)
		}
	}
	return noKey, noEndpoint
}
