package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/remote"
)

// env_from: a service's secret values come from the daemon's secret store, never from git. The plan names
// the key and the secret it comes from, never the value; a missing secret blocks the plan and stops apply;
// the value is bound into the plan's hash.

const sentinelSecret = "SENTINEL-SECRET-5e1d"

func storeOf(vals map[string]string) SecretLookup {
	return func(name string) (string, bool) {
		v, ok := vals[name]
		return v, ok
	}
}

func withEnvFrom(svc *config.ServiceConfig) *config.ServiceConfig {
	c := *svc
	c.EnvFrom = map[string]string{"API_KEY": "SERVICE_GITEA_API_KEY"}
	return &c
}

func planWithSecrets(t *testing.T, sim *hostSim, svc *config.ServiceConfig, lookup SecretLookup, key string) *ServicePlan {
	t.Helper()
	d := NewDeployer(remote.ReadOnly(sim))
	WithSecrets(lookup)(d)
	if key != "" {
		WithBindKey([]byte(key))(d)
	}
	p, err := d.PlanService(context.Background(), svc, t.TempDir())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p
}

func TestEnvFromIsPlannedByNameAndSourceNeverByValue(t *testing.T) {
	sim, _, svc := deployedGitea(t)
	p := planWithSecrets(t, sim, withEnvFrom(svc), storeOf(map[string]string{"SERVICE_GITEA_API_KEY": sentinelSecret}), "k")

	d := detailOf(p, ChangeSetEnv)
	if !strings.Contains(d, "add API_KEY (from secret SERVICE_GITEA_API_KEY)") {
		t.Fatalf("the plan should say the key and which secret fills it: %q", d)
	}
	js, _ := json.Marshal(p)
	if text := (&FleetPlan{Services: []*ServicePlan{p}}).Render(); strings.Contains(text, sentinelSecret) || strings.Contains(string(js), sentinelSecret) {
		t.Fatalf("the plan leaked the secret's value:\n%s", text)
	}
	if len(p.Blockers) != 0 {
		t.Fatalf("a secret that is set must not block: %v", p.Blockers)
	}

	// A service that does not exist yet says the same.
	fresh := newHostSim(t)
	fresh.aliases["gitea:latest"] = fpA
	p2 := planWithSecrets(t, fresh, withEnvFrom(giteaSvc()), storeOf(map[string]string{"SERVICE_GITEA_API_KEY": sentinelSecret}), "k")
	if d := detailOf(p2, ChangeSetEnv); !strings.Contains(d, "API_KEY (from secret SERVICE_GITEA_API_KEY)") || strings.Contains(d, sentinelSecret) {
		t.Fatalf("create plan: %q", d)
	}
}

func TestAMissingSecretBlocksThePlanAndStopsApply(t *testing.T) {
	sim, _, svc := deployedGitea(t)
	p := planWithSecrets(t, sim, withEnvFrom(svc), storeOf(nil), "k")
	if p.Action != ActionBlocked || len(p.Blockers) == 0 || !strings.Contains(p.Blockers[0], "SERVICE_GITEA_API_KEY") {
		t.Fatalf("a missing secret must block the plan and name the secret: action=%s blockers=%v", p.Action, p.Blockers)
	}

	before := sim.ctrs["gitea"].files["/etc/default/gitea"]
	d := newTestDeployer(sim)
	d.SetSecrets(storeOf(nil))
	err := d.DeployService(context.Background(), withEnvFrom(svc), t.TempDir())
	var missing *MissingSecretsError
	if !errors.As(err, &missing) || missing.Names[0] != "SERVICE_GITEA_API_KEY" {
		t.Fatalf("apply must stop with the missing secret's name, got %v", err)
	}
	if sim.ctrs["gitea"].files["/etc/default/gitea"] != before {
		t.Fatal("apply wrote the environment file although a secret was missing")
	}
}

func TestApplyWritesTheSecretValueIntoTheServiceEnvironment(t *testing.T) {
	sim, _, svc := deployedGitea(t)
	d := newTestDeployer(sim)
	d.SetSecrets(storeOf(map[string]string{"SERVICE_GITEA_API_KEY": sentinelSecret}))
	if err := d.DeployService(context.Background(), withEnvFrom(svc), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if file := sim.ctrs["gitea"].files["/etc/default/gitea"]; !strings.Contains(file, "API_KEY="+sentinelSecret+"\n") {
		t.Fatalf("the secret's value should be in /etc/default/gitea:\n%s", file)
	}
	// Once there, a second plan has nothing to do.
	p := planWithSecrets(t, sim, withEnvFrom(svc), storeOf(map[string]string{"SERVICE_GITEA_API_KEY": sentinelSecret}), "")
	if detailOf(p, ChangeSetEnv) != "" {
		t.Fatalf("an applied secret should not be planned again: %q", detailOf(p, ChangeSetEnv))
	}
}

func TestAChangedSecretChangesThePlanHash(t *testing.T) {
	sim, _, svc := deployedGitea(t)
	svc = withEnvFrom(svc)
	a := &FleetPlan{Services: []*ServicePlan{planWithSecrets(t, sim, svc, storeOf(map[string]string{"SERVICE_GITEA_API_KEY": "one"}), "k")}}
	b := &FleetPlan{Services: []*ServicePlan{planWithSecrets(t, sim, svc, storeOf(map[string]string{"SERVICE_GITEA_API_KEY": "two"}), "k")}}
	if a.Render() != b.Render() {
		t.Fatalf("test setup: the plans should read the same:\n%s\n%s", a.Render(), b.Render())
	}
	if a.Hash() == b.Hash() {
		t.Fatal("a rotated secret must change the hash, so an approval of one value cannot apply another")
	}
}

func TestWithoutADaemonEnvFromReadsTheProcessEnvironment(t *testing.T) {
	sim, _, svc := deployedGitea(t)
	t.Setenv("SERVICE_GITEA_API_KEY", sentinelSecret)
	d := newTestDeployer(sim) // no SetSecrets: a host-local apply
	if err := d.DeployService(context.Background(), withEnvFrom(svc), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sim.ctrs["gitea"].files["/etc/default/gitea"], "API_KEY="+sentinelSecret) {
		t.Fatal("a host-local apply should take env_from values from its environment")
	}
}
