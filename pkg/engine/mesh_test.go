package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/remote"
)

// meshSim is one host's side of the mesh: its WireGuard key files, the wg-quick unit, its routes, IP
// forwarding and ufw's mesh rules. It answers the probe and the commands apply runs, and hands everything
// else to a hostSim (the bridge check). It records every command with its stdin, so a test can check the
// private key is never in either.
type meshSim struct {
	*hostSim
	host     string
	priv     string // the private key: only ever in this field, as on a host it is only in the key file
	pub      string
	tools    bool
	active   bool
	enabled  bool
	livePub  string
	forward  bool
	sysctl   string
	ufw      bool
	routes   map[string]bool
	rules    map[string]bool
	conf     string
	restarts int
	log      []string // every command, and every stdin, in order
}

func newMeshSim(t *testing.T, host string) *meshSim {
	return &meshSim{hostSim: hostSimFor(t, host), host: host, ufw: true, routes: map[string]bool{}, rules: map[string]bool{}}
}

var _ remote.Executor = (*meshSim)(nil)

// meshMutatorRe is the commands that change this host's mesh.
var meshMutatorRe = regexp.MustCompile(`genkey$|^cat > /etc/sysctl|^umask 077|^ufw |^command -v wg-quick|^systemctl (enable|restart) `)

func (s *meshSim) Run(ctx context.Context, cmd string) (string, error) {
	s.log = append(s.log, cmd)
	switch {
	case strings.HasPrefix(cmd, "ufw delete "):
		delete(s.rules, strings.TrimPrefix(cmd, "ufw delete "))
	case strings.HasPrefix(cmd, "ufw "):
		spec, ok := strings.CutSuffix(strings.TrimPrefix(cmd, "ufw "), " "+meshRuleComment)
		if !ok {
			return "", fmt.Errorf("a mesh rule without its comment: %s", cmd)
		}
		s.rules[spec] = true
	case strings.HasPrefix(cmd, "command -v wg-quick"):
		s.tools = true
	case cmd == "systemctl enable wg-quick@wg0":
		s.enabled = true
	case cmd == "systemctl restart wg-quick@wg0":
		if !s.tools || s.priv == "" || s.conf == "" {
			return "", errors.New("wg-quick: not set up")
		}
		s.restarts++
		s.active, s.livePub = true, s.pub
		s.routes = map[string]bool{}
		for _, line := range strings.Split(s.conf, "\n") {
			if v, ok := strings.CutPrefix(line, "AllowedIPs = "); ok {
				for _, ip := range strings.Split(v, ", ") {
					if !strings.HasSuffix(ip, "/32") {
						s.routes[ip] = true
					}
				}
			}
		}
	default:
		return s.hostSim.Run(ctx, cmd)
	}
	return "", nil
}

func (s *meshSim) RunWithInput(ctx context.Context, cmd string, in io.Reader) (string, error) {
	b, _ := io.ReadAll(in)
	body := string(b)
	s.log = append(s.log, cmd, body)
	switch {
	case cmd == "sh -s -- wg0" && body == remote.MeshProbeScript:
		return s.probe(), nil
	case cmd == "sh -s -- wg0 genkey" && body == meshKeyScript:
		if s.priv == "" {
			s.priv = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%-32s", "private key of "+s.host)))
		}
		s.tools = true
		s.pub = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%-32s", "public key of "+s.host)))
		return "Reading package lists...\n" + s.pub + "\n", nil
	case strings.HasPrefix(cmd, "cat > /etc/sysctl.d/60-native-ops-mesh.conf && sysctl"):
		s.sysctl, s.forward = strings.TrimSpace(body), true
	case strings.HasPrefix(cmd, "umask 077 && mkdir -p /etc/wireguard && cat > /etc/wireguard/wg0.conf.new"):
		s.conf = body
	default:
		return s.hostSim.RunWithInput(ctx, cmd, strings.NewReader(body))
	}
	return "", nil
}

func (s *meshSim) probe() string {
	yes := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "keyfile %s\npub %s\ntools %s\n", yes(s.priv != ""), s.pub, yes(s.tools))
	active, enabled := "inactive", "disabled"
	if s.active {
		active = "active"
	}
	if s.enabled {
		enabled = "enabled"
	}
	fmt.Fprintf(&sb, "active %s\nenabled %s\nlivepub %s\n", active, enabled, s.livePub)
	fwd := "0"
	if s.forward {
		fwd = "1"
	}
	fmt.Fprintf(&sb, "forward %s\nforwardconf %s\n", fwd, s.sysctl)
	if s.ufw {
		sb.WriteString("ufw Status: active\n")
	} else {
		sb.WriteString("ufw Status: inactive\n")
	}
	var routes []string
	for r := range s.routes {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	for _, r := range routes {
		fmt.Fprintf(&sb, "route %s scope link\n", r)
	}
	var rules []string
	for r := range s.rules {
		rules = append(rules, r)
	}
	sort.Strings(rules)
	for _, r := range rules {
		fmt.Fprintf(&sb, "rule ufw %s %s\n", r, meshRuleComment)
	}
	sb.WriteString("conf\n")
	sb.WriteString(s.conf)
	return sb.String()
}

func (s *meshSim) meshMutations() []string {
	var out []string
	for _, c := range s.log {
		if meshMutatorRe.MatchString(c) {
			out = append(out, c)
		}
	}
	return out
}

func (s *meshSim) resetLog() { s.log = nil; s.hostSim.reset() }

const meshSection = "mesh: {enabled: true, subnet: 10.255.0.0/24}\n"

// meshFleet is twoHostFleet with a mesh, and the hosts' public keys when given (keys[host]).
func meshFleet(keys map[string]string) string {
	f := twoHostFleet
	for _, h := range []string{"h1", "h2"} {
		if k := keys[h]; k != "" {
			f = strings.Replace(f, h+": {provider: static,", h+": {mesh: {public_key: \""+k+"\"}, provider: static,", 1)
		}
	}
	return f + meshSection
}

func writeFleetYML(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "fleet.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func planMeshShare(t *testing.T, sim *meshSim, dir string) *FleetPlan {
	t.Helper()
	fp, err := PlanFleet(context.Background(), sim, dir, "", WithBindKey([]byte("k")), WithHostName(sim.host))
	if err != nil {
		t.Fatalf("plan %s: %v", sim.host, err)
	}
	if fp.Mesh == nil {
		t.Fatalf("plan %s: an enabled mesh must be planned", sim.host)
	}
	return fp
}

func applyMeshShare(t *testing.T, sim *meshSim, dir string, fp *FleetPlan) []string {
	t.Helper()
	var logs []string
	d := newTestDeployer(sim.hostSim)
	d.exec = sim
	d.SetLogger(func(format string, a ...any) { logs = append(logs, fmt.Sprintf(format, a...)) })
	if err := d.ApplyPlan(context.Background(), dir, fp); err != nil {
		t.Fatalf("apply %s: %v", sim.host, err)
	}
	return logs
}

func meshKinds(m *MeshPlan) map[string]int {
	out := map[string]int{}
	for _, c := range m.Changes {
		out[c.Kind]++
	}
	return out
}

// The whole key exchange and convergence, on two hosts, each from its own view.
func TestTheMeshKeyExchangeAndConvergenceOnTwoHosts(t *testing.T) {
	dir := shareTree(t, meshFleet(nil), nil)
	sims := map[string]*meshSim{"h1": newMeshSim(t, "h1"), "h2": newMeshSim(t, "h2")}
	var everything []string // every plan, JSON and log, to look for the private keys in

	// 1. The first deploy: each host generates its key, and nothing else of the mesh is planned.
	for _, h := range []string{"h1", "h2"} {
		sim := sims[h]
		fp := planMeshShare(t, sim, dir)
		if fp.Blocked() || !fp.Pending() || len(fp.Mesh.Changes) != 1 || fp.Mesh.Changes[0].Kind != ChangeMeshKey {
			t.Fatalf("%s's first plan generates its key, unblocked:\n%s", h, fp.Render())
		}
		if !strings.Contains(fp.Render(), "plan again") {
			t.Fatalf("the first plan says what comes next:\n%s", fp.Render())
		}
		logs := applyMeshShare(t, sim, dir, fp)
		if sim.priv == "" || !strings.Contains(strings.Join(logs, ""), sim.pub) || !strings.Contains(strings.Join(logs, ""), "hosts."+h+".mesh.public_key") {
			t.Fatalf("%s: the key is generated and its public half logged: %v", h, logs)
		}
		j, _ := json.Marshal(fp)
		everything = append(everything, fp.Render(), string(j), strings.Join(logs, ""))
	}

	// 2. Planned again before fleet.yml has the keys: blocked, with this host's public key in the message.
	fp := planMeshShare(t, sims["h1"], dir)
	want := "this host's mesh public key is " + sims["h1"].pub + "; add it to fleet.yml as hosts.h1.mesh.public_key"
	if !fp.Blocked() || !strings.Contains(fp.Render(), want) || fp.Mesh.PublicKey != sims["h1"].pub {
		t.Fatalf("the plan must block with the key to commit:\n%s", fp.Render())
	}
	if !strings.Contains(fp.Render(), "hosts.h2.mesh.public_key is not set") {
		t.Fatalf("the peer's missing key blocks too:\n%s", fp.Render())
	}
	// Only h1's key committed: h1 is still blocked on h2's.
	writeFleetYML(t, dir, meshFleet(map[string]string{"h1": sims["h1"].pub}))
	if fp = planMeshShare(t, sims["h1"], dir); !fp.Blocked() || strings.Contains(fp.Render(), "this host's mesh public key") {
		t.Fatalf("with its own key committed, only the peer's is missing:\n%s", fp.Render())
	}

	// 3. Both keys committed: each host converges its interface, peers, routes, forwarding and firewall.
	writeFleetYML(t, dir, meshFleet(map[string]string{"h1": sims["h1"].pub, "h2": sims["h2"].pub}))
	for _, h := range []string{"h1", "h2"} {
		sim := sims[h]
		sim.resetLog()
		fp := planMeshShare(t, sim, dir)
		if fp.Blocked() {
			t.Fatalf("%s:\n%s", h, fp.Render())
		}
		if len(sim.meshMutations()) != 0 {
			t.Fatalf("a plan changes nothing: %v", sim.meshMutations())
		}
		k := meshKinds(fp.Mesh)
		if k[ChangeMeshInterface] != 1 || k[ChangeMeshPeer] != 1 || k[ChangeMeshRoute] != 1 || k[ChangeMeshForward] != 1 || k[ChangeMeshRestart] != 1 || k[ChangeMeshFirewall] != 4 {
			t.Fatalf("%s's plan: %v\n%s", h, k, fp.Render())
		}
		logs := applyMeshShare(t, sim, dir, fp)
		j, _ := json.Marshal(fp)
		everything = append(everything, fp.Render(), string(j), strings.Join(logs, ""))
		if !sim.active || !sim.enabled || !sim.forward || sim.restarts != 1 {
			t.Fatalf("%s: the interface is up and enabled, forwarding on: %+v", h, sim)
		}

		// Planned again: nothing to do. Applied again: nothing changes.
		sim.resetLog()
		again := planMeshShare(t, sim, dir)
		if again.Pending() || again.Blocked() {
			t.Fatalf("%s after apply:\n%s", h, again.Render())
		}
		applyMeshShare(t, sim, dir, again)
		if m := sim.meshMutations(); len(m) != 0 {
			t.Fatalf("%s: the second apply must change nothing: %v", h, m)
		}
	}

	// Each host's view: its own address, and the other host as its only peer, whose AllowedIPs are that
	// host's tunnel address and its bridge subnet (routed over wg0).
	h1, h2 := sims["h1"], sims["h2"]
	for _, c := range []struct {
		sim                                                       *meshSim
		addr, peer, peerKey, peerAllowed, peerEndpoint, peerRoute string
		rules                                                     []string
	}{
		{h1, "10.255.0.1/24", "h2", h2.pub, "10.255.0.2/32, 10.0.101.0/24", "192.0.2.11:51820", "10.0.101.0/24", []string{
			"allow 51820/udp", "allow in on wg0 from 10.255.0.2 to 10.255.0.1",
			"route allow in on wg0 out on incusbr0 from 10.255.0.2 to 10.0.100.0/24", "route allow in on wg0 out on incusbr0 from 10.0.101.0/24 to 10.0.100.0/24"}},
		{h2, "10.255.0.2/24", "h1", h1.pub, "10.255.0.1/32, 10.0.100.0/24", "192.0.2.10:51820", "10.0.100.0/24", []string{
			"allow 51820/udp", "allow in on wg0 from 10.255.0.1 to 10.255.0.2",
			"route allow in on wg0 out on incusbr0 from 10.255.0.1 to 10.0.101.0/24", "route allow in on wg0 out on incusbr0 from 10.0.100.0/24 to 10.0.101.0/24"}},
	} {
		for _, want := range []string{"Address = " + c.addr, "ListenPort = 51820", "# peer " + c.peer, "PublicKey = " + c.peerKey,
			"AllowedIPs = " + c.peerAllowed, "Endpoint = " + c.peerEndpoint, "PostUp = wg set %i private-key /etc/wireguard/%i.key"} {
			if !strings.Contains(c.sim.conf, want) {
				t.Fatalf("%s's wg0.conf should have %q:\n%s", c.sim.host, want, c.sim.conf)
			}
		}
		if strings.Count(c.sim.conf, "[Peer]") != 1 || strings.Contains(c.sim.conf, "PrivateKey") {
			t.Fatalf("%s's wg0.conf:\n%s", c.sim.host, c.sim.conf)
		}
		if !c.sim.routes[c.peerRoute] {
			t.Fatalf("%s routes %s over wg0: %v", c.sim.host, c.peerRoute, c.sim.routes)
		}
		if len(c.sim.rules) != len(c.rules) {
			t.Fatalf("%s's mesh rules: %v", c.sim.host, c.sim.rules)
		}
		for _, r := range c.rules {
			if !c.sim.rules[r] {
				t.Fatalf("%s should have the rule %q: %v", c.sim.host, r, c.sim.rules)
			}
		}
	}

	// The private key is never in a plan, its JSON, a log, a command line or anything fed on stdin: it
	// is generated on the host, in a script that never prints it.
	for _, sim := range sims {
		for _, s := range everything {
			if strings.Contains(s, sim.priv) {
				t.Fatalf("%s's private key leaked into a plan or log:\n%s", sim.host, s)
			}
		}
		for _, c := range sim.log {
			if strings.Contains(c, sim.priv) {
				t.Fatalf("%s's private key is in a command or its stdin: %s", sim.host, c)
			}
		}
		if strings.Contains(sim.conf, sim.priv) {
			t.Fatalf("%s's private key is in wg0.conf", sim.host)
		}
	}
}

func TestTheMeshPlanBlocksOnADifferentKeyAndFollowsAChangedPeer(t *testing.T) {
	dir := shareTree(t, meshFleet(nil), nil)
	h1, h2 := newMeshSim(t, "h1"), newMeshSim(t, "h2")
	applyMeshShare(t, h1, dir, planMeshShare(t, h1, dir))
	applyMeshShare(t, h2, dir, planMeshShare(t, h2, dir))

	// fleet.yml has another key for h1 than the one h1 has: blocked, and the message says which to set.
	writeFleetYML(t, dir, meshFleet(map[string]string{"h1": h2.pub, "h2": h2.pub}))
	fp := planMeshShare(t, h1, dir)
	want := fmt.Sprintf("this host's mesh public key is %s, but hosts.h1.mesh.public_key is %s; set it to %s", h1.pub, h2.pub, h1.pub)
	if !fp.Blocked() || !strings.Contains(fp.Render(), want) {
		t.Fatalf("a different key blocks:\n%s", fp.Render())
	}
	h1.resetLog()
	d := newTestDeployer(h1.hostSim)
	d.exec = h1
	if err := d.ApplyPlan(context.Background(), dir, fp); err == nil || len(h1.meshMutations()) != 0 {
		t.Fatalf("a blocked plan is not applied: %v %v", err, h1.meshMutations())
	}

	// Converged, then h2 moves: h1 rewrites its peer and restarts the interface; nothing else.
	writeFleetYML(t, dir, meshFleet(map[string]string{"h1": h1.pub, "h2": h2.pub}))
	applyMeshShare(t, h1, dir, planMeshShare(t, h1, dir))
	writeFleetYML(t, dir, strings.Replace(meshFleet(map[string]string{"h1": h1.pub, "h2": h2.pub}), "address: 192.0.2.11", "address: 192.0.2.99", 1))
	fp = planMeshShare(t, h1, dir)
	k := meshKinds(fp.Mesh)
	if len(k) != 3 || k[ChangeMeshInterface] != 1 || k[ChangeMeshPeer] != 1 || k[ChangeMeshRestart] != 1 || !strings.Contains(fp.Render(), "endpoint 192.0.2.99:51820") {
		t.Fatalf("a moved peer: %v\n%s", k, fp.Render())
	}
	// The host moved since the plan (someone stopped the interface): apply refuses rather than doing something else.
	h1.active = false
	if err := d.ApplyPlan(context.Background(), dir, fp); err == nil || !strings.Contains(err.Error(), "plan again") {
		t.Fatalf("a host that moved since the plan: %v", err)
	}
	h1.active = true
	applyMeshShare(t, h1, dir, fp)
	if !strings.Contains(h1.conf, "Endpoint = 192.0.2.99:51820") || h1.restarts != 2 {
		t.Fatalf("the peer is rewritten and the interface restarted: %d\n%s", h1.restarts, h1.conf)
	}

	// A rule the mesh no longer needs (another port) is removed; ufw rules without the mesh's comment are
	// never looked at.
	writeFleetYML(t, dir, strings.Replace(meshFleet(map[string]string{"h1": h1.pub, "h2": h2.pub}), "mesh: {enabled: true,", "mesh: {enabled: true, listen_port: 51821,", 1))
	fp = planMeshShare(t, h1, dir)
	if r := fp.Render(); !strings.Contains(r, "- ufw allow 51820/udp") || !strings.Contains(r, "+ ufw allow 51821/udp") {
		t.Fatalf("a changed port:\n%s", r)
	}
	applyMeshShare(t, h1, dir, fp)
	if h1.rules["allow 51820/udp"] || !h1.rules["allow 51821/udp"] {
		t.Fatalf("rules: %v", h1.rules)
	}
	if again := planMeshShare(t, h1, dir); again.Pending() {
		t.Fatalf("converged:\n%s", again.Render())
	}
}

func TestWithoutUFWTheMeshNotesTheFirewallInsteadOfWritingIt(t *testing.T) {
	dir := shareTree(t, meshFleet(nil), nil)
	h1, h2 := newMeshSim(t, "h1"), newMeshSim(t, "h2")
	h1.ufw = false
	applyMeshShare(t, h1, dir, planMeshShare(t, h1, dir))
	applyMeshShare(t, h2, dir, planMeshShare(t, h2, dir))
	writeFleetYML(t, dir, meshFleet(map[string]string{"h1": h1.pub, "h2": h2.pub}))
	fp := planMeshShare(t, h1, dir)
	if meshKinds(fp.Mesh)[ChangeMeshFirewall] != 0 || !strings.Contains(fp.Render(), "ufw is not active") {
		t.Fatalf("no ufw:\n%s", fp.Render())
	}
	applyMeshShare(t, h1, dir, fp)
	if len(h1.rules) != 0 || !h1.active {
		t.Fatalf("no ufw rules are written: %v", h1.rules)
	}
}

// With mesh: unset or disabled nothing is read or planned, and a single-host plan is byte-for-byte what it
// was on main (the golden of TestASingleHostFleetPlansByteForByteAsBefore).
func TestMeshOffChangesNothing(t *testing.T) {
	for name, fleet := range map[string]string{
		"unset":    singleHostFleet,
		"disabled": singleHostFleet + "mesh: {enabled: false, subnet: 10.255.0.0/24}\n",
	} {
		dir := shareTree(t, fleet, singleHostServices)
		sim := newHostSim(t)
		fp, err := PlanFleet(context.Background(), sim, dir, "", WithBindKey([]byte("k")))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := json.Marshal(fp)
		if fp.Mesh != nil || fp.Render() != goldenSingleHostRender || string(b) != goldenSingleHostJSON || fp.Hash() != goldenSingleHostHash {
			t.Fatalf("%s: the plan changed:\n%s\n%s\n%s", name, fp.Render(), b, fp.Hash())
		}
		for _, c := range sim.cmds {
			if strings.Contains(c, "wg") || strings.Contains(c, "ufw") {
				t.Fatalf("%s: a mesh command ran: %s", name, c)
			}
		}
	}
}

// The plan's read of the mesh goes through the read-only executor: only the exact probe script passes.
func TestTheMeshProbeIsTheOnlyScriptAPlanMayRun(t *testing.T) {
	sim := newMeshSim(t, "h1")
	ro := remote.ReadOnly(sim)
	if _, err := ro.RunWithInput(context.Background(), remote.MeshProbeCommand("wg0"), strings.NewReader(remote.MeshProbeScript)); err != nil {
		t.Fatalf("the probe is read-only: %v", err)
	}
	for _, c := range []struct{ cmd, in string }{
		{remote.MeshProbeCommand("wg0"), remote.MeshProbeScript + "rm -rf /\n"},
		{"sh -s -- wg0 genkey", meshKeyScript},
		{"sh -s -- wg0 genkey", remote.MeshProbeScript},
		{"sh -s -- 'wg0'", remote.MeshProbeScript},
	} {
		var re *remote.ReadOnlyError
		if _, err := ro.RunWithInput(context.Background(), c.cmd, strings.NewReader(c.in)); !errors.As(err, &re) {
			t.Fatalf("%q must be refused: %v", c.cmd, err)
		}
	}
	// The probe names the key file once, to test that it is there; it never reads it.
	if strings.Count(remote.MeshProbeScript, "$i.key") != 1 || !strings.Contains(remote.MeshProbeScript, `[ -s "$d/$i.key" ]`) {
		t.Fatal("the probe must never read the private key")
	}
}
