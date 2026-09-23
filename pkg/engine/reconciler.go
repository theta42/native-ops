package engine

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
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
		}
	}

	return privKeyPEM, pubKeyStr
}

func waitForSSH(ctx context.Context, host string, port int, timeout time.Duration) error {
	addr := fmt.Sprintf("%s:%d", host, port)
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
			if err == nil {
				for _, h := range hosts {
					if h.Name == hostName && h.Status == "active" {
						primaryHostIP = h.PublicIP
						log.Printf("    Host %s already active at IP %s\n", hostName, primaryHostIP)
						break
					}
				}
			}

			// If not found, provision new host
			if primaryHostIP == "" {
				log.Printf("    Host %s not found in DigitalOcean. Provisioning with cloud-init...\n", hostName)
				spec := config.HostSpec{
					Name:     hostName,
					Provider: "digitalocean",
					Size:     fleet.Providers.DigitalOcean.DefaultSize,
					Region:   fleet.Providers.DigitalOcean.Region,
					UserData: GenerateCloudInitUserData(pubKeyStr),
				}
				newHost, err := r.hostMgr.CreateHost(ctx, spec)
				if err != nil {
					return fmt.Errorf("provision host %s: %w", hostName, err)
				}
				primaryHostIP = newHost.PublicIP
			}
		}
	}

	// 2. Sync DNS
	if primaryHostIP != "" && fleet.Domain != "" {
		log.Printf("==> [GitOps] Syncing DNS records for %s -> %s\n", fleet.Domain, primaryHostIP)
		records := []provider.DNSRecord{
			{Type: "A", Name: "@", Value: primaryHostIP},
			{Type: "A", Name: "*", Value: primaryHostIP},
		}

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

		if err := dnsProv.SyncRecords(ctx, fleet.Domain, records); err != nil {
			log.Printf("    Warning: DNS sync had issue: %v\n", err)
		} else {
			log.Printf("    DNS synchronized successfully.\n")
		}
	}

	// 3. Level 1: Reconcile Declarative Services
	deployer := r.deployer

	// If target host is remote and we have an SSH key, execute deployments over SSH
	if primaryHostIP != "" && primaryHostIP != "127.0.0.1" && primaryHostIP != "localhost" && len(privKeyPEM) > 0 {
		log.Printf("==> [GitOps] Connecting to host %s:%d via SSH (%s)...\n", primaryHostIP, hostSSHPort, hostSSHUser)
		if err := waitForSSH(ctx, primaryHostIP, hostSSHPort, 90*time.Second); err != nil {
			return fmt.Errorf("wait for host SSH: %w", err)
		}

		sshExec, err := remote.NewSSHExecutor(primaryHostIP, hostSSHPort, hostSSHUser, privKeyPEM)
		if err != nil {
			return fmt.Errorf("init SSH executor to %s: %w", primaryHostIP, err)
		}
		defer sshExec.Close()
		deployer = NewDeployer(sshExec)
	}

	servicesDir := filepath.Join(r.configDir, "services")
	entries, err := os.ReadDir(servicesDir)
	if err == nil {
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

			if err := deployer.DeployService(ctx, svcCfg, r.configDir); err != nil {
				return fmt.Errorf("deploy service %s: %w", svcCfg.Name, err)
			}
		}
	}

	log.Printf("==> [GitOps] Full fleet reconciliation completed successfully!\n")
	return nil
}
