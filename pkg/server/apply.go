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
//   - It needs the host free: 409 while another change runs or waits (an apply cannot wait in line, see
//     queue.go), checked before the upload is read.
//   - It runs as a job that outlives the request: 202 with the job, whose progress and outcome
//     are at /v1/jobs/{id} and are kept on disk.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	expect := r.URL.Query().Get("expect")
	if !hashRe.MatchString(expect) {
		writeError(w, http.StatusBadRequest, "bad_request", "expect is required: the 64-character hash of the plan you reviewed (from /v1/plan)")
		return
	}
	// An apply checks the plan against the host as it is now and uses its approval up, so it cannot wait
	// in line: it takes the host only if nothing runs or waits (see queue.go), and holds it while it plans.
	t, free := s.queue.tryTicket()
	if !free {
		s.answerNotStarted(w, r, "apply", errBusy(s.busyMessage()))
		return
	}
	handedOff := false
	defer func() {
		if !handedOff {
			s.queue.release()
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
	handedOff = true // launch releases the host from here on, whatever happens
	job, err := s.launch(t, jobSpec{
		kind: "apply", actor: actor, sha: u.sha, subject: u.service, planHash: hash,
		what: "apply of plan " + hash[:12], audit: "apply", cleanup: u.discard,
		// One approval, one apply: it is used up here, before anything runs. This request holds the host,
		// so no other apply can have used it since Check.
		prepare: func(job Job) error {
			if err := s.opts.Plans.Consume(hash, job.ID); err != nil {
				return fmt.Errorf("the approval could not be used: %v", err)
			}
			return nil
		},
		work: func(ctx context.Context, logf func(string, ...any)) error {
			return s.opts.Apply(ctx, u.root, fp, logf)
		},
	})
	if err != nil {
		log.Printf("apply: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the job or use the approval, so nothing was applied")
		return
	}
	auditDetail(r, "apply job=%s sha=%s service=%s hash=%s", job.ID, orDash(u.sha), orDash(u.service), hash[:12])
	answerJob(w, job, map[string]any{"hash": hash, "plan": fp, "text": fp.Render()})
}

// jobSpec is a change to run as a job.
type jobSpec struct {
	kind, actor, sha, subject, planHash string
	what                                string // what the job is, for its log: "build crew@main"
	audit                               string // how its end is named in the audit log
	cleanup                             func() // run when the job ends (or will never run), before the host is released
	// prepare, when set, runs before the job is handed to the background, with the job recorded; an
	// error ends the job as failed before anything else happens.
	prepare func(Job) error
	work    func(ctx context.Context, logf func(string, ...any)) error
}

type jobIDKey struct{}

// jobIDOf is the job a job's work runs as.
func jobIDOf(ctx context.Context) JobID {
	id, _ := ctx.Value(jobIDKey{}).(JobID)
	return id
}

// queueJob records a change and runs it when the host is free: at once if it is, else after the
// changes queued before it (see queue.go). It fails only when the queue is full, the daemon is
// stopping or the job cannot be recorded; the caller answers with answerNotStarted.
func (s *Server) queueJob(spec jobSpec) (Job, error) {
	t, err := s.queue.enqueue()
	if err != nil {
		return Job{}, err
	}
	return s.launch(t, spec)
}

// runJobNow is queueJob for a change that cannot wait in line: errBusy unless the host is free.
func (s *Server) runJobNow(spec jobSpec) (Job, error) {
	t, ok := s.queue.tryTicket()
	if !ok {
		return Job{}, errBusy(s.busyMessage())
	}
	return s.launch(t, spec)
}

// busyMessage says what the host is doing, for a change that needed it free.
func (s *Server) busyMessage() string {
	msg := "another change is already running on this host"
	if j, ok := s.opts.Jobs.Running(); ok {
		msg += " (" + string(j.ID) + ")"
	}
	if n := s.queue.waitingCount(); n > 0 {
		msg += fmt.Sprintf(" and %d more wait for it", n)
	}
	return msg
}

// answerNotStarted answers a request whose job could not be queued or started.
func (s *Server) answerNotStarted(w http.ResponseWriter, r *http.Request, what string, err error) {
	var busy errBusy
	switch {
	case errors.As(err, &busy):
		auditDetail(r, "%s rejected: busy", what)
		writeError(w, http.StatusConflict, "busy", busy.Error())
	case errors.Is(err, errQueueFull):
		auditDetail(r, "%s rejected: queue full", what)
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, "queue_full", fmt.Sprintf("%d changes are already waiting for this host; retry shortly", maxQueuedJobs))
	case errors.Is(err, errQueueClosed):
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusServiceUnavailable, "stopping", "the daemon is stopping; retry in a moment")
	default:
		log.Printf("%s: %v", what, err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the job, so nothing was done")
	}
}

// answerJob answers 202 with a job that was queued or started.
func answerJob(w http.ResponseWriter, job Job, extra map[string]any) {
	out := map[string]any{"job": job}
	for k, v := range extra {
		out[k] = v
	}
	w.Header().Set("Location", "/v1/jobs/"+string(job.ID))
	writeJSON(w, http.StatusAccepted, out)
}

// launch records the job of a ticket and runs it in the background when the ticket is granted. The host
// is released, and cleanup run, BEFORE the job is reported finished: a client that sees "succeeded" and
// starts the next change must find the host free (or its own change next in line), not still taken by a
// job that is over.
func (s *Server) launch(t *ticket, spec jobSpec) (Job, error) {
	job, err := s.opts.Jobs.CreateQueued(spec.kind, spec.actor, spec.sha, spec.subject, spec.planHash)
	if err != nil {
		s.queue.abandon(t)
		if spec.cleanup != nil {
			spec.cleanup()
		}
		return Job{}, err
	}
	s.queue.name(t, job.ID)
	// end finishes the job. release is false only for a job that gave its place in line up (abandon) and
	// so holds nothing to release.
	end := func(err error, release bool) {
		if spec.cleanup != nil {
			spec.cleanup()
		}
		if release {
			s.queue.release()
		}
		s.opts.Jobs.Finish(job.ID, err)
		outcome, code := "succeeded", http.StatusOK
		if err != nil {
			outcome, code = "failed", http.StatusInternalServerError
		}
		s.opts.Audit.Log(AuditEntry{Time: time.Now().UTC(), Actor: spec.actor, Method: "JOB", Path: "/v1/jobs/" + string(job.ID), Status: code, Detail: spec.audit + " " + outcome})
	}
	if t.granted {
		s.opts.Jobs.Start(job.ID)
	}
	if spec.prepare != nil {
		if err := spec.prepare(job); err != nil {
			if t.granted {
				end(err, true)
			} else {
				s.queue.abandon(t) // its place in line, or the host if it was handed over meanwhile
				end(err, false)
			}
			return Job{}, err
		}
	}
	if job, _ = s.opts.Jobs.Get(job.ID); !t.granted {
		job.Position = s.queue.position(job.ID)
	}
	job.Log = ""

	s.jobsWG.Add(1)
	go func() {
		defer s.jobsWG.Done()
		if !<-t.ch {
			// The daemon is stopping and this never got the host: nothing ran, and nothing is released.
			if spec.cleanup != nil {
				spec.cleanup()
			}
			s.opts.Jobs.Interrupt(job.ID, notStarted)
			s.opts.Audit.Log(AuditEntry{Time: time.Now().UTC(), Actor: spec.actor, Method: "JOB", Path: "/v1/jobs/" + string(job.ID), Status: http.StatusServiceUnavailable, Detail: spec.audit + " not started"})
			return
		}
		if !t.granted {
			s.opts.Jobs.Start(job.ID)
		}
		logf := func(format string, a ...any) { s.opts.Jobs.Logf(job.ID, format, a...) }
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("%s panicked: %v", spec.what, p)
					log.Printf("job %s: panic: %v", job.ID, p)
				}
			}()
			ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), jobIDKey{}, job.ID), maxApplyDuration)
			defer cancel()
			if !t.granted {
				logf("%s started by %s (it waited %s for the host)", spec.what, spec.actor, time.Since(job.Created).Round(time.Second))
			} else {
				logf("%s started by %s", spec.what, spec.actor)
			}
			err = spec.work(ctx, logf)
		}()
		if err != nil {
			logf("FAILED: %v", err)
		} else {
			logf("done")
		}
		end(err, true)
	}()
	return job, nil
}

// handleJobs is GET /v1/jobs?kind=&status=&limit=: the recent jobs, newest first, without logs. kind is a
// job kind ("deploy", "apply", "image:build"), or a family of them ending in a colon ("instance:");
// status is queued, running, succeeded, failed or interrupted. A token with a scope sees only its own jobs (see
// scopeSees).
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind, status := q.Get("kind"), JobStatus(q.Get("status"))
	switch status {
	case "", JobQueued, JobRunning, JobSucceeded, JobFailed, JobInterrupted:
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "status must be queued, running, succeeded, failed or interrupted")
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
		if j.Status == JobQueued {
			j.Position = s.queue.position(j.ID)
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
// deployers and admins; a viewer sees the outcome. With wait (up to 60), a queued or running job is
// answered when it ends or when the wait is over, whichever is first, so a client need not poll. A queued
// job's position says where it is in line.
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
	for deadline := time.Now().Add(wait); ok && job.pending() && time.Now().Before(deadline); {
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
	if job.Status == JobQueued {
		job.Position = s.queue.position(job.ID)
	}
	writeJSON(w, http.StatusOK, job)
}

// WaitForJobs waits, up to timeout, for a running job to finish, so a shutdown does not cut one off. Jobs
// still queued end at once when the queue is closed (Serve closes it first).
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
