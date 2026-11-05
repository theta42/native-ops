package engine

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/provider"
	"github.com/theta42/native-ops/pkg/provider/digitalocean"
	"github.com/theta42/native-ops/pkg/provider/plugin"
	"github.com/theta42/native-ops/pkg/remote"
)

// Reconciler brings a fleet's hosts up: it provisions the hosts fleet.yml declares (Level 0), syncs
// their DNS, and prepares each host's Incus runtime. Deploying services is the daemon's job, through a
// plan an admin approved (POST /v1/plan, /v1/apply); reconcile does it too only when DeployServices is
// set, as a break-glass path for a host whose daemon is not running yet or is broken, since that path
// skips the plan, the approval, the host lock and the job record.
type Reconciler struct {
	configDir string
	exec      remote.Executor
	deployer  *Deployer
	hostMgr   *HostManager
	// DeployServices also deploys every service directly, bypassing the daemon's approval gate.
	DeployServices bool

	// The ways reconcile reaches the world, replaced by tests: SSH credentials, an SSH session to a host,
	// waiting for a host's SSH port, the pause between SSH attempts, and the fleet's DNS provider.
	sshCreds      func() (priv []byte, pub string, generated bool)
	dialSSH       func(host string, port int, user string, key []byte) (remote.Executor, error)
	waitSSH       func(ctx context.Context, host string, port int, timeout time.Duration) error
	sshRetryDelay time.Duration
	dnsFor        func(fleet *config.FleetConfig) (provider.DNSProvider, error)
}

func NewReconciler(configDir string, exec remote.Executor) *Reconciler {
	return &Reconciler{
		configDir: configDir,
		exec:      exec,
		deployer:  NewDeployer(exec),
		hostMgr:   NewHostManager(),
		sshCreds:  LoadSSHCredentials,
		dialSSH: func(host string, port int, user string, key []byte) (remote.Executor, error) {
			return remote.NewSSHExecutor(host, port, user, key)
		},
		waitSSH:       waitForSSH,
		sshRetryDelay: 3 * time.Second,
		dnsFor:        fleetDNSProvider(configDir),
	}
}

// fleetDNSProvider is the DNS provider fleet.yml's dns_provider names: DigitalOcean, or a script plugin in
// the config repo.
func fleetDNSProvider(configDir string) func(fleet *config.FleetConfig) (provider.DNSProvider, error) {
	return func(fleet *config.FleetConfig) (provider.DNSProvider, error) {
		if fleet.DNSProvider == "digitalocean" || fleet.DNSProvider == "do" {
			do, err := digitalocean.New("")
			if err != nil {
				return nil, fmt.Errorf("DO DNS init: %w", err)
			}
			return do, nil
		}
		p, err := plugin.NewScriptDNSProvider(fleet.DNSProvider, configDir)
		if err != nil {
			return nil, fmt.Errorf("custom DNS plugin init: %w", err)
		}
		return p, nil
	}
}

// PlanSummary summarizes what the configuration describes.
type PlanSummary struct {
	FleetName   string
	Domain      string
	DNSProvider string
	Hosts       []string
	Services    []string
	Templates   []string
	// PerHost is what each host's daemon plans and applies, in the order of the hosts' names: its services
	// and the DNS records that point at it. Empty when fleet.yml has no hosts:.
	PerHost []HostSummary
}

// HostSummary is one host's share of the fleet.
type HostSummary struct {
	Name     string
	Address  string
	Default  bool     // the default host: services without host: and DNS records tied to no service
	Services []string // sorted
	DNS      []string // "TYPE name value", as fleet.yml declares them (dns_records first, then routes)
}

// Validate loads and validates all configuration files and returns a PlanSummary.
func (r *Reconciler) Validate(ctx context.Context) (*PlanSummary, error) {
	fleet, err := config.LoadFleetConfig(r.configDir)
	if err != nil {
		return nil, fmt.Errorf("load fleet.yml: %w", err)
	}

	if d := fleet.Daemon; d != nil {
		// The token is not needed to check the rest; a placeholder hash stands in for it.
		di := &DaemonInstall{Version: d.Version, SHA256: strings.ToLower(d.SHA256), Arch: d.Arch,
			BootstrapTokenSHA256: strings.Repeat("0", 64), ServeFlags: d.Flags}
		if err := di.Validate(); err != nil {
			return nil, fmt.Errorf("fleet.yml daemon: %w", err)
		}
	}
	summary := &PlanSummary{
		FleetName:   fleet.Name,
		Domain:      fleet.Domain,
		DNSProvider: fleet.DNSProvider,
	}

	summary.Hosts = fleet.HostNames()
	var services []*config.ServiceConfig

	// Validate services
	servicesDir := filepath.Join(r.configDir, "services")
	if entries, err := os.ReadDir(servicesDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			svcPath := filepath.Join(servicesDir, e.Name(), "service.yml")
			if _, err := os.Stat(svcPath); err == nil {
				svc, err := config.LoadServiceConfig(svcPath)
				if err != nil {
					return nil, fmt.Errorf("invalid service %s: %w", svcPath, err)
				}
				if err := fleet.ValidateServiceHost(svc); err != nil {
					return nil, fmt.Errorf("invalid service %s: %w", svcPath, err)
				}
				summary.Services = append(summary.Services, svc.Name)
				if svc.Name == "" {
					svc.Name = e.Name()
				}
				services = append(services, svc)
			}
		}
	}

	// Each host's share: its services, and the DNS records that point at it (refusing a record no host can
	// own alone).
	records, err := fleet.DNSRecordsByHost(services)
	if err != nil {
		return nil, fmt.Errorf("fleet.yml dns: %w", err)
	}
	for _, name := range summary.Hosts {
		hs := HostSummary{Name: name, Address: fleet.Hosts[name].Address, Default: name == fleet.DefaultHostName()}
		for _, svc := range services {
			if fleet.HostOf(svc) == name {
				hs.Services = append(hs.Services, svc.Name)
			}
		}
		sort.Strings(hs.Services)
		for _, r := range records {
			if r.Host == name {
				hs.DNS = append(hs.DNS, strings.TrimSpace(r.Type+" "+dnsName(r.Name, r.Zone)+" "+r.Value))
			}
		}
		summary.PerHost = append(summary.PerHost, hs)
	}

	// Validate templates
	templatesDir := filepath.Join(r.configDir, "templates")
	if entries, err := os.ReadDir(templatesDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			tmplPath := filepath.Join(templatesDir, e.Name(), "template.yml")
			if _, err := os.Stat(tmplPath); err == nil {
				tmpl, err := config.LoadTemplateConfig(tmplPath)
				if err != nil {
					return nil, fmt.Errorf("invalid template %s: %w", tmplPath, err)
				}
				summary.Templates = append(summary.Templates, tmpl.Name)
			}
		}
	}

	return summary, nil
}

// GenerateSSHKeypair creates an in-memory ed25519 keypair if none is provided.
func GenerateSSHKeypair() ([]byte, string, error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate ed25519 key: %w", err)
	}

	privBlock, err := ssh.MarshalPrivateKey(privKey, "")
	if err != nil {
		return nil, "", fmt.Errorf("marshal openssh private key: %w", err)
	}
	privPEM := pem.EncodeToMemory(privBlock)

	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return nil, "", fmt.Errorf("marshal openssh public key: %w", err)
	}
	pubKeyStr := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))

	return privPEM, pubKeyStr, nil
}

// LoadSSHCredentials returns the operator's SSH private key and its public
// half: $SSH_PRIVATE_KEY, $FLEET_SSH_KEY, or ~/.ssh/id_ed25519 / id_rsa. If none
// is configured a throwaway ed25519 pair is generated and generated is true; its
// private half exists only in this process, so a host that only trusts it can
// never be logged in to again by anyone.
func LoadSSHCredentials() (priv []byte, pub string, generated bool) {
	var privKeyPEM []byte
	if keyEnv := os.Getenv("SSH_PRIVATE_KEY"); keyEnv != "" {
		privKeyPEM = []byte(keyEnv)
	} else if keyEnv := os.Getenv("FLEET_SSH_KEY"); keyEnv != "" {
		privKeyPEM = []byte(keyEnv)
	} else {
		home, _ := os.UserHomeDir()
		candidates := []string{
			filepath.Join(home, ".ssh", "id_ed25519"),
			filepath.Join(home, ".ssh", "id_rsa"),
			"/root/.ssh/id_ed25519",
		}
		for _, p := range candidates {
			if data, err := os.ReadFile(p); err == nil {
				privKeyPEM = data
				break
			}
		}
	}

	if len(privKeyPEM) > 0 {
		signer, err := ssh.ParsePrivateKey(privKeyPEM)
		if err == nil {
			return privKeyPEM, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), false
		}
	}

	// Auto-generate a fresh Ed25519 keypair if none configured
	privPEM, pubStr, err := GenerateSSHKeypair()
	if err == nil {
		log.Printf("==> [GitOps] No usable SSH key found. Generated a throwaway Ed25519 keypair for this run only.\n")
		return privPEM, pubStr, true
	}

	return nil, "", false
}

func waitForSSH(ctx context.Context, host string, port int, timeout time.Duration) error {
	// JoinHostPort brackets IPv6 literals correctly (host:port breaks on "::1").
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("timed out waiting for SSH port %s", addr)
}

// hostRun is one host's reconcile: what it found, and how it failed.
type hostRun struct {
	name string
	ip   string // the host's public address; "" when it has none (not provisioned by reconcile, no address:)
	user string
	port int
	err  error
	// net is the host's bridge; exec and deployer reach the host (over SSH once it was bootstrapped).
	net      config.NetworkConfig
	exec     remote.Executor
	deployer *Deployer
}

// sshAccess is the SSH user and port a host is reached on: its own, else root and 22.
func sshAccess(h config.HostConfig) (string, int) {
	user, port := "root", 22
	if h.SSHUser != "" {
		user = h.SSHUser
	}
	if h.SSHPort > 0 {
		port = h.SSHPort
	}
	return user, port
}

// Reconcile brings each of fleet.yml's hosts up on its own, in the order of their names: Level 0 (the host
// exists: provisioned with its daemon from cloud-init, or adopted at its address), then the fleet's DNS,
// then each host's Incus runtime over SSH. With several hosts a host that fails is reported and the
// others carry on; the run fails at the end, naming every host that failed. The fleet's `domain:` (its
// apex and wildcard) points at the default host. Services are deployed by each host's daemon, or here
// with the break-glass DeployServices (a single host only).
func (r *Reconciler) Reconcile(ctx context.Context) error {
	log.Printf("==> [GitOps] Starting complete fleet reconciliation from %s\n", r.configDir)

	fleet, err := config.LoadFleetConfig(r.configDir)
	if err != nil {
		return fmt.Errorf("load fleet config: %w", err)
	}

	if r.DeployServices && len(fleet.Hosts) >= 2 {
		// The break-glass path deploys every service to one host over SSH; with several hosts that would
		// put every host's services on it. Each host's daemon applies its own share instead.
		return fmt.Errorf("--deploy-services deploys every service to one host, and fleet.yml has %d hosts; deploy through each host's daemon (a deploy tag), which applies only that host's services", len(fleet.Hosts))
	}

	privKeyPEM, pubKeyStr, generatedKey := r.sshCreds()
	names := fleet.HostNames()
	several := len(names) >= 2
	// failed reports a host's failure. With one host it is the run's error, as it always was; with several
	// the other hosts carry on and the run fails at the end.
	failed := func(h *hostRun, err error) error {
		h.err = err
		if !several {
			return err
		}
		log.Printf("    Host %s failed: %v (the other hosts carry on; the run will fail at the end)\n", h.name, err)
		return nil
	}

	// 1. Level 0: each host exists.
	runs := make([]*hostRun, 0, len(names))
	for _, name := range names {
		hostCfg := fleet.Hosts[name]
		h := &hostRun{name: name}
		h.user, h.port = sshAccess(hostCfg)
		runs = append(runs, h)
		log.Printf("==> [GitOps] Reconciling host: %s (provider=%s)\n", name, hostCfg.Provider)
		if h.ip, err = r.provisionHost(ctx, fleet, name, hostCfg, h.user, h.port, privKeyPEM, pubKeyStr, generatedKey); err != nil {
			if err := failed(h, err); err != nil {
				return err
			}
		}
	}

	// 2. Sync the fleet domain's DNS to the default host. A failure is remembered and returned at the end:
	// the hosts can still be brought up, but the run must not report success with stale DNS.
	var dnsErr error
	dnsHost := hostRunOf(runs, fleet.DefaultHostName())
	if several && fleet.Domain != "" && dnsHost != nil && dnsHost.err != nil {
		dnsErr = fmt.Errorf("%s points at the default host %s, which failed", fleet.Domain, dnsHost.name)
		log.Printf("==> [GitOps] Not syncing DNS for %s: the default host %s failed\n", fleet.Domain, dnsHost.name)
	} else if dnsHost != nil && dnsHost.ip != "" && fleet.Domain != "" {
		log.Printf("==> [GitOps] Syncing DNS records for %s -> %s\n", fleet.Domain, dnsHost.ip)
		dnsProv, err := r.dnsFor(fleet)
		if err != nil && !several {
			return err
		}
		if err == nil {
			err = syncDNS(ctx, dnsProv, fleet.Domain, dnsHost.ip)
		}
		if dnsErr = err; dnsErr != nil {
			log.Printf("    DNS sync failed: %v (continuing with services; the run will fail at the end)\n", dnsErr)
		} else {
			log.Printf("    DNS synchronized successfully.\n")
		}
	}

	// 3. Level 1: each host's Incus runtime, over SSH.
	for _, h := range runs {
		if h.err != nil {
			continue
		}
		if err := r.bootstrapHost(ctx, fleet, h, privKeyPEM); err != nil {
			if err := failed(h, err); err != nil {
				return err
			}
		}
		if h.exec != nil && h.exec != r.exec {
			defer h.exec.Close()
		}
	}
	var hostErrs []error
	for _, h := range runs {
		if h.err != nil {
			hostErrs = append(hostErrs, fmt.Errorf("host %s: %w", h.name, h.err))
		}
	}
	if len(hostErrs) > 0 {
		err := fmt.Errorf("%d of %d hosts failed: %w", len(hostErrs), len(runs), errors.Join(hostErrs...))
		if dnsErr != nil {
			err = fmt.Errorf("%w; and DNS sync failed: %v", err, dnsErr)
		}
		return err
	}

	// The break-glass deploy has at most one host (several are refused above), or none: then it runs here.
	deployer, activeExec := r.deployer, r.exec
	if len(runs) == 1 {
		deployer, activeExec = runs[0].deployer, runs[0].exec
	} else if hostNet, _ := fleet.LocalNetwork(""); hostNet.BridgeName != "" {
		if err := deployer.SetNetwork(hostNet); err != nil {
			return fmt.Errorf("host : %w", err) // as before the per-host loop, which named no host here
		}
	}

	if !r.DeployServices {
		if dnsErr != nil {
			return fmt.Errorf("hosts reconciled, but DNS sync failed: %w", dnsErr)
		}
		log.Printf("==> [GitOps] Hosts and DNS reconciled. Services are deployed through the daemon: plan and apply an approved plan (native-ops remote plan / apply). Pass --deploy-services to deploy them directly over SSH instead (break-glass: no plan, approval or job record).\n")
		return nil
	}
	log.Printf("==> [GitOps] --deploy-services: deploying services directly, without the daemon's plan approval\n")
	servicesDir := filepath.Join(r.configDir, "services")
	entries, err := os.ReadDir(servicesDir)
	if err == nil {
		var serviceConfigs []*config.ServiceConfig
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			svcFile := filepath.Join(servicesDir, entry.Name(), "service.yml")
			if _, err := os.Stat(svcFile); os.IsNotExist(err) {
				continue
			}

			svcCfg, err := config.LoadServiceConfig(svcFile)
			if err != nil {
				return fmt.Errorf("load service %s: %w", svcFile, err)
			}
			if svcCfg.Name == "" {
				svcCfg.Name = entry.Name()
			}
			serviceConfigs = append(serviceConfigs, svcCfg)
		}

		// Sort so 'edge' is deployed first to allow downstream services to register routes
		sort.SliceStable(serviceConfigs, func(i, j int) bool {
			if serviceConfigs[i].Name == "edge" {
				return true
			}
			if serviceConfigs[j].Name == "edge" {
				return false
			}
			return serviceConfigs[i].Name < serviceConfigs[j].Name
		})

		for _, svcCfg := range serviceConfigs {
			if err := deployer.DeployService(ctx, svcCfg, r.configDir); err != nil {
				return fmt.Errorf("deploy service %s: %w", svcCfg.Name, err)
			}
		}

		// The edge's own config (edge/Caddyfile) last, so the routes it adds -- the daemon's included, on a
		// first bootstrap -- point at services that now exist. Normally a deploy tag's plan applies it.
		if _, err := os.Stat(filepath.Join(r.configDir, "edge", "Caddyfile")); err == nil {
			log.Printf("==> [GitOps] Applying edge/Caddyfile to the edge container\n")
			if err := ApplyEdgeConfig(ctx, activeExec, r.configDir, "edge"); err != nil {
				return fmt.Errorf("apply edge/Caddyfile: %w", err)
			}
		}

		diagEdge, _ := activeExec.Run(ctx, "sleep 10; echo '--- TAIL 40 CADDY LOG ---'; incus exec edge -- tail -n 40 /var/log/caddy.log || true")
		log.Printf("==> [GitOps] Edge container diagnostics:\n%s\n", diagEdge)
	}

	if dnsErr != nil {
		return fmt.Errorf("services reconciled, but DNS sync failed: %w", dnsErr)
	}
	log.Printf("==> [GitOps] Full fleet reconciliation completed successfully!\n")
	return nil
}

func hostRunOf(runs []*hostRun, name string) *hostRun {
	for _, h := range runs {
		if h.name == name {
			return h
		}
	}
	return nil
}

// provisionHost makes sure one host exists and returns its public address: the one fleet.yml gives, or the
// provider's (creating the host, with its daemon from cloud-init, when there is none). "" means the host
// has no address reconcile knows (a provider it does not provision).
func (r *Reconciler) provisionHost(ctx context.Context, fleet *config.FleetConfig, hostName string, hostCfg config.HostConfig, sshUser string, sshPort int, privKeyPEM []byte, pubKeyStr string, generatedKey bool) (string, error) {
	if hostCfg.Address != "" && hostCfg.Address != "auto" {
		return hostCfg.Address, nil
	}
	if hostCfg.Provider != "digitalocean" && hostCfg.Provider != "do" {
		return "", nil
	}
	var ip string
	// Query provider to see if host exists
	do, err := digitalocean.New("")
	if err != nil {
		return "", fmt.Errorf("digitalocean provider init: %w", err)
	}
	hosts, err := do.ListHosts(ctx)
	if err != nil {
		return "", fmt.Errorf("list hosts: %w", err)
	}
	existing, action, err := classifyHost(hosts, hostName)
	if err != nil {
		return "", err
	}
	switch action {
	case hostUse:
		ip = existing.PublicIP
		log.Printf("    Host %s already active at IP %s\n", hostName, ip)
	case hostWait:
		log.Printf("    Host %s (id %s) is still provisioning; waiting for it instead of creating another\n", hostName, existing.ID)
		h, err := waitHostActive(ctx, do, existing.ID, 5*time.Minute, 5*time.Second)
		if err != nil {
			return "", err
		}
		ip = h.PublicIP
	}

	// A host that exists must be reachable with our credentials. If it is not,
	// stop and say so: reconcile never destroys or rebuilds a host on its own.
	if ip != "" && len(privKeyPEM) > 0 {
		testExec, testErr := r.dialSSH(ip, sshPort, sshUser, privKeyPEM)
		if testErr == nil {
			if _, err := testExec.Run(ctx, "true"); err != nil {
				testErr = err
			}
			testExec.Close()
		}
		if testErr != nil {
			return "", fmt.Errorf("host %s (%s) exists but SSH verification failed: %v. Not destroying or re-provisioning it automatically (it may hold data): fix the SSH key or user, or remove the host yourself if you want it rebuilt", hostName, ip, testErr)
		}
	}
	if ip != "" {
		return ip, nil
	}

	// Not found: provision it
	log.Printf("    Host %s not found. Provisioning with cloud-init...\n", hostName)
	spec := config.HostSpec{
		Name:     hostName,
		Provider: "digitalocean",
		Size:     fleet.Providers.DigitalOcean.DefaultSize,
		Region:   fleet.Providers.DigitalOcean.Region,
	}
	// The cloud-init opens the firewall for this host's own bridge, and, with fleet.yml's daemon
	// section, installs the daemon.
	var di *DaemonInstall
	if fleet.Daemon != nil {
		if di, err = DaemonInstallFor(fleet.Daemon); err != nil {
			return "", fmt.Errorf("provision host %s: daemon: %w", hostName, err)
		}
		// With several hosts each daemon must know which it is; its unit carries that
		// (a deploy's daemon.flags keep it, see pkg/serveflags).
		if len(fleet.Hosts) >= 2 {
			di.ServeFlags = strings.TrimSpace(di.ServeFlags + " --host-name " + hostName)
			if err := di.Validate(); err != nil {
				return "", fmt.Errorf("provision host %s: daemon: %w", hostName, err)
			}
		}
	}
	ud, err := GenerateCloudInitUserDataFor(pubKeyStr, fleet.HostNetwork(hostName), di)
	if err != nil {
		return "", fmt.Errorf("provision host %s: %w", hostName, err)
	}
	spec.UserData = ud
	if di != nil {
		log.Printf("    The host's cloud-init installs the native-ops daemon %s\n", fleet.Daemon.Version)
	}
	// A throwaway key is acceptable here: this run uses it to SSH in. But a failure to
	// register the key must stop the run BEFORE a droplet nobody can log in to is created.
	if err := r.hostMgr.PrepareAccess(ctx, &spec, pubKeyStr, generatedKey, true); err != nil {
		return "", fmt.Errorf("provision host %s: %w", hostName, err)
	}
	newHost, err := r.hostMgr.CreateHost(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("provision host %s: %w", hostName, err)
	}
	return newHost.PublicIP, nil
}

// bootstrapHost prepares one host's Incus runtime over SSH (when it has a remote address and there is a
// key) and sets h's executor and deployer for it. A host reconcile cannot reach (no address, or the
// local machine) keeps the reconciler's own executor.
func (r *Reconciler) bootstrapHost(ctx context.Context, fleet *config.FleetConfig, h *hostRun, privKeyPEM []byte) error {
	// The bridge is the host's own `network:` (fleet.yml's, or the host's override).
	h.net = fleet.HostNetwork(h.name)
	if h.ip == "" {
		h.net, _ = fleet.LocalNetwork("")
	}
	h.deployer, h.exec = r.deployer, r.exec
	if len(fleet.Hosts) >= 2 {
		h.deployer = NewDeployer(r.exec)
	}
	if h.net.BridgeName != "" {
		if err := h.deployer.SetNetwork(h.net); err != nil {
			return fmt.Errorf("host %s: %w", h.name, err)
		}
	}
	if h.ip == "" || h.ip == "127.0.0.1" || h.ip == "localhost" || len(privKeyPEM) == 0 {
		if len(fleet.Hosts) >= 2 {
			log.Printf("    Host %s is not reached over SSH (no address reconcile knows, the local machine, or no SSH key); its runtime is not bootstrapped from here\n", h.name)
		}
		return nil
	}

	log.Printf("==> [GitOps] Connecting to host %s:%d via SSH (%s)...\n", h.ip, h.port, h.user)
	if err := r.waitSSH(ctx, h.ip, h.port, 120*time.Second); err != nil {
		return fmt.Errorf("wait for host SSH: %w", err)
	}

	// Allow cloud-init to insert authorized_keys if just booted
	var sshExec remote.Executor
	var sshErr error
	for attempt := 1; attempt <= 10; attempt++ {
		sshExec, sshErr = r.dialSSH(h.ip, h.port, h.user, privKeyPEM)
		if sshErr == nil {
			break
		}
		time.Sleep(r.sshRetryDelay)
	}
	if sshErr != nil {
		return fmt.Errorf("init SSH executor to %s: %w", h.ip, sshErr)
	}
	h.exec = sshExec

	// Pre-flight host initialization
	log.Printf("==> [GitOps] Verifying host runtime on %s...\n", h.ip)
	bootstrap, err := hostBootstrapCommands(h.net)
	if err != nil {
		return fmt.Errorf("host %s: %w", h.name, err)
	}
	for _, cmd := range bootstrap {
		_, _ = sshExec.Run(ctx, cmd)
	}

	bridge, _, _ := h.net.Bridge()
	diag, _ := sshExec.Run(ctx, "echo '--- NETWORK ---'; incus network show "+bridge+"; echo '--- DEFAULT PROFILE ---'; incus profile show default; echo '--- STORAGE ---'; incus storage list")
	log.Printf("==> [GitOps] Host runtime diagnostics:\n%s\n", diag)

	h.deployer = NewDeployer(sshExec)
	if err := h.deployer.SetNetwork(h.net); err != nil {
		return fmt.Errorf("host %s: %w", h.name, err)
	}
	return nil
}
