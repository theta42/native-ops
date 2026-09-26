package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/provider"
)

type hostAction int

const (
	hostCreate hostAction = iota // no host of that name exists
	hostUse                      // an active host exists
	hostWait                     // a host exists but is still provisioning
)

// classifyHost decides what reconcile does about a named host, looking at
// every host of that name whatever its status. Matching only "active" hosts made
// an interrupted run (a droplet still provisioning when the job died) create a
// duplicate on the next run.
func classifyHost(hosts []*provider.Host, name string) (*provider.Host, hostAction, error) {
	var matches []*provider.Host
	for _, h := range hosts {
		if h.Name == name {
			matches = append(matches, h)
		}
	}
	switch len(matches) {
	case 0:
		return nil, hostCreate, nil
	case 1:
		h := matches[0]
		switch h.Status {
		case "active":
			return h, hostUse, nil
		case "new", "provisioning":
			return h, hostWait, nil
		default:
			return nil, hostCreate, fmt.Errorf("host %s exists (id %s) with status %q; not creating a second one — start it or remove it yourself", name, h.ID, h.Status)
		}
	default:
		return nil, hostCreate, fmt.Errorf("%d hosts are named %s (ids: %s); refusing to guess which one to use — remove the extras", len(matches), name, hostIDs(matches))
	}
}

func hostIDs(hs []*provider.Host) string {
	s := ""
	for i, h := range hs {
		if i > 0 {
			s += ", "
		}
		s += h.ID
	}
	return s
}

// waitHostActive polls a provisioning host until it is active with a public address.
func waitHostActive(ctx context.Context, p provider.ComputeProvider, id string, timeout, interval time.Duration) (*provider.Host, error) {
	deadline := time.Now().Add(timeout)
	for {
		h, err := p.GetHost(ctx, id)
		if err == nil && h.Status == "active" && h.PublicIP != "" {
			return h, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("host %s did not become active within %s", id, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// syncDNS upserts the records the fleet needs.
func syncDNS(ctx context.Context, p provider.DNSProvider, domain, ip string) error {
	return p.SyncRecords(ctx, domain, []provider.DNSRecord{
		{Type: "A", Name: "@", Value: ip},
		{Type: "A", Name: "*", Value: ip},
	})
}

// hostBootstrapCommands prepares an Incus host's runtime. Every command is
// safe to run repeatedly and changes nothing when the host is already set up:
// each is guarded by a check of the current state (or is naturally idempotent),
// so re-running reconcile does not stack duplicate firewall rules or re-apply
// unchanged network settings. Failures are tolerated (`|| true`) because the
// scoped CI user on an adopted host has no sudo and simply cannot apply them.
func hostBootstrapCommands() []string {
	const bridge = "incusbr0"
	iptablesOnce := func(table, chain, rule string) string {
		t := ""
		if table != "" {
			t = "-t " + table + " "
		}
		return fmt.Sprintf("iptables %s-C %s %s 2>/dev/null || iptables %s-I %s %s 2>/dev/null || true", t, chain, rule, t, chain, rule)
	}
	return []string{
		"chage -I -1 -m 0 -M 99999 -E -1 root || true",
		// Only clear the root password if it is still set (status P); never touch a locked or already-empty one.
		`[ "$(passwd -S root 2>/dev/null | awk '{print $2}')" = P ] && passwd -d root || true`,
		"which cloud-init >/dev/null 2>&1 && cloud-init status --wait || true",
		"which incus >/dev/null 2>&1 || (apt-get update && apt-get install -y incus)",
		"incus profile show default >/dev/null 2>&1 || incus admin init --auto",
		"incus storage list | grep -q default || incus storage create default dir || true",
		"incus profile device show default | grep -q 'path: /' || incus profile device add default root disk path=/ pool=default || true",
		`[ "$(sysctl -n net.ipv4.ip_forward 2>/dev/null)" = 1 ] || sysctl -w net.ipv4.ip_forward=1 || true`,
		"iptables -t nat -C POSTROUTING -s 10.0.100.0/24 -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -s 10.0.100.0/24 -j MASQUERADE || true",
		iptablesOnce("", "FORWARD", "-i "+bridge+" -j ACCEPT"),
		iptablesOnce("", "FORWARD", "-o "+bridge+" -j ACCEPT"),
		iptablesOnce("", "INPUT", "-i "+bridge+" -j ACCEPT"),
		"which ufw >/dev/null 2>&1 && (ufw allow in on " + bridge + "; ufw route allow in on " + bridge + "; ufw route allow out on " + bridge + ") || true",
		"incus network show " + bridge + " >/dev/null 2>&1 || incus network create " + bridge + " || true",
		incus.NetworkSetIfChanged(bridge, "ipv4.address", "10.0.100.1/24"),
		incus.NetworkSetIfChanged(bridge, "ipv4.nat", "true"),
		incus.NetworkSetIfChanged(bridge, "ipv6.address", "none"),
		incus.NetworkSetIfChanged(bridge, "dns.mode", "managed"),
		incus.NetworkSetIfChanged(bridge, "raw.dnsmasq", "server=1.1.1.1"),
		"incus profile device show default | grep -q 'network: " + bridge + "' || incus profile device add default eth0 nic network=" + bridge + " name=eth0 || true",
		"incus remote list | grep -q ' docker ' || incus remote add docker https://docker.io --protocol=oci --public || true",
	}
}
