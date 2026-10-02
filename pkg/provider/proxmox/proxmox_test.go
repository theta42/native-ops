package proxmox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
)

type fakePVE struct {
	mu     sync.Mutex
	calls  []string
	bodies map[string]map[string]any
}

func newFake(t *testing.T) (*fakePVE, *Client) {
	t.Helper()
	f := &fakePVE{bodies: map[string]map[string]any{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "PVEAPIToken=root@pam!ci=secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		call := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api2/json")
		f.calls = append(f.calls, call)
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			f.bodies[call] = m
		}
		reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		switch call {
		case "GET /cluster/nextid":
			reply("105") // the real API answers a string
		case "GET /nodes/pve-01/qemu":
			reply([]map[string]any{{"vmid": 101, "name": "node-01", "status": "running", "cpus": 4, "maxmem": 8589934592}})
		case "GET /nodes/pve-01/qemu/105/status/current":
			reply(map[string]any{"name": "node-02", "status": "running", "cpus": 4})
		default:
			reply(nil)
		}
	}))
	t.Cleanup(ts.Close)
	c, err := New(ts.URL+"/", "pve-01", "root@pam!ci=secret", false)
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func TestListHosts(t *testing.T) {
	_, c := newFake(t)
	hosts, err := c.ListHosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Name != "node-01" || hosts[0].ID != "101" {
		t.Fatalf("hosts: %+v", hosts[0])
	}
}

func TestCreateHostReadsTheStringVMIDAndTheSize(t *testing.T) {
	f, c := newFake(t)
	h, err := c.CreateHost(context.Background(), config.HostSpec{Name: "node-02", Size: "4c-8192mb"})
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != "105" {
		t.Fatalf("host: %+v", h)
	}
	body := f.bodies["POST /nodes/pve-01/qemu"]
	if body["vmid"] != float64(105) || body["cores"] != float64(4) || body["memory"] != float64(8192) || body["name"] != "node-02" {
		t.Fatalf("create payload: %v", body)
	}
	if f.calls[len(f.calls)-2] != "POST /nodes/pve-01/qemu/105/status/start" {
		t.Fatalf("the VM must be started: %v", f.calls)
	}
}

func TestResizeHost(t *testing.T) {
	f, c := newFake(t)
	if err := c.ResizeHost(context.Background(), "101", "8c-16384mb"); err != nil {
		t.Fatal(err)
	}
	if b := f.bodies["PUT /nodes/pve-01/qemu/101/config"]; b["cores"] != float64(8) || b["memory"] != float64(16384) {
		t.Fatalf("resize payload: %v", b)
	}
	if err := c.ResizeHost(context.Background(), "x", "4"); err == nil {
		t.Fatal("a non-numeric VMID must be refused")
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string][2]int{"4c-8192mb": {4, 8192}, "2C-4096MB": {2, 4096}, "6": {6, 0}, "": {0, 0}, "big": {0, 0}} {
		if c, m := parseSize(in); c != want[0] || m != want[1] {
			t.Errorf("parseSize(%q) = %d, %d; want %v", in, c, m, want)
		}
	}
}

func TestNewNeedsAnEndpointAndAToken(t *testing.T) {
	t.Setenv("PVE_ENDPOINT", "")
	t.Setenv("PVE_API_TOKEN", "")
	if _, err := New("", "", "tok", false); err == nil {
		t.Fatal("no endpoint must be an error")
	}
	if _, err := New("https://pve:8006", "", "", false); err == nil {
		t.Fatal("no token must be an error")
	}
}
