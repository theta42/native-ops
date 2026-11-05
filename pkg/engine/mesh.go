package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/remote"
)

// The WireGuard mesh between hosts (fleet.yml `mesh:`). Each host's daemon converges its own side: a
// wg-quick interface with this host's tunnel address and a peer for every other host, whose AllowedIPs
// are that host's tunnel address and its bridge subnet, so wg-quick routes every other host's bridge
// subnet over the mesh; IP forwarding; and the ufw rules that let the mesh in, and only between the
// tunnel addresses and the bridges.
//
// The private key is generated on the host (`wg genkey`, in a script fed on stdin) and never leaves it:
// the config file has no PrivateKey line (wg-quick's PostUp sets it from the key file), the plan reads only
// whether the key file is there and the public key next to it, and no command line carries it.

// Kinds of mesh change a plan reports.
const (
	ChangeMeshKey       = "mesh-key"
	ChangeMeshInterface = "mesh-interface"
	ChangeMeshPeer      = "mesh-peer"
	ChangeMeshRoute     = "mesh-route"
	ChangeMeshForward   = "mesh-forward"
	ChangeMeshFirewall  = "mesh-firewall"
	ChangeMeshRestart   = "mesh-restart"
)

const (
	meshDir         = "/etc/wireguard"
	meshSysctlFile  = "/etc/sysctl.d/60-native-ops-mesh.conf"
	meshSysctlLine  = "net.ipv4.ip_forward = 1"
	meshRuleComment = "comment 'native-ops mesh'"
)

// MeshPlan is what apply would do to this host's side of the mesh. Like the network part it is worked out
// without changing anything, it is in the plan's hash, and apply does what it says or refuses when the
// host moved since the plan. Nil when fleet.yml has no enabled mesh.
type MeshPlan struct {
	Host      string `json:"host"`
	Interface string `json:"interface"`
	// PublicKey is this host's public key as the host has it (empty before it is generated). Public by
	// definition; it is what goes into hosts.<host>.mesh.public_key.
	PublicKey string   `json:"public_key,omitempty"`
	Changes   []Change `json:"changes,omitempty"`
	Notes     []string `json:"notes,omitempty"`
	Blockers  []string `json:"blockers,omitempty"`

	// What apply does, decided by the plan.
	genKey     bool
	conf       string // the interface's config file, when it is (re)written
	tools      bool   // install wireguard-tools
	enable     bool
	restart    bool
	forward    bool
	addRules   []string
	delRules   []string
	desiredCfg string // the config file the mesh should have, for the hash
}

func (m *MeshPlan) add(kind, format string, a ...any) {
	m.Changes = append(m.Changes, Change{Kind: kind, Detail: fmt.Sprintf(format, a...)})
}
func (m *MeshPlan) note(format string, a ...any) {
	m.Notes = append(m.Notes, fmt.Sprintf(format, a...))
}
func (m *MeshPlan) block(format string, a ...any) {
	m.Blockers = append(m.Blockers, fmt.Sprintf(format, a...))
}

// bindOrEmpty digests what the plan does not print (the whole config file, which has no secret in it), for
// the hash.
func (m *MeshPlan) bindOrEmpty() string {
	if m == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(m.desiredCfg + "\x00" + strings.Join(m.addRules, "\n") + "\x00" + strings.Join(m.delRules, "\n")))
	return hex.EncodeToString(sum[:])
}

// meshState is what the probe (remote.MeshProbeScript) read from the host.
type meshState struct {
	keyFile  bool
	pub      string
	tools    bool
	active   bool
	enabled  bool
	livePub  string
	forward  bool
	sysctlOK bool
	ufw      bool
	routes   map[string]bool
	rules    map[string]bool // the mesh's ufw rules, as specs without `ufw ` and the comment
	conf     string
}

func readMeshState(ctx context.Context, exec remote.Executor, iface string) (*meshState, error) {
	out, err := exec.RunWithInput(ctx, remote.MeshProbeCommand(iface), strings.NewReader(remote.MeshProbeScript))
	if err != nil {
		return nil, fmt.Errorf("read the mesh interface %s: %w", iface, err)
	}
	return parseMeshState(out), nil
}

func parseMeshState(out string) *meshState {
	st := &meshState{routes: map[string]bool{}, rules: map[string]bool{}}
	head, conf, found := strings.Cut(out, "\nconf\n")
	if !found {
		head = strings.TrimSuffix(out, "\nconf")
	}
	st.conf = conf
	for _, line := range strings.Split(head, "\n") {
		k, v, _ := strings.Cut(line, " ")
		v = strings.TrimSpace(v)
		switch k {
		case "keyfile":
			st.keyFile = v == "yes"
		case "pub":
			st.pub = v
		case "tools":
			st.tools = v == "yes"
		case "active":
			st.active = v == "active"
		case "enabled":
			st.enabled = v == "enabled"
		case "livepub":
			st.livePub = v
		case "forward":
			st.forward = v == "1"
		case "forwardconf":
			st.sysctlOK = v == meshSysctlLine
		case "ufw":
			st.ufw = v == "Status: active"
		case "route":
			if f := strings.Fields(v); len(f) > 0 {
				st.routes[f[0]] = true
			}
		case "rule":
			if spec, ok := strings.CutSuffix(strings.TrimPrefix(v, "ufw "), " "+meshRuleComment); ok {
				st.rules[spec] = true
			}
		}
	}
	return st
}

// PlanMesh works out what apply would do to this host's side of the mesh: nil when fleet.yml has no
// enabled mesh. host is the host this daemon is (config.DaemonHost).
func (d *Deployer) PlanMesh(ctx context.Context, fleet *config.FleetConfig, host string) (*MeshPlan, error) {
	if !fleet.MeshEnabled() {
		return nil, nil
	}
	return planMesh(ctx, d.exec, fleet, host)
}

func planMesh(ctx context.Context, exec remote.Executor, fleet *config.FleetConfig, host string) (*MeshPlan, error) {
	peers, err := fleet.MeshPeers()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadConfig, err)
	}
	iface := fleet.Mesh.MeshInterface()
	m := &MeshPlan{Host: host, Interface: iface}
	var self *config.MeshPeer
	var others []config.MeshPeer
	for i := range peers {
		if peers[i].Host == host {
			self = &peers[i]
		} else {
			others = append(others, peers[i])
		}
	}
	if self == nil {
		return nil, fmt.Errorf("%w: mesh: this daemon's host %q is not an entry of hosts:", ErrBadConfig, host)
	}
	st, err := readMeshState(ctx, exec, iface)
	if err != nil {
		return nil, err
	}
	m.PublicKey = st.pub
	keyPath := meshDir + "/" + iface + ".key"

	// The key first: nothing else is planned until the host has one and fleet.yml has its public half.
	if !st.keyFile || !config.ValidWireGuardKey(st.pub) {
		m.genKey = true
		if !st.keyFile {
			m.add(ChangeMeshKey, "generate this host's WireGuard key in %s (root-only; the private key never leaves the host) and its public key in %s/%s.pub", keyPath, meshDir, iface)
		} else {
			m.add(ChangeMeshKey, "derive this host's public key from %s into %s/%s.pub", keyPath, meshDir, iface)
		}
		m.note("after this apply, plan again: it reports this host's public key, to commit as hosts.%s.mesh.public_key; the interface is brought up once fleet.yml has every host's key", host)
		return m, nil
	}
	switch {
	case self.PublicKey == "":
		m.block("this host's mesh public key is %s; add it to fleet.yml as hosts.%s.mesh.public_key", st.pub, host)
	case self.PublicKey != st.pub:
		m.block("this host's mesh public key is %s, but hosts.%s.mesh.public_key is %s; set it to %s (a plan never replaces the host's key)", st.pub, host, self.PublicKey, st.pub)
	}
	for _, p := range others {
		if p.PublicKey == "" {
			m.block("hosts.%s.mesh.public_key is not set: apply the mesh on %s once, then commit the public key its plan reports", p.Host, p.Host)
		}
		if p.Endpoint == "" {
			m.block("host %s has no mesh endpoint (its address is unset or auto); set hosts.%s.mesh.endpoint", p.Host, p.Host)
		}
	}
	if len(m.Blockers) > 0 {
		return m, nil
	}

	subnet, _ := fleet.Mesh.MeshSubnet()
	bridge := strings.TrimSpace(fleet.HostNetwork(host).BridgeName)
	conf := meshConf(fleet, iface, host, *self, subnet.Bits(), others)
	m.desiredCfg = conf

	if st.conf != conf {
		m.conf = conf
		verb := "write"
		if st.conf != "" {
			verb = "rewrite"
		}
		m.add(ChangeMeshInterface, "%s %s/%s.conf: address %s/%d, listen port %d, peers: %d (no private key in it: wg-quick sets it from %s)",
			verb, meshDir, iface, self.Address, subnet.Bits(), fleet.Mesh.Port(), len(others), keyPath)
		for _, p := range others {
			if !strings.Contains(st.conf, peerBlock(p)) {
				m.add(ChangeMeshPeer, "+ %s: public key %s, endpoint %s, allowed-ips %s", p.Host, p.PublicKey, p.Endpoint, allowedIPs(p))
			}
		}
		for _, h := range confPeers(st.conf) {
			if !hasPeer(others, h) {
				m.add(ChangeMeshPeer, "- %s (no longer in hosts:)", h)
			}
		}
	}
	missingRoute := false
	for _, p := range others {
		if !st.active || !st.routes[p.Bridge.String()] {
			missingRoute = missingRoute || st.active
			m.add(ChangeMeshRoute, "%s (host %s's bridge) via %s", p.Bridge, p.Host, iface)
		}
	}
	if !st.tools {
		m.tools = true
	}
	if !st.enabled {
		m.enable = true
	}
	var why []string
	switch {
	case !st.active:
		why = append(why, "it is not running")
	case m.conf != "":
		why = append(why, "its config changed; the tunnel drops for a moment")
	case missingRoute:
		why = append(why, "a route over it is missing")
	case st.livePub != "" && st.livePub != st.pub:
		why = append(why, "it runs with another key than "+keyPath)
	}
	if len(why) > 0 || m.tools || m.enable {
		m.restart = len(why) > 0
		var parts []string
		if m.tools {
			parts = append(parts, "install wireguard-tools")
		}
		if m.enable {
			parts = append(parts, "enable wg-quick@"+iface+" at boot")
		}
		if m.restart {
			parts = append(parts, "(re)start wg-quick@"+iface+" ("+strings.Join(why, ", ")+")")
		}
		m.add(ChangeMeshRestart, "%s", strings.Join(parts, "; "))
	}
	if !st.forward || !st.sysctlOK {
		m.forward = true
		m.add(ChangeMeshForward, "net.ipv4.ip_forward=1, kept in %s (instances' traffic is forwarded between the bridge and %s)", meshSysctlFile, iface)
	}

	// The firewall: only ufw's own rules for the mesh, marked with a comment so they can be told from
	// every other rule. The rest of the host firewall is outside the plan (theta42/native-ops#75).
	if !st.ufw {
		m.note("ufw is not active on this host, so no mesh firewall rules are written; the host firewall is outside the plan (#75)")
		return m, nil
	}
	want := meshRules(fleet.Mesh.Port(), iface, bridge, *self, others)
	for _, r := range want {
		if !st.rules[r] {
			m.addRules = append(m.addRules, r)
			m.add(ChangeMeshFirewall, "+ ufw %s", r)
		}
	}
	wantSet := map[string]bool{}
	for _, r := range want {
		wantSet[r] = true
	}
	var stale []string
	for r := range st.rules {
		if !wantSet[r] {
			stale = append(stale, r)
		}
	}
	sort.Strings(stale)
	for _, r := range stale {
		m.delRules = append(m.delRules, r)
		m.add(ChangeMeshFirewall, "- ufw %s", r)
	}
	return m, nil
}

// meshRules is the ufw rules the mesh needs on this host: the WireGuard port, and over the mesh interface
// only the peers' tunnel addresses to this host's, and the peers' tunnel addresses and bridges to this
// host's bridge.
func meshRules(port int, iface, bridge string, self config.MeshPeer, others []config.MeshPeer) []string {
	rules := []string{fmt.Sprintf("allow %d/udp", port)}
	for _, p := range others {
		rules = append(rules,
			fmt.Sprintf("allow in on %s from %s to %s", iface, p.Address, self.Address),
			fmt.Sprintf("route allow in on %s out on %s from %s to %s", iface, bridge, p.Address, self.Bridge),
			fmt.Sprintf("route allow in on %s out on %s from %s to %s", iface, bridge, p.Bridge, self.Bridge),
		)
	}
	return rules
}

func allowedIPs(p config.MeshPeer) string {
	return fmt.Sprintf("%s/32, %s", p.Address, p.Bridge)
}

func peerBlock(p config.MeshPeer) string {
	return fmt.Sprintf("# peer %s\n[Peer]\nPublicKey = %s\nEndpoint = %s\nAllowedIPs = %s\nPersistentKeepalive = 25\n", p.Host, p.PublicKey, p.Endpoint, allowedIPs(p))
}

// meshConf is the interface's wg-quick config file. It has no PrivateKey: PostUp sets it from the key file.
func meshConf(fleet *config.FleetConfig, iface, host string, self config.MeshPeer, bits int, others []config.MeshPeer) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Written by native-ops from fleet.yml's mesh: (host %s). An apply rewrites it.\n", host)
	fmt.Fprintf(&sb, "# No private key here: PostUp sets it from %s/%s.key, which never leaves this host.\n", meshDir, iface)
	sb.WriteString("[Interface]\n")
	fmt.Fprintf(&sb, "Address = %s/%d\n", self.Address, bits)
	fmt.Fprintf(&sb, "ListenPort = %d\n", fleet.Mesh.Port())
	fmt.Fprintf(&sb, "PostUp = wg set %%i private-key %s/%%i.key\n", meshDir)
	for _, p := range others {
		sb.WriteString("\n")
		sb.WriteString(peerBlock(p))
	}
	return sb.String()
}

func confPeers(conf string) []string {
	var out []string
	for _, line := range strings.Split(conf, "\n") {
		if h, ok := strings.CutPrefix(line, "# peer "); ok {
			out = append(out, strings.TrimSpace(h))
		}
	}
	return out
}

func hasPeer(peers []config.MeshPeer, host string) bool {
	for _, p := range peers {
		if p.Host == host {
			return true
		}
	}
	return false
}

// meshKeyScript makes the host's key if it has none, and (re)writes its public half next to it. Only the
// public key is printed. Fed on stdin to `sh -s -- <interface> genkey`.
const meshKeyScript = `set -e
i="$1"
d=/etc/wireguard
if ! command -v wg >/dev/null 2>&1; then
  { apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -y -q wireguard-tools; } >&2
fi
umask 077
mkdir -p "$d"
chmod 700 "$d"
[ -s "$d/$i.key" ] || wg genkey > "$d/$i.key"
chmod 600 "$d/$i.key"
wg pubkey < "$d/$i.key" > "$d/$i.pub.new"
chmod 644 "$d/$i.pub.new"
mv "$d/$i.pub.new" "$d/$i.pub"
cat "$d/$i.pub"
`

// applyMesh does what a mesh plan says. It reads the host again and works the plan out afresh (the same
// function, so the two cannot disagree); if that is not the plan that was approved, it refuses.
func (d *Deployer) applyMesh(ctx context.Context, configDir string, m *MeshPlan) error {
	if m == nil || len(m.Changes) == 0 {
		return nil
	}
	fleet, err := config.LoadFleetConfig(configDir)
	if err != nil {
		return err
	}
	if !fleet.MeshEnabled() {
		return errors.New("the plan changes the mesh, but fleet.yml has no enabled mesh")
	}
	now, err := planMesh(ctx, d.exec, fleet, m.Host)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(now)
	b, _ := json.Marshal(m)
	if string(a) != string(b) || now.bindOrEmpty() != m.bindOrEmpty() {
		return errors.New("this host's mesh changed since the plan; plan again")
	}
	iface := now.Interface
	if now.genKey {
		out, err := d.exec.RunWithInput(ctx, "sh -s -- "+iface+" genkey", strings.NewReader(meshKeyScript))
		if err != nil {
			return fmt.Errorf("generate the mesh key: %w", err)
		}
		pub := lastLine(out)
		if !config.ValidWireGuardKey(pub) {
			return fmt.Errorf("generate the mesh key: the host did not report a public key")
		}
		d.log("    Generated this host's mesh key; its public key is %s: add it to fleet.yml as hosts.%s.mesh.public_key\n", pub, now.Host)
		return nil
	}
	if now.forward {
		if _, err := d.exec.RunWithInput(ctx, "cat > "+meshSysctlFile+" && sysctl -q -w net.ipv4.ip_forward=1", strings.NewReader(meshSysctlLine+"\n")); err != nil {
			return fmt.Errorf("mesh: IP forwarding: %w", err)
		}
		d.log("    IP forwarding on (%s)\n", meshSysctlFile)
	}
	if now.conf != "" {
		path := meshDir + "/" + iface + ".conf"
		cmd := "umask 077 && mkdir -p " + meshDir + " && cat > " + path + ".new && mv " + path + ".new " + path
		if _, err := d.exec.RunWithInput(ctx, cmd, strings.NewReader(now.conf)); err != nil {
			return fmt.Errorf("mesh: write %s: %w", path, err)
		}
		d.log("    Wrote %s\n", path)
	}
	for _, r := range now.delRules {
		if _, err := d.exec.Run(ctx, "ufw delete "+r); err != nil {
			return fmt.Errorf("mesh firewall: %w", err)
		}
	}
	for _, r := range now.addRules {
		if _, err := d.exec.Run(ctx, "ufw "+r+" "+meshRuleComment); err != nil {
			return fmt.Errorf("mesh firewall: %w", err)
		}
	}
	if len(now.addRules)+len(now.delRules) > 0 {
		d.log("    Mesh firewall: %d rules added, %d removed\n", len(now.addRules), len(now.delRules))
	}
	if now.tools {
		if _, err := d.exec.Run(ctx, "command -v wg-quick >/dev/null 2>&1 || { apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -y -q wireguard-tools; }"); err != nil {
			return fmt.Errorf("mesh: install wireguard-tools: %w", err)
		}
	}
	unit := "wg-quick@" + iface
	if now.enable {
		if _, err := d.exec.Run(ctx, "systemctl enable "+unit); err != nil {
			return fmt.Errorf("mesh: enable %s: %w", unit, err)
		}
	}
	if now.restart {
		if _, err := d.exec.Run(ctx, "systemctl restart "+unit); err != nil {
			return fmt.Errorf("mesh: start %s: %w", unit, err)
		}
		d.log("    (Re)started %s\n", unit)
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
