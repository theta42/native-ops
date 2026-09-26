package server

import (
	"context"
	"errors"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
	"github.com/theta42/native-ops/pkg/incus"
)

// PlanFunc plans an unpacked configuration directory against the host (only one service when
// service is set). A configuration it cannot use is reported as engine.ErrBadConfig; any other
// error means the host could not be read.
type PlanFunc func(ctx context.Context, configDir, service string) (*engine.FleetPlan, error)

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// maxConcurrentPlans bounds how many uploads are unpacked and planned at once.
const maxConcurrentPlans = 4

// handlePlan is POST /v1/plan?service=<name>&sha=<commit>. The body is a gzip-compressed tar of
// the native-ops-conf tree (fleet.yml at its root, or inside one top-level directory), which
// the caller's CI checked out from git: the daemon needs no git access and no credentials.
// The tree is unpacked into a private temporary directory, planned against the host without
// changing it, and removed.
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	service, sha := q.Get("service"), q.Get("sha")
	if service != "" && !incus.ValidName(service) {
		writeError(w, http.StatusBadRequest, "bad_request", "service is not a valid service name")
		return
	}
	if sha != "" && !shaRe.MatchString(sha) {
		writeError(w, http.StatusBadRequest, "bad_request", "sha must be 7 to 64 lowercase hex characters")
		return
	}
	if ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil ||
		(ct != "application/gzip" && ct != "application/x-gzip" && ct != "application/octet-stream") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "send the configuration as a gzip-compressed tar (Content-Type: application/gzip)")
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

	dir, err := os.MkdirTemp("", "native-ops-plan-")
	if err != nil {
		log.Printf("plan: temp dir: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not prepare the configuration")
		return
	}
	defer os.RemoveAll(dir)

	body := http.MaxBytesReader(w, r.Body, maxArchiveBytes+1)
	if err := ExtractTarGz(body, dir); err != nil {
		var ae *ArchiveError
		if errors.As(err, &ae) {
			auditDetail(r, "plan rejected: bad archive")
			writeError(w, http.StatusBadRequest, "bad_archive", ae.Error())
			return
		}
		log.Printf("plan: extract: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not unpack the configuration")
		return
	}
	root, ok := configRoot(dir)
	if !ok {
		auditDetail(r, "plan rejected: no fleet.yml")
		writeError(w, http.StatusBadRequest, "bad_archive", "the archive has no fleet.yml at its root (or inside a single top-level directory)")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	fp, err := s.opts.Plan(ctx, root, service)
	if err != nil {
		if errors.Is(err, engine.ErrBadConfig) {
			auditDetail(r, "plan rejected: bad configuration")
			writeError(w, http.StatusBadRequest, "bad_config", err.Error())
			return
		}
		log.Printf("plan: %v", err)
		writeError(w, http.StatusBadGateway, "host_unavailable", "could not read the host's state")
		return
	}
	auditDetail(r, "plan sha=%s service=%s exit=%d hash=%s", orDash(sha), orDash(service), fp.ExitStatus(), fp.Hash()[:12])
	writeJSON(w, http.StatusOK, map[string]any{
		"sha":  sha,
		"hash": fp.Hash(),
		"exit": fp.ExitStatus(),
		"plan": fp,
		"text": fp.Render(),
	})
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// configRoot finds the directory holding fleet.yml: dir itself, or the only directory in it
// (what `git archive --prefix=conf/` and many tar invocations produce).
func configRoot(dir string) (string, bool) {
	if fileExists(filepath.Join(dir, "fleet.yml")) {
		return dir, true
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return "", false
	}
	sub := filepath.Join(dir, entries[0].Name())
	return sub, fileExists(filepath.Join(sub, "fleet.yml"))
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
