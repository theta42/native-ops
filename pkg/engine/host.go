package engine

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/provider"
	"github.com/theta42/native-ops/pkg/provider/digitalocean"
	"github.com/theta42/native-ops/pkg/provider/proxmox"
)

// HostManager provisions and manages host VMs / Droplets across providers.
type HostManager struct {
	doClient  *digitalocean.Client
	pveClient *proxmox.Client
}

func NewHostManager() *HostManager {
	do, _ := digitalocean.New("")
	pve, _ := proxmox.New("", "", "", false)
	return &HostManager{
		doClient:  do,
		pveClient: pve,
	}
}

// GenerateCloudInitUserData produces the cloud-init script for Debian 13 Incus host setup.
func GenerateCloudInitUserData(sshPubKey string) string {
	var sb strings.Builder
	sb.WriteString("#cloud-config\n")
	sb.WriteString("chpasswd:\n  expire: false\n")
	sb.WriteString("ssh_pwauth: false\n")
	sb.WriteString("package_update: true\n")
	sb.WriteString("package_upgrade: true\n")

	if sshPubKey != "" {
		sb.WriteString("ssh_authorized_keys:\n")
		sb.WriteString(fmt.Sprintf("  - %s\n", strings.TrimSpace(sshPubKey)))
	}

	sb.WriteString(`packages:
  - curl
  - ufw
  - git
  - jq
  - ca-certificates

runcmd:
  # 0. Ensure root account is active without password expiration
  - chage -I -1 -m 0 -M 99999 -E -1 root || true
  - passwd -d root || true

  # 1. Install Zabbly Incus repo
  - mkdir -p /etc/apt/keyrings
  - curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc
  - echo "deb [signed-by=/etc/apt/keyrings/zabbly.asc] https://pkgs.zabbly.com/incus/stable $(lsb_release -cs) main" > /etc/apt/sources.list.d/zabbly-incus-stable.list
  - apt-get update
  - apt-get install -y incus

  # 2. Configure Firewall
  - ufw default deny incoming
  - ufw default allow outgoing
  - ufw allow in on incusbr0
  - ufw route allow in on incusbr0
  - ufw route allow out on incusbr0
  - ufw allow 22/tcp
  - ufw allow 80/tcp
  - ufw allow 443/tcp
  - ufw --force enable
`)
	return sb.String()
}

// sshKeyRegistrar is the part of a provider that can register a public key with the account.
type sshKeyRegistrar interface {
	EnsureSSHKey(ctx context.Context, name, pubKey string) (string, error)
}

// prepareAccess gives a new host a way in: the operator's public key is
// registered with the provider, attached to the host, and authorized by the
// cloud-init. Without it a DigitalOcean droplet comes up with a random,
// already-expired root password and cannot be logged in to at all. Any failure
// returns an error before anything is created, and spec is only modified on success.
// A throwaway (generated) key is refused unless allowGenerated: a host that
// trusts only a key nobody holds is unreachable for good.
func prepareAccess(ctx context.Context, reg sshKeyRegistrar, spec *config.HostSpec, pubKey string, generated, allowGenerated bool) error {
	if strings.TrimSpace(pubKey) == "" {
		return fmt.Errorf("no SSH public key available for the new host")
	}
	if generated && !allowGenerated {
		return fmt.Errorf("no SSH key is configured, so the host would be created trusting a throwaway key that is discarded when this command exits and could never be logged in to: set SSH_PRIVATE_KEY (or FLEET_SSH_KEY) to the private key you will use, or put one at ~/.ssh/id_ed25519")
	}
	fp, err := reg.EnsureSSHKey(ctx, spec.Name+"-key", pubKey)
	if err != nil {
		return fmt.Errorf("register the SSH key with the provider (a host created without one cannot be logged in to): %w", err)
	}
	if fp == "" {
		return fmt.Errorf("the provider returned no fingerprint for the SSH key; not creating a host nobody can log in to")
	}
	keys := spec.SSHKeyNames
	found := false
	for _, k := range keys {
		found = found || k == fp
	}
	if !found {
		keys = append(append([]string{}, keys...), fp)
	}
	spec.SSHKeyNames = keys
	if spec.UserData == "" {
		spec.UserData = GenerateCloudInitUserData(pubKey)
	}
	return nil
}

// PrepareAccess makes a host spec reachable before it is created (see
// prepareAccess). It only acts for DigitalOcean; other providers are left as they are.
func (h *HostManager) PrepareAccess(ctx context.Context, spec *config.HostSpec, pubKey string, generated, allowGenerated bool) error {
	switch spec.Provider {
	case "digitalocean", "do":
		if h.doClient == nil {
			return fmt.Errorf("DigitalOcean provider not initialized (set DO_API_TOKEN)")
		}
		return prepareAccess(ctx, h.doClient, spec, pubKey, generated, allowGenerated)
	}
	return nil
}

func (h *HostManager) CreateHost(ctx context.Context, spec config.HostSpec) (*provider.Host, error) {
	log.Printf("==> [Host] Provisioning %s host: %s (size=%s)\n", spec.Provider, spec.Name, spec.Size)

	var p provider.ComputeProvider
	switch spec.Provider {
	case "digitalocean", "do":
		if h.doClient == nil {
			return nil, fmt.Errorf("DigitalOcean provider not initialized (set DO_API_TOKEN)")
		}
		p = h.doClient
	case "proxmox", "pve":
		if h.pveClient == nil {
			return nil, fmt.Errorf("Proxmox provider not initialized (set PVE_ENDPOINT and PVE_API_TOKEN)")
		}
		p = h.pveClient
	default:
		return nil, fmt.Errorf("unsupported provider: %s", spec.Provider)
	}

	host, err := p.CreateHost(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("create host failed: %w", err)
	}

	log.Printf("==> [Host] Host %s successfully created! (IP: %s, Status: %s)\n", host.Name, host.PublicIP, host.Status)
	return host, nil
}

func (h *HostManager) DestroyHost(ctx context.Context, providerName, hostID string) error {
	log.Printf("==> [Host] Destroying %s host ID: %s\n", providerName, hostID)
	switch providerName {
	case "digitalocean", "do":
		return h.doClient.DestroyHost(ctx, hostID)
	case "proxmox", "pve":
		return h.pveClient.DestroyHost(ctx, hostID)
	default:
		return fmt.Errorf("unsupported provider: %s", providerName)
	}
}
