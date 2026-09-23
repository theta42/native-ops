package engine

import (
	"context"
	"fmt"
	"log"

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
func GenerateCloudInitUserData() string {
	return `#cloud-config
package_update: true
package_upgrade: true
packages:
  - curl
  - ufw
  - git
  - jq
  - ca-certificates

runcmd:
  # 1. Install Zabbly Incus repo
  - mkdir -p /etc/apt/keyrings
  - curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc
  - echo "deb [signed-by=/etc/apt/keyrings/zabbly.asc] https://pkgs.zabbly.com/incus/stable $(lsb_release -cs) main" > /etc/apt/sources.list.d/zabbly-incus-stable.list
  - apt-get update
  - apt-get install -y incus

  # 2. Configure Firewall
  - ufw default deny incoming
  - ufw default allow outgoing
  - ufw allow 22/tcp
  - ufw allow 80/tcp
  - ufw allow 443/tcp
  - ufw --force enable
`
}

func (h *HostManager) CreateHost(ctx context.Context, spec config.HostSpec) (*provider.Host, error) {
	log.Printf("==> [Host] Provisioning %s host: %s (size=%s)\n", spec.Provider, spec.Name, spec.Size)

	if spec.UserData == "" {
		spec.UserData = GenerateCloudInitUserData()
	}

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
