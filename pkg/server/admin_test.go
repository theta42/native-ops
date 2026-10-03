package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/status"
)

type adminRig struct {
	url                     string
	tokens                  *TokenStore
	users                   *UserStore
	admin, deployer, scoped string
	srv                     *Server
	auditPath               string
}

func newAdminRig(t *testing.T) *adminRig {
	t.Helper()
	dir := t.TempDir()
	tokens, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	users, _ := OpenUserStore(filepath.Join(dir, "users.json"))
	rig := &adminRig{tokens: tokens, users: users}
	// Only the bootstrap token exists, as on a freshly provisioned host.
	rig.admin = "nops_" + strings.Repeat("b", 64)
	if err := tokens.SetBootstrap(rig.admin); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	sess, _ := NewSessions(key, time.Hour, false)
	audit, _ := OpenAudit(filepath.Join(dir, "audit.log"))
	t.Cleanup(func() { audit.Close() })
	s, err := New(Options{Tokens: tokens, Audit: audit, Version: "test", Users: users, Sessions: sess,
		Status: func(context.Context) (*status.Snapshot, error) { return &status.Snapshot{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	rig.srv, rig.auditPath = s, filepath.Join(dir, "audit.log")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	rig.url = ts.URL
	return rig
}

// reload serves the rig's server again, after a test changed its options.
func (a *adminRig) reload(t *testing.T) {
	t.Helper()
	ts := httptest.NewServer(a.srv.Handler())
	t.Cleanup(ts.Close)
	a.url = ts.URL
}

func (a *adminRig) call(t *testing.T, method, path, token string, body any, cookies ...*http.Cookie) (int, map[string]any, *http.Response) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, a.url+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return res.StatusCode, m, res
}

func TestBootstrapTokenSetsUpCITokensOverTheAPI(t *testing.T) {
	rig := newAdminRig(t)
	code, out, _ := rig.call(t, "POST", "/v1/tokens", rig.admin, map[string]any{"name": "ci-plan", "role": "planner"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	secret, _ := out["secret"].(string)
	if !ValidSecretFormat(secret) {
		t.Fatalf("no usable secret in %v", out)
	}
	if tok, _ := out["token"].(map[string]any); tok["hash"] != nil && tok["hash"] != "" {
		t.Fatalf("a hash must never be returned: %v", tok)
	}
	// The new token works at once, with its role.
	if code, who, _ := rig.call(t, "GET", "/v1/whoami", secret, nil); code != 200 || who["role"] != "planner" {
		t.Fatalf("new token: %d %v", code, who)
	}
	// ...and cannot manage tokens itself.
	if code, _, _ := rig.call(t, "POST", "/v1/tokens", secret, map[string]any{"name": "x", "role": "admin"}); code != http.StatusForbidden {
		t.Fatalf("a planner must not create tokens: %d", code)
	}

	// A scoped token for a tenant system.
	code, out, _ = rig.call(t, "POST", "/v1/tokens", rig.admin, map[string]any{"name": "fleet", "role": "deployer",
		"scope": map[string]any{"names": []string{"demo-*"}, "images": []string{"app:*"}}})
	if code != http.StatusCreated {
		t.Fatalf("scoped create: %d %v", code, out)
	}
	scoped := out["secret"].(string)
	if code, _, _ := rig.call(t, "GET", "/v1/tokens", scoped, nil); code != http.StatusForbidden {
		t.Fatalf("a scoped token must not list tokens: %d", code)
	}
	// Bad input is refused with the reason.
	if code, _, _ := rig.call(t, "POST", "/v1/tokens", rig.admin, map[string]any{"name": "x", "role": "planner",
		"scope": map[string]any{"names": []string{"a"}, "images": []string{"b"}}}); code != http.StatusBadRequest {
		t.Fatalf("a scoped planner must be refused: %d", code)
	}

	code, list, _ := rig.call(t, "GET", "/v1/tokens", rig.admin, nil)
	toks, _ := list["tokens"].([]any)
	if code != 200 || len(toks) != 2 {
		t.Fatalf("list: %d %v", code, list)
	}
	id := toks[0].(map[string]any)["id"].(string)
	if code, _, _ := rig.call(t, "DELETE", "/v1/tokens/"+id, rig.admin, nil); code != 200 {
		t.Fatalf("revoke: %d", code)
	}
	if code, _, _ := rig.call(t, "DELETE", "/v1/tokens/"+id, rig.admin, nil); code != 404 {
		t.Fatalf("revoking twice: %d", code)
	}
}

func TestAdminManagesUsersOverTheAPI(t *testing.T) {
	rig := newAdminRig(t)
	code, out, _ := rig.call(t, "POST", "/v1/users", rig.admin, map[string]any{"username": "ana", "name": "Ana", "role": "admin", "password": "a long enough password"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	if code, _, _ := rig.call(t, "POST", "/v1/users", rig.admin, map[string]any{"username": "bob", "role": "viewer", "password": "short"}); code != http.StatusBadRequest {
		t.Fatalf("a short password must be refused: %d", code)
	}
	_, _ = rig.users.CreateLocal("bob", "", RoleViewer, "another long password")
	if code, out, _ := rig.call(t, "PATCH", "/v1/users/bob", rig.admin, map[string]any{"role": "deployer"}); code != 200 {
		t.Fatalf("role change: %d %v", code, out)
	}
	if u, _, _ := rig.users.Get("bob"); u.Role != RoleDeployer {
		t.Fatalf("role not changed: %v", u.Role)
	}
	if code, _, _ := rig.call(t, "PATCH", "/v1/users/nobody", rig.admin, map[string]any{"disabled": true}); code != 404 {
		t.Fatalf("unknown user: %d", code)
	}

	// Ana signs in; she may change others but not lock herself out.
	_, _, res := rig.call(t, "POST", "/api/login", "", map[string]any{"username": "ana", "password": "a long enough password"})
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == SessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie")
	}
	if code, _, _ := rig.call(t, "PATCH", "/v1/users/bob", "", map[string]any{"disabled": true}, cookie); code != 200 {
		t.Fatalf("an admin session disabling another user: %d", code)
	}
	if code, _, _ := rig.call(t, "PATCH", "/v1/users/ana", "", map[string]any{"disabled": true}, cookie); code != http.StatusConflict {
		t.Fatalf("disabling yourself must be refused: %d", code)
	}
	if code, _, _ := rig.call(t, "PATCH", "/v1/users/ana", "", map[string]any{"role": "viewer"}, cookie); code != http.StatusConflict {
		t.Fatalf("demoting yourself must be refused: %d", code)
	}
	code, list, _ := rig.call(t, "GET", "/v1/users", "", nil, cookie)
	if code != 200 || strings.Contains(toJSON(list), "$2a$") {
		t.Fatalf("list: %d (hashes must never be returned) %v", code, list)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestUpsertByEmailGrantsAccessBeforeTheFirstSignIn(t *testing.T) {
	rig := newAdminRig(t)
	code, out, _ := rig.call(t, "PUT", "/v1/users/by-email/Sam@Acme.test", rig.admin, map[string]any{"role": "deployer", "name": "Sam"})
	if code != http.StatusCreated || out["created"] != true {
		t.Fatalf("create: %d %v", code, out)
	}
	if code, out, _ := rig.call(t, "PUT", "/v1/users/by-email/sam@acme.test", rig.admin, map[string]any{"role": "planner", "disabled": true}); code != 200 || out["created"] != false {
		t.Fatalf("update: %d %v", code, out)
	}
	users, _ := rig.users.List()
	var sam User
	for _, u := range users {
		if u.Email == "sam@acme.test" {
			sam = u
		}
	}
	if sam.Role != RolePlanner || !sam.Disabled || sam.Provider != ProviderOIDC || sam.Name != "Sam" || len(users) != 1 {
		t.Fatalf("one OIDC user, planner, disabled, its name kept: %+v (of %d)", sam, len(users))
	}

	for _, bad := range []map[string]any{{"role": "root"}, {"role": ""}} {
		if code, _, _ := rig.call(t, "PUT", "/v1/users/by-email/x@acme.test", rig.admin, bad); code != http.StatusBadRequest {
			t.Fatalf("role %v must be refused: %d", bad["role"], code)
		}
	}
	if code, _, _ := rig.call(t, "PUT", "/v1/users/by-email/not-an-email", rig.admin, map[string]any{"role": "viewer"}); code != http.StatusBadRequest {
		t.Fatalf("a malformed email must be refused: %d", code)
	}
	viewer, _, _ := rig.tokens.Create("dash", RoleViewer)
	if code, _, _ := rig.call(t, "PUT", "/v1/users/by-email/y@acme.test", viewer, map[string]any{"role": "admin"}); code != http.StatusForbidden {
		t.Fatalf("only an admin may grant access: %d", code)
	}
}

func TestOIDCRoleNoneLetsOnlyExistingUsersSignIn(t *testing.T) {
	rig := newAdminRig(t)
	idp := fakeIdP(t, map[string]any{"email": "new@opsavor.ai", "email_verified": true, "name": "New"})
	oidc, err := NewOIDC(OIDCSettings{Issuer: idp.URL, ClientID: "c", ClientSecret: "s", RedirectURL: idp.URL + "/cb", AllowedDomain: "opsavor.ai", Role: OIDCRoleNone})
	if err != nil || oidc.Role != OIDCRoleNone {
		t.Fatalf("none must be kept as the role: %v %v", err, oidc.Role)
	}
	rig.srv.opts.OIDC = oidc
	rig.reload(t)
	signIn := func() (*http.Response, bool) {
		t.Helper()
		req, _ := http.NewRequest("GET", rig.url+"/auth/oidc/callback?state=st&code=good", nil)
		req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: "st"})
		req.AddCookie(&http.Cookie{Name: oidcVerifierCookie, Value: "the-verifier"})
		res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		for _, c := range res.Cookies() {
			if c.Name == SessionCookie && c.Value != "" {
				return res, true
			}
		}
		return res, false
	}
	res, ok := signIn()
	if ok || !strings.Contains(res.Header.Get("Location"), "no+account") {
		t.Fatalf("an unknown email must be refused, not created: signed in=%v, went to %s", ok, res.Header.Get("Location"))
	}
	if users, _ := rig.users.List(); len(users) != 0 {
		t.Fatalf("nobody may be created: %+v", users)
	}
	if _, _, err := rig.users.UpsertByEmail("new@opsavor.ai", "", RoleDeployer, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := signIn(); !ok {
		t.Fatal("a user granted access must be able to sign in")
	}
	f := false
	tr := true
	_, _, _ = rig.users.UpsertByEmail("new@opsavor.ai", "", RoleDeployer, &tr)
	if _, ok := signIn(); ok {
		t.Fatal("a disabled user must not sign in")
	}
	_, _, _ = rig.users.UpsertByEmail("new@opsavor.ai", "", RoleDeployer, &f)
}
