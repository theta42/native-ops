package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"time"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/selfupdate"
)

// fleet.yml's `daemon: {version, sha256}` is the native-ops release the configuration was written for.
// Cloud-init installs it on a host that reconcile creates; a deploy moves a running daemon to it. So a
// daemon upgrade is a reviewed change to fleet.yml and a protected deploy tag, like any other change,
// and CI never needs an admin token for it.
//
// A deploy whose daemon is not on the pin upgrades first (a checksum-verified install, as
// POST /v1/daemon/upgrade does), records the deploy in ResumeFile, and restarts. The new binary, once
// its upgrade has committed, resumes the deploy as a new job (ResumeDeploy), so the plan and the apply
// run on the version the commit pins. If the new binary does not stay up, the unit's guard puts the old
// one back, and that one records the deploy as failed instead of running it.

var releaseRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

type deployResume struct {
	Tag     string    `json:"tag"`
	Version string    `json:"version"` // the version the deploy upgraded to
	Actor   string    `json:"actor"`
	At      time.Time `json:"at"`
}

// followPin moves the daemon to fleet.yml's pin. It reports whether it upgraded, in which case the
// daemon is restarting and the caller's job ends here.
func (s *Server) followPin(ctx context.Context, root, tag, actor string, resumed bool, logf func(string, ...any)) (bool, error) {
	fleet, err := config.LoadFleetConfig(root)
	if err != nil {
		return false, err
	}
	pin := fleet.Daemon
	if pin == nil || pin.Version == "" {
		return false, nil
	}
	if err := selfupdate.Validate(pin.Version, pin.SHA256); err != nil {
		return false, fmt.Errorf("fleet.yml's daemon pin: %w", err)
	}
	running := s.opts.Version
	switch {
	case pin.Version == running:
		logf("the daemon is %s, as fleet.yml pins", running)
		return false, nil
	case !releaseRe.MatchString(running):
		logf("fleet.yml pins the daemon at %s; this daemon is a %q build, which is left as it is", pin.Version, running)
		return false, nil
	case resumed:
		return false, fmt.Errorf("fleet.yml pins the daemon at %s, but after upgrading it is %s, so nothing was deployed", pin.Version, running)
	case s.opts.Upgrade == nil || s.opts.ResumeFile == "":
		return false, fmt.Errorf("fleet.yml pins the daemon at %s, but this daemon is %s and cannot upgrade itself (its binary must run from <state-dir>/bin, see docs/daemon.md), so nothing was deployed", pin.Version, running)
	}

	logf("fleet.yml pins the daemon at %s; this is %s. Upgrading first: the deploy of %s resumes on the new binary", pin.Version, running, tag)
	if err := s.opts.Upgrade(ctx, pin.Version, pin.SHA256, logf); err != nil {
		return false, fmt.Errorf("upgrade the daemon to %s: %w; nothing was deployed", pin.Version, err)
	}
	b, _ := json.Marshal(deployResume{Tag: tag, Version: pin.Version, Actor: actor, At: time.Now().UTC()})
	if err := os.WriteFile(s.opts.ResumeFile, b, 0o600); err != nil {
		return false, fmt.Errorf("%s is installed, but the deploy could not be recorded to resume after the restart (%v): the daemon runs %s from its next start; push the tag again then", pin.Version, err, pin.Version)
	}
	logf("restarting on %s; the deploy of %s continues as a new job once the new binary has served for a while", pin.Version, tag)
	go s.opts.Restart()
	return true, nil
}

// ResumeDeploy runs a deploy that an upgrade to fleet.yml's daemon pin interrupted. The daemon calls it
// at startup, once a pending upgrade has committed. The record is used once: if this daemon is not the
// version the deploy upgraded to (the new binary did not stay up and was rolled back), the deploy is
// recorded as a failed job instead of run.
func (s *Server) ResumeDeploy() {
	if s.opts.ResumeFile == "" || s.opts.Deploy == nil || s.opts.Jobs == nil {
		return
	}
	b, err := os.ReadFile(s.opts.ResumeFile)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	_ = os.Remove(s.opts.ResumeFile)
	var r deployResume
	if err != nil || json.Unmarshal(b, &r) != nil || r.Tag == "" {
		log.Printf("deploy resume: unreadable record %s, ignored", s.opts.ResumeFile)
		return
	}
	if r.Version != s.opts.Version {
		msg := fmt.Sprintf("the deploy of %s upgraded the daemon to %s, but %s is running (the new binary did not stay up and was rolled back; see journalctl -u native-ops-serve), so it did not run", r.Tag, r.Version, s.opts.Version)
		log.Print(msg)
		if job, err := s.opts.Jobs.CreateKind("deploy", r.Actor, "", r.Tag, ""); err == nil {
			s.opts.Jobs.Logf(job.ID, "%s", msg)
			s.opts.Jobs.Finish(job.ID, errors.New(msg))
		}
		return
	}
	job, err := s.startDeploy(r.Tag, r.Actor, true)
	if err != nil {
		log.Printf("deploy resume: could not resume the deploy of %s: %v", r.Tag, err)
		return
	}
	log.Printf("resumed the deploy of %s on %s as job %s", r.Tag, s.opts.Version, job.ID)
}
