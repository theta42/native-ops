package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/engine"
)

// A deploy is a job of kind "deploy" (see deploy.go). These endpoints answer the questions asked about
// deploys, without reading job logs: what the host runs now, what ran before, and what a tag would
// change before anyone pushes it.

// DeployView is one deploy, as GET /v1/deploys reports it.
type DeployView struct {
	Job      JobID          `json:"job"`
	Tag      string         `json:"tag"`
	Sha      string         `json:"sha,omitempty"`
	Status   JobStatus      `json:"status"`
	Result   string         `json:"result,omitempty"`
	Actor    string         `json:"actor"`
	Started  time.Time      `json:"started"`
	Finished *time.Time     `json:"finished,omitempty"`
	Error    string         `json:"error,omitempty"`
	PlanHash string         `json:"plan_hash,omitempty"`
	Counts   map[string]int `json:"counts,omitempty"`
}

func (s *Server) deployView(j Job) DeployView {
	d := DeployView{Job: j.ID, Tag: j.Service, Sha: j.Sha, Status: j.Status, Result: j.Result, Actor: j.Actor,
		Started: j.Created, Finished: j.Finished, Error: j.Error, PlanHash: j.PlanHash}
	if j.PlanHash != "" && s.opts.Plans != nil {
		if rec, ok := s.opts.Plans.Get(j.PlanHash); ok {
			d.Counts = rec.Counts
		}
	}
	return d
}

// live reports whether a deploy left the host matching its tag.
func (d DeployView) live() bool {
	return d.Status == JobSucceeded && (d.Result == ResultApplied || d.Result == ResultNoChanges)
}

// handleDeploys is GET /v1/deploys?limit=50 (viewer): the deploys, newest first; current, the newest one
// that left the host matching its tag (what it runs now, as far as deploys go: an apply, an instance
// change or an image build since then is in /v1/jobs); and running, the deploy in progress, if any.
func (s *Server) handleDeploys(w http.ResponseWriter, r *http.Request) {
	limit, ok := queryLimit(w, r, 50)
	if !ok {
		return
	}
	var (
		all             []DeployView
		current, active *DeployView
	)
	for _, j := range s.opts.Jobs.List() {
		if j.Kind != "deploy" {
			continue
		}
		d := s.deployView(j)
		if current == nil && d.live() {
			c := d
			current = &c
		}
		if active == nil && d.Status == JobRunning {
			a := d
			active = &a
		}
		if len(all) < limit {
			all = append(all, d)
		}
	}
	if all == nil {
		all = []DeployView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"current": current, "running": active, "deploys": all})
}

// queryLimit reads ?limit= (1..keepJobs, default def).
func queryLimit(w http.ResponseWriter, r *http.Request, def int) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > keepJobs {
		writeError(w, http.StatusBadRequest, "bad_request", "limit must be a number from 1 to "+strconv.Itoa(keepJobs))
		return 0, false
	}
	return n, true
}

// handleDeployPlan is POST /v1/deploy/plan {"tag": "deploy-2026.10.03"} (planner): what deploying that tag
// would change, without changing anything. The daemon resolves the tag and reads its commit from the git
// server, as a deploy does, and plans it against the host now. It answers at once (no job):
//
//   - deployable is false, with the reason, when POST /v1/deploy would refuse the tag (no protection
//     rule covers it); the plan is still made, so a tag can be checked before it is protected;
//   - daemon names the running release and fleet.yml's pin; when they differ the deploy would upgrade
//     first, and the plan, made by this release, may differ from the one the pinned release makes.
//
// The plan is recorded like any other (GET /v1/plans).
func (s *Server) handleDeployPlan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tag string `json:"tag"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Tag == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "tag is required")
		return
	}
	select {
	case s.planSlots <- struct{}{}:
		defer func() { <-s.planSlots }()
	default:
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "busy", "too many plans are running; retry shortly")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()

	sha, err := s.opts.Deploy.Commit(ctx, body.Tag)
	if err != nil {
		auditDetail(r, "deploy plan %s: tag not resolved", body.Tag)
		writeError(w, http.StatusUnprocessableEntity, "tag_unresolved", err.Error())
		return
	}
	deployable, reason := true, ""
	rule, err := s.opts.Deploy.Protected(ctx, body.Tag)
	if err != nil {
		deployable, reason = false, err.Error()
	}
	dir, root, err := s.fetchCommit(ctx, sha)
	if dir != "" {
		defer os.RemoveAll(dir)
	}
	if err != nil {
		log.Printf("deploy plan %s: %v", body.Tag, err)
		writeError(w, http.StatusBadGateway, "git_unavailable", "could not read commit "+sha[:12]+" from the git server: "+err.Error())
		return
	}

	daemon := map[string]any{"running": s.opts.Version}
	if fleet, err := config.LoadFleetConfig(root); err == nil && fleet.Daemon != nil && fleet.Daemon.Version != "" {
		daemon["pinned"] = fleet.Daemon.Version
		daemon["upgrade"] = fleet.Daemon.Version != s.opts.Version && releaseRe.MatchString(s.opts.Version)
	}

	pctx, pcancel := context.WithTimeout(ctx, 60*time.Second)
	fp, err := s.opts.Plan(pctx, root, "")
	pcancel()
	if err != nil {
		if errors.Is(err, engine.ErrBadConfig) {
			auditDetail(r, "deploy plan %s: bad configuration", body.Tag)
			writeError(w, http.StatusBadRequest, "bad_config", err.Error())
			return
		}
		log.Printf("deploy plan %s: %v", body.Tag, err)
		writeError(w, http.StatusBadGateway, "host_unavailable", "could not read the host's state")
		return
	}
	actor, _ := s.actorScope(r)
	hash := fp.Hash()
	s.opts.Plans.Record(PlanSeen{Hash: hash, Actor: actor, Sha: sha, Exit: fp.ExitStatus(), Counts: fp.Counts(), Text: fp.Render()})
	auditDetail(r, "deploy plan %s sha=%s exit=%d hash=%s", body.Tag, sha[:12], fp.ExitStatus(), hash[:12])
	out := map[string]any{
		"tag": body.Tag, "sha": sha, "deployable": deployable, "daemon": daemon,
		"hash": hash, "exit": fp.ExitStatus(), "counts": fp.Counts(), "plan": fp, "text": fp.Render(),
	}
	if deployable {
		out["protected_by"] = rule
	} else {
		out["reason"] = reason
	}
	writeJSON(w, http.StatusOK, out)
}
