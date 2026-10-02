package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// An image build runs the uploaded tree's scripts/build-image.sh (and whatever it calls under scripts/
// and images/) on the host, as the daemon's user. Whoever wrote those scripts can run anything there,
// so the daemon builds from a tree only when an admin has approved its recipe: the digest of
// everything under scripts/ and images/ (engine.RecipeDigest). Approval is per recipe, not per build:
// a release pipeline building a new ref with an unchanged recipe needs nothing, while any change to a
// build script waits for an admin, like an apply waits for its plan to be approved.
//
//	GET    /v1/images/recipes                     admin: the recipes seen, approved or waiting
//	POST   /v1/images/recipes/{digest}/approve    admin: allow builds from this recipe
//	DELETE /v1/images/recipes/{digest}/approval   admin: take the approval back

// RecipeRecord is a recipe the daemon has been asked to build from.
type RecipeRecord struct {
	Digest     string     `json:"digest"`
	FirstSeen  time.Time  `json:"first_seen"`
	LastSeen   time.Time  `json:"last_seen"`
	LastActor  string     `json:"last_actor"`
	LastApp    string     `json:"last_app"`
	LastSha    string     `json:"last_sha,omitempty"`
	ApprovedBy string     `json:"approved_by,omitempty"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
}

// RecipeStore keeps recipe records in one 0600 JSON file.
type RecipeStore struct {
	mu   sync.Mutex
	path string
	recs map[string]*RecipeRecord
	now  func() time.Time
}

const keepRecipes = 200

// OpenRecipeStore loads (or starts) the store at path.
func OpenRecipeStore(path string) (*RecipeStore, error) {
	s := &RecipeStore{path: path, recs: map[string]*RecipeRecord{}, now: func() time.Time { return time.Now().UTC() }}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Recipes []*RecipeRecord `json:"recipes"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	for _, r := range f.Recipes {
		if hashRe.MatchString(r.Digest) {
			s.recs[r.Digest] = r
		}
	}
	return s, nil
}

func (s *RecipeStore) save() error {
	list := s.listLocked()
	if len(list) > keepRecipes {
		// Keep every approved recipe, and the most recently seen of the rest.
		var keep []RecipeRecord
		for _, r := range list {
			if r.ApprovedAt != nil || len(keep) < keepRecipes {
				keep = append(keep, r)
			}
		}
		list = keep
		s.recs = map[string]*RecipeRecord{}
		for i := range list {
			r := list[i]
			s.recs[r.Digest] = &r
		}
	}
	b, err := json.MarshalIndent(struct {
		Recipes []RecipeRecord `json:"recipes"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".recipes-*")
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

func (s *RecipeStore) listLocked() []RecipeRecord {
	out := make([]RecipeRecord, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// Seen records a build request for a recipe and reports whether the recipe is approved.
func (s *RecipeStore) Seen(digest, actor, app, sha string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	r := s.recs[digest]
	if r == nil {
		r = &RecipeRecord{Digest: digest, FirstSeen: now}
		s.recs[digest] = r
	}
	r.LastSeen, r.LastActor, r.LastApp, r.LastSha = now, actor, app, sha
	return r.ApprovedAt != nil, s.save()
}

// Approve allows builds from a recipe. A digest the daemon has not seen yet may be approved ahead of the
// first build (an admin who computed it from the tree with `native-ops image recipe-digest`).
func (s *RecipeStore) Approve(digest, by string) (RecipeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	r := s.recs[digest]
	if r == nil {
		r = &RecipeRecord{Digest: digest, FirstSeen: now, LastSeen: now}
		s.recs[digest] = r
	}
	r.ApprovedBy, r.ApprovedAt = by, &now
	return *r, s.save()
}

// Revoke takes an approval back.
func (s *RecipeStore) Revoke(digest string) (RecipeRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[digest]
	if r == nil {
		return RecipeRecord{}, false, nil
	}
	r.ApprovedBy, r.ApprovedAt = "", nil
	return *r, true, s.save()
}

// List returns the records, most recently seen first.
func (s *RecipeStore) List() []RecipeRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *Server) handleRecipeList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"recipes": s.opts.Recipes.List()})
}

func (s *Server) handleRecipeApprove(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("digest")
	if !hashRe.MatchString(digest) {
		writeError(w, http.StatusBadRequest, "bad_request", "the digest is the 64-character hex recipe digest")
		return
	}
	by, _ := s.actorScope(r)
	rec, err := s.opts.Recipes.Approve(digest, by)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not record the approval")
		return
	}
	auditDetail(r, "image recipe approved %s", digest[:12])
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleRecipeRevoke(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("digest")
	if !hashRe.MatchString(digest) {
		writeError(w, http.StatusNotFound, "not_found", "no such recipe")
		return
	}
	rec, ok, err := s.opts.Recipes.Revoke(digest)
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", "could not record the change")
		return
	case !ok:
		writeError(w, http.StatusNotFound, "not_found", "no such recipe")
		return
	}
	auditDetail(r, "image recipe approval revoked %s", digest[:12])
	writeJSON(w, http.StatusOK, rec)
}
