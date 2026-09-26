package provider

import "strings"

// DNSChange is one API call needed to bring a zone to the desired state.
type DNSChange struct {
	// Record is the desired record. When Update is true its ID is the existing
	// record to modify; otherwise it is created.
	Record DNSRecord
	Update bool
}

func dnsKey(typ, name string) string { return strings.ToUpper(typ) + "\x00" + strings.ToLower(name) }

// normValue compares hostnames case-insensitively and ignores a trailing dot;
// A, AAAA and TXT values are compared exactly.
func normValue(typ, v string) string {
	switch strings.ToUpper(typ) {
	case "A", "AAAA", "TXT":
		return v
	}
	return strings.ToLower(strings.TrimSuffix(v, "."))
}

func singleValued(typ string) bool {
	switch strings.ToUpper(typ) {
	case "A", "AAAA", "CNAME":
		return true
	}
	return false
}

// PlanDNSSync computes the minimal changes that make `existing` contain
// `desired`. It only ever creates or updates: records that are not mentioned in
// `desired` are left alone (a sync must never delete mail or verification
// records it does not know about). Records are matched by type, name AND value,
// so multi-value sets (MX, TXT, round-robin A) work; a single-valued type
// (A, AAAA, CNAME) whose only record has a different value is updated in
// place. A TTL or priority is compared only when the desired record sets one.
// Applying the result and planning again yields no changes.
func PlanDNSSync(existing, desired []DNSRecord) []DNSChange {
	have := map[string][]DNSRecord{}
	for _, e := range existing {
		k := dnsKey(e.Type, e.Name)
		have[k] = append(have[k], e)
	}
	want := map[string][]DNSRecord{}
	var order []string
	for _, d := range desired {
		k := dnsKey(d.Type, d.Name)
		if _, seen := want[k]; !seen {
			order = append(order, k)
		}
		want[k] = append(want[k], d)
	}

	withID := func(d DNSRecord, id string) DNSRecord { d.ID = id; return d }
	var changes []DNSChange
	for _, k := range order {
		ex := have[k]
		used := make([]bool, len(ex))
		var missing []DNSRecord
		for _, d := range want[k] {
			hit := -1
			for i, e := range ex {
				if !used[i] && normValue(d.Type, e.Value) == normValue(d.Type, d.Value) {
					hit = i
					break
				}
			}
			if hit < 0 {
				missing = append(missing, d)
				continue
			}
			used[hit] = true
			if (d.TTL > 0 && ex[hit].TTL != d.TTL) || (d.Priority > 0 && ex[hit].Priority != d.Priority) {
				changes = append(changes, DNSChange{Record: withID(d, ex[hit].ID), Update: true})
			}
		}
		var spare []DNSRecord
		for i, e := range ex {
			if !used[i] {
				spare = append(spare, e)
			}
		}
		if len(missing) == 1 && len(spare) == 1 && singleValued(missing[0].Type) {
			changes = append(changes, DNSChange{Record: withID(missing[0], spare[0].ID), Update: true})
			continue
		}
		for _, m := range missing {
			changes = append(changes, DNSChange{Record: m})
		}
	}
	return changes
}
