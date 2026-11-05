package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/theta42/native-ops/pkg/labels"
)

// FleetConfig defines global fleet configuration.
type FleetConfig struct {
	Name        string                `yaml:"name"`
	Domain      string                `yaml:"domain"`
	DNSProvider string                `yaml:"dns_provider"` // "digitalocean", "proxmox", or custom plugin
	Network     NetworkConfig         `yaml:"network"`
	Providers   ProvidersConfig       `yaml:"providers"`
	Hosts       map[string]HostConfig `yaml:"hosts"`
	// DefaultHost is the host a service without `host:` belongs to. With one entry in hosts: that entry
	// is the default and this may be left out; with two or more it is required (see ValidateHosts).
	DefaultHost string        `yaml:"default_host,omitempty"`
	Backup      *BackupConfig `yaml:"backup,omitempty"`
	// Snapshots is the default local snapshot policy for every volume a service declares (a volume's own
	// `snapshots:` replaces it). It is applied as Incus' own storage-volume config by plan and apply, so
	// the host needs no snapshot timer. Absent, volumes' snapshot config is left as it is.
	Snapshots *SnapshotPolicy `yaml:"snapshots,omitempty"`
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
	// Service ties the record to a service: it follows that service's host (its `host:`, else the default
	// host), and only that host's daemon plans and applies it. An A or AAAA record tied to a service leaves
	// Value out: it is the host's `address:`. A record tied to no service belongs to the default host.
	Service string `yaml:"service,omitempty"`
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
	// RetainDays also keeps every object younger than this many days. With any retention set, the
	// newest object of a volume is always kept.
	RetainDays int `yaml:"retain_days,omitempty"`
	// KeepLocalSnapshots keeps the transient pre-backup snapshot on the host
	// after a successful upload (default false).
	KeepLocalSnapshots bool `yaml:"keep_local_snapshots,omitempty"`
	// Schedule makes the daemon back up every allowed volume by itself, then apply retention, as a
	// recorded job: one or more daily UTC times, "HH:MM" or "HH:MM,HH:MM". Empty: no schedule (backups
	// run only when asked, e.g. by POST /v1/backups). See ScheduleTimes.
	Schedule string `yaml:"schedule,omitempty"`
	// HostName is the host this backup runs on, the second part of every object key
	// (<prefix>/<host>/<volume>/...), so hosts sharing a bucket never overwrite each other. It is never
	// read from fleet.yml, which every host shares: the daemon (or the CLI) sets it, and the backup
	// package falls back to the OS host name.
	HostName string `yaml:"-"`
}

var scheduleTimeRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// ScheduleTimes parses Schedule into minutes after midnight UTC, sorted and without duplicates. An
// empty Schedule gives none.
func (b *BackupConfig) ScheduleTimes() ([]int, error) {
	if strings.TrimSpace(b.Schedule) == "" {
		return nil, nil
	}
	seen := map[int]bool{}
	var out []int
	for _, part := range strings.Split(b.Schedule, ",") {
		m := scheduleTimeRe.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			return nil, fmt.Errorf("backup.schedule: %q is not a daily UTC time HH:MM (00:00-23:59)", strings.TrimSpace(part))
		}
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		if t := h*60 + mi; !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Ints(out)
	return out, nil
}

// Validate checks the parts of the section that need no object store: the schedule and retention.
func (b *BackupConfig) Validate() error {
	if _, err := b.ScheduleTimes(); err != nil {
		return err
	}
	if b.RetainDaily < 0 || b.RetainMonthly < 0 || b.RetainDays < 0 {
		return fmt.Errorf("backup: retention counts cannot be negative")
	}
	return nil
}

// SnapshotPolicy is a volume's local snapshot schedule, kept by Incus itself through the custom volume's
// own config (snapshots.schedule, snapshots.expiry, snapshots.pattern), so no host timer is needed. A
// declared policy is exactly what the volume carries: a field left empty unsets that key (Incus' default
// then applies), and an empty policy (`snapshots: {}`) turns scheduled snapshots off.
type SnapshotPolicy struct {
	// Schedule is a cron expression or an alias Incus accepts ("@daily", "0 */6 * * *"); several are
	// separated by commas.
	Schedule string `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	// Expiry is how long a snapshot is kept, e.g. "7d", "2w", "1d 12H" (M minutes, H hours, d days,
	// w weeks, m months, y years).
	Expiry string `yaml:"expiry,omitempty" json:"expiry,omitempty"`
	// Pattern names the snapshots, e.g. "snap%d" or "auto-{{ creation_date|date:'2006-01-02_15-04' }}".
	Pattern string `yaml:"pattern,omitempty" json:"pattern,omitempty"`
}

// Snapshot config keys on an Incus custom volume, in the order plans list them.
var SnapshotKeys = []string{"snapshots.schedule", "snapshots.expiry", "snapshots.pattern"}

var (
	snapshotScheduleRe = regexp.MustCompile(`^[A-Za-z0-9@*/,\- ]{1,200}$`)
	snapshotExpiryRe   = regexp.MustCompile(`^[0-9]+[MHdwmy]( +[0-9]+[MHdwmy])*$`)
)

// VolumeConfig is the policy as the Incus volume config keys it sets (an empty value means unset).
func (p SnapshotPolicy) VolumeConfig() map[string]string {
	return map[string]string{
		"snapshots.schedule": strings.TrimSpace(p.Schedule),
		"snapshots.expiry":   strings.TrimSpace(p.Expiry),
		"snapshots.pattern":  strings.TrimSpace(p.Pattern),
	}
}

// Validate refuses values Incus would reject, or that do not belong in a volume's config.
func (p SnapshotPolicy) Validate() error {
	for _, raw := range []string{p.Schedule, p.Expiry, p.Pattern} {
		for _, r := range raw {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("snapshots: a value has a control character")
			}
		}
	}
	c := p.VolumeConfig()
	if v := c["snapshots.schedule"]; v != "" && !snapshotScheduleRe.MatchString(v) {
		return fmt.Errorf("snapshots.schedule %q is not a cron expression or @alias", v)
	}
	if v := c["snapshots.expiry"]; v != "" && !snapshotExpiryRe.MatchString(v) {
		return fmt.Errorf("snapshots.expiry %q is not a duration like 7d, 2w or \"1d 12H\"", v)
	}
	if v := c["snapshots.pattern"]; v != "" {
		if len(v) > 200 || strings.ContainsAny(v, "/") {
			return fmt.Errorf("snapshots.pattern %q: at most 200 characters, and no '/'", v)
		}
	}
	return nil
}

// validateVolumeSnapshots checks every volume's own snapshot policy.
func validateVolumeSnapshots(vols []VolumeMount) error {
	for _, v := range vols {
		if v.Snapshots == nil {
			continue
		}
		if err := v.Snapshots.Validate(); err != nil {
			return fmt.Errorf("volume %s: %w", v.Name, err)
		}
	}
	return nil
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

// NetworkConfig is a host's instance bridge: its name and its IPv4 subnet. The fleet-level `network:` is
// every host's default; a host can override either field under `hosts.<name>.network` (see HostNetwork).
type NetworkConfig struct {
	BridgeName string `yaml:"bridge_name"` // default: incusbr0 (DefaultBridgeName)
	IPv4CIDR   string `yaml:"ipv4_cidr"`   // default: 10.0.100.0/24 (DefaultIPv4CIDR)
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
	Provider string `yaml:"provider"` // "digitalocean", "proxmox", "static"
	Address  string `yaml:"address"`
	SSHUser  string `yaml:"ssh_user"`
	SSHPort  int    `yaml:"ssh_port"`
	// IncusRemote is the Incus remote, as the other hosts name it, that reaches this host: a migration to
	// it (POST /v1/instances/{name}/migrate with target_host) goes to this remote. Optional.
	IncusRemote string `yaml:"incus_remote,omitempty"`
	// Network overrides the fleet-level `network:` for this host; a field left empty falls back to it.
	// Each host needs its own subnet (LoadFleetConfig refuses two that overlap).
	Network *NetworkConfig `yaml:"network,omitempty"`
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
	// Snapshots is this volume's local snapshot policy (see SnapshotPolicy); it replaces fleet.yml's
	// `snapshots:` default. Absent, the fleet default applies, and without one the volume's snapshot
	// config is left as it is. (The json name keeps a plan's hash unchanged for a manifest without it.)
	Snapshots *SnapshotPolicy `yaml:"snapshots,omitempty" json:",omitempty"`
}

// SnapshotPolicyFor is the policy a volume gets: its own, else the fleet default, else nil (leave the
// volume's snapshot config alone).
func SnapshotPolicyFor(vol VolumeMount, fleetDefault *SnapshotPolicy) *SnapshotPolicy {
	if vol.Snapshots != nil {
		return vol.Snapshots
	}
	return fleetDefault
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
	// External means the domain is already served by the hand-maintained Caddyfile (a wildcard block
	// with a host matcher). plan and apply leave it alone: publishing sites/<name>.caddy would ask for
	// a second certificate and shadow that block.
	External bool `yaml:"external,omitempty"`
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
		name = f.DeviceName()
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

// DeviceName is the Incus device the forward is published as: its name, or "<protocol>-<host port>".
func (f PortForward) DeviceName() string {
	if s := strings.TrimSpace(f.Name); s != "" {
		return s
	}
	protocol := strings.ToLower(strings.TrimSpace(f.Protocol))
	if protocol == "" {
		protocol = "tcp"
	}
	_, port, err := splitPort(f.Listen, "0.0.0.0")
	if err != nil {
		return ""
	}
	return protocol + "-" + port
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

// SecretPrefix starts the name of every secret a service may read (env_from). The daemon's own
// credentials (DNS token, backup keys, OIDC secret) never have it.
const SecretPrefix = "SERVICE_"

var (
	envKeyRe        = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	serviceSecretRe = regexp.MustCompile(`^SERVICE_[A-Z0-9_]{1,56}$`)
)

// ValidateEnvFrom checks a service's env_from: environment keys in [A-Z_][A-Z0-9_]*, secret names that
// start with SecretPrefix (and fit the secret store's names), and no key that env also sets.
func ValidateEnvFrom(svc *ServiceConfig) error {
	for key, name := range svc.EnvFrom {
		if !envKeyRe.MatchString(key) {
			return fmt.Errorf("env_from: %q is not an environment key (A-Z, 0-9 and _)", key)
		}
		if !serviceSecretRe.MatchString(name) {
			return fmt.Errorf("env_from: %s names %q; a service's secrets are named %s<A-Z, 0-9, _> (at most 64 characters)", key, name, SecretPrefix)
		}
		if _, dup := svc.Env[key]; dup {
			return fmt.Errorf("env_from: %s is also set in env; set it in one place", key)
		}
	}
	return nil
}

// ServiceConfig defines a static infrastructure service (e.g. gitea, plane, edge).
type ServiceConfig struct {
	Name     string            `yaml:"name"`
	Image    string            `yaml:"image"`               // local alias or OCI image
	BuildDir string            `yaml:"build_dir,omitempty"` // path relative to config repo
	Profiles []string          `yaml:"profiles"`            // e.g. ["base", "service"]
	Volumes  []VolumeMount     `yaml:"volumes"`
	Env      map[string]string `yaml:"env,omitempty"`
	EnvFile  string            `yaml:"env_file,omitempty"`
	// EnvFrom sets environment keys from the daemon's secret store instead of from git:
	// {ENV_KEY: SERVICE_SECRET_NAME}. The value is written to /etc/default/<service> like env, but the
	// manifest only names the secret. Only names starting with SecretPrefix are allowed, so a manifest can
	// never route the daemon's own credentials into a container. See ValidateEnvFrom.
	EnvFrom     map[string]string `yaml:"env_from,omitempty"`
	Service     string            `yaml:"service,omitempty"` // systemd unit + /etc/default/<service> when it differs from the instance name
	Limits      map[string]string `yaml:"limits,omitempty"`  // e.g. limits.cpu: 2
	HealthCheck HealthCheckConfig `yaml:"healthcheck,omitempty"`
	Routing     *RoutingConfig    `yaml:"routing,omitempty"`
	Forwards    []PortForward     `yaml:"forwards,omitempty"`
	Hooks       ServiceHooks      `yaml:"hooks,omitempty"`
	// Labels say what the service is (environment, app, ...): see pkg/labels. Declared, they are exactly the set
	// the instance carries, and a change is applied live (no restart). Absent (nil), the instance's labels are
	// left alone.
	Labels map[string]string `yaml:"labels,omitempty"`
	// Host is the entry of fleet.yml's hosts: this service runs on; empty means the default host. Only
	// that host's daemon plans and applies it (see FleetConfig.DaemonHost); the others show it as skipped.
	// json omitempty: a manifest without host: digests (bindOf) exactly as it did before the field existed.
	Host string `yaml:"host,omitempty" json:",omitempty"`
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
		cfg.Network.BridgeName = DefaultBridgeName
	}
	if cfg.Network.IPv4CIDR == "" {
		cfg.Network.IPv4CIDR = DefaultIPv4CIDR
	}
	if err := cfg.ValidateHostNetworks(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Backup != nil {
		cfg.Backup.ApplyDefaults()
		if err := cfg.Backup.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if cfg.Snapshots != nil {
		if err := cfg.Snapshots.Validate(); err != nil {
			return nil, fmt.Errorf("%s: snapshots: %w", path, err)
		}
	}
	if err := cfg.ValidateHosts(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
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
	if err := labels.Validate(svc.Labels); err != nil {
		return nil, fmt.Errorf("service config %s: %w", path, err)
	}
	if err := ValidateEnvFrom(&svc); err != nil {
		return nil, fmt.Errorf("service config %s: %w", path, err)
	}
	if err := validateVolumeSnapshots(svc.Volumes); err != nil {
		return nil, fmt.Errorf("service config %s: %w", path, err)
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
	if err := validateVolumeSnapshots(tmpl.Volumes); err != nil {
		return nil, fmt.Errorf("template config %s: %w", path, err)
	}

	return &tmpl, nil
}
