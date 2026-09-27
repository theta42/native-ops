package server

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
)

// planView is a plan record as the API shows it: the record and where it stands now.
type planView struct {
	PlanRecord
	State PlanState `json:"state"`
}

func (s *Server) viewOf(rec PlanRecord) planView {
	return planView{PlanRecord: rec, State: rec.State(time.Now().UTC())}
}

// recordPlan notes a plan that was just made, so it can be looked at and approved.
func (s *Server) recordPlan(r *http.Request, u *upload, fp *engine.FleetPlan) {
	if s.opts.Plans == nil {
		return
	}
	actor := "-"
	if h, ok := r.Context().Value(actorKey{}).(*actorHolder); ok {
		actor = h.name
	}
	s.opts.Plans.Record(PlanSeen{
		Hash: fp.Hash(), Actor: actor, Sha: u.sha, Service: u.service,
		Exit: fp.ExitStatus(), Counts: fp.Counts(), Text: fp.Render(),
	})
}

// handlePlanList is GET /v1/plans: the recent plans and where each stands, without their text.
func (s *Server) handlePlanList(w http.ResponseWriter, r *http.Request) {
	recs := s.opts.Plans.List()
	views := make([]planView, len(recs))
	for i, rec := range recs {
		views[i] = s.viewOf(rec)
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": views, "approval_ttl_seconds": int(s.opts.Plans.TTL().Seconds())})
}

// handlePlanGet is GET /v1/plans/{hash}: one plan, with its text.
func (s *Server) handlePlanGet(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !hashRe.MatchString(hash) {
		writeError(w, http.StatusNotFound, "not_found", "no such plan")
		return
	}
	rec, ok := s.opts.Plans.Get(hash)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such plan")
		return
	}
	writeJSON(w, http.StatusOK, s.viewOf(rec))
}

// handlePlanApprove is POST /v1/plans/{hash}/approve, for admins. It lets one apply of exactly this
// plan through, within the approval's lifetime. The approver is the token's name, so it is on the
// record and in the audit log.
func (s *Server) handlePlanApprove(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !hashRe.MatchString(hash) {
		writeError(w, http.StatusNotFound, "not_found", "no such plan")
		return
	}
	by := "-"
	if h, ok := r.Context().Value(actorKey{}).(*actorHolder); ok {
		by = h.name
	}
	rec, err := s.opts.Plans.Approve(hash, by)
	switch {
	case errors.Is(err, ErrNoSuchPlan):
		writeError(w, http.StatusNotFound, "not_found", "no such plan: it is only known once it has been made (POST /v1/plan)")
		return
	case errors.Is(err, ErrNotApprovable):
		writeError(w, http.StatusConflict, "not_approvable", "only a plan with changes that is not blocked can be approved")
		return
	case errors.Is(err, ErrApprovalUsed):
		writeError(w, http.StatusConflict, "approval_used", "this plan was already applied; make a new plan")
		return
	case err != nil:
		log.Printf("approve plan %s: %v", hash, err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the approval")
		return
	}
	auditDetail(r, "approve plan=%s expires=%s", hash[:12], rec.Approval.Expires.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, s.viewOf(rec))
}

// handlePlanRevoke is DELETE /v1/plans/{hash}/approval, for admins: take an unused approval back.
func (s *Server) handlePlanRevoke(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !hashRe.MatchString(hash) {
		writeError(w, http.StatusNotFound, "not_found", "no such plan")
		return
	}
	rec, err := s.opts.Plans.Revoke(hash)
	switch {
	case errors.Is(err, ErrNoSuchPlan):
		writeError(w, http.StatusNotFound, "not_found", "no such plan")
		return
	case errors.Is(err, ErrApprovalUsed):
		writeError(w, http.StatusConflict, "approval_used", "this plan was already applied")
		return
	case err != nil:
		log.Printf("revoke plan %s: %v", hash, err)
		writeError(w, http.StatusInternalServerError, "internal", "could not withdraw the approval")
		return
	}
	auditDetail(r, "revoke approval plan=%s", hash[:12])
	writeJSON(w, http.StatusOK, s.viewOf(rec))
}
