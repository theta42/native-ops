package server

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/status"
)

type oauthRig struct {
	*applyRig
	users    *UserStore
	store    *OAuthStore
	browser  *http.Client // a person's browser: keeps cookies, does not follow redirects
	clientID string
}

const (
	testRedirect = "http://127.0.0.1:33418/callback"
	testVerifier = "dBjftJeZ4CVP-mJ92K9yUAPhaBYPKSVelM2TlVHKTyU1abcdef"
)

func newOAuthRig(t *testing.T) *oauthRig {
	t.Helper()
	dir := t.TempDir()
	users, err := OpenUserStore(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.CreateLocal("alice", "Alice", RolePlanner, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	sess, _ := NewSessions([]byte("0123456789abcdef0123456789abcdef"), time.Hour, false)
	store, err := OpenOAuthStore(filepath.Join(dir, "oauth.json"), []byte("fedcba9876543210fedcba9876543210"))
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{protected: true, tree: tgz(t, entry{name: "fleet.yml", body: "name: x\n"})}
	rig := &oauthRig{users: users, store: store}
	rig.applyRig = newApplyRigWith(t, func(o *Options) {
		o.Deploy, o.DeployTags = src, "deploy-*"
		o.Users, o.Sessions, o.OAuth = users, sess, store
	})
	jar, _ := cookiejar.New(nil)
	rig.browser = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return rig
}

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}

func (r *oauthRig) form(t *testing.T, client *http.Client, path string, v url.Values) (*http.Response, string) {
	t.Helper()
	res, err := client.PostForm(r.srv.URL+path, v)
	if err != nil {
		t.Fatal(err)
	}
	return res, readBody(t, res)
}

func (r *oauthRig) register(t *testing.T) {
	t.Helper()
	res, body := r.post(t, "/oauth/register", "", "application/json", []byte(`{"client_name":"Test Agent","redirect_uris":["`+testRedirect+`"],"token_endpoint_auth_method":"none"}`))
	var out struct {
		ClientID string `json:"client_id"`
		Method   string `json:"token_endpoint_auth_method"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if res.StatusCode != http.StatusCreated || !strings.HasPrefix(out.ClientID, "nopc_") || out.Method != "none" {
		t.Fatalf("register: %d %s", res.StatusCode, body)
	}
	r.clientID = out.ClientID
}

func (r *oauthRig) authorizeURL(state string) string {
	q := url.Values{"response_type": {"code"}, "client_id": {r.clientID}, "redirect_uri": {testRedirect}, "state": {state},
		"code_challenge": {pkceS256(testVerifier)}, "code_challenge_method": {"S256"}, "resource": {r.srv.URL + "/mcp"}}
	return "/oauth/authorize?" + q.Encode()
}

var hiddenRe = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)

// signIn signs alice in through the authorize page's local form.
func (r *oauthRig) signIn(t *testing.T) {
	t.Helper()
	res, body := r.form(t, r.browser, "/oauth/login", url.Values{"username": {"alice"}, "password": {"correct horse battery"}, "next": {r.authorizeURL("s")}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != r.authorizeURL("s") {
		t.Fatalf("login: %d %s %s", res.StatusCode, res.Header.Get("Location"), body)
	}
}

// consent opens the consent page and answers it; it returns where the browser is sent.
func (r *oauthRig) consent(t *testing.T, state, decision string) *url.URL {
	t.Helper()
	res, err := r.browser.Get(r.srv.URL + r.authorizeURL(state))
	if err != nil {
		t.Fatal(err)
	}
	page := readBody(t, res)
	if res.StatusCode != 200 || !strings.Contains(page, "Test Agent") || !strings.Contains(page, "planner") {
		t.Fatalf("consent page: %d %s", res.StatusCode, page)
	}
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("the consent page must not be framed")
	}
	v := url.Values{"decision": {decision}}
	for _, m := range hiddenRe.FindAllStringSubmatch(page, -1) {
		v.Add(m[1], html.UnescapeString(m[2]))
	}
	res, body := r.form(t, r.browser, "/oauth/authorize", v)
	if res.StatusCode != http.StatusFound {
		t.Fatalf("consent: %d %s", res.StatusCode, body)
	}
	u, _ := url.Parse(res.Header.Get("Location"))
	return u
}

type tokenAnswer struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Type    string `json:"token_type"`
	Expires int    `json:"expires_in"`
	Error   string `json:"error"`
}

func (r *oauthRig) token(t *testing.T, v url.Values) (int, tokenAnswer) {
	t.Helper()
	v.Set("client_id", r.clientID)
	res, body := r.form(t, http.DefaultClient, "/oauth/token", v)
	var out tokenAnswer
	_ = json.Unmarshal([]byte(body), &out)
	return res.StatusCode, out
}

func (r *oauthRig) codeFor(t *testing.T) string {
	t.Helper()
	u := r.consent(t, "xyz", "allow")
	if u.Host != "127.0.0.1:33418" || u.Query().Get("state") != "xyz" || u.Query().Get("code") == "" {
		t.Fatalf("the code goes to the registered redirect with the state: %s", u)
	}
	return u.Query().Get("code")
}

func (r *oauthRig) exchange(t *testing.T, code string) tokenAnswer {
	t.Helper()
	code2, tok := r.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect}, "code_verifier": {testVerifier}, "resource": {r.srv.URL + "/mcp"}})
	if code2 != 200 || !strings.HasPrefix(tok.Access, oauthAccessPrefix) || !strings.HasPrefix(tok.Refresh, oauthRefreshPrefix) || tok.Type != "Bearer" || tok.Expires != 3600 {
		t.Fatalf("token: %d %+v", code2, tok)
	}
	return tok
}

func hasTool(tools []string, name string) bool {
	for _, t := range tools {
		if t == name {
			return true
		}
	}
	return false
}

func TestAnMCPClientDiscoversTheSignInFromA401(t *testing.T) {
	rig := newOAuthRig(t)
	res, _ := rig.mcp(t, "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	want := `resource_metadata="` + rig.srv.URL + `/.well-known/oauth-protected-resource"`
	if res.StatusCode != 401 || !strings.Contains(res.Header.Get("WWW-Authenticate"), want) {
		t.Fatalf("401: %d %q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		res, body := rig.do(t, "GET", p, "")
		var prm struct {
			Resource string   `json:"resource"`
			AS       []string `json:"authorization_servers"`
		}
		_ = json.Unmarshal([]byte(body), &prm)
		if res.StatusCode != 200 || prm.Resource != rig.srv.URL+"/mcp" || len(prm.AS) != 1 || prm.AS[0] != rig.srv.URL || res.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s: %d %s", p, res.StatusCode, body)
		}
	}
	_, body := rig.do(t, "GET", "/.well-known/oauth-authorization-server", "")
	var as map[string]any
	_ = json.Unmarshal([]byte(body), &as)
	if as["issuer"] != rig.srv.URL || as["registration_endpoint"] != rig.srv.URL+"/oauth/register" ||
		!strings.Contains(body, `"S256"`) || !strings.Contains(body, `"none"`) {
		t.Fatalf("authorization server metadata: %s", body)
	}
}

func TestTheOAuthFlowGivesAnAgentItsPersonsRoleAtMCPOnly(t *testing.T) {
	rig := newOAuthRig(t)
	rig.register(t)

	// Not signed in: the page offers sign-in and returns to the request.
	res, err := rig.browser.Get(rig.srv.URL + rig.authorizeURL("s"))
	if err != nil {
		t.Fatal(err)
	}
	if page := readBody(t, res); res.StatusCode != 200 || !strings.Contains(page, `action="/oauth/login"`) || !strings.Contains(page, "Test Agent") {
		t.Fatalf("sign-in page: %d %s", res.StatusCode, page)
	}
	rig.signIn(t)

	// A wrong verifier spends the code; the right one with a fresh code gets tokens; a code works once.
	code := rig.codeFor(t)
	if c, tok := rig.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect}, "code_verifier": {strings.Repeat("x", 43)}}); c != 400 || tok.Error != "invalid_grant" {
		t.Fatalf("a wrong verifier: %d %+v", c, tok)
	}
	if c, _ := rig.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect}, "code_verifier": {testVerifier}}); c != 400 {
		t.Fatal("a code is spent by a failed exchange")
	}
	code = rig.codeFor(t)
	tok := rig.exchange(t, code)
	if c, _ := rig.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect}, "code_verifier": {testVerifier}}); c != 400 {
		t.Fatal("a code works once")
	}

	// The agent is alice, a planner: it can plan, not deploy.
	if tools := rig.mcpTools(t, tok.Access); !hasTool(tools, "plan_deploy") || hasTool(tools, "deploy") {
		t.Fatalf("tools for a planner: %v", tools)
	}
	who, _ := rig.callTool(t, tok.Access, "whoami", `{}`)
	if who.StructuredContent["name"] != "alice (via Test Agent)" || who.StructuredContent["role"] != "planner" {
		t.Fatalf("whoami: %+v", who.StructuredContent)
	}
	plan, _ := rig.callTool(t, tok.Access, "plan_deploy", `{"tag":"deploy-1"}`)
	if plan.IsError {
		t.Fatalf("plan_deploy as alice: %+v", plan)
	}

	// The token is for /mcp only.
	if res, _ := rig.do(t, "GET", "/v1/status", tok.Access); res.StatusCode != 401 {
		t.Fatalf("an access token used on /v1: %d", res.StatusCode)
	}

	// Her role, as it is now: raised, the agent can deploy; disabled, it is shut out.
	_ = rig.users.SetRole("alice", RoleDeployer)
	if tools := rig.mcpTools(t, tok.Access); !hasTool(tools, "deploy") {
		t.Fatalf("after a role change: %v", tools)
	}
	_ = rig.users.SetDisabled("alice", true)
	if res, _ := rig.mcp(t, tok.Access, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); res.StatusCode != 401 || !strings.Contains(res.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("a disabled person's agent: %d", res.StatusCode)
	}
	if c, _ := rig.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.Refresh}}); c != 400 {
		t.Fatal("no new tokens for a disabled person")
	}
}

func TestRefreshTokensRotateAndAReplayRevokesTheGrant(t *testing.T) {
	rig := newOAuthRig(t)
	rig.register(t)
	rig.signIn(t)
	first := rig.exchange(t, rig.codeFor(t))

	c, second := rig.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.Refresh}})
	if c != 200 || second.Refresh == first.Refresh || second.Access == "" {
		t.Fatalf("refresh: %d %+v", c, second)
	}
	if res, _ := rig.mcp(t, second.Access, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); res.StatusCode != 200 {
		t.Fatalf("the new access token: %d", res.StatusCode)
	}
	// The spent refresh token again: someone has a copy, so the whole grant goes.
	if c, _ := rig.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.Refresh}}); c != 400 {
		t.Fatal("a spent refresh token must be refused")
	}
	if res, _ := rig.mcp(t, second.Access, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); res.StatusCode != 401 {
		t.Fatalf("after a replay the grant is revoked: %d", res.StatusCode)
	}
	if len(rig.store.Grants()) != 0 {
		t.Fatal("the grant must be gone")
	}
}

func TestAdminsSeeAndRevokeGrantsAndClientsCanRevokeTheirOwn(t *testing.T) {
	rig := newOAuthRig(t)
	rig.register(t)
	rig.signIn(t)
	tok := rig.exchange(t, rig.codeFor(t))

	if res, _ := rig.do(t, "GET", "/v1/oauth/grants", rig.viewer); res.StatusCode != 403 {
		t.Fatalf("a viewer listing grants: %d", res.StatusCode)
	}
	res, body := rig.do(t, "GET", "/v1/oauth/grants", rig.secret)
	var out struct {
		Grants []map[string]any `json:"grants"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if res.StatusCode != 200 || len(out.Grants) != 1 || out.Grants[0]["username"] != "alice" || out.Grants[0]["client_name"] != "Test Agent" || strings.Contains(body, "hash") {
		t.Fatalf("grants: %d %s", res.StatusCode, body)
	}
	id := out.Grants[0]["id"].(string)
	if res, _ := rig.do(t, "DELETE", "/v1/oauth/grants/"+id, rig.secret); res.StatusCode != 200 {
		t.Fatalf("revoke: %d", res.StatusCode)
	}
	if res, _ := rig.mcp(t, tok.Access, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); res.StatusCode != 401 {
		t.Fatalf("a revoked grant's token: %d", res.StatusCode)
	}

	// RFC 7009: the client ends its own grant with its refresh token.
	tok = rig.exchange(t, rig.codeFor(t))
	if res, _ := rig.form(t, http.DefaultClient, "/oauth/revoke", url.Values{"token": {tok.Refresh}, "client_id": {rig.clientID}}); res.StatusCode != 200 {
		t.Fatalf("revoke endpoint: %d", res.StatusCode)
	}
	if res, _ := rig.mcp(t, tok.Access, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); res.StatusCode != 401 {
		t.Fatal("revoking the refresh token ends the grant")
	}
}

func TestTheAuthorizeEndpointRefusesWhatCouldLeakACode(t *testing.T) {
	rig := newOAuthRig(t)
	rig.register(t)

	for _, bad := range []string{`"http://evil.example/cb"`, `"javascript:alert(1)"`, `"http://127.0.0.1.evil.example/cb"`, `"https://x/cb#frag"`} {
		if res, body := rig.post(t, "/oauth/register", "", "application/json", []byte(`{"redirect_uris":[`+bad+`]}`)); res.StatusCode != 400 || !strings.Contains(body, "invalid_redirect_uri") {
			t.Errorf("redirect %s: %d %s", bad, res.StatusCode, body)
		}
	}

	// An unregistered redirect is shown to the person, never redirected to.
	q := url.Values{"response_type": {"code"}, "client_id": {rig.clientID}, "redirect_uri": {"http://127.0.0.1:9/other"},
		"code_challenge": {pkceS256(testVerifier)}, "code_challenge_method": {"S256"}}
	res, err := rig.browser.Get(rig.srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if readBody(t, res); res.StatusCode != 400 || res.Header.Get("Location") != "" {
		t.Fatalf("unregistered redirect: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// No PKCE: the error goes back to the (registered) client.
	q.Set("redirect_uri", testRedirect)
	q.Del("code_challenge")
	res, _ = rig.browser.Get(rig.srv.URL + "/oauth/authorize?" + q.Encode())
	readBody(t, res)
	if loc, _ := url.Parse(res.Header.Get("Location")); res.StatusCode != 302 || loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("no PKCE: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	// A consent without the page's token (another site submitting it) is refused, even signed in.
	rig.signIn(t)
	v := url.Values{"client_id": {rig.clientID}, "redirect_uri": {testRedirect}, "response_type": {"code"},
		"code_challenge": {pkceS256(testVerifier)}, "code_challenge_method": {"S256"}, "decision": {"allow"}}
	if res, _ := rig.form(t, rig.browser, "/oauth/authorize", v); res.StatusCode != 403 {
		t.Fatalf("a forged consent: %d", res.StatusCode)
	}
	if u := rig.consent(t, "st", "deny"); u.Query().Get("error") != "access_denied" || u.Query().Get("state") != "st" {
		t.Fatalf("deny: %s", u)
	}
	if res, _ := rig.form(t, rig.browser, "/oauth/login", url.Values{"username": {"alice"}, "password": {"correct horse battery"}, "next": {"https://evil.example/"}}); res.StatusCode != 400 {
		t.Fatalf("a sign-in may only return to an authorize request: %d", res.StatusCode)
	}
	if c, tok := rig.token(t, url.Values{"grant_type": {"password"}}); c != 400 || tok.Error != "unsupported_grant_type" {
		t.Fatalf("grant type: %d %+v", c, tok)
	}
}

func TestSafeNextOnlyAllowsAnAuthorizeRequestHere(t *testing.T) {
	for next, want := range map[string]bool{
		"/oauth/authorize?client_id=x":           true,
		"/oauth/authorize":                       false,
		"//evil.example/oauth/authorize?x":       false,
		"https://evil.example/oauth/authorize?x": false,
		"/oauth/authorize/../x?y":                false,
		"/oauth/authorize?\r\nx":                 false,
		"/\\evil.example?x":                      false,
	} {
		if got := safeNext(next); got != want {
			t.Errorf("safeNext(%q) = %v, want %v", next, got, want)
		}
	}
}

func TestOAuthNeedsSignIn(t *testing.T) {
	store, _ := OpenOAuthStore(filepath.Join(t.TempDir(), "o.json"), make([]byte, 32))
	tokens, _ := OpenTokenStore(filepath.Join(t.TempDir(), "t.json"))
	audit, _ := OpenAudit(filepath.Join(t.TempDir(), "a.log"))
	defer audit.Close()
	if _, err := New(Options{Tokens: tokens, Audit: audit, OAuth: store, Status: func(context.Context) (*status.Snapshot, error) { return &status.Snapshot{}, nil }}); err == nil {
		t.Fatal("OAuth without sign-in must be refused")
	}
}
