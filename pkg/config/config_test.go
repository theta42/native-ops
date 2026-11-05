package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
    listen: 2222
    target: 2222
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
	if len(svc.Forwards) != 1 || svc.Forwards[0].Name != "ssh-git" || svc.Forwards[0].Listen != "2222" || svc.Forwards[0].Target != "2222" {
		t.Errorf("unexpected forwards: %+v", svc.Forwards)
	}
}

func TestServiceUnitOverride(t *testing.T) {
	// Unit() falls back to the instance name, or uses Service when it is set.
	if got := (ServiceConfig{Name: "git-mcp"}).Unit(); got != "git-mcp" {
		t.Errorf("Unit() without service: got %q, want git-mcp", got)
	}
	if got := (ServiceConfig{Name: "git-mcp", Service: "gitea-mcp"}).Unit(); got != "gitea-mcp" {
		t.Errorf("Unit() with service: got %q, want gitea-mcp", got)
	}

	tmp := t.TempDir()
	ok := filepath.Join(tmp, "ok.yml")
	if err := os.WriteFile(ok, []byte("name: git-mcp\nimage: x\nservice: gitea-mcp\n"), 0644); err != nil {
		t.Fatal(err)
	}
	svc, err := LoadServiceConfig(ok)
	if err != nil || svc.Unit() != "gitea-mcp" {
		t.Fatalf("valid service: err=%v unit=%q", err, svc.Unit())
	}

	bad := filepath.Join(tmp, "bad.yml")
	if err := os.WriteFile(bad, []byte("name: git-mcp\nimage: x\nservice: ../evil\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadServiceConfig(bad); err == nil {
		t.Fatal("expected an invalid service name to be rejected")
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
	// A bare port pair takes the defaults: tcp, host 0.0.0.0, instance 127.0.0.1.
	props, err := (PortForward{Name: "ssh-git", Listen: "2222", Target: "2222"}).DeviceProps()
	if err != nil {
		t.Fatalf("DeviceProps: %v", err)
	}
	if props["type"] != "proxy" || props["listen"] != "tcp:0.0.0.0:2222" || props["connect"] != "tcp:127.0.0.1:2222" {
		t.Errorf("unexpected props: %+v", props)
	}
	// udp, an explicit host address, and a defaulted name.
	if p, err := (PortForward{Protocol: "udp", Listen: "0.0.0.0:53", Target: "53"}).DeviceProps(); err != nil ||
		p["listen"] != "udp:0.0.0.0:53" || p["connect"] != "udp:127.0.0.1:53" {
		t.Errorf("udp forward: %v %+v", err, p)
	}
	for _, bad := range []PortForward{
		{Name: "bad name", Listen: "2222", Target: "2222"},
		{Name: "x", Protocol: "sctp", Listen: "2222", Target: "2222"},
		{Name: "x", Listen: "", Target: "2222"},
		{Name: "x", Listen: "70000", Target: "2222"},
		{Name: "x", Listen: "0.0.0.0:22; rm -rf /", Target: "2222"},
	} {
		if _, err := bad.DeviceProps(); err == nil {
			t.Errorf("expected %+v to be refused", bad)
		}
	}
}

func TestDNSRecordsByZone(t *testing.T) {
	f := &FleetConfig{
		Domain: "example.com",
		DNSRecords: []DNSRecordConfig{
			{Type: "MX", Name: "inbound", Value: "inbound.example.com.", Priority: 10}, // zone falls back to fleet.domain
			{Zone: "opsavor.work", Type: "MX", Name: "inbound", Value: "inbound.opsavor.work.", Priority: 10},
		},
	}
	by, err := f.DNSRecordsByZone()
	if err != nil {
		t.Fatalf("DNSRecordsByZone: %v", err)
	}
	if len(by["example.com"]) != 1 || len(by["opsavor.work"]) != 1 {
		t.Fatalf("unexpected zones: %+v", by)
	}
	if by["opsavor.work"][0].Priority != 10 {
		t.Errorf("priority not carried: %+v", by["opsavor.work"][0])
	}
	if _, err := (&FleetConfig{DNSRecords: []DNSRecordConfig{{Type: "MX", Name: "inbound", Value: "x."}}}).DNSRecordsByZone(); err == nil {
		t.Fatal("a record with no zone and no fleet.domain must be refused")
	}
}

func TestVolumeShiftedDefaultsToTrue(t *testing.T) {
	var svc ServiceConfig
	if err := yaml.Unmarshal([]byte("name: web\nvolumes:\n  - name: a\n    path: /a\n  - name: b\n    path: /b\n    shifted: false\n"), &svc); err != nil {
		t.Fatal(err)
	}
	if len(svc.Volumes) != 2 || !svc.Volumes[0].Shifted || svc.Volumes[1].Shifted || svc.Volumes[0].Name != "a" || svc.Volumes[1].Path != "/b" {
		t.Fatalf("volumes: %+v (an omitted shifted is true, an explicit false is kept)", svc.Volumes)
	}
}

func TestARouteReadsItsCacheAndRateLimitFromYAML(t *testing.T) {
	var svc ServiceConfig
	doc := "name: web\nrouting:\n  domain: www.example.com\n  upstream_port: 3000\n  cache:\n    ttl: 5m\n    paths: [\"/static/*\"]\n  rate_limit:\n    requests: 120\n    window: 1m\n    paths: [\"/api/*\"]\n"
	if err := yaml.Unmarshal([]byte(doc), &svc); err != nil {
		t.Fatal(err)
	}
	r := svc.Routing
	if r.Cache == nil || r.Cache.TTL != "5m" || r.Cache.Paths[0] != "/static/*" || r.RateLimit == nil || r.RateLimit.Requests != 120 || r.RateLimit.Window != "1m" {
		t.Fatalf("routing: %+v cache %+v limit %+v", r, r.Cache, r.RateLimit)
	}
	if err := r.ValidateEdgeOptions(); err != nil {
		t.Fatal(err)
	}
	var tpl TemplateConfig
	if err := yaml.Unmarshal([]byte("name: platform\nrouting_cache:\n  paths: [\"/assets/*\"]\nrouting_rate_limit:\n  requests: 60\n  window: 30s\n"), &tpl); err != nil {
		t.Fatal(err)
	}
	if tpl.RoutingCache == nil || tpl.RoutingRateLimit == nil || tpl.RoutingCache.TTLOrDefault() != "2m" {
		t.Fatalf("template: %+v", tpl)
	}
}

func TestAServiceManifestCarriesItsLabelsAndRefusesABadOne(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "service.yml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	svc, err := LoadServiceConfig(write("name: crew\nimage: crew:latest\nlabels:\n  environment: production\n  app: crew\n"))
	if err != nil {
		t.Fatal(err)
	}
	if svc.Labels["environment"] != "production" || svc.Labels["app"] != "crew" {
		t.Fatalf("%v", svc.Labels)
	}
	none, err := LoadServiceConfig(write("name: crew\nimage: crew:latest\n"))
	if err != nil || none.Labels != nil {
		t.Fatalf("no labels: declared nothing (nil), %v %v", none.Labels, err)
	}
	for _, bad := range []string{"environment: prod", "Environment: production", "app: a b"} {
		if _, err := LoadServiceConfig(write("name: crew\nimage: crew:latest\nlabels:\n  " + bad + "\n")); err == nil {
			t.Errorf("labels %q must be refused when the manifest is loaded", bad)
		}
	}
}

func TestEnvFromNamesOnlyServiceSecretsAndValidKeys(t *testing.T) {
	tmp := t.TempDir()
	load := func(body string) (*ServiceConfig, error) {
		f := filepath.Join(tmp, "svc.yml")
		if err := os.WriteFile(f, []byte("name: crew\nimage: x\n"+body), 0644); err != nil {
			t.Fatal(err)
		}
		return LoadServiceConfig(f)
	}

	svc, err := load("env_from:\n  GOOGLE_KEY: SERVICE_CREW_GOOGLE_KEY\n")
	if err != nil || svc.EnvFrom["GOOGLE_KEY"] != "SERVICE_CREW_GOOGLE_KEY" {
		t.Fatalf("a valid env_from: err=%v env_from=%v", err, svc.EnvFrom)
	}

	for body, want := range map[string]string{
		// The daemon's own credentials cannot be routed into a service.
		"env_from:\n  TOKEN: DO_API_TOKEN\n":                          "SERVICE_",
		"env_from:\n  KEY: BACKUP_S3_SECRET_KEY\n":                    "SERVICE_",
		"env_from:\n  KEY: SERVICE_\n":                                "SERVICE_",
		"env_from:\n  KEY: service_lower\n":                           "SERVICE_",
		"env_from:\n  bad-key: SERVICE_X\n":                           "not an environment key",
		"env: {KEY: plain}\nenv_from:\n  KEY: SERVICE_X\n":            "also set in env",
		"env_from:\n  KEY: SERVICE_" + strings.Repeat("A", 57) + "\n": "SERVICE_",
	} {
		if _, err := load(body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want an error mentioning %q, got %v", body, want, err)
		}
	}
}

// fleetWithHosts loads a fleet.yml made of extra (network: and hosts: sections). A fleet of two or more
// hosts needs a default_host; these fixtures all have a node-1, so it is that unless extra names one.
func fleetWithHosts(t *testing.T, extra string) (*FleetConfig, error) {
	t.Helper()
	if strings.Contains(extra, "node-2") && !strings.Contains(extra, "default_host") {
		extra += "default_host: node-1\n"
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fleet.yml"), []byte("name: test\n"+extra), 0o644); err != nil {
		t.Fatal(err)
	}
	return LoadFleetConfig(dir)
}

func TestAHostOverridesTheFleetBridgeFieldByField(t *testing.T) {
	f, err := fleetWithHosts(t, `
hosts:
  node-1:
    provider: static
  node-2:
    provider: static
    network:
      ipv4_cidr: 10.0.101.0/24
  node-3:
    provider: static
    network:
      bridge_name: br-inst
      ipv4_cidr: 10.0.102.0/24
`)
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]NetworkConfig{
		"node-1":  {BridgeName: "incusbr0", IPv4CIDR: "10.0.100.0/24"},
		"node-2":  {BridgeName: "incusbr0", IPv4CIDR: "10.0.101.0/24"},
		"node-3":  {BridgeName: "br-inst", IPv4CIDR: "10.0.102.0/24"},
		"unknown": {BridgeName: "incusbr0", IPv4CIDR: "10.0.100.0/24"},
	} {
		if got := f.HostNetwork(host); got != want {
			t.Errorf("%s: got %+v, want %+v", host, got, want)
		}
	}
	if _, ok := f.LocalNetwork(""); ok {
		t.Error("with three hosts the local one cannot be known")
	}
}

func TestLocalNetworkIsTheOnlyHostsOrTheFleets(t *testing.T) {
	f, err := fleetWithHosts(t, "hosts:\n  node-1:\n    network:\n      ipv4_cidr: 10.0.101.0/24\n")
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := f.LocalNetwork(""); !ok || n.IPv4CIDR != "10.0.101.0/24" || n.BridgeName != "incusbr0" {
		t.Fatalf("one host: got %+v %v", n, ok)
	}
	f, err = fleetWithHosts(t, "network:\n  ipv4_cidr: 10.0.50.0/24\n")
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := f.LocalNetwork(""); !ok || n.IPv4CIDR != "10.0.50.0/24" {
		t.Fatalf("no hosts: got %+v %v", n, ok)
	}
}

func TestBridgeAddressIsTheSubnetsFirstAddressUnlessOneIsNamed(t *testing.T) {
	for cidr, want := range map[string]string{
		"10.0.100.0/24":   "10.0.100.1/24",
		"10.0.101.0/24":   "10.0.101.1/24",
		"10.0.101.254/24": "10.0.101.254/24",
		"10.0.100.1/24":   "10.0.100.1/24",
		"172.16.0.0/16":   "172.16.0.1/16",
	} {
		if got, err := (NetworkConfig{IPv4CIDR: cidr}).BridgeAddress(); err != nil || got != want {
			t.Errorf("%s: got %q %v, want %q", cidr, got, err, want)
		}
	}
	for _, bad := range []string{"", "10.0.100.0", "nope", "fd00::/64", "10.0.100.0/31"} {
		if got, err := (NetworkConfig{IPv4CIDR: bad}).BridgeAddress(); err == nil {
			t.Errorf("%q: got %q, want an error", bad, got)
		}
	}
	if _, _, err := (NetworkConfig{BridgeName: "br0; reboot", IPv4CIDR: "10.0.100.0/24"}).Bridge(); err == nil {
		t.Error("a bridge name goes into host commands: anything but an interface name is refused")
	}
}

func TestOverlappingHostSubnetsAreRefused(t *testing.T) {
	ok := []string{
		"hosts:\n  node-1: {}\n",
		"hosts:\n  node-1: {}\n  node-2:\n    network:\n      ipv4_cidr: 10.0.101.0/24\n",
		"hosts:\n  node-1:\n    network:\n      ipv4_cidr: 10.0.100.0/25\n  node-2:\n    network:\n      ipv4_cidr: 10.0.100.128/25\n",
	}
	for _, extra := range ok {
		if _, err := fleetWithHosts(t, extra); err != nil {
			t.Errorf("%s: %v", extra, err)
		}
	}
	bad := map[string]string{
		// Two hosts without an override share the fleet's subnet.
		"hosts:\n  node-1: {}\n  node-2: {}\n":                                                                                   "overlapping",
		"hosts:\n  node-1: {}\n  node-2:\n    network:\n      ipv4_cidr: 10.0.100.128/25\n":                                      "overlapping",
		"hosts:\n  node-1:\n    network:\n      ipv4_cidr: 10.0.0.0/16\n  node-2:\n    network:\n      ipv4_cidr: 10.0.7.0/24\n": "overlapping",
		"hosts:\n  node-1:\n    network:\n      ipv4_cidr: 10.0.300.0/24\n":                                                      "not an IPv4 CIDR",
		"hosts:\n  node-1:\n    network:\n      bridge_name: \"a b\"\n":                                                          "not a usable interface name",
	}
	for extra, want := range bad {
		if _, err := fleetWithHosts(t, extra); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error with %q", extra, err, want)
		}
	}
}

func writeFleet(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fleet.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestASingleHostIsTheDefaultWithoutDefaultHost(t *testing.T) {
	cfg, err := LoadFleetConfig(writeFleet(t, "name: f\nhosts:\n  only: {provider: static, address: 192.0.2.10}\n"))
	if err != nil {
		t.Fatalf("one host needs no default_host: %v", err)
	}
	if cfg.DefaultHostName() != "only" || cfg.HostOf(&ServiceConfig{Name: "web"}) != "only" {
		t.Fatalf("the only host is the default: %q", cfg.DefaultHostName())
	}
	if h, filtered, err := cfg.DaemonHost(""); err != nil || filtered || h != "only" {
		t.Fatalf("an untold daemon of a single-host fleet is that host, planning everything: %q %v %v", h, filtered, err)
	}
	if _, _, err := cfg.DaemonHost("other"); err == nil {
		t.Fatal("a daemon told it is a host fleet.yml does not have is refused")
	}
	// No hosts: at all (a conf repo that never named one): nothing to tell apart.
	none, err := LoadFleetConfig(writeFleet(t, "name: f\n"))
	if err != nil || none.DefaultHostName() != "" {
		t.Fatalf("no hosts: %v %q", err, none.DefaultHostName())
	}
	if _, filtered, err := none.DaemonHost(""); err != nil || filtered {
		t.Fatalf("no hosts plans everything: %v %v", filtered, err)
	}
}

func TestTwoHostsNeedADefaultHost(t *testing.T) {
	two := "name: f\nhosts:\n  h1: {provider: static, address: 192.0.2.10}\n  h2: {provider: static, address: 192.0.2.11, network: {ipv4_cidr: 10.0.101.0/24}}\n"
	_, err := LoadFleetConfig(writeFleet(t, two))
	if err == nil || !strings.Contains(err.Error(), "default_host") || !strings.Contains(err.Error(), "h1, h2") {
		t.Fatalf("two hosts without default_host must say why it is needed: %v", err)
	}
	if _, err := LoadFleetConfig(writeFleet(t, two+"default_host: h3\n")); err == nil || !strings.Contains(err.Error(), "h3") {
		t.Fatalf("a default_host that is not a host: %v", err)
	}
	cfg, err := LoadFleetConfig(writeFleet(t, two+"default_host: h2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostOf(&ServiceConfig{Name: "a"}) != "h2" || cfg.HostOf(&ServiceConfig{Name: "b", Host: "h1"}) != "h1" {
		t.Fatal("a service is on its host:, else the default host")
	}
	if _, _, err := cfg.DaemonHost(""); !errors.Is(err, ErrHostNameRequired) {
		t.Fatalf("an untold daemon of two hosts is refused: %v", err)
	}
	if h, filtered, err := cfg.DaemonHost("h1"); err != nil || !filtered || h != "h1" {
		t.Fatalf("a told daemon plans its share: %q %v %v", h, filtered, err)
	}
	// daemon.flags are the same for every host's daemon: they cannot say which host one is.
	if _, err := LoadFleetConfig(writeFleet(t, two+"default_host: h1\ndaemon: {version: v1.0.0, flags: \"--enable-apply --host-name h1\"}\n")); err == nil || !strings.Contains(err.Error(), "--host-name") {
		t.Fatalf("--host-name in daemon.flags with two hosts: %v", err)
	}
	if _, err := LoadFleetConfig(writeFleet(t, "name: f\nhosts:\n  only: {provider: static}\ndaemon: {flags: \"--host-name only\"}\n")); err != nil {
		t.Fatalf("with one host, daemon.flags may name it: %v", err)
	}
	if _, err := LoadFleetConfig(writeFleet(t, "name: f\nhosts:\n  only: {provider: static}\ndaemon: {flags: \"--host-name=other\"}\n")); err == nil {
		t.Fatal("with one host, daemon.flags may not name another")
	}
}

func TestAServiceHostMustBeAHostOfTheFleet(t *testing.T) {
	cfg, err := LoadFleetConfig(writeFleet(t, "name: f\ndefault_host: h1\nhosts:\n  h1: {provider: static}\n  h2: {provider: static, network: {ipv4_cidr: 10.0.101.0/24}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "service.yml")
	if err := os.WriteFile(path, []byte("name: web\nimage: x\nhost: h2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc, err := LoadServiceConfig(path)
	if err != nil || svc.Host != "h2" {
		t.Fatalf("host: is read from the manifest: %+v %v", svc, err)
	}
	if err := cfg.ValidateServiceHost(svc); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateServiceHost(&ServiceConfig{Name: "web", Host: "h9"}); err == nil || !strings.Contains(err.Error(), "h9") || !strings.Contains(err.Error(), "h1, h2") {
		t.Fatalf("an unknown host: %v", err)
	}
	if err := cfg.ValidateServiceHost(&ServiceConfig{Name: "web"}); err != nil {
		t.Fatalf("no host: is the default host: %v", err)
	}
}
