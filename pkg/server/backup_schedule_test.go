package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/status"
)

// schedRig is a daemon with backups on and a fake BackupFunc that can be held mid-run.
type schedRig struct {
	srv   *Server
	dir   string // BackupScheduleDir
	audit string
	mu    sync.Mutex
	runs  []BackupRequest
	dirs  []string
	hold  chan struct{} // when set, a backup waits on it
}

func newSchedRig(t *testing.T, enableBackup bool) *schedRig {
	t.Helper()
	base := t.TempDir()
	rig := &schedRig{dir: filepath.Join(base, "backup-schedule"), audit: filepath.Join(base, "audit.log")}
	tokens, _ := OpenTokenStore(filepath.Join(base, "tokens.json"))
	audit, _ := OpenAudit(rig.audit)
	t.Cleanup(func() { audit.Close() })
	opts := Options{Tokens: tokens, Audit: audit, Version: "test", BackupScheduleDir: rig.dir,
		Status: func(context.Context) (*status.Snapshot, error) { return &status.Snapshot{}, nil }}
	if enableBackup {
		opts.Jobs, _ = OpenJobs(filepath.Join(base, "jobs"))
		opts.Backup = func(_ context.Context, dir string, req BackupRequest, logf func(string, ...any)) error {
			rig.mu.Lock()
			rig.runs = append(rig.runs, req)
			rig.dirs = append(rig.dirs, dir)
			hold := rig.hold
			rig.mu.Unlock()
			if hold != nil {
				<-hold
			}
			logf("backed up")
			return nil
		}
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	rig.srv = s
	t.Cleanup(func() { s.WaitForJobs(5 * time.Second) })
	return rig
}

func (r *schedRig) runCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

// follow records the schedule a deploy's tree declares, returning the deploy log lines.
func (r *schedRig) follow(t *testing.T, fleetYML string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fleet.yml"), []byte(fleetYML), 0o644); err != nil {
		t.Fatal(err)
	}
	var log []string
	if err := r.srv.followBackupSchedule(root, func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) }); err != nil {
		t.Fatal(err)
	}
	return strings.Join(log, "\n")
}

func (r *schedRig) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.srv.sched.mu.Lock()
		pending := r.srv.sched.pending
		r.srv.sched.mu.Unlock()
		if !pending {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the scheduled backup never finished")
}

func (r *schedRig) waitRuns(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r.runCount() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("want %d scheduled backups, got %d", n, r.runCount())
}

const scheduledFleet = `name: test
backup:
  endpoint: https://s3.example.com
  region: r
  bucket: backups
  prefix: native-ops/
  schedule: "03:30"
  retain_daily: 7
`

func day(h, m int) time.Time { return time.Date(2026, 11, 4, h, m, 0, 0, time.UTC) }

func TestADeployTagSetsChangesAndRemovesTheBackupSchedule(t *testing.T) {
	rig := newSchedRig(t, true)
	out := rig.follow(t, scheduledFleet)
	if !strings.Contains(out, "backup schedule set") || !strings.Contains(out, "daily at 03:30 UTC") || !strings.Contains(out, "newest 7") {
		t.Fatalf("log: %s", out)
	}
	if times, err := rig.srv.loadBackupSchedule(); err != nil || len(times) != 1 || times[0] != 3*60+30 {
		t.Fatalf("recorded schedule: %v %v", times, err)
	}
	if out := rig.follow(t, scheduledFleet); !strings.Contains(out, "unchanged") {
		t.Fatalf("the same tag again: %s", out)
	}
	if out := rig.follow(t, strings.Replace(scheduledFleet, `"03:30"`, `"01:00,13:00"`, 1)); !strings.Contains(out, "set") {
		t.Fatalf("a changed schedule: %s", out)
	}
	if times, _ := rig.srv.loadBackupSchedule(); len(times) != 2 {
		t.Fatalf("changed schedule: %v", times)
	}
	if out := rig.follow(t, "name: test\n"); !strings.Contains(out, "removed") {
		t.Fatalf("no schedule any more: %s", out)
	}
	if times, _ := rig.srv.loadBackupSchedule(); times != nil {
		t.Fatalf("removed schedule still there: %v", times)
	}
	// The recorded section never holds a credential: only the names of the variables that do.
	rig.follow(t, scheduledFleet)
	b, _ := os.ReadFile(filepath.Join(rig.dir, backupScheduleFile))
	if !strings.Contains(string(b), "access_key_env: BACKUP_S3_ACCESS_KEY") || strings.Contains(string(b), "host") {
		t.Fatalf("recorded section:\n%s", b)
	}
}

func TestADaemonWithoutBackupsSchedulesNothing(t *testing.T) {
	rig := newSchedRig(t, false)
	out := rig.follow(t, scheduledFleet)
	if !strings.Contains(out, "without --enable-backup") {
		t.Fatalf("the deploy log must say the schedule is not followed: %s", out)
	}
	if _, err := os.Stat(filepath.Join(rig.dir, backupScheduleFile)); !os.IsNotExist(err) {
		t.Fatal("nothing may be recorded")
	}
	rig.srv.RunBackupSchedule(context.Background()) // returns at once: nothing to run
}

func TestWithoutAScheduleNoBackupEverRuns(t *testing.T) {
	rig := newSchedRig(t, true)
	rig.follow(t, "name: test\nbackup:\n  endpoint: https://s3.example.com\n  region: r\n  bucket: b\n")
	for m := 0; m <= 24*60; m += 7 {
		if rig.srv.backupScheduleTick(day(0, 0).Add(time.Duration(m) * time.Minute)) {
			t.Fatalf("a tick at +%dm started a backup with no schedule", m)
		}
	}
	if rig.runCount() != 0 || len(rig.srv.opts.Jobs.List()) != 0 {
		t.Fatal("no schedule, no backup job")
	}
}

func TestTheScheduledBackupRunsAtItsTimeAsARecordedJob(t *testing.T) {
	rig := newSchedRig(t, true)
	rig.follow(t, scheduledFleet)

	if rig.srv.backupScheduleTick(day(3, 29)) {
		t.Fatal("the first check only records the time")
	}
	if rig.srv.backupScheduleTick(day(3, 29).Add(30 * time.Second)) {
		t.Fatal("not yet 03:30")
	}
	if !rig.srv.backupScheduleTick(day(3, 30)) {
		t.Fatal("03:30 is due")
	}
	rig.waitRuns(t, 1)
	rig.waitIdle(t)
	if rig.srv.backupScheduleTick(day(3, 31)) {
		t.Fatal("one run per scheduled time")
	}
	rig.mu.Lock()
	req, dir := rig.runs[0], rig.dirs[0]
	rig.mu.Unlock()
	if !req.Prune || req.Volume != "" || dir != rig.dir {
		t.Fatalf("a scheduled run backs up every allowed volume and applies retention, from the recorded section: %+v %s", req, dir)
	}
	jobs := rig.srv.opts.Jobs.List()
	if len(jobs) != 1 || jobs[0].Kind != "backup" || jobs[0].Actor != scheduledActor {
		t.Fatalf("the run is a recorded backup job by the scheduler: %+v", jobs)
	}
	if b, _ := os.ReadFile(rig.audit); !strings.Contains(string(b), "scheduled backup succeeded") {
		t.Fatalf("the run is audited:\n%s", b)
	}
	// The next day, again.
	if !rig.srv.backupScheduleTick(day(3, 30).Add(24 * time.Hour)) {
		t.Fatal("the next day's run is due")
	}
	rig.waitRuns(t, 2)
}

func TestARestartAfterTheScheduledTimeDoesNotRunItLate(t *testing.T) {
	rig := newSchedRig(t, true)
	rig.follow(t, scheduledFleet)
	rig.srv.backupScheduleTick(day(4, 0)) // the daemon starts at 04:00
	if rig.srv.backupScheduleTick(day(4, 1)) || rig.runCount() != 0 {
		t.Fatal("03:30 has passed before the daemon started; it runs tomorrow")
	}
}

func TestALongGapRunsTheBackupOnce(t *testing.T) {
	rig := newSchedRig(t, true)
	rig.follow(t, strings.Replace(scheduledFleet, `"03:30"`, `"03:30,15:30"`, 1))
	rig.srv.backupScheduleTick(day(0, 0))
	if !rig.srv.backupScheduleTick(day(0, 0).Add(72 * time.Hour)) {
		t.Fatal("after a long gap a run is due")
	}
	rig.waitRuns(t, 1)
	rig.waitIdle(t)
	if rig.runCount() != 1 {
		t.Fatalf("missed runs are not replayed one by one: %d", rig.runCount())
	}
}

func TestAScheduledBackupStillRunningMakesTheNextOneSkip(t *testing.T) {
	rig := newSchedRig(t, true)
	rig.follow(t, strings.Replace(scheduledFleet, `"03:30"`, `"03:30,03:45"`, 1))
	rig.hold = make(chan struct{})

	rig.srv.backupScheduleTick(day(3, 0))
	rig.srv.backupScheduleTick(day(3, 30))
	rig.waitRuns(t, 1) // running, held

	if !rig.srv.backupScheduleTick(day(3, 45)) {
		t.Fatal("03:45 comes due")
	}
	time.Sleep(50 * time.Millisecond)
	if rig.runCount() != 1 || len(rig.srv.opts.Jobs.List()) != 1 {
		t.Fatalf("a second backup must never start or queue while the first runs: %d runs, %d jobs", rig.runCount(), len(rig.srv.opts.Jobs.List()))
	}
	b, _ := os.ReadFile(rig.audit)
	if !strings.Contains(string(b), "scheduled backup of 03:45 UTC skipped") {
		t.Fatalf("the skip is audited:\n%s", b)
	}

	close(rig.hold)
	rig.waitIdle(t)
	rig.mu.Lock()
	rig.hold = nil
	rig.mu.Unlock()
	if !rig.srv.backupScheduleTick(day(3, 30).Add(24 * time.Hour)) {
		t.Fatal("the next day's run is due")
	}
	rig.waitRuns(t, 2)
}

func TestAScheduledBackupWaitsForTheHostInsteadOfBeingDropped(t *testing.T) {
	rig := newSchedRig(t, true)
	rig.follow(t, scheduledFleet)
	held, ok := rig.srv.queue.tryTicket() // a deploy is running
	if !ok {
		t.Fatal("host busy")
	}
	_ = held
	rig.srv.backupScheduleTick(day(3, 0))
	rig.srv.backupScheduleTick(day(3, 30))
	time.Sleep(50 * time.Millisecond)
	if rig.runCount() != 0 {
		t.Fatal("the backup must not run over another change")
	}
	if jobs := rig.srv.opts.Jobs.List(); len(jobs) != 1 || jobs[0].Status != JobQueued {
		t.Fatalf("it waits in line: %+v", jobs)
	}
	rig.srv.queue.release()
	rig.waitRuns(t, 1)
}

func TestDueBetween(t *testing.T) {
	times := []int{3*60 + 30, 15*60 + 30}
	for _, tc := range []struct {
		last, now time.Time
		due       bool
		slot      time.Time
	}{
		{day(3, 0), day(3, 29), false, time.Time{}},
		{day(3, 29), day(3, 30), true, day(3, 30)},
		{day(3, 30), day(3, 31), false, time.Time{}}, // 03:30 itself was the last check
		{day(15, 0), day(15, 30).Add(time.Second), true, day(15, 30)},
		{day(23, 59), day(3, 30).Add(24 * time.Hour), true, day(3, 30).Add(24 * time.Hour)}, // across midnight
		{day(0, 0), day(0, 0).Add(100 * time.Hour), true, day(3, 30).Add(96 * time.Hour)},   // a long gap: the latest
	} {
		slot, due := dueBetween(times, tc.last, tc.now)
		if due != tc.due || !slot.Equal(tc.slot) {
			t.Errorf("(%s, %s]: due=%v slot=%s, want %v %s", tc.last, tc.now, due, slot, tc.due, tc.slot)
		}
	}
	if _, due := dueBetween(nil, day(0, 0), day(23, 0)); due {
		t.Fatal("no times, never due")
	}
}

func TestADeployTagRecordsItsBackupScheduleInTheDeployLog(t *testing.T) {
	src := &fakeSource{protected: true, tree: tgz(t, entry{name: "fleet.yml", body: scheduledFleet}, entry{name: "services/web/service.yml", body: "image: x\n"})}
	schedDir := filepath.Join(t.TempDir(), "backup-schedule")
	rig := newApplyRigWith(t, func(o *Options) {
		o.Deploy, o.DeployTags = src, "deploy-*"
		o.BackupScheduleDir = schedDir
		o.Backup = func(context.Context, string, BackupRequest, func(string, ...any)) error { return nil }
	})
	res, body := rig.post(t, "/v1/deploy", rig.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	if res.StatusCode != 202 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	j := rig.waitJob(t, jobID(t, body), JobSucceeded)
	if !strings.Contains(j.Log, "backup schedule set") {
		t.Fatalf("the deploy log must show the schedule it set:\n%s", j.Log)
	}
	if _, err := os.Stat(filepath.Join(schedDir, backupScheduleFile)); err != nil {
		t.Fatalf("schedule not recorded: %v", err)
	}
}
