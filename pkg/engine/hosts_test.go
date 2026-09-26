package engine

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/provider"
)

func hostsOf(hs ...*provider.Host) []*provider.Host { return hs }

func TestClassifyHost(t *testing.T) {
	active := &provider.Host{ID: "1", Name: "node-01", Status: "active", PublicIP: "1.2.3.4"}
	fresh := &provider.Host{ID: "2", Name: "node-01", Status: "new"}
	off := &provider.Host{ID: "3", Name: "node-01", Status: "off"}
	other := &provider.Host{ID: "4", Name: "other", Status: "active"}

	if h, a, err := classifyHost(hostsOf(other), "node-01"); err != nil || a != hostCreate || h != nil {
		t.Fatalf("no host of that name: create, got %v %v %v", h, a, err)
	}
	if h, a, err := classifyHost(hostsOf(other, active), "node-01"); err != nil || a != hostUse || h.ID != "1" {
		t.Fatalf("active host is used: %v %v %v", h, a, err)
	}
	// The bug this guards: a droplet that is still provisioning must be waited for, not duplicated.
	if h, a, err := classifyHost(hostsOf(fresh), "node-01"); err != nil || a != hostWait || h.ID != "2" {
		t.Fatalf("a provisioning host is waited for: %v %v %v", h, a, err)
	}
	if _, _, err := classifyHost(hostsOf(off), "node-01"); err == nil || !strings.Contains(err.Error(), "not creating a second one") {
		t.Fatalf("a stopped host must not be silently duplicated: %v", err)
	}
	if _, _, err := classifyHost(hostsOf(active, fresh), "node-01"); err == nil || !strings.Contains(err.Error(), "refusing to guess") {
		t.Fatalf("duplicates are an error, not a coin flip: %v", err)
	}
}

type fakeCompute struct {
	provider.ComputeProvider
	polls    int
	activeAt int
	err      error
}

func (f *fakeCompute) GetHost(_ context.Context, id string) (*provider.Host, error) {
	f.polls++
	if f.err != nil {
		return nil, f.err
	}
	if f.polls >= f.activeAt {
		return &provider.Host{ID: id, Status: "active", PublicIP: "9.9.9.9"}, nil
	}
	return &provider.Host{ID: id, Status: "new"}, nil
}

func TestWaitHostActive(t *testing.T) {
	f := &fakeCompute{activeAt: 3}
	h, err := waitHostActive(context.Background(), f, "7", time.Second, time.Millisecond)
	if err != nil || h.PublicIP != "9.9.9.9" || f.polls != 3 {
		t.Fatalf("got %v %v polls=%d", h, err, f.polls)
	}
	f = &fakeCompute{activeAt: 1 << 30}
	if _, err := waitHostActive(context.Background(), f, "7", 20*time.Millisecond, 5*time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not become active") {
		t.Fatalf("expected a timeout, got %v", err)
	}
	f = &fakeCompute{err: errors.New("api down")}
	if _, err := waitHostActive(context.Background(), f, "7", 20*time.Millisecond, 5*time.Millisecond); err == nil {
		t.Fatal("expected an error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := waitHostActive(ctx, &fakeCompute{activeAt: 1 << 30}, "7", time.Hour, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

type fakeDNS struct {
	provider.DNSProvider
	got []provider.DNSRecord
	err error
}

func (f *fakeDNS) SyncRecords(_ context.Context, _ string, r []provider.DNSRecord) error {
	f.got = r
	return f.err
}

func TestSyncDNSReportsFailures(t *testing.T) {
	f := &fakeDNS{}
	if err := syncDNS(context.Background(), f, "example.com", "1.2.3.4"); err != nil || len(f.got) != 2 || f.got[1].Name != "*" || f.got[0].Value != "1.2.3.4" {
		t.Fatalf("got %v %v", f.got, err)
	}
	if err := syncDNS(context.Background(), &fakeDNS{err: errors.New("403")}, "example.com", "1.2.3.4"); err == nil {
		t.Fatal("a DNS failure must be returned, not swallowed")
	}
}

func TestHostBootstrapCommandsAreGuarded(t *testing.T) {
	cmds := hostBootstrapCommands()
	if len(cmds) == 0 {
		t.Fatal("no commands")
	}
	for _, c := range cmds {
		switch {
		case strings.Contains(c, "iptables") && regexp.MustCompile(`-[IA] `).MatchString(c) && !strings.Contains(c, "-C "):
			t.Errorf("an iptables rule is added without checking for it first (duplicates on every run): %s", c)
		case strings.Contains(c, "incus network set") && !strings.Contains(c, "incus network get"):
			t.Errorf("network setting applied unconditionally on every run: %s", c)
		case strings.Contains(c, "passwd -d") && !strings.Contains(c, "passwd -S"):
			t.Errorf("root password cleared without checking whether it is set: %s", c)
		case strings.Contains(c, "sysctl -w") && !strings.Contains(c, "sysctl -n"):
			t.Errorf("sysctl applied without checking the current value: %s", c)
		}
	}
	joined := strings.Join(cmds, "\n")
	for _, want := range []string{"-C FORWARD -i incusbr0", "-C FORWARD -o incusbr0", "-C INPUT -i incusbr0", "ipv4.address", "raw.dnsmasq"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected the bootstrap to cover %q", want)
		}
	}
}

var _ = config.HostSpec{}
