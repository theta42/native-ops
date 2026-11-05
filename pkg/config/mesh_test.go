package config

import (
	"strings"
	"testing"
)

// Two keys of the right shape (32 bytes, base64); WireGuard does not care what they are in a test.
const (
	meshKeyA = "YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWE="
	meshKeyB = "YmJiYmJiYmJiYmJiYmJiYmJiYmJiYmJiYmJiYmJiYmI="
)

const meshHosts = `name: f
default_host: h1
hosts:
  h1: {provider: static, address: 192.0.2.10}
  h2: {provider: static, address: 192.0.2.11, network: {ipv4_cidr: 10.0.101.0/24}}
  h0: {provider: static, address: auto, network: {ipv4_cidr: 10.0.102.0/24}, mesh: {endpoint: "vpn.example.com"}}
`

func TestMeshTunnelAddressesAreDerivedFromTheSortedHosts(t *testing.T) {
	cfg, err := LoadFleetConfig(writeFleet(t, meshHosts+"mesh: {enabled: true, subnet: 10.255.0.0/24}\n"))
	if err != nil {
		t.Fatal(err)
	}
	peers, err := cfg.MeshPeers()
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ host, addr, endpoint, bridge string }{
		{"h0", "10.255.0.1", "vpn.example.com:51820", "10.0.102.0/24"},
		{"h1", "10.255.0.2", "192.0.2.10:51820", "10.0.100.0/24"},
		{"h2", "10.255.0.3", "192.0.2.11:51820", "10.0.101.0/24"},
	}
	if len(peers) != len(want) {
		t.Fatalf("peers: %+v", peers)
	}
	for i, w := range want {
		p := peers[i]
		if p.Host != w.host || p.Address.String() != w.addr || p.Endpoint != w.endpoint || p.Bridge.String() != w.bridge {
			t.Fatalf("peer %d: %+v, want %+v", i, p, w)
		}
	}
	if cfg.Mesh.MeshInterface() != "wg0" || cfg.Mesh.Port() != 51820 {
		t.Fatalf("defaults: %s %d", cfg.Mesh.MeshInterface(), cfg.Mesh.Port())
	}
	// The same fleet.yml gives the same addresses, every time and on every host.
	again, _ := cfg.MeshPeers()
	for i := range peers {
		if again[i] != peers[i] {
			t.Fatal("tunnel addresses must be deterministic")
		}
	}
	// No keys yet: the plan reports them, fleet.yml still loads (a host's key exists only once its daemon made it).
	if noKey, noEndpoint := MeshMissing(peers); strings.Join(noKey, ",") != "h0,h1,h2" || len(noEndpoint) != 0 {
		t.Fatalf("missing: %v %v", noKey, noEndpoint)
	}
}

func TestMeshExplicitAddressesPortsAndEndpoints(t *testing.T) {
	body := `name: f
default_host: h1
hosts:
  h1: {provider: static, address: 192.0.2.10, mesh: {public_key: "` + meshKeyA + `", address: 10.255.0.10}}
  h2: {provider: static, address: auto, network: {ipv4_cidr: 10.0.101.0/24}, mesh: {public_key: "` + meshKeyB + `", endpoint: "198.51.100.7:6000"}}
mesh: {enabled: true, interface: mesh0, listen_port: 51999, subnet: 10.255.0.0/24}
`
	cfg, err := LoadFleetConfig(writeFleet(t, body))
	if err != nil {
		t.Fatal(err)
	}
	peers, _ := cfg.MeshPeers()
	if peers[0].Address.String() != "10.255.0.10" || peers[0].Endpoint != "192.0.2.10:51999" || peers[0].PublicKey != meshKeyA {
		t.Fatalf("h1: %+v", peers[0])
	}
	if peers[1].Address.String() != "10.255.0.2" || peers[1].Endpoint != "198.51.100.7:6000" {
		t.Fatalf("h2: %+v", peers[1])
	}
	if noKey, noEndpoint := MeshMissing(peers); len(noKey)+len(noEndpoint) != 0 {
		t.Fatalf("nothing is missing: %v %v", noKey, noEndpoint)
	}
	// h2 has address auto and no mesh.endpoint: it has no endpoint, which the plan reports.
	cfg.Hosts["h2"].Mesh.Endpoint = ""
	peers, _ = cfg.MeshPeers()
	if _, noEndpoint := MeshMissing(peers); strings.Join(noEndpoint, ",") != "h2" {
		t.Fatalf("no endpoint: %v", noEndpoint)
	}
}

func TestMeshValidation(t *testing.T) {
	for name, c := range map[string]struct{ body, want string }{
		"bad subnet":               {meshHosts + "mesh: {enabled: true, subnet: nope}\n", "mesh.subnet"},
		"subnet overlaps a bridge": {meshHosts + "mesh: {enabled: true, subnet: 10.0.0.0/16}\n", "overlaps host"},
		"address outside the subnet": {strings.Replace(meshHosts, "address: 192.0.2.10}", "address: 192.0.2.10, mesh: {address: 10.254.0.1}}", 1) +
			"mesh: {enabled: true, subnet: 10.255.0.0/24}\n", "not a usable address"},
		"the subnet's own address": {strings.Replace(meshHosts, "address: 192.0.2.10}", "address: 192.0.2.10, mesh: {address: 10.255.0.0}}", 1) +
			"mesh: {enabled: true, subnet: 10.255.0.0/24}\n", "not a usable address"},
		// h1 asks for h2's derived address.
		"duplicate address": {strings.Replace(meshHosts, "address: 192.0.2.10}", "address: 192.0.2.10, mesh: {address: 10.255.0.3}}", 1) +
			"mesh: {enabled: true, subnet: 10.255.0.0/24}\n", "same tunnel address"},
		"bad key": {strings.Replace(meshHosts, "address: 192.0.2.10}", "address: 192.0.2.10, mesh: {public_key: notakey}}", 1) +
			"mesh: {enabled: true, subnet: 10.255.0.0/24}\n", "not a WireGuard public key"},
		"bad interface":   {meshHosts + "mesh: {enabled: true, interface: \"wg0; reboot\", subnet: 10.255.0.0/24}\n", "mesh.interface"},
		"bad port":        {meshHosts + "mesh: {enabled: true, listen_port: 70000, subnet: 10.255.0.0/24}\n", "listen_port"},
		"bad endpoint":    {strings.Replace(meshHosts, "vpn.example.com", "vpn example", 1) + "mesh: {enabled: true, subnet: 10.255.0.0/24}\n", "mesh.endpoint"},
		"subnet too full": {meshHosts + "mesh: {enabled: true, subnet: 10.255.0.0/30}\n", "not a usable address"},
		"no hosts":        {"name: f\nmesh: {enabled: true, subnet: 10.255.0.0/24}\n", "no hosts"},
	} {
		_, err := LoadFleetConfig(writeFleet(t, c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error with %q, got %v", name, c.want, err)
		}
	}
	// A disabled mesh is not looked at, so a half-written one can wait.
	if _, err := LoadFleetConfig(writeFleet(t, meshHosts+"mesh: {enabled: false, subnet: nope}\n")); err != nil {
		t.Fatalf("a disabled mesh: %v", err)
	}
}
