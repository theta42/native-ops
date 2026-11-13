package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
)

// paused is the manifest line that takes a service out of service without deleting it. These tests pin
// what went wrong without it (#paused): a stopped service has no IPv4 address, so planning it blocked the
// whole fleet's deploy, and every roll of every other service failed with "the plan is blocked".

func pausedGitea(t *testing.T) (*hostSim, *Deployer, *config.ServiceConfig) {
	t.Helper()
	sim, d, svc := deployedGitea(t)
	svc.Paused = true
	return sim, d, svc
}

func TestPausedRunningServiceIsStoppedAndAnsweredWith503(t *testing.T) {
	sim, d, svc := pausedGitea(t)
	svc.Paused = true
	p := plan(t, sim, svc)
	if len(p.Blockers) != 0 {
		t.Fatalf("a paused service must never block: %+v", p)
	}
	if !sameKinds(kindsOf(p), ChangeStop, ChangeSuspendRoute) {
		t.Fatalf("want stop and suspend-route, got %v", kindsOf(p))
	}
	if err := d.DeployService(context.Background(), svc, t.TempDir()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sim.ctrs["gitea"].status != "Stopped" {
		t.Fatalf("apply left it running")
	}
	site := sim.ctrs["edge"].files["/etc/caddy/sites/gitea.caddy"]
	if !strings.Contains(site, "This site is suspended") {
		t.Fatalf("the route must answer 503 while paused, got:\n%s", site)
	}
}

func TestPausedServiceThatIsAlreadyStoppedIsNotBlocked(t *testing.T) {
	// Exactly the `twenty` case: the instance is stopped by hand, so it has no address. Before `paused:`,
	// planning it produced a blocker and apply refused the entire plan, crew's included.
	sim, d, svc := pausedGitea(t)
	svc.Paused = true
	if err := d.PauseService(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	sim.reset()
	p := plan(t, sim, svc)
	if len(p.Blockers) != 0 {
		t.Fatalf("a paused, stopped service must not block: %+v", p)
	}
	if p.Action != ActionNone || len(p.Changes) != 0 {
		t.Fatalf("nothing left to do once it is stopped and suspended: %+v", p)
	}
}

func TestPausedServiceIsNotRolledByANewImage(t *testing.T) {
	sim, _, svc := pausedGitea(t)
	svc.Paused = true
	sim.aliases["gitea:latest"] = fpB // the fleet's tag moved
	p := plan(t, sim, svc)
	for _, k := range kindsOf(p) {
		if k == ChangeReplaceImage {
			t.Fatalf("a paused service must not roll: %v", kindsOf(p))
		}
	}
}

func TestPausedServiceIsNotCreated(t *testing.T) {
	sim := newHostSim(t)
	d := newTestDeployer(sim)
	svc := giteaSvc()
	svc.Paused = true
	p := plan(t, sim, svc)
	if p.Action != ActionNone || len(p.Blockers) != 0 {
		t.Fatalf("a paused service that does not exist is nothing to do: %+v", p)
	}
	if err := d.DeployService(context.Background(), svc, t.TempDir()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, launched := sim.ctrs["gitea"]; launched {
		t.Fatal("a paused service must not be created")
	}
}

func TestResumingAPausedServiceStartsItAndRepublishesTheRoute(t *testing.T) {
	sim, d, svc := pausedGitea(t)
	svc.Paused = true
	if err := d.PauseService(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	sim.reset()

	// `paused:` removed: the plan is a start, not a blocker, and apply brings it back.
	svc.Paused = false
	p := plan(t, sim, svc)
	if len(p.Blockers) != 0 {
		t.Fatalf("resuming must not block: %+v", p)
	}
	if !sameKinds(kindsOf(p), ChangeStart) {
		t.Fatalf("want a single start change, got %v", kindsOf(p))
	}
	if err := d.DeployService(context.Background(), svc, t.TempDir()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sim.ctrs["gitea"].status != "Running" {
		t.Fatal("apply did not start it")
	}
	site := sim.ctrs["edge"].files["/etc/caddy/sites/gitea.caddy"]
	if strings.Contains(site, "This site is suspended") {
		t.Fatalf("the route must be published again, got:\n%s", site)
	}
	if !strings.Contains(site, "reverse_proxy") {
		t.Fatalf("want a reverse_proxy route, got:\n%s", site)
	}
}
