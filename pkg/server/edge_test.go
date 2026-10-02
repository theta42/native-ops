package server

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/status"
)

// edgeCall records one call to fakeEdgeApply.fn. sawCaddyfile is read from
// inside the call, because the uploaded tree is removed as soon as the call
// returns (before the job is reported finished).
type edgeCall struct {
	dir          string
	sawCaddyfile bool
}

type fakeEdgeApply struct {
	calls []edgeCall
	err   error
}

func (f *fakeEdgeApply) fn(_ context.Context, dir string, logf func(string, ...any)) error {
	_, err := os.Stat(filepath.Join(dir, "edge", "Caddyfile"))
	f.calls = append(f.calls, edgeCall{dir, err == nil})
	if logf != nil {
		logf("applying edge config from %s", dir)
	}
	return f.err
}

type edgeRig struct {
	*fixture
	b        *fakeEdgeApply
	srv2     *Server
	deployer string
	scoped   string
}

func newEdgeRig(t *testing.T) *edgeRig {
	t.Helper()
	dir := t.TempDir()
	tokens, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	admin, _, _ := tokens.Create("ci-admin", RoleAdmin)
	viewer, _, _ := tokens.Create("dash", RoleViewer)
	planner, _, _ := tokens.Create("pr-ci", RolePlanner)
	rig := &edgeRig{b: &fakeEdgeApply{}}
	rig.deployer, _, _ = tokens.Create("ci-edge", RoleDeployer)
	var err error
	rig.scoped, _, err = tokens.CreateScoped("fleet-manager", RoleDeployer, Scope{Names: []string{"demo-*"}, Images: []string{"opsavor-demo:*"}})
	if err != nil {
		t.Fatal(err)
	}
	audit, _ := OpenAudit(filepath.Join(dir, "audit.log"))
	t.Cleanup(func() { audit.Close() })
	jobs, err := OpenJobs(filepath.Join(dir, "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Tokens: tokens, Audit: audit, Version: "test", Jobs: jobs, EdgeApply: rig.b.fn,
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

func (r *edgeRig) apply(t *testing.T, token string, body []byte) (int, string) {
	t.Helper()
	res, b := r.post(t, "/v1/edge/apply", token, "application/gzip", body)
	return res.StatusCode, b
}

func (r *edgeRig) waitJob(t *testing.T, id string, want JobStatus) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, ok := r.srv2.opts.Jobs.Get(JobID(id))
		if ok && j.Status == want {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never became %s", id, want)
	return Job{}
}

func edgeTree(t *testing.T) []byte {
	return tgz(t, entry{name: "fleet.yml", body: "name: prod\n"}, entry{name: "edge/Caddyfile", body: "import /etc/caddy/sites/*.caddy\n"})
}

func TestEdgeApplyRunsAsAJobAndSeesTheCaddyfile(t *testing.T) {
	rig := newEdgeRig(t)
	code, body := rig.apply(t, rig.deployer, edgeTree(t))
	if code != 202 {
		t.Fatalf("%d %s", code, body)
	}
	j := rig.waitJob(t, jobID(t, body), JobSucceeded)
	if j.Kind != "edge:apply" {
		t.Fatalf("job kind: %+v", j)
	}
	if len(rig.b.calls) != 1 || !rig.b.calls[0].sawCaddyfile {
		t.Fatalf("the apply did not see edge/Caddyfile: %+v", rig.b.calls)
	}
}

func TestEdgeApplyNeedsADeployerToken(t *testing.T) {
	rig := newEdgeRig(t)
	if code, _ := rig.apply(t, "", edgeTree(t)); code != 401 {
		t.Fatalf("no token: got %d, want 401", code)
	}
	if code, _ := rig.apply(t, rig.viewer, edgeTree(t)); code != 403 {
		t.Fatalf("viewer: got %d, want 403", code)
	}
	// A scoped (instance-only) token must not reach a host-wide endpoint.
	if code, _ := rig.apply(t, rig.scoped, edgeTree(t)); code != 403 {
		t.Fatalf("scoped deployer: got %d, want 403", code)
	}
}

func TestEdgeApplyRejectsABadUpload(t *testing.T) {
	rig := newEdgeRig(t)
	if code, _ := rig.apply(t, rig.deployer, []byte("not a tar")); code != 400 {
		t.Fatalf("bad archive: got %d, want 400", code)
	}
	res, _ := rig.post(t, "/v1/edge/apply", rig.deployer, "text/plain", edgeTree(t))
	if res.StatusCode != 415 {
		t.Fatalf("bad content type: got %d, want 415", res.StatusCode)
	}
}
