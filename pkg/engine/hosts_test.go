package engine

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/provider"
)

func hostsOf(hs ...*provider.Host) []*provider.Host { return hs }

func TestClassifyHost(t *testing.T) {
	active := &provider.Host{ID: "1", Name: "node-01", Status: "active", PublicIP: "1.2.3.4"}
	fresh := &provider.Host{ID: "2", Name: "node-01", Status: "new"}
	off := &provider.Host{ID: "3", Name: "node-01", Status: "off"}
	other := &provider.Host{ID: "4", Name: "other", Status: "active"}

	if h, a, err := classifyHost(hostsOf(other), "node-01"); err != nil || a != hostCreate || h != nil {
		t.Fatalf("no host of that name: create, got %v %v %v", h, a, err)
	}
	if h, a, err := classifyHost(hostsOf(other, active), "node-01"); err != nil || a != hostUse || h.ID != "1" {
		t.Fatalf("active host is used: %v %v %v", h, a, err)
	}
	// The bug this guards: a droplet that is still provisioning must be waited for, not duplicated.
	if h, a, err := classifyHost(hostsOf(fresh), "node-01"); err != nil || a != hostWait || h.ID != "2" {
		t.Fatalf("a provisioning host is waited for: %v %v %v", h, a, err)
	}
	if _, _, err := classifyHost(hostsOf(off), "node-01"); err == nil || !strings.Contains(err.Error(), "not creating a second one") {
		t.Fatalf("a stopped host must not be silently duplicated: %v", err)
	}
	if _, _, err := classifyHost(hostsOf(active, fresh), "node-01"); err == nil || !strings.Contains(err.Error(), "refusing to guess") {
		t.Fatalf("duplicates are an error, not a coin flip: %v", err)
	}
}

type fakeCompute struct {
	provider.ComputeProvider
	polls    int
	activeAt int
	err      error
}

func (f *fakeCompute) GetHost(_ context.Context, id string) (*provider.Host, error) {
	f.polls++
	if f.err != nil {
		return nil, f.err
	}
	if f.polls >= f.activeAt {
		return &provider.Host{ID: id, Status: "active", PublicIP: "9.9.9.9"}, nil
	}
	return &provider.Host{ID: id, Status: "new"}, nil
}

func TestWaitHostActive(t *testing.T) {
	f := &fakeCompute{activeAt: 3}
	h, err := waitHostActive(context.Background(), f, "7", time.Second, time.Millisecond)
	if err != nil || h.PublicIP != "9.9.9.9" || f.polls != 3 {
		t.Fatalf("got %v %v polls=%d", h, err, f.polls)
	}
	f = &fakeCompute{activeAt: 1 << 30}
	if _, err := waitHostActive(context.Background(), f, "7", 20*time.Millisecond, 5*time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not become active") {
		t.Fatalf("expected a timeout, got %v", err)
	}
	f = &fakeCompute{err: errors.New("api down")}
	if _, err := waitHostActive(context.Background(), f, "7", 20*time.Millisecond, 5*time.Millisecond); err == nil {
		t.Fatal("expected an error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := waitHostActive(ctx, &fakeCompute{activeAt: 1 << 30}, "7", time.Hour, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

type fakeDNS struct {
	provider.DNSProvider
	got []provider.DNSRecord
	err error
}

func (f *fakeDNS) SyncRecords(_ context.Context, _ string, r []provider.DNSRecord) error {
	f.got = r
	return f.err
}

func TestSyncDNSReportsFailures(t *testing.T) {
	f := &fakeDNS{}
	if err := syncDNS(context.Background(), f, "example.com", "1.2.3.4"); err != nil || len(f.got) != 2 || f.got[1].Name != "*" || f.got[0].Value != "1.2.3.4" {
		t.Fatalf("got %v %v", f.got, err)
	}
	if err := syncDNS(context.Background(), &fakeDNS{err: errors.New("403")}, "example.com", "1.2.3.4"); err == nil {
		t.Fatal("a DNS failure must be returned, not swallowed")
	}
}

func TestHostBootstrapCommandsAreGuarded(t *testing.T) {
	cmds, err := hostBootstrapCommands(config.NetworkConfig{BridgeName: config.DefaultBridgeName, IPv4CIDR: config.DefaultIPv4CIDR})
	if err != nil || len(cmds) == 0 {
		t.Fatal("no commands")
	}
	for _, c := range cmds {
		switch {
		case strings.Contains(c, "iptables") && regexp.MustCompile(`-[IA] `).MatchString(c) && !strings.Contains(c, "-C "):
			t.Errorf("an iptables rule is added without checking for it first (duplicates on every run): %s", c)
		case strings.Contains(c, "incus network set") && !strings.Contains(c, "incus network get"):
			t.Errorf("network setting applied unconditionally on every run: %s", c)
		case strings.Contains(c, "passwd -d") && !strings.Contains(c, "passwd -S"):
			t.Errorf("root password cleared without checking whether it is set: %s", c)
		case strings.Contains(c, "sysctl -w") && !strings.Contains(c, "sysctl -n"):
			t.Errorf("sysctl applied without checking the current value: %s", c)
		}
	}
	joined := strings.Join(cmds, "\n")
	for _, want := range []string{"-C FORWARD -i incusbr0", "-C FORWARD -o incusbr0", "-C INPUT -i incusbr0", "ipv4.address", "raw.dnsmasq"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected the bootstrap to cover %q", want)
		}
	}
}

var _ = config.HostSpec{}

// With the default network the bootstrap is what it was before hosts had their own bridge, word for word,
// but for one line: the bridge's address is now only set on a host with no instances, so a re-run of
// reconcile can never renumber a live bridge.
func TestTheDefaultBootstrapIsUnchangedButForTheAddressGuard(t *testing.T) {
	got, err := hostBootstrapCommands(config.NetworkConfig{BridgeName: config.DefaultBridgeName, IPv4CIDR: config.DefaultIPv4CIDR})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"chage -I -1 -m 0 -M 99999 -E -1 root || true",
		`[ "$(passwd -S root 2>/dev/null | awk '{print $2}')" = P ] && passwd -d root || true`,
		"which cloud-init >/dev/null 2>&1 && cloud-init status --wait || true",
		"which incus >/dev/null 2>&1 || (apt-get update && apt-get install -y incus)",
		"incus profile show default >/dev/null 2>&1 || incus admin init --auto",
		"incus storage list | grep -q default || incus storage create default dir || true",
		"incus profile device show default | grep -q 'path: /' || incus profile device add default root disk path=/ pool=default || true",
		`[ "$(sysctl -n net.ipv4.ip_forward 2>/dev/null)" = 1 ] || sysctl -w net.ipv4.ip_forward=1 || true`,
		"iptables -t nat -C POSTROUTING -s 10.0.100.0/24 -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -s 10.0.100.0/24 -j MASQUERADE || true",
		"iptables -C FORWARD -i incusbr0 -j ACCEPT 2>/dev/null || iptables -I FORWARD -i incusbr0 -j ACCEPT 2>/dev/null || true",
		"iptables -C FORWARD -o incusbr0 -j ACCEPT 2>/dev/null || iptables -I FORWARD -o incusbr0 -j ACCEPT 2>/dev/null || true",
		"iptables -C INPUT -i incusbr0 -j ACCEPT 2>/dev/null || iptables -I INPUT -i incusbr0 -j ACCEPT 2>/dev/null || true",
		"which ufw >/dev/null 2>&1 && (ufw allow in on incusbr0; ufw route allow in on incusbr0; ufw route allow out on incusbr0) || true",
		"incus network show incusbr0 >/dev/null 2>&1 || incus network create incusbr0 || true",
		// Before: [ "$(incus network get 'incusbr0' 'ipv4.address' 2>/dev/null)" = '10.0.100.1/24' ] || incus network set 'incusbr0' 'ipv4.address=10.0.100.1/24' || true
		`[ "$(incus network get 'incusbr0' 'ipv4.address' 2>/dev/null)" = '10.0.100.1/24' ] || { n="$(incus list --format csv --columns n 2>/dev/null)" && [ -z "$n" ] && incus network set 'incusbr0' 'ipv4.address=10.0.100.1/24'; } || true`,
		`[ "$(incus network get 'incusbr0' 'ipv4.nat' 2>/dev/null)" = 'true' ] || incus network set 'incusbr0' 'ipv4.nat=true' || true`,
		`[ "$(incus network get 'incusbr0' 'ipv6.address' 2>/dev/null)" = 'none' ] || incus network set 'incusbr0' 'ipv6.address=none' || true`,
		`[ "$(incus network get 'incusbr0' 'dns.mode' 2>/dev/null)" = 'managed' ] || incus network set 'incusbr0' 'dns.mode=managed' || true`,
		`[ "$(incus network get 'incusbr0' 'raw.dnsmasq' 2>/dev/null)" = 'server=1.1.1.1' ] || incus network set 'incusbr0' 'raw.dnsmasq=server=1.1.1.1' || true`,
		"incus profile device show default | grep -q 'network: incusbr0' || incus profile device add default eth0 nic network=incusbr0 name=eth0 || true",
		"incus remote list | grep -q ' docker ' || incus remote add docker https://docker.io --protocol=oci --public || true",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A second host bootstraps its own bridge and subnet, and nothing of the first host's.
func TestASecondHostBootstrapsItsOwnSubnet(t *testing.T) {
	fleet := &config.FleetConfig{
		Network: config.NetworkConfig{BridgeName: "incusbr0", IPv4CIDR: "10.0.100.0/24"},
		Hosts: map[string]config.HostConfig{
			"node-1": {Provider: "static"},
			"node-2": {Provider: "static", Network: &config.NetworkConfig{IPv4CIDR: "10.0.101.0/24"}},
			"node-3": {Provider: "static", Network: &config.NetworkConfig{BridgeName: "br-inst", IPv4CIDR: "10.0.102.0/24"}},
		},
	}
	cmds, err := hostBootstrapCommands(fleet.HostNetwork("node-2"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmds, "\n")
	for _, want := range []string{"-s 10.0.101.0/24 -j MASQUERADE", "'ipv4.address=10.0.101.1/24'", "-i incusbr0"} {
		if !strings.Contains(joined, want) {
			t.Errorf("node-2's bootstrap is missing %q", want)
		}
	}
	if strings.Contains(joined, "10.0.100.") {
		t.Errorf("node-2's bootstrap mentions the first host's subnet:\n%s", joined)
	}
	cmds, _ = hostBootstrapCommands(fleet.HostNetwork("node-3"))
	joined = strings.Join(cmds, "\n")
	if strings.Contains(joined, "incusbr0") || !strings.Contains(joined, "nic network=br-inst") || !strings.Contains(joined, "-o br-inst") {
		t.Errorf("node-3 uses its own bridge throughout:\n%s", joined)
	}
	if _, err := hostBootstrapCommands(config.NetworkConfig{BridgeName: "incusbr0", IPv4CIDR: "10.0.100.0"}); err == nil {
		t.Error("a bootstrap without a usable subnet is refused, not run with a guess")
	}
}
