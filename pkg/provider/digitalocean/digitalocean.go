package digitalocean

import (
	"bytes"
	"context"
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

const (
	apiBaseURL     = "https://api.digitalocean.com/v2"
	defaultTimeout = 60 * time.Second
)

// Client implements ComputeProvider and DNSProvider for DigitalOcean.
type Client struct {
	token      string
	httpClient *http.Client
	baseURL    string // apiBaseURL; a test points it at a fake
}

// New creates a new DigitalOcean client from token or DO_API_TOKEN env.
func New(token string) (*Client, error) {
	if token == "" {
		token = os.Getenv("DO_API_TOKEN")
	}
	if token == "" {
		return nil, fmt.Errorf("digitalocean API token required (set DO_API_TOKEN)")
	}

	return &Client{
		token:   token,
		baseURL: apiBaseURL,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}, nil
}

func (c *Client) Name() string {
	return "digitalocean"
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

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
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
		return fmt.Errorf("DO API error (status %d): %s", resp.StatusCode, string(respData))
	}

	if bodyOut != nil && len(respData) > 0 {
		if err := json.Unmarshal(respData, bodyOut); err != nil {
			return fmt.Errorf("unmarshal response body: %w", err)
		}
	}

	return nil
}

// pageLinks is the part of a DigitalOcean list response that says whether there is another page.
type pageLinks struct {
	Links struct {
		Pages struct {
			Next string `json:"next"`
		} `json:"pages"`
	} `json:"links"`
}

// maxPages bounds a listing, so a misbehaving API cannot keep one going forever.
const maxPages = 100

// listAll fetches every page of a list endpoint (path already carries per_page), handing each page's
// raw JSON to add. A listing that stopped at the first page would make reconcile miss a host it should
// find (and create a duplicate) or a DNS sync miss a record (and create a duplicate).
func (c *Client) listAll(ctx context.Context, path string, add func(json.RawMessage) error) error {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= maxPages; page++ {
		var raw json.RawMessage
		if err := c.request(ctx, http.MethodGet, fmt.Sprintf("%s%spage=%d", path, sep, page), nil, &raw); err != nil {
			return err
		}
		if err := add(raw); err != nil {
			return err
		}
		var links pageLinks
		if err := json.Unmarshal(raw, &links); err != nil || links.Links.Pages.Next == "" {
			return nil
		}
	}
	return fmt.Errorf("listing %s: more than %d pages", path, maxPages)
}

// ComputeProvider implementation

type dropletResponse struct {
	Droplet doDroplet `json:"droplet"`
}

type dropletsListResponse struct {
	Droplets []doDroplet `json:"droplets"`
}

type doDroplet struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Region struct {
		Slug string `json:"slug"`
	} `json:"region"`
	SizeSlug  string    `json:"size_slug"`
	CreatedAt time.Time `json:"created_at"`
	Networks  struct {
		V4 []struct {
			IPAddress string `json:"ip_address"`
			Type      string `json:"type"` // "public" or "private"
		} `json:"v4"`
	} `json:"networks"`
	Tags []string `json:"tags"`
}

func (c *Client) toHost(d *doDroplet) *provider.Host {
	host := &provider.Host{
		ID:        strconv.Itoa(d.ID),
		Name:      d.Name,
		Provider:  "digitalocean",
		Status:    d.Status,
		Region:    d.Region.Slug,
		Size:      d.SizeSlug,
		CreatedAt: d.CreatedAt,
		Tags:      d.Tags,
	}

	for _, net := range d.Networks.V4 {
		if net.Type == "public" && host.PublicIP == "" {
			host.PublicIP = net.IPAddress
		} else if net.Type == "private" && host.PrivateIP == "" {
			host.PrivateIP = net.IPAddress
		}
	}

	return host
}

type doSSHKey struct {
	ID          int    `json:"id"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
	Name        string `json:"name"`
}

type doSSHKeysResponse struct {
	SSHKeys []doSSHKey `json:"ssh_keys"`
}

type doSSHKeyResponse struct {
	SSHKey doSSHKey `json:"ssh_key"`
}

func (c *Client) EnsureSSHKey(ctx context.Context, name, pubKeyStr string) (string, error) {
	if pubKeyStr == "" {
		return "", nil
	}
	if keys, err := c.ListSSHKeys(ctx); err == nil {
		for _, k := range keys {
			if strings.TrimSpace(k.PublicKey) == strings.TrimSpace(pubKeyStr) {
				return k.Fingerprint, nil
			}
		}
	}

	payload := map[string]string{
		"name":       name,
		"public_key": strings.TrimSpace(pubKeyStr),
	}
	var createRes doSSHKeyResponse
	if err := c.request(ctx, http.MethodPost, "/account/keys", payload, &createRes); err != nil {
		// If name collision, try listing again
		if res2, err2 := c.ListSSHKeys(ctx); err2 == nil {
			for _, k := range res2 {
				if strings.TrimSpace(k.PublicKey) == strings.TrimSpace(pubKeyStr) {
					return k.Fingerprint, nil
				}
			}
		}
		return "", fmt.Errorf("register DO ssh key: %w", err)
	}
	return createRes.SSHKey.Fingerprint, nil
}

func (c *Client) ListSSHKeys(ctx context.Context) ([]doSSHKey, error) {
	var keys []doSSHKey
	err := c.listAll(ctx, "/account/keys?per_page=100", func(raw json.RawMessage) error {
		var res doSSHKeysResponse
		if err := json.Unmarshal(raw, &res); err != nil {
			return err
		}
		keys = append(keys, res.SSHKeys...)
		return nil
	})
	return keys, err
}

func (c *Client) CreateHost(ctx context.Context, spec config.HostSpec) (*provider.Host, error) {
	image := spec.Image
	if image == "" {
		image = "debian-13-x64"
	}
	region := spec.Region
	if region == "" {
		region = "nyc1"
	}
	size := spec.Size
	if size == "" {
		size = "s-4vcpu-8gb"
	}

	payload := map[string]any{
		"name":               spec.Name,
		"region":             region,
		"size":               size,
		"image":              image,
		"tags":               spec.Tags,
		"ipv6":               false,
		"private_networking": true,
	}
	if spec.UserData != "" {
		payload["user_data"] = spec.UserData
	}
	if len(spec.SSHKeyNames) > 0 {
		payload["ssh_keys"] = spec.SSHKeyNames
	}

	var res dropletResponse
	if err := c.request(ctx, http.MethodPost, "/droplets", payload, &res); err != nil {
		return nil, fmt.Errorf("create droplet: %w", err)
	}

	// Poll until active (max 3 minutes)
	hostID := strconv.Itoa(res.Droplet.ID)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	timeoutChan := time.After(3 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeoutChan:
			return nil, fmt.Errorf("timed out waiting for droplet %s to become active", hostID)
		case <-ticker.C:
			h, err := c.GetHost(ctx, hostID)
			if err != nil {
				continue
			}
			if h.Status == "active" && h.PublicIP != "" {
				return h, nil
			}
		}
	}
}

func (c *Client) GetHost(ctx context.Context, hostID string) (*provider.Host, error) {
	var res dropletResponse
	if err := c.request(ctx, http.MethodGet, "/droplets/"+hostID, nil, &res); err != nil {
		return nil, fmt.Errorf("get droplet %s: %w", hostID, err)
	}
	return c.toHost(&res.Droplet), nil
}

func (c *Client) ListHosts(ctx context.Context) ([]*provider.Host, error) {
	var hosts []*provider.Host
	err := c.listAll(ctx, "/droplets?per_page=100", func(raw json.RawMessage) error {
		var res dropletsListResponse
		if err := json.Unmarshal(raw, &res); err != nil {
			return err
		}
		for i := range res.Droplets {
			hosts = append(hosts, c.toHost(&res.Droplets[i]))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list droplets: %w", err)
	}
	return hosts, nil
}

func (c *Client) ResizeHost(ctx context.Context, hostID string, newSize string) error {
	payload := map[string]any{
		"type": "resize",
		"size": newSize,
	}
	if err := c.request(ctx, http.MethodPost, "/droplets/"+hostID+"/actions", payload, nil); err != nil {
		return fmt.Errorf("resize droplet %s to %s: %w", hostID, newSize, err)
	}
	return nil
}

func (c *Client) DestroyHost(ctx context.Context, hostID string) error {
	if err := c.request(ctx, http.MethodDelete, "/droplets/"+hostID, nil, nil); err != nil {
		return fmt.Errorf("destroy droplet %s: %w", hostID, err)
	}
	return nil
}

// DNSProvider implementation

type doDNSRecordsResponse struct {
	DomainRecords []doDNSRecord `json:"domain_records"`
}

type doDNSRecordResponse struct {
	DomainRecord doDNSRecord `json:"domain_record"`
}

type doDNSRecord struct {
	ID       int    `json:"id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Data     string `json:"data"`
	Priority *int   `json:"priority"`
	TTL      int    `json:"ttl"`
}

func (c *Client) ListRecords(ctx context.Context, domain string) ([]provider.DNSRecord, error) {
	var all []doDNSRecord
	path := fmt.Sprintf("/domains/%s/records?per_page=200", domain)
	err := c.listAll(ctx, path, func(raw json.RawMessage) error {
		var res doDNSRecordsResponse
		if err := json.Unmarshal(raw, &res); err != nil {
			return err
		}
		all = append(all, res.DomainRecords...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list DNS records for %s: %w", domain, err)
	}

	var records []provider.DNSRecord
	for _, r := range all {
		rec := provider.DNSRecord{
			ID:    strconv.Itoa(r.ID),
			Type:  r.Type,
			Name:  r.Name,
			Value: r.Data,
			TTL:   r.TTL,
		}
		if r.Priority != nil {
			rec.Priority = *r.Priority
		}
		records = append(records, rec)
	}
	return records, nil
}

func (c *Client) DeleteRecord(ctx context.Context, domain string, recordID string) error {
	path := fmt.Sprintf("/domains/%s/records/%s", domain, recordID)
	if err := c.request(ctx, http.MethodDelete, path, nil, nil); err != nil {
		return fmt.Errorf("delete DNS record %s on %s: %w", recordID, domain, err)
	}
	return nil
}

// SyncRecords brings the zone to the desired state by creating and updating
// records (never deleting). See provider.PlanDNSSync for the matching rules;
// running it again with the same input makes no API calls beyond the listing.
func (c *Client) SyncRecords(ctx context.Context, domain string, desired []provider.DNSRecord) error {
	existing, err := c.ListRecords(ctx, domain)
	if err != nil {
		return fmt.Errorf("sync DNS records: %w", err)
	}

	for _, ch := range provider.PlanDNSSync(existing, desired) {
		d := ch.Record
		payload := map[string]any{"data": d.Value}
		if d.TTL > 0 {
			payload["ttl"] = d.TTL
		}
		if d.Priority > 0 {
			payload["priority"] = d.Priority
		}
		if ch.Update {
			path := fmt.Sprintf("/domains/%s/records/%s", domain, d.ID)
			if err := c.request(ctx, http.MethodPut, path, payload, nil); err != nil {
				return fmt.Errorf("update DNS record %s on %s: %w", d.Name, domain, err)
			}
			continue
		}
		payload["type"] = d.Type
		payload["name"] = d.Name
		if _, ok := payload["ttl"]; !ok {
			payload["ttl"] = 1800
		}
		path := fmt.Sprintf("/domains/%s/records", domain)
		if err := c.request(ctx, http.MethodPost, path, payload, nil); err != nil {
			return fmt.Errorf("create DNS record %s on %s: %w", d.Name, domain, err)
		}
	}
	return nil
}
