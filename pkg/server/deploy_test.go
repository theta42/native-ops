package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// A deploy applies its commit's fleet.yml dns_records, so a record change lands with the deploy tag
// (docs/daemon.md, "Service secrets" and "DNS as part of a deploy").
func TestADeploySyncsTheCommitsDNSRecords(t *testing.T) {
	tree := tgz(t, entry{name: "fleet.yml", body: "name: x\ndns_records:\n  - zone: example.com\n    type: A\n    name: inbound\n    value: 1.2.3.4\n"})
	src := &fakeSource{protected: true, tree: tree}
	var roots []string
	rig := newApplyRigWith(t, func(o *Options) {
		o.Deploy, o.DeployTags = src, "deploy-*"
		o.DNSSync = func(_ context.Context, root string, logf func(string, ...any)) error {
			b, err := os.ReadFile(filepath.Join(root, "fleet.yml"))
			if err != nil {
				return err
			}
			roots = append(roots, string(b))
			logf("synced dns_records")
			return nil
		}
	})
	_, body := rig.post(t, "/v1/deploy", rig.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	j := rig.waitJob(t, jobID(t, body), JobSucceeded)
	if len(roots) != 1 {
		t.Fatalf("the deploy must sync the commit's dns_records once, got %d: %+v", len(roots), j)
	}
	if !strings.Contains(roots[0], "dns_records") {
		t.Fatalf("dns sync got the deployed commit's root, got %q", roots[0])
	}
	if !strings.Contains(j.Log, "dns_records") {
		t.Fatalf("the deploy log must name the dns sync: %s", j.Log)
	}
}

// A record the daemon may not write (a zone outside NATIVE_OPS_DNS_ZONES) fails the deploy, and it
// fails before the services are applied: the record change is part of the deploy, not a side effect.
func TestADeployFailsWhenDNSCannotBeApplied(t *testing.T) {
	tree := tgz(t, entry{name: "fleet.yml", body: "name: x\ndns_records:\n  - zone: example.com\n    type: A\n    name: inbound\n    value: 1.2.3.4\n"})
	src := &fakeSource{protected: true, tree: tree}
	rig := newApplyRigWith(t, func(o *Options) {
		o.Deploy, o.DeployTags = src, "deploy-*"
		o.DNSSync = func(context.Context, string, func(string, ...any)) error {
			return errors.New("dns_records name the zone example.com, which this daemon may not change")
		}
	})
	_, body := rig.post(t, "/v1/deploy", rig.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	j := rig.waitJob(t, jobID(t, body), JobFailed)
	if !strings.Contains(j.Error, "dns sync") || rig.applies.Load() != 0 {
		t.Fatalf("a DNS failure must fail the deploy before the services are applied: %+v", j)
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

const pinSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// pinRig is a deploy rig whose daemon runs version and can upgrade itself; upgrades and restarts are
// recorded, not done.
type pinRig struct {
	*applyRig
	src      *fakeSource
	resume   string
	upgrades []string
	restarts chan struct{}
}

func newPinRig(t *testing.T, version, pinned string, canUpgrade bool) *pinRig {
	fleet := "name: f\n"
	if pinned != "" {
		fleet += "daemon:\n  version: " + pinned + "\n  sha256: " + pinSHA + "\n"
	}
	p := &pinRig{
		src:      &fakeSource{protected: true, tree: tgz(t, entry{name: "conf/fleet.yml", body: fleet}, entry{name: "conf/services/web/service.yml", body: "image: x\n"})},
		resume:   filepath.Join(t.TempDir(), "deploy-resume.json"),
		restarts: make(chan struct{}, 4),
	}
	p.applyRig = newApplyRigWith(t, func(o *Options) {
		o.Deploy, o.DeployTags, o.Version = p.src, "deploy-*", version
		if canUpgrade {
			o.ResumeFile = p.resume
			o.Upgrade = func(_ context.Context, v, sha string, _ func(string, ...any)) error {
				p.upgrades = append(p.upgrades, v+" "+sha[:4])
				return nil
			}
			o.Restart = func() { p.restarts <- struct{}{} }
		}
	})
	return p
}

func (p *pinRig) deploy(t *testing.T, want JobStatus) Job {
	t.Helper()
	res, body := p.post(t, "/v1/deploy", p.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	return p.waitJob(t, jobID(t, body), want)
}

func (p *pinRig) appliedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.applied)
}

func TestADeployMovesTheDaemonToFleetYmlsPinFirst(t *testing.T) {
	p := newPinRig(t, "v1.58.0", "v1.59.0", true)
	j := p.deploy(t, JobSucceeded)
	select {
	case <-p.restarts:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon must restart onto the pinned version")
	}
	if len(p.upgrades) != 1 || p.upgrades[0] != "v1.59.0 aaaa" {
		t.Fatalf("upgrades: %v", p.upgrades)
	}
	if p.appliedCount() != 0 {
		t.Fatal("nothing may be applied by the old binary: the deploy resumes on the new one")
	}
	if !strings.Contains(j.Log, "pins the daemon at v1.59.0") {
		t.Fatalf("log: %s", j.Log)
	}
	b, err := os.ReadFile(p.resume)
	if err != nil || !strings.Contains(string(b), `"tag":"deploy-1"`) || !strings.Contains(string(b), `"version":"v1.59.0"`) {
		t.Fatalf("resume record: %s %v", b, err)
	}
}

func TestTheUpgradedDaemonResumesTheDeploy(t *testing.T) {
	p := newPinRig(t, "v1.59.0", "v1.59.0", true)
	if err := os.WriteFile(p.resume, []byte(`{"tag":"deploy-1","version":"v1.59.0","actor":"ci-deploy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p.serverUnderTest.ResumeDeploy()
	j := p.waitLatest(t, JobSucceeded)
	if p.appliedCount() != 1 || len(p.upgrades) != 0 || !strings.Contains(j.Log, "resumed on the upgraded daemon") {
		t.Fatalf("applied %d, upgrades %v, log: %s", p.appliedCount(), p.upgrades, j.Log)
	}
	if _, err := os.Stat(p.resume); !os.IsNotExist(err) {
		t.Fatal("the resume record is used once")
	}
}

func TestARolledBackUpgradeRecordsTheDeployAsFailed(t *testing.T) {
	p := newPinRig(t, "v1.58.0", "v1.59.0", true)
	if err := os.WriteFile(p.resume, []byte(`{"tag":"deploy-1","version":"v1.59.0","actor":"ci-deploy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p.serverUnderTest.ResumeDeploy()
	j := p.waitLatest(t, JobFailed)
	if p.appliedCount() != 0 || len(p.upgrades) != 0 || !strings.Contains(j.Error, "rolled back") {
		t.Fatalf("the old binary must not deploy or upgrade again: applied %d, upgrades %v, %+v", p.appliedCount(), p.upgrades, j)
	}
	if _, err := os.Stat(p.resume); !os.IsNotExist(err) {
		t.Fatal("the resume record is used once")
	}
}

func TestADeployOnThePinnedVersionJustDeploys(t *testing.T) {
	p := newPinRig(t, "v1.59.0", "v1.59.0", true)
	p.deploy(t, JobSucceeded)
	if p.appliedCount() != 1 || len(p.upgrades) != 0 {
		t.Fatalf("applied %d, upgrades %v", p.appliedCount(), p.upgrades)
	}
}

func TestAPinTheDaemonCannotFollowDeploysNothing(t *testing.T) {
	p := newPinRig(t, "v1.58.0", "v1.59.0", false)
	j := p.deploy(t, JobFailed)
	if p.appliedCount() != 0 || !strings.Contains(j.Error, "cannot upgrade itself") {
		t.Fatalf("applied %d, %+v", p.appliedCount(), j)
	}
}

func TestADevBuildIgnoresThePin(t *testing.T) {
	p := newPinRig(t, "dev", "v1.59.0", true)
	p.deploy(t, JobSucceeded)
	if p.appliedCount() != 1 || len(p.upgrades) != 0 {
		t.Fatalf("applied %d, upgrades %v", p.appliedCount(), p.upgrades)
	}
}

func TestABadPinDeploysNothing(t *testing.T) {
	p := newPinRig(t, "v1.58.0", "latest", true)
	j := p.deploy(t, JobFailed)
	if p.appliedCount() != 0 || len(p.upgrades) != 0 || !strings.Contains(j.Error, "daemon pin") {
		t.Fatalf("applied %d, upgrades %v, %+v", p.appliedCount(), p.upgrades, j)
	}
}

// waitLatest waits for the newest job to reach want (a resumed deploy is started by the daemon, not a
// request, so there is no answer naming it).
func (p *pinRig) waitLatest(t *testing.T, want JobStatus) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if jobs := p.serverUnderTest.opts.Jobs.List(); len(jobs) > 0 {
			return p.waitJob(t, string(jobs[0].ID), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no job was started")
	return Job{}
}

func TestAPinOlderThanResumingDeploysNothing(t *testing.T) {
	p := newPinRig(t, "v1.58.0", "v1.57.1", true)
	j := p.deploy(t, JobFailed)
	if p.appliedCount() != 0 || len(p.upgrades) != 0 || !strings.Contains(j.Error, "older than v1.58.0") {
		t.Fatalf("applied %d, upgrades %v, %+v", p.appliedCount(), p.upgrades, j)
	}
}

func TestReleaseBefore(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"v1.57.1", "v1.58.0", true}, {"v1.58.0", "v1.58.0", false}, {"v1.100.0", "v1.58.0", false}, {"v2.0.0", "v1.99.9", false}, {"dev", "v1.58.0", false}} {
		if got := releaseBefore(c.a, c.b); got != c.want {
			t.Errorf("releaseBefore(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// A deploy tree: the configuration plus the scripts an image build runs.
func deployRecipeTree(t *testing.T, script string) []byte {
	t.Helper()
	return tgz(t,
		entry{name: "conf/fleet.yml", body: "name: from-the-tag\n"},
		entry{name: "conf/services/web/service.yml", body: "image: x\n"},
		entry{name: "conf/scripts/build-image.sh", body: "#!/bin/sh\n" + script + "\n"},
		entry{name: "conf/images/app/build.sh", body: "echo build\n"})
}

// recipeDigestOf is the digest the daemon should compute for a tree: the same extraction and the same
// engine.RecipeDigest a build upload would be hashed with.
func recipeDigestOf(t *testing.T, tree []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := ExtractTarGz(bytes.NewReader(tree), dir); err != nil {
		t.Fatal(err)
	}
	d, err := engine.RecipeDigest(filepath.Join(dir, "conf"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newRecipeDeployRig(t *testing.T, src *fakeSource) (*applyRig, *RecipeStore) {
	t.Helper()
	store, err := OpenRecipeStore(filepath.Join(t.TempDir(), "recipes.json"))
	if err != nil {
		t.Fatal(err)
	}
	return newApplyRigWith(t, func(o *Options) { o.Deploy, o.DeployTags, o.Recipes = src, "deploy-*", store }), store
}

func TestADeployTagApprovesTheRecipeInItsCommit(t *testing.T) {
	tree := deployRecipeTree(t, "echo v1")
	src := &fakeSource{protected: true, tree: tree}
	rig, store := newRecipeDeployRig(t, src)
	want := recipeDigestOf(t, tree)

	if ok, _ := store.Seen(want, "ci", "app", ""); ok {
		t.Fatal("the recipe must not be approved before the tag is deployed")
	}
	res, body := rig.post(t, "/v1/deploy", rig.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	j := rig.waitJob(t, jobID(t, body), JobSucceeded)
	if !strings.Contains(j.Log, "image recipe "+want[:12]+" approved by the tag") {
		t.Fatalf("the job log must say the tag approved the recipe:\n%s", j.Log)
	}
	var rec *RecipeRecord
	for _, r := range store.List() {
		if r.Digest == want {
			r := r
			rec = &r
		}
	}
	if rec == nil || rec.ApprovedAt == nil || rec.ApprovedBy != "tag deploy-1 ("+deployCommit[:12]+")" {
		t.Fatalf("recipe record: %+v", rec)
	}
	// A build of that recipe is now accepted without anyone calling the daemon.
	if ok, _ := store.Seen(want, "ci", "app", ""); !ok {
		t.Fatal("a build of the deployed commit's recipe must be approved")
	}
}

func TestADeployTagApprovesOnlyThatCommitsRecipe(t *testing.T) {
	src := &fakeSource{protected: true, tree: deployRecipeTree(t, "echo v1")}
	rig, store := newRecipeDeployRig(t, src)
	other := recipeDigestOf(t, deployRecipeTree(t, "echo something else"))
	res, body := rig.post(t, "/v1/deploy", rig.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	rig.waitJob(t, jobID(t, body), JobSucceeded)
	if ok, _ := store.Seen(other, "ci", "app", ""); ok {
		t.Fatal("a different recipe must still need its own approval")
	}
}

func TestADeployKeepsTheFirstApproverOfARecipe(t *testing.T) {
	tree := deployRecipeTree(t, "echo v1")
	src := &fakeSource{protected: true, tree: tree}
	rig, store := newRecipeDeployRig(t, src)
	want := recipeDigestOf(t, tree)
	if _, err := store.Approve(want, "alice"); err != nil {
		t.Fatal(err)
	}
	res, body := rig.post(t, "/v1/deploy", rig.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	j := rig.waitJob(t, jobID(t, body), JobSucceeded)
	if strings.Contains(j.Log, "approved by the tag") {
		t.Fatalf("an already approved recipe must not be announced again:\n%s", j.Log)
	}
	for _, r := range store.List() {
		if r.Digest == want && r.ApprovedBy != "alice" {
			t.Fatalf("the first approver must be kept: %+v", r)
		}
	}
}

func TestAnUnprotectedTagApprovesNoRecipe(t *testing.T) {
	tree := deployRecipeTree(t, "echo v1")
	src := &fakeSource{protected: false, tree: tree}
	rig, store := newRecipeDeployRig(t, src)
	want := recipeDigestOf(t, tree)
	res, body := rig.post(t, "/v1/deploy", rig.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	rig.waitJob(t, jobID(t, body), JobFailed)
	if ok, _ := store.Seen(want, "ci", "app", ""); ok {
		t.Fatal("a tag no protection rule covers must not approve a recipe")
	}
}
