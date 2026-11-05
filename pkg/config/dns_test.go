package config

import (
	"strings"
	"testing"
)

func twoHosts() *FleetConfig {
	return &FleetConfig{Domain: "example.com", DefaultHost: "h1", Hosts: map[string]HostConfig{
		"h1": {Address: "192.0.2.10"},
		"h2": {Address: "2001:db8::2"},
		"h3": {Address: "auto"},
	}}
}

func TestDNSRecordsFollowTheirServicesHost(t *testing.T) {
	f := twoHosts()
	f.DNSRecords = []DNSRecordConfig{
		{Type: "TXT", Name: "@", Value: "hello"},
		{Type: "AAAA", Name: "db", Service: "db"},
		{Type: "CNAME", Name: "www", Value: "web.example.com.", Service: "web"},
	}
	svcs := []*ServiceConfig{{Name: "web"}, {Name: "db", Host: "h2"}}
	got, err := f.DNSRecordsByHost(svcs)
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, r := range got {
		s = append(s, r.Host+" "+r.Type+" "+r.Name+" "+r.Value)
	}
	if want := "h1 TXT @ hello|h2 AAAA db 2001:db8::2|h1 CNAME www web.example.com."; strings.Join(s, "|") != want {
		t.Fatalf("got %q, want %q", strings.Join(s, "|"), want)
	}

	// One host: everything is that host's, as before (and a service-tied A still resolves).
	one := &FleetConfig{Domain: "example.com", Hosts: map[string]HostConfig{"only": {Address: "192.0.2.10"}},
		DNSRecords: []DNSRecordConfig{{Type: "MX", Name: "@", Value: "mx.example.com.", Priority: 10}}}
	got, err = one.DNSRecordsByHost([]*ServiceConfig{{Name: "web", Routing: &RoutingConfig{Domain: "web.example.com"}}})
	if err != nil || len(got) != 1 || got[0].Host != "only" {
		t.Fatalf("a single host's records are its own, and its routes add none: %+v %v", got, err)
	}
}

func TestDNSRecordsNoHostCanOwnAreRefused(t *testing.T) {
	svcs := []*ServiceConfig{{Name: "web"}, {Name: "db", Host: "h2"}, {Name: "q", Host: "h3"}}
	cases := map[string]struct {
		records []DNSRecordConfig
		svcs    []*ServiceConfig
		want    string
	}{
		"no such service":   {[]DNSRecordConfig{{Type: "A", Name: "x", Service: "nope"}}, svcs, `no service "nope"`},
		"a tied wildcard":   {[]DNSRecordConfig{{Type: "TXT", Name: "*", Value: "x", Service: "web"}}, svcs, "cannot be a wildcard"},
		"a value on tied A": {[]DNSRecordConfig{{Type: "A", Name: "x", Value: "192.0.2.99", Service: "web"}}, svcs, "leave value out"},
		"no address":        {[]DNSRecordConfig{{Type: "A", Name: "x", Service: "q"}}, svcs, "host h3 has no IP address"},
		"wrong family":      {[]DNSRecordConfig{{Type: "A", Name: "x", Service: "db"}}, svcs, "not an IPv4 (A) address"},
		"two hosts":         {[]DNSRecordConfig{{Type: "TXT", Name: "x", Value: "a"}, {Type: "TXT", Name: "x", Value: "b", Service: "db"}}, svcs, "claimed by host h1 (dns_records) and host h2 (dns_records)"},
		"a CNAME across":    {[]DNSRecordConfig{{Type: "CNAME", Name: "x", Value: "y.", Service: "web"}, {Type: "AAAA", Name: "x", Service: "db"}}, svcs, "a CNAME is the only record of its name"},
		"a route wildcard":  {nil, []*ServiceConfig{{Name: "w", Host: "h2", Routing: &RoutingConfig{Domain: "*.example.com"}}}, "routed at the wildcard"},
		"a route at apex":   {nil, []*ServiceConfig{{Name: "w", Host: "h1x", Routing: &RoutingConfig{Domain: "example.com"}}}, "the fleet's apex"},
		"a route, no addr":  {nil, []*ServiceConfig{{Name: "w", Host: "h3", Routing: &RoutingConfig{Domain: "w.example.com"}}}, "host h3 has no IP address"},
		"route vs record":   {[]DNSRecordConfig{{Type: "A", Name: "w", Value: "192.0.2.1"}}, []*ServiceConfig{{Name: "w", Host: "h4", Routing: &RoutingConfig{Domain: "w.example.com"}}}, "claimed by host h1 (dns_records) and host h4 (service w's route)"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := twoHosts()
			f.Hosts["h1x"] = HostConfig{Address: "192.0.2.12"}
			f.Hosts["h4"] = HostConfig{Address: "192.0.2.14"}
			f.DNSRecords = c.records
			if _, err := f.DNSRecordsByHost(c.svcs); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

// A route off the default host under the fleet's domain gets its own A record; an external route, a
// route on the default host and a route outside the zone get none.
func TestRoutesOffTheDefaultHostGetTheirOwnRecord(t *testing.T) {
	f := twoHosts()
	f.Hosts["h4"] = HostConfig{Address: "192.0.2.14"}
	got, err := f.DNSRecordsByHost([]*ServiceConfig{
		{Name: "a", Host: "h4", Routing: &RoutingConfig{Domain: "a.example.com"}},
		{Name: "b", Host: "h4", Routing: &RoutingConfig{Domain: "*.example.com", External: true}},
		{Name: "c", Routing: &RoutingConfig{Domain: "c.example.com"}},
		{Name: "d", Host: "h4", Routing: &RoutingConfig{Domain: "d.example.org"}},
	})
	if err != nil || len(got) != 1 || got[0].Host != "h4" || got[0].Name != "a" || got[0].Value != "192.0.2.14" || got[0].Zone != "example.com" {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestIncusRemoteIsChecked(t *testing.T) {
	for name, c := range map[string]struct {
		hosts map[string]HostConfig
		want  string
	}{
		"local":  {map[string]HostConfig{"a": {IncusRemote: "local"}}, "not the name of an Incus remote"},
		"spaces": {map[string]HostConfig{"a": {IncusRemote: "host b"}}, "not the name of an Incus remote"},
		"shared": {map[string]HostConfig{"a": {IncusRemote: "r"}, "b": {IncusRemote: "r"}}, "both have incus_remote"},
		"fine":   {map[string]HostConfig{"a": {IncusRemote: "host-a"}, "b": {IncusRemote: "host-b"}}, ""},
	} {
		f := &FleetConfig{Hosts: c.hosts, DefaultHost: "a"}
		err := f.ValidateHosts()
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
}
