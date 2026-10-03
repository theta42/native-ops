package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
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
	s.recordPlan(r, u, fp)
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

	// The plan is the one that was reviewed and it has changes. It still needs an admin's approval:
	// a deployer token is not enough on its own to change the host.
	if err := s.opts.Plans.Check(hash); err != nil {
		switch {
		case errors.Is(err, ErrApprovalUsed):
			auditDetail(r, "apply rejected: approval already used hash=%s", hash[:12])
			answer(http.StatusForbidden, "approval_used", "the approval for this plan was already used by an apply. An admin has to approve it again.")
		case errors.Is(err, ErrApprovalExpiry):
			auditDetail(r, "apply rejected: approval expired hash=%s", hash[:12])
			answer(http.StatusForbidden, "approval_expired", "the approval for this plan has expired. An admin has to approve it again.")
		default:
			auditDetail(r, "apply rejected: not approved hash=%s", hash[:12])
			answer(http.StatusForbidden, "not_approved", "this plan has not been approved. An admin can approve it (in the UI under Plans, or POST /v1/plans/"+hash+"/approve); then run the apply again.")
		}
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
	// One approval, one apply: it is used up here, before anything runs. The apply lock is held, so no
	// other apply can have used it since Check.
	if err := s.opts.Plans.Consume(hash, job.ID); err != nil {
		s.opts.Jobs.Finish(job.ID, fmt.Errorf("the approval could not be used: %v", err))
		writeError(w, http.StatusInternalServerError, "internal", "could not use the approval, so nothing was applied")
		return
	}
	auditDetail(r, "apply job=%s sha=%s service=%s hash=%s", job.ID, orDash(u.sha), orDash(u.service), hash[:12])
	handedOff = true
	s.runApply(job.ID, actor, u, fp) // returns at once: the job runs in the background

	w.Header().Set("Location", "/v1/jobs/"+string(job.ID))
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job, "hash": hash, "plan": fp, "text": fp.Render()})
}

// runApply runs one apply job to its end and releases everything the request handed to it.
func (s *Server) runApply(id JobID, actor string, u *upload, fp *engine.FleetPlan) {
	s.startJob(id, actor, "apply of plan "+fp.Hash()[:12], "apply", u.discard, func(ctx context.Context, logf func(string, ...any)) error {
		return s.opts.Apply(ctx, u.root, fp, logf)
	})
}

// startJob runs work as the job id in the background. The caller holds the host lock (applyMu); it is
// released, and cleanup run, BEFORE the job is reported finished: a client that sees "succeeded" and
// starts the next change must find the host free, not a 409 for a job that is over. what says what the
// job is, for its log; audit is how its end is named in the audit log.
func (s *Server) startJob(id JobID, actor, what, audit string, cleanup func(), work func(ctx context.Context, logf func(string, ...any)) error) {
	s.jobsWG.Add(1)
	go func() {
		defer s.jobsWG.Done()
		logf := func(format string, a ...any) { s.opts.Jobs.Logf(id, format, a...) }
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("%s panicked: %v", what, p)
					log.Printf("job %s: panic: %v", id, p)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), maxApplyDuration)
			defer cancel()
			logf("%s started by %s", what, actor)
			err = work(ctx, logf)
		}()
		if err != nil {
			logf("FAILED: %v", err)
		} else {
			logf("done")
		}

		if cleanup != nil {
			cleanup()
		}
		s.applyMu.Unlock()

		s.opts.Jobs.Finish(id, err)
		outcome, code := "succeeded", http.StatusOK
		if err != nil {
			outcome, code = "failed", http.StatusInternalServerError
		}
		s.opts.Audit.Log(AuditEntry{Time: time.Now().UTC(), Actor: actor, Method: "JOB", Path: "/v1/jobs/" + string(id), Status: code, Detail: audit + " " + outcome})
	}()
}

// handleJobs is GET /v1/jobs?kind=&status=&limit=: the recent jobs, newest first, without logs. kind is a
// job kind ("deploy", "apply", "image:build"), or a family of them ending in a colon ("instance:");
// status is running, succeeded, failed or interrupted. A token with a scope sees only its own jobs (see
// scopeSees).
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind, status := q.Get("kind"), JobStatus(q.Get("status"))
	switch status {
	case "", JobRunning, JobSucceeded, JobFailed, JobInterrupted:
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "status must be running, succeeded, failed or interrupted")
		return
	}
	limit, ok := queryLimit(w, r, keepJobs)
	if !ok {
		return
	}
	actor, scope := s.actorScope(r)
	out := []Job{}
	for _, j := range s.opts.Jobs.List() {
		switch {
		case scope != nil && !scopeSees(scope, actor, j),
			kind != "" && j.Kind != kind && !(strings.HasSuffix(kind, ":") && strings.HasPrefix(j.Kind, kind)),
			status != "" && j.Status != status:
			continue
		}
		if out = append(out, j); len(out) == limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// scopeSees reports whether a token limited to scope, named actor, may see a job: one it started
// itself, or a change to an instance its scope allows. A tenant system must not learn what else
// runs on the host -- other tenants, applies, builds -- from the job list.
func scopeSees(scope *Scope, actor string, j Job) bool {
	if j.Actor == actor {
		return true
	}
	return strings.HasPrefix(j.Kind, "instance:") && scope.AllowsName(j.Service)
}

// maxJobWait bounds GET /v1/jobs/{id}?wait=, well inside the server's write timeout.
const maxJobWait = 60 * time.Second

// handleJob is GET /v1/jobs/{id}?wait=<seconds>. The log (which can hold whatever a hook printed) is for
// deployers and admins; a viewer sees the outcome. With wait (up to 60), a running job is answered when it
// ends or when the wait is over, whichever is first, so a client need not poll.
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validJobID(id) {
		writeError(w, http.StatusNotFound, "not_found", "no such job")
		return
	}
	var wait time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || time.Duration(n)*time.Second > maxJobWait {
			writeError(w, http.StatusBadRequest, "bad_request", "wait must be a number of seconds from 0 to 60")
			return
		}
		wait = time.Duration(n) * time.Second
	}
	job, ok := s.opts.Jobs.Get(JobID(id))
	for deadline := time.Now().Add(wait); ok && job.Status == JobRunning && time.Now().Before(deadline); {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
		job, ok = s.opts.Jobs.Get(JobID(id))
	}
	if actor, scope := s.actorScope(r); ok && scope != nil && !scopeSees(scope, actor, job) {
		ok = false // the same answer as a job that does not exist
	}
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
