package engine

import (
	"context"
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
