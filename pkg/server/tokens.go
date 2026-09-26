package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Role orders what a token may do: viewer reads, deployer may change instances
// (later), admin manages tokens.
type Role string

const (
	RoleViewer   Role = "viewer"
	RoleDeployer Role = "deployer"
	RoleAdmin    Role = "admin"
)

func (r Role) rank() int {
	switch r {
	case RoleViewer:
		return 1
	case RoleDeployer:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

// Allows reports whether a token of role r may do something that needs min.
func (r Role) Allows(min Role) bool { return r.rank() > 0 && r.rank() >= min.rank() }

func ValidRole(r Role) bool { return r.rank() > 0 }

// Token is one API credential. Only the SHA-256 of the secret is ever stored;
// the secret itself is shown once, at creation.
type Token struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Role    Role      `json:"role"`
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
}

const secretPrefix = "nops_"

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// ValidSecretFormat is the shape of a token secret: the prefix plus at least 32 chars.
func ValidSecretFormat(s string) bool {
	return strings.HasPrefix(s, secretPrefix) && len(s) >= len(secretPrefix)+32 && !strings.ContainsAny(s, " \t\r\n")
}

// TokenStore keeps tokens in a 0600 JSON file. It re-reads the file when it
// changes, so `native-ops token create` on the host takes effect in a running
// daemon without a restart. A bootstrap token (from the environment, provisioned
// by IaC/CI) is held in memory only.
type TokenStore struct {
	path      string
	mu        sync.Mutex
	tokens    []Token
	loaded    os.FileInfo // the file as last read; a change is a different file, mtime or size
	bootstrap *Token
}

func OpenTokenStore(path string) (*TokenStore, error) {
	s := &TokenStore{path: path}
	if err := s.reload(true); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *TokenStore) reload(force bool) error {
	fi, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.tokens, s.loaded = nil, nil
		return nil
	}
	if err != nil {
		return err
	}
	// Timestamps are coarse (two writes in one tick share an mtime), and every save
	// replaces the file, so compare identity and size as well as the time.
	if !force && s.loaded != nil && os.SameFile(s.loaded, fi) && fi.ModTime().Equal(s.loaded.ModTime()) && fi.Size() == s.loaded.Size() {
		return nil
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("token file %s must not be readable by group or others (mode %o)", s.path, fi.Mode().Perm())
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var f struct {
		Tokens []Token `json:"tokens"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	s.tokens, s.loaded = f.Tokens, fi
	return nil
}

func (s *TokenStore) save() error {
	data, err := json.MarshalIndent(struct {
		Tokens []Token `json:"tokens"`
	}{s.tokens}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tokens-*")
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
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	if fi, err := os.Stat(s.path); err == nil {
		s.loaded = fi
	}
	return nil
}

// SetBootstrap registers an in-memory admin token from a secret provisioned
// out of band (an IaC/CI secret), so an operator never has to run a command on
// the host to obtain the first credential.
func (s *TokenStore) SetBootstrap(secret string) error {
	if !ValidSecretFormat(secret) {
		return fmt.Errorf("bootstrap token must look like %s followed by at least 32 characters", secretPrefix)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bootstrap = &Token{ID: "bootstrap", Name: "bootstrap", Role: RoleAdmin, Hash: hashSecret(secret)}
	return nil
}

// Create makes a new token and returns its secret, which is not recoverable afterwards.
func (s *TokenStore) Create(name string, role Role) (secret string, t Token, err error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 || strings.ContainsAny(name, "\n\r") {
		return "", Token{}, errors.New("a token needs a short name")
	}
	if !ValidRole(role) {
		return "", Token{}, fmt.Errorf("unknown role %q (viewer, deployer or admin)", role)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", Token{}, err
	}
	secret = secretPrefix + hex.EncodeToString(b)
	h := hashSecret(secret)
	t = Token{ID: h[:8], Name: name, Role: role, Hash: h, Created: time.Now().UTC()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(true); err != nil {
		return "", Token{}, err
	}
	s.tokens = append(s.tokens, t)
	if err := s.save(); err != nil {
		return "", Token{}, err
	}
	return secret, t, nil
}

func (s *TokenStore) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(true); err != nil {
		return err
	}
	for i, t := range s.tokens {
		if t.ID == id {
			s.tokens = append(s.tokens[:i], s.tokens[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("no token with id %q", id)
}

func (s *TokenStore) List() ([]Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(true); err != nil {
		return nil, err
	}
	out := make([]Token, len(s.tokens))
	copy(out, s.tokens)
	for i := range out {
		out[i].Hash = "" // never hand a hash back out
	}
	return out, nil
}

// Verify returns the token for a presented secret. Every stored hash is
// compared in constant time and the whole list is always walked.
func (s *TokenStore) Verify(secret string) (Token, bool) {
	if !ValidSecretFormat(secret) {
		return Token{}, false
	}
	h := hashSecret(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reload(false) // on a read error keep serving the last good copy
	var found Token
	ok := false
	consider := func(t Token) {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(h)) == 1 {
			found, ok = t, true
		}
	}
	if s.bootstrap != nil {
		consider(*s.bootstrap)
	}
	for _, t := range s.tokens {
		consider(t)
	}
	return found, ok
}
