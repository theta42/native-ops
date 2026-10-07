package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/status"
)

type maintRig struct {
	*fixture
	srv2     *Server
	deployer string
	scoped   string
	mu       sync.Mutex
	backups  []BackupRequest
	restores []RestoreRequest
}

func newMaintRig(t *testing.T) *maintRig {
	t.Helper()
	dir := t.TempDir()
	tokens, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	admin, _, _ := tokens.Create("ci-admin", RoleAdmin)
	viewer, _, _ := tokens.Create("dash", RoleViewer)
	planner, _, _ := tokens.Create("pr-ci", RolePlanner)
	rig := &maintRig{}
	rig.deployer, _, _ = tokens.Create("ci-nightly", RoleDeployer)
	rig.scoped, _, _ = tokens.CreateScoped("fleet-manager", RoleDeployer, Scope{Names: []string{"demo-*"}, Images: []string{"app:*"}})
	audit, _ := OpenAudit(filepath.Join(dir, "audit.log"))
	t.Cleanup(func() { audit.Close() })
	jobs, _ := OpenJobs(filepath.Join(dir, "jobs"))
	s, err := New(Options{Tokens: tokens, Audit: audit, Version: "test", Jobs: jobs,
		Backup: func(_ context.Context, _ string, req BackupRequest, logf func(string, ...any)) error {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.backups = append(rig.backups, req)
			return nil
		},
		Restore: func(_ context.Context, _ string, req RestoreRequest, logf func(string, ...any)) error {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.restores = append(rig.restores, req)
			return nil
		},
		Status: func(context.Context) (*status.Snapshot, error) { return &status.Snapshot{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	rig.srv2 = s
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	rig.fixture = &fixture{srv: ts, secret: admin, viewer: viewer, planner: planner, audit: filepath.Join(dir, "audit.log")}
	t.Cleanup(func() { s.WaitForJobs(5 * time.Second) })
	return rig
}

func (r *maintRig) wait(t *testing.T, body string) Job {
	t.Helper()
	id := JobID(jobID(t, body))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := r.srv2.opts.Jobs.Get(id); ok && j.Status != JobRunning {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never finished", id)
	return Job{}
}

func TestBackupEndpointRunsAJobWithTheRequestedVolume(t *testing.T) {
	rig := newMaintRig(t)
	res, body := rig.post(t, "/v1/backups?volume=gitea-data&prune=1", rig.deployer, "application/gzip", goodTree(t))
	if res.StatusCode != 202 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if j := rig.wait(t, body); j.Kind != "backup" || j.Status != JobSucceeded || j.Service != "gitea-data" {
		t.Fatalf("job: %+v", j)
	}
	res, body = rig.post(t, "/v1/backups", rig.deployer, "application/gzip", goodTree(t))
	if res.StatusCode != 202 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	rig.wait(t, body)
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if len(rig.backups) != 2 || rig.backups[0] != (BackupRequest{Volume: "gitea-data", Prune: true}) || rig.backups[1] != (BackupRequest{}) {
		t.Fatalf("requests: %+v", rig.backups)
	}
}

func TestMaintenanceEndpointsAreGatedByRole(t *testing.T) {
	rig := newMaintRig(t)
	for _, c := range []struct {
		path  string
		token string
		want  int
	}{
		{"/v1/backups", "", 401},
		{"/v1/backups", rig.viewer, 403},
		{"/v1/backups", rig.planner, 403},
		{"/v1/backups", rig.scoped, 403},
		// A restore replaces data: a deployer is not enough.
		{"/v1/backups/restore?volume=gitea-data", rig.deployer, 403},
	} {
		if res, _ := rig.post(t, c.path, c.token, "application/gzip", goodTree(t)); res.StatusCode != c.want {
			t.Errorf("POST %s: got %d, want %d", c.path, res.StatusCode, c.want)
		}
	}
}

func TestRestoreRunsAsAJob(t *testing.T) {
	rig := newMaintRig(t)
	res, body := rig.post(t, "/v1/backups/restore?volume=gitea-data&as=gitea-drill", rig.secret, "application/gzip", goodTree(t))
	if res.StatusCode != 202 {
		t.Fatalf("restore: %d %s", res.StatusCode, body)
	}
	if j := rig.wait(t, body); j.Kind != "restore" || j.Status != JobSucceeded {
		t.Fatalf("restore job: %+v", j)
	}
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if len(rig.restores) != 1 || rig.restores[0] != (RestoreRequest{Volume: "gitea-data", From: "latest", As: "gitea-drill"}) {
		t.Fatalf("restores %+v", rig.restores)
	}
}

func TestMaintenanceEndpointsRejectBadParameters(t *testing.T) {
	rig := newMaintRig(t)
	for _, path := range []string{
		"/v1/backups?volume=../etc",
		"/v1/backups?volume=a%20b",
		"/v1/backups/restore",
		"/v1/backups/restore?volume=v&as=bad%3Bname",
		"/v1/backups/restore?volume=v&from=../../x",
	} {
		if res, _ := rig.post(t, path, rig.secret, "application/gzip", goodTree(t)); res.StatusCode != 400 {
			t.Errorf("POST %s: got %d, want 400", path, res.StatusCode)
		}
	}
}
