package server

import (
	"context"
	"net/http"
	"regexp"
)

// Maintenance jobs: the recurring work a fleet needs besides deploying -- backups and retention --
// driven from CI like everything else. A scheduled pipeline uploads the configuration tree it checked
// out (fleet.yml holds the backup settings), and the daemon runs the work as a job. The daemon holds the
// credentials (the object store's keys, in its environment); CI holds
// only a token. There is no scheduler in the daemon: the pipeline's schedule is the schedule.
//
//	POST /v1/backups?volume=<name>&prune=1          deployer: back up one volume, or every allowlisted
//	                                                  volume without volume=; prune=1 then applies retention
//	POST /v1/backups/restore?volume=<name>&from=<key|latest>&as=<new name>&force=1
//	                                                  admin: restore a volume (in place, or as a new volume)
//
// DNS records are not maintenance: they are part of the plan, and change with a deploy tag.

// BackupRequest says what POST /v1/backups should back up.
type BackupRequest struct {
	Volume string // "" means every volume fleet.yml allows
	Prune  bool   // apply retention afterwards
}

// RestoreRequest says what POST /v1/backups/restore should restore.
type RestoreRequest struct {
	Volume string
	From   string // an object key, or "latest"
	As     string // restore under this new name instead of in place
	Force  bool   // stop the containers that mount the volume to restore it in place
}

// BackupFunc backs up as fleet.yml in configDir says. RestoreFunc restores.
type (
	BackupFunc  func(ctx context.Context, configDir string, req BackupRequest, logf func(string, ...any)) error
	RestoreFunc func(ctx context.Context, configDir string, req RestoreRequest, logf func(string, ...any)) error
)

var (
	objectKeyRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,300}$`)
	volumeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

func validVolumeName(s string) bool { return volumeNameRe.MatchString(s) }

// handleBackup is POST /v1/backups.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := BackupRequest{Volume: q.Get("volume"), Prune: q.Get("prune") == "1" || q.Get("prune") == "true"}
	if req.Volume != "" && !validVolumeName(req.Volume) {
		writeError(w, http.StatusBadRequest, "bad_request", "volume is not a valid volume name")
		return
	}
	what := "backup of every allowed volume"
	if req.Volume != "" {
		what = "backup of " + req.Volume
	}
	s.runTreeJob(w, r, "backup", req.Volume, what, func(ctx context.Context, root string, logf func(string, ...any)) error {
		return s.opts.Backup(ctx, root, req, logf)
	})
}

// handleRestore is POST /v1/backups/restore. It replaces data, so it needs an admin.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := RestoreRequest{Volume: q.Get("volume"), From: q.Get("from"), As: q.Get("as"), Force: q.Get("force") == "1" || q.Get("force") == "true"}
	if req.From == "" {
		req.From = "latest"
	}
	switch {
	case !validVolumeName(req.Volume):
		writeError(w, http.StatusBadRequest, "bad_request", "volume (the volume whose backup to restore) is required")
		return
	case req.As != "" && !validVolumeName(req.As):
		writeError(w, http.StatusBadRequest, "bad_request", "as is not a valid volume name")
		return
	case req.From != "latest" && !objectKeyRe.MatchString(req.From):
		writeError(w, http.StatusBadRequest, "bad_request", "from must be an object key or latest")
		return
	}
	what := "restore of " + req.Volume + " from " + req.From
	if req.As != "" {
		what += " as " + req.As
	}
	s.runTreeJob(w, r, "restore", req.Volume, what, func(ctx context.Context, root string, logf func(string, ...any)) error {
		return s.opts.Restore(ctx, root, req, logf)
	})
}

// goneToDeployTag answers a retired endpoint whose work is now part of the plan: 410, naming the way.
func goneToDeployTag(what string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusGone, "gone", what+" is part of the plan now: commit it to the configuration repository and push a deploy tag")
	}
}

// runTreeJob receives an uploaded configuration tree, takes the host lock, and runs work on the tree as
// a job of the given kind. It answers 202 with the job (or the reason it could not start).
func (s *Server) runTreeJob(w http.ResponseWriter, r *http.Request, kind, subject, what string, work func(ctx context.Context, root string, logf func(string, ...any)) error) {
	u, ok := s.receiveTree(w, r, what)
	if !ok {
		return
	}
	if !s.applyMu.TryLock() {
		u.discard()
		msg := "another change is already running on this host"
		if j, ok := s.opts.Jobs.Running(); ok {
			msg += " (" + string(j.ID) + ")"
		}
		auditDetail(r, "%s rejected: busy", what)
		writeError(w, http.StatusConflict, "busy", msg)
		return
	}
	actor, _ := s.actorScope(r)
	job, err := s.opts.Jobs.CreateKind(kind, actor, u.sha, subject, "")
	if err != nil {
		s.applyMu.Unlock()
		u.discard()
		writeError(w, http.StatusInternalServerError, "internal", "could not record the job, so nothing was done")
		return
	}
	auditDetail(r, "%s job=%s sha=%s", what, job.ID, orDash(u.sha))
	s.startJob(job.ID, actor, what, kind, u.discard, func(ctx context.Context, logf func(string, ...any)) error {
		return work(ctx, u.root, logf)
	})
	w.Header().Set("Location", "/v1/jobs/"+string(job.ID))
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}
