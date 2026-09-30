package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFleetConfig(t *testing.T) {
	tmpDir := t.TempDir()
	fleetYAML := `
name: test-fleet
domain: example.com
dns_provider: digitalocean
network:
  bridge_name: incusbr0
  ipv4_cidr: 10.0.100.0/24
providers:
  digitalocean:
    region: nyc1
    default_size: s-4vcpu-8gb
`
	if err := os.WriteFile(filepath.Join(tmpDir, "fleet.yml"), []byte(fleetYAML), 0644); err != nil {
		t.Fatalf("write fleet.yml: %v", err)
	}

	cfg, err := LoadFleetConfig(tmpDir)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}

	if cfg.Name != "test-fleet" {
		t.Errorf("expected name 'test-fleet', got '%s'", cfg.Name)
	}
	if cfg.Domain != "example.com" {
		t.Errorf("expected domain 'example.com', got '%s'", cfg.Domain)
	}
	if cfg.DNSProvider != "digitalocean" {
		t.Errorf("expected DNS provider 'digitalocean', got '%s'", cfg.DNSProvider)
	}
}

func TestLoadServiceConfig(t *testing.T) {
	tmpDir := t.TempDir()
	svcYAML := `
name: gitea
image: opsavor-gitea:latest
profiles:
  - base
  - service
volumes:
  - name: gitea-data
    path: /var/lib/gitea
    shifted: true
healthcheck:
  path: /
  port: 3000
  timeout: 30
routing:
  domain: git.example.com
  upstream_port: 3000
forwards:
  - name: ssh-git
    listen: tcp:0.0.0.0:2222
    connect: tcp:127.0.0.1:2222
`
	svcPath := filepath.Join(tmpDir, "service.yml")
	if err := os.WriteFile(svcPath, []byte(svcYAML), 0644); err != nil {
		t.Fatalf("write service.yml: %v", err)
	}

	svc, err := LoadServiceConfig(svcPath)
	if err != nil {
		t.Fatalf("LoadServiceConfig: %v", err)
	}

	if svc.Name != "gitea" {
		t.Errorf("expected name 'gitea', got '%s'", svc.Name)
	}
	if len(svc.Volumes) != 1 || svc.Volumes[0].Name != "gitea-data" {
		t.Errorf("unexpected volumes: %+v", svc.Volumes)
	}
	if svc.Routing.Domain != "git.example.com" {
		t.Errorf("expected routing domain 'git.example.com', got '%s'", svc.Routing.Domain)
	}
	if len(svc.Forwards) != 1 || svc.Forwards[0].Name != "ssh-git" || svc.Forwards[0].Listen != "tcp:0.0.0.0:2222" {
		t.Errorf("unexpected forwards: %+v", svc.Forwards)
	}
}

func TestLoadTemplateConfig(t *testing.T) {
	tmpDir := t.TempDir()
	tmplYAML := `
name: platform
image: opsavor-platform:latest
profiles:
  - base
  - service
volumes:
  - name: "{slug}-data"
    path: /app/.data
    shifted: true
routing_pattern: "{slug}.example.com"
healthcheck:
  path: /health
  port: 8787
`
	tmplPath := filepath.Join(tmpDir, "template.yml")
	if err := os.WriteFile(tmplPath, []byte(tmplYAML), 0644); err != nil {
		t.Fatalf("write template.yml: %v", err)
	}

	tmpl, err := LoadTemplateConfig(tmplPath)
	if err != nil {
		t.Fatalf("LoadTemplateConfig: %v", err)
	}

	if tmpl.Name != "platform" {
		t.Errorf("expected name 'platform', got '%s'", tmpl.Name)
	}
	if tmpl.RoutingPattern != "{slug}.example.com" {
		t.Errorf("expected routing pattern '{slug}.example.com', got '%s'", tmpl.RoutingPattern)
	}
}

func TestLoadServicesReadsEveryManifestInNameOrderAndFailsBeforeAnythingIsApplied(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Join(dir, "services", name), 0o755); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			os.WriteFile(filepath.Join(dir, "services", name, "service.yml"), []byte(body), 0o644)
		}
	}
	write("web", "name: web\nimage: x\n")
	write("aaa", "image: y\n") // the directory name is the service name when the manifest has none
	write("no-manifest", "")
	svcs, err := LoadServices(dir, "")
	if err != nil || len(svcs) != 2 || svcs[0].Name != "aaa" || svcs[1].Name != "web" {
		t.Fatalf("got %v %v", svcs, err)
	}
	if one, _ := LoadServices(dir, "web"); len(one) != 1 || one[0].Name != "web" {
		t.Fatalf("only one service: %v", one)
	}
	write("aaa", "name: [broken")
	if _, err := LoadServices(dir, ""); err == nil {
		t.Fatal("a manifest that does not parse must be an error before anything is applied")
	}
}

func TestManifestPathsCannotLeaveTheConfigDirectory(t *testing.T) {
	for _, ok := range []string{"env", "services/web/.env", "./a/b", "a/../b"} {
		if !RelativeInside(ok) {
			t.Errorf("%q stays inside and should be allowed", ok)
		}
	}
	for _, bad := range []string{"", "/etc/passwd", "..", "../x", "a/../../x", "a/../..", "x\x00y"} {
		if RelativeInside(bad) {
			t.Errorf("%q escapes and must be refused", bad)
		}
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "service.yml")
	os.WriteFile(f, []byte("name: web\nimage: x\nenv_file: ../../etc/shadow\n"), 0o644)
	if _, err := LoadServiceConfig(f); err == nil || !strings.Contains(err.Error(), "env_file") {
		t.Fatalf("an env_file outside the config directory must be refused when the manifest is loaded, got %v", err)
	}
	os.WriteFile(f, []byte("name: web\nimage: x\nenv_file: /etc/shadow\n"), 0o644)
	if _, err := LoadServiceConfig(f); err == nil {
		t.Fatal("an absolute env_file must be refused")
	}
	os.WriteFile(f, []byte("name: web\nimage: x\nenv_file: env\n"), 0o644)
	if _, err := LoadServiceConfig(f); err != nil {
		t.Fatalf("an env_file inside is fine: %v", err)
	}
}

func TestPortForwardDeviceProps(t *testing.T) {
	props, err := (PortForward{Name: "ssh-git", Listen: "0.0.0.0:2222", Connect: "127.0.0.1:2222"}).DeviceProps()
	if err != nil {
		t.Fatalf("DeviceProps: %v", err)
	}
	if props["type"] != "proxy" || props["listen"] != "tcp:0.0.0.0:2222" || props["connect"] != "tcp:127.0.0.1:2222" {
		t.Errorf("unexpected props: %+v", props)
	}
	if p, err := (PortForward{Name: "dns", Protocol: "udp", Listen: "0.0.0.0:53", Connect: "127.0.0.1:53"}).DeviceProps(); err != nil || p["listen"] != "udp:0.0.0.0:53" {
		t.Errorf("udp forward: %v %+v", err, p)
	}
	for _, bad := range []PortForward{
		{Name: "bad name", Listen: "0.0.0.0:1", Connect: "127.0.0.1:1"},
		{Name: "x", Listen: "0.0.0.0:1; rm -rf /", Connect: "127.0.0.1:1"},
		{Name: "x", Protocol: "sctp", Listen: "0.0.0.0:1", Connect: "127.0.0.1:1"},
		{Name: "x", Listen: "", Connect: "127.0.0.1:1"},
	} {
		if _, err := bad.DeviceProps(); err == nil {
			t.Errorf("expected %+v to be refused", bad)
		}
	}
}
