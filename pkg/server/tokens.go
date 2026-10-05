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
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
)

// Role orders what a token may do: viewer reads; planner may also upload a configuration to be
// planned (which never changes the host), so it is the role for a pull-request pipeline, whose
// secrets any branch of the repository can read; deployer may change the host (apply); admin
// manages tokens.
type Role string

const (
	RoleViewer   Role = "viewer"
	RolePlanner  Role = "planner"
	RoleDeployer Role = "deployer"
	RoleAdmin    Role = "admin"
)

func (r Role) rank() int {
	switch r {
	case RoleViewer:
		return 1
	case RolePlanner:
		return 2
	case RoleDeployer:
		return 3
	case RoleAdmin:
		return 4
	}
	return 0
}

// Allows reports whether a token of role r may do something that needs min.
func (r Role) Allows(min Role) bool { return r.rank() > 0 && r.rank() >= min.rank() }

func ValidRole(r Role) bool { return r.rank() > 0 }

// Scope limits a token to managing tenant instances, and to particular ones: the instance names, image
// references and route domains it may use, as globs (path.Match). A token with a scope can call only the
// instance endpoints, never plan, apply or read the host's status, so a system that manages tenants (the
// fleet manager) holds a token that cannot touch gitea, plane or anything else, whatever it is asked to do.
type Scope struct {
	Names   []string `json:"names,omitempty"`
	Images  []string `json:"images,omitempty"`
	Domains []string `json:"domains,omitempty"` // empty: no route may be published
	// Labels limit the token to instances carrying one of the listed values for each label named, e.g.
	// {"environment": ["staging","testing"]}: it cannot see, create or change an instance with another
	// value, or with none, so a non-production token cannot reach production whatever it is asked to do.
	Labels map[string][]string `json:"labels,omitempty"`
}

// Any reports whether the scope limits anything.
func (s Scope) Any() bool { return len(s.Names)+len(s.Images)+len(s.Domains)+len(s.Labels) > 0 }

var patternRe = regexp.MustCompile(`^[A-Za-z0-9*?._:/@-]{1,100}$`)

// Validate checks that the patterns are usable and that a scope names what it allows.
func (s Scope) Validate() error {
	if !s.Any() {
		return nil
	}
	if len(s.Names) == 0 || len(s.Images) == 0 {
		return errors.New("a scoped token needs at least one instance name pattern and one image pattern")
	}
	for _, list := range [][]string{s.Names, s.Images, s.Domains} {
		for _, p := range list {
			if !patternRe.MatchString(p) {
				return fmt.Errorf("pattern %q is not allowed", p)
			}
			if _, err := path.Match(p, ""); err != nil {
				return fmt.Errorf("pattern %q is not valid: %v", p, err)
			}
		}
	}
	for k, values := range s.Labels {
		if !engine.ValidLabelKey(k) {
			return fmt.Errorf("label name %q is not allowed", k)
		}
		if len(values) == 0 {
			return fmt.Errorf("label %s needs at least one allowed value", k)
		}
		for _, v := range values {
			if !patternRe.MatchString(v) {
				return fmt.Errorf("label value %q is not allowed", v)
			}
			if _, err := path.Match(v, ""); err != nil {
				return fmt.Errorf("label value %q is not valid: %v", v, err)
			}
			// A plain environment value must be a real one, so a typo cannot quietly scope a token to nothing.
			if k == engine.EnvironmentLabel && !strings.ContainsAny(v, "*?[") && !slices.Contains(engine.Environments, v) {
				return fmt.Errorf("environment %q is not one of %s", v, strings.Join(engine.Environments, ", "))
			}
		}
	}
	return nil
}

// ParseLabelScope reads a label scope as written on a command line: key=v1,v2;key2=v3.
func ParseLabelScope(text string) (map[string][]string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	out := map[string][]string{}
	for _, part := range strings.Split(text, ";") {
		k, vs, ok := strings.Cut(strings.TrimSpace(part), "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.TrimSpace(vs) == "" {
			return nil, fmt.Errorf("a label scope looks like environment=staging,testing;app=platform, not %q", part)
		}
		for _, v := range strings.Split(vs, ",") {
			if v = strings.TrimSpace(v); v != "" {
				out[k] = append(out[k], v)
			}
		}
	}
	return out, nil
}

func matchAny(patterns []string, v string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, v); ok {
			return true
		}
	}
	return false
}

func (s Scope) AllowsName(n string) bool   { return matchAny(s.Names, n) }
func (s Scope) AllowsImage(i string) bool  { return matchAny(s.Images, i) }
func (s Scope) AllowsDomain(d string) bool { return matchAny(s.Domains, d) }

// AllowsLabels reports whether an instance with these labels is inside the label scope: every label the
// scope names must be present with an allowed value. An unlabelled instance is never inside a label scope.
func (s Scope) AllowsLabels(labels map[string]string) bool {
	for k, allowed := range s.Labels {
		v, ok := labels[k]
		if !ok || !matchAny(allowed, v) {
			return false
		}
	}
	return true
}

// ScopeSummary is a scope as one line, for listings.
func (s Scope) Summary() string {
	out := "names=" + strings.Join(s.Names, ",") + " images=" + strings.Join(s.Images, ",") + " domains=" + strings.Join(s.Domains, ",")
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out += " " + k + "=" + strings.Join(s.Labels[k], ",")
	}
	return out
}

// Token is one API credential. Only the SHA-256 of the secret is ever stored;
// the secret itself is shown once, at creation.
type Token struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Role    Role      `json:"role"`
	Scope   *Scope    `json:"scope,omitempty"`
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
}

// Scoped reports whether the token is limited to a scope of instances.
func (t Token) Scoped() bool { return t.Scope != nil && t.Scope.Any() }

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

// ownerOf returns the uid/gid that should own the token file: the existing file's owner when it exists,
// else the parent directory's (the daemon's state dir, owned by the daemon's user). ok is false when
// neither can be read.
func ownerOf(paths ...string) (uid, gid int, ok bool) {
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			return int(st.Uid), int(st.Gid), true
		}
	}
	return 0, 0, false
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
	// The temp file is owned by whoever runs this (root, for a token command on the host). Keep the
	// file's ownership in step with the daemon that must read it: the existing file's owner, else the
	// state directory's. Otherwise a root write leaves a 0600 file the native-ops user cannot read, and
	// the daemon crash-loops on the next restart.
	if uid, gid, ok := ownerOf(s.path, filepath.Dir(s.path)); ok {
		_ = tmp.Chown(uid, gid) // best effort: a non-root writer to its own file is already correct
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

// SetBootstrapHash registers the in-memory admin token by the SHA-256 of its secret (64 hex
// characters) instead of the secret itself. It is for places that may be read by more than root, such
// as a cloud provider's user-data, which a container that can reach the metadata service can fetch:
// the hash of a 256-bit secret gives nothing away, and CI keeps the secret.
func (s *TokenStore) SetBootstrapHash(sum string) error {
	sum = strings.ToLower(strings.TrimSpace(sum))
	if len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" {
		return errors.New("the bootstrap token hash must be the 64-character hex SHA-256 of the token")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bootstrap = &Token{ID: "bootstrap", Name: "bootstrap", Role: RoleAdmin, Hash: sum}
	return nil
}

// HashSecret is how a token secret is stored: hex SHA-256. It is what SetBootstrapHash expects.
func HashSecret(secret string) string { return hashSecret(secret) }

// Create makes a new token and returns its secret, which is not recoverable afterwards.
func (s *TokenStore) Create(name string, role Role) (secret string, t Token, err error) {
	return s.CreateScoped(name, role, Scope{})
}

// CreateScoped makes a token limited to a scope of instances (see Scope). It needs at least the
// deployer role: the scope narrows what that role may touch.
func (s *TokenStore) CreateScoped(name string, role Role, scope Scope) (secret string, t Token, err error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 || strings.ContainsAny(name, "\n\r") {
		return "", Token{}, errors.New("a token needs a short name")
	}
	if !ValidRole(role) {
		return "", Token{}, fmt.Errorf("unknown role %q (viewer, planner, deployer or admin)", role)
	}
	if err := scope.Validate(); err != nil {
		return "", Token{}, err
	}
	if scope.Any() && role != RoleDeployer {
		return "", Token{}, errors.New("a scoped token must have the deployer role (the scope is what limits it)")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", Token{}, err
	}
	secret = secretPrefix + hex.EncodeToString(b)
	h := hashSecret(secret)
	t = Token{ID: h[:8], Name: name, Role: role, Hash: h, Created: time.Now().UTC()}
	if scope.Any() {
		t.Scope = &scope
	}
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
