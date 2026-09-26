package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/status"
)

type fixture struct {
	srv    *httptest.Server
	secret string
	viewer string
	calls  *atomic.Int32
	audit  string
}

func setup(t *testing.T, ttl time.Duration, statusErr error) *fixture {
	t.Helper()
	dir := t.TempDir()
	tokens, err := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	admin, _, _ := tokens.Create("ci-admin", RoleAdmin)
	viewer, _, _ := tokens.Create("dash", RoleViewer)
	audit, err := OpenAudit(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { audit.Close() })
	calls := &atomic.Int32{}
	s, err := New(Options{Tokens: tokens, Audit: audit, Version: "test", StatusTTL: ttl,
		Status: func(context.Context) (*status.Snapshot, error) {
			calls.Add(1)
			if statusErr != nil {
				return nil, statusErr
			}
			return &status.Snapshot{Host: "h1", Instances: []status.Instance{{Name: "web", Status: "Running"}}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &fixture{srv: ts, secret: admin, viewer: viewer, calls: calls, audit: filepath.Join(dir, "audit.log")}
}

func (f *fixture) do(t *testing.T, method, path, token string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return res, sb.String()
}

func TestEveryAPIRouteNeedsAToken(t *testing.T) {
	f := setup(t, time.Minute, nil)
	for _, path := range []string{"/v1/status", "/v1/whoami", "/v1/anything", "/v1/"} {
		for _, tok := range []string{"", "garbage", "nops_" + strings.Repeat("0", 64)} {
			res, body := f.do(t, "GET", path, tok)
			if res.StatusCode != 401 || strings.Contains(body, "web") {
				t.Errorf("GET %s with token %q = %d, want 401 and no data (%s)", path, tok, res.StatusCode, body)
			}
			if res.Header.Get("WWW-Authenticate") == "" {
				t.Errorf("a 401 should say how to authenticate")
			}
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("an unauthenticated request must never reach the host")
	}
}

func TestHealthzAndTheUIAreOpenButNothingElseIs(t *testing.T) {
	f := setup(t, time.Minute, nil)
	res, body := f.do(t, "GET", "/healthz", "")
	if res.StatusCode != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("healthz: %d %s", res.StatusCode, body)
	}
	if strings.Contains(body, "web") || strings.Contains(body, "instances") {
		t.Fatal("healthz must not leak any state")
	}
	for _, p := range []string{"/", "/index.html", "/static/js/app.js", "/static/css/styles.css", "/static/favicon.svg", "/static-modules/fontawesome-free/webfonts/fa-solid-900.woff2"} {
		res, _ := f.do(t, "GET", p, "")
		if res.StatusCode != 200 {
			t.Errorf("%s = %d", p, res.StatusCode)
		}
		csp := res.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe-inline") || res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s needs a strict CSP and nosniff, got %q", p, csp)
		}
		// default-src 'none' blocks fonts too: without this every icon renders as an empty box.
		if !strings.Contains(csp, "font-src 'self'") {
			t.Errorf("%s: the CSP must allow the vendored icon font, got %q", p, csp)
		}
	}
	if res, _ := f.do(t, "GET", "/etc/passwd", ""); res.StatusCode != 404 {
		t.Errorf("unknown path = %d", res.StatusCode)
	}
	if res, _ := f.do(t, "GET", "/nope.html", ""); res.StatusCode != 404 {
		t.Errorf("only the embedded UI files are served, got %d", res.StatusCode)
	}
}

func TestStatusAndWhoami(t *testing.T) {
	f := setup(t, time.Minute, nil)
	res, body := f.do(t, "GET", "/v1/status", f.viewer)
	var snap status.Snapshot
	if res.StatusCode != 200 || json.Unmarshal([]byte(body), &snap) != nil || snap.Host != "h1" || snap.Instances[0].Name != "web" {
		t.Fatalf("status: %d %s", res.StatusCode, body)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Error("API responses must not be cached")
	}
	_, body = f.do(t, "GET", "/v1/whoami", f.viewer)
	if !strings.Contains(body, `"name":"dash"`) || !strings.Contains(body, `"role":"viewer"`) {
		t.Fatalf("whoami: %s", body)
	}
	if res, _ := f.do(t, "POST", "/v1/status", f.secret); res.StatusCode == 200 {
		t.Fatal("status is read-only")
	}
}

func TestStatusIsCachedBriefly(t *testing.T) {
	f := setup(t, time.Hour, nil)
	for i := 0; i < 5; i++ {
		f.do(t, "GET", "/v1/status", f.viewer)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("the host was queried %d times for 5 requests inside the TTL", f.calls.Load())
	}
	f = setup(t, time.Millisecond, nil)
	f.do(t, "GET", "/v1/status", f.viewer)
	time.Sleep(5 * time.Millisecond)
	f.do(t, "GET", "/v1/status", f.viewer)
	if f.calls.Load() != 2 {
		t.Fatalf("an expired snapshot must be refreshed, calls=%d", f.calls.Load())
	}
}

func TestHostErrorsDoNotLeakInternals(t *testing.T) {
	f := setup(t, time.Minute, errors.New("exec: /var/lib/incus/unix.socket: permission denied for user secretuser"))
	res, body := f.do(t, "GET", "/v1/status", f.viewer)
	if res.StatusCode != 502 || strings.Contains(body, "unix.socket") || strings.Contains(body, "secretuser") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

func TestEveryRequestIsAudited(t *testing.T) {
	f := setup(t, time.Minute, nil)
	f.do(t, "GET", "/v1/status?token=SHOULD-NEVER-BE-LOGGED", f.secret)
	f.do(t, "GET", "/v1/status", "wrong")
	f.do(t, "GET", "/healthz", "")
	raw, _ := os.ReadFile(f.audit)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 audit lines, got %d:\n%s", len(lines), raw)
	}
	if strings.Contains(string(raw), "SHOULD-NEVER-BE-LOGGED") || strings.Contains(string(raw), f.secret) || strings.Contains(string(raw), "wrong") {
		t.Fatal("the audit log must never contain a query string or a credential")
	}
	var e0, e1 AuditEntry
	_ = json.Unmarshal([]byte(lines[0]), &e0)
	_ = json.Unmarshal([]byte(lines[1]), &e1)
	if e0.Actor != "ci-admin" || e0.Role != RoleAdmin || e0.Path != "/v1/status" || e0.Status != 200 || e0.Method != "GET" {
		t.Errorf("authenticated entry: %+v", e0)
	}
	if e1.Actor != "-" || e1.Status != 401 {
		t.Errorf("failed auth is recorded with no actor: %+v", e1)
	}
}

func TestTheUIInsertsDataAsTextNeverHTML(t *testing.T) {
	js, err := uiFS.ReadFile("ui/static/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		if strings.Contains(string(js), banned) {
			t.Errorf("app.js uses %s: a container name would become markup", banned)
		}
	}
	html, _ := uiFS.ReadFile("ui/index.html")
	for _, tag := range scriptTag.FindAllString(string(html), -1) {
		if !strings.Contains(tag, " src=") {
			t.Errorf("%s is an inline script: the CSP forbids it", tag)
		}
	}
	if strings.Contains(string(html), "onclick=") {
		t.Error("no inline event handlers: the CSP forbids them")
	}
	if strings.Contains(string(html), "<style") || strings.Contains(string(html), " style=") || strings.Contains(string(js), `"style"`) {
		t.Error("no inline style: the CSP forbids it, and the page would render unstyled")
	}
}

var scriptTag = regexp.MustCompile(`<script[^>]*>`)

// The page is only as good as the files it names: every asset index.html loads, and every
// url() the vendored stylesheets load in turn, must be served by this binary (the CSP
// allows nothing else) with a type the browser will accept under nosniff.
func TestEveryAssetThePageNeedsIsServedWithTheRightType(t *testing.T) {
	f := setup(t, time.Minute, nil)
	_, page := f.do(t, "GET", "/", "")
	refs := regexp.MustCompile(`(?:href|src)="(/static[^"]*)"`).FindAllStringSubmatch(page, -1)
	if len(refs) < 6 {
		t.Fatalf("expected the page to load its css, js and images, found %v", refs)
	}
	want := map[string]string{".css": "text/css", ".js": "text/javascript", ".svg": "image/svg+xml", ".woff2": "font/woff2"}
	seen := map[string]bool{}
	var check func(p string)
	check = func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		res, body := f.do(t, "GET", p, "")
		if res.StatusCode != 200 || len(body) == 0 {
			t.Errorf("%s = %d (%d bytes): the page needs it", p, res.StatusCode, len(body))
			return
		}
		if w, ok := want[path.Ext(p)]; ok && !strings.HasPrefix(res.Header.Get("Content-Type"), w) {
			t.Errorf("%s served as %q, want %s", p, res.Header.Get("Content-Type"), w)
		}
		if path.Ext(p) == ".css" {
			for _, m := range regexp.MustCompile(`url\(([^)]+)\)`).FindAllStringSubmatch(body, -1) {
				u := strings.Trim(m[1], `"'`)
				if strings.HasPrefix(u, "data:") || strings.HasPrefix(u, "#") {
					continue
				}
				if strings.Contains(u, "://") {
					t.Errorf("%s loads %s from another origin; the CSP would block it", p, u)
					continue
				}
				check(path.Join(path.Dir(p), strings.SplitN(u, "?", 2)[0]))
			}
		}
	}
	for _, m := range refs {
		check(m[1])
	}
	if !seen["/static-modules/fontawesome-free/webfonts/fa-solid-900.woff2"] {
		t.Error("the icon font is not reachable from the stylesheets, every icon would be a blank box")
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("a server without a token store or status source must not start")
	}
}

// No endpoint needs more than "viewer" yet, but the role gate must already work:
// the next endpoints (deploy, token management) depend on it.
func TestTheRoleGateEnforcesTheMinimumRole(t *testing.T) {
	dir := t.TempDir()
	tokens, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	viewer, _, _ := tokens.Create("v", RoleViewer)
	deployer, _, _ := tokens.Create("d", RoleDeployer)
	admin, _, _ := tokens.Create("a", RoleAdmin)
	s, _ := New(Options{Tokens: tokens, Status: func(context.Context) (*status.Snapshot, error) { return nil, nil }})
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	for _, tc := range []struct {
		min   Role
		token string
		want  int
	}{
		{RoleAdmin, viewer, 403}, {RoleAdmin, deployer, 403}, {RoleAdmin, admin, 200},
		{RoleDeployer, viewer, 403}, {RoleDeployer, deployer, 200}, {RoleDeployer, admin, 200},
		{RoleViewer, viewer, 200}, {RoleViewer, "", 401},
	} {
		req := httptest.NewRequest("GET", "/x", nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		rec := httptest.NewRecorder()
		s.auth(tc.min, ok).ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("min=%s token=%.8s: got %d, want %d", tc.min, tc.token, rec.Code, tc.want)
		}
	}
}
