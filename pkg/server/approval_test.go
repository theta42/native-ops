package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
)

func (r *applyRig) planned(t *testing.T) string {
	t.Helper()
	res, body := r.post(t, "/v1/plan?sha=cafe123", r.planner, "application/gzip", goodTree(t))
	if res.StatusCode != 200 {
		t.Fatalf("plan: %d %s", res.StatusCode, body)
	}
	var out struct{ Hash string }
	json.Unmarshal([]byte(body), &out)
	return out.Hash
}

func (r *applyRig) approve(t *testing.T, token, hash string) (*http.Response, string) {
	t.Helper()
	return r.post(t, "/v1/plans/"+hash+"/approve", token, "", nil)
}

func (r *applyRig) planView(t *testing.T, token, hash string) planView {
	t.Helper()
	_, body := r.do(t, "GET", "/v1/plans/"+hash, token)
	var v planView
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return v
}

// The point of the gate: a deployer token, which any branch of the repository can read, is not enough to
// change the host. What it can apply is only what an admin approved.
func TestApplyNeedsAnAdminsApprovalOfThatExactPlan(t *testing.T) {
	rig := newApplyRig(t)
	rig.autoApprove = false
	hash := rig.planned(t)

	res, body := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t))
	if res.StatusCode != 403 || !strings.Contains(body, `"code":"not_approved"`) || !strings.Contains(body, "/v1/plans/"+hash+"/approve") || !strings.Contains(body, "launch web") {
		t.Fatalf("an unapproved plan: %d %s", res.StatusCode, body)
	}
	if rig.applies.Load() != 0 || len(rig.jobsList(t)) != 0 {
		t.Fatal("nothing may run, and no job is created, without an approval")
	}
	if v := rig.planView(t, rig.viewer, hash); v.State != PlanPending {
		t.Fatalf("it should be waiting for an admin: %s", v.State)
	}

	// Nobody but an admin can approve: not the pipeline's own tokens, not a viewer.
	for name, tok := range map[string]string{"viewer": rig.viewer, "planner": rig.planner, "deployer": rig.deployerToken} {
		if res, _ := rig.approve(t, tok, hash); res.StatusCode != 403 {
			t.Errorf("a %s must not approve, got %d", name, res.StatusCode)
		}
	}
	if res, _ := rig.approve(t, "", hash); res.StatusCode != 401 {
		t.Errorf("no token: %d", res.StatusCode)
	}
	if res, _ := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t)); res.StatusCode != 403 || rig.applies.Load() != 0 {
		t.Fatal("still unapproved")
	}

	res, body = rig.approve(t, rig.secret, hash)
	var v planView
	json.Unmarshal([]byte(body), &v)
	if res.StatusCode != 200 || v.State != PlanApproved || v.Approval == nil || v.Approval.By != "ci-admin" || !v.Approval.Expires.After(time.Now()) {
		t.Fatalf("approve: %d %s", res.StatusCode, body)
	}
	res, body = rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t))
	if res.StatusCode != 202 {
		t.Fatalf("an approved plan: %d %s", res.StatusCode, body)
	}
	rig.waitJob(t, jobID(t, body), JobSucceeded)
	if used := rig.planView(t, rig.viewer, hash); used.State != PlanUsed || used.Used == nil || string(used.Used.Job) == "" {
		t.Fatalf("the approval is used up and says by which job: %+v", used)
	}
}

func TestAnApprovalIsForOneApplyOnly(t *testing.T) {
	rig := newApplyRig(t)
	rig.autoApprove = false
	hash := rig.planned(t)
	rig.approve(t, rig.secret, hash)

	_, body := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t))
	rig.waitJob(t, jobID(t, body), JobSucceeded)

	// Right after it ran, an approval that was used cannot be renewed.
	if res, _ := rig.approve(t, rig.secret, hash); res.StatusCode != 409 {
		t.Errorf("an approval that was used cannot be renewed, got %d", res.StatusCode)
	}
	// And presenting the same plan again does not reuse it: the plan is made again, so it is pending
	// again (a new occurrence), and an apply is refused until an admin approves that.
	res, b := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t))
	if res.StatusCode != 403 || !strings.Contains(b, `"code":"not_approved"`) || rig.applies.Load() != 1 {
		t.Fatalf("a second apply on the same approval: %d %s (applied %d times)", res.StatusCode, b, rig.applies.Load())
	}

	// The same changes pending once more later on is a new occurrence too: it does not inherit the old approval.
	hash2 := rig.planned(t)
	if hash2 != hash {
		t.Fatalf("test setup: the same plan should have the same hash")
	}
	if v := rig.planView(t, rig.viewer, hash); v.State != PlanPending || v.Used != nil {
		t.Fatalf("a plan that is pending again is pending again: %+v", v)
	}
	if res, _ := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t)); res.StatusCode != 403 {
		t.Fatalf("it needs a new approval, got %d", res.StatusCode)
	}
}

func TestAnApprovalExpiresAndCanBeWithdrawn(t *testing.T) {
	rig := newApplyRig(t)
	rig.autoApprove = false
	hash := rig.planned(t)

	now := time.Now().UTC()
	rig.plans.now = func() time.Time { return now }
	rig.approve(t, rig.secret, hash)
	now = now.Add(59 * time.Minute)
	if v := rig.planView(t, rig.viewer, hash); v.State != PlanApproved {
		t.Fatalf("still inside its hour: %s", v.State)
	}
	now = now.Add(2 * time.Minute)
	res, body := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t))
	if res.StatusCode != 403 || !strings.Contains(body, `"code":"approval_expired"`) || rig.applies.Load() != 0 {
		t.Fatalf("an expired approval: %d %s", res.StatusCode, body)
	}
	rig.plans.now = func() time.Time { return now } // the store's clock is the one that counts

	// Approving again renews it; withdrawing it takes it back.
	rig.approve(t, rig.secret, hash)
	res, _ = rig.post(t, "/v1/plans/"+hash+"/approve", rig.secret, "", nil)
	if res.StatusCode != 200 {
		t.Fatalf("renew: %d", res.StatusCode)
	}
	req, _ := http.NewRequest("DELETE", rig.srv.URL+"/v1/plans/"+hash+"/approval", nil)
	req.Header.Set("Authorization", "Bearer "+rig.deployerToken)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 403 {
		t.Fatalf("only an admin can withdraw an approval, got %d", r.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer "+rig.secret)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 200 {
		t.Fatalf("withdraw: %d", r.StatusCode)
	}
	if res, b := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t)); res.StatusCode != 403 || !strings.Contains(b, "not_approved") {
		t.Fatalf("a withdrawn approval is no approval: %d %s", res.StatusCode, b)
	}
}

func TestOnlyThePlanThatWasApprovedIsApplied(t *testing.T) {
	rig := newApplyRig(t)
	rig.autoApprove = false
	hash := rig.planned(t)
	rig.approve(t, rig.secret, hash)

	// The host moves: the plan now is a different one. The approval of the old one does not carry over.
	rig.plan.plan = &engine.FleetPlan{Services: []*engine.ServicePlan{{Service: "web", Action: engine.ActionUpdate,
		Changes: []engine.Change{{Kind: engine.ChangeSetLimits, Detail: "limits.cpu: 1 -> 8"}}}}}
	res, body := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t))
	if res.StatusCode != 409 || !strings.Contains(body, "plan_changed") || rig.applies.Load() != 0 {
		t.Fatalf("approved one plan, host now says another: %d %s", res.StatusCode, body)
	}
	// Asking for the new plan's hash, which nobody approved, is refused too.
	newHash := rig.plan.plan.Hash()
	if res, b := rig.apply(t, rig.deployerToken, "?expect="+newHash, goodTree(t)); res.StatusCode != 403 || !strings.Contains(b, "not_approved") {
		t.Fatalf("the new plan is unapproved: %d %s", res.StatusCode, b)
	}
	if rig.applies.Load() != 0 {
		t.Fatal("nothing ran")
	}
}

func TestOnlyAPlanWithChangesThatCanBeAppliedCanBeApproved(t *testing.T) {
	rig := newApplyRig(t)
	rig.autoApprove = false

	rig.plan.plan = &engine.FleetPlan{Services: []*engine.ServicePlan{{Service: "web", Action: engine.ActionBlocked, Blockers: []string{"no edge"}}}}
	blocked := rig.planned(t)
	rig.plan.plan = &engine.FleetPlan{Services: []*engine.ServicePlan{{Service: "web", Action: engine.ActionNone}}}
	nothing := rig.planned(t)
	for name, h := range map[string]string{"blocked": blocked, "nothing to change": nothing} {
		if res, body := rig.approve(t, rig.secret, h); res.StatusCode != 409 || !strings.Contains(body, "not_approvable") {
			t.Errorf("a %s plan: %d %s", name, res.StatusCode, body)
		}
	}
	if v := rig.planView(t, rig.viewer, blocked); v.State != PlanBlocked {
		t.Errorf("state %s", v.State)
	}
	if v := rig.planView(t, rig.viewer, nothing); v.State != PlanNothing {
		t.Errorf("state %s", v.State)
	}
	if res, _ := rig.approve(t, rig.secret, strings.Repeat("a", 64)); res.StatusCode != 404 {
		t.Errorf("an unknown plan: %d", res.StatusCode)
	}
	if res, _ := rig.approve(t, rig.secret, "not-a-hash"); res.StatusCode != 404 {
		t.Errorf("not a hash: %d", res.StatusCode)
	}
}

func TestPlansAreListedForEveryoneWithoutTheirText(t *testing.T) {
	rig := newApplyRig(t)
	rig.autoApprove = false
	hash := rig.planned(t)

	_, body := rig.do(t, "GET", "/v1/plans", rig.viewer)
	var list struct {
		Plans []planView `json:"plans"`
		TTL   int        `json:"approval_ttl_seconds"`
	}
	json.Unmarshal([]byte(body), &list)
	if len(list.Plans) != 1 || list.Plans[0].Hash != hash || list.Plans[0].Text != "" || list.Plans[0].Actor != "pr-ci" || list.Plans[0].Sha != "cafe123" || list.TTL != 3600 {
		t.Fatalf("the list: %s", body)
	}
	if v := rig.planView(t, rig.viewer, hash); !strings.Contains(v.Text, "launch web") || v.Counts["create"] != 1 {
		t.Fatalf("one plan shows what it says: %+v", v)
	}
	if res, _ := rig.do(t, "GET", "/v1/plans", ""); res.StatusCode != 401 {
		t.Errorf("plans need a token, got %d", res.StatusCode)
	}
}

func TestApprovalsSurviveARestartAndAreAudited(t *testing.T) {
	rig := newApplyRig(t)
	rig.autoApprove = false
	hash := rig.planned(t)
	rig.approve(t, rig.secret, hash)

	reopened, err := OpenPlans(filepath.Dir(filepath.Join(rig.plans.dir, "x")), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := reopened.Get(hash)
	if !ok || rec.State(time.Now().UTC()) != PlanApproved || rec.Approval.By != "ci-admin" {
		t.Fatalf("an approval must survive a restart of the daemon: %+v", rec)
	}
	st, _ := os.Stat(filepath.Join(rig.plans.dir, hash+".json"))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("plan files are 0600, got %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(rig.plans.dir); st.Mode().Perm() != 0o700 {
		t.Errorf("the plans directory is 0700, got %v", st.Mode().Perm())
	}

	raw, _ := os.ReadFile(rig.audit)
	log := string(raw)
	if !strings.Contains(log, `"detail":"approve plan=`+hash[:12]) {
		t.Fatalf("the approval is audited with who did it:\n%s", log)
	}
	if !strings.Contains(log, `"actor":"ci-admin"`) || strings.Contains(log, rig.secret) {
		t.Fatalf("who approved is in the log, the token is not:\n%s", log)
	}
}

func TestAPlanStoreBoundsItselfButNeverDropsAUsableApproval(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plans")
	p, err := OpenPlans(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	p.now = func() time.Time { return now }
	hashOf := func(i int) string {
		return strings.Repeat(string(rune('a'+i%6)), 60) + strings.Repeat("0", 4-len(itoa(i))) + itoa(i)
	}
	first := hashOf(0)
	p.Record(PlanSeen{Hash: first, Exit: 2, Actor: "ci"})
	if _, err := p.Approve(first, "admin"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < keepPlans+20; i++ {
		now = now.Add(time.Second)
		p.Record(PlanSeen{Hash: hashOf(i), Exit: 2, Actor: "ci"})
	}
	if n := len(p.List()); n > keepPlans+1 {
		t.Fatalf("bounded: %d", n)
	}
	if _, ok := p.Get(first); !ok {
		t.Fatal("an approval that can still be used is never pruned, however old the plan")
	}
	if _, ok := p.Get(hashOf(1)); ok {
		t.Fatal("the oldest plan without an approval is pruned")
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestPlanStateTransitions(t *testing.T) {
	now := time.Now().UTC()
	exp := now.Add(time.Hour)
	cases := []struct {
		name string
		rec  PlanRecord
		want PlanState
	}{
		{"nothing", PlanRecord{Exit: 0}, PlanNothing},
		{"blocked", PlanRecord{Exit: 1}, PlanBlocked},
		{"pending", PlanRecord{Exit: 2}, PlanPending},
		{"approved", PlanRecord{Exit: 2, Approval: &Approval{Expires: exp}}, PlanApproved},
		{"expired", PlanRecord{Exit: 2, Approval: &Approval{Expires: now}}, PlanExpired},
		{"used beats approved", PlanRecord{Exit: 2, Approval: &Approval{Expires: exp}, Used: &PlanUse{}}, PlanUsed},
		{"blocked beats approved", PlanRecord{Exit: 1, Approval: &Approval{Expires: exp}}, PlanBlocked},
	}
	for _, c := range cases {
		if got := c.rec.State(now); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
