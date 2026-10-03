package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
)

type deploysAnswer struct {
	Current *DeployView  `json:"current"`
	Running *DeployView  `json:"running"`
	Deploys []DeployView `json:"deploys"`
}

func (r *applyRig) deploys(t *testing.T, query string) deploysAnswer {
	t.Helper()
	res, body := r.do(t, "GET", "/v1/deploys"+query, r.viewer)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("deploys: %d %s", res.StatusCode, body)
	}
	var out deploysAnswer
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (r *applyRig) deploy(t *testing.T, want JobStatus) Job {
	t.Helper()
	res, body := r.post(t, "/v1/deploy", r.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", res.StatusCode, body)
	}
	return r.waitJob(t, jobID(t, body), want)
}

func TestTheDeploysSayWhatTheHostRunsAndWhatRanBefore(t *testing.T) {
	src := &fakeSource{protected: true, tree: tgz(t, entry{name: "fleet.yml", body: "name: x\n"})}
	rig := newDeployRig(t, src)
	if got := rig.deploys(t, ""); got.Current != nil || got.Running != nil || got.Deploys == nil || len(got.Deploys) != 0 {
		t.Fatalf("a host never deployed has no current deploy and an empty list: %+v", got)
	}

	applied := rig.deploy(t, JobSucceeded)
	if applied.Sha != deployCommit || applied.PlanHash != creatingPlan().Hash() || applied.Result != ResultApplied {
		t.Fatalf("the job records the commit, the plan and how it ended: %+v", applied)
	}
	got := rig.deploys(t, "")
	c := got.Current
	if c == nil || c.Job != applied.ID || c.Tag != "deploy-1" || c.Sha != deployCommit || c.Result != ResultApplied || c.Counts[engine.ActionCreate] != 1 {
		t.Fatalf("current: %+v", c)
	}

	// A deploy that fails is history, not what the host runs.
	src.protected = false
	failed := rig.deploy(t, JobFailed)
	got = rig.deploys(t, "")
	if len(got.Deploys) != 2 || got.Deploys[0].Job != failed.ID || got.Deploys[0].Status != JobFailed || got.Current.Job != applied.ID {
		t.Fatalf("after a failed deploy: %+v", got)
	}
	if got := rig.deploys(t, "?limit=1"); len(got.Deploys) != 1 || got.Current == nil {
		t.Fatalf("limit bounds the list, not current: %+v", got)
	}
	if res, _ := rig.do(t, "GET", "/v1/deploys?limit=0", rig.viewer); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a bad limit is refused: %d", res.StatusCode)
	}

	// Nothing to change still means the host matches the tag.
	src.protected = true
	rig.plan.plan = &engine.FleetPlan{Services: []*engine.ServicePlan{{Service: "web", Action: engine.ActionNone}}}
	same := rig.deploy(t, JobSucceeded)
	if got := rig.deploys(t, ""); got.Current.Job != same.ID || got.Current.Result != ResultNoChanges {
		t.Fatalf("a deploy with nothing to change is current: %+v", got.Current)
	}
}

func TestADeployInProgressIsRunning(t *testing.T) {
	src := &fakeSource{protected: true, tree: tgz(t, entry{name: "fleet.yml", body: "name: x\n"})}
	rig := newDeployRig(t, src)
	rig.block, rig.started = make(chan struct{}), make(chan struct{}, 1)
	_, body := rig.post(t, "/v1/deploy", rig.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	id := jobID(t, body)
	<-rig.started
	if got := rig.deploys(t, ""); got.Running == nil || string(got.Running.Job) != id || got.Current != nil {
		t.Fatalf("running: %+v", got)
	}

	// ?wait= answers when the wait is over if the job is still running ...
	start := time.Now()
	res, body := rig.do(t, "GET", "/v1/jobs/"+id+"?wait=1", rig.deployerToken)
	if res.StatusCode != 200 || !strings.Contains(body, `"status":"running"`) || time.Since(start) < time.Second {
		t.Fatalf("a wait that runs out answers with the running job: %d %s after %s", res.StatusCode, body, time.Since(start))
	}
	// ... and as soon as the job ends, if that is sooner.
	go func() { time.Sleep(300 * time.Millisecond); close(rig.block) }()
	start = time.Now()
	res, body = rig.do(t, "GET", "/v1/jobs/"+id+"?wait=30", rig.deployerToken)
	if res.StatusCode != 200 || !strings.Contains(body, `"status":"succeeded"`) || time.Since(start) > 5*time.Second {
		t.Fatalf("the wait ends with the job: %d %s after %s", res.StatusCode, body, time.Since(start))
	}
	for _, bad := range []string{"61", "-1", "x"} {
		if res, _ := rig.do(t, "GET", "/v1/jobs/"+id+"?wait="+bad, rig.viewer); res.StatusCode != http.StatusBadRequest {
			t.Errorf("wait=%s: %d", bad, res.StatusCode)
		}
	}
}

func TestJobsCanBeFilteredByKindAndStatus(t *testing.T) {
	src := &fakeSource{protected: true, tree: tgz(t, entry{name: "fleet.yml", body: "name: x\n"})}
	rig := newDeployRig(t, src)
	rig.deploy(t, JobSucceeded)
	jobs := rig.serverUnderTest.opts.Jobs
	for _, k := range []string{"instance:put", "instance:delete", "image:build"} {
		j, _ := jobs.CreateKind(k, "fleet", "", "demo-1", "")
		jobs.Finish(j.ID, nil)
	}
	j, _ := jobs.CreateKind("image:build", "ci", "", "", "")
	jobs.Finish(j.ID, errBusy("boom"))

	count := func(query string) int {
		t.Helper()
		res, body := rig.do(t, "GET", "/v1/jobs"+query, rig.viewer)
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d %s", query, res.StatusCode, body)
		}
		var out struct{ Jobs []Job }
		_ = json.Unmarshal([]byte(body), &out)
		return len(out.Jobs)
	}
	for q, want := range map[string]int{
		"": 5, "?kind=deploy": 1, "?kind=instance:": 2, "?kind=image:build": 2, "?kind=image:build&status=failed": 1,
		"?status=succeeded": 4, "?limit=2": 2, "?kind=nothing": 0,
	} {
		if got := count(q); got != want {
			t.Errorf("/v1/jobs%s: %d jobs, want %d", q, got, want)
		}
	}
	if res, _ := rig.do(t, "GET", "/v1/jobs?status=done", rig.viewer); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown status is refused: %d", res.StatusCode)
	}
}

func TestADeployPlanSaysWhatATagWouldChangeAndChangesNothing(t *testing.T) {
	pin := "daemon:\n  version: v9.9.9\n  sha256: " + strings.Repeat("a", 64) + "\n"
	src := &fakeSource{protected: true, tree: tgz(t, entry{name: "conf/fleet.yml", body: "name: x\n" + pin})}
	rig := newDeployRig(t, src)

	if res, _ := rig.post(t, "/v1/deploy/plan", rig.viewer, "application/json", []byte(`{"tag":"deploy-1"}`)); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a viewer must not plan: %d", res.StatusCode)
	}
	res, body := rig.post(t, "/v1/deploy/plan", rig.planner, "application/json", []byte(`{"tag":"deploy-1"}`))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	var out struct {
		Deployable bool           `json:"deployable"`
		Exit       int            `json:"exit"`
		Daemon     map[string]any `json:"daemon"`
		Counts     map[string]int `json:"counts"`
	}
	raw := map[string]any{}
	_ = json.Unmarshal([]byte(body), &raw)
	_ = json.Unmarshal([]byte(body), &out)
	hash := creatingPlan().Hash()
	if raw["sha"] != deployCommit || raw["hash"] != hash || raw["protected_by"] != "deploy-*" || !out.Deployable || out.Exit != 2 ||
		out.Counts[engine.ActionCreate] != 1 || !strings.Contains(raw["text"].(string), "web") {
		t.Fatalf("plan: %s", body)
	}
	if out.Daemon["running"] != "test" || out.Daemon["pinned"] != "v9.9.9" || out.Daemon["upgrade"] != false {
		t.Fatalf("daemon: %+v (a non-release build never upgrades)", out.Daemon)
	}
	if rig.applies.Load() != 0 || len(rig.jobsList(t)) != 0 {
		t.Fatal("a deploy plan must not apply or start a job")
	}
	if rec, ok := rig.plans.Get(hash); !ok || rec.Sha != deployCommit {
		t.Fatalf("the plan is on record: %+v", rec)
	}

	// An unprotected tag is planned, and said not to deploy.
	src.protected = false
	_, body = rig.post(t, "/v1/deploy/plan", rig.planner, "application/json", []byte(`{"tag":"deploy-1"}`))
	if !strings.Contains(body, `"deployable":false`) || !strings.Contains(body, "not protected") {
		t.Fatalf("unprotected: %s", body)
	}
	if res, body := rig.post(t, "/v1/deploy/plan", rig.planner, "application/json", []byte(`{"tag":"nope"}`)); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("an unknown tag: %d %s", res.StatusCode, body)
	}
	if res, _ := rig.post(t, "/v1/deploy/plan", rig.planner, "application/json", []byte(`{}`)); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("no tag: %d", res.StatusCode)
	}
}

func TestTheDeployEndpointsRoleGates(t *testing.T) {
	src := &fakeSource{protected: true, tree: tgz(t, entry{name: "fleet.yml", body: "name: x\n"})}
	rig := newDeployRig(t, src)
	scoped, _, err := rig.serverUnderTest.opts.Tokens.CreateScoped("fleet", RoleDeployer, Scope{Names: []string{"demo-*"}, Images: []string{"x:*"}})
	if err != nil {
		t.Fatal(err)
	}
	tag := []byte(`{"tag":"deploy-1"}`)
	for _, tc := range []struct {
		method, path, token string
		want                int
	}{
		{"GET", "/v1/deploys", "", 401},
		{"GET", "/v1/deploys", scoped, 403},
		{"GET", "/v1/deploys", rig.viewer, 200},
		{"POST", "/v1/deploy/plan", "", 401},
		{"POST", "/v1/deploy/plan", scoped, 403},
		{"POST", "/v1/deploy/plan", rig.viewer, 403},
		{"POST", "/v1/deploy/plan", rig.planner, 200},
	} {
		var res *http.Response
		if tc.method == "GET" {
			res, _ = rig.do(t, tc.method, tc.path, tc.token)
		} else {
			res, _ = rig.post(t, tc.path, tc.token, "application/json", tag)
		}
		if res.StatusCode != tc.want {
			t.Errorf("%s %s with %.8s: %d, want %d", tc.method, tc.path, tc.token, res.StatusCode, tc.want)
		}
	}
	if src.fetched != 1 {
		t.Fatalf("only the allowed plan may read the git server: fetched %d times", src.fetched)
	}
}
