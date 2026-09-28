package caddy

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/remote"
)

// edgeSim simulates the edge container's filesystem and Caddy.
type edgeSim struct {
	files       map[string]string
	cmds        []string
	writes      int // file pushes
	removes     int
	reloads     int
	restarts    int
	rejectWhen  string // `caddy validate` fails while any file contains this
	reloadFails bool

	// resolvBroken simulates whether the container can currently resolve its own hostname -- false (the
	// default) is a resolver that already works by some other means (a fresh container's systemd-resolved
	// symlink, say) and resolvScript must leave it alone. Fixing it, when it is true, makes it false: the
	// container's own resolution is what the fix repairs, so a second attempt finds nothing to do.
	resolvBroken bool
	resolvFixes  int // how many times the resolv repair actually ran
}

var _ remote.Executor = (*edgeSim)(nil)

func newEdgeSim() *edgeSim { return &edgeSim{files: map[string]string{}} }

var (
	pushRe = regexp.MustCompile(`^printf %s '([^']*)' \| base64 -d \| incus file push .* - 'edge(/[^']*)'$`)
	pullRe = regexp.MustCompile(`^incus file pull 'edge(/[^']*)' -$`)
)

func (s *edgeSim) Run(_ context.Context, cmd string) (string, error) {
	s.cmds = append(s.cmds, cmd)
	if m := pullRe.FindStringSubmatch(cmd); m != nil {
		c, ok := s.files[m[1]]
		if !ok {
			return "", errors.New("Error: Path not found")
		}
		return c, nil
	}
	if m := pushRe.FindStringSubmatch(cmd); m != nil {
		raw, _ := base64.StdEncoding.DecodeString(m[1])
		s.files[m[2]] = string(raw)
		s.writes++
		return "", nil
	}
	switch {
	case strings.Contains(cmd, "caddy validate"):
		if s.rejectWhen != "" {
			for _, c := range s.files {
				if strings.Contains(c, s.rejectWhen) {
					return "", errors.New("Error: adapting config: unrecognized directive")
				}
			}
		}
	case strings.Contains(cmd, "caddy reload"):
		if s.reloadFails {
			return "", errors.New("Error: connection refused")
		}
		s.reloads++
	case strings.Contains(cmd, "incus restart"):
		s.restarts++
	case strings.Contains(cmd, "getent hosts"):
		if s.resolvBroken {
			s.resolvFixes++
			s.files["/etc/resolv.conf"] = "search incus\nnameserver 10.0.100.1\n"
			s.resolvBroken = false // the fix repairs the very thing it checks, so it won't run again
		}
	case strings.Contains(cmd, "-- rm -f"):
		m := regexp.MustCompile(`rm -f '([^']*)'`).FindStringSubmatch(cmd)
		delete(s.files, m[1])
		s.removes++
	}
	return "", nil
}
func (s *edgeSim) RunWithInput(context.Context, string, io.Reader) (string, error) { return "", nil }
func (s *edgeSim) WriteFile(context.Context, string, []byte, os.FileMode) error    { return nil }
func (s *edgeSim) Close() error                                                    { return nil }

func (s *edgeSim) mutations() int { return s.writes + s.removes + s.reloads + s.restarts }

var route = config.RoutingConfig{Domain: "git.example.com", UpstreamPort: 3000}

func TestPublishSiteCreatesTheBaseCaddyfileWithoutAHardcodedEmail(t *testing.T) {
	sim := newEdgeSim()
	e := NewEdgeManager(sim, "edge")
	e.Email = ""
	if err := e.PublishSite(context.Background(), "gitea", route, "10.0.100.21"); err != nil {
		t.Fatal(err)
	}
	cf := sim.files["/etc/caddy/Caddyfile"]
	if strings.Contains(cf, "email") || strings.Contains(cf, "@") || !strings.Contains(cf, "import /etc/caddy/sites/*.caddy") {
		t.Fatalf("unexpected base Caddyfile:\n%s", cf)
	}
	if !strings.Contains(sim.files["/etc/caddy/sites/gitea.caddy"], "reverse_proxy 10.0.100.21:3000") || sim.reloads != 1 {
		t.Fatalf("site not published: %v reloads=%d", sim.files, sim.reloads)
	}

	sim = newEdgeSim()
	e = NewEdgeManager(sim, "edge")
	e.Email = "ops@example.com"
	_ = e.PublishSite(context.Background(), "gitea", route, "10.0.100.21")
	if !strings.Contains(sim.files["/etc/caddy/Caddyfile"], "    email ops@example.com\n") {
		t.Fatalf("a configured email must be used:\n%s", sim.files["/etc/caddy/Caddyfile"])
	}
	e.Email = "bad email {"
	sim = newEdgeSim()
	e2 := NewEdgeManager(sim, "edge")
	e2.Email = "bad email {"
	if err := e2.PublishSite(context.Background(), "gitea", route, "10.0.100.21"); err == nil {
		t.Fatal("an unsafe email must be rejected")
	}
}

func TestPublishSiteIsIdempotent(t *testing.T) {
	sim := newEdgeSim()
	e := NewEdgeManager(sim, "edge")
	ctx := context.Background()
	if err := e.PublishSite(ctx, "gitea", route, "10.0.100.21"); err != nil {
		t.Fatal(err)
	}
	before := sim.mutations()
	for i := 0; i < 3; i++ {
		if err := e.PublishSite(ctx, "gitea", route, "10.0.100.21"); err != nil {
			t.Fatal(err)
		}
	}
	if sim.mutations() != before {
		t.Fatalf("publishing the same site again must change nothing and not reload: %v", sim.cmds)
	}
	if err := e.PublishSite(ctx, "gitea", route, "10.0.100.22"); err != nil {
		t.Fatal(err)
	}
	if sim.mutations() == before || !strings.Contains(sim.files["/etc/caddy/sites/gitea.caddy"], "10.0.100.22") {
		t.Fatalf("a real change must be applied")
	}
}

func TestExistingCaddyfileIsNeverOverwritten(t *testing.T) {
	hand := "{\n    acme_dns digitalocean {env.DO_TOKEN}\n}\nimport /etc/caddy/sites/*.caddy\nlegacy.example.com {\n    respond \"hi\"\n}\n"
	sim := newEdgeSim()
	sim.files["/etc/caddy/Caddyfile"] = hand
	e := NewEdgeManager(sim, "edge")
	if err := e.PublishSite(context.Background(), "gitea", route, "10.0.100.21"); err != nil {
		t.Fatal(err)
	}
	if sim.files["/etc/caddy/Caddyfile"] != hand {
		t.Fatalf("the hand-maintained Caddyfile was modified:\n%s", sim.files["/etc/caddy/Caddyfile"])
	}

	sim = newEdgeSim()
	sim.files["/etc/caddy/Caddyfile"] = "legacy.example.com {\n    respond \"hi\"\n}\n"
	err := NewEdgeManager(sim, "edge").PublishSite(context.Background(), "gitea", route, "10.0.100.21")
	if err == nil || !strings.Contains(err.Error(), "does not import") {
		t.Fatalf("a Caddyfile that would never serve the site must be an error, got %v", err)
	}
	if _, wrote := sim.files["/etc/caddy/sites/gitea.caddy"]; wrote || sim.writes != 0 {
		t.Fatalf("nothing may be written when the Caddyfile does not import sites")
	}
}

func TestPublishSiteRestoresThePreviousConfigWhenCaddyRejectsTheNewOne(t *testing.T) {
	sim := newEdgeSim()
	e := NewEdgeManager(sim, "edge")
	ctx := context.Background()
	if err := e.PublishSite(ctx, "gitea", route, "10.0.100.21"); err != nil {
		t.Fatal(err)
	}
	good := sim.files["/etc/caddy/sites/gitea.caddy"]
	reloads := sim.reloads

	sim.rejectWhen = "BROKEN"
	bad := route
	bad.ExtraDirectives = []string{"BROKEN directive"}
	err := e.PublishSite(ctx, "gitea", bad, "10.0.100.21")
	if err == nil || !strings.Contains(err.Error(), "previous one was restored") {
		t.Fatalf("got %v", err)
	}
	if sim.files["/etc/caddy/sites/gitea.caddy"] != good || sim.reloads != reloads {
		t.Fatalf("a rejected config must be rolled back without a reload")
	}

	// A brand-new site that Caddy rejects is removed again.
	if err := e.PublishSite(ctx, "other", bad, "10.0.100.30"); err == nil {
		t.Fatal("expected a rejection")
	}
	if _, exists := sim.files["/etc/caddy/sites/other.caddy"]; exists {
		t.Fatal("a rejected new site must not be left behind")
	}
}

func TestReloadNeverTouchesAResolverThatAlreadyWorks(t *testing.T) {
	sim := newEdgeSim()
	e := NewEdgeManager(sim, "edge")
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := e.PublishSite(ctx, "gitea", route, "10.0.100.21"); err != nil {
			t.Fatal(err)
		}
	}
	if sim.resolvFixes != 0 {
		t.Fatalf("a resolver that already works must never be rewritten, got %d fixes", sim.resolvFixes)
	}
	if _, touched := sim.files["/etc/resolv.conf"]; touched {
		t.Fatal("/etc/resolv.conf must not be touched when resolution already works")
	}
}

func TestReloadRepairsAResolverThatCannotResolveTheEdgeItself(t *testing.T) {
	sim := newEdgeSim()
	sim.resolvBroken = true
	e := NewEdgeManager(sim, "edge")
	ctx := context.Background()
	if err := e.PublishSite(ctx, "gitea", route, "10.0.100.21"); err != nil {
		t.Fatal(err)
	}
	if sim.resolvFixes != 1 {
		t.Fatalf("expected one repair, got %d", sim.resolvFixes)
	}
	got := sim.files["/etc/resolv.conf"]
	if !strings.HasPrefix(got, "nameserver ") && !strings.Contains(got, "\nnameserver ") {
		t.Fatalf("the repaired file must set a nameserver:\n%s", got)
	}
	if strings.Contains(got, "1.1.1.1") || strings.Contains(got, "8.8.8.8") {
		t.Fatalf("a public resolver must never be written ahead of (or instead of) the bridge's own: %s", got)
	}

	// Repaired: a second publish finds nothing to fix.
	if err := e.PublishSite(ctx, "home", route, "10.0.100.22"); err != nil {
		t.Fatal(err)
	}
	if sim.resolvFixes != 1 {
		t.Fatalf("a working resolver must not be repaired again, got %d fixes", sim.resolvFixes)
	}
}

func TestReloadFailureIsReportedNotSwallowed(t *testing.T) {
	sim := newEdgeSim()
	sim.reloadFails = true
	err := NewEdgeManager(sim, "edge").PublishSite(context.Background(), "gitea", route, "10.0.100.21")
	if err == nil || !strings.Contains(err.Error(), "reload failed") {
		t.Fatalf("got %v", err)
	}
	if sim.restarts != 1 {
		t.Errorf("one restart-and-retry is expected, got %d", sim.restarts)
	}
}

func TestRemoveSiteIsIdempotent(t *testing.T) {
	sim := newEdgeSim()
	e := NewEdgeManager(sim, "edge")
	ctx := context.Background()
	if err := e.RemoveSite(ctx, "gitea"); err != nil || sim.mutations() != 0 {
		t.Fatalf("removing a site that is not there must do nothing: %v %v", err, sim.cmds)
	}
	_ = e.PublishSite(ctx, "gitea", route, "10.0.100.21")
	reloads := sim.reloads
	if err := e.RemoveSite(ctx, "gitea"); err != nil {
		t.Fatal(err)
	}
	if _, still := sim.files["/etc/caddy/sites/gitea.caddy"]; still || sim.reloads != reloads+1 {
		t.Fatalf("site must be removed with one reload")
	}
	before := sim.mutations()
	_ = e.RemoveSite(ctx, "gitea")
	if sim.mutations() != before {
		t.Fatalf("the second removal must be a no-op")
	}
}

func TestPublishSiteForKeepsTheCurrentUpstreamWhileItIsStillValid(t *testing.T) {
	sim := newEdgeSim()
	e := NewEdgeManager(sim, "edge")
	ctx := context.Background()
	if err := e.PublishSite(ctx, "gitea", route, "10.0.100.50"); err != nil {
		t.Fatal(err)
	}
	before := sim.mutations()
	// The container now reports two addresses, sorted; the published one is still among them.
	if err := e.PublishSiteFor(ctx, "gitea", route, []string{"10.0.100.21", "10.0.100.50"}); err != nil {
		t.Fatal(err)
	}
	if sim.mutations() != before || !strings.Contains(sim.files["/etc/caddy/sites/gitea.caddy"], "10.0.100.50") {
		t.Fatalf("an upstream that is still valid must not flap: %v", sim.cmds)
	}
	// If it is gone, the first candidate is used.
	if err := e.PublishSiteFor(ctx, "gitea", route, []string{"10.0.100.21", "10.0.100.60"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sim.files["/etc/caddy/sites/gitea.caddy"], "10.0.100.21") {
		t.Fatalf("expected a switch to the first candidate")
	}
}

func TestPublishSiteRejectsUnsafeInput(t *testing.T) {
	for name, tc := range map[string]struct {
		site, ip string
		r        config.RoutingConfig
	}{
		"site name": {"a;b", "10.0.0.1", route},
		"domain":    {"a", "10.0.0.1", config.RoutingConfig{Domain: "x.com {\nrespond", UpstreamPort: 80}},
		"ip":        {"a", "10.0.0.1; id", route},
		"port":      {"a", "10.0.0.1", config.RoutingConfig{Domain: "x.com", UpstreamPort: 0}},
	} {
		t.Run(name, func(t *testing.T) {
			sim := newEdgeSim()
			if err := NewEdgeManager(sim, "edge").PublishSite(context.Background(), tc.site, tc.r, tc.ip); err == nil {
				t.Fatal("expected an error")
			}
			if len(sim.cmds) != 0 {
				t.Fatalf("invalid input must be rejected before anything runs: %v", sim.cmds)
			}
		})
	}
}

func TestRepointUpstreamRewritesOnlyTheAddress(t *testing.T) {
	sim := newEdgeSim()
	m := NewEdgeManager(sim, "edge")
	ctx := context.Background()
	r := config.RoutingConfig{Domain: "git.example.com", UpstreamPort: 3000, TLS: "internal", ExtraDirectives: []string{"encode gzip"}}
	if err := m.PublishSite(ctx, "gitea", r, "10.0.100.50"); err != nil {
		t.Fatal(err)
	}
	reloads := sim.reloads

	changed, err := m.RepointUpstream(ctx, "gitea", func(context.Context) ([]string, error) { return []string{"10.0.100.99"}, nil })
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	want := RenderSiteBlock("git.example.com", "10.0.100.99", 3000, "internal", []string{"encode gzip"})
	if sim.files["/etc/caddy/sites/gitea.caddy"] != want {
		t.Fatalf("only the address may change:\n%s", sim.files["/etc/caddy/sites/gitea.caddy"])
	}
	if sim.reloads != reloads+1 {
		t.Fatalf("one reload expected, got %d", sim.reloads-reloads)
	}
	before := sim.mutations()
	if changed, err := m.RepointUpstream(ctx, "gitea", func(context.Context) ([]string, error) { return []string{"10.0.100.99"}, nil }); err != nil || changed || sim.mutations() != before {
		t.Fatalf("the second call must be a no-op: changed=%v err=%v", changed, err)
	}
}

func TestRepointUpstreamLeavesAValidAddressAloneAndSkipsInstancesWithoutARoute(t *testing.T) {
	sim := newEdgeSim()
	m := NewEdgeManager(sim, "edge")
	ctx := context.Background()

	asked := false
	spy := func(context.Context) ([]string, error) { asked = true; return []string{"10.0.100.1"}, nil }
	if changed, err := m.RepointUpstream(ctx, "nothing", spy); err != nil || changed || asked || sim.mutations() != 0 {
		t.Fatalf("no site: nothing to do and no address lookup (changed=%v err=%v asked=%v)", changed, err, asked)
	}

	_ = m.PublishSite(ctx, "gitea", route, "10.0.100.50")
	before := sim.mutations()
	if changed, err := m.RepointUpstream(ctx, "gitea", func(context.Context) ([]string, error) { return []string{"10.0.100.21", "10.0.100.50"}, nil }); err != nil || changed || sim.mutations() != before {
		t.Fatalf("the published address is still one of the container's: leave it (changed=%v err=%v)", changed, err)
	}
}

func TestRepointUpstreamRestoresTheRouteIfCaddyRejectsIt(t *testing.T) {
	sim := newEdgeSim()
	m := NewEdgeManager(sim, "edge")
	ctx := context.Background()
	_ = m.PublishSite(ctx, "gitea", route, "10.0.100.50")
	good := sim.files["/etc/caddy/sites/gitea.caddy"]
	reloads := sim.reloads
	sim.rejectWhen = "10.0.100.77"
	changed, err := m.RepointUpstream(ctx, "gitea", func(context.Context) ([]string, error) { return []string{"10.0.100.77"}, nil })
	if err == nil || changed || !strings.Contains(err.Error(), "previous one was restored") {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if sim.files["/etc/caddy/sites/gitea.caddy"] != good || sim.reloads != reloads {
		t.Fatalf("a rejected repoint must roll back without a reload")
	}
	if _, err := m.RepointUpstream(ctx, "gitea", func(context.Context) ([]string, error) { return nil, nil }); err == nil {
		t.Fatal("an instance with a route but no address is an error")
	}
}

func TestSanitizeSuspendReason(t *testing.T) {
	cases := map[string]string{
		"non-payment":                      "non-payment",
		"":                                 "suspended",
		"  spaced   out  ":                 "spaced out",
		"line\nbreak\r\nhere":              "line break here",
		"quote\" and {brace} and $(shell)": "quote and brace and (shell)", // parens are allowed (matches opsavor/management's own allowlist); quotes, braces and $ are not
	}
	for in, want := range cases {
		if got := SanitizeSuspendReason(in); got != want {
			t.Errorf("SanitizeSuspendReason(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SanitizeSuspendReason(strings.Repeat("a", 200)); len(got) != 120 {
		t.Errorf("must be capped at 120 chars, got %d", len(got))
	}
}

func TestSuspendReplacesTheSiteWithA503AndIsUndoneByPublishingAgain(t *testing.T) {
	sim := newEdgeSim()
	e := NewEdgeManager(sim, "edge")
	ctx := context.Background()
	if err := e.PublishSite(ctx, "acme", route, "10.0.100.21"); err != nil {
		t.Fatal(err)
	}
	active := sim.files["/etc/caddy/sites/acme.caddy"]
	reloads := sim.reloads

	if err := e.Suspend(ctx, "acme", route, "non-payment"); err != nil {
		t.Fatal(err)
	}
	got := sim.files["/etc/caddy/sites/acme.caddy"]
	if strings.Contains(got, "reverse_proxy") || !strings.Contains(got, "503") || !strings.Contains(got, "non-payment") {
		t.Fatalf("expected a 503 responder naming the reason, got:\n%s", got)
	}
	if !strings.HasPrefix(got, "git.example.com {") {
		t.Fatalf("the domain must be unchanged:\n%s", got)
	}
	if sim.reloads != reloads+1 {
		t.Fatalf("expected one reload, got %d", sim.reloads-reloads)
	}

	// Suspending again with the same reason is a no-op (idempotent, like PublishSite).
	before := sim.mutations()
	if err := e.Suspend(ctx, "acme", route, "non-payment"); err != nil {
		t.Fatal(err)
	}
	if sim.mutations() != before {
		t.Fatal("suspending with the same reason again must change nothing")
	}

	// Publishing the site normally again is how a suspension is undone.
	if err := e.PublishSite(ctx, "acme", route, "10.0.100.21"); err != nil {
		t.Fatal(err)
	}
	if sim.files["/etc/caddy/sites/acme.caddy"] != active {
		t.Fatalf("publishing again must restore the normal site:\n%s", sim.files["/etc/caddy/sites/acme.caddy"])
	}
}

func TestSuspendRejectsUnsafeInputAndRollsBackOnRejection(t *testing.T) {
	sim := newEdgeSim()
	e := NewEdgeManager(sim, "edge")
	ctx := context.Background()
	if err := e.Suspend(ctx, "a;b", route, "x"); err == nil {
		t.Fatal("an unsafe site name must be refused")
	}
	if err := e.Suspend(ctx, "acme", config.RoutingConfig{Domain: "a b.com"}, "x"); err == nil {
		t.Fatal("an unsafe domain must be refused")
	}
	if len(sim.cmds) != 0 {
		t.Fatalf("unsafe input must be rejected before anything runs: %v", sim.cmds)
	}

	if err := e.PublishSite(ctx, "acme", route, "10.0.100.21"); err != nil {
		t.Fatal(err)
	}
	good := sim.files["/etc/caddy/sites/acme.caddy"]
	reloads := sim.reloads
	sim.rejectWhen = "503"
	if err := e.Suspend(ctx, "acme", route, "non-payment"); err == nil || !strings.Contains(err.Error(), "previous one was restored") {
		t.Fatalf("got %v", err)
	}
	if sim.files["/etc/caddy/sites/acme.caddy"] != good || sim.reloads != reloads {
		t.Fatal("a rejected suspend must roll back without a reload")
	}
}
