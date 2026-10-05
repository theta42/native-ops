package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

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
	// Daemon, when set, has a host that reconcile creates install the native-ops daemon from its
	// cloud-init (see engine.DaemonInstall); the bootstrap admin token comes from the environment.
	Daemon *DaemonConfig `yaml:"daemon,omitempty"`
	// DNSRecords are records kept in sync by `dns sync` (and reconcile): arbitrary A/AAAA/CNAME/MX/TXT
	// beyond the computed apex/wildcard A. Each names its own zone, so a fleet that spans zones (and
	// leaves `domain` empty) can still declare records — e.g. an MX for an intake domain.
	DNSRecords []DNSRecordConfig `yaml:"dns_records,omitempty"`
}

// DNSRecordConfig is one record declared in fleet.yml. Name is zone-relative ("@", "*", "inbound");
// Value is the record's data (a hostname for MX/CNAME, an address for A/AAAA, text for TXT); Priority
// applies to MX. TTL is optional (the provider defaults it).
type DNSRecordConfig struct {
	Zone     string `yaml:"zone,omitempty"` // the zone; empty falls back to fleet.domain
	Type     string `yaml:"type"`
	Name     string `yaml:"name"`
	Value    string `yaml:"value"`
	TTL      int    `yaml:"ttl,omitempty"`
	Priority int    `yaml:"priority,omitempty"`
}

// DNSRecordsByZone groups the declared records by zone (fleet.domain when a record omits it). It errors
// when a record has no zone at all, so a typo cannot silently sync into the wrong place.
func (f *FleetConfig) DNSRecordsByZone() (map[string][]DNSRecordConfig, error) {
	out := map[string][]DNSRecordConfig{}
	for _, r := range f.DNSRecords {
		zone := strings.TrimSpace(r.Zone)
		if zone == "" {
			zone = strings.TrimSpace(f.Domain)
		}
		if zone == "" {
			return nil, fmt.Errorf("dns record %s %s has no zone and fleet.domain is empty", r.Type, r.Name)
		}
		out[zone] = append(out[zone], r)
	}
	return out, nil
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
	return b.CredentialsFrom(os.Getenv)
}

// CredentialsFrom is Credentials reading the named values with get (the daemon's secret store, which
// falls back to the environment).
func (b *BackupConfig) CredentialsFrom(get func(string) string) (access, secret string, err error) {
	b.ApplyDefaults()
	access = get(b.AccessKeyEnv)
	secret = get(b.SecretKeyEnv)
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

// DaemonConfig pins the native-ops release a new host's cloud-init installs as its daemon.
type DaemonConfig struct {
	Version string `yaml:"version"`         // release tag, e.g. v1.60.0
	SHA256  string `yaml:"sha256"`          // of native-ops_<version>_linux_<arch>.tar.gz (checksums.txt)
	Arch    string `yaml:"arch,omitempty"`  // amd64 (default) or arm64
	Flags   string `yaml:"flags,omitempty"` // extra `native-ops serve` flags, e.g. "--enable-apply"
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
	Shifted  bool   `yaml:"shifted"` // default true: a new volume gets security.shifted=true (see UnmarshalYAML)
	ReadOnly bool   `yaml:"read_only"`
	// Owner, if set, is a user inside the instance that the mount point is handed to once attached (a
	// fresh volume attaches root-owned regardless of the image, so a service that runs as another user
	// otherwise cannot write to its own data directory). Empty leaves it as Incus attaches it.
	Owner string `yaml:"owner,omitempty"`
}

// UnmarshalYAML makes `shifted` default to true when a manifest leaves it out, as documented; a plain
// bool field would default to false.
func (v *VolumeMount) UnmarshalYAML(value *yaml.Node) error {
	type plain VolumeMount
	p := plain{Shifted: true}
	if err := value.Decode(&p); err != nil {
		return err
	}
	*v = VolumeMount(p)
	return nil
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
	// Cache and RateLimit are HTTP response caching and per-client request limits at the edge, for
	// this route only (see RouteCache, RouteRateLimit). They need the edge's Caddy to carry the
	// cache-handler and caddy-ratelimit plugins (images/edge does).
	Cache     *RouteCache     `yaml:"cache,omitempty"`
	RateLimit *RouteRateLimit `yaml:"rate_limit,omitempty"`
}

// PortForward is a generic raw port forward: a TCP/UDP port on the host published straight into an
// instance, for a protocol the edge cannot carry (git-over-SSH; a service terminating its own mail).
// It is deliberately Incus-independent — a protocol and a host port to an instance port, not a proxy
// device — so the same manifest reads the same on any backend. `listen` is the host side, `target` the
// instance side; each is a bare port number or "address:port" (the address defaults to 0.0.0.0 on the
// host and 127.0.0.1 in the instance).
type PortForward struct {
	Name     string  `yaml:"name,omitempty"`     // device name; defaults to "<protocol>-<port>"
	Protocol string  `yaml:"protocol,omitempty"` // tcp (default) or udp
	Listen   PortRef `yaml:"listen"`             // host port, or "address:port"
	Target   PortRef `yaml:"target"`             // instance port, or "address:port"
}

// PortRef is a port or "address:port", accepted as a YAML number or string, so `listen: 2222` and
// `listen: "0.0.0.0:2222"` both work.
type PortRef string

func (p *PortRef) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("a port must be a number or an address:port string")
	}
	*p = PortRef(strings.TrimSpace(value.Value))
	return nil
}

func (p PortRef) String() string { return string(p) }

var (
	deviceNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	proxyAddrRe  = regexp.MustCompile(`^[A-Za-z0-9_.:\[\]-]+$`)
)

// DeviceProps renders the forward as the Incus proxy-device properties it maps to (`type`, `listen`,
// `connect`), resolving defaults (protocol tcp; host address 0.0.0.0; instance address 127.0.0.1) and
// refusing anything invalid. It is pure, so a manifest is validated without a host.
func (f PortForward) DeviceProps() (map[string]string, error) {
	name := strings.TrimSpace(f.Name)
	protocol := strings.ToLower(strings.TrimSpace(f.Protocol))
	if protocol == "" {
		protocol = "tcp"
	}
	if protocol != "tcp" && protocol != "udp" {
		return nil, fmt.Errorf("forward %s: protocol must be tcp or udp, not %q", f.describe(), f.Protocol)
	}
	hostAddr, hostPort, err := splitPort(f.Listen, "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("forward %s listen: %w", f.describe(), err)
	}
	instAddr, instPort, err := splitPort(f.Target, "127.0.0.1")
	if err != nil {
		return nil, fmt.Errorf("forward %s target: %w", f.describe(), err)
	}
	if name == "" {
		name = fmt.Sprintf("%s-%s", protocol, hostPort)
	}
	if !deviceNameRe.MatchString(name) {
		return nil, fmt.Errorf("forward has no usable name (%q)", name)
	}
	if !proxyAddrRe.MatchString(hostAddr) || !proxyAddrRe.MatchString(instAddr) {
		return nil, fmt.Errorf("forward %s: %q -> %q is not a usable address pair", name, hostAddr, instAddr)
	}
	return map[string]string{
		"type":    "proxy",
		"listen":  protocol + ":" + hostAddr + ":" + hostPort,
		"connect": protocol + ":" + instAddr + ":" + instPort,
	}, nil
}

func (f PortForward) describe() string {
	if s := strings.TrimSpace(f.Name); s != "" {
		return s
	}
	return strings.TrimSpace(f.Listen.String()) + "->" + strings.TrimSpace(f.Target.String())
}

// splitPort parses "2222" or "address:port" (the last colon separates), defaulting the address.
func splitPort(value PortRef, defaultAddr string) (addr, port string, err error) {
	v := strings.TrimSpace(value.String())
	if v == "" {
		return "", "", fmt.Errorf("a port is required")
	}
	addr, port = defaultAddr, v
	if i := strings.LastIndex(v, ":"); i >= 0 {
		addr = strings.TrimSpace(v[:i])
		port = strings.TrimSpace(v[i+1:])
		if addr == "" {
			addr = defaultAddr
		}
	}
	n, convErr := strconv.Atoi(port)
	if convErr != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("port %q must be a number 1-65535", port)
	}
	return addr, port, nil
}

// serviceUnitRe matches a safe systemd unit / /etc/default file name (the shape incus.ValidName
// accepts): a service's `service:` override must be one.
var serviceUnitRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ServiceConfig defines a static infrastructure service (e.g. gitea, plane, edge).
type ServiceConfig struct {
	Name        string            `yaml:"name"`
	Image       string            `yaml:"image"`               // local alias or OCI image
	BuildDir    string            `yaml:"build_dir,omitempty"` // path relative to config repo
	Profiles    []string          `yaml:"profiles"`            // e.g. ["base", "service"]
	Volumes     []VolumeMount     `yaml:"volumes"`
	Env         map[string]string `yaml:"env,omitempty"`
	EnvFile     string            `yaml:"env_file,omitempty"`
	Service     string            `yaml:"service,omitempty"` // systemd unit + /etc/default/<service> when it differs from the instance name
	Limits      map[string]string `yaml:"limits,omitempty"`  // e.g. limits.cpu: 2
	HealthCheck HealthCheckConfig `yaml:"healthcheck,omitempty"`
	Routing     *RoutingConfig    `yaml:"routing,omitempty"`
	Forwards    []PortForward     `yaml:"forwards,omitempty"`
	Hooks       ServiceHooks      `yaml:"hooks,omitempty"`
}

// Unit is the systemd unit (and /etc/default/<name>) this service runs: Service when set, else the
// instance Name. A service whose unit is not named after its instance (env file, restart) declares
// `service:` so apply converges the right unit instead of one that does not exist.
func (s ServiceConfig) Unit() string {
	if s.Service != "" {
		return s.Service
	}
	return s.Name
}

type ServiceHooks struct {
	PreDeploy     string `yaml:"pre_deploy,omitempty"`
	ContainerInit string `yaml:"container_init,omitempty"`
	PostDeploy    string `yaml:"post_deploy,omitempty"`
}

// TemplateConfig defines a blueprint for dynamic tenant workloads (e.g. platform instances).
type TemplateConfig struct {
	Name           string            `yaml:"name"`
	Image          string            `yaml:"image"`             // base image alias
	Service        string            `yaml:"service,omitempty"` // service/unit + /etc/default/<service> name
	Profiles       []string          `yaml:"profiles"`
	Volumes        []VolumeMount     `yaml:"volumes"`
	DefaultLimits  map[string]string `yaml:"default_limits,omitempty"`
	EnvTemplate    map[string]string `yaml:"env_template,omitempty"`
	HealthCheck    HealthCheckConfig `yaml:"healthcheck,omitempty"`
	RoutingPattern string            `yaml:"routing_pattern,omitempty"` // e.g. "{slug}.example.com"
	// RoutingDirectives are extra Caddy directives for the route (for example "import strip-forged-identity").
	RoutingDirectives []string `yaml:"routing_directives,omitempty"`
	// RoutingCache and RoutingRateLimit are the route's edge cache and rate limit (see RoutingConfig).
	RoutingCache     *RouteCache     `yaml:"routing_cache,omitempty"`
	RoutingRateLimit *RouteRateLimit `yaml:"routing_rate_limit,omitempty"`
	Forwards         []PortForward   `yaml:"forwards,omitempty"`
	Hooks            ServiceHooks    `yaml:"hooks,omitempty"`
	// Labels are written to the instance as user.native-ops.label.<key>. nil leaves an existing instance's
	// labels alone; non-nil (even empty) is exactly the set it carries.
	Labels map[string]string `yaml:"labels,omitempty"`
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
	if svc.EnvFile != "" && !RelativeInside(svc.EnvFile) {
		return nil, fmt.Errorf("service config %s: env_file %q must be a relative path inside the config directory", path, svc.EnvFile)
	}
	if svc.Service != "" && !serviceUnitRe.MatchString(svc.Service) {
		return nil, fmt.Errorf("service config %s: service %q is not a valid unit name", path, svc.Service)
	}

	return &svc, nil
}

// RelativeInside reports whether rel is a relative path that stays inside the directory it is
// joined to: not absolute, and no ".." that climbs out of it. A manifest can come from a
// pull request or an upload, so a path in it must never be able to name a file elsewhere on the host.
func RelativeInside(rel string) bool {
	if rel == "" || filepath.IsAbs(rel) || strings.ContainsRune(rel, 0) {
		return false
	}
	c := filepath.Clean(rel)
	return c != ".." && !strings.HasPrefix(c, ".."+string(filepath.Separator))
}

// LoadServices reads every services/<name>/service.yml under configDir (only that one when
// only is set), in name order. `apply` and `plan` both use it, so they always work on the same set.
func LoadServices(configDir, only string) ([]*ServiceConfig, error) {
	servicesDir := filepath.Join(configDir, "services")
	entries, err := os.ReadDir(servicesDir)
	if err != nil {
		return nil, fmt.Errorf("read services directory %s: %w", servicesDir, err)
	}
	var out []*ServiceConfig
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if only != "" && only != name {
			continue
		}
		file := filepath.Join(servicesDir, name, "service.yml")
		if _, err := os.Stat(file); os.IsNotExist(err) {
			continue
		}
		svc, err := LoadServiceConfig(file)
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", file, err)
		}
		if svc.Name == "" {
			svc.Name = name
		}
		out = append(out, svc)
	}
	return out, nil
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
