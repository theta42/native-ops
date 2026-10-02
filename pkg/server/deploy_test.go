package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
)

const deployCommit = "0123456789abcdef0123456789abcdef01234567"

// fakeSource is a git server with one protected deploy tag.
type fakeSource struct {
	tree      []byte
	protected bool
	fetched   int
}

func (f *fakeSource) Commit(_ context.Context, tag string) (string, error) {
	if tag != "deploy-1" {
		return "", errors.New("no such tag")
	}
	return deployCommit, nil
}
func (f *fakeSource) Protected(_ context.Context, tag string) (string, error) {
	if !f.protected {
		return "", errors.New("tag deploy-1 is not protected")
	}
	return "deploy-*", nil
}
func (f *fakeSource) Archive(_ context.Context, sha string) (io.ReadCloser, error) {
	f.fetched++
	return io.NopCloser(bytes.NewReader(f.tree)), nil
}

func newDeployRig(t *testing.T, src *fakeSource) *applyRig {
	return newApplyRigWith(t, func(o *Options) { o.Deploy, o.DeployTags = src, "deploy-*" })
}

func TestAProtectedTagDeploysTheCommitFromTheGitServer(t *testing.T) {
	src := &fakeSource{protected: true, tree: tgz(t, entry{name: "conf/fleet.yml", body: "name: from-the-tag\n"}, entry{name: "conf/services/web/service.yml", body: "image: x\n"})}
	rig := newDeployRig(t, src)
	res, body := rig.post(t, "/v1/deploy", rig.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	j := rig.waitJob(t, jobID(t, body), JobSucceeded)
	if j.Kind != "deploy" || j.Service != "deploy-1" || !strings.Contains(j.Log, deployCommit[:12]) {
		t.Fatalf("job: %+v", j)
	}
	rig.mu.Lock()
	applied := append([]string(nil), rig.applied...)
	rig.mu.Unlock()
	if len(applied) != 1 || applied[0] != "name: from-the-tag\n" || src.fetched != 1 {
		t.Fatalf("the tree applied must be the tagged commit's, from the git server: %q (fetched %d)", applied, src.fetched)
	}
	// The plan is on record as approved by the tag and used by the job: it is done, not pending.
	hash := creatingPlan().Hash()
	rec, ok := rig.plans.Get(hash)
	if !ok || rec.Used == nil || rec.Used.Job != j.ID || !strings.Contains(rec.Approval.By, "tag deploy-1") || rec.Sha != deployCommit {
		t.Fatalf("plan record: %+v", rec)
	}
	if got := rig.serverUnderTest.viewOf(rec).State; got != PlanUsed {
		t.Fatalf("a deployed plan is used, got %s", got)
	}
}

func TestADeployRefusesAnUnprotectedTagAndAPlannerToken(t *testing.T) {
	src := &fakeSource{protected: false, tree: tgz(t, entry{name: "fleet.yml", body: "name: x\n"})}
	rig := newDeployRig(t, src)
	if res, _ := rig.post(t, "/v1/deploy", rig.planner, "application/json", []byte(`{"tag":"deploy-1"}`)); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a planner must not deploy: %d", res.StatusCode)
	}
	res, body := rig.post(t, "/v1/deploy", rig.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	j := rig.waitJob(t, jobID(t, body), JobFailed)
	if !strings.Contains(j.Error, "not protected") || rig.applies.Load() != 0 || src.fetched != 0 {
		t.Fatalf("an unprotected tag must not be fetched or applied: %+v", j)
	}
}

func TestADeployWithNothingToChangeAppliesNothing(t *testing.T) {
	src := &fakeSource{protected: true, tree: tgz(t, entry{name: "fleet.yml", body: "name: x\n"})}
	rig := newDeployRig(t, src)
	rig.plan.plan = &engine.FleetPlan{Services: []*engine.ServicePlan{{Service: "web", Action: engine.ActionNone}}}
	_, body := rig.post(t, "/v1/deploy", rig.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	j := rig.waitJob(t, jobID(t, body), JobSucceeded)
	if rig.applies.Load() != 0 || !strings.Contains(j.Log, "nothing to change") {
		t.Fatalf("nothing to change must apply nothing: %+v", j)
	}
}

func TestAPlanMadeBeforeTheHostChangedIsStaleAndCannotBeApproved(t *testing.T) {
	rig := newApplyRig(t)
	rig.autoApprove = false
	res, body := rig.post(t, "/v1/plan", rig.deployerToken, "application/gzip", goodTree(t))
	if res.StatusCode != 200 {
		t.Fatalf("plan: %d %s", res.StatusCode, body)
	}
	hash := creatingPlan().Hash()
	rec, _ := rig.plans.Get(hash)
	if got := rig.serverUnderTest.viewOf(rec).State; got != PlanPending {
		t.Fatalf("a fresh plan is pending, got %s", got)
	}
	// The host changes afterwards (any job that changes it).
	time.Sleep(10 * time.Millisecond)
	j, _ := rig.serverUnderTest.opts.Jobs.CreateKind("edge:apply", "ci-edge", "", "", "")
	rig.serverUnderTest.opts.Jobs.Finish(j.ID, nil)
	rec, _ = rig.plans.Get(hash)
	if got := rig.serverUnderTest.viewOf(rec).State; got != PlanStale {
		t.Fatalf("a plan made before the host changed is stale, got %s", got)
	}
	if res, _ := rig.post(t, "/v1/plans/"+hash+"/approve", rig.secret, "", nil); res.StatusCode != http.StatusConflict {
		t.Fatalf("a stale plan must not be approvable: %d", res.StatusCode)
	}
	// The plans list says whether apply is on, so the UI can tell review-only from waiting.
	_, list := rig.do(t, "GET", "/v1/plans", rig.viewer)
	if !strings.Contains(list, `"apply_enabled":true`) || !strings.Contains(list, `"state":"stale"`) {
		t.Fatalf("plans list: %s", list)
	}
}
