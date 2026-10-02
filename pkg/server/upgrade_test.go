package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/status"
)

func TestDaemonUpgradeIsAnAdminJobThatRestartsOnlyOnSuccess(t *testing.T) {
	dir := t.TempDir()
	tokens, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	admin, _, _ := tokens.Create("ci-admin", RoleAdmin)
	deployer, _, _ := tokens.Create("ci-deploy", RoleDeployer)
	audit, _ := OpenAudit(filepath.Join(dir, "audit.log"))
	defer audit.Close()
	jobs, _ := OpenJobs(filepath.Join(dir, "jobs"))
	var restarts atomic.Int32
	fail := errors.New("checksum mismatch")
	var upgradeErr error
	s, err := New(Options{Tokens: tokens, Audit: audit, Version: "v1.55.0", Jobs: jobs,
		Upgrade: func(_ context.Context, v, sha string, logf func(string, ...any)) error {
			logf("installing %s", v)
			return upgradeErr
		},
		Restart: func() { restarts.Add(1) },
		Status:  func(context.Context) (*status.Snapshot, error) { return &status.Snapshot{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rig := &adminRig{url: ts.URL}
	good := map[string]any{"version": "v1.56.0", "sha256": strings.Repeat("a", 64)}

	if code, _, _ := rig.call(t, "POST", "/v1/daemon/upgrade", deployer, good); code != http.StatusForbidden {
		t.Fatalf("a deployer must not upgrade the daemon: %d", code)
	}
	if code, _, _ := rig.call(t, "POST", "/v1/daemon/upgrade", admin, map[string]any{"version": "latest", "sha256": "x"}); code != http.StatusBadRequest {
		t.Fatalf("an unpinned version or checksum must be refused: %d", code)
	}
	wait := func(id string) Job {
		for i := 0; i < 200; i++ {
			if j, ok := jobs.Get(JobID(id)); ok && j.Status != JobRunning {
				return j
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("job never finished")
		return Job{}
	}

	upgradeErr = fail
	code, out, _ := rig.call(t, "POST", "/v1/daemon/upgrade", admin, good)
	if code != http.StatusAccepted || out["from"] != "v1.55.0" || out["to"] != "v1.56.0" {
		t.Fatalf("upgrade: %d %v", code, out)
	}
	if j := wait(out["job"].(map[string]any)["id"].(string)); j.Status != JobFailed || j.Kind != "daemon:upgrade" {
		t.Fatalf("a failed install is a failed job: %+v", j)
	}
	if restarts.Load() != 0 {
		t.Fatal("a failed install must not restart the daemon")
	}

	upgradeErr = nil
	_, out, _ = rig.call(t, "POST", "/v1/daemon/upgrade", admin, good)
	if j := wait(out["job"].(map[string]any)["id"].(string)); j.Status != JobSucceeded {
		t.Fatalf("upgrade job: %+v", j)
	}
	time.Sleep(20 * time.Millisecond)
	if restarts.Load() != 1 {
		t.Fatalf("a successful install restarts the daemon once, got %d", restarts.Load())
	}
}
