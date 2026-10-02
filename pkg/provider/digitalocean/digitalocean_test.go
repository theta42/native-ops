package digitalocean

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/provider"
)

// fakeDO is a small DigitalOcean API: droplets, SSH keys and one DNS zone, paged like the real one.
type fakeDO struct {
	mu       sync.Mutex
	t        *testing.T
	perPage  int
	droplets []map[string]any
	keys     []map[string]any
	records  []map[string]any
	calls    []string
	bodies   map[string]map[string]any // "METHOD path" -> last JSON body
}

func newFake(t *testing.T) (*fakeDO, *Client) {
	f := &fakeDO{t: t, perPage: 2, bodies: map[string]map[string]any{}}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	c, err := New("tok")
	if err != nil {
		t.Fatal(err)
	}
	c.baseURL = ts.URL
	return f, c
}

func (f *fakeDO) page(w http.ResponseWriter, r *http.Request, key string, items []map[string]any) {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if p < 1 {
		p = 1
	}
	from, to := (p-1)*f.perPage, p*f.perPage
	if from > len(items) {
		from = len(items)
	}
	if to > len(items) {
		to = len(items)
	}
	out := map[string]any{key: items[from:to]}
	if to < len(items) {
		out["links"] = map[string]any{"pages": map[string]any{"next": fmt.Sprintf("%s?page=%d", r.URL.Path, p+1)}}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeDO) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	call := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, call)
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		f.bodies[call] = m
	}
	switch {
	case call == "GET /droplets":
		f.page(w, r, "droplets", f.droplets)
	case call == "POST /droplets":
		d := map[string]any{"id": 77, "name": f.bodies[call]["name"], "status": "active",
			"networks": map[string]any{"v4": []any{map[string]any{"ip_address": "203.0.113.9", "type": "public"}}}}
		f.droplets = append(f.droplets, d)
		_ = json.NewEncoder(w).Encode(map[string]any{"droplet": d})
	case strings.HasPrefix(call, "GET /droplets/"):
		for _, d := range f.droplets {
			if fmt.Sprint(d["id"]) == strings.TrimPrefix(r.URL.Path, "/droplets/") {
				_ = json.NewEncoder(w).Encode(map[string]any{"droplet": d})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case call == "GET /account/keys":
		f.page(w, r, "ssh_keys", f.keys)
	case call == "POST /account/keys":
		k := map[string]any{"id": len(f.keys) + 1, "name": f.bodies[call]["name"], "public_key": f.bodies[call]["public_key"], "fingerprint": "fp-new"}
		f.keys = append(f.keys, k)
		_ = json.NewEncoder(w).Encode(map[string]any{"ssh_key": k})
	case call == "GET /domains/example.com/records":
		f.page(w, r, "domain_records", f.records)
	case call == "POST /domains/example.com/records":
		rec := f.bodies[call]
		rec["id"] = 1000 + len(f.records)
		f.records = append(f.records, rec)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"domain_record": rec})
	case strings.HasPrefix(call, "PUT /domains/example.com/records/"):
		id := strings.TrimPrefix(r.URL.Path, "/domains/example.com/records/")
		for _, rec := range f.records {
			if fmt.Sprint(rec["id"]) == id {
				rec["data"] = f.bodies[call]["data"]
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{})
	default:
		f.t.Errorf("unexpected call %s", call)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeDO) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func TestListHostsReadsEveryPage(t *testing.T) {
	f, c := newFake(t)
	for i := 1; i <= 5; i++ {
		f.droplets = append(f.droplets, map[string]any{"id": i, "name": fmt.Sprintf("node-%02d", i), "status": "active"})
	}
	hosts, err := c.ListHosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 5 || hosts[4].Name != "node-05" {
		t.Fatalf("a host past the first page was missed (reconcile would create a duplicate): %d hosts", len(hosts))
	}
}

func TestEnsureSSHKeyReusesAKeyOnALaterPageAndRegistersANewOne(t *testing.T) {
	f, c := newFake(t)
	for i := 1; i <= 3; i++ {
		f.keys = append(f.keys, map[string]any{"id": i, "name": fmt.Sprint("k", i), "public_key": fmt.Sprint("ssh-ed25519 KEY", i), "fingerprint": fmt.Sprint("fp", i)})
	}
	fp, err := c.EnsureSSHKey(context.Background(), "node-key", "ssh-ed25519 KEY3\n")
	if err != nil || fp != "fp3" {
		t.Fatalf("an existing key on page 2 must be reused: %q %v", fp, err)
	}
	if f.count("POST /account/keys") != 0 {
		t.Fatal("an existing key was registered again")
	}
	fp, err = c.EnsureSSHKey(context.Background(), "node-key", "ssh-ed25519 OTHER")
	if err != nil || fp != "fp-new" {
		t.Fatalf("a new key must be registered: %q %v", fp, err)
	}
}

func TestCreateHostSendsUserDataAndKeysAndWaitsForAnAddress(t *testing.T) {
	if testing.Short() {
		t.Skip("polls every 3s")
	}
	f, c := newFake(t)
	h, err := c.CreateHost(context.Background(), config.HostSpec{Name: "node-01", Region: "ams3", Size: "s-2vcpu-4gb", UserData: "#cloud-config\n", SSHKeyNames: []string{"fp1"}})
	if err != nil {
		t.Fatal(err)
	}
	if h.PublicIP != "203.0.113.9" {
		t.Fatalf("host: %+v", h)
	}
	body := f.bodies["POST /droplets"]
	if body["user_data"] != "#cloud-config\n" || body["region"] != "ams3" || body["size"] != "s-2vcpu-4gb" || fmt.Sprint(body["ssh_keys"]) != "[fp1]" {
		t.Fatalf("create payload: %v", body)
	}
}

func TestSyncRecordsCreatesAndUpdatesWithoutDuplicatingARecordOnALaterPage(t *testing.T) {
	f, c := newFake(t)
	f.records = []map[string]any{
		{"id": 1, "type": "NS", "name": "@", "data": "ns1.digitalocean.com", "ttl": 1800},
		{"id": 2, "type": "NS", "name": "@", "data": "ns2.digitalocean.com", "ttl": 1800},
		{"id": 3, "type": "A", "name": "@", "data": "198.51.100.1", "ttl": 1800}, // page 2
		{"id": 4, "type": "MX", "name": "@", "data": "mx.example.com", "priority": 10, "ttl": 1800},
	}
	desired := []provider.DNSRecord{
		{Type: "A", Name: "@", Value: "203.0.113.9"},                    // changed: update id 3
		{Type: "A", Name: "*", Value: "203.0.113.9"},                    // new
		{Type: "MX", Name: "@", Value: "mx.example.com.", Priority: 10}, // unchanged (trailing dot)
	}
	if err := c.SyncRecords(context.Background(), "example.com", desired); err != nil {
		t.Fatal(err)
	}
	if f.count("PUT /domains/example.com/records/3") != 1 || f.count("POST /domains/example.com/records") != 1 {
		t.Fatalf("want one update and one create, calls: %v", f.calls)
	}
	// A second run changes nothing.
	f.calls = nil
	if err := c.SyncRecords(context.Background(), "example.com", desired); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if !strings.HasPrefix(call, "GET ") {
			t.Fatalf("a second sync must only read, got %s", call)
		}
	}
}

func TestAPIErrorsAreReported(t *testing.T) {
	_, c := newFake(t)
	c.token = "wrong"
	if _, err := c.ListHosts(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("an API error must surface with its status: %v", err)
	}
}
