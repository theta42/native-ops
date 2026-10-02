package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
)

const oidcStateCookie = "nops_oidc_state"

// handleSession tells the UI how to sign someone in and, if they already are, who they are. It is
// public (it carries no host data), so the SPA can decide between a login form and the app.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"local_signin": s.opts.Users != nil,
		"oidc":         s.opts.OIDC != nil,
	}
	if s.opts.OIDC != nil {
		out["oidc_label"] = s.opts.OIDC.Label
	}
	if s.opts.Sessions != nil {
		if username, role, ok := s.currentSession(r); ok {
			out["user"] = map[string]any{"username": username, "role": role}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleLogin signs a local user in and sets the session cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.opts.Users == nil {
		writeError(w, http.StatusBadRequest, "local_signin_off", "local sign-in is not enabled on this daemon")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if wait := s.logins.Wait(body.Username); wait > 0 {
		s.setActor(r, body.Username)
		auditDetail(r, "sign-in refused: too many failed attempts")
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed sign-ins for this account; wait a little and try again")
		return
	}
	u, err := s.opts.Users.Authenticate(body.Username, body.Password)
	if err != nil {
		s.logins.Fail(body.Username)
		s.setActor(r, body.Username)
		auditDetail(r, "sign-in failed")
		writeError(w, http.StatusUnauthorized, "bad_login", "wrong username or password")
		return
	}
	s.logins.Succeed(body.Username)
	s.opts.Sessions.SetCookie(w, r, s.opts.Sessions.Issue(u.Username, u.Role))
	_ = s.opts.Users.TouchLogin(u.Username)
	s.setActor(r, u.Username)
	auditDetail(r, "signed in")
	writeJSON(w, http.StatusOK, map[string]any{"user": PublicUser(u)})
}

// handleLogout clears the session cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.opts.Sessions != nil {
		s.opts.Sessions.ClearCookie(w, r)
	}
	if username, _, ok := s.opts.Sessions.FromRequest(r); ok {
		s.setActor(r, username)
		auditDetail(r, "signed out")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleOIDCStart sends the browser to the identity provider with a random state in a short-lived
// cookie (the CSRF check for the callback).
func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not start the sign-in")
		return
	}
	state := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookie, Value: state, Path: "/auth/oidc", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r), MaxAge: 600,
	})
	target, err := s.opts.OIDC.AuthorizeURL(r.Context(), state)
	if err != nil {
		writeError(w, http.StatusBadGateway, "oidc", "could not reach the identity provider")
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// handleOIDCCallback finishes the OIDC flow: check the state, exchange the code, read the identity from
// the provider, find-or-create the matching user, and set the session cookie.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	clearState := &http.Cookie{
		Name: oidcStateCookie, Value: "", Path: "/auth/oidc", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r), MaxAge: -1,
	}
	fail := func(msg string) {
		http.SetCookie(w, clearState)
		http.Redirect(w, r, "/?error="+url.QueryEscape(msg), http.StatusFound)
	}
	if s.opts.Users == nil {
		fail("no user store is configured")
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	cookie, err := r.Cookie(oidcStateCookie)
	if err != nil || state == "" || state != cookie.Value {
		fail("invalid state, try again")
		return
	}
	if code == "" {
		fail("sign-in was cancelled")
		return
	}
	access, err := s.opts.OIDC.Exchange(r.Context(), code)
	if err != nil {
		fail("could not complete the sign-in")
		return
	}
	id, err := s.opts.OIDC.Identity(r.Context(), access)
	if err != nil {
		fail(err.Error())
		return
	}
	u, err := s.opts.Users.FindOrCreateOIDC(id.Email, id.Name, s.opts.OIDC.Role)
	if err != nil {
		fail("could not record the user")
		return
	}
	if u.Disabled {
		fail("this account is disabled")
		return
	}
	s.opts.Sessions.SetCookie(w, r, s.opts.Sessions.Issue(u.Username, u.Role))
	_ = s.opts.Users.TouchLogin(u.Username)
	http.SetCookie(w, clearState)
	s.setActor(r, u.Username)
	auditDetail(r, "signed in via oidc")
	http.Redirect(w, r, "/", http.StatusFound)
}

// setActor records who acted for the audit line (the middleware reads it after the handler returns).
func (s *Server) setActor(r *http.Request, name string) {
	if h, ok := r.Context().Value(actorKey{}).(*actorHolder); ok {
		h.name = name
	}
}
