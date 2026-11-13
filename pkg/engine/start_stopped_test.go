package engine

import (
	"context"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
)

// Running is the default state: `state: running` and no `state:` at all both mean the container should be
// up. It used to be that only an explicit `state: running` planned a start, and even that plan then went on
// to the route check and reported `no IPv4 address (is it stopped?)` as a blocker - which refuses the whole
// plan. A service that was stopped by hand, or by a host reboot, or that is coming back from
// `state: stopped`, therefore dead-ended every deploy, its own and everyone else's.

func TestStoppedServiceWithoutStateIsStartedNotBlocked(t *testing.T) {
	sim, d, svc := deployedGitea(t) // no state: in the manifest
	sim.ctrs["gitea"].status, sim.ctrs["gitea"].ip = "Stopped", ""
	sim.reset()

	p := plan(t, sim, svc)
	if len(p.Blockers) != 0 {
		t.Fatalf("a stopped service must not block the plan: %+v", p)
	}
	if p.Action != ActionUpdate || !sameKinds(kindsOf(p), ChangeStartService) {
		t.Fatalf("want a single start change, got %+v", p)
	}

	if err := d.DeployService(context.Background(), svc, t.TempDir()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sim.ctrs["gitea"].status != "Running" {
		t.Fatal("apply left it stopped")
	}
	if _, ok := sim.ctrs["edge"].files["/etc/caddy/sites/gitea.caddy"]; !ok {
		t.Fatal("the route must be published again once it is up")
	}
}

func TestExplicitRunningIsNotBlockedByTheAddressItHasNotGotYet(t *testing.T) {
	// The resume path of `state:`: the service was stopped and now declares running. The start change used
	// to be followed by the no-IPv4 blocker, so removing `state: stopped` refused the deploy that was meant
	// to bring the service back.
	sim, d, svc := deployedGitea(t)
	stopSvc := *svc
	stopSvc.State = config.ServiceStateStopped
	if err := d.DeployService(context.Background(), &stopSvc, t.TempDir()); err != nil {
		t.Fatalf("setup stop: %v", err)
	}
	sim.reset()

	svc.State = config.ServiceStateRunning
	p := plan(t, sim, svc)
	if len(p.Blockers) != 0 {
		t.Fatalf("resuming must not block: %+v", p)
	}
	if !sameKinds(kindsOf(p), ChangeStartService) {
		t.Fatalf("want a single start change, got %v", kindsOf(p))
	}
	if err := d.DeployService(context.Background(), svc, t.TempDir()); err != nil {
		t.Fatalf("apply resume: %v", err)
	}
	if sim.ctrs["gitea"].status != "Running" {
		t.Fatal("the service did not come back up")
	}
}

func TestADeclaredStopStillWinsOverTheDefault(t *testing.T) {
	// The default must not undo `state: stopped`: a running service that declares stopped is stopped, and
	// nothing is started behind its back.
	sim, _, svc := deployedGitea(t)
	svc.State = config.ServiceStateStopped
	p := plan(t, sim, svc)
	if len(p.Blockers) != 0 || !sameKinds(kindsOf(p), ChangeStopService) {
		t.Fatalf("want a single stop change, got %+v", p)
	}
	for _, c := range p.Changes {
		if c.Kind == ChangeStartService {
			t.Fatalf("a stopped service must not plan a start: %+v", p)
		}
	}
}
