package server

import (
	"context"
	"net/http"

	"github.com/theta42/native-ops/pkg/selfupdate"
)

// UpgradeFunc installs a pinned release of the daemon in place of the running binary (see
// pkg/selfupdate). It does not restart anything; RestartFunc does, once the job is recorded.
type (
	UpgradeFunc func(ctx context.Context, version, sha256 string, logf func(string, ...any)) error
	RestartFunc func()
)

// handleDaemonUpgrade is POST /v1/daemon/upgrade {version, sha256} (admin): upgrade this daemon to a
// pinned release, as a job. When the job has succeeded the daemon restarts on the new binary, after any
// running job ends; a binary that cannot stay up is swapped back by the unit's guard. GET /healthz names
// the version that is running, which is how a caller sees the upgrade land.
func (s *Server) handleDaemonUpgrade(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version string `json:"version"`
		SHA256  string `json:"sha256"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := selfupdate.Validate(body.Version, body.SHA256); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !s.applyMu.TryLock() {
		msg := "another change is already running on this host"
		if j, ok := s.opts.Jobs.Running(); ok {
			msg += " (" + string(j.ID) + ")"
		}
		writeError(w, http.StatusConflict, "busy", msg)
		return
	}
	actor, _ := s.actorScope(r)
	job, err := s.opts.Jobs.CreateKind("daemon:upgrade", actor, "", body.Version, "")
	if err != nil {
		s.applyMu.Unlock()
		writeError(w, http.StatusInternalServerError, "internal", "could not record the job, so nothing was changed")
		return
	}
	auditDetail(r, "daemon upgrade to %s job=%s (from %s)", body.Version, job.ID, s.opts.Version)
	version, sha := body.Version, body.SHA256
	s.startJob(job.ID, actor, "daemon upgrade to "+version, "daemon upgrade", nil, func(ctx context.Context, logf func(string, ...any)) error {
		if err := s.opts.Upgrade(ctx, version, sha, logf); err != nil {
			return err
		}
		logf("restarting on %s (the job record is kept; GET /healthz shows the running version)", version)
		go s.opts.Restart()
		return nil
	})
	w.Header().Set("Location", "/v1/jobs/"+string(job.ID))
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job, "from": s.opts.Version, "to": version})
}
