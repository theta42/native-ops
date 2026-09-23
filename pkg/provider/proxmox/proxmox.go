package proxmox

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/provider"
)

// Client implements ComputeProvider for Proxmox VE.
type Client struct {
	endpoint   string // e.g. "https://pve.example.com:8006"
	node       string // e.g. "pve-01"
	apiToken   string // e.g. "root@pam!nativeops=xxxx-xxxx-xxxx"
	httpClient *http.Client
}

// New creates a Proxmox client from environment or parameters.
func New(endpoint, node, apiToken string, insecureTLS bool) (*Client, error) {
	if endpoint == "" {
		endpoint = os.Getenv("PVE_ENDPOINT")
	}
	if node == "" {
		node = os.Getenv("PVE_NODE")
	}
	if apiToken == "" {
		apiToken = os.Getenv("PVE_API_TOKEN")
	}

	if endpoint == "" {
		return nil, fmt.Errorf("proxmox endpoint required (set PVE_ENDPOINT)")
	}
	if node == "" {
		node = "pve"
	}
	if apiToken == "" {
		return nil, fmt.Errorf("proxmox API token required (set PVE_API_TOKEN)")
	}

	endpoint = strings.TrimSuffix(endpoint, "/")

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: insecureTLS,
		},
	}

	return &Client{
		endpoint: endpoint,
		node:     node,
		apiToken: apiToken,
		httpClient: &http.Client{
			Timeout:   60 * time.Second,
			Transport: tr,
		},
	}, nil
}

func (c *Client) Name() string {
	return "proxmox"
}

type pveResponse struct {
	Data json.RawMessage `json:"data"`
}

func (c *Client) request(ctx context.Context, method, path string, bodyIn any, bodyOut any) error {
	var bodyReader io.Reader
	if bodyIn != nil {
		data, err := json.Marshal(bodyIn)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	url := fmt.Sprintf("%s/api2/json%s", c.endpoint, path)
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "PVEAPIToken="+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("Proxmox API error (status %d): %s", resp.StatusCode, string(respData))
	}

	if bodyOut != nil && len(respData) > 0 {
		var wrapper pveResponse
		if err := json.Unmarshal(respData, &wrapper); err != nil {
			return fmt.Errorf("unmarshal PVE wrapper: %w", err)
		}
		if err := json.Unmarshal(wrapper.Data, bodyOut); err != nil {
			return fmt.Errorf("unmarshal PVE data payload: %w", err)
		}
	}

	return nil
}

type pveVM struct {
	VMID      int     `json:"vmid"`
	Name      string  `json:"name"`
	Status    string  `json:"status"` // "running", "stopped"
	CPUs      int     `json:"cpus"`
	MaxMem    int64   `json:"maxmem"`
	MaxDisk   int64   `json:"maxdisk"`
	NetIn     int64   `json:"netin"`
	NetOut    int64   `json:"netout"`
	Uptime    int64   `json:"uptime"`
}

func (c *Client) toHost(vm *pveVM) *provider.Host {
	status := "off"
	if vm.Status == "running" {
		status = "active"
	}

	memMB := vm.MaxMem / (1024 * 1024)
	return &provider.Host{
		ID:        strconv.Itoa(vm.VMID),
		Name:      vm.Name,
		Provider:  "proxmox",
		Status:    status,
		Size:      fmt.Sprintf("%dc-%dmb", vm.CPUs, memMB),
		CreatedAt: time.Now(),
		Metadata: map[string]string{
			"node": c.node,
		},
	}
}

func (c *Client) CreateHost(ctx context.Context, spec config.HostSpec) (*provider.Host, error) {
	// Allocate next VMID or parse from spec
	var nextID int
	if err := c.request(ctx, http.MethodGet, "/cluster/nextid", nil, &nextID); err != nil {
		return nil, fmt.Errorf("get next VMID: %w", err)
	}

	cores := spec.Cores
	if cores <= 0 {
		cores = 2
	}
	memMB := spec.MemoryMB
	if memMB <= 0 {
		memMB = 4096
	}

	payload := map[string]any{
		"vmid":    nextID,
		"name":    spec.Name,
		"cores":   cores,
		"memory":  memMB,
		"net0":    "virtio,bridge=vmbr0,firewall=1",
		"scsihw":  "virtio-scsi-pci",
		"ostype":  "l26",
		"agent":   "1",
	}

	path := fmt.Sprintf("/nodes/%s/qemu", c.node)
	if err := c.request(ctx, http.MethodPost, path, payload, nil); err != nil {
		return nil, fmt.Errorf("create QEMU VM %d: %w", nextID, err)
	}

	// Start VM
	startPath := fmt.Sprintf("/nodes/%s/qemu/%d/status/start", c.node, nextID)
	if err := c.request(ctx, http.MethodPost, startPath, nil, nil); err != nil {
		return nil, fmt.Errorf("start VM %d: %w", nextID, err)
	}

	return c.GetHost(ctx, strconv.Itoa(nextID))
}

func (c *Client) GetHost(ctx context.Context, hostID string) (*provider.Host, error) {
	vmid, err := strconv.Atoi(hostID)
	if err != nil {
		return nil, fmt.Errorf("invalid VMID: %s", hostID)
	}

	var status pveVM
	path := fmt.Sprintf("/nodes/%s/qemu/%d/status/current", c.node, vmid)
	if err := c.request(ctx, http.MethodGet, path, nil, &status); err != nil {
		return nil, fmt.Errorf("get status for VM %d: %w", vmid, err)
	}
	status.VMID = vmid

	return c.toHost(&status), nil
}

func (c *Client) ListHosts(ctx context.Context) ([]*provider.Host, error) {
	var vms []pveVM
	path := fmt.Sprintf("/nodes/%s/qemu", c.node)
	if err := c.request(ctx, http.MethodGet, path, nil, &vms); err != nil {
		return nil, fmt.Errorf("list VMs on node %s: %w", c.node, err)
	}

	var hosts []*provider.Host
	for i := range vms {
		hosts = append(hosts, c.toHost(&vms[i]))
	}
	return hosts, nil
}

func (c *Client) ResizeHost(ctx context.Context, hostID string, newSize string) error {
	vmid, err := strconv.Atoi(hostID)
	if err != nil {
		return fmt.Errorf("invalid VMID: %s", hostID)
	}

	// Parse size e.g. "4c-8192mb" or "4"
	var cores int
	var memMB int
	if strings.Contains(newSize, "c") {
		parts := strings.Split(newSize, "-")
		cores, _ = strconv.Atoi(strings.TrimSuffix(parts[0], "c"))
		if len(parts) > 1 {
			memMB, _ = strconv.Atoi(strings.TrimSuffix(parts[1], "mb"))
		}
	} else {
		cores, _ = strconv.Atoi(newSize)
	}

	payload := map[string]any{}
	if cores > 0 {
		payload["cores"] = cores
	}
	if memMB > 0 {
		payload["memory"] = memMB
	}

	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", c.node, vmid)
	if err := c.request(ctx, http.MethodPut, path, payload, nil); err != nil {
		return fmt.Errorf("update VM %d config: %w", vmid, err)
	}

	return nil
}

func (c *Client) DestroyHost(ctx context.Context, hostID string) error {
	vmid, err := strconv.Atoi(hostID)
	if err != nil {
		return fmt.Errorf("invalid VMID: %s", hostID)
	}

	// Stop first
	stopPath := fmt.Sprintf("/nodes/%s/qemu/%d/status/stop", c.node, vmid)
	_ = c.request(ctx, http.MethodPost, stopPath, nil, nil)

	time.Sleep(2 * time.Second)

	path := fmt.Sprintf("/nodes/%s/qemu/%d", c.node, vmid)
	if err := c.request(ctx, http.MethodDelete, path, nil, nil); err != nil {
		return fmt.Errorf("delete VM %d: %w", vmid, err)
	}

	return nil
}
