package config

import (
	"os"
	"path/filepath"
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
