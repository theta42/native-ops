package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
)

// shareTree writes a config tree: fleet.yml (body) and services/<name>/service.yml for each entry.
func shareTree(t *testing.T, fleet string, services map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("fleet.yml", fleet)
	write("services/.keep", "")
	for name, body := range services {
		write(filepath.Join("services", name, "service.yml"), body)
	}
	return dir
}

const singleHostFleet = "name: f\nhosts:\n  only:\n    provider: static\n    address: 192.0.2.10\n"

var singleHostServices = map[string]string{
	"web": "name: web\nimage: gitea:latest\nlimits: {limits.cpu: \"1\"}\nenv: {PORT: \"80\"}\nrouting: {domain: web.example.com, upstream_port: 80}\n",
	"db":  "name: db\nimage: postgres:16\nvolumes:\n  - {name: db-data, path: /var/lib/postgresql}\n",
}

// What the plan of singleHostServices was before services could name a host (taken from main without
// it, with the same tree, host state and key; regenerated after #92 moved an OCI image's env to
// environment.*). A single-host fleet must plan exactly as it did:
// the same text, the same JSON and the same hash, so no approval goes stale on upgrade.
const (
	goldenSingleHostRender = `+ db (create)
    create-instance        launch db from docker:postgres:16 with profiles base,service
    create-volume          db-data on pool default
    attach-volume          db-data at /var/lib/postgresql
+ web (create)
    create-instance        launch web from docker:gitea:latest with profiles base,service and limits.cpu=1
    set-env                write environment.* with 1 keys: PORT
    publish-route          web.example.com -> web:80 (the address is assigned when the instance is launched)

Plan: 2 to create, 0 to update, 0 unchanged, 0 blocked.
`
	goldenSingleHostJSON = `{"services":[{"service":"db","action":"create","changes":[{"kind":"create-instance","detail":"launch db from docker:postgres:16 with profiles base,service"},{"kind":"create-volume","detail":"db-data on pool default"},{"kind":"attach-volume","detail":"db-data at /var/lib/postgresql"}]},{"service":"web","action":"create","changes":[{"kind":"create-instance","detail":"launch web from docker:gitea:latest with profiles base,service and limits.cpu=1"},{"kind":"set-env","detail":"write environment.* with 1 keys: PORT"},{"kind":"publish-route","detail":"web.example.com -\u003e web:80 (the address is assigned when the instance is launched)"}]}],"network":{}}`
	goldenSingleHostHash = "d172ffc01717b1ab49a48b381bcfe082930c0f1a4f82d56a0b3ce5beea341c12"
)

func TestASingleHostFleetPlansByteForByteAsBefore(t *testing.T) {
	dir := shareTree(t, singleHostFleet, singleHostServices)
	// Not told its host, told it, or a fleet.yml with no hosts: at all: one plan.
	noHosts := shareTree(t, "name: f\n", singleHostServices)
	for name, c := range map[string]struct {
		dir  string
		opts []PlanOption
	}{
		"untold":   {dir, nil},
		"told":     {dir, []PlanOption{WithHostName("only")}},
		"no hosts": {noHosts, nil},
	} {
		sim := newHostSim(t)
		fp, err := PlanFleet(context.Background(), sim, c.dir, "", append([]PlanOption{WithBindKey([]byte("k"))}, c.opts...)...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := json.Marshal(fp)
		if got := fp.Render(); got != goldenSingleHostRender {
			t.Fatalf("%s: the text changed:\n%s", name, got)
		}
		if string(b) != goldenSingleHostJSON {
			t.Fatalf("%s: the JSON changed:\n%s", name, b)
		}
		if fp.Hash() != goldenSingleHostHash {
			t.Fatalf("%s: the hash changed: %s", name, fp.Hash())
		}
	}
	// A single-host daemon told it is another host is refused, not quietly treated as the only one.
	if _, err := PlanFleet(context.Background(), newHostSim(t), dir, "", WithHostName("other")); !errors.Is(err, ErrBadConfig) || !strings.Contains(err.Error(), `"only"`) {
		t.Fatalf("the wrong host name: %v", err)
	}
}

// Each host has its own bridge subnet (fleet.yml refuses two that overlap): h1 the fleet's 10.0.100.0/24,
// h2 10.0.101.0/24.
const twoHostFleet = "name: f\ndefault_host: h1\nhosts:\n  h1: {provider: static, address: 192.0.2.10}\n  h2: {provider: static, address: 192.0.2.11, network: {ipv4_cidr: 10.0.101.0/24}}\n"

// a has no host: (the default, h1); b is pinned to h2; c is pinned to h1.
var twoHostServices = map[string]string{
	"a": "name: a\nimage: gitea:latest\nlimits: {limits.cpu: \"1\"}\n",
	"b": "name: b\nimage: postgres:16\nhost: h2\nlimits: {limits.cpu: \"2\"}\n",
	"c": "name: c\nimage: gitea:latest\nhost: h1\nenv: {PORT: \"80\"}\n",
}

// hostSimFor is a host simulator for one of twoHostFleet's hosts, with that host's bridge.
func hostSimFor(t *testing.T, host string) *hostSim {
	sim := newHostSim(t)
	if host == "h2" {
		sim.bridges = map[string]string{"incusbr0": "10.0.101.1/24"}
	}
	return sim
}

func planShare(t *testing.T, sim *hostSim, dir, host string) *FleetPlan {
	t.Helper()
	fp, err := PlanFleet(context.Background(), sim, dir, "", WithBindKey([]byte("k")), WithHostName(host))
	if err != nil {
		t.Fatalf("plan %s: %v", host, err)
	}
	return fp
}

func actions(fp *FleetPlan) map[string]string {
	out := map[string]string{}
	for _, s := range fp.Services {
		out[s.Service] = s.Action
		if s.Action == ActionSkipped {
			out[s.Service] += ":" + s.Host
		}
	}
	return out
}

func touched(sim *hostSim, name string) bool {
	for _, c := range sim.cmds {
		if strings.Contains(c, "'"+name+"'") || strings.Contains(c, "incus list "+name+" ") {
			return true
		}
	}
	return false
}

func TestEachHostPlansOnlyItsOwnServices(t *testing.T) {
	dir := shareTree(t, twoHostFleet, twoHostServices)

	sim1 := newHostSim(t)
	p1 := planShare(t, sim1, dir, "h1")
	if got := actions(p1); got["a"] != ActionCreate || got["c"] != ActionCreate || got["b"] != "skipped:h2" || p1.Host != "h1" {
		t.Fatalf("h1's view: %v (host %q)", got, p1.Host)
	}
	if !touched(sim1, "a") {
		t.Fatalf("test setup: h1's own service is read from the host: %v", sim1.cmds)
	}
	if touched(sim1, "b") {
		t.Fatalf("h1 must not even look at h2's service: %v", sim1.cmds)
	}
	text := p1.Render()
	for _, want := range []string{"Host h1: only its own services", "- b (skipped: on host h2)\n", "+ a (create)", ", 1 on other hosts."} {
		if !strings.Contains(text, want) {
			t.Fatalf("h1's plan should say %q:\n%s", want, text)
		}
	}

	sim2 := hostSimFor(t, "h2")
	p2 := planShare(t, sim2, dir, "h2")
	if got := actions(p2); got["b"] != ActionCreate || got["a"] != "skipped:h1" || got["c"] != "skipped:h1" {
		t.Fatalf("h2's view: %v", got)
	}
	if touched(sim2, "a") || touched(sim2, "c") {
		t.Fatalf("h2 must not look at h1's services: %v", sim2.cmds)
	}

	// The two shares are two plans, each approved on its own host.
	if p1.Hash() == p2.Hash() {
		t.Fatal("the two hosts' shares must not have the same hash")
	}
	// The hash is deterministic: the same share, planned again (services in any order), is the same plan.
	again := planShare(t, newHostSim(t), dir, "h1")
	again.Services[0], again.Services[len(again.Services)-1] = again.Services[len(again.Services)-1], again.Services[0]
	if again.Hash() != p1.Hash() {
		t.Fatal("the same share must give the same hash")
	}
}

func TestAShareHashCoversOnlyThatHostsServices(t *testing.T) {
	dir := shareTree(t, twoHostFleet, twoHostServices)
	h1, h2 := planShare(t, newHostSim(t), dir, "h1").Hash(), planShare(t, hostSimFor(t, "h2"), dir, "h2").Hash()

	// A change to h2's service: h2 needs a new approval, h1 does not.
	if err := os.WriteFile(filepath.Join(dir, "services", "b", "service.yml"), []byte("name: b\nimage: postgres:16\nhost: h2\nlimits: {limits.cpu: \"4\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if planShare(t, newHostSim(t), dir, "h1").Hash() != h1 {
		t.Fatal("a change to another host's service must not change this host's plan")
	}
	if planShare(t, hostSimFor(t, "h2"), dir, "h2").Hash() == h2 {
		t.Fatal("a change to this host's service must change its plan")
	}
	// A value the plan does not print (an env value) still counts on the host that owns it.
	if err := os.WriteFile(filepath.Join(dir, "services", "c", "service.yml"), []byte("name: c\nimage: gitea:latest\nhost: h1\nenv: {PORT: \"81\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if planShare(t, newHostSim(t), dir, "h1").Hash() == h1 {
		t.Fatal("a changed env value of this host's service must change its hash")
	}
}

func TestAFleetOfSeveralHostsIsRefusedWithoutAHostName(t *testing.T) {
	dir := shareTree(t, twoHostFleet, twoHostServices)
	sim := newHostSim(t)
	_, err := PlanFleet(context.Background(), sim, dir, "", WithBindKey([]byte("k")))
	if !errors.Is(err, ErrBadConfig) || !errors.Is(err, config.ErrHostNameRequired) {
		t.Fatalf("no host name with two hosts must be refused: %v", err)
	}
	for _, want := range []string{"h1, h2", "--host-name"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal should say %q: %v", want, err)
		}
	}
	if len(sim.cmds) != 0 {
		t.Fatalf("a refused plan reads nothing from the host: %v", sim.cmds)
	}
	if _, err := PlanFleet(context.Background(), newHostSim(t), dir, "", WithHostName("h3")); !errors.Is(err, ErrBadConfig) || !strings.Contains(err.Error(), "h3") {
		t.Fatalf("a host name fleet.yml does not have: %v", err)
	}
	// A service pinned to a host fleet.yml does not have is a bad configuration, on every host.
	bad := shareTree(t, twoHostFleet, map[string]string{"x": "name: x\nimage: gitea:latest\nhost: h9\n"})
	if _, err := PlanFleet(context.Background(), newHostSim(t), bad, "", WithHostName("h1")); !errors.Is(err, ErrBadConfig) || !strings.Contains(err.Error(), "h9") {
		t.Fatalf("an unknown host: %v", err)
	}
}

func TestApplyOfAShareDoesOnlyThatShareAndIsIdempotent(t *testing.T) {
	dir := shareTree(t, twoHostFleet, twoHostServices)
	sim := newHostSim(t)
	fp := planShare(t, sim, dir, "h1")
	sim.reset()
	if err := newTestDeployer(sim).ApplyPlan(context.Background(), dir, fp); err != nil {
		t.Fatal(err)
	}
	if sim.ctrs["a"] == nil || sim.ctrs["c"] == nil {
		t.Fatalf("h1's services were not deployed: %v", sim.mutations())
	}
	if sim.ctrs["b"] != nil || touched(sim, "b") {
		t.Fatal("h2's service must never be deployed on h1")
	}

	// Planned again, nothing is pending; b is still only listed. Applied again, nothing changes.
	sim.reset()
	again := planShare(t, sim, dir, "h1")
	if again.Pending() || actions(again)["b"] != "skipped:h2" {
		t.Fatalf("after apply h1 has nothing to do:\n%s", again.Render())
	}
	if m := sim.mutations(); len(m) != 0 {
		t.Fatalf("a plan never changes the host: %v", m)
	}
	sim.reset()
	if err := newTestDeployer(sim).ApplyPlan(context.Background(), dir, again); err != nil {
		t.Fatal(err)
	}
	if m := sim.mutations(); len(m) != 0 {
		t.Fatalf("the second apply must change nothing: %v", m)
	}
	if planShare(t, newHostSim(t), dir, "h1").Hash() != fp.Hash() {
		t.Fatal("test setup: a fresh host plans the same share")
	}
}

func TestApplyRefusesAPlanThatIsNotThisHostsShare(t *testing.T) {
	dir := shareTree(t, twoHostFleet, twoHostServices)
	create := func(name string) *ServicePlan {
		return &ServicePlan{Service: name, Action: ActionCreate, Changes: []Change{{Kind: ChangeCreateInstance, Detail: "launch " + name}}}
	}
	for name, fp := range map[string]*FleetPlan{
		"no host":         {Services: []*ServicePlan{create("a")}},
		"another's share": {Host: "h1", Services: []*ServicePlan{create("b")}},
	} {
		sim := newHostSim(t)
		if err := newTestDeployer(sim).ApplyPlan(context.Background(), dir, fp); err == nil || len(sim.mutations()) != 0 {
			t.Fatalf("%s: must be refused before anything changes: %v %v", name, err, sim.mutations())
		}
	}
}

func TestOnlyTheDefaultHostDoesTheFleetsDNSAndTheEdgeHostItsCaddyfile(t *testing.T) {
	fleet := twoHostFleet + "dns_provider: fake\ndns_records:\n  - {zone: example.com, type: TXT, name: \"@\", value: hello}\n"
	dir := shareTree(t, fleet, map[string]string{
		"edge": "name: edge\nimage: edge:latest\nhost: h1\n",
		"b":    "name: b\nimage: postgres:16\nhost: h2\n",
	})
	if err := os.MkdirAll(filepath.Join(dir, "edge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "edge", "Caddyfile"), []byte("import /etc/caddy/sites/*.caddy\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// h2 has no edge instance: the Caddyfile is h1's to apply, not a blocker on h2; and dns_records are
	// the default host's.
	sim2 := hostSimFor(t, "h2")
	delete(sim2.ctrs, "edge")
	p2 := planShare(t, sim2, dir, "h2")
	if p2.Blocked() {
		t.Fatalf("h2's plan must not be blocked by h1's edge:\n%s", p2.Render())
	}
	notes := strings.Join(p2.Network.Notes, "\n")
	if !strings.Contains(notes, "applied by host h1's daemon") || !strings.Contains(notes, "default host, h1") {
		t.Fatalf("h2 should say h1 does these:\n%s", p2.Render())
	}

	// h1 (the default host) plans the records itself (here: without a provider, the usual note).
	p1 := planShare(t, newHostSim(t), dir, "h1")
	if strings.Contains(strings.Join(p1.Network.Notes, "\n"), "default host, h1") {
		t.Fatalf("h1 is the default host and plans the records:\n%s", p1.Render())
	}
}

func TestReconcileChecksServiceHostsAndWillNotDeployEveryHostsServicesToOne(t *testing.T) {
	bad := shareTree(t, twoHostFleet, map[string]string{"x": "name: x\nimage: gitea:latest\nhost: h9\n"})
	if _, err := NewReconciler(bad, nil).Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "h9") {
		t.Fatalf("validate must refuse a host fleet.yml does not have: %v", err)
	}
	dir := shareTree(t, twoHostFleet, twoHostServices)
	if _, err := NewReconciler(dir, nil).Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	sim := newHostSim(t)
	r := NewReconciler(dir, sim)
	r.DeployServices = true
	if err := r.Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "--deploy-services") || len(sim.cmds) != 0 {
		t.Fatalf("--deploy-services with two hosts is refused before anything runs: %v %v", err, sim.cmds)
	}
}
