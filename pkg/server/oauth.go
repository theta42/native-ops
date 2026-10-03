package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The daemon is an OAuth 2.1 authorization server for its own MCP endpoint, so an agent connects by
// sending its person through a browser sign-in instead of holding a pasted API token (the MCP
// authorization spec: protected resource metadata, authorization server metadata, dynamic client
// registration, the authorization code flow with PKCE, refresh tokens).
//
// A grant is one person's consent for one client. Its access tokens act as that person: the role is the
// one the user store holds at each request (as for a UI session), so when a directory changes or removes
// someone's role, their agents follow at once. Access tokens are good only at /mcp.
//
// Clients are public (no secret; PKCE protects the code). Access tokens are signed and short-lived;
// refresh tokens are random, stored hashed, and rotate on every use: presenting an old one again
// revokes the grant, since only a copy could still have it.

const (
	oauthAccessPrefix  = "nopa_"
	oauthRefreshPrefix = "nopr_"
	oauthAccessTTL     = time.Hour
	oauthRefreshTTL    = 30 * 24 * time.Hour
	oauthCodeTTL       = time.Minute
	maxOAuthClients    = 500
	oauthScope         = "mcp"
)

// OAuthClient is a registered client (RFC 7591).
type OAuthClient struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirect_uris"`
	Created      time.Time `json:"created"`
}

// OAuthGrant is one person's consent for one client.
type OAuthGrant struct {
	ID         string    `json:"id"`
	ClientID   string    `json:"client_id"`
	ClientName string    `json:"client_name"`
	Username   string    `json:"username"`
	Resource   string    `json:"resource"`
	Created    time.Time `json:"created"`
	LastUsed   time.Time `json:"last_used"`
	// The current refresh token's hash, and the one before it (to notice a stolen copy being replayed).
	RefreshHash     string    `json:"refresh_hash"`
	PrevRefreshHash string    `json:"prev_refresh_hash,omitempty"`
	RefreshExpires  time.Time `json:"refresh_expires"`
}

// PublicGrant is a grant as the API shows it: no hashes.
func PublicGrant(g OAuthGrant) map[string]any {
	return map[string]any{"id": g.ID, "client_id": g.ClientID, "client_name": g.ClientName, "username": g.Username,
		"created": g.Created, "last_used": g.LastUsed, "expires": g.RefreshExpires}
}

type oauthCode struct {
	ClientID, RedirectURI, Challenge, Username, Resource string
	Expires                                              time.Time
}

// OAuthStore keeps clients and grants in a 0600 JSON file, and authorization codes in memory (they live
// a minute; a restart only means signing in again).
type OAuthStore struct {
	path string
	key  []byte
	now  func() time.Time

	mu      sync.Mutex
	clients []OAuthClient
	grants  []OAuthGrant
	codes   map[string]oauthCode
}

// OpenOAuthStore opens (or starts) the store at path. key signs access tokens (at least 32 bytes).
func OpenOAuthStore(path string, key []byte) (*OAuthStore, error) {
	if len(key) < 32 {
		return nil, errors.New("an OAuth signing key of at least 32 bytes is required")
	}
	s := &OAuthStore{path: path, key: key, now: time.Now, codes: map[string]oauthCode{}}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must not be readable by group or others (mode %o)", path, fi.Mode().Perm())
	}
	var f struct {
		Clients []OAuthClient `json:"clients"`
		Grants  []OAuthGrant  `json:"grants"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	s.clients, s.grants = f.Clients, f.Grants
	return s, nil
}

func (s *OAuthStore) save() error {
	data, err := json.MarshalIndent(struct {
		Clients []OAuthClient `json:"clients"`
		Grants  []OAuthGrant  `json:"grants"`
	}{s.clients, s.grants}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".oauth-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// validRedirectURI accepts what a native or web client needs and nothing that could hand a code to
// someone else: https anywhere, or http only to this machine (a loopback port the client listens on).
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.User != nil || u.Host == "" || len(raw) > 2000 {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "127.0.0.1" || h == "localhost" || h == "::1"
	}
	return false
}

// RegisterClient records a new public client. When the store is full, the oldest clients that hold no
// grant make room (anyone may register; only a person's consent makes a client useful).
func (s *OAuthStore) RegisterClient(name string, redirects []string) (OAuthClient, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "an MCP client"
	}
	if len(name) > 100 || strings.ContainsAny(name, "\r\n<>") {
		return OAuthClient{}, errors.New("client_name must be up to 100 characters, without line breaks or angle brackets")
	}
	if len(redirects) == 0 || len(redirects) > 10 {
		return OAuthClient{}, errors.New("redirect_uris must name 1 to 10 URIs")
	}
	for _, r := range redirects {
		if !validRedirectURI(r) {
			return OAuthClient{}, fmt.Errorf("redirect URI %q must be https, or http to 127.0.0.1, ::1 or localhost", r)
		}
	}
	id, err := randomHex(16)
	if err != nil {
		return OAuthClient{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clients) >= maxOAuthClients {
		used := map[string]bool{}
		for _, g := range s.grants {
			used[g.ClientID] = true
		}
		sort.SliceStable(s.clients, func(i, j int) bool { return s.clients[i].Created.Before(s.clients[j].Created) })
		need := len(s.clients) - maxOAuthClients + 1
		kept := make([]OAuthClient, 0, len(s.clients))
		for _, c := range s.clients {
			if need > 0 && !used[c.ID] {
				need--
				continue
			}
			kept = append(kept, c)
		}
		s.clients = kept
		if need > 0 {
			return OAuthClient{}, errors.New("too many registered clients hold grants; revoke some first")
		}
	}
	c := OAuthClient{ID: "nopc_" + id, Name: name, RedirectURIs: redirects, Created: s.now().UTC()}
	s.clients = append(s.clients, c)
	if err := s.save(); err != nil {
		s.clients = s.clients[:len(s.clients)-1]
		return OAuthClient{}, err
	}
	return c, nil
}

// Client returns a registered client.
func (s *OAuthStore) Client(id string) (OAuthClient, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clients {
		if c.ID == id {
			return c, true
		}
	}
	return OAuthClient{}, false
}

// NewCode issues a single-use authorization code for a consent just given.
func (s *OAuthStore) NewCode(c oauthCode) (string, error) {
	raw, err := randomHex(32)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, v := range s.codes {
		if now.After(v.Expires) {
			delete(s.codes, k)
		}
	}
	c.Expires = now.Add(oauthCodeTTL)
	s.codes[raw] = c
	return raw, nil
}

// ErrOAuthGrant is the token endpoint's invalid_grant: a code or refresh token that is wrong, used,
// expired, or not this client's.
var ErrOAuthGrant = errors.New("invalid_grant")

// pkceS256 is the S256 code challenge of a verifier.
func pkceS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// validVerifier is RFC 7636's code_verifier: 43 to 128 unreserved characters.
func validVerifier(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", c)) {
			return false
		}
	}
	return true
}

// ExchangeCode trades a code for a new grant's tokens. The code is used up whatever the outcome.
func (s *OAuthStore) ExchangeCode(code, clientID, redirectURI, verifier string) (OAuthGrant, string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.codes[code]
	delete(s.codes, code)
	switch {
	case !ok, s.now().After(c.Expires), c.ClientID != clientID, c.RedirectURI != redirectURI:
		return OAuthGrant{}, "", "", ErrOAuthGrant
	case !validVerifier(verifier) || subtle.ConstantTimeCompare([]byte(pkceS256(verifier)), []byte(c.Challenge)) != 1:
		return OAuthGrant{}, "", "", ErrOAuthGrant
	}
	client, ok := s.clientLocked(clientID)
	if !ok {
		return OAuthGrant{}, "", "", ErrOAuthGrant
	}
	id, err := randomHex(12)
	if err != nil {
		return OAuthGrant{}, "", "", err
	}
	refresh, err := newRefreshToken()
	if err != nil {
		return OAuthGrant{}, "", "", err
	}
	now := s.now().UTC()
	g := OAuthGrant{ID: "g_" + id, ClientID: clientID, ClientName: client.Name, Username: c.Username, Resource: c.Resource,
		Created: now, LastUsed: now, RefreshHash: hashSecret(refresh), RefreshExpires: now.Add(oauthRefreshTTL)}
	s.grants = append(s.grants, g)
	if err := s.save(); err != nil {
		s.grants = s.grants[:len(s.grants)-1]
		return OAuthGrant{}, "", "", err
	}
	return g, s.signAccess(g), refresh, nil
}

func (s *OAuthStore) clientLocked(id string) (OAuthClient, bool) {
	for _, c := range s.clients {
		if c.ID == id {
			return c, true
		}
	}
	return OAuthClient{}, false
}

func newRefreshToken() (string, error) {
	r, err := randomHex(32)
	return oauthRefreshPrefix + r, err
}

// Refresh rotates a refresh token: new tokens, and the old refresh token is spent. A spent one presented
// again means a copy exists, so the grant is revoked.
func (s *OAuthStore) Refresh(refresh, clientID string) (OAuthGrant, string, string, error) {
	h := hashSecret(refresh)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.grants {
		g := &s.grants[i]
		if g.PrevRefreshHash != "" && subtle.ConstantTimeCompare([]byte(g.PrevRefreshHash), []byte(h)) == 1 {
			s.grants = append(s.grants[:i], s.grants[i+1:]...)
			_ = s.save()
			return OAuthGrant{}, "", "", ErrOAuthGrant
		}
		if subtle.ConstantTimeCompare([]byte(g.RefreshHash), []byte(h)) != 1 {
			continue
		}
		if g.ClientID != clientID || s.now().After(g.RefreshExpires) {
			return OAuthGrant{}, "", "", ErrOAuthGrant
		}
		next, err := newRefreshToken()
		if err != nil {
			return OAuthGrant{}, "", "", err
		}
		now := s.now().UTC()
		old := *g
		g.PrevRefreshHash, g.RefreshHash, g.RefreshExpires, g.LastUsed = g.RefreshHash, hashSecret(next), now.Add(oauthRefreshTTL), now
		if err := s.save(); err != nil {
			*g = old
			return OAuthGrant{}, "", "", err
		}
		return *g, s.signAccess(*g), next, nil
	}
	return OAuthGrant{}, "", "", ErrOAuthGrant
}

type accessPayload struct {
	Grant string `json:"g"`
	Exp   int64  `json:"e"`
}

func (s *OAuthStore) mac(body string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("native-ops oauth access\x00" + body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *OAuthStore) signAccess(g OAuthGrant) string {
	p, _ := json.Marshal(accessPayload{Grant: g.ID, Exp: s.now().Add(oauthAccessTTL).Unix()})
	body := base64.RawURLEncoding.EncodeToString(p)
	return oauthAccessPrefix + body + "." + s.mac(body)
}

// VerifyAccess returns the grant an access token was issued for, if the token is genuine, unexpired,
// and its grant has not been revoked.
func (s *OAuthStore) VerifyAccess(token string) (OAuthGrant, bool) {
	rest, ok := strings.CutPrefix(token, oauthAccessPrefix)
	if !ok {
		return OAuthGrant{}, false
	}
	body, sig, ok := strings.Cut(rest, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.mac(body))) {
		return OAuthGrant{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	var p accessPayload
	if err != nil || json.Unmarshal(raw, &p) != nil || s.now().Unix() >= p.Exp {
		return OAuthGrant{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.grants {
		if s.grants[i].ID == p.Grant {
			// Last use is kept to the minute or so, not written on every call.
			if now := s.now().UTC(); now.Sub(s.grants[i].LastUsed) > 5*time.Minute {
				s.grants[i].LastUsed = now
				_ = s.save()
			}
			return s.grants[i], true
		}
	}
	return OAuthGrant{}, false
}

// Grants lists the grants, newest first.
func (s *OAuthStore) Grants() []OAuthGrant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]OAuthGrant(nil), s.grants...)
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// Revoke removes a grant: its access and refresh tokens stop working at once.
func (s *OAuthStore) Revoke(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.grants {
		if s.grants[i].ID == id {
			s.grants = append(s.grants[:i], s.grants[i+1:]...)
			return true, s.save()
		}
	}
	return false, nil
}

// RevokeRefresh removes the grant a refresh token belongs to (RFC 7009); an unknown token is no error.
func (s *OAuthStore) RevokeRefresh(refresh string) error {
	h := hashSecret(refresh)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.grants {
		if s.grants[i].RefreshHash == h {
			s.grants = append(s.grants[:i], s.grants[i+1:]...)
			return s.save()
		}
	}
	return nil
}
