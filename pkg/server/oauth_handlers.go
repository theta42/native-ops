package server

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The OAuth endpoints (see oauth.go):
//
//	GET  /.well-known/oauth-protected-resource[/mcp]   RFC 9728: /mcp is protected by this daemon
//	GET  /.well-known/oauth-authorization-server       RFC 8414: where to register, sign in, get tokens
//	POST /oauth/register                               RFC 7591: dynamic client registration (public clients)
//	GET  /oauth/authorize                              sign in (the UI's sign-in), then a consent page
//	POST /oauth/authorize                              the consent: a code back to the client
//	POST /oauth/login                                  local sign-in from the authorize page
//	POST /oauth/token                                  code (with PKCE) or refresh token -> tokens
//	POST /oauth/revoke                                 RFC 7009: end a grant by its refresh token
//
// The browser pages carry no script; the consent form carries a signed, short-lived token tied to the
// signed-in person and the exact request, so another site cannot submit a consent for them.

// publicBase is the daemon's public origin: --public-url, else what the request came in on.
func (s *Server) publicBase(r *http.Request) string {
	if s.opts.PublicURL != "" {
		return strings.TrimRight(s.opts.PublicURL, "/")
	}
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) mcpResource(r *http.Request) string { return s.publicBase(r) + "/mcp" }

// mcpChallenge is /mcp's WWW-Authenticate: with OAuth on, it names the resource metadata, which is how an
// MCP client finds out where to send its person to sign in.
func (s *Server) mcpChallenge(r *http.Request, oauthErr string) string {
	h := `Bearer realm="native-ops"`
	if s.opts.OAuth != nil {
		h += `, resource_metadata="` + s.publicBase(r) + `/.well-known/oauth-protected-resource"`
		if oauthErr != "" {
			h += `, error="` + oauthErr + `"`
		}
	}
	return h
}

func cors(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
}

// oauthPublic wraps the endpoints a client calls directly: they take no cookies, so any origin may.
func oauthPublic(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func oauthError(w http.ResponseWriter, code int, kind, desc string) {
	writeJSON(w, code, map[string]string{"error": kind, "error_description": desc})
}

func (s *Server) handleProtectedResource(w http.ResponseWriter, r *http.Request) {
	base := s.publicBase(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{oauthScope},
		"resource_name":            "native-ops",
		"resource_documentation":   "https://github.com/theta42/native-ops/blob/main/docs/api.md#mcp",
	})
}

func (s *Server) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	base := s.publicBase(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                     base,
		"authorization_endpoint":                     base + "/oauth/authorize",
		"token_endpoint":                             base + "/oauth/token",
		"registration_endpoint":                      base + "/oauth/register",
		"revocation_endpoint":                        base + "/oauth/revoke",
		"response_types_supported":                   []string{"code"},
		"grant_types_supported":                      []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":           []string{"S256"},
		"token_endpoint_auth_methods_supported":      []string{"none"},
		"revocation_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                           []string{oauthScope},
	})
}

func (s *Server) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(&body); err != nil { // unknown metadata is ignored, as RFC 7591 allows
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body must be JSON client metadata")
		return
	}
	c, err := s.opts.OAuth.RegisterClient(body.ClientName, body.RedirectURIs)
	if err != nil {
		kind := "invalid_client_metadata"
		if strings.Contains(err.Error(), "redirect") {
			kind = "invalid_redirect_uri"
		}
		oauthError(w, http.StatusBadRequest, kind, err.Error())
		return
	}
	auditDetail(r, "oauth client registered id=%s name=%q", c.ID, c.Name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        c.Created.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      oauthScope,
	})
}

// authzRequest is an authorization request's parameters, from the query (GET) or the consent form (POST).
type authzRequest struct {
	ClientID, RedirectURI, State, Challenge, Method, Resource, ResponseType string
}

func readAuthz(v url.Values) authzRequest {
	return authzRequest{ClientID: v.Get("client_id"), RedirectURI: v.Get("redirect_uri"), State: v.Get("state"),
		Challenge: v.Get("code_challenge"), Method: v.Get("code_challenge_method"), Resource: v.Get("resource"),
		ResponseType: v.Get("response_type")}
}

func (a authzRequest) query() url.Values {
	q := url.Values{"client_id": {a.ClientID}, "redirect_uri": {a.RedirectURI}, "response_type": {a.ResponseType},
		"code_challenge": {a.Challenge}, "code_challenge_method": {a.Method}}
	if a.State != "" {
		q.Set("state", a.State)
	}
	if a.Resource != "" {
		q.Set("resource", a.Resource)
	}
	return q
}

// redirectError sends an error back to the client's redirect URI (RFC 6749 4.1.2.1).
func redirectError(w http.ResponseWriter, r *http.Request, a authzRequest, kind, desc string) {
	q := url.Values{"error": {kind}, "error_description": {desc}}
	if a.State != "" {
		q.Set("state", a.State)
	}
	http.Redirect(w, r, withQuery(a.RedirectURI, q), http.StatusFound)
}

func withQuery(raw string, q url.Values) string {
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	return raw + sep + q.Encode()
}

// checkAuthz validates a request. A bad client or redirect URI is shown to the person (a code must never
// go to a URI the client did not register); anything else goes back to the client.
func (s *Server) checkAuthz(w http.ResponseWriter, r *http.Request, a authzRequest) (OAuthClient, bool) {
	c, ok := s.opts.OAuth.Client(a.ClientID)
	if !ok {
		s.oauthPage(w, http.StatusBadRequest, pageData{Title: "Unknown client", Error: "This sign-in link names a client this daemon does not know. Start again from your app."})
		return c, false
	}
	registered := false
	for _, u := range c.RedirectURIs {
		registered = registered || u == a.RedirectURI
	}
	if !registered {
		s.oauthPage(w, http.StatusBadRequest, pageData{Title: "Wrong redirect", Error: "This sign-in link would send you somewhere the client did not register. Start again from your app."})
		return c, false
	}
	switch {
	case a.ResponseType != "code":
		redirectError(w, r, a, "unsupported_response_type", "only response_type=code is supported")
	case a.Challenge == "" || a.Method != "S256":
		redirectError(w, r, a, "invalid_request", "PKCE is required: code_challenge with code_challenge_method=S256")
	case a.Resource != "" && strings.TrimRight(a.Resource, "/") != s.mcpResource(r):
		redirectError(w, r, a, "invalid_target", "resource must be "+s.mcpResource(r))
	default:
		return c, true
	}
	return c, false
}

const consentTTL = 10 * time.Minute

// consentToken ties a consent form to the person and the exact request, for consentTTL.
func (s *Server) consentToken(username string, a authzRequest, at time.Time) string {
	t := strconv.FormatInt(at.Unix(), 10)
	return t + "." + s.opts.OAuth.mac("consent\x00"+username+"\x00"+a.query().Encode()+"\x00"+t)
}

func (s *Server) validConsent(token, username string, a authzRequest) bool {
	t, _, ok := strings.Cut(token, ".")
	n, err := strconv.ParseInt(t, 10, 64)
	if !ok || err != nil {
		return false
	}
	at := time.Unix(n, 0)
	if age := s.opts.OAuth.now().Sub(at); age < 0 || age > consentTTL {
		return false
	}
	return hmac.Equal([]byte(token), []byte(s.consentToken(username, a, at)))
}

func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	a := readAuthz(r.URL.Query())
	client, ok := s.checkAuthz(w, r, a)
	if !ok {
		return
	}
	username, role, signedIn := s.currentSession(r)
	if !signedIn {
		next := "/oauth/authorize?" + a.query().Encode()
		d := pageData{Title: "Sign in to native-ops", Client: client.Name, Next: next, Local: s.opts.Users != nil}
		if s.opts.OIDC != nil {
			d.OIDCLabel, d.OIDCURL = s.opts.OIDC.Label, "/auth/oidc?next="+url.QueryEscape(next)
		}
		s.oauthPage(w, http.StatusOK, d)
		return
	}
	s.oauthPage(w, http.StatusOK, pageData{Title: "Allow access?", Client: client.Name, User: username, Role: string(role),
		Host: s.publicBase(r), Fields: a.query(), Consent: s.consentToken(username, a, s.opts.OAuth.now())})
}

func (s *Server) handleOAuthConsent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.oauthPage(w, http.StatusBadRequest, pageData{Title: "Bad request", Error: "The form could not be read."})
		return
	}
	a := readAuthz(r.PostForm)
	client, ok := s.checkAuthz(w, r, a)
	if !ok {
		return
	}
	username, _, signedIn := s.currentSession(r)
	if !signedIn || !s.validConsent(r.PostForm.Get("consent"), username, a) {
		s.oauthPage(w, http.StatusForbidden, pageData{Title: "Start again", Error: "This consent form has expired or was not yours. Start again from your app."})
		return
	}
	s.setActor(r, username)
	if r.PostForm.Get("decision") != "allow" {
		auditDetail(r, "oauth consent denied client=%s", client.ID)
		redirectError(w, r, a, "access_denied", "the person declined")
		return
	}
	resource := a.Resource
	if resource == "" {
		resource = s.mcpResource(r)
	}
	code, err := s.opts.OAuth.NewCode(oauthCode{ClientID: client.ID, RedirectURI: a.RedirectURI, Challenge: a.Challenge, Username: username, Resource: resource})
	if err != nil {
		redirectError(w, r, a, "server_error", "could not issue a code")
		return
	}
	auditDetail(r, "oauth consent given client=%s name=%q", client.ID, client.Name)
	q := url.Values{"code": {code}}
	if a.State != "" {
		q.Set("state", a.State)
	}
	http.Redirect(w, r, withQuery(a.RedirectURI, q), http.StatusFound)
}

// safeNext reports whether next is an authorize request on this daemon: the only place a sign-in for
// an OAuth client may return to.
func safeNext(next string) bool {
	u, err := url.Parse(next)
	return err == nil && u.Scheme == "" && u.Host == "" && u.Path == "/oauth/authorize" && strings.HasPrefix(next, "/oauth/authorize?") &&
		!strings.ContainsAny(next, "\\\r\n")
}

// handleOAuthLogin is local sign-in from the authorize page: a plain form, then back to the request.
func (s *Server) handleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.oauthPage(w, http.StatusBadRequest, pageData{Title: "Bad request", Error: "The form could not be read."})
		return
	}
	next, username, password := r.PostForm.Get("next"), r.PostForm.Get("username"), r.PostForm.Get("password")
	if !safeNext(next) || s.opts.Users == nil {
		s.oauthPage(w, http.StatusBadRequest, pageData{Title: "Start again", Error: "This sign-in cannot continue. Start again from your app."})
		return
	}
	retry := pageData{Title: "Sign in to native-ops", Next: next, Local: true}
	if s.opts.OIDC != nil {
		retry.OIDCLabel, retry.OIDCURL = s.opts.OIDC.Label, "/auth/oidc?next="+url.QueryEscape(next)
	}
	if wait := s.logins.Wait(username); wait > 0 {
		s.setActor(r, username)
		auditDetail(r, "sign-in refused: too many failed attempts")
		retry.Error = "Too many failed sign-ins for this account. Wait a little and try again."
		s.oauthPage(w, http.StatusTooManyRequests, retry)
		return
	}
	u, err := s.opts.Users.Authenticate(username, password)
	if err != nil {
		s.logins.Fail(username)
		s.setActor(r, username)
		auditDetail(r, "sign-in failed")
		retry.Error = "Wrong username or password."
		s.oauthPage(w, http.StatusUnauthorized, retry)
		return
	}
	s.logins.Succeed(username)
	s.opts.Sessions.SetCookie(w, r, s.opts.Sessions.Issue(u.Username, u.Role))
	_ = s.opts.Users.TouchLogin(u.Username)
	s.setActor(r, u.Username)
	auditDetail(r, "signed in (oauth)")
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body must be application/x-www-form-urlencoded")
		return
	}
	f := r.PostForm
	clientID := f.Get("client_id")
	if _, ok := s.opts.OAuth.Client(clientID); !ok {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client_id")
		return
	}
	if res := f.Get("resource"); res != "" && strings.TrimRight(res, "/") != s.mcpResource(r) {
		oauthError(w, http.StatusBadRequest, "invalid_target", "resource must be "+s.mcpResource(r))
		return
	}
	var (
		g               OAuthGrant
		access, refresh string
		err             error
	)
	switch f.Get("grant_type") {
	case "authorization_code":
		g, access, refresh, err = s.opts.OAuth.ExchangeCode(f.Get("code"), clientID, f.Get("redirect_uri"), f.Get("code_verifier"))
	case "refresh_token":
		g, access, refresh, err = s.opts.OAuth.Refresh(f.Get("refresh_token"), clientID)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
		return
	}
	if errors.Is(err, ErrOAuthGrant) {
		auditDetail(r, "oauth token refused client=%s grant_type=%s", clientID, f.Get("grant_type"))
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code or refresh token is invalid, expired, used, or not this client's")
		return
	}
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	// The person must still be allowed in: a token is never issued for a disabled or removed user.
	if _, _, ok := s.oauthUser(g); !ok {
		_, _ = s.opts.OAuth.Revoke(g.ID)
		auditDetail(r, "oauth token refused: %s may no longer sign in", g.Username)
		oauthError(w, http.StatusBadRequest, "invalid_grant", "this account may no longer sign in")
		return
	}
	s.setActor(r, g.Username)
	auditDetail(r, "oauth token issued grant=%s client=%q grant_type=%s", g.ID, g.ClientName, f.Get("grant_type"))
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": int(oauthAccessTTL.Seconds()),
		"refresh_token": refresh, "scope": oauthScope,
	})
}

func (s *Server) handleOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body must be application/x-www-form-urlencoded")
		return
	}
	if t := r.PostForm.Get("token"); strings.HasPrefix(t, oauthRefreshPrefix) {
		_ = s.opts.OAuth.RevokeRefresh(t)
	} else if g, ok := s.opts.OAuth.VerifyAccess(t); ok {
		_, _ = s.opts.OAuth.Revoke(g.ID)
	}
	w.WriteHeader(http.StatusOK) // RFC 7009: the same answer whether or not the token was known
}

// oauthUser is the person a grant acts for, as the user store has them now.
func (s *Server) oauthUser(g OAuthGrant) (string, Role, bool) {
	if s.opts.Users == nil {
		return "", "", false
	}
	u, found, err := s.opts.Users.Get(g.Username)
	if err != nil || !found || u.Disabled || !ValidRole(u.Role) {
		return "", "", false
	}
	return u.Username, u.Role, true
}

// handleOAuthGrants is GET /v1/oauth/grants (admin): who has let which client act for them.
func (s *Server) handleOAuthGrants(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, g := range s.opts.OAuth.Grants() {
		out = append(out, PublicGrant(g))
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": out})
}

// handleOAuthGrantRevoke is DELETE /v1/oauth/grants/{id} (admin).
func (s *Server) handleOAuthGrantRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ok, err := s.opts.OAuth.Revoke(id)
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", "could not revoke the grant")
	case !ok:
		writeError(w, http.StatusNotFound, "not_found", "no such grant")
	default:
		auditDetail(r, "oauth grant revoked id=%s", id)
		writeJSON(w, http.StatusOK, map[string]any{"revoked": id})
	}
}

type pageData struct {
	Title, Error, Client, User, Role, Host, Next, OIDCLabel, OIDCURL, Consent string
	Local                                                                     bool
	Fields                                                                    url.Values
}

var oauthTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · native-ops</title>
<style>
:root{color-scheme:light dark;--bg:#f6f7f9;--fg:#1d2330;--card:#fff;--muted:#5b6475;--line:#d9dde4;--accent:#2563eb;--danger:#b42318}
@media (prefers-color-scheme:dark){:root{--bg:#11151c;--fg:#e6e9ef;--card:#1a2029;--muted:#9aa3b2;--line:#2c3440;--accent:#5b8def;--danger:#f97066}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif;display:grid;place-items:center;min-height:100vh;padding:16px}
main{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:28px;max-width:440px;width:100%}
h1{font-size:1.3rem;margin:0 0 12px}p{margin:0 0 14px}.muted{color:var(--muted);font-size:.92rem}.err{color:var(--danger)}
label{display:block;font-size:.9rem;margin:10px 0 4px}input[type=text],input[type=password]{width:100%;padding:9px 10px;border:1px solid var(--line);border-radius:8px;background:transparent;color:inherit;font:inherit}
button,a.btn{display:inline-block;padding:9px 16px;border-radius:8px;border:1px solid var(--line);background:transparent;color:inherit;font:inherit;cursor:pointer;text-decoration:none;margin:14px 8px 0 0}
.primary{background:var(--accent)!important;border-color:var(--accent)!important;color:#fff!important}ul{margin:0 0 14px;padding-left:20px}hr{border:0;border-top:1px solid var(--line);margin:18px 0}
</style></head><body><main>
<h1>{{.Title}}</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if .Consent}}
<p><strong>{{.Client}}</strong> wants to use native-ops at {{.Host}} as you, <strong>{{.User}}</strong>.</p>
<ul><li>It gets your role, <strong>{{.Role}}</strong>, and only through the MCP endpoint.</li>
<li>If your role changes or your account is disabled, its access follows at once.</li>
<li>Approving plans, tokens, users and secrets are never available to it.</li></ul>
<p class="muted">An admin can revoke this at any time (Tokens page).</p>
<form method="post" action="/oauth/authorize">
{{range $k, $v := .Fields}}{{range $v}}<input type="hidden" name="{{$k}}" value="{{.}}">{{end}}{{end}}
<input type="hidden" name="consent" value="{{.Consent}}">
<button class="primary" name="decision" value="allow">Allow</button><button name="decision" value="deny">Deny</button>
</form>
{{else if .Next}}
{{if .Client}}<p><strong>{{.Client}}</strong> wants to use native-ops as you. Sign in first.</p>{{end}}
{{if .OIDCURL}}<a class="btn primary" href="{{.OIDCURL}}">Sign in with {{.OIDCLabel}}</a>{{end}}
{{if .Local}}{{if .OIDCURL}}<hr>{{end}}
<form method="post" action="/oauth/login"><input type="hidden" name="next" value="{{.Next}}">
<label for="u">Username</label><input id="u" type="text" name="username" autocomplete="username" required>
<label for="p">Password</label><input id="p" type="password" name="password" autocomplete="current-password" required>
<button class="primary">Sign in</button></form>{{end}}
{{if not (or .OIDCURL .Local)}}<p class="err">No sign-in method is configured on this daemon.</p>{{end}}
{{end}}
</main></body></html>`))

func (s *Server) oauthPage(w http.ResponseWriter, code int, d pageData) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	// No form-action: the consent form's answer redirects to the client, which form-action would block.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	w.WriteHeader(code)
	_ = oauthTmpl.Execute(w, d)
}
