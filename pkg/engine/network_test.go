package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/provider"
)

// The fleet's network (DNS records, the edge's Caddyfile, the bridge) is part of the plan: shown, in the
// hash, applied by apply exactly as planned, and with nothing left to do once applied.

type zoneStore struct {
	zones  map[string][]provider.DNSRecord
	writes int
	next   int
}

func (f *zoneStore) Name() string { return "fake" }
func (f *zoneStore) ListRecords(_ context.Context, zone string) ([]provider.DNSRecord, error) {
	return append([]provider.DNSRecord(nil), f.zones[zone]...), nil
}
func (f *zoneStore) DeleteRecord(context.Context, string, string) error {
	panic("a sync never deletes")
}
func (f *zoneStore) SyncRecords(_ context.Context, zone string, desired []provider.DNSRecord) error {
	for _, c := range provider.PlanDNSSync(f.zones[zone], desired) {
		f.writes++
		if c.Update {
			for i, r := range f.zones[zone] {
				if r.ID == c.Record.ID {
					f.zones[zone][i] = c.Record
				}
			}
			continue
		}
		f.next++
		c.Record.ID = fmt.Sprint(f.next)
		f.zones[zone] = append(f.zones[zone], c.Record)
	}
	return nil
}

func (f *zoneStore) fn() DNSFunc {
	return func(*config.FleetConfig) (provider.DNSProvider, error) { return f, nil }
}

const mailRecords = `dns_records:
  - zone: example.com
    type: A
    name: inbound
    value: "203.0.113.7"
  - zone: example.com
    type: MX
    name: inbound
    value: inbound.example.com.
    priority: 10
`

// networkTree writes a config tree with no services: fleet.yml (with extra appended) and files.
func networkTree(t *testing.T, extra string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("fleet.yml", "name: test\ndns_provider: fake\n"+extra)
	write("services/.keep", "")
	for rel, body := range files {
		write(rel, body)
	}
	return dir
}

func planNet(t *testing.T, sim *hostSim, dir string, opts ...PlanOption) *FleetPlan {
	t.Helper()
	fp, err := PlanFleet(context.Background(), sim, dir, "", append([]PlanOption{WithBindKey([]byte("k"))}, opts...)...)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return fp
}

func applyNet(t *testing.T, sim *hostSim, dir string, fp *FleetPlan, dns *zoneStore) error {
	t.Helper()
	d := newTestDeployer(sim)
	if dns != nil {
		d.SetDNS(dns.fn(), nil)
	}
	return d.ApplyPlan(context.Background(), dir, fp)
}

func networkDetails(fp *FleetPlan) string {
	if fp.Network == nil {
		return ""
	}
	var parts []string
	for _, c := range fp.Network.Changes {
		parts = append(parts, c.Kind+" "+c.Detail)
	}
	return strings.Join(parts, "\n")
}

func TestDNSRecordsArePlannedAppliedAndThenSettled(t *testing.T) {
	sim := newHostSim(t)
	dns := &zoneStore{zones: map[string][]provider.DNSRecord{"example.com": {
		{ID: "1", Type: "A", Name: "inbound", Value: "198.51.100.1"},
		{ID: "2", Type: "TXT", Name: "@", Value: "v=spf1 -all"}, // not in the manifest: left alone
	}}}
	dir := networkTree(t, mailRecords, nil)

	fp := planNet(t, sim, dir, WithDNS(dns.fn(), nil))
	got := networkDetails(fp)
	for _, want := range []string{"dns-update A inbound.example.com 203.0.113.7", "dns-create MX inbound.example.com inbound.example.com. priority 10"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the plan should say %q:\n%s", want, fp.Render())
		}
	}
	if !fp.Pending() || fp.ExitStatus() != 2 || !strings.Contains(fp.Render(), "~ network (update)") {
		t.Fatalf("a DNS-only change is a pending change:\n%s", fp.Render())
	}
	if dns.writes != 0 {
		t.Fatal("planning wrote DNS")
	}

	if err := applyNet(t, sim, dir, fp, dns); err != nil {
		t.Fatal(err)
	}
	if dns.writes != 2 || len(dns.zones["example.com"]) != 3 {
		t.Fatalf("apply should update one record and create one, kept the TXT: %+v", dns.zones)
	}
	again := planNet(t, sim, dir, WithDNS(dns.fn(), nil))
	if again.Pending() || networkDetails(again) != "" {
		t.Fatalf("once applied, nothing is left to do:\n%s", again.Render())
	}
}

// Somebody edits the zone between plan and apply: apply refuses instead of doing what nobody reviewed.
func TestApplyRefusesDNSThatChangedSinceThePlan(t *testing.T) {
	sim := newHostSim(t)
	dns := &zoneStore{zones: map[string][]provider.DNSRecord{"example.com": {{ID: "1", Type: "A", Name: "inbound", Value: "198.51.100.1"}}}}
	dir := networkTree(t, mailRecords, nil)
	fp := planNet(t, sim, dir, WithDNS(dns.fn(), nil))

	dns.zones["example.com"] = append(dns.zones["example.com"], provider.DNSRecord{ID: "9", Type: "MX", Name: "inbound", Value: "inbound.example.com.", Priority: 10})
	err := applyNet(t, sim, dir, fp, dns)
	if err == nil || !strings.Contains(err.Error(), "changed since the plan") {
		t.Fatalf("apply must refuse a zone that moved since the plan: %v", err)
	}
	if dns.writes != 0 {
		t.Fatal("nothing may be written when the plan is stale")
	}
}

func TestDNSThatCannotBePlannedBlocks(t *testing.T) {
	sim := newHostSim(t)
	dir := networkTree(t, mailRecords, nil)

	noToken := func(*config.FleetConfig) (provider.DNSProvider, error) {
		return nil, fmt.Errorf("DO_API_TOKEN is not in the daemon's secret store")
	}
	if fp := planNet(t, sim, dir, WithDNS(noToken, nil)); !fp.Blocked() || !strings.Contains(fp.Render(), "DO_API_TOKEN") {
		t.Fatalf("a provider that cannot be reached blocks the plan, naming why:\n%s", fp.Render())
	}
	dns := &zoneStore{zones: map[string][]provider.DNSRecord{}}
	if fp := planNet(t, sim, dir, WithDNS(dns.fn(), []string{"example.net"})); !fp.Blocked() || !strings.Contains(fp.Render(), "outside the zones") {
		t.Fatalf("a zone outside the daemon's limit blocks the plan:\n%s", fp.Render())
	}
	twice := networkTree(t, "domain: example.com\ndns_records:\n  - zone: example.com\n    type: A\n    name: \"*\"\n    value: 203.0.113.9\n", nil)
	if fp := planNet(t, sim, twice, WithDNS(dns.fn(), nil)); !fp.Blocked() || !strings.Contains(fp.Render(), "declare it in one place") {
		t.Fatalf("a record reconcile also writes blocks the plan:\n%s", fp.Render())
	}
	// Without a provider (a local plan) the records are named as not planned here, not silently skipped.
	if fp := planNet(t, sim, dir); fp.Blocked() || !strings.Contains(fp.Render(), "dns_records are not planned here") {
		t.Fatalf("a plan with no DNS provider should say so:\n%s", fp.Render())
	}
}

func TestTheEdgeCaddyfileIsPlannedAppliedAndThenSettled(t *testing.T) {
	sim := newHostSim(t)
	body := "{\n\temail ops@example.com\n}\nimport /etc/caddy/sites/*.caddy\n"
	dir := networkTree(t, "", map[string]string{"edge/Caddyfile": body})

	fp := planNet(t, sim, dir)
	if got := networkDetails(fp); !strings.Contains(got, ChangeEdgeCaddyfile+" edge/Caddyfile: 3 lines added, 0 removed") || !fp.Pending() {
		t.Fatalf("the plan should show the Caddyfile change:\n%s", fp.Render())
	}
	if err := applyNet(t, sim, dir, fp, nil); err != nil {
		t.Fatal(err)
	}
	if sim.ctrs["edge"].files["/etc/caddy/Caddyfile"] != body || sim.ctrs["edge"].reloads == 0 {
		t.Fatalf("apply should write and reload the Caddyfile: %q", sim.ctrs["edge"].files["/etc/caddy/Caddyfile"])
	}
	if again := planNet(t, sim, dir); again.Pending() {
		t.Fatalf("once applied, nothing is left to do:\n%s", again.Render())
	}
}

// Two Caddyfiles whose plans read the same (the same line counts) have different hashes: approving one
// cannot apply the other.
func TestTheCaddyfileContentIsInTheHash(t *testing.T) {
	sim := newHostSim(t)
	a := planNet(t, sim, networkTree(t, "", map[string]string{"edge/Caddyfile": "import /etc/caddy/sites/*.caddy\na.example.com {\n}\n"}))
	b := planNet(t, sim, networkTree(t, "", map[string]string{"edge/Caddyfile": "import /etc/caddy/sites/*.caddy\nb.example.com {\n}\n"}))
	if a.Render() != b.Render() {
		t.Fatalf("test setup: the plans should read the same:\n%s\n%s", a.Render(), b.Render())
	}
	if a.Hash() == b.Hash() {
		t.Fatal("the Caddyfile's content must be in the hash")
	}
}

func TestACaddyfileThatWouldNotServeSitesBlocks(t *testing.T) {
	sim := newHostSim(t)
	fp := planNet(t, sim, networkTree(t, "", map[string]string{"edge/Caddyfile": "example.com {\n}\n"}))
	if !fp.Blocked() || !strings.Contains(fp.Render(), "does not import") {
		t.Fatalf("a Caddyfile without the sites import blocks the plan:\n%s", fp.Render())
	}
}

func TestABridgeThatDiffersFromFleetYmlBlocks(t *testing.T) {
	sim := newHostSim(t)
	fp := planNet(t, sim, networkTree(t, "network:\n  ipv4_cidr: 10.0.200.0/24\n", nil))
	if !fp.Blocked() || !strings.Contains(fp.Render(), "does not re-address a live bridge") {
		t.Fatalf("a bridge that differs from fleet.yml blocks the plan:\n%s", fp.Render())
	}
	if ok := planNet(t, sim, networkTree(t, "", nil)); ok.Blocked() || ok.Pending() {
		t.Fatalf("a matching bridge is not a change:\n%s", ok.Render())
	}
}

// A plan narrowed to one service leaves the shared network out, and so does its hash.
func TestAOneServicePlanHasNoNetworkPart(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["gitea:latest"] = fpA
	dir := networkTree(t, mailRecords, map[string]string{
		"services/gitea/service.yml": "name: gitea\nimage: gitea:latest\n",
	})
	dns := &zoneStore{zones: map[string][]provider.DNSRecord{}}
	fp, err := PlanFleet(context.Background(), sim, dir, "gitea", WithDNS(dns.fn(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if fp.Network != nil {
		t.Fatalf("a one-service plan should not plan the network: %+v", fp.Network)
	}
}

// With one host and no override, the plan reads exactly as it did before hosts could have their own
// bridge: the same text, the same JSON and the same hash, whether the bridge matches or not.
func TestASingleHostPlanIsUnchangedByPerHostBridges(t *testing.T) {
	sim := newHostSim(t)
	cases := []struct{ extra, render, json, hash string }{{
		extra:  "hosts:\n  node-1:\n    provider: static\n    address: 192.0.2.10\n",
		render: "\nPlan: 0 to create, 0 to update, 0 unchanged, 0 blocked.\n",
		json:   `{"services":[],"network":{}}`,
		hash:   "9c9d33c96fa391ddb390f54b4cffac3c01a689f35690ac323a3eb67c9105dd1d",
	}, {
		extra:  "network:\n  ipv4_cidr: 10.0.200.0/24\nhosts:\n  node-1:\n    provider: static\n",
		render: "! network (blocked)\n    BLOCKED: the bridge incusbr0 has ipv4.address 10.0.100.1/24, but fleet.yml says 10.0.200.0/24; the daemon does not re-address a live bridge\n\nPlan: 0 to create, 0 to update, 0 unchanged, 0 blocked; the network is blocked.\n",
		json:   `{"services":[],"network":{"blockers":["the bridge incusbr0 has ipv4.address 10.0.100.1/24, but fleet.yml says 10.0.200.0/24; the daemon does not re-address a live bridge"]}}`,
		hash:   "fc54022290942517dbe957bfe8a1b48742998138844852de49e9f372d37d4f79",
	}}
	for _, c := range cases {
		fp := planNet(t, sim, networkTree(t, c.extra, nil))
		j, _ := json.Marshal(fp)
		if fp.Render() != c.render || string(j) != c.json || fp.Hash() != c.hash {
			t.Errorf("%s: the plan changed:\n%s\n%s\n%s", c.extra, fp.Render(), j, fp.Hash())
		}
	}
}

// A host's own subnet is what its bridge is checked against; a live bridge that differs is reported and
// left alone, never renumbered.
func TestTheBridgeIsCheckedAgainstTheHostsOwnSubnet(t *testing.T) {
	sim := newHostSim(t)
	sim.bridges = map[string]string{"incusbr0": "10.0.101.1/24", "br-inst": "10.0.102.1/24"}
	own := "hosts:\n  node-2:\n    provider: static\n    network:\n      ipv4_cidr: 10.0.101.0/24\n"
	if fp := planNet(t, sim, networkTree(t, own, nil)); fp.Blocked() || fp.Pending() {
		t.Fatalf("the host's bridge matches its own subnet:\n%s", fp.Render())
	}
	named := "hosts:\n  node-3:\n    provider: static\n    network:\n      bridge_name: br-inst\n      ipv4_cidr: 10.0.102.0/24\n"
	if fp := planNet(t, sim, networkTree(t, named, nil)); fp.Blocked() || fp.Pending() {
		t.Fatalf("the host's own bridge name is the one checked:\n%s", fp.Render())
	}
	// The fleet's default subnet does not fit this host's live bridge: reported, and nothing to apply.
	fp := planNet(t, sim, networkTree(t, "hosts:\n  node-2:\n    provider: static\n", nil))
	if !fp.Blocked() || !strings.Contains(fp.Render(), "the bridge incusbr0 has ipv4.address 10.0.101.1/24, but fleet.yml says 10.0.100.0/24; the daemon does not re-address a live bridge") {
		t.Fatalf("a live bridge that differs is reported:\n%s", fp.Render())
	}
	if fp.Network != nil && len(fp.Network.Changes) > 0 {
		t.Fatalf("a differing bridge is never a change to apply: %v", fp.Network.Changes)
	}
	for _, c := range sim.cmds {
		if strings.Contains(c, "network set") {
			t.Fatalf("a plan never sets the bridge: %s", c)
		}
	}
}

// With several hosts the daemon cannot tell which one it is on, so it says it did not check the bridge
// rather than checking it against another host's subnet.
// With several hosts each daemon checks the bridge of the host it is (--host-name), against that host's own
// subnet: never the fleet's, never another host's.
func TestEachHostChecksItsOwnBridge(t *testing.T) {
	two := "default_host: node-1\nhosts:\n  node-1:\n    provider: static\n  node-2:\n    provider: static\n    network:\n      ipv4_cidr: 10.0.101.0/24\n"
	dir := networkTree(t, two, nil)
	node1, node2 := newHostSim(t), newHostSim(t)
	node2.bridges = map[string]string{"incusbr0": "10.0.101.1/24"}
	for host, sim := range map[string]*hostSim{"node-1": node1, "node-2": node2} {
		fp := planNet(t, sim, dir, WithHostName(host))
		if fp.Blocked() || fp.Pending() || strings.Contains(fp.Render(), "does not know which one it runs on") {
			t.Fatalf("%s on its own bridge: nothing to do, and checked:\n%s", host, fp.Render())
		}
	}
	// Each host's daemon on the other's bridge is blocked, naming the subnet fleet.yml gives its own host.
	for host, c := range map[string]struct {
		sim  *hostSim
		want string
	}{"node-1": {node2, "fleet.yml says 10.0.100.0/24"}, "node-2": {node1, "fleet.yml says 10.0.101.0/24"}} {
		fp := planNet(t, c.sim, dir, WithHostName(host))
		if !fp.Blocked() || !strings.Contains(fp.Render(), c.want) {
			t.Fatalf("%s on the other host's bridge must be blocked (%q):\n%s", host, c.want, fp.Render())
		}
	}
	// Without a host name the daemon cannot tell, and refuses the plan (it never checks the wrong subnet).
	if _, err := PlanFleet(context.Background(), node2, dir, ""); !errors.Is(err, config.ErrHostNameRequired) {
		t.Fatalf("no host name: %v", err)
	}
}

// Apply on the second host of two launches with that host's bridge.
func TestApplyOnOneOfTwoHostsLaunchesWithItsOwnBridge(t *testing.T) {
	sim := newHostSim(t)
	sim.bridges = map[string]string{"incusbr0": "10.0.101.1/24"}
	sim.aliases["web:latest"] = fpA
	dir := networkTree(t, "default_host: node-1\nhosts:\n  node-1:\n    provider: static\n  node-2:\n    provider: static\n    network:\n      ipv4_cidr: 10.0.101.0/24\n",
		map[string]string{"services/web/service.yml": "name: web\nimage: web:latest\nhost: node-2\nprofiles: [default, service]\n"})
	fp := planNet(t, sim, dir, WithHostName("node-2"))
	if fp.Blocked() || !fp.Pending() || fp.Host != "node-2" {
		t.Fatalf("got:\n%s", fp.Render())
	}
	if err := applyNet(t, sim, dir, fp, nil); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(sim.cmds, "\n")
	if !strings.Contains(joined, "'ipv4.address=10.0.101.1/24'") || strings.Contains(joined, "10.0.100.1/24") {
		t.Fatalf("the launch must make sure of node-2's bridge only:\n%s", joined)
	}
}

// An apply launches with the host's own bridge: a host on 10.0.101.0/24 never hears of 10.0.100.1/24.
func TestApplyLaunchesWithTheHostsOwnBridge(t *testing.T) {
	sim := newHostSim(t)
	sim.bridges = map[string]string{"incusbr0": "10.0.101.1/24"}
	sim.aliases["web:latest"] = fpA
	dir := networkTree(t, "hosts:\n  node-2:\n    provider: static\n    network:\n      ipv4_cidr: 10.0.101.0/24\n",
		map[string]string{"services/web/service.yml": "name: web\nimage: web:latest\nprofiles: [default, service]\n"})
	fp := planNet(t, sim, dir)
	if fp.Blocked() || !fp.Pending() {
		t.Fatalf("got:\n%s", fp.Render())
	}
	if err := applyNet(t, sim, dir, fp, nil); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(sim.cmds, "\n")
	if !strings.Contains(joined, "'ipv4.address=10.0.101.1/24'") || strings.Contains(joined, "10.0.100.1/24") {
		t.Fatalf("the launch must make sure of this host's bridge only:\n%s", joined)
	}
}
