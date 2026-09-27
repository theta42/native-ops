package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
	"github.com/theta42/native-ops/pkg/status"
)

// applyRig is a server with a fake plan source and a fake apply, and the job store on disk.
type applyRig struct {
	*fixture
	plan    *planRecorder
	mu      sync.Mutex
	applied []string // the tree's fleet.yml at the moment of each apply
	applies atomic.Int32
	err     error
	panics  bool
	block   chan struct{}
	started chan struct{}
	jobsDir string

	deployerToken   string
	serverUnderTest *Server

	plans *Plans
	// autoApprove makes apply() plan and have an admin approve first, so the tests of what an apply does
	// once it is allowed to run stay about that. The gate's own tests turn it off.
	autoApprove bool
}

func newApplyRig(t *testing.T) *applyRig { return newApplyRigWith(t, nil) }

// newApplyRigWith is newApplyRig with a chance to change the server's options before it is built, so a
// test can turn on more of the daemon (the instance endpoints, say) next to apply.
func newApplyRigWith(t *testing.T, tweak func(*Options)) *applyRig {
	t.Helper()
	rig := &applyRig{plan: &planRecorder{plan: creatingPlan()}, jobsDir: filepath.Join(t.TempDir(), "jobs"), autoApprove: true}
	jobs, err := OpenJobs(rig.jobsDir)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	plans, err := OpenPlans(filepath.Join(dir, "plans"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rig.plans = plans
	tokens, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	admin, _, _ := tokens.Create("ci-admin", RoleAdmin)
	viewer, _, _ := tokens.Create("dash", RoleViewer)
	deployer, _, _ := tokens.Create("ci", RoleDeployer)
	planner, _, _ := tokens.Create("pr-ci", RolePlanner)
	audit, _ := OpenAudit(filepath.Join(dir, "audit.log"))
	t.Cleanup(func() { audit.Close() })
	opts := Options{Tokens: tokens, Audit: audit, Version: "test", Plan: rig.plan.fn, Jobs: jobs, Plans: plans,
		Status: func(context.Context) (*status.Snapshot, error) { return &status.Snapshot{}, nil },
		Apply: func(ctx context.Context, cfg string, p *engine.FleetPlan, logf func(string, ...any)) error {
			rig.applies.Add(1)
			b, _ := os.ReadFile(filepath.Join(cfg, "fleet.yml"))
			rig.mu.Lock()
			rig.applied = append(rig.applied, string(b))
			rig.mu.Unlock()
			logf("deploying %s", p.Services[0].Service)
			if rig.started != nil {
				rig.started <- struct{}{}
			}
			if rig.block != nil {
				<-rig.block
			}
			if rig.panics {
				panic("boom")
			}
			return rig.err
		}}
	if tweak != nil {
		tweak(&opts)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	rig.fixture = &fixture{srv: ts, secret: admin, viewer: viewer, audit: filepath.Join(dir, "audit.log"), calls: &atomic.Int32{}}
	rig.deployerToken = deployer
	rig.fixture.planner = planner
	rig.serverUnderTest = s
	// Runs before the temp directories are removed: a job accepted by a test but not awaited by it
	// must not still be writing into them.
	t.Cleanup(func() { s.WaitForJobs(5 * time.Second) })
	return rig
}

func (r *applyRig) apply(t *testing.T, token, query string, body []byte) (*http.Response, string) {
	t.Helper()
	if r.autoApprove {
		r.approveFor(t, query, body)
	}
	return r.post(t, "/v1/apply"+query, token, "application/gzip", body)
}

// approveFor does what a person does before an apply: the plan is made, and an admin approves the hash
// the apply is about to ask for. Plans that cannot be approved (blocked, nothing to do) just stay so.
func (r *applyRig) approveFor(t *testing.T, query string, body []byte) {
	t.Helper()
	const key = "expect="
	i := strings.Index(query, key)
	if i < 0 || len(query) < i+len(key)+64 {
		return
	}
	hash := query[i+len(key) : i+len(key)+64]
	r.post(t, "/v1/plan", r.deployerToken, "application/gzip", body)
	r.post(t, "/v1/plans/"+hash+"/approve", r.secret, "", nil)
}

func (r *applyRig) waitJob(t *testing.T, id string, want JobStatus) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, body := r.do(t, "GET", "/v1/jobs/"+id, r.deployerToken)
		var j Job
		json.Unmarshal([]byte(body), &j)
		if j.Status == want {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never became %s", id, want)
	return Job{}
}

func jobID(t *testing.T, body string) string {
	var out struct {
		Job Job `json:"job"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Job.ID == "" {
		t.Fatalf("no job in %s (%v)", body, err)
	}
	return string(out.Job.ID)
}

func TestApplyRunsTheReviewedPlanAsAJobAndKeepsTheRecord(t *testing.T) {
	rig := newApplyRig(t)
	hash := creatingPlan().Hash()

	res, body := rig.apply(t, rig.deployerToken, "?expect="+hash+"&sha=cafe123&service=web", goodTree(t))
	if res.StatusCode != 202 || !strings.HasPrefix(res.Header.Get("Location"), "/v1/jobs/j-") {
		t.Fatalf("%d %s (%v)", res.StatusCode, body, res.Header)
	}
	id := jobID(t, body)
	j := rig.waitJob(t, id, JobSucceeded)
	if j.Actor != "ci" || j.Sha != "cafe123" || j.Service != "web" || j.PlanHash != hash || j.Finished == nil {
		t.Fatalf("job: %+v", j)
	}
	if !strings.Contains(j.Log, "deploying web") || !strings.Contains(j.Log, "done") {
		t.Fatalf("the job's log should have what happened:\n%s", j.Log)
	}
	if rig.applied[0] != "name: prod\n" {
		t.Fatalf("apply must get the uploaded tree, got %q", rig.applied[0])
	}
	// The tree is gone, the record is on disk.
	if _, err := os.Stat(filepath.Join(rig.jobsDir, id+".json")); err != nil {
		t.Fatalf("the job must be persisted: %v", err)
	}
	st, _ := os.Stat(filepath.Join(rig.jobsDir, id+".json"))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("job files are 0600, got %v", st.Mode().Perm())
	}
	// "succeeded" must mean the host is free again, at once: the tree is gone and the next apply is
	// accepted, not refused as busy by a job that is over.
	if _, err := os.Stat(rig.plan.dirs[0]); !os.IsNotExist(err) {
		t.Fatal("the unpacked tree must be removed before the job is reported finished")
	}
	if res, b := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t)); res.StatusCode != 202 {
		t.Fatalf("the moment a job reports succeeded the next apply must be accepted, got %d %s", res.StatusCode, b)
	}

	raw, _ := os.ReadFile(rig.audit)
	log := string(raw)
	if !strings.Contains(log, `"detail":"apply job=`+id) || !strings.Contains(log, `"method":"JOB"`) || !strings.Contains(log, `"detail":"apply succeeded"`) {
		t.Fatalf("the request and the job's end are both audited:\n%s", log)
	}
	if strings.Contains(log, rig.deployerToken) || strings.Contains(log, "?") {
		t.Fatalf("no token or query string in the audit log:\n%s", log)
	}
}

func TestApplyRefusesWithoutTheHashOfAReviewedPlan(t *testing.T) {
	rig := newApplyRig(t)
	rig.autoApprove = false // these are refusals: nothing may reach the plan, not even a helper's
	for _, q := range []string{"", "?expect=", "?expect=abc", "?expect=" + strings.Repeat("g", 64), "?expect=" + strings.Repeat("A", 64), "?expect=" + strings.Repeat("a", 65)} {
		res, body := rig.apply(t, rig.deployerToken, q, goodTree(t))
		if res.StatusCode != 400 || !strings.Contains(body, "bad_request") {
			t.Errorf("%q: %d %s, want 400", q, res.StatusCode, body)
		}
	}
	if rig.applies.Load() != 0 || len(rig.plan.dirs) != 0 {
		t.Fatal("nothing may be unpacked, planned or applied without the hash")
	}
	rig.autoApprove = true
	// A refusal must not leave the host locked.
	if res, _ := rig.apply(t, rig.deployerToken, "?expect="+creatingPlan().Hash(), goodTree(t)); res.StatusCode != 202 {
		t.Fatalf("the next apply must be able to run, got %d", res.StatusCode)
	}
}

func TestApplyStopsWhenThePlanIsNotTheOneThatWasReviewed(t *testing.T) {
	rig := newApplyRig(t)
	stale := strings.Repeat("0", 64)
	res, body := rig.apply(t, rig.deployerToken, "?expect="+stale, goodTree(t))
	if res.StatusCode != 409 || !strings.Contains(body, `"code":"plan_changed"`) || !strings.Contains(body, creatingPlan().Hash()) || !strings.Contains(body, "launch web") {
		t.Fatalf("the answer must carry the plan that would run now, so it can be reviewed: %d %s", res.StatusCode, body)
	}
	if rig.applies.Load() != 0 {
		t.Fatal("a plan that changed since it was reviewed must not be applied")
	}
	if jobs := rig.jobsList(t); len(jobs) != 0 {
		t.Fatalf("no job for a refused apply: %v", jobs)
	}

	// The same tree, but the host moved: the plan the daemon makes now differs from the reviewed one.
	reviewed := creatingPlan().Hash()
	rig.plan.plan = &engine.FleetPlan{Services: []*engine.ServicePlan{{Service: "web", Action: engine.ActionUpdate,
		Changes: []engine.Change{{Kind: engine.ChangeSetLimits, Detail: "limits.cpu: 1 -> 2"}}}}}
	if res, body := rig.apply(t, rig.deployerToken, "?expect="+reviewed, goodTree(t)); res.StatusCode != 409 || rig.applies.Load() != 0 {
		t.Fatalf("%d %s calls=%d", res.StatusCode, body, rig.applies.Load())
	}
}

func (r *applyRig) jobsList(t *testing.T) []Job {
	_, body := r.do(t, "GET", "/v1/jobs", r.viewer)
	var out struct {
		Jobs []Job `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out.Jobs
}

func TestApplyNeverRunsABlockedPlanOrStartsAJobForNothing(t *testing.T) {
	rig := newApplyRig(t)
	rig.plan.plan = &engine.FleetPlan{Services: []*engine.ServicePlan{{Service: "web", Action: engine.ActionBlocked, Blockers: []string{"no edge"}}}}
	res, body := rig.apply(t, rig.deployerToken, "?expect="+rig.plan.plan.Hash(), goodTree(t))
	if res.StatusCode != 409 || !strings.Contains(body, `"code":"blocked"`) || rig.applies.Load() != 0 {
		t.Fatalf("a blocked plan even with the right hash: %d %s", res.StatusCode, body)
	}

	rig.plan.plan = &engine.FleetPlan{Services: []*engine.ServicePlan{{Service: "web", Action: engine.ActionNone}}}
	res, body = rig.apply(t, rig.deployerToken, "?expect="+rig.plan.plan.Hash(), goodTree(t))
	if res.StatusCode != 200 || !strings.Contains(body, "nothing_to_do") || rig.applies.Load() != 0 || len(rig.jobsList(t)) != 0 {
		t.Fatalf("nothing to change is not a job: %d %s", res.StatusCode, body)
	}
}

func TestApplyNeedsADeployerAndOnlyShowsLogsToOne(t *testing.T) {
	rig := newApplyRig(t)
	rig.autoApprove = false // the refusals below must reach nothing, not even a helper's plan
	hash := creatingPlan().Hash()
	for _, tok := range []string{"", "garbage"} {
		if res, _ := rig.apply(t, tok, "?expect="+hash, goodTree(t)); res.StatusCode != 401 {
			t.Errorf("token %q: %d", tok, res.StatusCode)
		}
	}
	if res, _ := rig.apply(t, rig.viewer, "?expect="+hash, goodTree(t)); res.StatusCode != 403 {
		t.Fatalf("a viewer must not apply, got %d", res.StatusCode)
	}
	// The pull-request pipeline's token can plan but never apply, whatever the tree says.
	if res, _ := rig.apply(t, rig.planner, "?expect="+hash, goodTree(t)); res.StatusCode != 403 {
		t.Fatalf("a planner must not apply, got %d", res.StatusCode)
	}
	if rig.applies.Load() != 0 || len(rig.plan.dirs) != 0 {
		t.Fatal("a refused request reaches nothing")
	}

	rig.autoApprove = true
	_, body := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t))
	id := jobID(t, body)
	rig.waitJob(t, id, JobSucceeded)

	for _, c := range []struct {
		token   string
		wantLog bool
	}{{rig.viewer, false}, {rig.planner, false}, {rig.deployerToken, true}, {rig.secret, true}} {
		res, b := rig.do(t, "GET", "/v1/jobs/"+id, c.token)
		var j Job
		json.Unmarshal([]byte(b), &j)
		if res.StatusCode != 200 || j.Status != JobSucceeded || (j.Log != "") != c.wantLog {
			t.Errorf("token %.4s: %d log=%v want %v", c.token, res.StatusCode, j.Log != "", c.wantLog)
		}
	}
	if res, _ := rig.do(t, "GET", "/v1/jobs", ""); res.StatusCode != 401 {
		t.Errorf("the job list needs a token, got %d", res.StatusCode)
	}
	for _, l := range rig.jobsList(t) {
		if l.Log != "" {
			t.Error("the list carries no logs")
		}
	}
	for _, bad := range []string{"../x", "j-1-2", "j-123456789-ZZZZZZZZ", "nope", "%2e%2e"} {
		if res, _ := rig.do(t, "GET", "/v1/jobs/"+bad, rig.secret); res.StatusCode != 404 {
			t.Errorf("job id %q: %d, want 404", bad, res.StatusCode)
		}
	}
}

func TestOnlyOneApplyRunsAtATime(t *testing.T) {
	rig := newApplyRig(t)
	rig.block, rig.started = make(chan struct{}), make(chan struct{}, 4)
	hash := creatingPlan().Hash()

	_, body := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t))
	id := jobID(t, body)
	<-rig.started

	res, b := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t))
	if res.StatusCode != 409 || !strings.Contains(b, `"code":"busy"`) || !strings.Contains(b, id) {
		t.Fatalf("a second apply while one runs: %d %s", res.StatusCode, b)
	}
	close(rig.block)
	rig.waitJob(t, id, JobSucceeded)

	rig.block = nil
	if res, _ := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t)); res.StatusCode != 202 {
		t.Fatalf("the lock must be free after the job, got %d", res.StatusCode)
	}
}

func TestOfManySimultaneousAppliesExactlyOneRuns(t *testing.T) {
	rig := newApplyRig(t)
	rig.block, rig.started = make(chan struct{}), make(chan struct{}, 16)
	hash := creatingPlan().Hash()
	var accepted, refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, _ := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t))
			switch res.StatusCode {
			case 202:
				accepted.Add(1)
			case 409:
				refused.Add(1)
			}
		}()
	}
	wg.Wait()
	// The accepted request answers 202 before its job goroutine has necessarily started: wait for
	// the one apply to actually begin, then check that no second one ever does.
	select {
	case <-rig.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the accepted apply never started")
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case <-rig.started:
		t.Fatal("a second apply started")
	default:
	}
	close(rig.block)
	if accepted.Load() != 1 || refused.Load() != 7 || rig.applies.Load() != 1 {
		t.Fatalf("accepted=%d refused=%d applied=%d, want exactly one", accepted.Load(), refused.Load(), rig.applies.Load())
	}
}

func TestAFailedOrPanickingApplyIsRecordedAndFreesTheHost(t *testing.T) {
	rig := newApplyRig(t)
	hash := creatingPlan().Hash()

	rig.err = errors.New("apply web: health gate failed")
	_, body := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t))
	j := rig.waitJob(t, jobID(t, body), JobFailed)
	if !strings.Contains(j.Error, "health gate failed") || !strings.Contains(j.Log, "FAILED") {
		t.Fatalf("the failure must be in the record: %+v", j)
	}

	rig.err, rig.panics = nil, true
	_, body = rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t))
	j = rig.waitJob(t, jobID(t, body), JobFailed)
	if !strings.Contains(j.Error, "panicked") {
		t.Fatalf("a panic is a failed job, not a dead daemon: %+v", j)
	}

	rig.panics = false
	_, body = rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t))
	rig.waitJob(t, jobID(t, body), JobSucceeded)
}

func TestApplyRejectsBadUploadsWithoutLockingTheHostOrStartingAJob(t *testing.T) {
	rig := newApplyRig(t)
	hash := creatingPlan().Hash()
	if res, _ := rig.apply(t, rig.deployerToken, "?expect="+hash, []byte("junk")); res.StatusCode != 400 {
		t.Fatalf("junk: %d", res.StatusCode)
	}
	rig.plan.err = fmt.Errorf("%w: bad manifest", engine.ErrBadConfig)
	if res, _ := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t)); res.StatusCode != 400 {
		t.Fatalf("bad config: %d", res.StatusCode)
	}
	rig.plan.err = errors.New("incus: permission denied for secretuser")
	if res, body := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t)); res.StatusCode != 502 || strings.Contains(body, "secretuser") {
		t.Fatalf("an unreadable host is a generic 502: %d %s", res.StatusCode, body)
	}
	rig.plan.err = nil
	if rig.applies.Load() != 0 || len(rig.jobsList(t)) != 0 {
		t.Fatal("none of these may apply anything")
	}
	if res, _ := rig.apply(t, rig.deployerToken, "?expect="+hash, goodTree(t)); res.StatusCode != 202 {
		t.Fatalf("the host must not stay locked after a refusal: %d", res.StatusCode)
	}
}

func TestJobsSurviveARestartAndARunningOneIsMarkedInterrupted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "jobs")
	j1, err := OpenJobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	done, _ := j1.Create("ci", "abc1234", "web", strings.Repeat("a", 64))
	j1.Logf(done.ID, "did a thing")
	j1.Finish(done.ID, nil)
	running, _ := j1.Create("ci", "", "", strings.Repeat("b", 64))
	j1.Logf(running.ID, "half way")

	j2, err := OpenJobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := j2.Get(done.ID)
	if !ok || d.Status != JobSucceeded || !strings.Contains(d.Log, "did a thing") {
		t.Fatalf("a finished job is kept: %+v", d)
	}
	r, _ := j2.Get(running.ID)
	if r.Status != JobInterrupted || r.Finished == nil || !strings.Contains(r.Error, "Run the apply again") || !strings.Contains(r.Log, "half way") {
		t.Fatalf("a job that was running when the daemon stopped did not finish; say so and keep its log: %+v", r)
	}
	if _, running := j2.Running(); running {
		t.Fatal("an interrupted job is not running")
	}
	// ...and that is persisted, not just reported.
	j3, _ := OpenJobs(dir)
	if r3, _ := j3.Get(running.ID); r3.Status != JobInterrupted {
		t.Fatalf("got %s", r3.Status)
	}
	os.WriteFile(filepath.Join(dir, "not-a-job.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(dir, "j-123456789-deadbeef.json"), []byte("garbage"), 0o600)
	if _, err := OpenJobs(dir); err != nil {
		t.Fatalf("stray files must not stop the daemon starting: %v", err)
	}
}

func TestJobsAreBoundedInNumberAndLogSize(t *testing.T) {
	j, _ := OpenJobs(filepath.Join(t.TempDir(), "jobs"))
	var first Job
	for i := 0; i < keepJobs+5; i++ {
		job, _ := j.Create("ci", "", "", strings.Repeat("a", 64))
		j.Finish(job.ID, nil)
		if i == 0 {
			first = job
		}
		time.Sleep(time.Millisecond)
	}
	if n := len(j.List()); n != keepJobs {
		t.Fatalf("keeps the newest %d, has %d", keepJobs, n)
	}
	if _, ok := j.Get(first.ID); ok {
		t.Fatal("the oldest is pruned")
	}
	if _, err := os.Stat(filepath.Join(j.dir, string(first.ID)+".json")); !os.IsNotExist(err) {
		t.Fatal("and its file is removed")
	}

	job, _ := j.Create("ci", "", "", strings.Repeat("a", 64))
	line := strings.Repeat("x", 1000)
	for i := 0; i < 400; i++ {
		j.Logf(job.ID, "%s", line)
	}
	got, _ := j.Get(job.ID)
	if len(got.Log) > maxJobLogBytes+100 || !got.LogTruncated || !strings.Contains(got.Log, "log truncated") {
		t.Fatalf("a log is bounded and says when it was cut: %d bytes truncated=%v", len(got.Log), got.LogTruncated)
	}
}

func TestApplyNeedsAPlanSourceAndAJobStore(t *testing.T) {
	dir := t.TempDir()
	tokens, _ := OpenTokenStore(filepath.Join(dir, "t.json"))
	st := func(context.Context) (*status.Snapshot, error) { return &status.Snapshot{}, nil }
	jobs, _ := OpenJobs(filepath.Join(dir, "jobs"))
	apply := func(context.Context, string, *engine.FleetPlan, func(string, ...any)) error { return nil }
	plan := func(context.Context, string, string) (*engine.FleetPlan, error) { return nil, nil }
	if _, err := New(Options{Tokens: tokens, Status: st, Apply: apply, Plan: plan}); err == nil {
		t.Error("apply without a job store must not start: there would be no record of what it did")
	}
	if _, err := New(Options{Tokens: tokens, Status: st, Apply: apply, Jobs: jobs}); err == nil {
		t.Error("apply without a plan source has nothing to check the hash against")
	}
	if _, err := New(Options{Tokens: tokens, Status: st, Jobs: jobs}); err == nil {
		t.Error("a job store without apply is a misconfiguration")
	}
}

func TestShutdownWaitsForARunningApply(t *testing.T) {
	rig := newApplyRig(t)
	rig.block, rig.started = make(chan struct{}), make(chan struct{}, 1)
	_, body := rig.apply(t, rig.deployerToken, "?expect="+creatingPlan().Hash(), goodTree(t))
	id := jobID(t, body)
	<-rig.started
	// Not finished yet: a short wait gives up, a long one returns once it ends.
	s := rig.serverUnderTest
	if s.WaitForJobs(50 * time.Millisecond) {
		t.Fatal("an apply is still running")
	}
	go func() { time.Sleep(50 * time.Millisecond); close(rig.block) }()
	if !s.WaitForJobs(5 * time.Second) {
		t.Fatal("must return once the apply finishes")
	}
	rig.waitJob(t, id, JobSucceeded)
}
