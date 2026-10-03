package server

import (
	"context"
	"net/http"

	"github.com/theta42/native-ops/pkg/engine"
)

// ImageBuildFunc builds app@ref from an unpacked configuration directory (the same shape POST
// /v1/plan and /v1/apply take), reporting progress to logf.
type ImageBuildFunc func(ctx context.Context, configDir, app, ref string, logf func(string, ...any)) error

// handleImageBuild is POST /v1/images/build?app=<name>&ref=<git-ref>. The body is a gzip-compressed
// tar of the native-ops-conf tree, exactly like /v1/plan and /v1/apply: the daemon needs no git
// access or credentials of its own, and every caller supplies the recipe it wants built rather than
// the daemon trusting a path on its own disk. A token with a scope may only build an image that
// matches its own Images glob, checked against the reference the build produces
// (<ImagePrefix><app>:<ref>, always with a tag: the recipe only omits one when no ref is given, and
// this endpoint always gives one), the same shape a scope is written in, e.g. "acme-platform:*".
func (s *Server) handleImageBuild(w http.ResponseWriter, r *http.Request) {
	app, ref := r.URL.Query().Get("app"), r.URL.Query().Get("ref")
	if !engine.ValidImageApp(app) {
		writeError(w, http.StatusBadRequest, "bad_request", "app must be a short lowercase label (a directory under images/ in the config repo)")
		return
	}
	if !engine.ValidImageRef(ref) {
		writeError(w, http.StatusBadRequest, "bad_request", "ref must be a plausible git branch or tag name")
		return
	}
	built := s.opts.ImagePrefix + app + ":" + ref
	actor, scope := s.actorScope(r)
	if scope != nil && !scope.AllowsImage(built) {
		auditDetail(r, "image build %s@%s refused: outside the token's scope", app, ref)
		writeError(w, http.StatusForbidden, "out_of_scope", "this token may not build "+built)
		return
	}
	u, ok := s.receiveTree(w, r, "image build")
	if !ok {
		return
	}
	// The build runs the upload's scripts on the host: only an approved recipe may (see recipes.go).
	digest, err := engine.RecipeDigest(u.root)
	if err != nil {
		u.discard()
		writeError(w, http.StatusBadRequest, "bad_config", trim(err.Error()))
		return
	}
	approved, err := s.opts.Recipes.Seen(digest, actor, app, u.sha)
	if err != nil {
		u.discard()
		writeError(w, http.StatusInternalServerError, "internal", "could not record the recipe, so nothing was built")
		return
	}
	if !approved {
		u.discard()
		auditDetail(r, "image build %s@%s refused: recipe %s not approved", app, ref, digest[:12])
		writeJSON(w, http.StatusForbidden, map[string]any{
			"code":   "recipe_not_approved",
			"digest": digest,
			"error": "this image recipe (scripts/ and images/ in the upload) has not been approved, and a build runs it on the host. " +
				"An admin can approve it with POST /v1/images/recipes/" + digest + "/approve (or `native-ops remote recipe-approve " + digest + "`); then run the build again. " +
				"Builds of other refs from the same recipe need no new approval.",
		})
		return
	}
	if !s.applyMu.TryLock() {
		u.discard()
		msg := "another change is already running on this host"
		if j, ok := s.opts.Jobs.Running(); ok {
			msg += " (" + string(j.ID) + ")"
		}
		auditDetail(r, "image build %s@%s rejected: busy", app, ref)
		writeError(w, http.StatusConflict, "busy", msg)
		return
	}
	job, err := s.opts.Jobs.CreateKind("image:build", actor, u.sha, app+"@"+ref, "")
	if err != nil {
		s.applyMu.Unlock()
		u.discard()
		writeError(w, http.StatusInternalServerError, "internal", "could not record the job, so nothing was built")
		return
	}
	auditDetail(r, "image build %s@%s job=%s", app, ref, job.ID)
	s.startJob(job.ID, actor, "build "+app+"@"+ref, "image build "+app+"@"+ref, u.discard, func(ctx context.Context, logf func(string, ...any)) error {
		return s.opts.ImageBuild(ctx, u.root, app, ref, logf)
	})
	w.Header().Set("Location", "/v1/jobs/"+string(job.ID))
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

// ImagePruneFunc applies image retention on the host (see engine.ImageRetention); with dryRun it only
// reports what it would delete.
type ImagePruneFunc func(ctx context.Context, dryRun bool, logf func(string, ...any)) error

// handleImagePrune is POST /v1/images/prune[?dry_run=1] (deployer, not a scoped token: it acts on every
// app's images). It deletes, as a job, the images no instance runs that retention does not keep: orphans
// left by rebuilds, and tags older than the newest few per app. Builds already clean up after
// themselves; this is for a scheduled maintenance job and for images left before that.
func (s *Server) handleImagePrune(w http.ResponseWriter, r *http.Request) {
	dry := r.URL.Query().Get("dry_run") == "1" || r.URL.Query().Get("dry_run") == "true"
	if !s.applyMu.TryLock() {
		msg := "another change is already running on this host"
		if j, ok := s.opts.Jobs.Running(); ok {
			msg += " (" + string(j.ID) + ")"
		}
		writeError(w, http.StatusConflict, "busy", msg)
		return
	}
	actor, _ := s.actorScope(r)
	what := "image prune"
	if dry {
		what += " (dry run)"
	}
	job, err := s.opts.Jobs.CreateKind("image:prune", actor, "", "", "")
	if err != nil {
		s.applyMu.Unlock()
		writeError(w, http.StatusInternalServerError, "internal", "could not record the job, so nothing was deleted")
		return
	}
	auditDetail(r, "%s job=%s", what, job.ID)
	s.startJob(job.ID, actor, what, what, nil, func(ctx context.Context, logf func(string, ...any)) error {
		return s.opts.ImagePrune(ctx, dry, logf)
	})
	w.Header().Set("Location", "/v1/jobs/"+string(job.ID))
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job, "dry_run": dry})
}
