package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// DeploySource is where POST /v1/deploy reads a tagged commit from: the configuration repository on its
// git server (pkg/gitsource). The daemon resolves and fetches the tree itself, so a deploy never runs
// anything a CI job uploaded.
type DeploySource interface {
	Commit(ctx context.Context, tag string) (string, error)
	Protected(ctx context.Context, tag string) (string, error)
	Archive(ctx context.Context, sha string) (io.ReadCloser, error)
}

// handleDeploy is POST /v1/deploy {"tag": "deploy-2026.10.03"} (deployer). Pushing a protected deploy tag
// is the approval: only the people the repository allows can push one. The job
//
//  1. resolves the tag on the git server and checks that a protection rule covers it;
//  2. downloads that commit's tree from the git server (nothing comes from the caller);
//  3. plans it, refusing a blocked plan, and records the plan as approved by the tag;
//  4. applies it.
//
// A plan with nothing to change ends the job successfully without applying. The job and the audit log
// name the tag and the commit.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
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
	actor, _ := s.actorScope(r)
	job, err := s.startDeploy(body.Tag, actor, false)
	var busy errBusy
	switch {
	case errors.As(err, &busy):
		writeError(w, http.StatusConflict, "busy", busy.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", "could not record the job, so nothing was deployed")
		return
	}
	auditDetail(r, "deploy %s job=%s", body.Tag, job.ID)
	w.Header().Set("Location", "/v1/jobs/"+string(job.ID))
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job, "tag": body.Tag})
}

type errBusy string

func (e errBusy) Error() string { return string(e) }

// startDeploy starts the deploy job for a tag. resumed is a deploy that an upgrade to fleet.yml's
// daemon pin interrupted, now running on the new binary: it never upgrades again.
func (s *Server) startDeploy(tag, actor string, resumed bool) (Job, error) {
	if !s.applyMu.TryLock() {
		msg := "another change is already running on this host"
		if j, ok := s.opts.Jobs.Running(); ok {
			msg += " (" + string(j.ID) + ")"
		}
		return Job{}, errBusy(msg)
	}
	job, err := s.opts.Jobs.CreateKind("deploy", actor, "", tag, "")
	if err != nil {
		s.applyMu.Unlock()
		return Job{}, err
	}
	var tmp string
	cleanup := func() {
		if tmp != "" {
			_ = os.RemoveAll(tmp)
		}
	}
	what := "deploy of " + tag
	if resumed {
		what += " (resumed on the upgraded daemon)"
	}
	s.startJob(job.ID, actor, what, "deploy "+tag, cleanup, func(ctx context.Context, logf func(string, ...any)) error {
		sha, err := s.opts.Deploy.Commit(ctx, tag)
		if err != nil {
			return err
		}
		rule, err := s.opts.Deploy.Protected(ctx, tag)
		if err != nil {
			return err
		}
		logf("tag %s is commit %s, protected by the rule %q", tag, sha[:12], rule)

		tmp, err = os.MkdirTemp("", "native-ops-deploy-")
		if err != nil {
			return err
		}
		rc, err := s.opts.Deploy.Archive(ctx, sha)
		if err != nil {
			return err
		}
		err = ExtractTarGz(rc, tmp)
		rc.Close()
		if err != nil {
			return fmt.Errorf("unpack commit %s: %w", sha[:12], err)
		}
		root, ok := configRoot(tmp)
		if !ok {
			return fmt.Errorf("commit %s has no fleet.yml", sha[:12])
		}

		// fleet.yml's daemon pin is the version the commit was written for: move to it first, so the
		// plan and the apply run on that binary.
		if upgraded, err := s.followPin(ctx, root, tag, actor, resumed, logf); err != nil || upgraded {
			return err
		}

		pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		fp, err := s.opts.Plan(pctx, root, "")
		cancel()
		if err != nil {
			return fmt.Errorf("plan commit %s: %w", sha[:12], err)
		}
		hash := fp.Hash()
		s.opts.Plans.Record(PlanSeen{Hash: hash, Actor: actor, Sha: sha, Service: "", Exit: fp.ExitStatus(), Counts: fp.Counts(), Text: fp.Render()})
		logf("plan %s:\n%s", hash[:12], fp.Render())
		switch {
		case fp.Blocked():
			return errors.New("the plan is blocked: apply would fail, so nothing was changed")
		case !fp.Pending():
			logf("nothing to change: the host already matches %s", tag)
			return nil
		}
		if err := s.opts.Plans.UseForDeploy(hash, "tag "+tag+" ("+sha[:12]+")", job.ID); err != nil {
			return fmt.Errorf("record the deploy: %w", err)
		}
		return s.opts.Apply(ctx, root, fp, logf)
	})
	return job, nil
}

// lastHostChange is when a job that changes what a plan compares against last finished: an apply, a
// deploy, an instance change, an edge apply, a restore or an image build. A plan made before it was
// made against a host that no longer exists, so it is stale.
func (s *Server) lastHostChange() time.Time {
	var last time.Time
	if s.opts.Jobs == nil {
		return last
	}
	for _, j := range s.opts.Jobs.List() {
		if j.Finished == nil || !changesHost(j.Kind) {
			continue
		}
		if j.Finished.After(last) {
			last = *j.Finished
		}
	}
	return last
}

func changesHost(kind string) bool {
	switch {
	case kind == "apply", kind == "deploy", kind == "edge:apply", kind == "restore", kind == "image:build":
		return true
	case len(kind) > 9 && kind[:9] == "instance:":
		return true
	}
	return false
}
