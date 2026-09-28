package status

import (
	"context"
	"sort"

	"github.com/theta42/native-ops/pkg/provider"
)

// DNSRecord is one record in a managed zone: what it is, and what it points to.
type DNSRecord struct {
	Domain string `json:"domain"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Value  string `json:"value"`
	TTL    int    `json:"ttl,omitempty"`
}

// collectDNS lists the records in each zone through the configured provider. A zone that cannot be read
// becomes a warning, not a failure.
func collectDNS(ctx context.Context, p provider.DNSProvider, domains []string) ([]DNSRecord, []string) {
	var out []DNSRecord
	var warnings []string
	for _, d := range domains {
		recs, err := p.ListRecords(ctx, d)
		if err != nil {
			warnings = append(warnings, "could not list DNS records for "+d+": "+err.Error())
			continue
		}
		for _, r := range recs {
			out = append(out, DNSRecord{Domain: d, Type: r.Type, Name: r.Name, Value: r.Value, TTL: r.TTL})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Domain != out[j].Domain {
			return out[i].Domain < out[j].Domain
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Type < out[j].Type
	})
	return out, warnings
}
