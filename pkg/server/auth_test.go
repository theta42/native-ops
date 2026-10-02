package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/status"
)

func testStore(t *testing.T) *UserStore {
	t.Helper()
	s, err := OpenUserStore(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUserStoreCreateAuthenticateDisable(t *testing.T) {
	s := testStore(t)
	if _, err := s.CreateLocal("sam", "Sam", RoleDeployer, "short"); err == nil {
		t.Fatal("a weak password was accepted")
	}
	u, err := s.CreateLocal("Sam", "Sam", RoleDeployer, "a long enough password")
	if err != nil || u.Username != "sam" || u.Role != RoleDeployer {
		t.Fatalf("create: %v %+v", err, u)
	}
	if u.Hash != "" {
		t.Fatal("CreateLocal returned the hash")
	}
	if _, err := s.CreateLocal("sam", "", RoleViewer, "a long enough password"); err == nil {
		t.Fatal("a duplicate username was accepted")
	}
	if got, err := s.Authenticate("SAM", "a long enough password"); err != nil || got.Username != "sam" {
		t.Fatalf("authenticate (case-insensitive): %v", err)
	}
	if _, err := s.Authenticate("sam", "the wrong password"); err == nil {
		t.Fatal("a wrong password was accepted")
	}
	if _, err := s.Authenticate("nobody", "a long enough password"); err == nil {
		t.Fatal("an unknown user was accepted")
	}
	// The file holds a bcrypt hash, not the password, and is 0600.
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(s.path), "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "a long enough password") {
		t.Fatal("the password is stored in the clear")
	}
	if fi, _ := os.Stat(filepath.Join(filepath.Dir(s.path), "users.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("users file mode is %o", fi.Mode().Perm())
	}
	if err := s.SetDisabled("sam", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate("sam", "a long enough password"); err == nil {
		t.Fatal("a disabled user was accepted")
	}
}

func TestFindOrCreateOIDCProvisionAndLink(t *testing.T) {
	s := testStore(t)
	u, err := s.FindOrCreateOIDC("Owner@Acme.test", "Owner", RoleViewer)
	if err != nil || u.Provider != ProviderOIDC || u.Email != "owner@acme.test" || u.Username == "" {
		t.Fatalf("create oidc: %v %+v", err, u)
	}
	again, _ := s.FindOrCreateOIDC("owner@acme.test", "Owner Two", RoleAdmin)
	if again.Username != u.Username {
		t.Fatalf("a second sign-in made a new user: %q vs %q", again.Username, u.Username)
	}
	if again.Role != RoleViewer {
		t.Fatalf("a second sign-in changed the role to %s", again.Role)
	}
	// An email at a different domain is a different user.
	other, _ := s.FindOrCreateOIDC("someone@else.test", "", RoleViewer)
	if other.Username == u.Username {
		t.Fatal("two emails share a username")
	}
}

func TestSessionsSignVerifyTamperAndExpiry(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	s, err := NewSessions(key, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	tok := s.Issue("sam", RoleDeployer)
	if username, role, ok := s.Verify(tok); !ok || username != "sam" || role != RoleDeployer {
		t.Fatalf("verify round-trip: %q %q %v", username, role, ok)
	}
	if _, _, ok := s.Verify(tok + "x"); ok {
		t.Fatal("a tampered token verified")
	}
	other, _ := NewSessions([]byte("0123456789abcdef0123456789abcdef"), time.Hour, false)
	if _, _, ok := other.Verify(tok); ok {
		t.Fatal("a token verified under a different key")
	}
	short, _ := NewSessions(key, time.Millisecond, false)
	time.Sleep(5 * time.Millisecond)
	if _, _, ok := short.Verify(short.Issue("sam", RoleViewer)); ok {
		t.Fatal("an expired token verified")
	}
}

// fakeIdP is a minimal OpenID Connect provider: discovery, a token endpoint and a userinfo endpoint.
func fakeIdP(t *testing.T, userinfo map[string]any) *httptest.Server {
	t.Helper()
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"userinfo_endpoint":%q}`,
			issuer, issuer+"/authorize", issuer+"/token", issuer+"/userinfo")
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "good" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-123"})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(userinfo)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	issuer = ts.URL
	return ts
}

func TestOIDCGenericFlowAndChecks(t *testing.T) {
	idp := fakeIdP(t, map[string]any{"email": "Will@Opsavor.AI", "email_verified": true, "name": "Will"})
	cfg, err := NewOIDC(OIDCSettings{
		Issuer: idp.URL, ClientID: "cid", ClientSecret: "secret",
		RedirectURL: idp.URL + "/auth/oidc/callback", AllowedDomain: "opsavor.ai", Role: RoleDeployer, Label: "Test IdP",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	authURL, err := cfg.AuthorizeURL(ctx, "state123")
	if err != nil {
		t.Fatal(err)
	}
	redirect := url.QueryEscape(idp.URL + "/auth/oidc/callback")
	for _, want := range []string{"client_id=cid", "state=state123", "response_type=code", "redirect_uri=" + redirect} {
		if !strings.Contains(authURL, want) {
			t.Fatalf("authorize url missing %q: %s", want, authURL)
		}
	}
	at, err := cfg.Exchange(ctx, "good")
	if err != nil || at != "at-123" {
		t.Fatalf("exchange: %v", err)
	}
	if _, err := cfg.Exchange(ctx, "bad"); err == nil {
		t.Fatal("a bad code was exchanged")
	}
	id, err := cfg.Identity(ctx, at)
	if err != nil || id.Email != "will@opsavor.ai" || id.Name != "Will" {
		t.Fatalf("identity: %v %+v", err, id)
	}
}

func TestOIDCRejectsUnverifiedEmailAndWrongDomain(t *testing.T) {
	unverified := fakeIdP(t, map[string]any{"email": "will@opsavor.ai", "email_verified": false})
	cfg, _ := NewOIDC(OIDCSettings{Issuer: unverified.URL, ClientID: "c", ClientSecret: "s", RedirectURL: unverified.URL + "/cb", AllowedDomain: "opsavor.ai"})
	if _, err := cfg.Identity(context.Background(), "at-123"); err == nil {
		t.Fatal("an unverified email was accepted")
	}

	outsider := fakeIdP(t, map[string]any{"email": "someone@gmail.com", "email_verified": true})
	cfg2, _ := NewOIDC(OIDCSettings{Issuer: outsider.URL, ClientID: "c", ClientSecret: "s", RedirectURL: outsider.URL + "/cb", AllowedDomain: "opsavor.ai"})
	if _, err := cfg2.Identity(context.Background(), "at-123"); err == nil {
		t.Fatal("an email outside the allowed domain was accepted")
	}
}

func TestLoginSetsACookieAndTheAPIHonoursIt(t *testing.T) {
	dir := t.TempDir()
	tokens, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	users, _ := OpenUserStore(filepath.Join(dir, "users.json"))
	if _, err := users.CreateLocal("sam", "Sam", RoleDeployer, "a long enough password"); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	sess, _ := NewSessions(key, time.Hour, false)
	audit, _ := OpenAudit(filepath.Join(dir, "audit.log"))
	defer audit.Close()
	s, err := New(Options{Tokens: tokens, Audit: audit, Version: "test", Users: users, Sessions: sess,
		Status: func(context.Context) (*status.Snapshot, error) { return &status.Snapshot{Host: "h1"}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	post := func(body string) *http.Response {
		res, err := http.Post(ts.URL+"/api/login", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := post(`{"username":"sam","password":"the wrong password"}`); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad password: %d", res.StatusCode)
	}
	// Repeated failures for one account are refused for a while, even with the right password.
	for i := 0; i <= throttleFree; i++ {
		post(`{"username":"mallory","password":"guess guess guess"}`)
	}
	if res := post(`{"username":"mallory","password":"guess guess guess"}`); res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("a guessed-at account must be throttled: %d", res.StatusCode)
	}
	res := post(`{"username":"sam","password":"a long enough password"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", res.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == SessionCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly {
		t.Fatal("login did not set an HttpOnly session cookie")
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/whoami", nil)
	req.AddCookie(cookie)
	who, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if who.StatusCode != http.StatusOK {
		t.Fatalf("whoami with a session cookie: %d", who.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(who.Body).Decode(&out)
	if out["name"] != "sam" || out["role"] != "deployer" {
		t.Fatalf("whoami body: %v", out)
	}

	// The cookie follows the user store, not the role it was issued with: a demotion takes effect at
	// once, and a disabled user is signed out on their next request.
	whoami := func() (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/whoami", nil)
		req.AddCookie(cookie)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(res.Body).Decode(&m)
		return res.StatusCode, m
	}
	if err := users.SetRole("sam", RoleViewer); err != nil {
		t.Fatal(err)
	}
	if code, m := whoami(); code != http.StatusOK || m["role"] != "viewer" {
		t.Fatalf("after a demotion the session must carry the new role: %d %v", code, m)
	}
	if err := users.SetDisabled("sam", true); err != nil {
		t.Fatal(err)
	}
	if code, _ := whoami(); code != http.StatusUnauthorized {
		t.Fatalf("a disabled user's session must stop working at once: %d", code)
	}
	_ = users.SetDisabled("sam", false)
	_ = users.SetRole("sam", RoleDeployer)

	// A request with no cookie and no token is still refused.
	anon, _ := http.Get(ts.URL + "/v1/whoami")
	if anon.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous whoami: %d", anon.StatusCode)
	}
	// /api/session is public and says how one may sign in.
	sres, _ := http.Get(ts.URL + "/api/session")
	var sm map[string]any
	_ = json.NewDecoder(sres.Body).Decode(&sm)
	if sm["local_signin"] != true {
		t.Fatalf("session methods: %v", sm)
	}
}
