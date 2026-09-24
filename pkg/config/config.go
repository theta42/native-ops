package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// FleetConfig defines global fleet configuration.
type FleetConfig struct {
	Name        string            `yaml:"name"`
	Domain      string            `yaml:"domain"`
	DNSProvider string            `yaml:"dns_provider"` // "digitalocean", "proxmox", or custom plugin
	Network     NetworkConfig     `yaml:"network"`
	Providers   ProvidersConfig   `yaml:"providers"`
	Hosts       map[string]HostConfig `yaml:"hosts"`
}

type NetworkConfig struct {
	BridgeName string `yaml:"bridge_name"` // default: incusbr0
	IPv4CIDR   string `yaml:"ipv4_cidr"`    // default: 10.0.100.0/24
}

type ProvidersConfig struct {
	DigitalOcean DOConfig      `yaml:"digitalocean,omitempty"`
	Proxmox      ProxmoxConfig `yaml:"proxmox,omitempty"`
}

type DOConfig struct {
	Region    string `yaml:"region"`
	DefaultSize string `yaml:"default_size"` // e.g. "s-4vcpu-8gb"
}

type ProxmoxConfig struct {
	Endpoint   string `yaml:"endpoint"`
	Node       string `yaml:"node"`
	DefaultPool string `yaml:"default_pool"`
}

type HostConfig struct {
	Provider   string `yaml:"provider"` // "digitalocean", "proxmox", "static"
	Address    string `yaml:"address"`
	SSHUser    string `yaml:"ssh_user"`
	SSHPort    int    `yaml:"ssh_port"`
	IncusRemote string `yaml:"incus_remote"` // optional Incus remote name
}

// VolumeMount represents a storage volume attachment.
type VolumeMount struct {
	Name      string `yaml:"name"`
	Path      string `yaml:"path"`
	Pool      string `yaml:"pool,omitempty"`
	Shifted   bool   `yaml:"shifted"`   // default: true (security.shifted=true)
	ReadOnly  bool   `yaml:"read_only"`
}

// HealthCheckConfig defines how to verify a container after launch.
type HealthCheckConfig struct {
	Path     string `yaml:"path"`     // e.g. "/health"
	Port     int    `yaml:"port"`     // e.g. 8787 or 3000
	Timeout  int    `yaml:"timeout"`  // in seconds (default: 30)
	Interval int    `yaml:"interval"` // in seconds (default: 2)
}

// RoutingConfig defines Caddy edge routing rules.
type RoutingConfig struct {
	Domain       string   `yaml:"domain"`       // e.g. "git.example.com" or "*.example.com"
	UpstreamPort int      `yaml:"upstream_port"`// e.g. 3000
	TLS          string   `yaml:"tls,omitempty"`// e.g. "dns digitalocean" or "internal"
	ExtraDirectives []string `yaml:"extra_directives,omitempty"`
}

// ServiceConfig defines a static infrastructure service (e.g. gitea, plane, edge).
type ServiceConfig struct {
	Name        string            `yaml:"name"`
	Image       string            `yaml:"image"` // local alias or OCI image
	BuildDir    string            `yaml:"build_dir,omitempty"` // path relative to config repo
	Profiles    []string          `yaml:"profiles"` // e.g. ["base", "service"]
	Volumes     []VolumeMount     `yaml:"volumes"`
	Env         map[string]string `yaml:"env,omitempty"`
	EnvFile     string            `yaml:"env_file,omitempty"`
	Limits      map[string]string `yaml:"limits,omitempty"` // e.g. limits.cpu: 2
	HealthCheck HealthCheckConfig `yaml:"healthcheck,omitempty"`
	Routing     *RoutingConfig    `yaml:"routing,omitempty"`
	Hooks       ServiceHooks      `yaml:"hooks,omitempty"`
}

type ServiceHooks struct {
	PreDeploy     string `yaml:"pre_deploy,omitempty"`
	ContainerInit string `yaml:"container_init,omitempty"`
	PostDeploy    string `yaml:"post_deploy,omitempty"`
}

// TemplateConfig defines a blueprint for dynamic tenant workloads (e.g. platform instances).
type TemplateConfig struct {
	Name           string            `yaml:"name"`
	Image          string            `yaml:"image"` // base image alias
	Profiles       []string          `yaml:"profiles"`
	Volumes        []VolumeMount     `yaml:"volumes"`
	DefaultLimits  map[string]string `yaml:"default_limits,omitempty"`
	EnvTemplate    map[string]string `yaml:"env_template,omitempty"`
	HealthCheck    HealthCheckConfig `yaml:"healthcheck,omitempty"`
	RoutingPattern string            `yaml:"routing_pattern,omitempty"` // e.g. "{slug}.example.com"
	Hooks          ServiceHooks      `yaml:"hooks,omitempty"`
}

// HostSpec defines the parameters to create or resize a host VM / Droplet.
type HostSpec struct {
	Name        string            `yaml:"name"`
	Provider    string            `yaml:"provider"`
	Region      string            `yaml:"region,omitempty"`
	Size        string            `yaml:"size,omitempty"` // DO size or CPU/RAM
	Cores       int               `yaml:"cores,omitempty"`
	MemoryMB    int               `yaml:"memory_mb,omitempty"`
	DiskGB      int               `yaml:"disk_gb,omitempty"`
	Image       string            `yaml:"image,omitempty"` // e.g. "debian-13-x64" or PVE template
	SSHKeyNames []string          `yaml:"ssh_keys,omitempty"`
	UserData    string            `yaml:"user_data,omitempty"`
	Tags        []string          `yaml:"tags,omitempty"`
	FloatingIP  bool              `yaml:"floating_ip,omitempty"`
}

// LoadFleetConfig reads fleet.yml from a directory.
func LoadFleetConfig(dir string) (*FleetConfig, error) {
	path := filepath.Join(dir, "fleet.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}

	var cfg FleetConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}

	if cfg.Network.BridgeName == "" {
		cfg.Network.BridgeName = "incusbr0"
	}
	if cfg.Network.IPv4CIDR == "" {
		cfg.Network.IPv4CIDR = "10.0.100.0/24"
	}

	return &cfg, nil
}

// LoadServiceConfig reads a service.yml file.
func LoadServiceConfig(path string) (*ServiceConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read service config %s: %w", path, err)
	}

	var svc ServiceConfig
	if err := yaml.Unmarshal(data, &svc); err != nil {
		return nil, fmt.Errorf("failed to parse service config %s: %w", path, err)
	}

	return &svc, nil
}

// LoadTemplateConfig reads a template.yml file.
func LoadTemplateConfig(path string) (*TemplateConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read template config %s: %w", path, err)
	}

	var tmpl TemplateConfig
	if err := yaml.Unmarshal(data, &tmpl); err != nil {
		return nil, fmt.Errorf("failed to parse template config %s: %w", path, err)
	}

	return &tmpl, nil
}
