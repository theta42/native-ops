package engine

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/provider"
)

// DNS follows the host: each daemon plans the records that point at its own host, and the default host
// also those tied to no service. Two daemons never plan the same record.

const dnsTwoHostFleet = "name: f\ndomain: example.com\ndns_provider: fake\ndefault_host: h1\n" +
	"hosts:\n  h1: {provider: static, address: 192.0.2.10}\n  h2: {provider: static, address: 192.0.2.11, network: {ipv4_cidr: 10.0.101.0/24}}\n" +
	"dns_records:\n" +
	"  - {type: TXT, name: \"@\", value: v=spf1 -all}\n" + // tied to nothing: h1's
	"  - {type: A, name: mail, service: db}\n" + // db is on h2: points at h2
	"  - {type: MX, name: \"@\", value: mail.example.com., priority: 10, service: db}\n" + // h2's, value kept
	"  - {type: A, name: git, service: web}\n" // web is on h1 (no host:)

var dnsTwoHostServices = map[string]string{
	"web": "name: web\nimage: gitea:latest\nrouting: {domain: web.example.com, upstream_port: 80}\n",
	"db":  "name: db\nimage: postgres:16\nhost: h2\n",
	"app": "name: app\nimage: app:latest\nhost: h2\nrouting: {domain: app.example.com, upstream_port: 80}\n",
	"ext": "name: ext\nimage: app:latest\nhost: h2\nrouting: {domain: \"*.example.com\", upstream_port: 80, external: true}\n",
}

// planDNSOf plans host's share and returns the DNS changes it would make ("TYPE name value").
func planDNSOf(t *testing.T, dir, host string, dns *zoneStore) (*FleetPlan, []string) {
	t.Helper()
	fp, err := PlanFleet(context.Background(), hostSimFor(t, host), dir, "", WithBindKey([]byte("k")), WithHostName(host), WithDNS(dns.fn(), nil))
	if err != nil {
		t.Fatalf("plan %s: %v", host, err)
	}
	if fp.Network == nil || len(fp.Network.Blockers) > 0 {
		t.Fatalf("plan %s:\n%s", host, fp.Render())
	}
	var out []string
	for _, c := range fp.Network.Changes {
		if strings.HasPrefix(c.Kind, "dns-") {
			out = append(out, c.Detail)
		}
	}
	sort.Strings(out)
	return fp, out
}

func TestEachDaemonPlansTheDNSThatPointsAtItsHost(t *testing.T) {
	dir := shareTree(t, dnsTwoHostFleet, dnsTwoHostServices)
	dns := &zoneStore{zones: map[string][]provider.DNSRecord{}}

	p1, h1 := planDNSOf(t, dir, "h1", dns)
	p2, h2 := planDNSOf(t, dir, "h2", dns)
	want1 := []string{"A git.example.com 192.0.2.10", "TXT example.com v=spf1 -all"}
	want2 := []string{"A app.example.com 192.0.2.11", "A mail.example.com 192.0.2.11", "MX example.com mail.example.com. priority 10"}
	if strings.Join(h1, "|") != strings.Join(want1, "|") {
		t.Errorf("h1 plans %q, want %q", h1, want1)
	}
	if strings.Join(h2, "|") != strings.Join(want2, "|") {
		t.Errorf("h2 plans %q, want %q", h2, want2)
	}

	// No record is planned by both: the same type and name never appears in both shares.
	seen := map[string]string{}
	for host, recs := range map[string][]string{"h1": h1, "h2": h2} {
		for _, r := range recs {
			f := strings.Fields(r)
			key := f[0] + " " + f[1]
			if other, ok := seen[key]; ok {
				t.Fatalf("%s is planned by %s and %s", key, other, host)
			}
			seen[key] = host
		}
	}

	// Each says which host does the others.
	n1, n2 := strings.Join(p1.Network.Notes, "\n"), strings.Join(p2.Network.Notes, "\n")
	if !strings.Contains(n1, "host h2's daemon plans and applies A mail.example.com, MX example.com, A app.example.com") {
		t.Errorf("h1 should name h2's records:\n%s", p1.Render())
	}
	if !strings.Contains(n2, "host h1's daemon plans and applies TXT example.com, A git.example.com") {
		t.Errorf("h2 should name h1's records:\n%s", p2.Render())
	}
}

// Plan and apply on each host, then nothing is left on either; and neither daemon writes the other's.
func TestPerHostDNSIsAppliedAndThenSettled(t *testing.T) {
	dir := shareTree(t, dnsTwoHostFleet, dnsTwoHostServices)
	dns := &zoneStore{zones: map[string][]provider.DNSRecord{}}
	for _, host := range []string{"h2", "h1"} {
		fp, _ := planDNSOf(t, dir, host, dns)
		before := dns.writes
		sim := hostSimFor(t, host)
		d := newTestDeployer(sim)
		d.SetDNS(dns.fn(), nil)
		if err := d.ApplyPlan(context.Background(), dir, fp); err != nil {
			t.Fatalf("apply %s: %v", host, err)
		}
		if host == "h2" && dns.writes-before != 3 {
			t.Fatalf("h2 writes its 3 records, wrote %d: %+v", dns.writes-before, dns.zones)
		}
		if host == "h1" && dns.writes-before != 2 {
			t.Fatalf("h1 writes its 2 records, wrote %d: %+v", dns.writes-before, dns.zones)
		}
	}
	for _, host := range []string{"h1", "h2"} {
		if _, again := planDNSOf(t, dir, host, dns); len(again) != 0 {
			t.Fatalf("%s: once applied, nothing is left to do: %q", host, again)
		}
	}
	if len(dns.zones["example.com"]) != 5 {
		t.Fatalf("the zone holds both hosts' records: %+v", dns.zones)
	}
}

// A record that no single host can own blocks the plan (and validate refuses it).
func TestDNSNoSingleHostCanOwnIsRefused(t *testing.T) {
	cases := map[string]struct{ fleet, want string }{
		"a wildcard tied to a service": {"  - {type: A, name: \"*.db\", service: db}\n", "cannot be a wildcard"},
		"two hosts claim one name":     {"  - {type: A, name: x, service: db}\n  - {type: A, name: x, service: web}\n", "claimed by host h2"},
		"the apex off the default":     {"  - {type: A, name: \"@\", service: db}\n", "claimed by host h1 (reconcile, for domain:) and host h2"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fleet := strings.SplitAfter(dnsTwoHostFleet, "dns_records:\n")[0] + c.fleet
			dir := shareTree(t, fleet, map[string]string{"web": dnsTwoHostServices["web"], "db": dnsTwoHostServices["db"]})
			for _, host := range []string{"h1", "h2"} {
				fp, err := PlanFleet(context.Background(), hostSimFor(t, host), dir, "", WithHostName(host))
				if err != nil {
					t.Fatal(err)
				}
				if !fp.Blocked() || !strings.Contains(strings.Join(fp.Network.Blockers, "\n"), c.want) {
					t.Fatalf("%s: the plan must be blocked with %q:\n%s", host, c.want, fp.Render())
				}
			}
			if _, err := NewReconciler(dir, nil).Validate(context.Background()); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("validate must refuse it with %q: %v", c.want, err)
			}
		})
	}
	// A wildcard route on a host other than the default one.
	dir := shareTree(t, strings.SplitAfter(dnsTwoHostFleet, "dns_records:\n")[0]+"  - {type: TXT, name: t, value: x}\n",
		map[string]string{"w": "name: w\nimage: app:latest\nhost: h2\nrouting: {domain: \"*.apps.example.com\", upstream_port: 80}\n"})
	if _, err := NewReconciler(dir, nil).Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "wildcard *.apps.example.com") {
		t.Fatalf("a wildcard route off the default host must be refused: %v", err)
	}
}

// validate reports each host's share.
func TestValidateReportsEachHostsShare(t *testing.T) {
	dir := shareTree(t, dnsTwoHostFleet, dnsTwoHostServices)
	s, err := NewReconciler(dir, nil).Validate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.PerHost) != 2 || s.PerHost[0].Name != "h1" || !s.PerHost[0].Default || s.PerHost[1].Name != "h2" {
		t.Fatalf("got %+v", s.PerHost)
	}
	if got := strings.Join(s.PerHost[1].Services, ","); got != "app,db,ext" {
		t.Errorf("h2's services: %s", got)
	}
	if got := strings.Join(s.PerHost[1].DNS, "|"); got != "A mail.example.com 192.0.2.11|MX example.com mail.example.com.|A app.example.com 192.0.2.11" {
		t.Errorf("h2's DNS: %s", got)
	}
	if got := strings.Join(s.PerHost[0].DNS, "|"); got != "TXT example.com v=spf1 -all|A git.example.com 192.0.2.10" {
		t.Errorf("h1's DNS: %s", got)
	}
}
