package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

// refusals wraps the read-only executor and remembers every command it turned down. The wall
// already stops such a command; this catches plan code that tried and swallowed the error.
type refusals struct {
	remote.Executor
	refused []string
}

func (r *refusals) Run(ctx context.Context, cmd string) (string, error) {
	out, err := r.Executor.Run(ctx, cmd)
	var ro *remote.ReadOnlyError
	if errors.As(err, &ro) {
		r.refused = append(r.refused, cmd)
	}
	return out, err
}

// planDeployer is a Deployer that can only read: every command goes through remote.ReadOnly.
func planDeployer(sim *hostSim) (*Deployer, *refusals) {
	spy := &refusals{Executor: remote.ReadOnly(sim)}
	return NewDeployer(spy), spy
}

func kindsOf(p *ServicePlan) []string {
	var k []string
	for _, c := range p.Changes {
		k = append(k, c.Kind)
	}
	sort.Strings(k)
	return k
}

func sameKinds(got []string, want ...string) bool {
	sort.Strings(want)
	return strings.Join(got, ",") == strings.Join(want, ",")
}

func detailOf(p *ServicePlan, kind string) string {
	for _, c := range p.Changes {
		if c.Kind == kind {
			return c.Detail
		}
	}
	return ""
}

// plan runs PlanService and proves it changed nothing.
func plan(t *testing.T, sim *hostSim, svc *config.ServiceConfig) *ServicePlan {
	t.Helper()
	before := len(sim.cmds)
	d, spy := planDeployer(sim)
	p, err := d.PlanService(context.Background(), svc, t.TempDir())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(spy.refused) != 0 {
		t.Fatalf("plan tried to run commands that could change the host (the read-only executor stopped them):\n%s", strings.Join(spy.refused, "\n"))
	}
	for _, c := range sim.cmds[before:] {
		if !remote.IsReadOnly(c) {
			t.Fatalf("plan issued a command that is not read-only: %s", c)
		}
		if simMutatorRe.MatchString(c) {
			t.Fatalf("plan changed the host: %s", c)
		}
	}
	return p
}

func deployedGitea(t *testing.T) (*hostSim, *Deployer, *config.ServiceConfig) {
	t.Helper()
	sim := newHostSim(t)
	sim.aliases["gitea:latest"] = fpA
	d := newTestDeployer(sim)
	svc := giteaSvc()
	if err := d.DeployService(context.Background(), svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	sim.reset()
	return sim, d, svc
}

func deployedDB(t *testing.T) (*hostSim, *Deployer, *config.ServiceConfig) {
	t.Helper()
	sim := newHostSim(t)
	d := newTestDeployer(sim)
	svc := &config.ServiceConfig{Name: "db", Image: "postgres:16", Limits: map[string]string{"limits.cpu": "1"}}
	if err := d.DeployService(context.Background(), svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	sim.reset()
	return sim, d, svc
}

// What each kind of planned change looks like when apply really does it.
func observed(kind string, sim *hostSim, existedBefore bool) bool {
	mut := strings.Join(sim.mutations(), "\n")
	has := func(parts ...string) bool {
		for _, line := range sim.mutations() {
			ok := true
			for _, p := range parts {
				ok = ok && strings.Contains(line, p)
			}
			if ok {
				return true
			}
		}
		return false
	}
	switch kind {
	case ChangeCreateInstance:
		return sim.count("incus launch") > 0 && !existedBefore
	case ChangeReplaceImage:
		return sim.count("incus launch") > 0 && existedBefore
	case ChangeAdoptImage:
		return has("incus config set", incus.ImageKey)
	case ChangeCreateVolume:
		return has("incus storage volume create")
	case ChangeAttachVolume:
		return has("incus config device add")
	case ChangeSetLimits:
		return has("incus config set", "limits.")
	case ChangeSetEnv:
		return has("incus file push", "/etc/default/")
	case ChangeRestart:
		return has("systemctl restart") && !strings.Contains(mut, "incus launch")
	case ChangePublishRoute, ChangeUpdateRoute:
		return has("incus file push", "/etc/caddy/sites/")
	case ChangeRunHook:
		return len(sim.hooks)+len(sim.inits) > 0
	}
	return false
}

// The reason plan is trustworthy: for each state, what it predicts is what apply then does, and
// a plan with no changes is an apply that issues no mutating command.
func TestPlanPredictsWhatApplyDoes(t *testing.T) {
	scenarios := []struct {
		name    string
		setup   func(t *testing.T) (*hostSim, *Deployer, *config.ServiceConfig)
		drift   func(sim *hostSim, svc *config.ServiceConfig)
		existed bool
		want    []string
		action  string
	}{
		{"nothing exists yet", func(t *testing.T) (*hostSim, *Deployer, *config.ServiceConfig) {
			sim := newHostSim(t)
			sim.aliases["gitea:latest"] = fpA
			return sim, newTestDeployer(sim), giteaSvc()
		}, nil, false, []string{ChangeCreateInstance, ChangeCreateVolume, ChangeAttachVolume, ChangeSetEnv, ChangePublishRoute, ChangeRunHook, ChangeRunHook, ChangeRunHook}, ActionCreate},
		{"already converged", deployedGitea, nil, true, nil, ActionNone},
		{"a limit changed", deployedGitea, func(sim *hostSim, svc *config.ServiceConfig) { svc.Limits["limits.cpu"] = "4" }, true,
			[]string{ChangeSetLimits}, ActionUpdate},
		{"an env value changed and a key was added", deployedGitea, func(sim *hostSim, svc *config.ServiceConfig) {
			svc.Env["GITEA_PORT"] = "3001"
			svc.Env["NEW_KEY"] = "n"
		}, true, []string{ChangeSetEnv, ChangeRestart}, ActionUpdate},
		{"a volume was added to the manifest", deployedGitea, func(sim *hostSim, svc *config.ServiceConfig) {
			svc.Volumes = append(svc.Volumes, config.VolumeMount{Name: "gitea-repos", Path: "/srv/repos", Pool: "default", Shifted: true})
		}, true, []string{ChangeCreateVolume, ChangeAttachVolume}, ActionUpdate},
		{"the image moved", deployedGitea, func(sim *hostSim, svc *config.ServiceConfig) { sim.aliases["gitea:latest"] = fpB }, true,
			[]string{ChangeReplaceImage, ChangeRunHook, ChangeRunHook, ChangeRunHook, ChangeUpdateRoute}, ActionUpdate},
		{"the route is missing from the edge", deployedGitea, func(sim *hostSim, svc *config.ServiceConfig) {
			delete(sim.ctrs["edge"].files, "/etc/caddy/sites/gitea.caddy")
		}, true, []string{ChangePublishRoute}, ActionUpdate},
		{"the route points at the wrong address", deployedGitea, func(sim *hostSim, svc *config.ServiceConfig) {
			sim.ctrs["edge"].files["/etc/caddy/sites/gitea.caddy"] = strings.Replace(sim.ctrs["edge"].files["/etc/caddy/sites/gitea.caddy"], sim.ctrs["gitea"].ip, "10.0.100.250", 1)
		}, true, []string{ChangeUpdateRoute}, ActionUpdate},
		{"an OCI reference moved", deployedDB, func(sim *hostSim, svc *config.ServiceConfig) { svc.Image = "postgres:17" }, true,
			[]string{ChangeReplaceImage}, ActionUpdate},
		{"a running instance never had its image recorded", deployedDB, func(sim *hostSim, svc *config.ServiceConfig) {
			delete(sim.ctrs["db"].config, incus.ImageKey)
		}, true, []string{ChangeAdoptImage}, ActionUpdate},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			sim, d, svc := sc.setup(t)
			if sc.drift != nil {
				sc.drift(sim, svc)
			}
			sim.reset()
			p := plan(t, sim, svc)
			if p.Action != sc.action || !sameKinds(kindsOf(p), sc.want...) {
				t.Fatalf("plan = %s %v, want %s %v\n%s", p.Action, kindsOf(p), sc.action, sc.want, (&FleetPlan{Services: []*ServicePlan{p}}).Render())
			}

			sim.reset()
			if err := d.DeployService(context.Background(), svc, t.TempDir()); err != nil {
				t.Fatalf("apply: %v", err)
			}
			if (p.Action == ActionNone) != (len(sim.mutations()) == 0) {
				t.Fatalf("plan says %q but apply issued: %v", p.Action, sim.mutations())
			}
			predicted := map[string]bool{}
			for _, k := range kindsOf(p) {
				predicted[k] = true
			}
			for _, k := range []string{ChangeCreateInstance, ChangeReplaceImage, ChangeAdoptImage, ChangeCreateVolume, ChangeAttachVolume,
				ChangeSetLimits, ChangeSetEnv, ChangeRestart, ChangePublishRoute, ChangeRunHook} {
				// A replacement carries volumes and env over, restarts, and repoints the route as part of
				// the safe update path; they are not separate changes there.
				if predicted[ChangeReplaceImage] && (k == ChangeAttachVolume || k == ChangeSetEnv || k == ChangeRestart) {
					continue
				}
				got := observed(k, sim, sc.existed)
				want := predicted[k] || (k == ChangePublishRoute && predicted[ChangeUpdateRoute])
				if got != want {
					t.Errorf("%s: plan predicted=%v but apply did it=%v\nmutations:\n%s", k, want, got, strings.Join(sim.mutations(), "\n"))
				}
			}
		})
	}
}

func TestPlanFlagsWhatApplyWouldRefuse(t *testing.T) {
	ctx := context.Background()

	t.Run("a device already has the name a volume needs", func(t *testing.T) {
		sim, d, svc := deployedGitea(t)
		sim.ctrs["gitea"].devices["var-lib-gitea"]["source"] = "someone-elses-volume"
		p := plan(t, sim, svc)
		if p.Action != ActionBlocked || len(p.Blockers) != 1 || !strings.Contains(p.Blockers[0], "someone-elses-volume") {
			t.Fatalf("want a blocker naming the clash, got %+v", p)
		}
		if err := d.DeployService(ctx, svc, t.TempDir()); err == nil {
			t.Fatal("apply would in fact have failed here; the plan and apply must agree")
		}
	})

	t.Run("the edge does not import the sites directory", func(t *testing.T) {
		sim, d, svc := deployedGitea(t)
		sim.ctrs["edge"].files["/etc/caddy/Caddyfile"] = "# hand written, no import\n"
		p := plan(t, sim, svc)
		if p.Action != ActionBlocked || !strings.Contains(strings.Join(p.Blockers, " "), "does not import") {
			t.Fatalf("got %+v", p)
		}
		if err := d.DeployService(ctx, svc, t.TempDir()); err == nil {
			t.Fatal("apply must agree that this cannot be published")
		}
	})

	t.Run("there is no edge instance to publish to", func(t *testing.T) {
		sim, d, svc := deployedGitea(t)
		delete(sim.ctrs, "edge")
		p := plan(t, sim, svc)
		if p.Action != ActionBlocked || !strings.Contains(strings.Join(p.Blockers, " "), "edge") {
			t.Fatalf("got %+v", p)
		}
		if err := d.DeployService(ctx, svc, t.TempDir()); err == nil {
			t.Fatal("apply must agree")
		}
	})

	t.Run("a stopped instance cannot be health-checked or routed to", func(t *testing.T) {
		sim, _, svc := deployedGitea(t)
		sim.ctrs["gitea"].status, sim.ctrs["gitea"].ip = "Stopped", ""
		p := plan(t, sim, svc)
		if p.Action != ActionBlocked || !strings.Contains(strings.Join(p.Blockers, " "), "no IPv4") {
			t.Fatalf("got %+v", p)
		}
	})

	t.Run("a stopped instance that is being replaced is fine", func(t *testing.T) {
		sim, _, svc := deployedGitea(t)
		sim.ctrs["gitea"].status, sim.ctrs["gitea"].ip = "Stopped", ""
		sim.aliases["gitea:latest"] = fpB
		if p := plan(t, sim, svc); p.Action != ActionUpdate {
			t.Fatalf("the replacement gets its own address: %+v", p)
		}
	})
}

func TestPlanNotesWhatApplyWillNotAct_On(t *testing.T) {
	t.Run("an image that cannot be identified", func(t *testing.T) {
		sim, _, svc := deployedGitea(t)
		sim.ctrs["gitea"].config["volatile.base_image"] = ""
		p := plan(t, sim, svc)
		if p.Action != ActionNone || len(p.Notes) != 1 || !strings.Contains(p.Notes[0], "cannot be identified") {
			t.Fatalf("got %+v", p)
		}
	})
	t.Run("profiles that differ", func(t *testing.T) {
		sim, _, svc := deployedGitea(t)
		svc.Profiles = []string{"base", "other"}
		p := plan(t, sim, svc)
		if p.Action != ActionNone || !strings.Contains(strings.Join(p.Notes, " "), "profiles of gitea differ") {
			t.Fatalf("got %+v", p)
		}
	})
	t.Run("an OCI container that reads environment.* config, not /etc/default", func(t *testing.T) {
		sim, _, _ := deployedDB(t)
		sim.ctrs["db"].config["environment.POSTGRES_PASSWORD"] = "x"
		svc := &config.ServiceConfig{Name: "db", Image: "postgres:16", Env: map[string]string{"POSTGRES_DB": "app"}}
		p := plan(t, sim, svc)
		if !strings.Contains(strings.Join(p.Notes, " "), "environment.*") {
			t.Fatalf("this is the gap that would silently do nothing useful: %+v", p)
		}
	})
}

// The plan goes into CI logs. Env values, live or declared, never appear in it, only key names.
func TestPlanNeverContainsAnEnvironmentValue(t *testing.T) {
	sim, _, svc := deployedGitea(t)
	sim.ctrs["gitea"].files["/etc/default/gitea"] += "RUNTIME_TOKEN=SENTINEL-LIVE-7f3a\n"
	svc.Env["DB_PASSWORD"] = "SENTINEL-DECLARED-91bc"
	svc.Env["GITEA_PORT"] = "SENTINEL-CHANGED-44de"

	p := plan(t, sim, svc)
	text := (&FleetPlan{Services: []*ServicePlan{p}}).Render()
	js, _ := json.Marshal(p)
	for _, secret := range []string{"SENTINEL-LIVE-7f3a", "SENTINEL-DECLARED-91bc", "SENTINEL-CHANGED-44de"} {
		if strings.Contains(text, secret) || strings.Contains(string(js), secret) {
			t.Fatalf("the plan leaked %s:\n%s", secret, text)
		}
	}
	d := detailOf(p, ChangeSetEnv)
	if !strings.Contains(d, "add DB_PASSWORD") || !strings.Contains(d, "change GITEA_PORT") || !strings.Contains(d, "1 keys the manifest does not declare are kept") {
		t.Fatalf("the plan should name keys and count the preserved ones: %q", d)
	}

	// The same for a service that does not exist yet.
	fresh := newHostSim(t)
	fresh.aliases["gitea:latest"] = fpA
	svc2 := giteaSvc()
	svc2.Env["DB_PASSWORD"] = "SENTINEL-DECLARED-91bc"
	p2 := plan(t, fresh, svc2)
	if strings.Contains((&FleetPlan{Services: []*ServicePlan{p2}}).Render(), "SENTINEL-DECLARED-91bc") {
		t.Fatal("a create plan leaked an env value")
	}
}

// Plan is safe on production because the executor refuses anything that could change it, not
// because every function it calls happens to behave. Prove the wall is there: hand the read-only
// executor to apply itself and it can neither launch nor write.
func TestTheReadOnlyExecutorStopsApplyDeadInItsTracks(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["gitea:latest"] = fpA
	d := newTestDeployer(sim)
	d.incus = incus.NewClient(remote.ReadOnly(sim))
	d.exec = remote.ReadOnly(sim)

	err := d.DeployService(context.Background(), giteaSvc(), t.TempDir())
	var ro *remote.ReadOnlyError
	if !errors.As(err, &ro) {
		t.Fatalf("apply behind the read-only executor must fail with a ReadOnlyError, got %v", err)
	}
	if m := sim.mutations(); len(m) != 0 || sim.ctrs["gitea"] != nil {
		t.Fatalf("nothing may have changed: %v", m)
	}
}

func TestPlanIsRepeatableAndDoesNotDisturbApply(t *testing.T) {
	sim, d, svc := deployedGitea(t)
	svc.Limits["limits.cpu"] = "4"
	first := plan(t, sim, svc)
	for i := 0; i < 3; i++ {
		if again := plan(t, sim, svc); !sameKinds(kindsOf(again), kindsOf(first)...) {
			t.Fatalf("plan changed between runs: %v vs %v", kindsOf(again), kindsOf(first))
		}
	}
	if err := d.DeployService(context.Background(), svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if p := plan(t, sim, svc); p.Action != ActionNone {
		t.Fatalf("after apply the plan must be empty: %+v", p)
	}
}

func TestFleetPlanSummaryAndExitSemantics(t *testing.T) {
	f := &FleetPlan{Services: []*ServicePlan{
		{Service: "b", Action: ActionNone},
		{Service: "a", Action: ActionCreate, Changes: []Change{{Kind: ChangeCreateInstance, Detail: "launch a"}}},
	}}
	if !f.Pending() || f.Blocked() {
		t.Fatalf("pending=%v blocked=%v", f.Pending(), f.Blocked())
	}
	out := f.Render()
	if strings.Index(out, "a (create)") > strings.Index(out, "b (none)") || !strings.Contains(out, "Plan: 1 to create, 0 to update, 1 unchanged, 0 blocked.") {
		t.Fatalf("render:\n%s", out)
	}
	if (&FleetPlan{Services: []*ServicePlan{{Service: "x", Action: ActionNone}}}).Pending() {
		t.Fatal("nothing to do is not pending")
	}
	if !(&FleetPlan{Services: []*ServicePlan{{Service: "x", Action: ActionBlocked}}}).Blocked() {
		t.Fatal("a blocked service blocks the plan")
	}
}

func TestAHookThatNamesAFileOutsideTheConfigDirectoryIsNeverRead(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "outside-secret.sh")
	os.WriteFile(outside, []byte("echo SHOULD-NOT-RUN"), 0o644)
	defer os.Remove(outside)
	os.MkdirAll(filepath.Join(dir, "services", "web"), 0o755)
	os.WriteFile(filepath.Join(dir, "services", "web", "init.sh"), []byte("echo inside"), 0o644)

	if got := hookScript(dir, "web", "init.sh"); got != "echo inside" {
		t.Fatalf("a hook inside the directory is read: %q", got)
	}
	for _, escape := range []string{"../outside-secret.sh", "../../outside-secret.sh", outside} {
		if got := hookScript(dir, "web", escape); got != escape {
			t.Errorf("%q escapes the config directory and must be the script text itself, not the file it names: got %q", escape, got)
		}
	}
}

func TestPlanHashIdentifiesWhatWouldHappen(t *testing.T) {
	a := &ServicePlan{Service: "a", Action: ActionCreate, Changes: []Change{{Kind: ChangeCreateInstance, Detail: "launch a"}}}
	b := &ServicePlan{Service: "b", Action: ActionNone}
	f1 := &FleetPlan{Services: []*ServicePlan{a, b}}
	f2 := &FleetPlan{Services: []*ServicePlan{b, a}}
	if f1.Hash() != f2.Hash() || len(f1.Hash()) != 64 {
		t.Fatalf("the order services were planned in must not matter: %s %s", f1.Hash(), f2.Hash())
	}
	changed := &FleetPlan{Services: []*ServicePlan{{Service: "a", Action: ActionCreate, Changes: []Change{{Kind: ChangeCreateInstance, Detail: "launch a from another image"}}}, b}}
	if changed.Hash() == f1.Hash() {
		t.Fatal("a different change must give a different hash")
	}
	if (&FleetPlan{Services: []*ServicePlan{}}).Hash() == f1.Hash() {
		t.Fatal("an empty plan is not this plan")
	}
	if f1.ExitStatus() != 2 || (&FleetPlan{}).ExitStatus() != 0 || (&FleetPlan{Services: []*ServicePlan{{Service: "x", Action: ActionBlocked}}}).ExitStatus() != 1 {
		t.Fatal("exit status: pending is 2, empty is 0, blocked is 1")
	}
}

func bindPlan(t *testing.T, sim *hostSim, svc *config.ServiceConfig, dir string, key string) *FleetPlan {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "services", svc.Name), 0o755)
	os.WriteFile(filepath.Join(root, "fleet.yml"), []byte("name: f\n"), 0o644)
	if key == "" {
		key = "-"
	}
	// The plan is made through PlanService directly so the manifest under test is used as given.
	d := NewDeployer(remote.ReadOnly(sim))
	if key != "-" {
		WithBindKey([]byte(key))(d)
	}
	p, err := d.PlanService(context.Background(), svc, dir)
	if err != nil {
		t.Fatal(err)
	}
	return &FleetPlan{Services: []*ServicePlan{p}}
}

// The plan prints key names, not values, so two different secrets give the same plan text. The
// hash a gate compares must still tell them apart, or "apply what was reviewed" is a promise about
// the key names only.
func TestTheHashCoversValuesAndHooksThePlanDoesNotPrint(t *testing.T) {
	sim, _, svc := deployedGitea(t)
	dir := t.TempDir()
	svc.Env["GITEA_PORT"] = "3001" // live has 3000: the plan says "change GITEA_PORT"

	a := bindPlan(t, sim, svc, dir, "k1")
	svc.Env["GITEA_PORT"] = "3002" // a different value; the same plan text
	b := bindPlan(t, sim, svc, dir, "k1")
	if a.Render() != b.Render() {
		t.Fatalf("test setup: the two plans should read the same:\n%s\n%s", a.Render(), b.Render())
	}
	if a.Hash() == b.Hash() {
		t.Fatal("a changed environment value with the same key names must change the hash")
	}
	if a.Hash() != bindPlan(t, sim, giteaWith(svc, "3001"), dir, "k1").Hash() {
		t.Fatal("the same manifest must give the same hash")
	}
	if a.Hash() == bindPlan(t, sim, giteaWith(svc, "3001"), dir, "another key").Hash() {
		t.Fatal("the hash is keyed: another key gives another hash, so it says nothing about a value without the key")
	}

	// A hook body is not in the plan either.
	h1 := giteaWith(svc, "3001")
	h1.Hooks.PostDeploy = "echo one"
	h2 := giteaWith(svc, "3001")
	h2.Hooks.PostDeploy = "echo two"
	if bindPlan(t, sim, h1, dir, "k1").Hash() == bindPlan(t, sim, h2, dir, "k1").Hash() {
		t.Fatal("a changed hook body must change the hash")
	}
	// ...and a file-based hook counts by its contents.
	os.MkdirAll(filepath.Join(dir, "services", "gitea"), 0o755)
	h3 := giteaWith(svc, "3001")
	h3.Hooks.PostDeploy = "post.sh"
	os.WriteFile(filepath.Join(dir, "services", "gitea", "post.sh"), []byte("echo A"), 0o644)
	x := bindPlan(t, sim, h3, dir, "k1").Hash()
	os.WriteFile(filepath.Join(dir, "services", "gitea", "post.sh"), []byte("echo B"), 0o644)
	if x == bindPlan(t, sim, h3, dir, "k1").Hash() {
		t.Fatal("the contents of a hook file count, not just its name")
	}

	// Values that come from an env_file are bound too: the manifest names the file, not its contents.
	f1 := giteaWith(svc, "3000")
	f1.EnvFile = "gitea.env"
	os.WriteFile(filepath.Join(dir, "gitea.env"), []byte("FROM_FILE=one\n"), 0o644)
	y := bindPlan(t, sim, f1, dir, "k1").Hash()
	os.WriteFile(filepath.Join(dir, "gitea.env"), []byte("FROM_FILE=two\n"), 0o644)
	if y == bindPlan(t, sim, f1, dir, "k1").Hash() {
		t.Fatal("a changed value in an env_file must change the hash")
	}

	// Without a key the plan is what it says, which is what the CLI wants.
	if bindPlan(t, sim, giteaWith(svc, "3001"), dir, "").Hash() != bindPlan(t, sim, giteaWith(svc, "3002"), dir, "").Hash() {
		t.Fatal("without a key only what the plan prints is covered")
	}
}

func giteaWith(base *config.ServiceConfig, port string) *config.ServiceConfig {
	c := *base
	c.Env = map[string]string{}
	for k, v := range base.Env {
		c.Env[k] = v
	}
	c.Env["GITEA_PORT"] = port
	return &c
}

func TestApplyPlanAppliesOnlyWhatThePlanSaysAndLogsToItsOwnLogger(t *testing.T) {
	sim, d, svc := deployedGitea(t)
	other := &config.ServiceConfig{Name: "db", Image: "postgres:16", Limits: map[string]string{"limits.cpu": "1"}}
	if err := d.DeployService(context.Background(), other, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(s *config.ServiceConfig, yml string) {
		os.MkdirAll(filepath.Join(dir, "services", s.Name), 0o755)
		os.WriteFile(filepath.Join(dir, "services", s.Name, "service.yml"), []byte(yml), 0o644)
	}
	// gitea drifted (a limit), db did not.
	write(svc, "name: gitea\nimage: gitea:latest\nvolumes:\n  - {name: gitea-data, path: /var/lib/gitea, pool: default, shifted: true}\nlimits: {limits.cpu: \"4\", limits.memory: 2GB}\nenv: {GITEA_PORT: \"3000\", GITEA_HOST: git.example.com}\nrouting: {domain: git.example.com, upstream_port: 3000}\n")
	write(other, "name: db\nimage: postgres:16\nlimits: {limits.cpu: \"1\"}\n")
	os.WriteFile(filepath.Join(dir, "fleet.yml"), []byte("name: f\n"), 0o644)
	sim.reset()

	fp, err := PlanFleet(context.Background(), sim, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if fp.Services[0].Action != ActionUpdate && fp.Services[1].Action != ActionUpdate {
		t.Fatalf("test setup: expected a pending change: %s", fp.Render())
	}

	var mu sync.Mutex
	var lines []string
	var global bytes.Buffer
	log.SetOutput(&global)
	defer log.SetOutput(os.Stderr)
	ad := newTestDeployer(sim)
	ad.SetLogger(func(format string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, a...))
		mu.Unlock()
	})
	sim.reset()
	if err := ad.ApplyPlan(context.Background(), dir, fp); err != nil {
		t.Fatal(err)
	}
	for _, c := range sim.cmds {
		if strings.Contains(c, "'db'") || strings.Contains(c, "postgres") {
			t.Fatalf("db was not in the plan's changes and must not even be looked at: %s", c)
		}
	}
	if sim.ctrs["gitea"].config["limits.cpu"] != "4" {
		t.Fatalf("what the plan said was not applied: %v", sim.ctrs["gitea"].config)
	}
	if strings.TrimSpace(global.String()) != "" {
		t.Fatalf("progress belongs to the job's logger, not the process log: %q", global.String())
	}
	if len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "gitea") {
		t.Fatalf("the job's logger got nothing: %v", lines)
	}
	if p2, _ := PlanFleet(context.Background(), sim, dir, ""); p2.Pending() {
		t.Fatalf("after apply the plan must be empty:\n%s", p2.Render())
	}

	// A replacement goes through the instance manager, which reports progress too.
	sim.aliases["gitea:latest"] = fpB
	fp2, _ := PlanFleet(context.Background(), sim, dir, "")
	if err := ad.ApplyPlan(context.Background(), dir, fp2); err != nil {
		t.Fatal(err)
	}
	if sim.ctrs["gitea"].config["volatile.base_image"] != fpB {
		t.Fatalf("test setup: the image should have been replaced")
	}
	if strings.TrimSpace(global.String()) != "" {
		t.Fatalf("the update path must report to the job's logger too: %q", global.String())
	}
}

func TestApplyPlanRefusesABlockedPlanAndStopsAtTheFirstFailure(t *testing.T) {
	sim, _, svc := deployedGitea(t)
	blocked := &FleetPlan{Services: []*ServicePlan{{Service: "gitea", Action: ActionBlocked, Blockers: []string{"x"}}}}
	if err := newTestDeployer(sim).ApplyPlan(context.Background(), t.TempDir(), blocked); err == nil || len(sim.mutations()) != 0 {
		t.Fatalf("a blocked plan applies nothing: %v %v", err, sim.mutations())
	}

	dir := t.TempDir()
	for _, name := range []string{"a", "b"} {
		os.MkdirAll(filepath.Join(dir, "services", name), 0o755)
		os.WriteFile(filepath.Join(dir, "services", name, "service.yml"), []byte("name: "+name+"\nimage: gitea:latest\nlimits: {limits.cpu: \"1\"}\n"), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "fleet.yml"), []byte("name: f\n"), 0o644)
	sim.reset()
	fp, err := PlanFleet(context.Background(), sim, dir, "")
	if err != nil || len(fp.Services) != 2 {
		t.Fatalf("%v %v", fp, err)
	}
	sim.fail = func(cmd string) error {
		if strings.HasPrefix(cmd, "incus launch") && strings.Contains(cmd, "'a'") {
			return errors.New("launch failed")
		}
		return nil
	}
	sim.reset()
	err = newTestDeployer(sim).ApplyPlan(context.Background(), dir, fp)
	if err == nil || !strings.Contains(err.Error(), "apply a") {
		t.Fatalf("the first failure is reported with its service: %v", err)
	}
	if sim.ctrs["b"] != nil {
		t.Fatal("services after the failure must be left alone")
	}
	_ = svc
}
