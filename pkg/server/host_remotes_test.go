package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/engine"
)

const remotesFleet = "name: f\ndefault_host: h1\nhosts:\n" +
	"  h1: {provider: static, incus_remote: host-one}\n" +
	"  h2: {provider: static, incus_remote: host-two, network: {ipv4_cidr: 10.0.101.0/24}}\n" +
	"  h3: {provider: static, network: {ipv4_cidr: 10.0.102.0/24}}\n"

// followRemotes records the hosts' remotes a deploy's fleet.yml declares, as a deploy does.
func followRemotes(t *testing.T, rig *instRig, fleetYML string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fleet.yml"), []byte(fleetYML), 0o644); err != nil {
		t.Fatal(err)
	}
	var log []string
	if err := rig.srv.followHostRemotes(root, func(f string, a ...any) { log = append(log, f) }); err != nil {
		t.Fatal(err)
	}
	return strings.Join(log, "\n")
}

// A migration may name its target by fleet.yml host: the daemon resolves it to the host's incus_remote,
// as the last deploy tag's fleet.yml gives it.
func TestMigrateByTargetHostGoesToItsIncusRemote(t *testing.T) {
	rig := newMigrateRig(t)
	rig.srv.opts.HostRemotesFile = filepath.Join(t.TempDir(), "host-remotes.json")
	rig.srv.opts.HostName = "h1"

	// Before any deploy recorded the hosts, target_host cannot be resolved.
	res, body := rig.do2(t, "POST", "/v1/instances/demo-old/migrate", rig.scoped, map[string]any{"target_host": "h2"})
	if res.StatusCode != 409 || !strings.Contains(body, "give target_remote") {
		t.Fatalf("no deploy yet: %d %s", res.StatusCode, body)
	}

	followRemotes(t, rig, remotesFleet)
	res, body = rig.do2(t, "POST", "/v1/instances/demo-old/migrate", rig.scoped, map[string]any{"target_host": "h2", "resume": true})
	if res.StatusCode != 202 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	rig.wait(t, body, JobSucceeded)
	if len(rig.ops.migrates) != 1 || rig.ops.migrates[0] != (engine.MigrateRequest{TargetRemote: "host-two", Resume: true}) {
		t.Fatalf("target_host h2 must reach the host as its remote host-two: %+v", rig.ops.migrates)
	}

	for name, c := range map[string]struct {
		body map[string]any
		code int
		want string
	}{
		"both":         {map[string]any{"target_host": "h2", "target_remote": "host-two"}, 400, "not both"},
		"unknown host": {map[string]any{"target_host": "h9"}, 400, "not a host of the fleet.yml"},
		"no remote":    {map[string]any{"target_host": "h3"}, 409, "hosts.h3.incus_remote"},
		"this host":    {map[string]any{"target_host": "h1"}, 400, "is this host"},
		"a bad name":   {map[string]any{"target_host": "h 2"}, 400, "target_host"},
	} {
		res, body := rig.do2(t, "POST", "/v1/instances/demo-old/migrate", rig.scoped, c.body)
		if res.StatusCode != c.code || !strings.Contains(body, c.want) {
			t.Errorf("%s: want %d %q, got %d %s", name, c.code, c.want, res.StatusCode, body)
		}
	}
	if len(rig.ops.migrates) != 1 {
		t.Fatalf("no refused request reaches the host: %+v", rig.ops.migrates)
	}

	// The role gate is the endpoint's: target_host changes nothing about who may migrate.
	for name, tok := range map[string]string{"viewer": rig.viewer, "an unscoped deployer": rig.deployer} {
		if res, body := rig.do2(t, "POST", "/v1/instances/demo-old/migrate", tok, map[string]any{"target_host": "h2"}); res.StatusCode != 403 {
			t.Errorf("%s must be refused: %d %s", name, res.StatusCode, body)
		}
	}

	// A later deploy without hosts: forgets them.
	followRemotes(t, rig, "name: f\n")
	if res, body := rig.do2(t, "POST", "/v1/instances/demo-old/migrate", rig.scoped, map[string]any{"target_host": "h2"}); res.StatusCode != 409 {
		t.Fatalf("hosts forgotten: %d %s", res.StatusCode, body)
	}
}

// A daemon that does not record the hosts refuses target_host and keeps taking target_remote.
func TestMigrateByTargetHostNeedsTheRecord(t *testing.T) {
	rig := newMigrateRig(t)
	res, body := rig.do2(t, "POST", "/v1/instances/demo-old/migrate", rig.scoped, map[string]any{"target_host": "h2"})
	if res.StatusCode != 409 || !strings.Contains(body, "does not record") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

func TestADeployTagRecordsTheHostsIncusRemotes(t *testing.T) {
	src := &fakeSource{protected: true, tree: tgz(t, entry{name: "fleet.yml", body: remotesFleet}, entry{name: "services/web/service.yml", body: "image: x\n"})}
	file := filepath.Join(t.TempDir(), "host-remotes.json")
	rig := newApplyRigWith(t, func(o *Options) {
		o.Deploy, o.DeployTags = src, "deploy-*"
		o.HostRemotesFile = file
	})
	res, body := rig.post(t, "/v1/deploy", rig.deployerToken, "application/json", []byte(`{"tag":"deploy-1"}`))
	if res.StatusCode != 202 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	rig.waitJob(t, jobID(t, body), JobSucceeded)
	b, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(b), `"h2": "host-two"`) || !strings.Contains(string(b), `"h3": ""`) {
		t.Fatalf("the deploy must record the hosts' remotes: %s %v", b, err)
	}
}
