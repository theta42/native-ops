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

// Reconciler performs end-to-end GitOps cluster reconciliation (Level 0 + DNS + Level 1).
type Reconciler struct {
	configDir string
	exec      remote.Executor
	deployer  *Deployer
	hostMgr   *HostManager
}

func NewReconciler(configDir string, exec remote.Executor) *Reconciler {
	return &Reconciler{
		configDir: configDir,
		exec:      exec,
		deployer:  NewDeployer(exec),
		hostMgr:   NewHostManager(),
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

func getSSHCredentials() ([]byte, string) {
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

	var pubKeyStr string
	if len(privKeyPEM) > 0 {
		signer, err := ssh.ParsePrivateKey(privKeyPEM)
		if err == nil {
			pubKeyStr = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
			return privKeyPEM, pubKeyStr
		}
	}

	// Auto-generate fresh Ed25519 keypair if none configured
	privPEM, pubStr, err := GenerateSSHKeypair()
	if err == nil {
		log.Printf("==> [GitOps] No existing SSH key found. Automatically generated fresh Ed25519 keypair for fleet.\n")
		return privPEM, pubStr
	}

	return nil, ""
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

	privKeyPEM, pubKeyStr := getSSHCredentials()
	var primaryHostIP string
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
				log.Printf("    Host %s already active at IP %s\n", hostName, primaryHostIP)
			case hostWait:
				log.Printf("    Host %s (id %s) is still provisioning; waiting for it instead of creating another\n", hostName, existing.ID)
				h, err := waitHostActive(ctx, do, existing.ID, 5*time.Minute, 5*time.Second)
				if err != nil {
					return err
				}
				primaryHostIP = h.PublicIP
			}

			// A host that exists must be reachable with our credentials. If it is not,
			// stop and say so: reconcile never destroys or rebuilds a host on its own.
			if primaryHostIP != "" && len(privKeyPEM) > 0 {
				testExec, testErr := remote.NewSSHExecutor(primaryHostIP, hostSSHPort, hostSSHUser, privKeyPEM)
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
				var sshKeyFingerprints []string
				if pubKeyStr != "" {
					fp, err := do.EnsureSSHKey(ctx, hostName+"-key", pubKeyStr)
					if err == nil && fp != "" {
						sshKeyFingerprints = append(sshKeyFingerprints, fp)
					}
				}
				spec := config.HostSpec{
					Name:        hostName,
					Provider:    "digitalocean",
					Size:        fleet.Providers.DigitalOcean.DefaultSize,
					Region:      fleet.Providers.DigitalOcean.Region,
					UserData:    GenerateCloudInitUserData(pubKeyStr),
					SSHKeyNames: sshKeyFingerprints,
				}
				newHost, err := r.hostMgr.CreateHost(ctx, spec)
				if err != nil {
					return fmt.Errorf("provision host %s: %w", hostName, err)
				}
				primaryHostIP = newHost.PublicIP
			}
		}
	}

	// 2. Sync DNS. A failure is remembered and returned at the end: services can
	// still be deployed, but the run must not report success with stale DNS.
	var dnsErr error
	if primaryHostIP != "" && fleet.Domain != "" {
		log.Printf("==> [GitOps] Syncing DNS records for %s -> %s\n", fleet.Domain, primaryHostIP)
		var dnsProv provider.DNSProvider
		if fleet.DNSProvider == "digitalocean" || fleet.DNSProvider == "do" {
			do, err := digitalocean.New("")
			if err != nil {
				return fmt.Errorf("DO DNS init: %w", err)
			}
			dnsProv = do
		} else {
			p, err := plugin.NewScriptDNSProvider(fleet.DNSProvider, r.configDir)
			if err != nil {
				return fmt.Errorf("custom DNS plugin init: %w", err)
			}
			dnsProv = p
		}
		if dnsErr = syncDNS(ctx, dnsProv, fleet.Domain, primaryHostIP); dnsErr != nil {
			log.Printf("    DNS sync failed: %v (continuing with services; the run will fail at the end)\n", dnsErr)
		} else {
			log.Printf("    DNS synchronized successfully.\n")
		}
	}

	// 3. Level 1: Reconcile Declarative Services
	deployer := r.deployer
	var activeExec remote.Executor = r.exec

	// If target host is remote and we have an SSH key, execute deployments over SSH
	if primaryHostIP != "" && primaryHostIP != "127.0.0.1" && primaryHostIP != "localhost" && len(privKeyPEM) > 0 {
		log.Printf("==> [GitOps] Connecting to host %s:%d via SSH (%s)...\n", primaryHostIP, hostSSHPort, hostSSHUser)
		if err := waitForSSH(ctx, primaryHostIP, hostSSHPort, 120*time.Second); err != nil {
			return fmt.Errorf("wait for host SSH: %w", err)
		}

		// Allow cloud-init to insert authorized_keys if just booted
		var sshExec *remote.SSHExecutor
		var sshErr error
		for attempt := 1; attempt <= 10; attempt++ {
			sshExec, sshErr = remote.NewSSHExecutor(primaryHostIP, hostSSHPort, hostSSHUser, privKeyPEM)
			if sshErr == nil {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if sshErr != nil {
			return fmt.Errorf("init SSH executor to %s: %w", primaryHostIP, sshErr)
		}
		defer sshExec.Close()
		activeExec = sshExec

		// Pre-flight host initialization
		log.Printf("==> [GitOps] Verifying host runtime on %s...\n", primaryHostIP)
		for _, cmd := range hostBootstrapCommands() {
			_, _ = sshExec.Run(ctx, cmd)
		}

		diag, _ := sshExec.Run(ctx, "echo '--- NETWORK ---'; incus network show incusbr0; echo '--- DEFAULT PROFILE ---'; incus profile show default; echo '--- STORAGE ---'; incus storage list")
		log.Printf("==> [GitOps] Host runtime diagnostics:\n%s\n", diag)

		deployer = NewDeployer(sshExec)
	}

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

		diagEdge, _ := activeExec.Run(ctx, "sleep 10; echo '--- TAIL 40 CADDY LOG ---'; incus exec edge -- tail -n 40 /var/log/caddy.log || true")
		log.Printf("==> [GitOps] Edge container diagnostics:\n%s\n", diagEdge)
	}

	if dnsErr != nil {
		return fmt.Errorf("services reconciled, but DNS sync failed: %w", dnsErr)
	}
	log.Printf("==> [GitOps] Full fleet reconciliation completed successfully!\n")
	return nil
}

