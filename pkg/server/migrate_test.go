package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/engine"
)

// recordedSpec is what demo-old records: the spec it was launched from. Env is set here only to prove that
// nothing ever answers with it (the engine never records it in the first place).
func recordedSpec() *engine.InstanceSpec {
	return &engine.InstanceSpec{
		Template: "platform", Image: "opsavor-platform:latest", Profiles: []string{"base", "service"},
		Volumes: []engine.VolumeSpec{{Name: "demo-old-data", Path: "/app/.data"}},
		Service: "platform", Env: map[string]string{"OPSAVOR_CONTROL_TOKEN": "SENTINEL-TENANT-SECRET-91af"},
		Health: &engine.HealthSpec{Path: "/health", Port: 8787},
		Domain: "demo-old.opsavor.app", RouteDirectives: []string{"import strip-forged-identity"},
		Labels: map[string]string{},
	}
}

func newMigrateRig(t *testing.T) *instRig {
	rig := newInstRig(t)
	rig.ops.specs = map[string]*engine.InstanceSpec{"demo-old": recordedSpec(), "x-one": recordedSpec()}
	return rig
}

func adoptSpec() map[string]any {
	s := tenantSpecFor("demo-old")
	delete(s, "env")
	return s
}

// The routes and the bodies that reach a job, for the role gates.
var migrationCalls = []struct {
	method, path string
	body         func() any
}{
	{"POST", "/v1/instances/demo-old/migrate", func() any { return map[string]any{"target_remote": "host-b"} }},
	{"POST", "/v1/instances/demo-old/adopt", func() any { return adoptSpec() }},
	{"POST", "/v1/instances/demo-old/finalize-migration", func() any { return map[string]any{} }},
}

func TestRolesForMovingAnInstance(t *testing.T) {
	for _, c := range migrationCalls {
		t.Run(c.path, func(t *testing.T) {
			rig := newMigrateRig(t)
			if c.path == "/v1/instances/demo-old/finalize-migration" {
				rig.ops.migrated = map[string]string{"demo-old": "host-b"}
			}
			if res, _ := rig.do2(t, c.method, c.path, "", c.body()); res.StatusCode != 401 {
				t.Errorf("no token: %d", res.StatusCode)
			}
			for name, tok := range map[string]string{"viewer": rig.viewer, "planner": rig.planner, "an unscoped deployer": rig.deployer} {
				if res, body := rig.do2(t, c.method, c.path, tok, c.body()); res.StatusCode != 403 {
					t.Errorf("%s must be refused: %d %s", name, res.StatusCode, body)
				}
			}
			if res, body := rig.do2(t, c.method, c.path, rig.narrow, c.body()); res.StatusCode != 403 || !strings.Contains(body, "out_of_scope") {
				t.Errorf("a token scoped to other instances: %d %s", res.StatusCode, body)
			}
			if n := len(rig.srv.opts.Jobs.List()); n != 0 {
				t.Fatalf("no refused call may start a job, got %d", n)
			}
			res, body := rig.do2(t, c.method, c.path, rig.scoped, c.body())
			if res.StatusCode != 202 {
				t.Fatalf("the token scoped to the instance: %d %s", res.StatusCode, body)
			}
			rig.wait(t, body, JobSucceeded)
			res, body = rig.do2(t, c.method, c.path, rig.adminTok, c.body())
			if res.StatusCode != 202 {
				t.Fatalf("an admin: %d %s", res.StatusCode, body)
			}
			rig.wait(t, body, JobSucceeded)
		})
	}
}

func TestMigrateIsAJobWhoseResultSaysTheSnapshotsStayed(t *testing.T) {
	rig := newMigrateRig(t)
	res, body := rig.do2(t, "POST", "/v1/instances/demo-old/migrate", rig.scoped, map[string]any{"target_remote": "host-b", "resume": true})
	if res.StatusCode != 202 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	j := rig.wait(t, body, JobSucceeded)
	if j.Kind != "instance:migrate" || j.Service != "demo-old" || !strings.Contains(j.Result, "snapshots were not moved") || !strings.Contains(j.Log, "migrating demo-old to host-b") {
		t.Fatalf("the job: %+v", j)
	}
	if len(rig.ops.migrates) != 1 || rig.ops.migrates[0] != (engine.MigrateRequest{TargetRemote: "host-b", Resume: true}) {
		t.Fatalf("what reached the host: %+v", rig.ops.migrates)
	}
}

func TestAFailedMigrateJobIsRecordedAsFailed(t *testing.T) {
	rig := newMigrateRig(t)
	rig.ops.err = errFake("health check on target failed; the source demo-old on local was restarted and nothing was lost")
	_, body := rig.do2(t, "POST", "/v1/instances/demo-old/migrate", rig.scoped, map[string]any{"target_remote": "host-b"})
	j := rig.wait(t, body, JobFailed)
	if j.Result != "" || !strings.Contains(j.Error, "nothing was lost") {
		t.Fatalf("a failed migration has no result and says why: %+v", j)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

func TestMigrateIsRefusedForWhatCannotMove(t *testing.T) {
	cases := map[string]struct {
		setup func(rig *instRig)
		inst  string
		body  any
		code  int
		want  string
	}{
		"a host path":      {func(r *instRig) { r.ops.hostPaths = map[string][]string{"demo-old": {"/srv/data"}} }, "demo-old", map[string]any{"target_remote": "host-b"}, 409, "not_migratable"},
		"no recorded spec": {func(r *instRig) { delete(r.ops.specs, "demo-old") }, "demo-old", map[string]any{"target_remote": "host-b"}, 409, "PUT"},
		"already migrated": {func(r *instRig) { r.ops.migrated = map[string]string{"demo-old": "host-b"} }, "demo-old", map[string]any{"target_remote": "host-c"}, 409, "migrated"},
		"no such instance": {nil, "demo-nope", map[string]any{"target_remote": "host-b"}, 404, "not_found"},
		"not a tenant":     {nil, "gitea", map[string]any{"target_remote": "host-b"}, 403, "not_managed"},
		"this host":        {nil, "demo-old", map[string]any{"target_remote": "local"}, 400, "this host"},
		"a bad remote":     {nil, "demo-old", map[string]any{"target_remote": "host b"}, 400, "target_remote"},
		"no remote":        {nil, "demo-old", map[string]any{}, 400, "target_remote"},
		"an unknown field": {nil, "demo-old", map[string]any{"target_remote": "host-b", "target_hots": "x"}, 400, "bad_request"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newMigrateRig(t)
			if tc.setup != nil {
				tc.setup(rig)
			}
			tok := rig.scoped
			if tc.inst == "gitea" {
				tok = rig.adminTok
			}
			res, body := rig.do2(t, "POST", "/v1/instances/"+tc.inst+"/migrate", tok, tc.body)
			if res.StatusCode != tc.code || !strings.Contains(body, tc.want) {
				t.Fatalf("want %d %q, got %d %s", tc.code, tc.want, res.StatusCode, body)
			}
			if len(rig.ops.migrates) != 0 || len(rig.srv.opts.Jobs.List()) != 0 {
				t.Fatal("nothing may run")
			}
		})
	}
}

func TestExportSpecIsScopedLikeGetAndNeverCarriesASecret(t *testing.T) {
	rig := newMigrateRig(t)
	rig.ops.migrated = map[string]string{"demo-old": "host-b"}
	for name, tok := range map[string]string{"the scoped token": rig.scoped, "a viewer": rig.viewer, "an admin": rig.adminTok} {
		res, body := rig.do2(t, "GET", "/v1/instances/demo-old/export-spec", tok, nil)
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d %s", name, res.StatusCode, body)
		}
		if strings.Contains(body, "SENTINEL-TENANT-SECRET") || strings.Contains(body, "OPSAVOR_CONTROL_TOKEN") || strings.Contains(body, `"env"`) {
			t.Fatalf("%s: the export must never carry the environment: %s", name, body)
		}
		var out struct {
			Name       string              `json:"name"`
			Spec       engine.InstanceSpec `json:"spec"`
			MigratedTo string              `json:"migrated_to"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil || out.Name != "demo-old" || out.MigratedTo != "host-b" ||
			out.Spec.Domain != "demo-old.opsavor.app" || len(out.Spec.Volumes) != 1 || out.Spec.Image != "opsavor-platform:latest" {
			t.Fatalf("%s: %v %s", name, err, body)
		}
	}
	if res, _ := rig.do2(t, "GET", "/v1/instances/demo-old/export-spec", "", nil); res.StatusCode != 401 {
		t.Errorf("no token: %d", res.StatusCode)
	}
	for _, name := range []string{"x-one", "gitea", "demo-nope"} {
		if res, _ := rig.do2(t, "GET", "/v1/instances/"+name+"/export-spec", rig.scoped, nil); res.StatusCode != 404 {
			t.Errorf("%s: outside the scope, static or missing all look the same: %d", name, res.StatusCode)
		}
	}
	if res, body := rig.do2(t, "GET", "/v1/instances/rest-one/export-spec", rig.scoped, nil); res.StatusCode != 409 || !strings.Contains(body, "no_spec") {
		t.Errorf("an instance with no recorded spec: %d %s", res.StatusCode, body)
	}
}

func TestAdoptIsAJobRefusedWhenTheInstanceIsNotHere(t *testing.T) {
	rig := newMigrateRig(t)
	res, body := rig.do2(t, "POST", "/v1/instances/demo-old/adopt", rig.scoped, adoptSpec())
	if res.StatusCode != 202 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	j := rig.wait(t, body, JobSucceeded)
	if j.Kind != "instance:adopt" || len(rig.ops.adopted) != 1 || rig.ops.adopted[0].Domain != "demo-old.opsavor.app" {
		t.Fatalf("the job: %+v %+v", j, rig.ops.adopted)
	}

	missing := adoptSpec()
	missing["volumes"] = []map[string]string{{"name": "demo-gone-data", "path": "/app/.data"}}
	missing["domain"] = "demo-gone.opsavor.app"
	if res, body := rig.do2(t, "POST", "/v1/instances/demo-gone/adopt", rig.scoped, missing); res.StatusCode != 404 || !strings.Contains(body, "migrate it here first") {
		t.Fatalf("an instance that is not on this host: %d %s", res.StatusCode, body)
	}
	withEnv := adoptSpec()
	withEnv["env"] = map[string]string{"K": "v"}
	if res, _ := rig.do2(t, "POST", "/v1/instances/demo-old/adopt", rig.scoped, withEnv); res.StatusCode != 400 {
		t.Fatalf("a spec with env: %d", res.StatusCode)
	}
	other := adoptSpec()
	other["template"] = "other"
	if res, body := rig.do2(t, "POST", "/v1/instances/demo-old/adopt", rig.scoped, other); res.StatusCode != 409 {
		t.Fatalf("another template: %d %s", res.StatusCode, body)
	}
	evil := adoptSpec()
	evil["domain"] = "evil.example.com"
	if res, _ := rig.do2(t, "POST", "/v1/instances/demo-old/adopt", rig.scoped, evil); res.StatusCode != 403 {
		t.Fatalf("a domain outside the scope: %d", res.StatusCode)
	}
	bad := adoptSpec()
	bad["profiles"] = []string{"privileged"}
	if res, _ := rig.do2(t, "POST", "/v1/instances/demo-old/adopt", rig.scoped, bad); res.StatusCode != 400 {
		t.Fatalf("an invalid spec: %d", res.StatusCode)
	}
	if res, _ := rig.do2(t, "POST", "/v1/instances/gitea/adopt", rig.adminTok, adoptSpec()); res.StatusCode != 400 && res.StatusCode != 403 {
		t.Fatalf("a static service is never adopted: %d", res.StatusCode)
	}
	if len(rig.ops.adopted) != 1 {
		t.Fatalf("no refused adopt reaches the host: %+v", rig.ops.adopted)
	}
}

func TestFinalizeMigrationIsAJobOnlyForAMigratedCopy(t *testing.T) {
	rig := newMigrateRig(t)
	if res, body := rig.do2(t, "POST", "/v1/instances/demo-old/finalize-migration", rig.scoped, map[string]any{}); res.StatusCode != 409 || !strings.Contains(body, "not_migrated") {
		t.Fatalf("an instance that was not migrated: %d %s", res.StatusCode, body)
	}
	rig.ops.migrated = map[string]string{"demo-old": "host-b"}
	if res, _ := rig.do2(t, "POST", "/v1/instances/demo-old/finalize-migration", rig.scoped, map[string]any{"target_remote": "host-c"}); res.StatusCode != 409 {
		t.Fatalf("another target: %d", res.StatusCode)
	}
	res, body := rig.do2(t, "POST", "/v1/instances/demo-old/finalize-migration", rig.scoped, map[string]any{"purge_source_volumes": true})
	if res.StatusCode != 202 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	j := rig.wait(t, body, JobSucceeded)
	if j.Kind != "instance:finalize-migration" || len(rig.ops.finalized) != 1 || !rig.ops.finalized[0].PurgeSourceVolumes {
		t.Fatalf("the job: %+v %+v", j, rig.ops.finalized)
	}
	// A repeat once the copy is gone needs the target named.
	if res, _ := rig.do2(t, "POST", "/v1/instances/demo-gone/finalize-migration", rig.scoped, map[string]any{}); res.StatusCode != 404 {
		t.Fatalf("gone, no target: %d", res.StatusCode)
	}
	if res, body := rig.do2(t, "POST", "/v1/instances/demo-gone/finalize-migration", rig.scoped, map[string]any{"target_remote": "host-b"}); res.StatusCode != 202 {
		t.Fatalf("gone, target named: %d %s", res.StatusCode, body)
	} else {
		rig.wait(t, body, JobSucceeded)
	}
}

func TestAMigratedCopyCannotBeChangedOnlyRemoved(t *testing.T) {
	rig := newMigrateRig(t)
	rig.ops.migrated = map[string]string{"demo-old": "host-b"}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"PUT", "/v1/instances/demo-old", tenantSpecFor("demo-old")},
		{"POST", "/v1/instances/demo-old/update", map[string]any{"image": "opsavor-platform:v2", "service": "platform"}},
		{"POST", "/v1/instances/demo-old/resize", map[string]any{"limits": map[string]string{"limits.cpu": "2"}}},
		{"POST", "/v1/instances/demo-old/suspend", map[string]any{"domain": "demo-old.opsavor.app", "reason": "x"}},
		{"PATCH", "/v1/instances/demo-old/labels", map[string]any{"set": map[string]string{"app": "x"}}},
	} {
		if res, body := rig.do2(t, c.method, c.path, rig.scoped, c.body); res.StatusCode != 409 || !strings.Contains(body, "host-b") {
			t.Errorf("%s %s on a migrated copy: %d %s", c.method, c.path, res.StatusCode, body)
		}
	}
	if len(rig.srv.opts.Jobs.List()) != 0 {
		t.Fatal("nothing may run on a migrated copy")
	}
	res, body := rig.do2(t, "DELETE", "/v1/instances/demo-old", rig.scoped, nil)
	if res.StatusCode != 202 {
		t.Fatalf("a migrated copy can still be removed: %d %s", res.StatusCode, body)
	}
	rig.wait(t, body, JobSucceeded)
}
