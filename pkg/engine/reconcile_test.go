package engine

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/provider"
	"github.com/theta42/native-ops/pkg/remote"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata/")

// hostExec is a recording fake for one host's SSH session: every command it is asked to run, in order.
type hostExec struct {
	mu   sync.Mutex
	host string
	cmds []string
}

func (h *hostExec) Run(_ context.Context, cmd string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cmds = append(h.cmds, cmd)
	return "", nil
}
func (h *hostExec) RunWithInput(ctx context.Context, cmd string, _ io.Reader) (string, error) {
	return h.Run(ctx, cmd)
}
func (h *hostExec) WriteFile(context.Context, string, []byte, os.FileMode) error { return nil }
func (h *hostExec) Close() error                                                 { return nil }

// recDNS records every zone sync.
type recDNS struct {
	provider.DNSProvider
	syncs []string
	err   error
}

func (f *recDNS) SyncRecords(_ context.Context, zone string, rs []provider.DNSRecord) error {
	var parts []string
	for _, r := range rs {
		parts = append(parts, r.Type+" "+r.Name+" "+r.Value)
	}
	f.syncs = append(f.syncs, zone+": "+strings.Join(parts, ", "))
	return f.err
}

// reconcileWorld is a Reconciler wired to fakes: one recording executor per host address, the ones that
// refuse SSH, and a recording DNS provider. Its log is captured.
type reconcileWorld struct {
	r       *Reconciler
	local   *hostExec
	hosts   map[string]*hostExec // by address
	refuse  map[string]bool      // addresses whose SSH never answers
	dns     *recDNS
	dialLog []string
}

func newReconcileWorld(t *testing.T, dir string) *reconcileWorld {
	t.Helper()
	w := &reconcileWorld{local: &hostExec{host: "local"}, hosts: map[string]*hostExec{}, refuse: map[string]bool{}, dns: &recDNS{}}
	r := NewReconciler(dir, w.local)
	r.sshCreds = func() ([]byte, string, bool) { return []byte("test-key"), "ssh-ed25519 AAAA test", false }
	r.dialSSH = func(host string, port int, user string, _ []byte) (remote.Executor, error) {
		w.dialLog = append(w.dialLog, fmt.Sprintf("dial %s@%s:%d", user, host, port))
		if w.refuse[host] {
			return nil, errors.New("connection refused")
		}
		if w.hosts[host] == nil {
			w.hosts[host] = &hostExec{host: host}
		}
		return w.hosts[host], nil
	}
	r.waitSSH = func(context.Context, string, int, time.Duration) error { return nil }
	r.sshRetryDelay = 0
	r.dnsFor = func(*config.FleetConfig) (provider.DNSProvider, error) { return w.dns, nil }
	w.r = r
	return w
}

// run reconciles and returns everything observable: the log, the error, the SSH dials, the DNS syncs and
// the commands run on each host.
func (w *reconcileWorld) run(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	flags, out := log.Flags(), log.Writer()
	log.SetFlags(0)
	log.SetOutput(&buf)
	err := w.r.Reconcile(context.Background())
	log.SetFlags(flags)
	log.SetOutput(out)

	var sb strings.Builder
	sb.WriteString("--- log\n")
	sb.WriteString(strings.ReplaceAll(buf.String(), w.r.configDir, "<config-dir>"))
	fmt.Fprintf(&sb, "--- error\n%v\n", err)
	sb.WriteString("--- ssh\n")
	for _, d := range w.dialLog {
		sb.WriteString(d + "\n")
	}
	sb.WriteString("--- dns\n")
	for _, s := range w.dns.syncs {
		sb.WriteString(s + "\n")
	}
	execs := []*hostExec{w.local}
	var addrs []string
	for a := range w.hosts {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	for _, a := range addrs {
		execs = append(execs, w.hosts[a])
	}
	for _, e := range execs {
		fmt.Fprintf(&sb, "--- commands on %s\n", e.host)
		for _, c := range e.cmds {
			sb.WriteString(c + "\n")
		}
	}
	return sb.String()
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Fatalf("%s differs from the golden output.\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// A single-host fleet reconciles exactly as it did when reconcile kept one primaryHostIP: the golden
// files were generated from main before the per-host loop (with only the fakes wired in).
func TestReconcileASingleHostIsUnchanged(t *testing.T) {
	const fleet = "name: f\ndomain: example.com\ndns_provider: fake\nhosts:\n  only: {provider: static, address: 198.51.100.10, ssh_user: ops, ssh_port: 2222}\n"
	cases := []struct {
		name, fleet string
		deploy      bool
		dnsErr      error
		refuse      bool
	}{
		{name: "ssh", fleet: fleet},
		{name: "ssh-deploy-services", fleet: fleet, deploy: true},
		{name: "dns-fails", fleet: fleet, dnsErr: errors.New("403 forbidden")},
		{name: "ssh-fails", fleet: fleet, refuse: true},
		{name: "local", fleet: "name: f\ndomain: example.com\ndns_provider: fake\nhosts:\n  only: {provider: static, address: 127.0.0.1}\n", deploy: true},
		{name: "no-address", fleet: "name: f\nhosts:\n  only: {provider: proxmox}\n", deploy: true},
		{name: "no-hosts", fleet: "name: f\n", deploy: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := shareTree(t, c.fleet, nil)
			w := newReconcileWorld(t, dir)
			w.r.DeployServices = c.deploy
			w.dns.err = c.dnsErr
			if c.refuse {
				w.refuse["198.51.100.10"] = true
			}
			checkGolden(t, "reconcile-single-host-"+c.name+".golden", w.run(t))
		})
	}
}
