package server

import (
	"context"
	"net/http"
)

// EdgeApplyFunc applies an unpacked configuration directory's edge Caddyfile to
// the edge container, reporting progress to logf. It changes the host, so it
// runs as a job like every other change.
type EdgeApplyFunc func(ctx context.Context, configDir string, logf func(string, ...any)) error

// handleEdgeApply is POST /v1/edge/apply. The body is the same gzip-compressed
// tar of the native-ops-conf tree as /v1/plan and /v1/apply: the daemon needs no
// git access, so the caller supplies the tree it checked out.
//
// It applies only edge/Caddyfile -- the edge's routes and TLS -- and never
// reconciles or replaces service containers, so unlike /v1/apply it is safe to
// run on every merge. It runs as a job: 202 with the job, whose progress and
// outcome are at /v1/jobs/{id}.
func (s *Server) handleEdgeApply(w http.ResponseWriter, r *http.Request) {
	u, ok := s.receiveTree(w, r, "edge apply")
	if !ok {
		return
	}
	if !s.applyMu.TryLock() {
		u.discard()
		msg := "another change is already running on this host"
		if j, ok := s.opts.Jobs.Running(); ok {
			msg += " (" + string(j.ID) + ")"
		}
		auditDetail(r, "edge apply rejected: busy")
		writeError(w, http.StatusConflict, "busy", msg)
		return
	}
	actor, _ := s.actorScope(r)
	job, err := s.opts.Jobs.CreateKind("edge:apply", actor, u.sha, "", "")
	if err != nil {
		s.applyMu.Unlock()
		u.discard()
		writeError(w, http.StatusInternalServerError, "internal", "could not record the job, so nothing was applied")
		return
	}
	auditDetail(r, "edge apply job=%s sha=%s", job.ID, orDash(u.sha))
	s.startJob(job.ID, actor, "edge apply", "edge apply", u.discard, func(ctx context.Context, logf func(string, ...any)) error {
		return s.opts.EdgeApply(ctx, u.root, logf)
	})
	w.Header().Set("Location", "/v1/jobs/"+string(job.ID))
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}
