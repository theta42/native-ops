package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// A User is a person who signs in to the daemon UI. A local user carries a bcrypt password hash; an
// OIDC user is matched on the verified email the identity provider returns and carries no password.
// Either way the user maps to a Role -- the same ladder a token uses -- so a signed-in person and a
// machine token are authorised identically.
type User struct {
	Username  string    `json:"username"`
	Name      string    `json:"name,omitempty"`
	Role      Role      `json:"role"`
	Provider  string    `json:"provider"` // ProviderLocal or ProviderOIDC
	Email     string    `json:"email,omitempty"`
	Hash      string    `json:"hash,omitempty"` // bcrypt; empty for an OIDC-only user
	Disabled  bool      `json:"disabled,omitempty"`
	Created   time.Time `json:"created"`
	LastLogin string    `json:"last_login,omitempty"`
}

const (
	ProviderLocal = "local"
	ProviderOIDC  = "oidc"
)

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,39}$`)

// UserStore keeps users in a 0600 JSON file, re-read when it changes so `native-ops user create` on the
// host takes effect in a running daemon without a restart -- the same treatment TokenStore gives tokens.
type UserStore struct {
	path   string
	mu     sync.Mutex
	users  []User
	loaded os.FileInfo
}

func OpenUserStore(path string) (*UserStore, error) {
	s := &UserStore{path: path}
	if err := s.reload(true); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *UserStore) reload(force bool) error {
	fi, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.users, s.loaded = nil, nil
		return nil
	}
	if err != nil {
		return err
	}
	if !force && s.loaded != nil && os.SameFile(s.loaded, fi) && fi.ModTime().Equal(s.loaded.ModTime()) && fi.Size() == s.loaded.Size() {
		return nil
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("user file %s must not be readable by group or others (mode %o)", s.path, fi.Mode().Perm())
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var f struct {
		Users []User `json:"users"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	s.users, s.loaded = f.Users, fi
	return nil
}

func (s *UserStore) save() error {
	data, err := json.MarshalIndent(struct {
		Users []User `json:"users"`
	}{s.users}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".users-*")
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

func (s *UserStore) find(username string) int {
	u := strings.ToLower(strings.TrimSpace(username))
	for i := range s.users {
		if s.users[i].Username == u {
			return i
		}
	}
	return -1
}

// Count reports how many users exist, so the CLI can tell a fresh host from a configured one.
func (s *UserStore) Count() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(false); err != nil {
		return 0, err
	}
	return len(s.users), nil
}

// List returns the users with their password hashes removed.
func (s *UserStore) List() ([]User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(false); err != nil {
		return nil, err
	}
	out := make([]User, len(s.users))
	for i, u := range s.users {
		u.Hash = ""
		out[i] = u
	}
	return out, nil
}

// Get returns a user (without the hash).
func (s *UserStore) Get(username string) (User, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(false); err != nil {
		return User{}, false, err
	}
	i := s.find(username)
	if i < 0 {
		return User{}, false, nil
	}
	u := s.users[i]
	u.Hash = ""
	return u, true, nil
}

func validPassword(pw string) error {
	if len(pw) < 12 || len(pw) > 72 { // 72 bytes is bcrypt's limit
		return errors.New("a password needs 12 to 72 characters")
	}
	if strings.ContainsAny(pw, "\r\n\x00") {
		return errors.New("a password may not contain line breaks")
	}
	return nil
}

// CreateLocal adds a local user with a bcrypt-hashed password.
func (s *UserStore) CreateLocal(username, name string, role Role, password string) (User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernameRe.MatchString(username) {
		return User{}, errors.New("a username is 3-40 characters of a-z, 0-9, dot, dash or underscore")
	}
	if !ValidRole(role) {
		return User{}, fmt.Errorf("unknown role %q (viewer, planner, deployer or admin)", role)
	}
	if err := validPassword(password); err != nil {
		return User{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(false); err != nil {
		return User{}, err
	}
	if s.find(username) >= 0 {
		return User{}, fmt.Errorf("user %q already exists", username)
	}
	u := User{Username: username, Name: strings.TrimSpace(name), Role: role, Provider: ProviderLocal, Hash: string(hash), Created: time.Now().UTC()}
	s.users = append(s.users, u)
	if err := s.save(); err != nil {
		return User{}, err
	}
	u.Hash = ""
	return u, nil
}

// SetPassword replaces a local user's password (creating a local password for an OIDC user is allowed).
func (s *UserStore) SetPassword(username, password string) error {
	if err := validPassword(password); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(false); err != nil {
		return err
	}
	i := s.find(username)
	if i < 0 {
		return fmt.Errorf("no such user %q", username)
	}
	s.users[i].Hash = string(hash)
	if s.users[i].Provider == "" {
		s.users[i].Provider = ProviderLocal
	}
	return s.save()
}

// SetRole changes a user's role.
func (s *UserStore) SetRole(username string, role Role) error {
	if !ValidRole(role) {
		return fmt.Errorf("unknown role %q", role)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(false); err != nil {
		return err
	}
	i := s.find(username)
	if i < 0 {
		return fmt.Errorf("no such user %q", username)
	}
	s.users[i].Role = role
	return s.save()
}

// SetDisabled enables or disables a user.
func (s *UserStore) SetDisabled(username string, disabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(false); err != nil {
		return err
	}
	i := s.find(username)
	if i < 0 {
		return fmt.Errorf("no such user %q", username)
	}
	s.users[i].Disabled = disabled
	return s.save()
}

// FindOrCreateOIDC returns the OIDC user with this verified email, creating one at the given role on
// first sight. An email that already belongs to a local user is linked to OIDC rather than duplicated.
func (s *UserStore) FindOrCreateOIDC(email, name string, role Role) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	username := usernameForEmail(email)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(false); err != nil {
		return User{}, err
	}
	for i := range s.users {
		if s.users[i].Email != "" && s.users[i].Email == email {
			s.users[i].Provider = ProviderOIDC
			if name != "" {
				s.users[i].Name = name
			}
			if err := s.save(); err != nil {
				return User{}, err
			}
			u := s.users[i]
			u.Hash = ""
			return u, nil
		}
	}
	base := username
	for n := 2; s.find(base) >= 0; n++ {
		base = fmt.Sprintf("%s-%d", username, n)
	}
	u := User{Username: base, Name: name, Role: role, Provider: ProviderOIDC, Email: email, Created: time.Now().UTC()}
	s.users = append(s.users, u)
	if err := s.save(); err != nil {
		return User{}, err
	}
	u.Hash = ""
	return u, nil
}

func usernameForEmail(email string) string {
	local := email
	if i := strings.IndexByte(email, '@'); i > 0 {
		local = email[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(local) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('.')
		}
	}
	s := strings.Trim(b.String(), ".-_")
	if len(s) > 40 {
		s = s[:40]
	}
	if s == "" {
		s = "oidc-user"
	}
	if len(s) < 3 {
		s = s + "-user"
	}
	return s
}

// dummyHash is a real bcrypt hash, compared against when a user does not exist or has no local
// password, so a sign-in attempt costs the same either way (no timing oracle for whether a username
// exists or is OIDC-only).
var dummyHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("native-ops-dummy-password"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return h
}()

// Authenticate checks a local password. An OIDC user with no stored hash never matches.
func (s *UserStore) Authenticate(username, password string) (User, error) {
	s.mu.Lock()
	if err := s.reload(false); err != nil {
		s.mu.Unlock()
		return User{}, err
	}
	i := s.find(username)
	var u User
	hash := dummyHash
	if i >= 0 {
		u = s.users[i]
		if u.Hash != "" {
			hash = []byte(u.Hash)
		}
	}
	s.mu.Unlock()
	ok := bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
	if !ok || i < 0 || u.Disabled || u.Hash == "" {
		return User{}, errors.New("wrong username or password")
	}
	u.Hash = ""
	return u, nil
}

// TouchLogin records a sign-in.
func (s *UserStore) TouchLogin(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(false); err != nil {
		return err
	}
	i := s.find(username)
	if i < 0 {
		return nil
	}
	s.users[i].LastLogin = time.Now().UTC().Format(time.RFC3339)
	return s.save()
}

// PublicUser is a user as the UI sees it.
func PublicUser(u User) map[string]any {
	return map[string]any{"username": u.Username, "name": u.Name, "role": u.Role, "provider": u.Provider}
}
