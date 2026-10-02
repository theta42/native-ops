package server

import (
	"net/http"
	"strings"
)

// The admin endpoints manage the daemon's own credentials over the API, so that nobody needs a shell on
// the host to give CI a token or a person an account: the bootstrap admin token (provisioned with the
// host) is enough to set up everything else. They need the admin role and refuse a scoped token, like
// every other non-instance endpoint.
//
//	GET    /v1/tokens               the tokens (never a secret or a hash)
//	POST   /v1/tokens               {name, role, scope?} -> the new token and its secret, shown once
//	DELETE /v1/tokens/{id}          revoke a token
//	GET    /v1/users                the users (never a password hash)
//	POST   /v1/users                {username, name?, role, password} -> a local user
//	PATCH  /v1/users/{username}     {role?, disabled?, password?}

// handleTokenList is GET /v1/tokens.
func (s *Server) handleTokenList(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.opts.Tokens.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not read the token store")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// handleTokenCreate is POST /v1/tokens.
func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		Role  Role   `json:"role"`
		Scope *Scope `json:"scope,omitempty"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	scope := Scope{}
	if body.Scope != nil {
		scope = *body.Scope
	}
	secret, tok, err := s.opts.Tokens.CreateScoped(body.Name, body.Role, scope)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	tok.Hash = ""
	auditDetail(r, "token created id=%s name=%q role=%s scoped=%t", tok.ID, tok.Name, tok.Role, tok.Scoped())
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":  tok,
		"secret": secret,
		"note":   "the secret is shown only now: store it (e.g. as a CI secret) before closing this",
	})
}

// handleTokenRevoke is DELETE /v1/tokens/{id}.
func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.opts.Tokens.Revoke(id); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such token")
		return
	}
	auditDetail(r, "token revoked id=%s", id)
	writeJSON(w, http.StatusOK, map[string]any{"revoked": id})
}

// handleUserList is GET /v1/users.
func (s *Server) handleUserList(w http.ResponseWriter, r *http.Request) {
	users, err := s.opts.Users.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not read the user store")
		return
	}
	out := make([]map[string]any, len(users))
	for i, u := range users {
		v := PublicUser(u)
		v["email"], v["disabled"], v["created"], v["last_login"] = u.Email, u.Disabled, u.Created, u.LastLogin
		out[i] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// handleUserCreate is POST /v1/users: a local user with a password.
func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Name     string `json:"name"`
		Role     Role   `json:"role"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	u, err := s.opts.Users.CreateLocal(body.Username, body.Name, body.Role, body.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	auditDetail(r, "user created %s role=%s", u.Username, u.Role)
	writeJSON(w, http.StatusCreated, map[string]any{"user": PublicUser(u)})
}

// handleUserUpdate is PATCH /v1/users/{username}. An admin may not demote or disable the account they
// are signed in with, so the last way in is never closed by accident.
func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	username := strings.ToLower(strings.TrimSpace(r.PathValue("username")))
	var body struct {
		Role     *Role   `json:"role,omitempty"`
		Disabled *bool   `json:"disabled,omitempty"`
		Password *string `json:"password,omitempty"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Role == nil && body.Disabled == nil && body.Password == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "say what to change: role, disabled or password")
		return
	}
	if _, found, err := s.opts.Users.Get(username); err != nil || !found {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	if self, _, ok := s.sessionUser(r); ok && self == username &&
		((body.Role != nil && *body.Role != RoleAdmin) || (body.Disabled != nil && *body.Disabled)) {
		writeError(w, http.StatusConflict, "self", "you cannot demote or disable the account you are signed in with")
		return
	}
	var changed []string
	apply := func(what string, err error) bool {
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return false
		}
		changed = append(changed, what)
		return true
	}
	if body.Role != nil && !apply("role="+string(*body.Role), s.opts.Users.SetRole(username, *body.Role)) {
		return
	}
	if body.Disabled != nil {
		what := "enabled"
		if *body.Disabled {
			what = "disabled"
		}
		if !apply(what, s.opts.Users.SetDisabled(username, *body.Disabled)) {
			return
		}
	}
	if body.Password != nil && !apply("password", s.opts.Users.SetPassword(username, *body.Password)) {
		return
	}
	auditDetail(r, "user %s changed: %s", username, strings.Join(changed, ", "))
	u, _, err := s.opts.Users.Get(username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "changed, but could not read the user back")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": PublicUser(u), "disabled": u.Disabled})
}

// sessionUser is who is signed in on this request with a session cookie, if anyone. authWith gives a
// session precedence over a token, so this is the account the request acts as.
func (s *Server) sessionUser(r *http.Request) (string, Role, bool) {
	if s.opts.Sessions == nil {
		return "", "", false
	}
	return s.currentSession(r)
}
