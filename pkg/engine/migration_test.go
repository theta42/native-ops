package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/remote"
)

var quotedRe = regexp.MustCompile(`'([^']*)'`)

func quotedArgs(cmd string) []string {
	var out []string
	for _, m := range quotedRe.FindAllStringSubmatch(cmd, -1) {
		out = append(out, m[1])
	}
	return out
}

func cut(s, sep string) (string, string) {
	a, b, _ := strings.Cut(s, sep)
	return a, b
}

// migSim is a small stateful stand-in for two Incus remotes. It answers the
// commands the migration issues and mutates its state accordingly, so tests
// can assert on where the instance ends up and not just on command order.
type migSim struct {
	status  map[string]map[string]string // remote -> instance -> status
	volumes map[string]map[string]bool   // remote -> custom volume (pool "default")
	cmds    []string
	snaps   []string

	configYAML      string                 // overrides the source `config show` output
	hook            func(cmd string) error // called first; a non-nil error fails the command
	stopErrButStops bool                   // `incus stop` reports an error but the instance did stop
}

var _ remote.Executor = (*migSim)(nil)

func newSim() *migSim {
	return &migSim{
		status:  map[string]map[string]string{"src": {"rest-x": "Running", "other": "Running"}, "dst": {}},
		volumes: map[string]map[string]bool{"src": {"rest-x-data": true}, "dst": {}},
	}
}

func (s *migSim) Run(_ context.Context, cmd string) (string, error) {
	s.cmds = append(s.cmds, cmd)
	if s.hook != nil {
		if err := s.hook(cmd); err != nil {
			return "", err
		}
	}
	q := quotedArgs(cmd)
	switch {
	case strings.HasPrefix(cmd, "incus list "):
		r := strings.TrimSuffix(q[0], ":")
		type row struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		var rows []row
		for n, st := range s.status[r] {
			rows = append(rows, row{n, st})
		}
		b, _ := json.Marshal(rows)
		return string(b), nil
	case strings.HasPrefix(cmd, "incus storage volume list "):
		r, pool := cut(q[0], ":")
		if pool != "default" {
			return "", errors.New("Error: Storage pool not found")
		}
		type row struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		rows := []row{{"rest-x-data", "image"}} // must be ignored: not a custom volume
		for v := range s.volumes[r] {
			rows = append(rows, row{v, "custom"})
		}
		b, _ := json.Marshal(rows)
		return string(b), nil
	case strings.HasPrefix(cmd, "incus config show "):
		r, n := cut(q[0], ":")
		if _, ok := s.status[r][n]; !ok {
			return "", errors.New("Error: Instance not found")
		}
		if s.configYAML != "" {
			return s.configYAML, nil
		}
		return configYAML(oldFP), nil
	case strings.HasPrefix(cmd, "incus storage volume snapshot create "):
		s.snaps = append(s.snaps, q[0]+"/"+q[1]+"@"+q[2])
	case strings.HasPrefix(cmd, "incus stop "):
		r, n := cut(q[0], ":")
		s.status[r][n] = "Stopped"
		if s.stopErrButStops {
			return "", errors.New("timed out waiting for shutdown")
		}
	case strings.HasPrefix(cmd, "incus start "):
		r, n := cut(q[0], ":")
		s.status[r][n] = "Running"
	case strings.HasPrefix(cmd, "incus storage volume copy "):
		dr, rest := cut(q[1], ":")
		_, vol := cut(rest, "/")
		s.volumes[dr][vol] = true
	case strings.HasPrefix(cmd, "incus copy "):
		dr, n := cut(q[1], ":")
		if _, ok := s.status[dr][n]; !ok {
			s.status[dr][n] = "Stopped" // a copy is created stopped
		}
	case strings.HasPrefix(cmd, "incus delete "):
		r, n := cut(q[0], ":")
		delete(s.status[r], n)
	case strings.HasPrefix(cmd, "incus storage volume delete "):
		r, _ := cut(q[0], ":")
		delete(s.volumes[r], q[1])
	}
	return "", nil
}
func (s *migSim) RunWithInput(context.Context, string, io.Reader) (string, error) { return "", nil }
func (s *migSim) WriteFile(context.Context, string, []byte, os.FileMode) error    { return nil }
func (s *migSim) Close() error                                                    { return nil }

func (s *migSim) idx(prefix string) []int {
	var out []int
	for i, c := range s.cmds {
		if strings.HasPrefix(c, prefix) {
			out = append(out, i)
		}
	}
	return out
}
func (s *migSim) count(prefix string) int { return len(s.idx(prefix)) }

// mutating is the number of commands that change anything on either remote.
func (s *migSim) mutating() int {
	n := 0
	for _, p := range []string{"incus storage volume snapshot", "incus copy", "incus storage volume copy", "incus stop", "incus start", "incus delete", "incus storage volume delete"} {
		n += s.count(p)
	}
	return n
}

type probeCall struct{ remote, name string }

func newMigMgr(sim *migSim, probeErr error) (*MigrationManager, *[]probeCall) {
	mm := NewMigrationManager(sim)
	var calls []probeCall
	mm.healthProbe = func(_ context.Context, r, n string, _ config.HealthCheckConfig) error {
		calls = append(calls, probeCall{r, n})
		return probeErr
	}
	return mm, &calls
}

var testHC = config.HealthCheckConfig{Path: "/health", Port: 8787, Timeout: 5}

func baseParams() MigrationParams {
	return MigrationParams{SourceRemote: "src", TargetRemote: "dst", InstanceName: "rest-x", HealthCheck: testHC}
}

func TestMigrateHappyPathWarmCopyThenStopThenFinalSync(t *testing.T) {
	sim := newSim()
	mm, probes := newMigMgr(sim, nil)
	if err := mm.Migrate(context.Background(), baseParams()); err != nil {
		t.Fatal(err)
	}
	if sim.status["src"]["rest-x"] != "Stopped" || sim.status["dst"]["rest-x"] != "Running" {
		t.Fatalf("want source Stopped and target Running, got %v", sim.status)
	}
	if sim.status["src"]["other"] != "Running" {
		t.Errorf("an unrelated instance must not be touched")
	}
	if sim.count("incus delete") != 0 || sim.count("incus storage volume delete") != 0 {
		t.Fatalf("Migrate must never delete the source: %v", sim.cmds)
	}
	if len(sim.snaps) != 1 || !strings.HasPrefix(sim.snaps[0], "src:default/rest-x-data@pre-migrate-") {
		t.Fatalf("source volume must be snapshotted first, got %v", sim.snaps)
	}
	snap, vc, ic := sim.idx("incus storage volume snapshot create")[0], sim.idx("incus storage volume copy"), sim.idx("incus copy")
	stop, start := sim.idx("incus stop 'src:rest-x'")[0], sim.idx("incus start 'dst:rest-x'")[0]
	if len(vc) != 2 || len(ic) != 2 {
		t.Fatalf("want a warm and a final copy of the volume and the instance, got %v %v\n%v", vc, ic, sim.cmds)
	}
	if !(snap < vc[0] && vc[0] < ic[0] && ic[0] < stop && stop < vc[1] && vc[1] < ic[1] && ic[1] < start) {
		t.Fatalf("wrong order snap=%d warm=%v/%v stop=%d final=%v/%v start=%d\n%v", snap, vc[0], ic[0], stop, vc[1], ic[1], start, sim.cmds)
	}
	for i, c := range append(append([]string{}, pick(sim.cmds, vc)...), pick(sim.cmds, ic)...) {
		final := strings.Contains(c, " --refresh")
		warm := i == 0 || i == 2
		if warm && final {
			t.Errorf("the first copy must be a plain copy: %s", c)
		}
		if !warm && !final {
			t.Errorf("the final copy must be an incremental refresh: %s", c)
		}
		if strings.HasPrefix(c, "incus storage volume copy") && !strings.Contains(c, "--volume-only") {
			t.Errorf("volume copies must leave snapshots behind: %s", c)
		}
		if strings.HasPrefix(c, "incus copy") && (!strings.Contains(c, "--instance-only") || !strings.Contains(c, "--mode=push")) {
			t.Errorf("instance copies must be push-mode and skip snapshots: %s", c)
		}
	}
	if len(*probes) != 1 || (*probes)[0] != (probeCall{"dst", "rest-x"}) {
		t.Errorf("target must be health-checked once, got %v", *probes)
	}
}

func pick(cmds []string, idx []int) []string {
	var out []string
	for _, i := range idx {
		out = append(out, cmds[i])
	}
	return out
}

func TestMigrateAbortsUntouchedWhenSnapshotFails(t *testing.T) {
	sim := newSim()
	sim.hook = func(c string) error {
		if strings.HasPrefix(c, "incus storage volume snapshot create") {
			return errors.New("no space left")
		}
		return nil
	}
	mm, _ := newMigMgr(sim, nil)
	err := mm.Migrate(context.Background(), baseParams())
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("expected a snapshot abort, got %v", err)
	}
	if sim.count("incus copy") != 0 || sim.count("incus storage volume copy") != 0 || sim.count("incus stop") != 0 {
		t.Fatalf("no data may move after a failed snapshot: %v", sim.cmds)
	}
	if sim.status["src"]["rest-x"] != "Running" {
		t.Errorf("source must keep running")
	}
}

func TestMigrateSkipSnapshotIsExplicit(t *testing.T) {
	sim := newSim()
	mm, _ := newMigMgr(sim, nil)
	p := baseParams()
	p.SkipSnapshot = true
	if err := mm.Migrate(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if sim.count("incus storage volume snapshot") != 0 {
		t.Fatalf("SkipSnapshot must skip the snapshot: %v", sim.cmds)
	}
}

func TestMigrateWarmCopyFailureLeavesSourceAlone(t *testing.T) {
	sim := newSim()
	sim.hook = func(c string) error {
		if strings.HasPrefix(c, "incus storage volume copy") {
			return errors.New("connection reset")
		}
		return nil
	}
	mm, _ := newMigMgr(sim, nil)
	err := mm.Migrate(context.Background(), baseParams())
	if err == nil || !strings.Contains(err.Error(), "source was not touched") {
		t.Fatalf("got %v", err)
	}
	if sim.count("incus stop") != 0 || sim.status["src"]["rest-x"] != "Running" {
		t.Fatalf("source must not be stopped by a failed warm copy: %v", sim.cmds)
	}
}

func TestMigrateSourceThatWillNotStopIsLeftServing(t *testing.T) {
	sim := newSim()
	sim.hook = func(c string) error {
		if strings.HasPrefix(c, "incus stop 'src:") {
			return errors.New("timed out")
		}
		return nil
	}
	mm, _ := newMigMgr(sim, nil)
	err := mm.Migrate(context.Background(), baseParams())
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("got %v", err)
	}
	if sim.count("incus copy") != 1 {
		t.Fatalf("only the warm copy may have run; a running source must never be final-copied: %v", sim.cmds)
	}
	if sim.count("incus start") != 0 || sim.status["src"]["rest-x"] != "Running" || sim.status["dst"]["rest-x"] == "Running" {
		t.Fatalf("nothing may be started and the source must still be running: %v %v", sim.status, sim.cmds)
	}
}

func TestMigrateAcceptsStopErrorWhenInstanceIsVerifiablyStopped(t *testing.T) {
	sim := newSim()
	sim.stopErrButStops = true
	mm, _ := newMigMgr(sim, nil)
	if err := mm.Migrate(context.Background(), baseParams()); err != nil {
		t.Fatalf("a stop that reports an error but leaves the instance stopped is fine: %v", err)
	}
}

func TestMigrateRollsBackToSourceOnLaterFailures(t *testing.T) {
	cases := []struct {
		name    string
		fail    func(sim *migSim) func(string) error
		probe   error
		wantErr string
	}{
		{"final sync fails", func(sim *migSim) func(string) error {
			n := 0
			return func(c string) error {
				if strings.HasPrefix(c, "incus copy ") {
					if n++; n == 2 {
						return errors.New("link dropped")
					}
				}
				return nil
			}
		}, nil, "final sync failed"},
		{"target will not start", func(*migSim) func(string) error {
			return func(c string) error {
				if strings.HasPrefix(c, "incus start 'dst:") {
					return errors.New("subnet mismatch")
				}
				return nil
			}
		}, nil, "start on target failed"},
		{"target is unhealthy", nil, errors.New("503"), "health check on target failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sim := newSim()
			if tc.fail != nil {
				sim.hook = tc.fail(sim)
			}
			mm, _ := newMigMgr(sim, tc.probe)
			err := mm.Migrate(context.Background(), baseParams())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "restarted and nothing was lost") {
				t.Fatalf("got %v", err)
			}
			if sim.status["src"]["rest-x"] != "Running" {
				t.Fatalf("the source must be running again: %v", sim.status)
			}
			if sim.status["dst"]["rest-x"] == "Running" {
				t.Fatalf("the failed target copy must not be left running: %v", sim.status)
			}
			if sim.count("incus delete") != 0 {
				t.Fatalf("a rollback must never delete anything: %v", sim.cmds)
			}
		})
	}
}

func TestMigrateReportsFailedRollbackLoudly(t *testing.T) {
	sim := newSim()
	sim.hook = func(c string) error {
		if strings.HasPrefix(c, "incus start ") {
			return errors.New("boom")
		}
		return nil
	}
	mm, _ := newMigMgr(sim, nil)
	err := mm.Migrate(context.Background(), baseParams())
	if err == nil || !strings.Contains(err.Error(), "rollback ALSO failed") || !strings.Contains(err.Error(), "data is intact") {
		t.Fatalf("got %v", err)
	}
}

func TestMigrateNeverOverwritesAnExistingTargetWithoutResume(t *testing.T) {
	t.Run("instance", func(t *testing.T) {
		sim := newSim()
		sim.status["dst"]["rest-x"] = "Stopped"
		mm, _ := newMigMgr(sim, nil)
		err := mm.Migrate(context.Background(), baseParams())
		if err == nil || !strings.Contains(err.Error(), "--resume") {
			t.Fatalf("got %v", err)
		}
		if sim.mutating() != 0 {
			t.Fatalf("a refused migration must change nothing: %v", sim.cmds)
		}
	})
	t.Run("volume", func(t *testing.T) {
		sim := newSim()
		sim.volumes["dst"]["rest-x-data"] = true
		mm, _ := newMigMgr(sim, nil)
		err := mm.Migrate(context.Background(), baseParams())
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("got %v", err)
		}
		if sim.mutating() != 0 {
			t.Fatalf("a refused migration must change nothing: %v", sim.cmds)
		}
	})
	t.Run("running target is refused even with resume", func(t *testing.T) {
		sim := newSim()
		sim.status["dst"]["rest-x"] = "Running"
		mm, _ := newMigMgr(sim, nil)
		p := baseParams()
		p.Resume = true
		err := mm.Migrate(context.Background(), p)
		if err == nil || !strings.Contains(err.Error(), "refusing to overwrite a running instance") || sim.mutating() != 0 {
			t.Fatalf("got %v / %v", err, sim.cmds)
		}
	})
}

func TestMigrateResumeRefreshesExistingCopy(t *testing.T) {
	sim := newSim()
	sim.status["dst"]["rest-x"] = "Stopped"
	sim.volumes["dst"]["rest-x-data"] = true
	mm, _ := newMigMgr(sim, nil)
	p := baseParams()
	p.Resume = true
	if err := mm.Migrate(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	for _, c := range append(pick(sim.cmds, sim.idx("incus copy")), pick(sim.cmds, sim.idx("incus storage volume copy"))...) {
		if !strings.Contains(c, " --refresh") {
			t.Errorf("copies onto an existing target must be incremental: %s", c)
		}
	}
}

func TestMigrateFromAnAlreadyStoppedSourceCopiesOnceAndLeavesItStoppedOnFailure(t *testing.T) {
	sim := newSim()
	sim.status["src"]["rest-x"] = "Stopped"
	sim.hook = func(c string) error {
		if strings.HasPrefix(c, "incus start 'dst:") {
			return errors.New("no route")
		}
		return nil
	}
	mm, _ := newMigMgr(sim, nil)
	err := mm.Migrate(context.Background(), baseParams())
	if err == nil || !strings.Contains(err.Error(), "left stopped") {
		t.Fatalf("got %v", err)
	}
	if sim.count("incus stop") != 0 || sim.count("incus copy") != 1 {
		t.Fatalf("a stopped source needs no stop and no warm pass: %v", sim.cmds)
	}
	if sim.count("incus start 'src:") != 0 || sim.status["src"]["rest-x"] != "Stopped" {
		t.Fatalf("a source that was already stopped must not be started by a rollback: %v", sim.cmds)
	}
}

func TestMigrateRollbackByReversingWithResume(t *testing.T) {
	sim := newSim()
	mm, _ := newMigMgr(sim, nil)
	if err := mm.Migrate(context.Background(), baseParams()); err != nil {
		t.Fatal(err)
	}
	sim.cmds = nil
	back := MigrationParams{SourceRemote: "dst", TargetRemote: "src", InstanceName: "rest-x", HealthCheck: testHC, Resume: true}
	if err := mm.Migrate(context.Background(), back); err != nil {
		t.Fatal(err)
	}
	if sim.status["src"]["rest-x"] != "Running" || sim.status["dst"]["rest-x"] != "Stopped" {
		t.Fatalf("rolling back must put the service back on the original host: %v", sim.status)
	}
	for _, c := range pick(sim.cmds, sim.idx("incus copy")) {
		if !strings.Contains(c, "--refresh") {
			t.Errorf("the reverse copy must bring the target's changes back incrementally: %s", c)
		}
	}
	if sim.count("incus delete") != 0 {
		t.Fatalf("no deletes anywhere: %v", sim.cmds)
	}
}

func TestMigrateRejectsUnsafeOrInconsistentInput(t *testing.T) {
	mm, _ := newMigMgr(newSim(), nil)
	for name, mut := range map[string]func(*MigrationParams){
		"unsafe instance": func(p *MigrationParams) { p.InstanceName = "rest-x; rm -rf /" },
		"unsafe remote":   func(p *MigrationParams) { p.TargetRemote = "dst'; id #" },
		"same remote":     func(p *MigrationParams) { p.TargetRemote = "src" },
		"quote in path":   func(p *MigrationParams) { p.HealthCheck.Path = "/h'ealth" },
		"bad port":        func(p *MigrationParams) { p.HealthCheck.Port = 70000 },
		"unsafe volume":   func(p *MigrationParams) { p.VolumeName = "v;x" },
	} {
		t.Run(name, func(t *testing.T) {
			sim := newSim()
			mm, _ = newMigMgr(sim, nil)
			p := baseParams()
			mut(&p)
			if err := mm.Migrate(context.Background(), p); err == nil {
				t.Fatal("expected an error")
			}
			if len(sim.cmds) != 0 {
				t.Fatalf("invalid input must be rejected before any command runs: %v", sim.cmds)
			}
		})
	}
}

func TestMigrateVolumeFlagMustMatchAttachedVolume(t *testing.T) {
	sim := newSim()
	mm, _ := newMigMgr(sim, nil)
	p := baseParams()
	p.VolumeName = "rest-y-data"
	err := mm.Migrate(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "not attached") || sim.mutating() != 0 {
		t.Fatalf("got %v / %v", err, sim.cmds)
	}
	p.VolumeName = "rest-x-data"
	if err := mm.Migrate(context.Background(), p); err != nil {
		t.Fatalf("the attached volume name must be accepted: %v", err)
	}
}

func TestMigrateRefusesHostPathMounts(t *testing.T) {
	sim := newSim()
	sim.configYAML = "config:\n  volatile.base_image: " + oldFP + "\ndevices:\n  logs:\n    path: /var/log/app\n    source: /srv/logs\n    type: disk\nprofiles:\n- default\n"
	mm, _ := newMigMgr(sim, nil)
	err := mm.Migrate(context.Background(), baseParams())
	if err == nil || !strings.Contains(err.Error(), "host path") || sim.mutating() != 0 {
		t.Fatalf("got %v / %v", err, sim.cmds)
	}
}

func TestMigrateFailsBeforeAnyChangeWhenTargetPoolIsMissing(t *testing.T) {
	sim := newSim()
	sim.configYAML = strings.ReplaceAll(configYAML(oldFP), "pool: default", "pool: fast")
	mm, _ := newMigMgr(sim, nil)
	err := mm.Migrate(context.Background(), baseParams())
	if err == nil || !strings.Contains(err.Error(), "cannot receive volume") || sim.mutating() != 0 {
		t.Fatalf("got %v / %v", err, sim.cmds)
	}
}

// ---- Finalize ----

func migrated() (*migSim, *MigrationManager, *[]probeCall) {
	sim := newSim()
	sim.status["src"]["rest-x"] = "Stopped"
	sim.status["dst"]["rest-x"] = "Running"
	sim.volumes["dst"]["rest-x-data"] = true
	mm, probes := newMigMgr(sim, nil)
	return sim, mm, probes
}

func finalizeParams() FinalizeParams {
	return FinalizeParams{SourceRemote: "src", TargetRemote: "dst", InstanceName: "rest-x", HealthCheck: testHC}
}

func TestFinalizeDeletesSourceInstanceAndKeepsItsVolumeByDefault(t *testing.T) {
	sim, mm, probes := migrated()
	if err := mm.Finalize(context.Background(), finalizeParams()); err != nil {
		t.Fatal(err)
	}
	if _, still := sim.status["src"]["rest-x"]; still {
		t.Fatalf("source instance should be gone: %v", sim.status)
	}
	if !sim.volumes["src"]["rest-x-data"] || sim.count("incus storage volume delete") != 0 {
		t.Fatalf("the source volume is the rollback copy and must be kept by default: %v", sim.cmds)
	}
	if len(*probes) != 1 {
		t.Errorf("the target must be health-checked before deleting anything")
	}
	if sim.status["dst"]["rest-x"] != "Running" || sim.status["src"]["other"] != "Running" {
		t.Errorf("nothing else may change: %v", sim.status)
	}
}

func TestFinalizePurgeDeletesVolumesAfterTheInstance(t *testing.T) {
	sim, mm, _ := migrated()
	p := finalizeParams()
	p.PurgeSourceVolumes = true
	if err := mm.Finalize(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	di, dv := sim.idx("incus delete 'src:rest-x'"), sim.idx("incus storage volume delete 'src:default' 'rest-x-data'")
	if len(di) != 1 || len(dv) != 1 || di[0] > dv[0] {
		t.Fatalf("delete the instance first, then its volume: %v", sim.cmds)
	}
	if sim.volumes["dst"]["rest-x-data"] != true {
		t.Fatalf("the target's volume must never be touched")
	}
}

func TestFinalizeRefusesWhenAnythingIsInDoubt(t *testing.T) {
	cases := map[string]func(*migSim) error{
		"source still running": func(s *migSim) error { s.status["src"]["rest-x"] = "Running"; return nil },
		"target not running":   func(s *migSim) error { s.status["dst"]["rest-x"] = "Stopped"; return nil },
		"target missing":       func(s *migSim) error { delete(s.status["dst"], "rest-x"); return nil },
		"volume not on target": func(s *migSim) error { delete(s.volumes["dst"], "rest-x-data"); return nil },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			sim, mm, _ := migrated()
			_ = mut(sim)
			if err := mm.Finalize(context.Background(), finalizeParams()); err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("expected a refusal, got %v", err)
			}
			if sim.count("incus delete") != 0 || sim.count("incus storage volume delete") != 0 {
				t.Fatalf("a refused finalize must delete nothing: %v", sim.cmds)
			}
		})
	}
	t.Run("target unhealthy", func(t *testing.T) {
		sim, mm, _ := migrated()
		mm.healthProbe = func(context.Context, string, string, config.HealthCheckConfig) error { return fmt.Errorf("503") }
		if err := mm.Finalize(context.Background(), finalizeParams()); err == nil || !strings.Contains(err.Error(), "not healthy") {
			t.Fatalf("got %v", err)
		}
		if sim.count("incus delete") != 0 {
			t.Fatalf("nothing may be deleted: %v", sim.cmds)
		}
	})
}
