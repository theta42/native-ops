package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/status"
)

// labelRig is a host with one production tenant, one staging tenant, one tenant with no labels and a
// static service, and tokens scoped to non-production or to production only.
type labelRig struct {
	*instRig
	nonprod, prod string
}

func newLabelRig(t *testing.T) *labelRig {
	t.Helper()
	dir := t.TempDir()
	ops := &fakeOps{existing: map[string]string{"gitea": "", "rest-prod": "platform", "rest-stage": "platform", "rest-plain": "platform"}}
	tokens, _ := OpenTokenStore(filepath.Join(dir, "tokens.json"))
	r := &instRig{ops: ops, jdir: filepath.Join(dir, "jobs")}
	r.adminTok, _, _ = tokens.Create("ci-admin", RoleAdmin)
	viewer, _, _ := tokens.Create("dash", RoleViewer)
	planner, _, _ := tokens.Create("pr-ci", RolePlanner)
	r.deployer, _, _ = tokens.Create("ci-apply", RoleDeployer)
	lr := &labelRig{instRig: r}
	var err error
	lr.nonprod, _, err = tokens.CreateScoped("nonprod", RoleDeployer, Scope{
		Names: []string{"rest-*"}, Images: []string{"opsavor-platform:*"}, Domains: []string{"*.opsavor.app"},
		Labels: map[string][]string{"environment": {"staging", "testing", "development", "demo"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lr.prod, _, err = tokens.CreateScoped("prod", RoleDeployer, Scope{
		Names: []string{"rest-*"}, Images: []string{"opsavor-platform:*"}, Domains: []string{"*.opsavor.app"},
		Labels: map[string][]string{"environment": {"production"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	audit, _ := OpenAudit(filepath.Join(dir, "audit.log"))
	t.Cleanup(func() { audit.Close() })
	jobs, err := OpenJobs(r.jdir)
	if err != nil {
		t.Fatal(err)
	}
	tenant := func(name string, labels map[string]string) status.Instance {
		return status.Instance{Name: name, Status: "Running", Recorded: map[string]string{"template": "platform", "image": "opsavor-platform:latest"}, Labels: labels}
	}
	s, err := New(Options{Tokens: tokens, Audit: audit, Version: "test", Jobs: jobs, Instances: ops, InstancePolicy: testInstancePolicy,
		Status: func(context.Context) (*status.Snapshot, error) {
			return &status.Snapshot{Instances: []status.Instance{
				{Name: "gitea", Status: "Running"},
				tenant("rest-prod", map[string]string{"environment": "production", "app": "platform"}),
				tenant("rest-stage", map[string]string{"environment": "staging", "app": "platform"}),
				tenant("rest-plain", nil),
			}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	r.srv = s
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	r.fixture = &fixture{srv: ts, secret: r.adminTok, viewer: viewer, planner: planner, audit: filepath.Join(dir, "audit.log")}
	t.Cleanup(func() { s.WaitForJobs(5 * time.Second) })
	return lr
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func names(t *testing.T, body string) []string {
	t.Helper()
	var out struct {
		Instances []struct {
			Name   string
			Labels map[string]string
		}
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	var n []string
	for _, i := range out.Instances {
		n = append(n, i.Name)
	}
	return n
}

func TestTheInstanceListShowsLabelsAndFiltersByThem(t *testing.T) {
	r := newLabelRig(t)
	res, body := r.do2(t, "GET", "/v1/instances", r.adminTok, nil)
	if res.StatusCode != 200 || !strings.Contains(body, `"environment":"production"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if got := names(t, body); len(got) != 3 {
		t.Fatalf("all three tenants (not the static service): %v", got)
	}
	_, body = r.do2(t, "GET", "/v1/instances?label=environment=production", r.adminTok, nil)
	if got := names(t, body); len(got) != 1 || got[0] != "rest-prod" {
		t.Fatalf("only production: %v", got)
	}
	_, body = r.do2(t, "GET", "/v1/instances?label=environment=staging&label=app=platform", r.adminTok, nil)
	if got := names(t, body); len(got) != 1 || got[0] != "rest-stage" {
		t.Fatalf("every label given must match: %v", got)
	}
	_, body = r.do2(t, "GET", "/v1/instances?label=environment=demo", r.adminTok, nil)
	if got := names(t, body); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	for _, bad := range []string{"environment", "environment=", "Bad=x"} {
		if res, _ := r.do2(t, "GET", "/v1/instances?label="+bad, r.adminTok, nil); res.StatusCode != 400 {
			t.Errorf("label=%s must be refused, got %d", bad, res.StatusCode)
		}
	}
}

func TestAnInstanceSpecWithAnUnknownEnvironmentIsRefusedAndLabelsReachTheLaunch(t *testing.T) {
	r := newLabelRig(t)
	spec := tenantSpecFor("rest-new")
	spec["labels"] = map[string]string{"environment": "prod"}
	if res, body := r.do2(t, "PUT", "/v1/instances/rest-new", r.deployer, spec); res.StatusCode != 400 || !strings.Contains(body, "environment") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	spec["labels"] = map[string]string{"environment": "staging", "upgrade-window": "us-east-night"}
	res, body := r.do2(t, "PUT", "/v1/instances/rest-new", r.deployer, spec)
	if res.StatusCode != 202 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	r.wait(t, body, JobSucceeded)
	if len(r.ops.launched) != 1 || r.ops.launched[0].Labels["environment"] != "staging" || r.ops.launched[0].Labels["upgrade-window"] != "us-east-night" {
		t.Fatalf("labels must reach the launch: %+v", r.ops.launched)
	}
	// without labels the spec leaves an existing instance's labels alone
	delete(spec, "labels")
	_, body = r.do2(t, "PUT", "/v1/instances/rest-new", r.deployer, spec)
	r.wait(t, body, JobSucceeded)
	if r.ops.launched[1].Labels != nil {
		t.Fatalf("no labels in the request must stay nil, not empty: %+v", r.ops.launched[1].Labels)
	}
}

func TestPatchingLabelsChangesThemWithoutARestart(t *testing.T) {
	r := newLabelRig(t)
	res, body := r.do2(t, "PATCH", "/v1/instances/rest-stage/labels", r.deployer, map[string]any{"set": map[string]string{"upgrade-window": "us-west-night"}, "remove": []string{"app"}})
	if res.StatusCode != 202 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	r.wait(t, body, JobSucceeded)
	if len(r.ops.labeled) != 1 || r.ops.labeled[0].Name != "rest-stage" || r.ops.labeled[0].Set["upgrade-window"] != "us-west-night" || r.ops.labeled[0].Remove[0] != "app" {
		t.Fatalf("%+v", r.ops.labeled)
	}
	if len(r.ops.updated)+len(r.ops.launched)+len(r.ops.deleted) != 0 {
		t.Fatal("a label change must not touch the instance itself")
	}
	if !strings.Contains(readFileT(t, r.audit), "instance labels rest-stage") {
		t.Fatal("a label change must be audited")
	}
	for name, c := range map[string]struct {
		path string
		body any
		code int
	}{
		"nothing to change":    {"/v1/instances/rest-stage/labels", map[string]any{}, 400},
		"an unknown env":       {"/v1/instances/rest-stage/labels", map[string]any{"set": map[string]string{"environment": "prod"}}, 400},
		"a bad name to remove": {"/v1/instances/rest-stage/labels", map[string]any{"remove": []string{"Bad Key"}}, 400},
		"a missing instance":   {"/v1/instances/rest-none/labels", map[string]any{"set": map[string]string{"app": "x"}}, 404},
		"a static service":     {"/v1/instances/gitea/labels", map[string]any{"set": map[string]string{"app": "x"}}, 403},
	} {
		if res, body := r.do2(t, "PATCH", c.path, r.deployer, c.body); res.StatusCode != c.code {
			t.Errorf("%s: want %d, got %d %s", name, c.code, res.StatusCode, body)
		}
	}
	if res, _ := r.do2(t, "PATCH", "/v1/instances/rest-stage/labels", r.viewer, map[string]any{"set": map[string]string{"app": "x"}}); res.StatusCode != 403 {
		t.Fatalf("a viewer must not change labels: %d", res.StatusCode)
	}
}

func TestANonProductionTokenCannotSeeOrTouchProduction(t *testing.T) {
	r := newLabelRig(t)
	// it sees staging only: not production, and not the unlabelled tenant
	_, body := r.do2(t, "GET", "/v1/instances", r.nonprod, nil)
	if got := names(t, body); len(got) != 1 || got[0] != "rest-stage" {
		t.Fatalf("a staging-scoped token sees only staging: %v", got)
	}
	if res, _ := r.do2(t, "GET", "/v1/instances/rest-prod", r.nonprod, nil); res.StatusCode != 404 {
		t.Fatalf("production must look like it does not exist: %d", res.StatusCode)
	}
	if res, _ := r.do2(t, "GET", "/v1/instances/rest-plain", r.nonprod, nil); res.StatusCode != 404 {
		t.Fatalf("an unlabelled instance is never inside a label scope: %d", res.StatusCode)
	}
	deny := func(why, method, path string, body any) {
		t.Helper()
		res, resp := r.do2(t, method, path, r.nonprod, body)
		if res.StatusCode != 403 || !strings.Contains(resp, "out_of_scope") {
			t.Errorf("%s: want 403 out_of_scope, got %d %s", why, res.StatusCode, resp)
		}
	}
	deny("update production", "POST", "/v1/instances/rest-prod/update", map[string]any{"image": "opsavor-platform:v2", "service": "platform"})
	deny("resize production", "POST", "/v1/instances/rest-prod/resize", map[string]any{"limits": map[string]string{"limits.cpu": "2"}})
	deny("suspend production", "POST", "/v1/instances/rest-prod/suspend", map[string]any{"domain": "rest-prod.opsavor.app"})
	deny("delete production", "DELETE", "/v1/instances/rest-prod", nil)
	deny("update an unlabelled tenant", "POST", "/v1/instances/rest-plain/update", map[string]any{"image": "opsavor-platform:v2", "service": "platform"})
	deny("relabel its own tenant into production", "PATCH", "/v1/instances/rest-stage/labels", map[string]any{"set": map[string]string{"environment": "production"}})
	deny("remove the environment from its own tenant", "PATCH", "/v1/instances/rest-stage/labels", map[string]any{"remove": []string{"environment"}})
	deny("relabel production at all", "PATCH", "/v1/instances/rest-prod/labels", map[string]any{"set": map[string]string{"app": "x"}})
	create := func(labels any) *http.Response {
		spec := tenantSpecFor("rest-made")
		if labels != nil {
			spec["labels"] = labels
		}
		res, body := r.do2(t, "PUT", "/v1/instances/rest-made", r.nonprod, spec)
		if res.StatusCode == 202 {
			r.wait(t, body, JobSucceeded) // the host does one change at a time
		}
		return res
	}
	if res := create(nil); res.StatusCode != 403 {
		t.Errorf("creating an unlabelled instance with a label-scoped token must be refused: %d", res.StatusCode)
	}
	if res := create(map[string]string{"environment": "production"}); res.StatusCode != 403 {
		t.Errorf("creating a production instance must be refused: %d", res.StatusCode)
	}
	if res := create(map[string]string{"environment": "testing"}); res.StatusCode != 202 {
		t.Errorf("creating a testing instance is allowed: %d", res.StatusCode)
	}
	// and what it is allowed to do, it can do
	res, resp := r.do2(t, "POST", "/v1/instances/rest-stage/update", r.nonprod, map[string]any{"image": "opsavor-platform:v2", "service": "platform"})
	if res.StatusCode != 202 {
		t.Errorf("updating its own staging tenant: %d", res.StatusCode)
	} else {
		r.wait(t, resp, JobSucceeded)
	}
	res, resp = r.do2(t, "PATCH", "/v1/instances/rest-stage/labels", r.nonprod, map[string]any{"set": map[string]string{"environment": "testing", "app": "x"}})
	if res.StatusCode != 202 {
		t.Errorf("moving its tenant between non-production environments: %d", res.StatusCode)
	} else {
		r.wait(t, resp, JobSucceeded)
	}
	// the production token is the mirror image
	_, body = r.do2(t, "GET", "/v1/instances", r.prod, nil)
	if got := names(t, body); len(got) != 1 || got[0] != "rest-prod" {
		t.Fatalf("%v", got)
	}
	if res, _ := r.do2(t, "POST", "/v1/instances/rest-stage/update", r.prod, map[string]any{"image": "opsavor-platform:v2", "service": "platform"}); res.StatusCode != 403 {
		t.Errorf("a production token must not touch staging: %d", res.StatusCode)
	}
	// an unscoped deployer is unaffected
	if res, _ := r.do2(t, "POST", "/v1/instances/rest-plain/update", r.deployer, map[string]any{"image": "opsavor-platform:v2", "service": "platform"}); res.StatusCode != 202 {
		t.Errorf("an unscoped deployer can update anything: %d", res.StatusCode)
	}
}

func TestALabelScopeIsValidatedAndParsed(t *testing.T) {
	good := Scope{Names: []string{"rest-*"}, Images: []string{"a:*"}, Labels: map[string][]string{"environment": {"staging", "testing"}, "app": {"plat*"}}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for why, sc := range map[string]Scope{
		"an environment typo":   {Names: []string{"*"}, Images: []string{"*"}, Labels: map[string][]string{"environment": {"prodution"}}},
		"a bad label name":      {Names: []string{"*"}, Images: []string{"*"}, Labels: map[string][]string{"Bad": {"x"}}},
		"no values":             {Names: []string{"*"}, Images: []string{"*"}, Labels: map[string][]string{"app": {}}},
		"a bad value":           {Names: []string{"*"}, Images: []string{"*"}, Labels: map[string][]string{"app": {"a b"}}},
		"labels without scopes": {Labels: map[string][]string{"environment": {"staging"}}},
	} {
		if sc.Validate() == nil {
			t.Errorf("%s was accepted", why)
		}
	}
	if !good.AllowsLabels(map[string]string{"environment": "testing", "app": "platform"}) {
		t.Error("a matching instance must be inside the scope")
	}
	for _, l := range []map[string]string{nil, {"environment": "production", "app": "platform"}, {"environment": "testing"}, {"app": "platform"}} {
		if good.AllowsLabels(l) {
			t.Errorf("%v must be outside the scope", l)
		}
	}
	got, err := ParseLabelScope("environment=staging, testing; app=platform")
	if err != nil || len(got["environment"]) != 2 || got["environment"][1] != "testing" || got["app"][0] != "platform" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"environment", "environment=", "=staging", "a=b;c"} {
		if _, err := ParseLabelScope(bad); err == nil {
			t.Errorf("%q must not parse", bad)
		}
	}
	if m, err := ParseLabelScope(""); m != nil || err != nil {
		t.Fatal("empty means no label scope")
	}
	if !strings.Contains(good.Summary(), "environment=staging,testing") {
		t.Fatalf("the summary must show the labels: %s", good.Summary())
	}
}
