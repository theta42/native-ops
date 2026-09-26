package provider

import "testing"

func rec(id, typ, name, val string, ttl, prio int) DNSRecord {
	return DNSRecord{ID: id, Type: typ, Name: name, Value: val, TTL: ttl, Priority: prio}
}

// apply simulates the API calls, so a plan can be checked for convergence.
func apply(existing []DNSRecord, changes []DNSChange) []DNSRecord {
	out := append([]DNSRecord{}, existing...)
	next := 1000
	for _, ch := range changes {
		if ch.Update {
			for i := range out {
				if out[i].ID == ch.Record.ID {
					r := ch.Record
					if r.TTL == 0 {
						r.TTL = out[i].TTL
					}
					if r.Priority == 0 {
						r.Priority = out[i].Priority
					}
					out[i] = r
				}
			}
			continue
		}
		r := ch.Record
		next++
		r.ID = string(rune('a' + next%26))
		r.ID += string(rune('0' + next%10))
		out = append(out, r)
	}
	return out
}

func TestPlanDNSSyncIsEmptyWhenTheZoneAlreadyMatches(t *testing.T) {
	existing := []DNSRecord{rec("1", "A", "@", "1.2.3.4", 1800, 0), rec("2", "A", "*", "1.2.3.4", 1800, 0), rec("3", "MX", "@", "mail.example.com.", 3600, 10)}
	desired := []DNSRecord{{Type: "A", Name: "@", Value: "1.2.3.4"}, {Type: "a", Name: "*", Value: "1.2.3.4"}}
	if ch := PlanDNSSync(existing, desired); len(ch) != 0 {
		t.Fatalf("nothing to do, got %+v", ch)
	}
}

func TestPlanDNSSyncUpdatesASingleValuedRecordInPlace(t *testing.T) {
	existing := []DNSRecord{rec("1", "A", "@", "1.1.1.1", 1800, 0)}
	ch := PlanDNSSync(existing, []DNSRecord{{Type: "A", Name: "@", Value: "2.2.2.2"}})
	if len(ch) != 1 || !ch[0].Update || ch[0].Record.ID != "1" || ch[0].Record.Value != "2.2.2.2" {
		t.Fatalf("got %+v", ch)
	}
}

func TestPlanDNSSyncHandlesMultiValueRecordsWithoutCollapsingThem(t *testing.T) {
	existing := []DNSRecord{rec("1", "TXT", "@", "v=spf1 -all", 300, 0), rec("2", "TXT", "@", "google-site-verification=abc", 300, 0)}
	desired := []DNSRecord{{Type: "TXT", Name: "@", Value: "v=spf1 -all"}, {Type: "TXT", Name: "@", Value: "google-site-verification=abc"}, {Type: "TXT", Name: "@", Value: "new=1"}}
	ch := PlanDNSSync(existing, desired)
	if len(ch) != 1 || ch[0].Update || ch[0].Record.Value != "new=1" {
		t.Fatalf("only the missing value may be created; the existing ones must not be touched: %+v", ch)
	}

	// Round-robin A records: a second address is added, the first is not rewritten.
	ch = PlanDNSSync([]DNSRecord{rec("1", "A", "www", "1.1.1.1", 300, 0)}, []DNSRecord{{Type: "A", Name: "www", Value: "1.1.1.1"}, {Type: "A", Name: "www", Value: "2.2.2.2"}})
	if len(ch) != 1 || ch[0].Update || ch[0].Record.Value != "2.2.2.2" {
		t.Fatalf("got %+v", ch)
	}
}

func TestPlanDNSSyncNeverDeletesUnlistedRecords(t *testing.T) {
	existing := []DNSRecord{rec("1", "MX", "@", "mail.example.com.", 3600, 10), rec("2", "TXT", "_dmarc", "v=DMARC1", 300, 0), rec("3", "A", "@", "1.1.1.1", 1800, 0)}
	ch := PlanDNSSync(existing, []DNSRecord{{Type: "A", Name: "@", Value: "1.1.1.1"}})
	if len(ch) != 0 {
		t.Fatalf("a sync must leave records it was not told about alone: %+v", ch)
	}
}

func TestPlanDNSSyncComparesTTLAndPriorityOnlyWhenDesired(t *testing.T) {
	existing := []DNSRecord{rec("1", "A", "@", "1.1.1.1", 1800, 0), rec("2", "MX", "@", "mail.example.com", 3600, 10)}
	if ch := PlanDNSSync(existing, []DNSRecord{{Type: "A", Name: "@", Value: "1.1.1.1"}}); len(ch) != 0 {
		t.Fatalf("an unspecified TTL must not be forced: %+v", ch)
	}
	ch := PlanDNSSync(existing, []DNSRecord{{Type: "A", Name: "@", Value: "1.1.1.1", TTL: 300}, {Type: "MX", Name: "@", Value: "mail.example.com", Priority: 20}})
	if len(ch) != 2 || !ch[0].Update || ch[0].Record.ID != "1" || !ch[1].Update || ch[1].Record.ID != "2" {
		t.Fatalf("TTL and priority changes must be updates of the existing records: %+v", ch)
	}
}

func TestPlanDNSSyncNamesAndHostnameValuesAreCaseAndDotInsensitive(t *testing.T) {
	existing := []DNSRecord{rec("1", "CNAME", "WWW", "Example.COM.", 1800, 0)}
	if ch := PlanDNSSync(existing, []DNSRecord{{Type: "CNAME", Name: "www", Value: "example.com"}}); len(ch) != 0 {
		t.Fatalf("got %+v", ch)
	}
}

func TestPlanDNSSyncConvergesInOneStep(t *testing.T) {
	existing := []DNSRecord{rec("1", "A", "@", "9.9.9.9", 1800, 0), rec("2", "TXT", "@", "keep-me", 300, 0)}
	desired := []DNSRecord{
		{Type: "A", Name: "@", Value: "1.2.3.4"}, {Type: "A", Name: "*", Value: "1.2.3.4"},
		{Type: "TXT", Name: "@", Value: "keep-me"}, {Type: "TXT", Name: "@", Value: "and-me"},
		{Type: "MX", Name: "@", Value: "mx.example.com", Priority: 10, TTL: 3600},
	}
	after := apply(existing, PlanDNSSync(existing, desired))
	if again := PlanDNSSync(after, desired); len(again) != 0 {
		t.Fatalf("running the same sync twice must be a no-op the second time, got %+v", again)
	}
}
