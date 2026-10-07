package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The daemon's own credentials -- the DNS provider's token, the object store's keys, the OIDC client
// secret -- and the values that pin what an upload may do with them (the backup destination, the DNS
// zones). People enter them in the git server's secret store; a CI workflow pushes them here
// (`native-ops remote secret-sync`), so nobody logs in to the host to put them in a file. The daemon
// reads a name here first and falls back to its environment, so a host that already has them in
// /etc/native-ops/serve.env keeps working.
//
//	GET    /v1/secrets          admin: the names, who set each and when -- never a value
//	PUT    /v1/secrets          admin, or a secrets token for its SERVICE_* names: {"secrets": {NAME: value, ...}, "prune": bool}
//	DELETE /v1/secrets/{name}   admin

var secretNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

const (
	maxSecrets     = 100
	maxSecretBytes = 16 << 10
)

// SecretInfo is what the API shows about a secret.
type SecretInfo struct {
	Name  string    `json:"name"`
	SetBy string    `json:"set_by"`
	SetAt time.Time `json:"set_at"`
}

type secretEntry struct {
	Value string    `json:"value"`
	SetBy string    `json:"set_by"`
	SetAt time.Time `json:"set_at"`
}

// SecretStore keeps secrets in one 0600 JSON file in the state directory.
type SecretStore struct {
	mu   sync.Mutex
	path string
	vals map[string]secretEntry
	now  func() time.Time
}

// OpenSecretStore loads (or starts) the store at path.
func OpenSecretStore(path string) (*SecretStore, error) {
	s := &SecretStore{path: path, vals: map[string]secretEntry{}, now: func() time.Time { return time.Now().UTC() }}
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("secret file %s must not be readable by group or others (mode %o)", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Secrets map[string]secretEntry `json:"secrets"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for k, v := range f.Secrets {
		if secretNameRe.MatchString(k) {
			s.vals[k] = v
		}
	}
	return s, nil
}

// Get returns a secret's value.
func (s *SecretStore) Get(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.vals[name]
	return e.Value, ok
}

// Lookup is the daemon's way to read a credential: the store first, then the environment.
func (s *SecretStore) Lookup(name string) string {
	if s != nil {
		if v, ok := s.Get(name); ok {
			return v
		}
	}
	return os.Getenv(name)
}

// List returns the names and when they were set, never the values.
func (s *SecretStore) List() []SecretInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SecretInfo, 0, len(s.vals))
	for k, e := range s.vals {
		out = append(out, SecretInfo{Name: k, SetBy: e.SetBy, SetAt: e.SetAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Sync sets the given secrets (all or nothing), and with prune removes every other one -- every other one
// within, when within is set (a secrets token prunes only the names it may set). It reports which names it
// changed and which it removed; a value that is already stored is not a change.
func (s *SecretStore) Sync(set map[string]string, prune bool, by string) (changed, removed []string, err error) {
	return s.SyncWithin(set, prune, by, nil)
}

// SyncWithin is Sync limited to the names within accepts (nil: every name).
func (s *SecretStore) SyncWithin(set map[string]string, prune bool, by string, within func(string) bool) (changed, removed []string, err error) {
	for k, v := range set {
		if !secretNameRe.MatchString(k) {
			return nil, nil, fmt.Errorf("%q is not a secret name (A-Z, 0-9 and _, starting with a letter)", k)
		}
		if within != nil && !within(k) {
			return nil, nil, fmt.Errorf("this token may not set %s", k)
		}
		if v == "" || len(v) > maxSecretBytes || strings.ContainsRune(v, 0) {
			return nil, nil, fmt.Errorf("the value of %s is empty, too long or has a NUL byte", k)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]secretEntry, len(s.vals)+len(set))
	for k, e := range s.vals {
		if prune && (within == nil || within(k)) {
			if _, keep := set[k]; !keep {
				removed = append(removed, k)
				continue
			}
		}
		next[k] = e
	}
	now := s.now()
	for k, v := range set {
		if e, ok := next[k]; ok && e.Value == v {
			continue
		}
		next[k] = secretEntry{Value: v, SetBy: by, SetAt: now}
		changed = append(changed, k)
	}
	if len(next) > maxSecrets {
		return nil, nil, fmt.Errorf("at most %d secrets", maxSecrets)
	}
	sort.Strings(changed)
	sort.Strings(removed)
	if len(changed) == 0 && len(removed) == 0 {
		return nil, nil, nil
	}
	if err := s.saveLocked(next); err != nil {
		return nil, nil, err
	}
	s.vals = next
	return changed, removed, nil
}

// Delete removes one secret; it reports whether there was one.
func (s *SecretStore) Delete(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.vals[name]; !ok {
		return false, nil
	}
	next := make(map[string]secretEntry, len(s.vals))
	for k, e := range s.vals {
		if k != name {
			next[k] = e
		}
	}
	if err := s.saveLocked(next); err != nil {
		return false, err
	}
	s.vals = next
	return true, nil
}

func (s *SecretStore) saveLocked(vals map[string]secretEntry) error {
	b, err := json.MarshalIndent(struct {
		Secrets map[string]secretEntry `json:"secrets"`
	}{vals}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".secrets-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func (s *Server) handleSecretList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"secrets": s.opts.Secrets.List()})
}

// handleSecretSync is PUT /v1/secrets. The values are never logged, audited or answered back: the audit
// line and the answer name what changed.
func (s *Server) handleSecretSync(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Secrets map[string]string `json:"secrets"`
		Prune   bool              `json:"prune"`
	}
	if !readJSONLimit(w, r, &body, maxSecrets*maxSecretBytes) {
		return
	}
	if len(body.Secrets) == 0 && !body.Prune {
		writeError(w, http.StatusBadRequest, "bad_request", "send at least one secret")
		return
	}
	by, scope := s.actorScope(r)
	// A secrets token sets, prunes and sees only the service secrets its scope names.
	var within func(string) bool
	if scope.SecretsOnly() {
		within = scope.AllowsSecret
		for k := range body.Secrets {
			if !within(k) {
				writeError(w, http.StatusForbidden, "forbidden", "this token may not set "+k)
				return
			}
		}
	}
	changed, removed, err := s.opts.Secrets.SyncWithin(body.Secrets, body.Prune, by, within)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	auditDetail(r, "secrets synced: changed=%s removed=%s", strings.Join(changed, ","), strings.Join(removed, ","))
	list := s.opts.Secrets.List()
	if within != nil {
		mine := list[:0:0]
		for _, info := range list {
			if within(info.Name) {
				mine = append(mine, info)
			}
		}
		list = mine
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": nonNil(changed), "removed": nonNil(removed), "secrets": list})
}

func (s *Server) handleSecretDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !secretNameRe.MatchString(name) {
		writeError(w, http.StatusNotFound, "not_found", "no such secret")
		return
	}
	ok, err := s.opts.Secrets.Delete(name)
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", "could not change the secret store")
	case !ok:
		writeError(w, http.StatusNotFound, "not_found", "no such secret")
	default:
		auditDetail(r, "secret deleted %s", name)
		writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
