package status

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/provider"
)

func TestMetricsParsers(t *testing.T) {
	l1, l5, l15 := parseLoadavg("0.52 0.48 0.44 1/512 12345\n")
	if l1 != 0.52 || l5 != 0.48 || l15 != 0.44 {
		t.Fatalf("loadavg: %v %v %v", l1, l5, l15)
	}
	mem := parseMeminfo("MemTotal:       16384000 kB\nMemAvailable:    8000000 kB\nSwapTotal:       2048000 kB\nSwapFree:        2000000 kB\nHugePages_Total:       0\n")
	if mem["MemTotal"] != 16384000 || mem["MemAvailable"] != 8000000 || mem["SwapTotal"] != 2048000 || mem["SwapFree"] != 2000000 {
		t.Fatalf("meminfo: %v", mem)
	}
	if got := parseUptime("123456.78 987654.32\n"); got != 123456 {
		t.Fatalf("uptime: %d", got)
	}
	d := parseDF("Filesystem     1024-blocks     Used Available Capacity Mounted on\n/dev/sda1         41251136 12345678  26800000      32% /\n")
	if d == nil || d.TotalKB != 41251136 || d.UsedKB != 12345678 || d.AvailableKB != 26800000 || d.UsePercent != 32 || d.Mount != "/" {
		t.Fatalf("df: %+v", d)
	}
	if parseDF("only a header\n") != nil {
		t.Fatal("a one-line df should parse to nil")
	}
}

const caddyConfig = `{"apps":{"http":{"servers":{"srv0":{"routes":[{"match":[{"host":["git.opsavor.work"]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"gitea:3000"}]}]}]}]},{"match":[{"host":["*.opsavor.work"]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"outline:3000"}]},{"handler":"reverse_proxy","upstreams":[{"dial":"plane:80"}]}]}]}]}]}}}}}`

func TestParseCaddyRoutes(t *testing.T) {
	routes, err := parseCaddyRoutes(caddyConfig)
	if err != nil {
		t.Fatal(err)
	}
	want := []Route{
		{Host: "*.opsavor.work", Upstreams: []string{"outline:3000", "plane:80"}},
		{Host: "git.opsavor.work", Upstreams: []string{"gitea:3000"}},
	}
	if !reflect.DeepEqual(routes, want) {
		t.Fatalf("routes: %+v", routes)
	}
	if _, err := parseCaddyRoutes("not json"); err == nil {
		t.Fatal("bad json should error")
	}
}

func testCertPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "example.com"},
		Issuer:       pkix.Name{CommonName: "Test CA"},
		DNSNames:     []string{"www.example.com", "example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestParseCert(t *testing.T) {
	c, ok := parseCert(testCertPEM(t))
	if !ok || c.Issuer != "example.com" || !reflect.DeepEqual(c.Names, []string{"example.com", "www.example.com"}) {
		t.Fatalf("cert: %+v ok=%v", c, ok)
	}
	if time.Until(c.NotAfter) < 80*24*time.Hour {
		t.Fatalf("not-after looks wrong: %v", c.NotAfter)
	}
	if _, ok := parseCert("not a pem"); ok {
		t.Fatal("garbage parsed as a certificate")
	}
}

func TestCollectEdgeReadsRoutesAndCerts(t *testing.T) {
	f := &fakeExec{out: map[string]string{
		"config/":        caddyConfig,
		"find /data/caddy": "/data/caddy/certificates/acme/x/example.com.crt\n",
		"example.com.crt": testCertPEM(t),
	}}
	e := collectEdge(context.Background(), f, "edge")
	if len(e.Routes) != 2 || e.Routes[0].Host != "*.opsavor.work" {
		t.Fatalf("routes: %+v", e.Routes)
	}
	if len(e.Certs) != 1 || e.Certs[0].Issuer != "example.com" {
		t.Fatalf("certs: %+v", e.Certs)
	}
	if len(e.Warnings) != 0 {
		t.Fatalf("warnings: %v", e.Warnings)
	}
}

type fakeDNS struct{ records map[string][]provider.DNSRecord; failOn string }

func (f fakeDNS) Name() string { return "fake" }
func (f fakeDNS) SyncRecords(context.Context, string, []provider.DNSRecord) error {
	return nil
}
func (f fakeDNS) ListRecords(_ context.Context, domain string) ([]provider.DNSRecord, error) {
	if domain == f.failOn {
		return nil, errTest
	}
	return f.records[domain], nil
}
func (fakeDNS) DeleteRecord(context.Context, string, string) error { return nil }

var errTest = &testError{}

type testError struct{}

func (*testError) Error() string { return "boom" }

func TestCollectDNSListsAndWarns(t *testing.T) {
	p := fakeDNS{records: map[string][]provider.DNSRecord{
		"opsavor.work": {{Type: "A", Name: "@", Value: "157.230.188.207", TTL: 1800}, {Type: "CNAME", Name: "wiki", Value: "opsavor.work."}},
	}, failOn: "opsavor.app"}
	recs, warns := collectDNS(context.Background(), p, []string{"opsavor.work", "opsavor.app"})
	if len(recs) != 2 || recs[0].Domain != "opsavor.work" || recs[1].Name != "wiki" {
		t.Fatalf("dns: %+v", recs)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "opsavor.app") {
		t.Fatalf("warnings: %v", warns)
	}
}

func TestCollectFullIncludesMetricsEdgeDNS(t *testing.T) {
	f := &fakeExec{out: map[string]string{
		"hostname": "node-1\n", "incus --version": "7.4\n", "incus list": `[]`,
		"volume list": `[]`, "image list": `[]`,
		"cat /proc/loadavg": "0.1 0.2 0.3 1/100 1\n", "nproc": "8\n",
		"cat /proc/meminfo": "MemTotal: 1000 kB\nMemAvailable: 400 kB\n",
		"cat /proc/uptime":  "500.0 1000.0\n",
		"df -Pk":            "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/x 100 40 60 40% /\n",
		"config/":           caddyConfig, "find /data/caddy": "/data/caddy/certificates/x/example.com.crt\n", "example.com.crt": testCertPEM(t),
	}}
	p := fakeDNS{records: map[string][]provider.DNSRecord{"opsavor.app": {{Type: "A", Name: "@", Value: "1.2.3.4"}}}}
	s, err := CollectFull(context.Background(), f, Options{Pool: "default", EdgeContainer: "edge", DNS: p, DNSDomains: []string{"opsavor.app"}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Metrics == nil || s.Metrics.Cores != 8 || s.Metrics.MemAvailKB != 400 || s.Metrics.Disk == nil || s.Metrics.UptimeSec != 500 {
		t.Fatalf("metrics: %+v", s.Metrics)
	}
	if s.Edge == nil || len(s.Edge.Routes) != 2 || len(s.Edge.Certs) != 1 {
		t.Fatalf("edge: %+v", s.Edge)
	}
	if len(s.DNS) != 1 || s.DNS[0].Value != "1.2.3.4" {
		t.Fatalf("dns: %+v", s.DNS)
	}
}
