package config

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// DNS follows the host. In a fleet of several hosts each daemon writes only the DNS records that point at
// its own host, so two daemons never write the same record:
//
//   - a record tied to a service (a dns_records entry with `service:`) belongs to that service's host, and
//     an A or AAAA record of it points at that host's `address:`;
//   - a service on a host other than the default one, routed at a name under fleet.yml's `domain:`, gets
//     an A record of that name pointing at its host (the domain's apex and wildcard point at the default
//     host, so without it the name would reach the wrong host);
//   - every other record (dns_records without `service:`) belongs to the default host.
//
// A fleet of one host has nothing to split: its records are planned exactly as before.

// HostDNSRecord is a DNS record of the fleet, with the host whose daemon writes it.
type HostDNSRecord struct {
	DNSRecordConfig
	// Zone is the record's zone (DNSRecordConfig.Zone, or fleet.yml's domain).
	Zone string
	// Host is the host whose daemon plans and applies the record; "" in a fleet.yml without hosts:.
	Host string
	// From says where the record comes from: "dns_records", or "service <name>'s route".
	From string
}

// Key is what makes two records the same record to a zone: zone, name and type.
func (r HostDNSRecord) Key() string {
	return strings.ToLower(r.Zone) + "\x00" + strings.ToLower(r.Name) + "\x00" + strings.ToUpper(r.Type)
}

// HostAddress is a host's address as fleet.yml gives it, when that is an IP address (not "auto", not a
// name, not empty).
func (f *FleetConfig) HostAddress(host string) (netip.Addr, bool) {
	h, ok := f.Hosts[host]
	if !ok {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(strings.TrimSpace(h.Address))
	return a, err == nil
}

// DNSRecordsByHost is every DNS record the fleet's daemons write, each with its host (see the rules
// above), dns_records first in the order fleet.yml declares them, then the routes' records by service.
// services are all the repository's services. It refuses
// what no rule can place: a record tied to a service that does not exist, a record tied to a service that
// names a wildcard, a wildcard route on a host other than the default one, an A or AAAA record whose host
// has no address of that family, and the same record claimed by two hosts.
func (f *FleetConfig) DNSRecordsByHost(services []*ServiceConfig) ([]HostDNSRecord, error) {
	byName := map[string]*ServiceConfig{}
	for _, s := range services {
		byName[s.Name] = s
	}
	defaultHost := f.DefaultHostName()
	var out []HostDNSRecord

	for _, r := range f.DNSRecords {
		zone := strings.TrimSpace(r.Zone)
		if zone == "" {
			zone = strings.TrimSpace(f.Domain)
		}
		if zone == "" {
			return nil, fmt.Errorf("dns record %s %s has no zone and fleet.domain is empty", r.Type, r.Name)
		}
		rec := HostDNSRecord{DNSRecordConfig: r, Zone: zone, Host: defaultHost, From: "dns_records"}
		if r.Service == "" {
			out = append(out, rec)
			continue
		}
		what := fmt.Sprintf("dns_records %s %s (service %s)", r.Type, recordName(r.Name, zone), r.Service)
		svc, ok := byName[r.Service]
		if !ok {
			return nil, fmt.Errorf("%s: there is no service %q in services/", what, r.Service)
		}
		if strings.Contains(r.Name, "*") {
			return nil, fmt.Errorf("%s: a record tied to a service cannot be a wildcard: it would claim every name under it, those of other hosts' services included; name it, or leave service: out so it stays with the default host", what)
		}
		rec.Host = f.HostOf(svc)
		if isAddressType(r.Type) {
			if strings.TrimSpace(r.Value) != "" {
				return nil, fmt.Errorf("%s: leave value out: an %s record tied to a service points at its host's address", what, strings.ToUpper(r.Type))
			}
			v, err := f.addressFor(rec.Host, r.Type)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", what, err)
			}
			rec.Value = v
		}
		out = append(out, rec)
	}

	// A service off the default host, routed under the fleet's domain, needs its own A record: the
	// domain's wildcard points at the default host.
	if len(f.Hosts) >= 2 {
		names := make([]string, 0, len(services))
		for _, s := range services {
			names = append(names, s.Name)
		}
		sort.Strings(names)
		for _, name := range names {
			svc := byName[name]
			host := f.HostOf(svc)
			rt := svc.Routing
			if host == defaultHost || rt == nil || rt.External || strings.TrimSpace(rt.Domain) == "" {
				continue
			}
			domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(rt.Domain), "."))
			if strings.Contains(domain, "*") {
				return nil, fmt.Errorf("service %s on host %s is routed at the wildcard %s: a wildcard record would claim the names of the other hosts' services (the fleet's wildcard points at the default host, %s); route it at a name, or move it to the default host", name, host, domain, defaultHost)
			}
			zone := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(f.Domain), "."))
			if zone == "" || !strings.HasSuffix(domain, "."+zone) {
				if zone != "" && domain == zone {
					return nil, fmt.Errorf("service %s on host %s is routed at %s, the fleet's apex, which points at the default host %s; route it at a name under it, or move it to the default host", name, host, domain, defaultHost)
				}
				// Outside the fleet's zone: its record is the repository's to declare (a dns_records entry
				// with service:), since the zone it lives in is not known here.
				continue
			}
			v, err := f.addressFor(host, "A")
			if err != nil {
				return nil, fmt.Errorf("service %s is routed at %s on host %s: %w", name, domain, host, err)
			}
			out = append(out, HostDNSRecord{
				DNSRecordConfig: DNSRecordConfig{Type: "A", Name: strings.TrimSuffix(domain, "."+zone), Value: v, Service: name},
				Zone:            strings.TrimSpace(f.Domain), Host: host, From: "service " + name + "'s route",
			})
		}
	}

	// No record may be claimed by two hosts: two daemons would each write it, the last one winning. A
	// CNAME is the only record of its name, so it conflicts with any type. The domain's apex and wildcard A
	// are the default host's (reconcile writes them).
	owner := map[string]HostDNSRecord{}
	claim := func(r HostDNSRecord, key string) error {
		if o, ok := owner[key]; ok && o.Host != r.Host {
			return fmt.Errorf("%s %s is claimed by host %s (%s) and host %s (%s): one record can point at one host; give it to one of them", r.Type, recordName(r.Name, r.Zone), o.Host, o.From, r.Host, r.From)
		}
		if _, ok := owner[key]; !ok {
			owner[key] = r
		}
		return nil
	}
	if d := strings.TrimSpace(f.Domain); d != "" && defaultHost != "" {
		for _, n := range []string{"@", "*"} {
			r := HostDNSRecord{DNSRecordConfig: DNSRecordConfig{Type: "A", Name: n}, Zone: d, Host: defaultHost, From: "reconcile, for domain:"}
			owner[r.Key()] = r
		}
	}
	for _, r := range out {
		if err := claim(r, r.Key()); err != nil {
			return nil, err
		}
	}
	for _, r := range out {
		zn := strings.ToLower(r.Zone) + "\x00" + strings.ToLower(r.Name) + "\x00"
		for k, o := range owner {
			if strings.HasPrefix(k, zn) && o.Host != r.Host && (strings.EqualFold(r.Type, "CNAME") || strings.EqualFold(o.Type, "CNAME")) {
				return nil, fmt.Errorf("%s %s (host %s, %s) and %s %s (host %s, %s): a CNAME is the only record of its name, and these point at different hosts", r.Type, recordName(r.Name, r.Zone), r.Host, r.From, o.Type, recordName(o.Name, o.Zone), o.Host, o.From)
			}
		}
	}

	return out, nil
}

// addressFor is the host's address for an A (IPv4) or AAAA (IPv6) record.
func (f *FleetConfig) addressFor(host, typ string) (string, error) {
	a, ok := f.HostAddress(host)
	if !ok {
		return "", fmt.Errorf("host %s has no IP address in fleet.yml (hosts.%s.address), and the record points at it; set it", host, host)
	}
	if strings.EqualFold(typ, "A") != a.Is4() {
		return "", fmt.Errorf("host %s's address %s is not an %s address", host, a, map[bool]string{true: "IPv4 (A)", false: "IPv6 (AAAA)"}[strings.EqualFold(typ, "A")])
	}
	return a.String(), nil
}

func isAddressType(t string) bool {
	return strings.EqualFold(t, "A") || strings.EqualFold(t, "AAAA")
}

func recordName(name, zone string) string {
	if name == "@" {
		return zone
	}
	return name + "." + zone
}
