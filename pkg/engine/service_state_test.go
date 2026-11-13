package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
)

// simStatus reads a simulated container's status the way InstanceStatus does.
func simStatus(t *testing.T, sim *hostSim, name string) (string, bool, error) {
	t.Helper()
	out, err := sim.Run(context.Background(), "incus list '"+name+"' --format json")
	if err != nil {
		return "", false, err
	}
	var rows []struct {
		Name   string
		Status string
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return "", false, err
	}
	for _, r := range rows {
		if r.Name == name {
			return r.Status, true, nil
		}
	}
	return "", false, nil
}

// A manifest that says stopped, on a running container, plans exactly one
// change: stop the service. Nothing else is touched.
func TestPlanStoppedShowsStopOnly(t *testing.T) {
	sim, _, svc := deployedGitea(t)
	svc.State = config.ServiceStateStopped
	p := plan(t, sim, svc)
	if p.Action != ActionUpdate {
		t.Fatalf("action = %q, want %q", p.Action, ActionUpdate)
	}
	if !sameKinds(kindsOf(p), ChangeStopService) {
		t.Fatalf("changes = %v, want only %q", kindsOf(p), ChangeStopService)
	}
}

// Applying the stop leaves the container stopped with its volumes and data
// intact, and a second plan is empty (idempotent).
func TestApplyStopsAndKeepsVolumes(t *testing.T) {
	sim, d, svc := deployedGitea(t)
	svc.State = config.ServiceStateStopped
	if err := d.DeployService(context.Background(), svc, t.TempDir()); err != nil {
		t.Fatalf("apply stop: %v", err)
	}
	st, exists, err := simStatus(t, sim, svc.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || st != "Stopped" {
		t.Fatalf("container status = %q exists=%v, want Stopped", st, exists)
	}
	if !sim.volumes["gitea-data"] {
		t.Fatalf("gitea-data volume is gone; a stop must never touch volumes")
	}
	p := plan(t, sim, svc)
	if p.Action != ActionNone || len(p.Changes) != 0 {
		t.Fatalf("second plan not empty: action=%q changes=%v", p.Action, kindsOf(p))
	}
}

// Absent state plans nothing new for a running container: today's behaviour,
// and today's plan hash, are unchanged.
func TestAbsentStatePlansNothing(t *testing.T) {
	sim, _, svc := deployedGitea(t)
	svc.State = ""
	p := plan(t, sim, svc)
	if p.Action != ActionNone || len(p.Changes) != 0 {
		t.Fatalf("absent state planned %v, want nothing", kindsOf(p))
	}
}

// Absent state leaves no trace in the plan: the field is omitted, so a
// manifest without state: hashes exactly as it did before the field existed.
func TestAbsentStateKeepsPlanHash(t *testing.T) {
	sim, _, svc := deployedGitea(t)
	svc.State = ""
	p := plan(t, sim, svc)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "state") {
		t.Fatalf("absent state leaks into the plan: %s", b)
	}
}

// state: running starts a stopped container.
func TestExplicitRunningStartsStopped(t *testing.T) {
	sim, d, svc := deployedGitea(t)
	stopSvc := *svc
	stopSvc.State = config.ServiceStateStopped
	if err := d.DeployService(context.Background(), &stopSvc, t.TempDir()); err != nil {
		t.Fatalf("setup stop: %v", err)
	}
	svc.State = config.ServiceStateRunning
	p := plan(t, sim, svc)
	if !sameKinds(kindsOf(p), ChangeStartService) {
		t.Fatalf("changes = %v, want only %q", kindsOf(p), ChangeStartService)
	}
	if err := d.DeployService(context.Background(), svc, t.TempDir()); err != nil {
		t.Fatalf("apply start: %v", err)
	}
	st, _, err := simStatus(t, sim, svc.Name)
	if err != nil {
		t.Fatal(err)
	}
	if st != "Running" {
		t.Fatalf("container status = %q, want Running", st)
	}
}

// A fresh service that declares stopped is created and then stopped in the
// same deploy: volumes attached and kept, no health gate failure.
func TestFreshStoppedStopsAfterCreate(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["gitea:latest"] = fpA
	d := newTestDeployer(sim)
	svc := giteaSvc()
	svc.State = config.ServiceStateStopped
	if err := d.DeployService(context.Background(), svc, t.TempDir()); err != nil {
		t.Fatalf("fresh stopped deploy: %v", err)
	}
	st, exists, err := simStatus(t, sim, svc.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || st != "Stopped" {
		t.Fatalf("container status = %q exists=%v, want a stopped container", st, exists)
	}
	if !sim.volumes["gitea-data"] {
		t.Fatalf("gitea-data volume missing after fresh stopped deploy")
	}
	p := plan(t, sim, svc)
	if p.Action != ActionNone || len(p.Changes) != 0 {
		t.Fatalf("plan after fresh stopped deploy not empty: action=%q changes=%v", p.Action, kindsOf(p))
	}
}
