package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
)

// ApplyFunc applies what a plan says, from the tree the plan was made from, reporting progress to
// logf. The plan has already been checked by the caller; it must apply only what the plan lists.
type ApplyFunc func(ctx context.Context, configDir string, plan *engine.FleetPlan, logf func(format string, a ...any)) error

var hashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// maxApplyDuration bounds one apply job.
const maxApplyDuration = 30 * time.Minute

// handleApply is POST /v1/apply?expect=<plan hash>&sha=<commit>&service=<name>. The body is the
// same configuration tree as for /v1/plan. Applying is deliberately harder than planning:
//
//   - expect is required. It is the hash of a plan (from /v1/plan) and the apply refuses, with
//     409 and the new plan, unless the plan it just made from this upload has that hash. What the
//     caller saw is what runs; if the host or the tree changed since, nothing is touched.
//   - A blocked plan is never applied, and a plan with nothing to change starts no job.
//   - One apply at a time on this host (409 while one runs), enforced before the upload is read.
//   - It runs as a job that outlives the request: 202 with the job, whose progress and outcome
//     are at /v1/jobs/{id} and are kept on disk.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	expect := r.URL.Query().Get("expect")
	if !hashRe.MatchString(expect) {
		writeError(w, http.StatusBadRequest, "bad_request", "expect is required: the 64-character hash of the plan you reviewed (from /v1/plan)")
		return
	}
	if !s.applyMu.TryLock() {
		msg := "an apply is already running on this host"
		if j, ok := s.opts.Jobs.Running(); ok {
			msg += " (" + string(j.ID) + ")"
		}
		auditDetail(r, "apply rejected: busy")
		writeError(w, http.StatusConflict, "busy", msg)
		return
	}
	handedOff := false
	defer func() {
		if !handedOff {
			s.applyMu.Unlock()
		}
	}()

	u, ok := s.receiveTree(w, r, "apply")
	if !ok {
		return
	}
	defer func() {
		if !handedOff {
			u.discard()
		}
	}()
	fp, ok := s.planUpload(w, r, "apply", u)
	if !ok {
		return
	}
	hash := fp.Hash()
	answer := func(code int, kind, msg string) {
		writeJSON(w, code, map[string]any{"error": msg, "code": kind, "hash": hash, "exit": fp.ExitStatus(), "plan": fp, "text": fp.Render()})
	}
	switch {
	case hash != expect:
		auditDetail(r, "apply rejected: plan changed expect=%s now=%s", expect[:12], hash[:12])
		answer(http.StatusConflict, "plan_changed", "the plan is no longer the one you approved: the host or the configuration changed. Review this plan and apply again with its hash.")
		return
	case fp.Blocked():
		auditDetail(r, "apply rejected: blocked hash=%s", hash[:12])
		answer(http.StatusConflict, "blocked", "the plan is blocked: apply would fail. Nothing was changed.")
		return
	case !fp.Pending():
		auditDetail(r, "apply: nothing to do hash=%s", hash[:12])
		writeJSON(w, http.StatusOK, map[string]any{"status": "nothing_to_do", "hash": hash, "exit": 0, "plan": fp, "text": fp.Render()})
		return
	}

	actor := "-"
	if h, ok := r.Context().Value(actorKey{}).(*actorHolder); ok {
		actor = h.name
	}
	job, err := s.opts.Jobs.Create(actor, u.sha, u.service, hash)
	if err != nil {
		log.Printf("apply: create job: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the job, so nothing was applied")
		return
	}
	auditDetail(r, "apply job=%s sha=%s service=%s hash=%s", job.ID, orDash(u.sha), orDash(u.service), hash[:12])
	handedOff = true
	s.jobsWG.Add(1)
	go s.runApply(job.ID, actor, u, fp)

	w.Header().Set("Location", "/v1/jobs/"+string(job.ID))
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job, "hash": hash, "plan": fp, "text": fp.Render()})
}

// runApply runs one job to its end and releases everything the request handed to it.
func (s *Server) runApply(id JobID, actor string, u *upload, fp *engine.FleetPlan) {
	defer s.jobsWG.Done()
	defer s.applyMu.Unlock()
	defer u.discard()

	logf := func(format string, a ...any) { s.opts.Jobs.Logf(id, format, a...) }
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("apply panicked: %v", p)
				log.Printf("apply %s: panic: %v", id, p)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), maxApplyDuration)
		defer cancel()
		logf("apply of plan %s started by %s", fp.Hash()[:12], actor)
		err = s.opts.Apply(ctx, u.root, fp, logf)
	}()
	if err != nil {
		logf("FAILED: %v", err)
	} else {
		logf("done")
	}
	s.opts.Jobs.Finish(id, err)
	outcome, code := "succeeded", http.StatusOK
	if err != nil {
		outcome, code = "failed", http.StatusInternalServerError
	}
	s.opts.Audit.Log(AuditEntry{Time: time.Now().UTC(), Actor: actor, Method: "JOB", Path: "/v1/jobs/" + string(id), Status: code, Detail: "apply " + outcome})
}

// handleJobs is GET /v1/jobs: the recent jobs, newest first, without logs.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.opts.Jobs.List()})
}

// handleJob is GET /v1/jobs/{id}. The log (which can hold whatever a hook printed) is for
// deployers and admins; a viewer sees the outcome.
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validJobID(id) {
		writeError(w, http.StatusNotFound, "not_found", "no such job")
		return
	}
	job, ok := s.opts.Jobs.Get(JobID(id))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such job")
		return
	}
	if h, ok := r.Context().Value(actorKey{}).(*actorHolder); !ok || !h.role.Allows(RoleDeployer) {
		job.Log = ""
	}
	writeJSON(w, http.StatusOK, job)
}

// WaitForJobs waits, up to timeout, for a running apply to finish, so a shutdown does not cut one off.
func (s *Server) WaitForJobs(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { s.jobsWG.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
