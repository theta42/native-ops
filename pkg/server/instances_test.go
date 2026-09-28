package server

import (
	"context"
	"encoding/json"
	"errors"
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

// fakeOps is the host as the instance endpoints see it: which instances exist and from which template
// ("" for a static service), and a record of what was asked of it.
type fakeOps struct {
	mu        sync.Mutex
	existing  map[string]string
	launched  []engine.InstanceSpec
	names     []string
	updated   []engine.UpdateRequest
	resized   []engine.ResizeRequest
	suspended []engine.SuspendRequest
	deleted   []string
	purged    []bool
	err       error
	block     chan struct{}
	started   chan struct{}
}

func (f *fakeOps) Template(_ context.Context, name string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.existing[name]
	return t, ok, nil
}

func (f *fakeOps) wait() {
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
}

func (f *fakeOps) Launch(_ context.Context, name string, spec engine.InstanceSpec, logf func(string, ...any)) (string, error) {
	f.mu.Lock()
	f.launched = append(f.launched, spec)
	f.names = append(f.names, name)
	f.mu.Unlock()
	logf("==> launching %s from %s", name, spec.Image)
	f.wait()
	return "10.0.100.9", f.err
}

func (f *fakeOps) Update(_ context.Context, name string, req engine.UpdateRequest, logf func(string, ...any)) error {
	f.mu.Lock()
	f.updated = append(f.updated, req)
	f.mu.Unlock()
	logf("==> updating %s", name)
	f.wait()
	return f.err
}

func (f *fakeOps) Resize(_ context.Context, name string, req engine.ResizeRequest, logf func(string, ...any)) error {
	f.mu.Lock()
	f.resized = append(f.resized, req)
	f.mu.Unlock()
	logf("==> resizing %s", name)
	f.wait()
	return f.err
}

func (f *fakeOps) Suspend(_ context.Context, name string, req engine.SuspendRequest, logf func(string, ...any)) error {
	f.mu.Lock()
	f.suspended = append(f.suspended, req)
	f.mu.Unlock()
	logf("==> suspending %s", name)
	f.wait()
	return f.err
}

func (f *fakeOps) Destroy(_ context.Context, name string, purge bool, logf func(string, ...any)) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, name)
	f.purged = append(f.purged, purge)
	f.mu.Unlock()
	logf("==> destroying %s", name)
	f.wait()
	return f.err
}

type instRig struct {
	*fixture
	ops                                      *fakeOps
	deployer, scoped, narrow, adminTok, jdir string
	srv                                      *Server
}

var testInstancePolicy = engine.InstancePolicy{Profiles: []string{"base", "service"}, RouteImports: []string{"strip-forged-identity"}}

func newInstRig(t *testing.T) *instRig {
	t.Helper()
	dir := t.TempDir()
	rig := &instRig{ops: &fakeOps{existing: map[string]string{"gitea": "", "demo-old": "platform", "rest-one": "platform", "x-one": "platform"}}, jdir: filepath.Join(dir, "jobs")}
	tokens, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	rig.adminTok, _, _ = tokens.Create("ci-admin", RoleAdmin)
	viewer, _, _ := tokens.Create("dash", RoleViewer)
	planner, _, _ := tokens.Create("pr-ci", RolePlanner)
	rig.deployer, _, _ = tokens.Create("ci-apply", RoleDeployer)
	var err error
	rig.scoped, _, err = tokens.CreateScoped("fleet-manager", RoleDeployer, Scope{Names: []string{"demo-*", "rest-*"}, Images: []string{"opsavor-platform:*"}, Domains: []string{"*.opsavor.app"}})
	if err != nil {
		t.Fatal(err)
	}
	rig.narrow, _, _ = tokens.CreateScoped("narrow", RoleDeployer, Scope{Names: []string{"x-*"}, Images: []string{"opsavor-platform:*"}})
	audit, _ := OpenAudit(filepath.Join(dir, "audit.log"))
	t.Cleanup(func() { audit.Close() })
	jobs, err := OpenJobs(rig.jdir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Tokens: tokens, Audit: audit, Version: "test", Jobs: jobs, Instances: rig.ops, InstancePolicy: testInstancePolicy,
		Status: func(context.Context) (*status.Snapshot, error) {
			return &status.Snapshot{Instances: []status.Instance{
				{Name: "gitea", Status: "Running"},
				{Name: "demo-old", Status: "Running", IPv4: []string{"10.0.100.7"}, Recorded: map[string]string{"template": "platform", "image": "opsavor-platform:latest"}},
				{Name: "rest-one", Status: "Stopped", Recorded: map[string]string{"template": "platform"}},
				{Name: "x-one", Status: "Running", Recorded: map[string]string{"template": "platform"}},
			}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	rig.srv = s
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	rig.fixture = &fixture{srv: ts, secret: rig.adminTok, viewer: viewer, planner: planner, audit: filepath.Join(dir, "audit.log")}
	t.Cleanup(func() { s.WaitForJobs(5 * time.Second) })
	return rig
}

func (r *instRig) do2(t *testing.T, method, path, token string, body any) (*http.Response, string) {
	t.Helper()
	var b []byte
	switch v := body.(type) {
	case nil:
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		b, _ = json.Marshal(v)
	}
	req, _ := http.NewRequest(method, r.srv2URL()+path, strings.NewReader(string(b)))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return res, sb.String()
}

func (r *instRig) srv2URL() string { return r.fixture.srv.URL }

func (r *instRig) wait(t *testing.T, body string, want JobStatus) Job {
	t.Helper()
	var out struct{ Job Job }
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Job.ID == "" {
		t.Fatalf("no job in %s", body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := r.srv.opts.Jobs.Get(out.Job.ID); ok && j.Status == want {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job never became %s", want)
	return Job{}
}

// tenantSpecFor is the platform spec for the instance called name.
func tenantSpecFor(name string) map[string]any {
	s := tenantSpec()
	s["volumes"] = []map[string]string{{"name": name + "-data", "path": "/app/.data"}}
	s["domain"] = name + ".opsavor.app"
	return s
}

func tenantSpec() map[string]any {
	return map[string]any{
		"template": "platform", "image": "opsavor-platform:latest", "profiles": []string{"base", "service"},
		"limits":  map[string]string{"limits.cpu": "1", "limits.memory": "1GB"},
		"volumes": []map[string]string{{"name": "demo-new-data", "path": "/app/.data"}},
		"service": "platform",
		"env":     map[string]string{"OPSAVOR_SEED": "multi", "OPSAVOR_CONTROL_TOKEN": "SENTINEL-TENANT-SECRET-91af"},
		"health":  map[string]any{"path": "/health", "port": 8787},
		"domain":  "demo-new.opsavor.app", "route_directives": []string{"import strip-forged-identity"},
	}
}

func TestAScopedTokenCanOnlyManageItsOwnInstances(t *testing.T) {
	rig := newInstRig(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/status"}, {"POST", "/v1/plan"}, {"POST", "/v1/apply?expect=" + strings.Repeat("a", 64)},
	} {
		res, body := rig.do2(t, c.method, c.path, rig.scoped, nil)
		if res.StatusCode != 403 || !strings.Contains(body, "limited to managing its own instances") {
			t.Errorf("%s %s with a scoped token: %d %s", c.method, c.path, res.StatusCode, body)
		}
	}
	res, body := rig.do2(t, "GET", "/v1/whoami", rig.scoped, nil)
	if res.StatusCode != 200 || !strings.Contains(body, `"role":"deployer"`) || !strings.Contains(body, "demo-*") || !strings.Contains(body, "opsavor-platform:*") {
		t.Fatalf("whoami shows what the token may do: %d %s", res.StatusCode, body)
	}
	if res, _ := rig.do2(t, "GET", "/v1/instances", rig.scoped, nil); res.StatusCode != 200 {
		t.Fatalf("it may list: %d", res.StatusCode)
	}
}

func TestPutCreatesATenantAsAJobWithinTheScope(t *testing.T) {
	rig := newInstRig(t)
	res, body := rig.do2(t, "PUT", "/v1/instances/demo-new", rig.scoped, tenantSpec())
	if res.StatusCode != 202 || !strings.HasPrefix(res.Header.Get("Location"), "/v1/jobs/j-") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	j := rig.wait(t, body, JobSucceeded)
	if j.Kind != "instance:put" || j.Service != "demo-new" || j.Actor != "fleet-manager" || !strings.Contains(j.Log, "launching demo-new from opsavor-platform:latest") {
		t.Fatalf("job: %+v", j)
	}
	if len(rig.ops.launched) != 1 || rig.ops.names[0] != "demo-new" || rig.ops.launched[0].Domain != "demo-new.opsavor.app" || rig.ops.launched[0].Env["OPSAVOR_SEED"] != "multi" {
		t.Fatalf("launched: %+v", rig.ops.launched)
	}
	if strings.Contains(body, "SENTINEL") {
		t.Fatalf("the answer must not echo the environment: %s", body)
	}
}

func TestTheScopeDecidesWhichNamesImagesAndDomainsAreAllowed(t *testing.T) {
	rig := newInstRig(t)
	cases := map[string]func(m map[string]any) (string, string){
		"a name outside the scope": func(m map[string]any) (string, string) {
			m["volumes"] = []map[string]string{{"name": "prod-data", "path": "/d"}}
			return "prod", ""
		},
		"an image outside the scope": func(m map[string]any) (string, string) {
			m["image"] = "docker:evil/miner:latest"
			return "demo-new", ""
		},
		"a domain outside the scope": func(m map[string]any) (string, string) { m["domain"] = "git.opsavor.work"; return "demo-new", "" },
	}
	for name, mutate := range cases {
		spec := tenantSpec()
		inst, _ := mutate(spec)
		res, body := rig.do2(t, "PUT", "/v1/instances/"+inst, rig.scoped, spec)
		if res.StatusCode != 403 || !strings.Contains(body, "out_of_scope") {
			t.Errorf("%s: %d %s", name, res.StatusCode, body)
		}
	}
	// A token with no domain patterns publishes no route at all.
	spec := tenantSpec()
	if res, body := rig.do2(t, "PUT", "/v1/instances/x-two", rig.narrow, spec); res.StatusCode != 403 || !strings.Contains(body, "domain") {
		t.Errorf("no domain patterns means no route: %d %s", res.StatusCode, body)
	}
	if len(rig.ops.launched) != 0 {
		t.Fatalf("nothing outside the scope may run: %+v", rig.ops.launched)
	}
}

func TestOnlyATemplateInstanceCanBeChangedOrRemoved(t *testing.T) {
	rig := newInstRig(t)
	// gitea is a static service: no token can manage it here, not even an unscoped deployer or an admin.
	spec := tenantSpec()
	spec["volumes"] = []map[string]string{{"name": "gitea-data", "path": "/d"}}
	delete(spec, "domain")
	delete(spec, "route_directives")
	for name, tok := range map[string]string{"deployer": rig.deployer, "admin": rig.adminTok} {
		res, body := rig.do2(t, "PUT", "/v1/instances/gitea", tok, spec)
		if res.StatusCode != 403 || !strings.Contains(body, "not_managed") {
			t.Errorf("put over gitea as %s: %d %s", name, res.StatusCode, body)
		}
		if res, body := rig.do2(t, "DELETE", "/v1/instances/gitea", tok, nil); res.StatusCode != 403 || !strings.Contains(body, "not_managed") {
			t.Errorf("delete gitea as %s: %d %s", name, res.StatusCode, body)
		}
		if res, body := rig.do2(t, "POST", "/v1/instances/gitea/update", tok, map[string]any{"image": "img", "service": "gitea"}); res.StatusCode != 403 || !strings.Contains(body, "not_managed") {
			t.Errorf("update gitea as %s: %d %s", name, res.StatusCode, body)
		}
	}
	// A tenant of another template is not taken over.
	other := tenantSpec()
	other["template"] = "restaurant"
	other["volumes"] = []map[string]string{{"name": "demo-old-data", "path": "/d"}}
	other["domain"] = "demo-old.opsavor.app"
	if res, body := rig.do2(t, "PUT", "/v1/instances/demo-old", rig.scoped, other); res.StatusCode != 409 || !strings.Contains(body, "not_managed") {
		t.Errorf("another template: %d %s", res.StatusCode, body)
	}
	// Something that does not exist cannot be updated or deleted.
	if res, _ := rig.do2(t, "DELETE", "/v1/instances/demo-nope", rig.scoped, nil); res.StatusCode != 404 {
		t.Errorf("delete a missing instance: %d", res.StatusCode)
	}
	if len(rig.ops.launched)+len(rig.ops.deleted)+len(rig.ops.updated) != 0 {
		t.Fatal("none of these may reach the host")
	}
}

func TestAnInvalidSpecNeverReachesTheHost(t *testing.T) {
	rig := newInstRig(t)
	cases := map[string]any{
		"privileged smuggled in as a field": func() map[string]any { s := tenantSpec(); s["privileged"] = true; return s }(),
		"a disallowed profile":              func() map[string]any { s := tenantSpec(); s["profiles"] = []string{"base", "privileged"}; return s }(),
		"an env value with a line break":    func() map[string]any { s := tenantSpec(); s["env"] = map[string]string{"A": "x\nEVIL=1"}; return s }(),
		"a route directive that is not an import": func() map[string]any {
			s := tenantSpec()
			s["route_directives"] = []string{"reverse_proxy evil:80"}
			return s
		}(),
		"a volume of someone else's": func() map[string]any {
			s := tenantSpec()
			s["volumes"] = []map[string]string{{"name": "gitea-data", "path": "/d"}}
			return s
		}(),
		"not json":      "not json at all",
		"two documents": `{"template":"platform"}{"template":"x"}`,
		"a huge body":   strings.Repeat("x", maxInstanceBody+10),
		"an empty spec": map[string]any{},
	}
	for name, body := range cases {
		res, out := rig.do2(t, "PUT", "/v1/instances/demo-new", rig.scoped, body)
		if res.StatusCode != 400 {
			t.Errorf("%s: %d %s", name, res.StatusCode, out)
		}
	}
	if res, _ := rig.do2(t, "PUT", "/v1/instances/Bad_Name!", rig.scoped, tenantSpec()); res.StatusCode != 400 {
		t.Errorf("a bad instance name: %d", res.StatusCode)
	}
	if len(rig.ops.launched) != 0 || len(rig.srv.opts.Jobs.List()) != 0 {
		t.Fatal("no job and no launch for an invalid request")
	}
}

func TestRolesForTheInstanceEndpoints(t *testing.T) {
	rig := newInstRig(t)
	for name, tok := range map[string]string{"viewer": rig.viewer, "planner": rig.planner} {
		if res, _ := rig.do2(t, "PUT", "/v1/instances/demo-new", tok, tenantSpec()); res.StatusCode != 403 {
			t.Errorf("a %s must not create: %d", name, res.StatusCode)
		}
		if res, _ := rig.do2(t, "DELETE", "/v1/instances/demo-old", tok, nil); res.StatusCode != 403 {
			t.Errorf("a %s must not delete: %d", name, res.StatusCode)
		}
	}
	if res, _ := rig.do2(t, "GET", "/v1/instances", rig.viewer, nil); res.StatusCode != 200 {
		t.Errorf("a viewer may list: %d", res.StatusCode)
	}
	if res, _ := rig.do2(t, "PUT", "/v1/instances/demo-new", "", tenantSpec()); res.StatusCode != 401 {
		t.Errorf("no token: %d", res.StatusCode)
	}
	if len(rig.ops.launched) != 0 {
		t.Fatal("nothing reaches the host")
	}
}

func TestListingShowsOnlyTenantInstancesTheCallerMaySee(t *testing.T) {
	rig := newInstRig(t)
	names := func(tok string) []string {
		_, body := rig.do2(t, "GET", "/v1/instances", tok, nil)
		var out struct{ Instances []tenantView }
		json.Unmarshal([]byte(body), &out)
		var n []string
		for _, i := range out.Instances {
			n = append(n, i.Name)
		}
		return n
	}
	if got := strings.Join(names(rig.scoped), ","); got != "demo-old,rest-one" {
		t.Fatalf("the scoped token sees its names only: %s", got)
	}
	if got := strings.Join(names(rig.narrow), ","); got != "x-one" {
		t.Fatalf("the narrow token: %s", got)
	}
	if got := strings.Join(names(rig.viewer), ","); got != "demo-old,rest-one,x-one" || strings.Contains(got, "gitea") {
		t.Fatalf("an unscoped token sees every tenant and never a static service: %s", got)
	}
	res, body := rig.do2(t, "GET", "/v1/instances/demo-old", rig.scoped, nil)
	if res.StatusCode != 200 || !strings.Contains(body, `"template":"platform"`) || !strings.Contains(body, "10.0.100.7") {
		t.Fatalf("get: %d %s", res.StatusCode, body)
	}
	for _, name := range []string{"x-one", "gitea", "nope"} {
		if res, _ := rig.do2(t, "GET", "/v1/instances/"+name, rig.scoped, nil); res.StatusCode != 404 {
			t.Errorf("%s: outside the scope, static, or missing all look the same: %d", name, res.StatusCode)
		}
	}
}

func TestUpdateAndDeleteAreJobsToo(t *testing.T) {
	rig := newInstRig(t)
	res, body := rig.do2(t, "POST", "/v1/instances/demo-old/update", rig.scoped, map[string]any{"image": "opsavor-platform:v2", "service": "platform", "health": map[string]any{"path": "/health", "port": 8787}})
	if res.StatusCode != 202 {
		t.Fatalf("update: %d %s", res.StatusCode, body)
	}
	j := rig.wait(t, body, JobSucceeded)
	if j.Kind != "instance:update" || len(rig.ops.updated) != 1 || rig.ops.updated[0].Image != "opsavor-platform:v2" {
		t.Fatalf("update job: %+v %+v", j, rig.ops.updated)
	}
	if res, _ := rig.do2(t, "POST", "/v1/instances/demo-old/update", rig.scoped, map[string]any{"image": "docker:evil/miner", "service": "platform"}); res.StatusCode != 403 {
		t.Errorf("an update to an image outside the scope: %d", res.StatusCode)
	}
	res, body = rig.do2(t, "DELETE", "/v1/instances/demo-old?purge_volumes=true", rig.scoped, nil)
	if res.StatusCode != 202 {
		t.Fatalf("delete: %d %s", res.StatusCode, body)
	}
	rig.wait(t, body, JobSucceeded)
	if len(rig.ops.deleted) != 1 || !rig.ops.purged[0] {
		t.Fatalf("delete with purge: %+v %+v", rig.ops.deleted, rig.ops.purged)
	}
	res, body = rig.do2(t, "DELETE", "/v1/instances/rest-one", rig.scoped, nil)
	rig.wait(t, body, JobSucceeded)
	if rig.ops.purged[1] {
		t.Fatal("data is kept unless asked for")
	}
}

func TestResizeIsAJobWithValidatedLimits(t *testing.T) {
	rig := newInstRig(t)
	res, body := rig.do2(t, "POST", "/v1/instances/demo-old/resize", rig.scoped, map[string]any{"limits": map[string]string{"limits.cpu": "2", "limits.memory": "2GB"}})
	if res.StatusCode != 202 {
		t.Fatalf("resize: %d %s", res.StatusCode, body)
	}
	j := rig.wait(t, body, JobSucceeded)
	if j.Kind != "instance:resize" || len(rig.ops.resized) != 1 || rig.ops.resized[0].Limits["limits.cpu"] != "2" {
		t.Fatalf("resize job: %+v %+v", j, rig.ops.resized)
	}
	for name, limits := range map[string]map[string]string{
		"an unknown limit":   {"security.privileged": "true"},
		"a cpu not a number": {"limits.cpu": "all"},
		"empty":              {},
	} {
		if res, _ := rig.do2(t, "POST", "/v1/instances/demo-old/resize", rig.scoped, map[string]any{"limits": limits}); res.StatusCode != 400 {
			t.Errorf("%s must be rejected, got %d", name, res.StatusCode)
		}
	}
	if len(rig.ops.resized) != 1 {
		t.Fatalf("rejected requests must never reach the ops layer: %+v", rig.ops.resized)
	}
	if res, _ := rig.do2(t, "POST", "/v1/instances/x-one/resize", rig.narrow, map[string]any{"limits": map[string]string{"limits.cpu": "1"}}); res.StatusCode != 202 {
		t.Errorf("a name within a narrow scope must be allowed to resize, got %d", res.StatusCode)
	}
	if res, _ := rig.do2(t, "POST", "/v1/instances/demo-old/resize", rig.narrow, map[string]any{"limits": map[string]string{"limits.cpu": "1"}}); res.StatusCode != 403 {
		t.Errorf("a name outside a narrow scope must be refused, got %d", res.StatusCode)
	}
}

func TestSuspendIsAJobAndOnlyAllowedDomainsAndImportsAreAccepted(t *testing.T) {
	rig := newInstRig(t)
	res, body := rig.do2(t, "POST", "/v1/instances/demo-old/suspend", rig.scoped, map[string]any{"domain": "demo-old.opsavor.app", "reason": "non-payment", "route_directives": []string{"import strip-forged-identity"}})
	if res.StatusCode != 202 {
		t.Fatalf("suspend: %d %s", res.StatusCode, body)
	}
	j := rig.wait(t, body, JobSucceeded)
	if j.Kind != "instance:suspend" || len(rig.ops.suspended) != 1 || rig.ops.suspended[0].Reason != "non-payment" {
		t.Fatalf("suspend job: %+v %+v", j, rig.ops.suspended)
	}
	if res, _ := rig.do2(t, "POST", "/v1/instances/demo-old/suspend", rig.scoped, map[string]any{"domain": "evil.example.com", "reason": "x"}); res.StatusCode != 403 {
		t.Errorf("a domain outside the token's scope must be refused, got %d", res.StatusCode)
	}
	if res, _ := rig.do2(t, "POST", "/v1/instances/demo-old/suspend", rig.scoped, map[string]any{"domain": "demo-old.opsavor.app", "reason": ""}); res.StatusCode != 400 {
		t.Errorf("an empty reason must be refused, got %d", res.StatusCode)
	}
	if res, _ := rig.do2(t, "POST", "/v1/instances/demo-old/suspend", rig.scoped, map[string]any{"domain": "demo-old.opsavor.app", "reason": "x", "route_directives": []string{"import admin-only"}}); res.StatusCode != 400 {
		t.Errorf("an import the operator did not allow must be refused, got %d", res.StatusCode)
	}
	if len(rig.ops.suspended) != 1 {
		t.Fatalf("rejected requests must never reach the ops layer: %+v", rig.ops.suspended)
	}
}

func TestInstanceChangesShareTheHostWithApplyAndAFinishedJobMeansFree(t *testing.T) {
	rig := newInstRig(t)
	rig.ops.block, rig.ops.started = make(chan struct{}), make(chan struct{}, 4)
	res, body := rig.do2(t, "PUT", "/v1/instances/demo-a", rig.scoped, tenantSpecFor("demo-a"))
	if res.StatusCode != 202 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	<-rig.ops.started

	// One change at a time on the host, whoever asks.
	res, b := rig.do2(t, "PUT", "/v1/instances/demo-b", rig.scoped, tenantSpecFor("demo-b"))
	if res.StatusCode != 409 || !strings.Contains(b, `"code":"busy"`) {
		t.Fatalf("a second change while one runs: %d %s", res.StatusCode, b)
	}
	if res, _ := rig.do2(t, "DELETE", "/v1/instances/demo-old", rig.deployer, nil); res.StatusCode != 409 {
		t.Fatalf("a delete while one runs: %d", res.StatusCode)
	}
	close(rig.ops.block)
	rig.wait(t, body, JobSucceeded)
	rig.ops.block = nil
	// The moment the job reports succeeded, the next change is accepted.
	if res, b := rig.do2(t, "PUT", "/v1/instances/demo-c", rig.scoped, tenantSpecFor("demo-c")); res.StatusCode != 202 {
		t.Fatalf("the host is free once the job is finished: %d %s", res.StatusCode, b)
	}
}

func TestAFailedInstanceJobIsRecordedAndNeverLeaksTheEnvironment(t *testing.T) {
	rig := newInstRig(t)
	rig.ops.err = errors.New("launch container demo-new: boom")
	res, body := rig.do2(t, "PUT", "/v1/instances/demo-new", rig.scoped, tenantSpec())
	j := rig.wait(t, body, JobFailed)
	if res.StatusCode != 202 || !strings.Contains(j.Error, "boom") || !strings.Contains(j.Log, "FAILED") {
		t.Fatalf("%d %+v", res.StatusCode, j)
	}
	files, _ := filepath.Glob(filepath.Join(rig.jdir, "*.json"))
	audit, _ := os.ReadFile(rig.audit)
	all := string(audit)
	for _, f := range files {
		b, _ := os.ReadFile(f)
		all += string(b)
	}
	if strings.Contains(all, "SENTINEL-TENANT-SECRET") {
		t.Fatalf("a tenant's secret must not reach the job record or the audit log:\n%s", all)
	}
	if !strings.Contains(string(audit), `"detail":"instance put demo-new job=`) || !strings.Contains(string(audit), `"actor":"fleet-manager"`) || strings.Contains(string(audit), rig.scoped) {
		t.Fatalf("the change is audited with who and what:\n%s", audit)
	}
}

func TestScopesAreValidatedAndPersisted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	store, _ := OpenTokenStore(path)
	good := Scope{Names: []string{"demo-*"}, Images: []string{"opsavor-platform:*"}, Domains: []string{"*.opsavor.app"}}
	secret, tok, err := store.CreateScoped("fm", RoleDeployer, good)
	if err != nil || !tok.Scoped() {
		t.Fatalf("%v %+v", err, tok)
	}
	for name, sc := range map[string]Scope{
		"no images":                  {Names: []string{"demo-*"}},
		"no names":                   {Images: []string{"x"}},
		"a pattern with a space":     {Names: []string{"a b"}, Images: []string{"x"}},
		"a bad pattern":              {Names: []string{"[a"}, Images: []string{"x"}},
		"a pattern with a semicolon": {Names: []string{"a;b"}, Images: []string{"x"}},
	} {
		if _, _, err := store.CreateScoped("bad", RoleDeployer, sc); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	for _, role := range []Role{RoleViewer, RolePlanner, RoleAdmin} {
		if _, _, err := store.CreateScoped("bad", role, good); err == nil {
			t.Errorf("a scoped %s must be refused: the scope is what limits a deployer", role)
		}
	}
	reopened, _ := OpenTokenStore(path)
	got, ok := reopened.Verify(secret)
	if !ok || !got.Scoped() || got.Scope.Names[0] != "demo-*" || !got.Scope.AllowsImage("opsavor-platform:v3") || got.Scope.AllowsImage("docker:x") || got.Scope.AllowsName("gitea") || !got.Scope.AllowsDomain("a.opsavor.app") || got.Scope.AllowsDomain("git.opsavor.work") {
		t.Fatalf("the scope survives a restart and matches as globs: %+v", got)
	}
	if _, plain, _ := store.Create("plain", RoleDeployer); plain.Scoped() {
		t.Fatal("an ordinary token has no scope")
	}
}

func TestInstancesNeedAJobStore(t *testing.T) {
	dir := t.TempDir()
	tokens, _ := OpenTokenStore(filepath.Join(dir, "t.json"))
	st := func(context.Context) (*status.Snapshot, error) { return &status.Snapshot{}, nil }
	if _, err := New(Options{Tokens: tokens, Status: st, Instances: &fakeOps{}}); err == nil {
		t.Error("instances without a job store must not start")
	}
	jobs, _ := OpenJobs(filepath.Join(dir, "jobs"))
	if _, err := New(Options{Tokens: tokens, Status: st, Jobs: jobs}); err == nil {
		t.Error("a job store that nothing uses is a misconfiguration")
	}
}
