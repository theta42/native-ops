package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// FleetConfig defines global fleet configuration.
type FleetConfig struct {
	Name        string                `yaml:"name"`
	Domain      string                `yaml:"domain"`
	DNSProvider string                `yaml:"dns_provider"` // "digitalocean", "proxmox", or custom plugin
	Network     NetworkConfig         `yaml:"network"`
	Providers   ProvidersConfig       `yaml:"providers"`
	Hosts       map[string]HostConfig `yaml:"hosts"`
	Backup      *BackupConfig         `yaml:"backup,omitempty"`
}

// BackupConfig configures off-host volume backups to any S3-compatible
// object store (DigitalOcean Spaces, MinIO, AWS S3, ...). Credentials are
// never stored here: only the names of the environment variables that hold
// the access key and secret.
type BackupConfig struct {
	// Provider is informational; "s3" (default) and "spaces" are both
	// generic S3-compatible stores.
	Provider string `yaml:"provider,omitempty"`
	// Endpoint is the S3 endpoint, e.g. https://nyc3.digitaloceanspaces.com.
	Endpoint string `yaml:"endpoint"`
	Region   string `yaml:"region"`
	Bucket   string `yaml:"bucket"`
	// Prefix is prepended to every object key (e.g. "incus/").
	Prefix string `yaml:"prefix,omitempty"`
	// PathStyle selects path-style addressing (bucket in the path). Defaults
	// to true, which is what most S3-compatible providers (incl. Spaces) use.
	PathStyle *bool `yaml:"path_style,omitempty"`
	// AccessKeyEnv / SecretKeyEnv name the environment variables holding the
	// S3 credentials. Default: BACKUP_S3_ACCESS_KEY / BACKUP_S3_SECRET_KEY.
	AccessKeyEnv string `yaml:"access_key_env,omitempty"`
	SecretKeyEnv string `yaml:"secret_key_env,omitempty"`
	// Volumes is an allowlist for `backup all`. Empty means every custom volume.
	Volumes []string `yaml:"volumes,omitempty"`
	// Retention. Zero means "keep everything".
	RetainDaily   int `yaml:"retain_daily,omitempty"`
	RetainMonthly int `yaml:"retain_monthly,omitempty"`
	// KeepLocalSnapshots keeps the transient pre-backup snapshot on the host
	// after a successful upload (default false).
	KeepLocalSnapshots bool `yaml:"keep_local_snapshots,omitempty"`
}

// ApplyDefaults fills in optional backup settings.
func (b *BackupConfig) ApplyDefaults() {
	if b.Provider == "" {
		b.Provider = "s3"
	}
	if b.AccessKeyEnv == "" {
		b.AccessKeyEnv = "BACKUP_S3_ACCESS_KEY"
	}
	if b.SecretKeyEnv == "" {
		b.SecretKeyEnv = "BACKUP_S3_SECRET_KEY"
	}
}

// S3PathStyle reports the effective addressing style (default path-style).
func (b *BackupConfig) S3PathStyle() bool { return b.PathStyle == nil || *b.PathStyle }

// Credentials resolves the S3 key pair from the configured environment
// variables. Secrets are read from the environment only, never from git.
func (b *BackupConfig) Credentials() (access, secret string, err error) {
	b.ApplyDefaults()
	access = os.Getenv(b.AccessKeyEnv)
	secret = os.Getenv(b.SecretKeyEnv)
	if access == "" || secret == "" {
		return "", "", fmt.Errorf("missing S3 credentials: set %s and %s", b.AccessKeyEnv, b.SecretKeyEnv)
	}
	return access, secret, nil
}

// ValidateForBackup checks the fields required to talk to the object store.
func (b *BackupConfig) ValidateForBackup() error {
	if b.Endpoint == "" {
		return fmt.Errorf("backup.endpoint is required")
	}
	if b.Bucket == "" {
		return fmt.Errorf("backup.bucket is required")
	}
	if b.Region == "" {
		return fmt.Errorf("backup.region is required")
	}
	return nil
}

type NetworkConfig struct {
	BridgeName string `yaml:"bridge_name"` // default: incusbr0
	IPv4CIDR   string `yaml:"ipv4_cidr"`   // default: 10.0.100.0/24
}

type ProvidersConfig struct {
	DigitalOcean DOConfig      `yaml:"digitalocean,omitempty"`
	Proxmox      ProxmoxConfig `yaml:"proxmox,omitempty"`
}

type DOConfig struct {
	Region      string `yaml:"region"`
	DefaultSize string `yaml:"default_size"` // e.g. "s-4vcpu-8gb"
}

type ProxmoxConfig struct {
	Endpoint    string `yaml:"endpoint"`
	Node        string `yaml:"node"`
	DefaultPool string `yaml:"default_pool"`
}

type HostConfig struct {
	Provider    string `yaml:"provider"` // "digitalocean", "proxmox", "static"
	Address     string `yaml:"address"`
	SSHUser     string `yaml:"ssh_user"`
	SSHPort     int    `yaml:"ssh_port"`
	IncusRemote string `yaml:"incus_remote"` // optional Incus remote name
}

// VolumeMount represents a storage volume attachment.
type VolumeMount struct {
	Name     string `yaml:"name"`
	Path     string `yaml:"path"`
	Pool     string `yaml:"pool,omitempty"`
	Shifted  bool   `yaml:"shifted"` // default: true (security.shifted=true)
	ReadOnly bool   `yaml:"read_only"`
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
	Domain          string   `yaml:"domain"`        // e.g. "git.example.com" or "*.example.com"
	UpstreamPort    int      `yaml:"upstream_port"` // e.g. 3000
	TLS             string   `yaml:"tls,omitempty"` // e.g. "dns digitalocean" or "internal"
	ExtraDirectives []string `yaml:"extra_directives,omitempty"`
}

// ServiceConfig defines a static infrastructure service (e.g. gitea, plane, edge).
type ServiceConfig struct {
	Name        string            `yaml:"name"`
	Image       string            `yaml:"image"`               // local alias or OCI image
	BuildDir    string            `yaml:"build_dir,omitempty"` // path relative to config repo
	Profiles    []string          `yaml:"profiles"`            // e.g. ["base", "service"]
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

// PreviewConfig declares how an app can be run as an ephemeral preview.
// {app} and {ref} are substituted (sanitized) from the launch arguments.
type PreviewConfig struct {
	Enabled      bool              `yaml:"enabled"`
	Image        string            `yaml:"image,omitempty"`   // e.g. "opsavor-platform:{ref}"
	Routing      string            `yaml:"routing,omitempty"` // e.g. "{app}-{ref}.preview.example.com"
	TTL          string            `yaml:"ttl,omitempty"`     // duration, e.g. "72h"
	Data         string            `yaml:"data,omitempty"`    // ephemeral | seed (informational)
	Limits       map[string]string `yaml:"limits,omitempty"`
	Env          map[string]string `yaml:"env,omitempty"`
	MaxInstances int               `yaml:"max_instances,omitempty"`
}

// TemplateConfig defines a blueprint for dynamic tenant workloads (e.g. platform instances).
type TemplateConfig struct {
	Name           string            `yaml:"name"`
	Image          string            `yaml:"image"`             // base image alias
	Service        string            `yaml:"service,omitempty"` // service/unit + /etc/default/<service> name
	Preview        *PreviewConfig    `yaml:"preview,omitempty"`
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
	Name        string   `yaml:"name"`
	Provider    string   `yaml:"provider"`
	Region      string   `yaml:"region,omitempty"`
	Size        string   `yaml:"size,omitempty"` // DO size or CPU/RAM
	Cores       int      `yaml:"cores,omitempty"`
	MemoryMB    int      `yaml:"memory_mb,omitempty"`
	DiskGB      int      `yaml:"disk_gb,omitempty"`
	Image       string   `yaml:"image,omitempty"` // e.g. "debian-13-x64" or PVE template
	SSHKeyNames []string `yaml:"ssh_keys,omitempty"`
	UserData    string   `yaml:"user_data,omitempty"`
	Tags        []string `yaml:"tags,omitempty"`
	FloatingIP  bool     `yaml:"floating_ip,omitempty"`
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
	if cfg.Backup != nil {
		cfg.Backup.ApplyDefaults()
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
