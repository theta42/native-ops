package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// flagsRig is a deploy rig whose fleet.yml sets daemon.flags, and whose daemon runs a given set (or the
// unit's own). Staging and restarting are recorded, not done.
type flagsRig struct {
	*applyRig
	resume   string
	staged   []string
	restarts chan struct{}
	flags    *DaemonFlags
}

func newFlagsRig(t *testing.T, fleetFlags, running string, managed bool) *flagsRig {
	t.Helper()
	fleet := "name: f\ndaemon:\n  version: v1.73.0\n  sha256: " + pinSHA + "\n"
	if fleetFlags != "" {
		fleet += "  flags: \"" + fleetFlags + "\"\n"
	}
	r := &flagsRig{resume: filepath.Join(t.TempDir(), "deploy-resume.json"), restarts: make(chan struct{}, 4)}
	r.flags = &DaemonFlags{Running: running, Managed: managed,
		Validate: func(set string) error {
			if strings.Contains(set, "--no-such-flag") {
				return errors.New("flag provided but not defined: -no-such-flag")
			}
			return nil
		},
		Stage: func(set string) error { r.staged = append(r.staged, set); return nil }}
	src := &fakeSource{protected: true, tree: tgz(t, entry{name: "conf/fleet.yml", body: fleet}, entry{name: "conf/services/web/service.yml", body: "image: x\n"})}
	r.applyRig = newApplyRigWith(t, func(o *Options) {
		o.Deploy, o.DeployTags, o.Version, o.Addr = src, "deploy-*", "v1.73.0", "10.0.100.1:8686"
		o.ResumeFile, o.Flags = r.resume, r.flags
		o.Restart = func() { r.restarts <- struct{}{} }
	})
	return r
}

func (r *flagsRig) deploy(t *testing.T, want JobStatus) Job {
	t.Helper()
	res, body := r.post(t, "/v1/deploy", r.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	return r.waitJob(t, jobID(t, body), want)
}

func (r *flagsRig) appliedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.applied)
}

// The opsavor case: the unit still carries a retired flag, and fleet.yml now sets the daemon's flags. The
// deploy stores them, restarts the daemon with them and resumes; nobody edits the host's unit.
func TestADeployAppliesFleetYmlsDaemonFlags(t *testing.T) {
	r := newFlagsRig(t, "--enable-apply   --image-prefix opsavor-", "", false)
	j := r.deploy(t, JobSucceeded)
	select {
	case <-r.restarts:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon must restart with the new flags")
	}
	if j.Result != ResultDaemonReconfigured || r.appliedCount() != 0 {
		t.Fatalf("the deploy ends with the restart and applies nothing on these flags: %+v, applied %d", j, r.appliedCount())
	}
	if len(r.staged) != 1 || r.staged[0] != "--enable-apply --image-prefix opsavor-" {
		t.Fatalf("staged: %q", r.staged)
	}
	if !strings.Contains(j.Log, "the unit's flags") || !strings.Contains(j.Log, "--image-prefix opsavor-") {
		t.Fatalf("the log says what changes: %s", j.Log)
	}
	b, err := os.ReadFile(r.resume)
	if err != nil || !strings.Contains(string(b), `"reconfigure":true`) || !strings.Contains(string(b), `"job":"`+string(j.ID)+`"`) {
		t.Fatalf("resume record: %s %v", b, err)
	}
}

func TestTheRestartedDaemonResumesTheDeployOnItsNewFlags(t *testing.T) {
	r := newFlagsRig(t, "--enable-apply", "--enable-apply", true) // started with the staged set
	_ = os.WriteFile(r.resume, []byte(`{"tag":"deploy-1","version":"v1.73.0","actor":"ci","reconfigure":true,"flags":"--enable-apply"}`), 0o600)
	r.serverUnderTest.ResumeDeploy()
	j := r.waitLatest(t, JobSucceeded)
	if r.appliedCount() != 1 || len(r.staged) != 0 || !strings.Contains(j.Log, "runs the flags fleet.yml sets") {
		t.Fatalf("applied %d, staged %q, log: %s", r.appliedCount(), r.staged, j.Log)
	}
}

// The new flags did not start, so the daemon went back to its old ones: the deploy is recorded as failed,
// and it does not try again (a loop of restarts).
func TestFlagsThatDidNotStartFailTheDeploy(t *testing.T) {
	r := newFlagsRig(t, "--enable-apply", "", false) // back on the unit's flags
	_ = os.WriteFile(r.resume, []byte(`{"tag":"deploy-1","version":"v1.73.0","actor":"ci","reconfigure":true,"flags":"--enable-apply"}`), 0o600)
	r.serverUnderTest.ResumeDeploy()
	j := r.waitLatest(t, JobFailed)
	if !strings.Contains(j.Error, "did not start") || r.appliedCount() != 0 || len(r.staged) != 0 {
		t.Fatalf("%+v applied %d staged %q", j, r.appliedCount(), r.staged)
	}
}

func TestMatchingFlagsDeployWithoutARestart(t *testing.T) {
	r := newFlagsRig(t, "--enable-apply  --image-prefix opsavor-", "--enable-apply --image-prefix opsavor-", true)
	r.deploy(t, JobSucceeded)
	if r.appliedCount() != 1 || len(r.staged) != 0 || len(r.restarts) != 0 {
		t.Fatalf("applied %d staged %q restarts %d", r.appliedCount(), r.staged, len(r.restarts))
	}
	// A daemon on its unit's flags with no daemon.flags in fleet.yml is left as it is.
	plain := newFlagsRig(t, "", "", false)
	plain.deploy(t, JobSucceeded)
	if plain.appliedCount() != 1 || len(plain.staged) != 0 {
		t.Fatalf("unmanaged: applied %d staged %q", plain.appliedCount(), plain.staged)
	}
}

func TestRemovingDaemonFlagsGoesBackToTheUnits(t *testing.T) {
	r := newFlagsRig(t, "", "--enable-apply", true)
	j := r.deploy(t, JobSucceeded)
	if j.Result != ResultDaemonReconfigured || len(r.staged) != 1 || r.staged[0] != "" {
		t.Fatalf("%+v staged %q", j, r.staged)
	}
}

func TestFlagsADeployMayNotSet(t *testing.T) {
	for name, c := range map[string]struct{ flags, want string }{
		"unknown flag":    {"--enable-apply --no-such-flag", "not defined"},
		"another address": {"--addr 0.0.0.0:8686 --enable-apply", "cannot do that"},
	} {
		r := newFlagsRig(t, c.flags, "", false)
		j := r.deploy(t, JobFailed)
		if !strings.Contains(j.Error, c.want) || len(r.staged) != 0 || r.appliedCount() != 0 {
			t.Errorf("%s: %+v staged %q applied %d", name, j, r.staged, r.appliedCount())
		}
	}
	// The same address is fine: it changes nothing.
	r := newFlagsRig(t, "--addr 10.0.100.1:8686 --enable-apply", "", false)
	if j := r.deploy(t, JobSucceeded); j.Result != ResultDaemonReconfigured {
		t.Fatalf("the address it already has: %+v", j)
	}
}

func (r *flagsRig) waitLatest(t *testing.T, want JobStatus) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if jobs := r.serverUnderTest.opts.Jobs.List(); len(jobs) > 0 {
			return r.waitJob(t, string(jobs[0].ID), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no job was started")
	return Job{}
}
