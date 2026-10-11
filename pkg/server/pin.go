package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/selfupdate"
	"github.com/theta42/native-ops/pkg/serveflags"
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

var releaseRe = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// firstResumingRelease is the first release that resumes a deploy after upgrading to a pin. A pin to an
// older one is refused: that binary would never resume the deploy.
const firstResumingRelease = "v1.58.0"

// releaseBefore reports whether release a is older than b (both vX.Y.Z).
func releaseBefore(a, b string) bool {
	ma, mb := releaseRe.FindStringSubmatch(a), releaseRe.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return x < y
		}
	}
	return false
}

type deployResume struct {
	Job     JobID     `json:"job,omitempty"` // the deploy job that restarted the daemon; its record names the resumed one
	Tag     string    `json:"tag"`
	Version string    `json:"version"` // the version the daemon runs after the restart
	Actor   string    `json:"actor"`
	At      time.Time `json:"at"`
	// Reconfigure: the restart applied fleet.yml's daemon flags (Flags, normalized; empty for the unit's
	// own) rather than a new version.
	Reconfigure bool   `json:"reconfigure,omitempty"`
	Flags       string `json:"flags,omitempty"`
}

// DaemonFlags lets a deploy bring the daemon's flags in line with fleet.yml's daemon.flags
// (pkg/serveflags): what it runs with now, how to check a set against the flags it parses, and how to
// stage one for the next start.
type DaemonFlags struct {
	Running  string // the managed set it runs with, normalized ("" when Managed is false)
	Managed  bool   // a set from a deploy replaced the unit's flags at this start
	Validate func(flags string) error
	Stage    func(flags string) error
}

// followPin brings the daemon in line with fleet.yml's daemon section: its release first, then its flags.
// Either needs a restart; it returns the deploy's result then (ResultDaemonUpgraded or
// ResultDaemonReconfigured), and the daemon is restarting and the caller's job ends here. resume is the
// record of the restart this deploy resumed from, if any: a deploy restarts for a version at most once,
// and for flags at most once and not after a flags restart.
func (s *Server) followPin(ctx context.Context, root, tag, actor string, job JobID, resume *deployResume, logf func(string, ...any)) (string, error) {
	fleet, err := config.LoadFleetConfig(root)
	if err != nil {
		return "", err
	}
	pin := fleet.Daemon
	if pin != nil && pin.Version != "" {
		if upgraded, err := s.followVersion(ctx, pin, tag, actor, job, resume, logf); err != nil || upgraded {
			if err != nil {
				return "", err
			}
			return ResultDaemonUpgraded, nil
		}
	}
	want := ""
	if pin != nil {
		want = serveflags.Normalize(pin.Flags)
	}
	if reconfigured, err := s.followFlags(want, tag, actor, job, resume, logf); err != nil || reconfigured {
		if err != nil {
			return "", err
		}
		return ResultDaemonReconfigured, nil
	}
	return "", nil
}

func (s *Server) followVersion(ctx context.Context, pin *config.DaemonConfig, tag, actor string, job JobID, resume *deployResume, logf func(string, ...any)) (bool, error) {
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
	case releaseBefore(pin.Version, firstResumingRelease):
		return false, fmt.Errorf("fleet.yml pins the daemon at %s, older than %s, the first release that resumes a deploy after upgrading; raise the pin (or remove it), so nothing was deployed", pin.Version, firstResumingRelease)
	case resume != nil:
		return false, fmt.Errorf("fleet.yml pins the daemon at %s, but after restarting it is %s, so nothing was deployed", pin.Version, running)
	case s.opts.Upgrade == nil || s.opts.ResumeFile == "":
		return false, fmt.Errorf("fleet.yml pins the daemon at %s, but this daemon is %s and cannot upgrade itself (its binary must run from <state-dir>/bin, see docs/daemon.md), so nothing was deployed", pin.Version, running)
	}

	logf("fleet.yml pins the daemon at %s; this is %s. Upgrading first: the deploy of %s resumes on the new binary", pin.Version, running, tag)
	if err := s.opts.Upgrade(ctx, pin.Version, pin.SHA256, logf); err != nil {
		return false, fmt.Errorf("upgrade the daemon to %s: %w; nothing was deployed", pin.Version, err)
	}
	if err := s.writeResume(deployResume{Job: job, Tag: tag, Version: pin.Version, Actor: actor}); err != nil {
		return false, fmt.Errorf("%s is installed, but the deploy could not be recorded to resume after the restart (%v): the daemon runs %s from its next start; push the tag again then", pin.Version, err, pin.Version)
	}
	logf("restarting on %s; the deploy of %s continues as a new job once the new binary has served for a while", pin.Version, tag)
	go s.opts.Restart()
	return true, nil
}

// followFlags brings the daemon's flags in line with fleet.yml's daemon.flags (want, normalized; empty:
// the unit's own). A set is checked against the flags this binary parses before anything restarts, may
// not set --state-dir, and may not move --addr (the edge reaches the daemon there: a deploy that moved it
// would cut its own way back off). After the restart a set that keeps the daemon from starting is dropped
// by the next start (pkg/serveflags), and the resumed deploy records the failure.
func (s *Server) followFlags(want, tag, actor string, job JobID, resume *deployResume, logf func(string, ...any)) (bool, error) {
	f := s.opts.Flags
	if f == nil {
		if want != "" {
			logf("fleet.yml sets daemon flags, which this daemon does not apply (it was started without a state directory it manages)")
		}
		return false, nil
	}
	describe := func(managed bool, set string) string {
		if !managed {
			return "the unit's flags"
		}
		return "\"" + set + "\""
	}
	switch {
	case want == "" && !f.Managed:
		return false, nil
	case want != "" && f.Managed && want == f.Running:
		logf("the daemon runs the flags fleet.yml sets")
		return false, nil
	case resume != nil && resume.Reconfigure:
		return false, fmt.Errorf("fleet.yml sets the daemon's flags to %s, but after restarting it runs %s (the new flags did not start, and the daemon went back to its previous ones; see journalctl -u native-ops-serve), so nothing was deployed",
			describe(want != "", want), describe(f.Managed, f.Running))
	}
	if want != "" {
		if err := f.Validate(want); err != nil {
			return false, fmt.Errorf("fleet.yml's daemon flags: %w; nothing was deployed", err)
		}
		if addr, ok := serveflags.Value(strings.Fields(want), "addr"); ok && addr != s.opts.Addr {
			return false, fmt.Errorf("fleet.yml's daemon flags move --addr from %s to %s; a deploy cannot do that (the edge reaches the daemon at its address, so the deploy would cut itself off): change it on the host, or leave --addr out of daemon.flags; nothing was deployed", s.opts.Addr, addr)
		}
	}
	if s.opts.Restart == nil || s.opts.ResumeFile == "" {
		return false, fmt.Errorf("fleet.yml changes the daemon's flags, but this daemon cannot restart itself; nothing was deployed")
	}
	logf("the daemon runs %s; fleet.yml sets %s. Restarting with them first: the deploy of %s continues afterwards", describe(f.Managed, f.Running), describe(want != "", want), tag)
	if err := f.Stage(want); err != nil {
		return false, fmt.Errorf("could not store the daemon's flags (%v); nothing was deployed", err)
	}
	if err := s.writeResume(deployResume{Job: job, Tag: tag, Version: s.opts.Version, Actor: actor, Reconfigure: true, Flags: want}); err != nil {
		return false, fmt.Errorf("the flags are stored, but the deploy could not be recorded to resume after the restart (%v): they apply at the daemon's next start; push the tag again then", err)
	}
	go s.opts.Restart()
	return true, nil
}

func (s *Server) writeResume(r deployResume) error {
	r.At = time.Now().UTC()
	b, _ := json.Marshal(r)
	return os.WriteFile(s.opts.ResumeFile, b, 0o600)
}

// ResumeDeploy runs a deploy that an upgrade to fleet.yml's daemon pin interrupted. The daemon calls it
// at startup, once a pending upgrade has committed. The record is used once: if this daemon is not the
// version the deploy upgraded to (the new binary did not stay up and was rolled back), the deploy is
// recorded as a failed job instead of run, as it is when the restart was for fleet.yml's daemon flags and
// they did not start. Either way the job that restarted names the new one as its next, so a client
// waiting on it (`native-ops remote deploy`) follows the deploy to its real end.
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
	msg := ""
	switch f := s.opts.Flags; {
	case r.Version != s.opts.Version:
		msg = fmt.Sprintf("the deploy of %s upgraded the daemon to %s, but %s is running (the new binary did not stay up and was rolled back; see journalctl -u native-ops-serve), so it did not run", r.Tag, r.Version, s.opts.Version)
	case r.Reconfigure && (f == nil || f.Managed != (r.Flags != "") || f.Running != r.Flags):
		msg = fmt.Sprintf("the deploy of %s restarted the daemon with fleet.yml's flags, but they did not start, and it went back to its previous ones (see journalctl -u native-ops-serve), so it did not run", r.Tag)
	}
	if msg != "" {
		log.Print(msg)
		if job, err := s.opts.Jobs.CreateKind("deploy", r.Actor, "", r.Tag, ""); err == nil {
			s.opts.Jobs.Logf(job.ID, "%s", msg)
			s.opts.Jobs.Finish(job.ID, errors.New(msg))
			s.linkNext(r.Job, job.ID)
		}
		return
	}
	job, err := s.startDeploy(r.Tag, r.Actor, &r)
	if err != nil {
		log.Printf("deploy resume: could not resume the deploy of %s: %v", r.Tag, err)
		return
	}
	s.linkNext(r.Job, job.ID)
	log.Printf("resumed the deploy of %s on %s as job %s", r.Tag, s.opts.Version, job.ID)
}

// linkNext records on a deploy that upgraded the daemon which job carries it on.
func (s *Server) linkNext(from, next JobID) {
	if validJobID(string(from)) {
		s.opts.Jobs.update(from, func(j *Job) { j.Next = next })
	}
}
