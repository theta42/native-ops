package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Sessions signs a small session cookie so a person who signs in -- locally or through OIDC -- can call
// the API with a cookie instead of holding a token. The cookie carries the username, role and expiry,
// HMAC-signed with a key kept in the state dir, so it is stateless: a restart does not end sessions, and
// nothing per-session is stored.
type Sessions struct {
	key    []byte
	ttl    time.Duration
	secure bool
}

const SessionCookie = "nops_session"

func NewSessions(key []byte, ttl time.Duration, secure bool) (*Sessions, error) {
	if len(key) < 16 {
		return nil, errors.New("a session key of at least 16 bytes is required")
	}
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &Sessions{key: key, ttl: ttl, secure: secure}, nil
}

func (s *Sessions) sign(b []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write(b)
	return m.Sum(nil)
}

type sessionPayload struct {
	Username string `json:"u"`
	Role     Role   `json:"r"`
	Exp      int64  `json:"e"`
}

// Issue makes a signed session token for a user and role.
func (s *Sessions) Issue(username string, role Role) string {
	p, _ := json.Marshal(sessionPayload{Username: username, Role: role, Exp: time.Now().Add(s.ttl).Unix()})
	body := base64.RawURLEncoding.EncodeToString(p)
	return body + "." + base64.RawURLEncoding.EncodeToString(s.sign([]byte(body)))
}

// Verify returns the username and role a token carries, or ok=false.
func (s *Sessions) Verify(token string) (string, Role, bool) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(raw, s.sign([]byte(body))) {
		return "", "", false
	}
	pb, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", "", false
	}
	var p sessionPayload
	if json.Unmarshal(pb, &p) != nil || p.Exp <= time.Now().Unix() || !ValidRole(p.Role) || p.Username == "" {
		return "", "", false
	}
	return p.Username, p.Role, true
}

// FromRequest reads and verifies the session cookie on a request.
func (s *Sessions) FromRequest(r *http.Request) (string, Role, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return "", "", false
	}
	return s.Verify(c.Value)
}

// SetCookie writes the session cookie. HttpOnly and SameSite=Lax: script cannot read it, and a
// cross-site POST does not carry it (so a cookie-authenticated mutation cannot be forged from a page).
func (s *Sessions) SetCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: s.secure || isHTTPS(r), SameSite: http.SameSiteLaxMode,
		MaxAge: int(s.ttl.Seconds()),
	})
}

// ClearCookie ends a session in the browser.
func (s *Sessions) ClearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: s.secure || isHTTPS(r), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// isHTTPS reports whether the client reached us over TLS, judging by the proxy headers the edge sets.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
