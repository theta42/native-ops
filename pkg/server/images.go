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
// matches its own Images glob, checked against the reference the build actually produces
// (opsavor-<app>:<ref>, always with a tag: build-image.sh only omits one when no ref is given, and
// this endpoint always gives one), the same shape a scope is written in, e.g. "opsavor-platform:*".
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
	built := "opsavor-" + app + ":" + ref
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
