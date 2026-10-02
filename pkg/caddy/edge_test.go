package caddy

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestRenderSiteBlock(t *testing.T) {
	domain := "git.example.com"
	upstreamIP := "10.0.100.12"
	upstreamPort := 3000
	tls := "dns digitalocean"
	extra := []string{"header Strict-Transport-Security max-age=31536000"}

	block := RenderSiteBlock(domain, upstreamIP, upstreamPort, tls, extra)

	if !strings.Contains(block, "git.example.com {") {
		t.Errorf("missing site header: %s", block)
	}
	if !strings.Contains(block, "reverse_proxy 10.0.100.12:3000") {
		t.Errorf("missing reverse_proxy directive: %s", block)
	}
	if !strings.Contains(block, "tls dns digitalocean") {
		t.Errorf("missing tls directive: %s", block)
	}
	if !strings.Contains(block, "header Strict-Transport-Security") {
		t.Errorf("missing extra directive: %s", block)
	}
}

// fakeEdge simulates the edge container's file store and caddy commands, so
// SyncCaddyfile can be exercised without a host.
type fakeEdge struct {
	stdin  string // what the current RunWithInput call was fed
	files  map[string]string
	valid  bool
	reload bool
	cmds   []string
}

func newFakeEdge() *fakeEdge { return &fakeEdge{files: map[string]string{}, valid: true, reload: true} }

var (
	fakePullRe = regexp.MustCompile(`^incus file pull '([^']*)' -$`)
	fakePushRe = regexp.MustCompile(`^incus file push .* - '([^']*)'$`)
	fakeRmRe   = regexp.MustCompile(`^incus exec .* -- rm -f '([^']*)'$`)
)

func (f *fakeEdge) Run(_ context.Context, cmd string) (string, error) {
	f.cmds = append(f.cmds, cmd)
	switch {
	case fakePullRe.MatchString(cmd):
		p := fakePullRe.FindStringSubmatch(cmd)[1]
		if c, ok := f.files[p]; ok {
			return c, nil
		}
		return "", fmt.Errorf("incus file pull: %s: not found", p)
	case fakePushRe.MatchString(cmd):
		m := fakePushRe.FindStringSubmatch(cmd)
		f.files[m[1]] = f.stdin
		return "", nil
	case fakeRmRe.MatchString(cmd):
		delete(f.files, fakeRmRe.FindStringSubmatch(cmd)[1])
		return "", nil
	case strings.Contains(cmd, "caddy validate"):
		if f.valid {
			return "", nil
		}
		return "invalid config", fmt.Errorf("caddy validate failed")
	case strings.Contains(cmd, "caddy reload"):
		if f.reload {
			return "", nil
		}
		return "", fmt.Errorf("caddy reload failed")
	}
	return "", nil
}

func (f *fakeEdge) RunWithInput(ctx context.Context, cmd string, in io.Reader) (string, error) {
	b, _ := io.ReadAll(in)
	f.stdin = string(b)
	defer func() { f.stdin = "" }()
	return f.Run(ctx, cmd)
}
func (f *fakeEdge) WriteFile(context.Context, string, []byte, os.FileMode) error { return nil }
func (f *fakeEdge) Close() error                                                 { return nil }

const goodCaddyfile = "{\n    email ops@example.com\n}\n\nimport /etc/caddy/sites/*.caddy\n"

func TestSyncCaddyfileCreatesWhenAbsent(t *testing.T) {
	f := newFakeEdge()
	em := NewEdgeManager(f, "edge")
	changed, err := em.SyncCaddyfile(context.Background(), goodCaddyfile)
	if err != nil {
		t.Fatalf("SyncCaddyfile: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true when the edge has no Caddyfile")
	}
	if f.files["edge/etc/caddy/Caddyfile"] != goodCaddyfile {
		t.Fatalf("Caddyfile not written: %q", f.files["edge/etc/caddy/Caddyfile"])
	}
}

func TestSyncCaddyfileNoOpWhenIdentical(t *testing.T) {
	f := newFakeEdge()
	f.files["edge/etc/caddy/Caddyfile"] = goodCaddyfile
	em := NewEdgeManager(f, "edge")
	changed, err := em.SyncCaddyfile(context.Background(), goodCaddyfile)
	if err != nil || changed {
		t.Fatalf("expected no-op, got changed=%v err=%v", changed, err)
	}
	for _, c := range f.cmds {
		if strings.Contains(c, "caddy reload") {
			t.Fatal("an identical Caddyfile must not trigger a reload")
		}
	}
}

func TestSyncCaddyfileRejectsMissingImport(t *testing.T) {
	f := newFakeEdge()
	em := NewEdgeManager(f, "edge")
	if _, err := em.SyncCaddyfile(context.Background(), "{\n    email a@b.c\n}\n"); err == nil {
		t.Fatal("expected a Caddyfile without the sites import to be refused")
	}
	if _, err := em.SyncCaddyfile(context.Background(), "   "); err == nil {
		t.Fatal("expected an empty Caddyfile to be refused")
	}
}

func TestSyncCaddyfileRestoresPreviousOnValidateFailure(t *testing.T) {
	f := newFakeEdge()
	f.files["edge/etc/caddy/Caddyfile"] = goodCaddyfile
	f.valid = false
	em := NewEdgeManager(f, "edge")
	changed, err := em.SyncCaddyfile(context.Background(), "{\n    email new@example.com\n}\n\nimport /etc/caddy/sites/*.caddy\n")
	if err == nil || changed {
		t.Fatalf("expected a refused sync, got changed=%v err=%v", changed, err)
	}
	if f.files["edge/etc/caddy/Caddyfile"] != goodCaddyfile {
		t.Fatalf("the previous Caddyfile was not restored: %q", f.files["edge/etc/caddy/Caddyfile"])
	}
}

func TestSyncCaddyfileRestoresPreviousOnReloadFailure(t *testing.T) {
	f := newFakeEdge()
	f.files["edge/etc/caddy/Caddyfile"] = goodCaddyfile
	f.reload = false
	em := NewEdgeManager(f, "edge")
	changed, err := em.SyncCaddyfile(context.Background(), "{\n    email new@example.com\n}\n\nimport /etc/caddy/sites/*.caddy\n")
	if err == nil || changed {
		t.Fatalf("expected a failed reload, got changed=%v err=%v", changed, err)
	}
	if f.files["edge/etc/caddy/Caddyfile"] != goodCaddyfile {
		t.Fatalf("the previous Caddyfile was not restored after a failed reload: %q", f.files["edge/etc/caddy/Caddyfile"])
	}
}
