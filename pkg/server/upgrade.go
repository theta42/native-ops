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
	// An upgrade restarts the daemon, and a restart ends the queue (jobs still waiting never run), so it
	// does not wait in line: it needs the host free and nothing waiting.
	actor, _ := s.actorScope(r)
	version, sha := body.Version, body.SHA256
	job, err := s.runJobNow(jobSpec{kind: "daemon:upgrade", actor: actor, subject: version, what: "daemon upgrade to " + version, audit: "daemon upgrade",
		work: func(ctx context.Context, logf func(string, ...any)) error {
			if err := s.opts.Upgrade(ctx, version, sha, logf); err != nil {
				return err
			}
			logf("restarting on %s (the job record is kept; GET /healthz shows the running version)", version)
			go s.opts.Restart()
			return nil
		}})
	if err != nil {
		s.answerNotStarted(w, r, "daemon upgrade to "+version, err)
		return
	}
	auditDetail(r, "daemon upgrade to %s job=%s (from %s)", version, job.ID, s.opts.Version)
	answerJob(w, job, map[string]any{"from": s.opts.Version, "to": version})
}
