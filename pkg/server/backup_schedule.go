package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/theta42/native-ops/pkg/config"
)

// Scheduled off-host backups: the daemon backs up every allowed volume by itself, then applies retention,
// at the daily UTC times fleet.yml's backup.schedule names, so the host needs no backup timer. The
// schedule is configuration like any other: a deploy tag's commit sets it (followBackupSchedule keeps
// that commit's `backup:` section in the state directory), and the next deploy tag changes or removes
// it. Each run is a recorded job ("backup", started by "native-ops (scheduled)") that waits its turn for
// the host like any change; a run that comes due while the previous scheduled one is still queued or
// running is skipped, logged and audited, so two never pile up. Where the backups go is still only the
// destination pinned on the daemon (daemonBackupStore in cmd/native-ops).

// backupSched is the scheduler's state.
type backupSched struct {
	mu       sync.Mutex
	last     time.Time // when the schedule was last checked; zero before the first check
	pending  bool      // a scheduled backup is queued or running
	inFlight JobID     // its job, once recorded
}

const backupScheduleFile = "fleet.yml"

// followBackupSchedule makes the daemon's backup schedule the one fleet.yml in root declares (a deploy
// calls it with its commit's tree). Without backup.schedule, any schedule is removed.
func (s *Server) followBackupSchedule(root string, logf func(string, ...any)) error {
	fleet, err := config.LoadFleetConfig(root)
	if err != nil {
		return err
	}
	var want *config.BackupConfig
	if fleet.Backup != nil && strings.TrimSpace(fleet.Backup.Schedule) != "" {
		want = fleet.Backup
	}
	if s.opts.Backup == nil || s.opts.Jobs == nil || s.opts.BackupScheduleDir == "" {
		if want != nil {
			logf("fleet.yml schedules backups (%s UTC), but this daemon runs without --enable-backup, so nothing is scheduled", want.Schedule)
		}
		return nil
	}
	file := filepath.Join(s.opts.BackupScheduleDir, backupScheduleFile)
	old, readErr := os.ReadFile(file)
	if want == nil {
		if readErr == nil {
			if err := os.Remove(file); err != nil {
				return fmt.Errorf("remove the backup schedule: %w", err)
			}
			logf("backup schedule removed: fleet.yml no longer has backup.schedule")
		}
		return nil
	}
	body, err := yaml.Marshal(struct {
		Backup *config.BackupConfig `yaml:"backup"`
	}{want})
	if err != nil {
		return err
	}
	if readErr == nil && bytes.Equal(old, body) {
		logf("backup schedule unchanged: %s", describeSchedule(want))
		return nil
	}
	if err := os.MkdirAll(s.opts.BackupScheduleDir, 0o700); err != nil {
		return fmt.Errorf("record the backup schedule: %w", err)
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("record the backup schedule: %w", err)
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("record the backup schedule: %w", err)
	}
	logf("backup schedule set: %s", describeSchedule(want))
	return nil
}

// describeSchedule says what a scheduled backup does, for a deploy's log.
func describeSchedule(b *config.BackupConfig) string {
	what := "every volume"
	if len(b.Volumes) > 0 {
		what = strings.Join(b.Volumes, ", ")
	}
	var keep []string
	if b.RetainDaily > 0 {
		keep = append(keep, fmt.Sprintf("the newest %d", b.RetainDaily))
	}
	if b.RetainMonthly > 0 {
		keep = append(keep, fmt.Sprintf("one a month for %d months", b.RetainMonthly))
	}
	if b.RetainDays > 0 {
		keep = append(keep, fmt.Sprintf("everything from the last %d days", b.RetainDays))
	}
	retention := "no retention (every backup is kept)"
	if len(keep) > 0 {
		retention = "retention keeps " + strings.Join(keep, ", ") + " of this host's backups"
	}
	return fmt.Sprintf("back up %s daily at %s UTC; %s", what, strings.ReplaceAll(strings.TrimSpace(b.Schedule), " ", ""), retention)
}

// loadBackupSchedule reads the schedule a deploy recorded: nil when there is none.
func (s *Server) loadBackupSchedule() ([]int, error) {
	if s.opts.BackupScheduleDir == "" {
		return nil, nil
	}
	if _, err := os.Stat(filepath.Join(s.opts.BackupScheduleDir, backupScheduleFile)); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	fleet, err := config.LoadFleetConfig(s.opts.BackupScheduleDir)
	if err != nil {
		return nil, err
	}
	if fleet.Backup == nil {
		return nil, nil
	}
	return fleet.Backup.ScheduleTimes()
}

// RunBackupSchedule checks the backup schedule every minute until ctx ends. It does nothing on a daemon
// without backups (--enable-backup); with no schedule recorded, each check is a no-op.
func (s *Server) RunBackupSchedule(ctx context.Context) {
	if s.opts.Backup == nil || s.opts.Jobs == nil || s.opts.BackupScheduleDir == "" {
		return
	}
	s.backupScheduleTick(time.Now().UTC())
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.backupScheduleTick(now.UTC())
		}
	}
}

// backupScheduleTick starts the scheduled backup when one of the schedule's times fell after the last
// check and at or before now. The first check only records the time: a daemon that starts after a
// scheduled time does not run that backup late. It reports whether it started (or skipped) a run, for
// tests, which drive it with their own clock.
func (s *Server) backupScheduleTick(now time.Time) bool {
	s.sched.mu.Lock()
	last := s.sched.last
	if now.After(last) {
		s.sched.last = now
	}
	s.sched.mu.Unlock()
	if last.IsZero() || !now.After(last) {
		return false
	}
	times, err := s.loadBackupSchedule()
	if err != nil {
		log.Printf("backup schedule: %v", err)
		return false
	}
	slot, due := dueBetween(times, last, now)
	if !due {
		return false
	}
	s.startScheduledBackup(slot)
	return true
}

// dueBetween reports whether one of the daily times (minutes after midnight UTC) falls in (last, now],
// and the latest such time. A gap of a day or more always has one.
func dueBetween(times []int, last, now time.Time) (time.Time, bool) {
	if len(times) == 0 {
		return time.Time{}, false
	}
	last, now = last.UTC(), now.UTC()
	if now.Sub(last) >= 24*time.Hour {
		last = now.Add(-24 * time.Hour) // every daily time falls in any 24 hours; one run is enough
	}
	var found time.Time
	for day := time.Date(last.Year(), last.Month(), last.Day(), 0, 0, 0, 0, time.UTC); !day.After(now); day = day.AddDate(0, 0, 1) {
		for _, m := range times {
			slot := day.Add(time.Duration(m) * time.Minute)
			if slot.After(last) && !slot.After(now) {
				found = slot
			}
		}
	}
	return found, !found.IsZero()
}

// startScheduledBackup queues the scheduled backup for slot, unless the previous one is still queued or
// running: then this one is skipped, in the process log and the audit log.
func (s *Server) startScheduledBackup(slot time.Time) {
	at := slot.UTC().Format("15:04")
	s.sched.mu.Lock()
	if s.sched.pending {
		id := s.sched.inFlight
		s.sched.mu.Unlock()
		log.Printf("scheduled backup of %s UTC skipped: the previous scheduled backup (%s) is still queued or running", at, orDash(string(id)))
		s.opts.Audit.Log(AuditEntry{Time: time.Now().UTC(), Actor: scheduledActor, Method: "SCHEDULE", Path: "/v1/backups", Status: http.StatusConflict,
			Detail: fmt.Sprintf("scheduled backup of %s UTC skipped: %s still queued or running", at, orDash(string(id)))})
		return
	}
	s.sched.pending = true
	s.sched.mu.Unlock()

	done := func() {
		s.sched.mu.Lock()
		s.sched.pending, s.sched.inFlight = false, ""
		s.sched.mu.Unlock()
	}
	dir := s.opts.BackupScheduleDir
	job, err := s.queueJob(jobSpec{kind: "backup", actor: scheduledActor, what: "scheduled backup (" + at + " UTC) of every allowed volume", audit: "scheduled backup",
		cleanup: done,
		work: func(ctx context.Context, logf func(string, ...any)) error {
			return s.opts.Backup(ctx, dir, BackupRequest{Prune: true}, logf)
		}})
	if err != nil {
		done()
		log.Printf("scheduled backup of %s UTC not started: %v", at, err)
		s.opts.Audit.Log(AuditEntry{Time: time.Now().UTC(), Actor: scheduledActor, Method: "SCHEDULE", Path: "/v1/backups", Status: http.StatusServiceUnavailable,
			Detail: fmt.Sprintf("scheduled backup of %s UTC not started: %v", at, err)})
		return
	}
	s.sched.mu.Lock()
	if s.sched.pending { // not finished already
		s.sched.inFlight = job.ID
	}
	s.sched.mu.Unlock()
}
