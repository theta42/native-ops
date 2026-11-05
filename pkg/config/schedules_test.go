package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFleet(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fleet.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBackupScheduleTimes(t *testing.T) {
	b := &BackupConfig{Schedule: " 15:30, 03:05,03:05 "}
	got, err := b.ScheduleTimes()
	if err != nil || len(got) != 2 || got[0] != 3*60+5 || got[1] != 15*60+30 {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := (&BackupConfig{}).ScheduleTimes(); got != nil || err != nil {
		t.Fatal("no schedule is none")
	}
	for _, bad := range []string{"3:30", "24:00", "03:60", "daily", "0 3 * * *", "03:30,"} {
		if _, err := (&BackupConfig{Schedule: bad}).ScheduleTimes(); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestFleetYmlWithSchedulesLoadsAndBadOnesAreRefused(t *testing.T) {
	cfg, err := LoadFleetConfig(writeFleet(t, `name: x
snapshots:
  schedule: "@daily"
  expiry: 2w
backup:
  endpoint: https://s3.example.com
  region: r
  bucket: b
  schedule: "02:00"
  retain_days: 30
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Snapshots == nil || cfg.Snapshots.Schedule != "@daily" || cfg.Backup.Schedule != "02:00" || cfg.Backup.RetainDays != 30 {
		t.Fatalf("%+v %+v", cfg.Snapshots, cfg.Backup)
	}
	for _, bad := range []string{
		"name: x\nbackup:\n  schedule: \"25:00\"\n",
		"name: x\nsnapshots:\n  expiry: forever\n",
		"name: x\nsnapshots:\n  schedule: \"@daily; rm -rf /\"\n",
		"name: x\nbackup:\n  retain_days: -1\n",
	} {
		if _, err := LoadFleetConfig(writeFleet(t, bad)); err == nil {
			t.Errorf("must be refused:\n%s", bad)
		}
	}
}

func TestAHostNameIsNeverReadFromFleetYml(t *testing.T) {
	cfg, err := LoadFleetConfig(writeFleet(t, "name: x\nbackup:\n  host_name: other\n  hostname: other\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backup.HostName != "" {
		t.Fatal("fleet.yml is shared by every host: it must not name this one")
	}
}

func TestSnapshotPolicyValidation(t *testing.T) {
	for _, ok := range []SnapshotPolicy{{}, {Schedule: "0 */6 * * *", Expiry: "1d 12H", Pattern: "auto-{{ creation_date|date:'2006-01-02' }}"}, {Schedule: "@hourly, @daily"}} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []SnapshotPolicy{{Schedule: "@daily\n"}, {Expiry: "7 days"}, {Pattern: "a/b"}, {Pattern: "x\x01"}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v must be refused", bad)
		}
	}
}

func TestAVolumesSnapshotPolicyIsReadAndValidated(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yml")
	os.WriteFile(good, []byte("name: s\nvolumes:\n  - name: s-data\n    path: /data\n    snapshots:\n      schedule: \"@daily\"\n"), 0o644)
	svc, err := LoadServiceConfig(good)
	if err != nil {
		t.Fatal(err)
	}
	if p := SnapshotPolicyFor(svc.Volumes[0], &SnapshotPolicy{Schedule: "@weekly"}); p == nil || p.Schedule != "@daily" {
		t.Fatalf("a volume's own policy wins: %+v", p)
	}
	if p := SnapshotPolicyFor(VolumeMount{}, &SnapshotPolicy{Schedule: "@weekly"}); p == nil || p.Schedule != "@weekly" {
		t.Fatal("else the fleet default")
	}
	if SnapshotPolicyFor(VolumeMount{}, nil) != nil {
		t.Fatal("else none")
	}
	bad := filepath.Join(dir, "bad.yml")
	os.WriteFile(bad, []byte("name: s\nvolumes:\n  - name: s-data\n    path: /data\n    snapshots:\n      expiry: soon\n"), 0o644)
	if _, err := LoadServiceConfig(bad); err == nil || !strings.Contains(err.Error(), "s-data") {
		t.Fatalf("a bad volume policy is refused, naming the volume: %v", err)
	}
}
