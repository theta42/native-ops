package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/remote"
)

// snapshotConfigDir is a config tree whose fleet.yml has the given extra YAML (e.g. a snapshots: section).
func snapshotConfigDir(t *testing.T, fleetExtra string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fleet.yml"), []byte("name: test\n"+fleetExtra), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// planIn plans one service against the simulator with a config dir, read-only, as plan() does.
func planIn(t *testing.T, sim *hostSim, svc *config.ServiceConfig, dir string) *ServicePlan {
	t.Helper()
	before := len(sim.cmds)
	d, spy := planDeployer(sim)
	p, err := d.PlanService(context.Background(), svc, dir)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(spy.refused) != 0 {
		t.Fatalf("plan tried to change the host:\n%s", strings.Join(spy.refused, "\n"))
	}
	for _, c := range sim.cmds[before:] {
		if !remote.IsReadOnly(c) || simMutatorRe.MatchString(c) {
			t.Fatalf("plan issued a command that changes the host: %s", c)
		}
	}
	return p
}

const fleetDailySnapshots = "snapshots:\n  schedule: \"@daily\"\n  expiry: 7d\n"

func TestFleetSnapshotDefaultIsPlannedAppliedAndThenLeftAlone(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["gitea:latest"] = fpA
	dir := snapshotConfigDir(t, fleetDailySnapshots)
	svc := giteaSvc()

	// A fresh service: the plan says its volume gets the schedule.
	p := planIn(t, sim, svc, dir)
	if got := detailOf(p, ChangeSetSnapshots); !strings.Contains(got, "gitea-data") || !strings.Contains(got, "snapshots.schedule: (unset) -> @daily") ||
		!strings.Contains(got, "snapshots.expiry: (unset) -> 7d") || strings.Contains(got, "snapshots.pattern") {
		t.Fatalf("plan of a fresh service: %q (%+v)", got, p.Changes)
	}

	d := newTestDeployer(sim)
	if err := d.DeployService(context.Background(), svc, dir); err != nil {
		t.Fatal(err)
	}
	if c := sim.volCfg["gitea-data"]; c["snapshots.schedule"] != "@daily" || c["snapshots.expiry"] != "7d" {
		t.Fatalf("volume config after apply: %v", c)
	}

	// Applied: the plan has nothing left, and applying again changes nothing.
	sim.reset()
	if p := planIn(t, sim, svc, dir); p.Action != ActionNone || detailOf(p, ChangeSetSnapshots) != "" {
		t.Fatalf("after apply the plan must be empty: %s %+v", p.Action, p.Changes)
	}
	if err := d.DeployService(context.Background(), svc, dir); err != nil {
		t.Fatal(err)
	}
	if m := sim.mutations(); len(m) != 0 {
		t.Fatalf("a second apply must change nothing:\n%s", strings.Join(m, "\n"))
	}
}

func TestAVolumesOwnPolicyReplacesTheFleetDefaultAndIsConvergedInPlace(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["gitea:latest"] = fpA
	dir := snapshotConfigDir(t, fleetDailySnapshots)
	svc := giteaSvc()
	d := newTestDeployer(sim)
	if err := d.DeployService(context.Background(), svc, dir); err != nil {
		t.Fatal(err)
	}
	sim.reset()

	// The volume declares its own: hourly, a pattern, and no expiry (so the fleet's 7d is unset).
	svc.Volumes[0].Snapshots = &config.SnapshotPolicy{Schedule: "0 * * * *", Pattern: "auto-%d"}
	p := planIn(t, sim, svc, dir)
	got := detailOf(p, ChangeSetSnapshots)
	for _, want := range []string{"snapshots.schedule: @daily -> 0 * * * *", "snapshots.expiry: 7d -> (unset)", "snapshots.pattern: (unset) -> auto-%d"} {
		if !strings.Contains(got, want) {
			t.Fatalf("plan detail %q lacks %q", got, want)
		}
	}
	if p.Action != ActionUpdate || !sameKinds(kindsOf(p), ChangeSetSnapshots) {
		t.Fatalf("only the snapshot config changes: %s %v", p.Action, kindsOf(p))
	}

	sim.reset()
	if err := d.DeployService(context.Background(), svc, dir); err != nil {
		t.Fatal(err)
	}
	for _, m := range sim.mutations() {
		if !strings.HasPrefix(m, "incus storage volume set ") && !strings.HasPrefix(m, "incus storage volume unset ") {
			t.Fatalf("a schedule change must touch only the volume's config, but issued %s", m)
		}
	}
	if c := sim.volCfg["gitea-data"]; c["snapshots.schedule"] != "0 * * * *" || c["snapshots.pattern"] != "auto-%d" || c["snapshots.expiry"] != "" {
		t.Fatalf("volume config: %v", c)
	}
	sim.reset()
	if p := planIn(t, sim, svc, dir); p.Action != ActionNone {
		t.Fatalf("converged, the plan is empty: %+v", p.Changes)
	}

	// `snapshots: {}` on the volume turns scheduled snapshots off, whatever the fleet default says.
	svc.Volumes[0].Snapshots = &config.SnapshotPolicy{}
	if err := d.DeployService(context.Background(), svc, dir); err != nil {
		t.Fatal(err)
	}
	if c := sim.volCfg["gitea-data"]; len(c) != 0 {
		t.Fatalf("an empty policy unsets every snapshot key: %v", c)
	}
}

func TestWithoutASnapshotPolicyNothingAboutVolumeConfigChanges(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["gitea:latest"] = fpA
	dir := snapshotConfigDir(t, "") // no snapshots: anywhere
	svc := giteaSvc()
	d := newTestDeployer(sim)
	if err := d.DeployService(context.Background(), svc, dir); err != nil {
		t.Fatal(err)
	}
	// Someone set a schedule by hand: with no policy declared it is not ours to change.
	sim.volCfg["gitea-data"] = map[string]string{"snapshots.schedule": "@weekly"}
	sim.reset()
	if p := planIn(t, sim, svc, dir); p.Action != ActionNone || detailOf(p, ChangeSetSnapshots) != "" {
		t.Fatalf("no policy, no plan change: %+v", p.Changes)
	}
	if err := d.DeployService(context.Background(), svc, dir); err != nil {
		t.Fatal(err)
	}
	if m := sim.mutations(); len(m) != 0 {
		t.Fatalf("no policy must change nothing:\n%s", strings.Join(m, "\n"))
	}
	if sim.volCfg["gitea-data"]["snapshots.schedule"] != "@weekly" {
		t.Fatal("a hand-set schedule must be left alone when nothing is declared")
	}
}

func TestSnapshotScheduleShowsInTheFleetPlanAndApplyPlanConvergesIt(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["gitea:latest"] = fpA
	dir := snapshotConfigDir(t, fleetDailySnapshots)
	svc := giteaSvc()
	d := newTestDeployer(sim)
	if err := d.DeployService(context.Background(), svc, dir); err != nil {
		t.Fatal(err)
	}
	// Drift on the host (the schedule was edited by hand), and a services/ tree for PlanFleet.
	sim.volCfg["gitea-data"]["snapshots.schedule"] = "@hourly"
	writeServiceYAML(t, dir, "gitea", `name: gitea
image: gitea:latest
volumes:
  - name: gitea-data
    path: /var/lib/gitea
    pool: default
healthcheck: {path: /, port: 3000}
routing: {domain: git.example.com, upstream_port: 3000}
env: {GITEA_PORT: "3000", GITEA_HOST: git.example.com}
limits: {limits.cpu: "2", limits.memory: 2GB}
`)
	sim.reset()
	fp, err := PlanFleet(context.Background(), sim, dir, "gitea")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fp.Render(), "set-snapshots") || !strings.Contains(fp.Render(), "@hourly -> @daily") || !fp.Pending() {
		t.Fatalf("the fleet plan must show the schedule drift:\n%s", fp.Render())
	}
	if err := newTestDeployer(sim).ApplyPlan(context.Background(), dir, fp); err != nil {
		t.Fatal(err)
	}
	if sim.volCfg["gitea-data"]["snapshots.schedule"] != "@daily" {
		t.Fatalf("apply did not converge the schedule: %v", sim.volCfg["gitea-data"])
	}
	again, err := PlanFleet(context.Background(), sim, dir, "gitea")
	if err != nil {
		t.Fatal(err)
	}
	if again.Pending() {
		t.Fatalf("after apply nothing is pending:\n%s", again.Render())
	}
}

func writeServiceYAML(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, "services", name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "service.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
