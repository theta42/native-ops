package engine

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
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

	for hostName := range fleet.Hosts {
		summary.Hosts = append(summary.Hosts, hostName)
	}

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
			}
		}
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

// Reconcile executes Level 0 host verification/creation, DNS sync, and Level 1 service deployment.
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
	var primaryHostIP, primaryHostName string
	var hostSSHUser string = "root"
	var hostSSHPort int = 22

	// 1. Level 0: Reconcile Cloud / Hypervisor Hosts
	for hostName, hostCfg := range fleet.Hosts {
		log.Printf("==> [GitOps] Reconciling host: %s (provider=%s)\n", hostName, hostCfg.Provider)

		if hostCfg.SSHUser != "" {
			hostSSHUser = hostCfg.SSHUser
		}
		if hostCfg.SSHPort > 0 {
			hostSSHPort = hostCfg.SSHPort
		}

		if hostCfg.Address != "" && hostCfg.Address != "auto" {
			primaryHostIP = hostCfg.Address
			primaryHostName = hostName
			continue
		}

		// Query provider to see if host exists
		if hostCfg.Provider == "digitalocean" || hostCfg.Provider == "do" {
			do, err := digitalocean.New("")
			if err != nil {
				return fmt.Errorf("digitalocean provider init: %w", err)
			}
			hosts, err := do.ListHosts(ctx)
			if err != nil {
				return fmt.Errorf("list hosts: %w", err)
			}
			existing, action, err := classifyHost(hosts, hostName)
			if err != nil {
				return err
			}
			switch action {
			case hostUse:
				primaryHostIP = existing.PublicIP
				primaryHostName = hostName
				log.Printf("    Host %s already active at IP %s\n", hostName, primaryHostIP)
			case hostWait:
				log.Printf("    Host %s (id %s) is still provisioning; waiting for it instead of creating another\n", hostName, existing.ID)
				h, err := waitHostActive(ctx, do, existing.ID, 5*time.Minute, 5*time.Second)
				if err != nil {
					return err
				}
				primaryHostIP = h.PublicIP
				primaryHostName = hostName
			}

			// A host that exists must be reachable with our credentials. If it is not,
			// stop and say so: reconcile never destroys or rebuilds a host on its own.
			if primaryHostIP != "" && len(privKeyPEM) > 0 {
				testExec, testErr := r.dialSSH(primaryHostIP, hostSSHPort, hostSSHUser, privKeyPEM)
				if testErr == nil {
					if _, err := testExec.Run(ctx, "true"); err != nil {
						testErr = err
					}
					testExec.Close()
				}
				if testErr != nil {
					return fmt.Errorf("host %s (%s) exists but SSH verification failed: %v. Not destroying or re-provisioning it automatically (it may hold data): fix the SSH key or user, or remove the host yourself if you want it rebuilt", hostName, primaryHostIP, testErr)
				}
			}

			// Not found: provision it
			if primaryHostIP == "" {
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
						return fmt.Errorf("provision host %s: daemon: %w", hostName, err)
					}
					// With several hosts each daemon must know which it is; its unit carries that
					// (a deploy's daemon.flags keep it, see pkg/serveflags).
					if len(fleet.Hosts) >= 2 {
						di.ServeFlags = strings.TrimSpace(di.ServeFlags + " --host-name " + hostName)
						if err := di.Validate(); err != nil {
							return fmt.Errorf("provision host %s: daemon: %w", hostName, err)
						}
					}
				}
				ud, err := GenerateCloudInitUserDataFor(pubKeyStr, fleet.HostNetwork(hostName), di)
				if err != nil {
					return fmt.Errorf("provision host %s: %w", hostName, err)
				}
				spec.UserData = ud
				if di != nil {
					log.Printf("    The host's cloud-init installs the native-ops daemon %s\n", fleet.Daemon.Version)
				}
				// A throwaway key is acceptable here: this run uses it to SSH in. But a failure to
				// register the key must stop the run BEFORE a droplet nobody can log in to is created.
				if err := r.hostMgr.PrepareAccess(ctx, &spec, pubKeyStr, generatedKey, true); err != nil {
					return fmt.Errorf("provision host %s: %w", hostName, err)
				}
				newHost, err := r.hostMgr.CreateHost(ctx, spec)
				if err != nil {
					return fmt.Errorf("provision host %s: %w", hostName, err)
				}
				primaryHostIP = newHost.PublicIP
				primaryHostName = hostName
			}
		}
	}

	// 2. Sync DNS. A failure is remembered and returned at the end: services can
	// still be deployed, but the run must not report success with stale DNS.
	var dnsErr error
	if primaryHostIP != "" && fleet.Domain != "" {
		log.Printf("==> [GitOps] Syncing DNS records for %s -> %s\n", fleet.Domain, primaryHostIP)
		dnsProv, err := r.dnsFor(fleet)
		if err != nil {
			return err
		}
		if dnsErr = syncDNS(ctx, dnsProv, fleet.Domain, primaryHostIP); dnsErr != nil {
			log.Printf("    DNS sync failed: %v (continuing with services; the run will fail at the end)\n", dnsErr)
		} else {
			log.Printf("    DNS synchronized successfully.\n")
		}
	}

	// 3. Level 1: Reconcile Declarative Services
	// The bridge is the host's own `network:` (fleet.yml's, or the host's override).
	hostNet := fleet.HostNetwork(primaryHostName)
	if primaryHostName == "" {
		hostNet, _ = fleet.LocalNetwork("")
	}
	deployer := r.deployer
	if hostNet.BridgeName != "" {
		if err := deployer.SetNetwork(hostNet); err != nil {
			return fmt.Errorf("host %s: %w", primaryHostName, err)
		}
	}
	var activeExec remote.Executor = r.exec

	// If target host is remote and we have an SSH key, execute deployments over SSH
	if primaryHostIP != "" && primaryHostIP != "127.0.0.1" && primaryHostIP != "localhost" && len(privKeyPEM) > 0 {
		log.Printf("==> [GitOps] Connecting to host %s:%d via SSH (%s)...\n", primaryHostIP, hostSSHPort, hostSSHUser)
		if err := r.waitSSH(ctx, primaryHostIP, hostSSHPort, 120*time.Second); err != nil {
			return fmt.Errorf("wait for host SSH: %w", err)
		}

		// Allow cloud-init to insert authorized_keys if just booted
		var sshExec remote.Executor
		var sshErr error
		for attempt := 1; attempt <= 10; attempt++ {
			sshExec, sshErr = r.dialSSH(primaryHostIP, hostSSHPort, hostSSHUser, privKeyPEM)
			if sshErr == nil {
				break
			}
			time.Sleep(r.sshRetryDelay)
		}
		if sshErr != nil {
			return fmt.Errorf("init SSH executor to %s: %w", primaryHostIP, sshErr)
		}
		defer sshExec.Close()
		activeExec = sshExec

		// Pre-flight host initialization
		log.Printf("==> [GitOps] Verifying host runtime on %s...\n", primaryHostIP)
		bootstrap, err := hostBootstrapCommands(hostNet)
		if err != nil {
			return fmt.Errorf("host %s: %w", primaryHostName, err)
		}
		for _, cmd := range bootstrap {
			_, _ = sshExec.Run(ctx, cmd)
		}

		bridge, _, _ := hostNet.Bridge()
		diag, _ := sshExec.Run(ctx, "echo '--- NETWORK ---'; incus network show "+bridge+"; echo '--- DEFAULT PROFILE ---'; incus profile show default; echo '--- STORAGE ---'; incus storage list")
		log.Printf("==> [GitOps] Host runtime diagnostics:\n%s\n", diag)

		deployer = NewDeployer(sshExec)
		if err := deployer.SetNetwork(hostNet); err != nil {
			return fmt.Errorf("host %s: %w", primaryHostName, err)
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
