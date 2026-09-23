package provider

import (
	"context"
	"time"

	"github.com/theta42/native-ops/pkg/config"
)

// Host represents a provisioned physical or virtual machine.
type Host struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Provider  string            `json:"provider"`
	Status    string            `json:"status"` // "active", "provisioning", "off", "error"
	PublicIP  string            `json:"public_ip"`
	PrivateIP string            `json:"private_ip,omitempty"`
	Region    string            `json:"region,omitempty"`
	Size      string            `json:"size,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	Tags      []string          `json:"tags,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// DNSRecord represents a DNS entry.
type DNSRecord struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"` // "A", "AAAA", "CNAME", "TXT"
	Name     string `json:"name"` // apex "@", wildcard "*", or subdomain
	Value    string `json:"value"`
	TTL      int    `json:"ttl,omitempty"`
	Priority int    `json:"priority,omitempty"`
}

// ComputeProvider abstracts cloud and hypervisor host management.
type ComputeProvider interface {
	Name() string
	CreateHost(ctx context.Context, spec config.HostSpec) (*Host, error)
	ResizeHost(ctx context.Context, hostID string, newSize string) error
	DestroyHost(ctx context.Context, hostID string) error
	GetHost(ctx context.Context, hostID string) (*Host, error)
	ListHosts(ctx context.Context) ([]*Host, error)
}

// DNSProvider abstracts DNS record management.
type DNSProvider interface {
	Name() string
	SyncRecords(ctx context.Context, domain string, records []DNSRecord) error
	ListRecords(ctx context.Context, domain string) ([]DNSRecord, error)
	DeleteRecord(ctx context.Context, domain string, recordID string) error
}
