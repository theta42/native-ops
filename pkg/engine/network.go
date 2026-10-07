package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/provider"
)

// Kinds of network change a plan reports.
const (
	ChangeDNSCreate     = "dns-create"
	ChangeDNSUpdate     = "dns-update"
	ChangeEdgeCaddyfile = "edge-caddyfile"
	ChangePublishPort   = "publish-port"
	ChangeRemovePort    = "remove-port"
)

// NetworkPlan is what apply would do to the fleet's shared network: the DNS records fleet.yml declares
// and the edge's main Caddyfile (edge/Caddyfile). It also checks the instance bridge against fleet.yml's
// `network:`. Like a service plan it is worked out without changing anything, it is in the plan's hash,
// and apply does exactly what it says (or refuses, when the world moved since the plan).
type NetworkPlan struct {
	Changes  []Change `json:"changes,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	Blockers []string `json:"blockers,omitempty"`

	// edgeCaddyfile is the repo's edge/Caddyfile when the plan applies it.
	edgeCaddyfile string
	// zones are the DNS zones apply syncs, with the records each one must contain.
	zones map[string][]provider.DNSRecord
	// planned is each zone's changes as dnsDetails renders them: apply checks the zone still needs exactly these.
	planned map[string]string
	// bind digests what the plan does not print (the Caddyfile's content); it only goes into the hash.
	bind string
}

func (n *NetworkPlan) add(kind, format string, a ...any) {
	n.Changes = append(n.Changes, Change{Kind: kind, Detail: fmt.Sprintf(format, a...)})
}
func (n *NetworkPlan) note(format string, a ...any) {
	n.Notes = append(n.Notes, fmt.Sprintf(format, a...))
}
func (n *NetworkPlan) block(format string, a ...any) {
	n.Blockers = append(n.Blockers, fmt.Sprintf(format, a...))
}

// DNSFunc gives the DNS provider for a fleet's dns_provider. An error says why records cannot be planned
// or applied (an unknown provider, a missing API token); it blocks the plan.
type DNSFunc func(fleet *config.FleetConfig) (provider.DNSProvider, error)

// WithDNS makes the plan (and apply) read and write fleet.yml's dns_records through dns. zoneLimit, when
// not empty, is the only zones a record may name; a record outside it blocks the plan.
func WithDNS(dns DNSFunc, zoneLimit []string) PlanOption {
	return func(d *Deployer) { d.SetDNS(dns, zoneLimit) }
}

// SetDNS is WithDNS for a deployer that applies.
func (d *Deployer) SetDNS(dns DNSFunc, zoneLimit []string) {
	d.dns, d.dnsZoneLimit = dns, zoneLimit
}

// PlanNetwork works out what apply would do to the fleet's network. services are the manifests being
// planned, so an edge created in the same plan is known to get its Caddyfile when it is created.
func (d *Deployer) PlanNetwork(ctx context.Context, fleet *config.FleetConfig, configDir string, services []*config.ServiceConfig) (*NetworkPlan, error) {
	n := &NetworkPlan{}
	if err := d.planBridge(ctx, n, fleet); err != nil {
		return nil, err
	}
	if err := d.planEdgeCaddyfile(ctx, n, configDir, services); err != nil {
		return nil, err
	}
	d.planDNS(ctx, n, fleet)
	if d.bind != nil {
		b, _ := json.Marshal(struct{ Caddyfile string }{n.edgeCaddyfile})
		n.bind = d.bind(b)
	}
	return n, nil
}

// planBridge checks the instance bridge against fleet.yml's `network:`. The daemon never re-addresses a
// live bridge (every instance and route would move), so a difference blocks the plan rather than being
// applied: fix fleet.yml to match the host, or move the host on purpose.
func (d *Deployer) planBridge(ctx context.Context, n *NetworkPlan, fleet *config.FleetConfig) error {
	bridge, want := fleet.Network.BridgeName, fleet.Network.IPv4CIDR
	_, wantNet, err := net.ParseCIDR(want)
	if err != nil {
		n.block("fleet.yml network.ipv4_cidr %q is not a CIDR", want)
		return nil
	}
	live, err := d.exec.Run(ctx, "incus network get "+incus.ShQuote(bridge)+" ipv4.address")
	if err != nil {
		n.block("the bridge %s that fleet.yml names cannot be read: %v", bridge, err)
		return nil
	}
	live = strings.TrimSpace(live)
	ip, liveNet, err := net.ParseCIDR(live)
	if err != nil || !wantNet.Contains(ip) || liveNet.String() != wantNet.String() {
		n.block("the bridge %s has ipv4.address %s, but fleet.yml says %s; the daemon does not re-address a live bridge", bridge, orNone(live), want)
	}
	return nil
}

// planEdgeCaddyfile plans the repo's edge/Caddyfile onto the edge. Without that file the edge keeps the
// Caddyfile it has (native-ops writes a base one when it creates the edge).
func (d *Deployer) planEdgeCaddyfile(ctx context.Context, n *NetworkPlan, configDir string, services []*config.ServiceConfig) error {
	b, err := os.ReadFile(filepath.Join(configDir, "edge", "Caddyfile"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read edge/Caddyfile: %w", err)
	}
	edge := d.caddy.Container()
	exists, err := d.incus.InstanceExists(ctx, edge)
	if err != nil {
		return err
	}
	if !exists {
		for _, svc := range services {
			if svc.Name == edge {
				n.note("edge/Caddyfile is written when %s is created", edge)
				return nil
			}
		}
		n.block("edge/Caddyfile is in the repo, but there is no %s instance to apply it to", edge)
		return nil
	}
	differs, found, added, removed, err := d.caddy.CaddyfileDrift(ctx, string(b))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		n.block("edge/Caddyfile: %v", err)
		return nil
	}
	if !differs {
		return nil
	}
	n.edgeCaddyfile = string(b)
	if !found {
		n.add(ChangeEdgeCaddyfile, "write the edge's Caddyfile from edge/Caddyfile (validated, reloaded, rolled back if Caddy rejects it)")
	} else {
		n.add(ChangeEdgeCaddyfile, "edge/Caddyfile: %d lines added, %d removed (validated, reloaded, rolled back if Caddy rejects it)", added, removed)
	}
	return nil
}

// planDNS plans fleet.yml's dns_records against the provider. It only ever creates or updates: a record the
// manifest does not mention is never deleted (mail and verification records live beside these).
func (d *Deployer) planDNS(ctx context.Context, n *NetworkPlan, fleet *config.FleetConfig) {
	if len(fleet.DNSRecords) == 0 {
		return
	}
	byZone, err := fleet.DNSRecordsByZone()
	if err != nil {
		n.block("dns_records: %v", err)
		return
	}
	zones := sortedZones(byZone)
	if fleet.Domain != "" {
		for _, r := range byZone[strings.ToLower(fleet.Domain)] {
			if strings.EqualFold(r.Type, "A") && (r.Name == "@" || r.Name == "*") {
				n.block("dns_records declares A %s in %s, which reconcile also writes for `domain:` (the host's address); declare it in one place", r.Name, fleet.Domain)
			}
		}
	}
	if len(d.dnsZoneLimit) > 0 {
		for _, z := range zones {
			if !containsFold(d.dnsZoneLimit, z) {
				n.block("dns_records name the zone %s, outside the zones this daemon is limited to (%s)", z, strings.Join(d.dnsZoneLimit, ", "))
			}
		}
	}
	if len(n.Blockers) > 0 {
		return
	}
	if d.dns == nil {
		n.note("dns_records are not planned here (no DNS provider); the daemon plans and applies them")
		return
	}
	prov, err := d.dns(fleet)
	if err != nil {
		n.block("dns_records: %v", err)
		return
	}
	n.zones, n.planned = map[string][]provider.DNSRecord{}, map[string]string{}
	for _, zone := range zones {
		var desired []provider.DNSRecord
		for _, r := range byZone[zone] {
			desired = append(desired, provider.DNSRecord{Type: r.Type, Name: r.Name, Value: r.Value, TTL: r.TTL, Priority: r.Priority})
		}
		existing, err := prov.ListRecords(ctx, zone)
		if err != nil {
			n.block("dns: cannot read the zone %s: %v", zone, err)
			continue
		}
		changes := provider.PlanDNSSync(existing, desired)
		if len(changes) == 0 {
			continue
		}
		n.zones[zone], n.planned[zone] = desired, dnsDetails(zone, changes)
		for _, c := range changes {
			if c.Update {
				n.add(ChangeDNSUpdate, "%s", dnsDetail(zone, c.Record))
			} else {
				n.add(ChangeDNSCreate, "%s", dnsDetail(zone, c.Record))
			}
		}
	}
}

func dnsDetail(zone string, r provider.DNSRecord) string {
	name := r.Name + "." + zone
	if r.Name == "@" {
		name = zone
	}
	s := fmt.Sprintf("%s %s %s", r.Type, name, r.Value)
	if r.Priority > 0 {
		s += fmt.Sprintf(" priority %d", r.Priority)
	}
	if r.TTL > 0 {
		s += fmt.Sprintf(" ttl %d", r.TTL)
	}
	return s
}

// applyNetwork does what a network plan says. The DNS zones are read again first: if the records moved
// since the plan (someone edited the zone), apply refuses rather than doing something nobody reviewed.
func (d *Deployer) applyNetwork(ctx context.Context, configDir string, n *NetworkPlan) error {
	if n == nil || len(n.Changes) == 0 {
		return nil
	}
	if len(n.zones) > 0 {
		fleet, err := config.LoadFleetConfig(configDir)
		if err != nil {
			return err
		}
		if d.dns == nil {
			return errors.New("the plan changes DNS, but this apply has no DNS provider")
		}
		prov, err := d.dns(fleet)
		if err != nil {
			return fmt.Errorf("dns: %w", err)
		}
		for _, zone := range sortedZones(n.zones) {
			desired := n.zones[zone]
			existing, err := prov.ListRecords(ctx, zone)
			if err != nil {
				return fmt.Errorf("dns: read %s: %w", zone, err)
			}
			if got, want := dnsDetails(zone, provider.PlanDNSSync(existing, desired)), n.planned[zone]; got != want {
				return fmt.Errorf("dns: the zone %s changed since the plan (it would now do: %s); plan again", zone, orNone(got))
			}
			if err := prov.SyncRecords(ctx, zone, desired); err != nil {
				return fmt.Errorf("dns: sync %s: %w", zone, err)
			}
			d.log("    DNS records synced for %s\n", zone)
		}
	}
	if n.edgeCaddyfile != "" {
		changed, err := d.caddy.SyncCaddyfile(ctx, n.edgeCaddyfile)
		if err != nil {
			return fmt.Errorf("edge/Caddyfile: %w", err)
		}
		if changed {
			d.log("    Applied edge/Caddyfile to %s\n", d.caddy.Container())
		}
	}
	return nil
}

func dnsDetails(zone string, changes []provider.DNSChange) string {
	var parts []string
	for _, c := range changes {
		parts = append(parts, dnsDetail(zone, c.Record))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func sortedZones[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for z := range m {
		out = append(out, z)
	}
	sort.Strings(out)
	return out
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(x), "."), strings.TrimSuffix(s, ".")) {
			return true
		}
	}
	return false
}
