package incus

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/remote"
)

// ContainerState represents the parsed JSON state of an Incus container.
type ContainerState struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // "Running", "Stopped", "Frozen"
	IPv4    string `json:"ipv4,omitempty"`
	CPU     string `json:"cpu,omitempty"`
	Memory  string `json:"memory,omitempty"`
	Created time.Time `json:"created_at"`
}

// Client wraps Incus CLI operations on a local or remote target.
type Client struct {
	exec remote.Executor
}

func NewClient(exec remote.Executor) *Client {
	return &Client{exec: exec}
}

// ResolveImageFingerprint looks up the full fingerprint for an alias safely via JSON.
func (c *Client) ResolveImageFingerprint(ctx context.Context, alias string) (string, error) {
	out, err := c.exec.Run(ctx, "incus image alias list --format json")
	if err != nil {
		return "", fmt.Errorf("list image aliases: %w", err)
	}

	var aliases []struct {
		Name   string `json:"name"`
		Target string `json:"target"`
	}
	if err := json.Unmarshal([]byte(out), &aliases); err != nil {
		return "", fmt.Errorf("parse image aliases JSON: %w", err)
	}

	for _, a := range aliases {
		if a.Name == alias {
			return a.Target, nil
		}
	}

	// Fallback: check if the alias is directly in incus image list
	imgOut, err := c.exec.Run(ctx, "incus image list --format json")
	if err == nil {
		var images []struct {
			Fingerprint string `json:"fingerprint"`
			Aliases     []struct {
				Name string `json:"name"`
			} `json:"aliases"`
		}
		if err := json.Unmarshal([]byte(imgOut), &images); err == nil {
			for _, img := range images {
				for _, a := range img.Aliases {
					if a.Name == alias {
						return img.Fingerprint, nil
					}
				}
			}
		}
	}

	return "", fmt.Errorf("image alias not found: %s", alias)
}

// GetContainerIP extracts the eth0/incusbr0 IPv4 address from incus list JSON.
func (c *Client) GetContainerIP(ctx context.Context, name string) (string, error) {
	cmd := fmt.Sprintf("incus list %s --format json", name)
	out, err := c.exec.Run(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("get container state: %w", err)
	}

	var instances []struct {
		Name  string `json:"name"`
		State *struct {
			Network map[string]struct {
				Addresses []struct {
					Family  string `json:"family"`
					Address string `json:"address"`
					Scope   string `json:"scope"`
				} `json:"addresses"`
			} `json:"network"`
		} `json:"state"`
	}

	if err := json.Unmarshal([]byte(out), &instances); err != nil {
		return "", fmt.Errorf("parse incus list JSON: %w", err)
	}

	if len(instances) == 0 || instances[0].State == nil {
		return "", fmt.Errorf("container %s is not running or has no network state", name)
	}

	for netName, netInfo := range instances[0].State.Network {
		if netName == "lo" {
			continue
		}
		for _, addr := range netInfo.Addresses {
			if addr.Family == "inet" && addr.Scope == "global" {
				return addr.Address, nil
			}
		}
	}

	return "", fmt.Errorf("no global IPv4 address assigned to container %s", name)
}

// ContainerExists checks if an instance exists.
func (c *Client) ContainerExists(ctx context.Context, name string) bool {
	cmd := fmt.Sprintf("incus info %s --format json", name)
	_, err := c.exec.Run(ctx, cmd)
	return err == nil
}

// EnsureProfile ensures a named profile exists, creating and configuring standard profiles if needed.
func (c *Client) EnsureProfile(ctx context.Context, name string) error {
	checkCmd := fmt.Sprintf("incus profile show %s", name)
	if _, err := c.exec.Run(ctx, checkCmd); err == nil {
		return nil
	}

	createCmd := fmt.Sprintf("incus profile create %s", name)
	if _, err := c.exec.Run(ctx, createCmd); err != nil {
		return fmt.Errorf("create profile %s: %w", name, err)
	}

	if name == "edge" {
		// Attach port 80 and 443 proxy devices to edge profile
		_, _ = c.exec.Run(ctx, "incus profile device add edge http proxy listen=tcp:0.0.0.0:80 connect=tcp:127.0.0.1:80")
		_, _ = c.exec.Run(ctx, "incus profile device add edge https proxy listen=tcp:0.0.0.0:443 connect=tcp:127.0.0.1:443")
	}

	return nil
}

// LaunchContainer creates and starts an instance from an image with profiles and config overrides.
func (c *Client) LaunchContainer(ctx context.Context, image string, name string, profiles []string, limits map[string]string) error {
	// Normalize image for Incus: if not prefixed with images:, docker:, or local remote, prefix with docker:
	if !strings.HasPrefix(image, "images:") && !strings.HasPrefix(image, "docker:") && !strings.HasPrefix(image, "local:") && len(image) != 64 {
		image = "docker:" + image
	}

	// Ensure all required profiles exist
	for _, p := range profiles {
		if p != "default" {
			_ = c.EnsureProfile(ctx, p)
		}
	}

	var args []string
	args = append(args, "incus", "launch", image, name)

	for _, p := range profiles {
		args = append(args, "--profile", p)
	}

	for k, v := range limits {
		args = append(args, "--config", fmt.Sprintf("%s=%s", k, v))
	}

	cmd := strings.Join(args, " ")
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("launch container %s: %w", name, err)
	}
	return nil
}

// StopAndDeleteContainer gracefully stops and purges a container.
func (c *Client) StopAndDeleteContainer(ctx context.Context, name string) error {
	if !c.ContainerExists(ctx, name) {
		return nil
	}
	// Stop with timeout, then force delete
	_, _ = c.exec.Run(ctx, fmt.Sprintf("incus stop %s --timeout 15 || incus stop %s --force", name, name))
	_, err := c.exec.Run(ctx, fmt.Sprintf("incus delete %s", name))
	if err != nil {
		return fmt.Errorf("delete container %s: %w", name, err)
	}
	return nil
}

// EnsureVolume creates a storage volume if it does not already exist.
func (c *Client) EnsureVolume(ctx context.Context, pool, volumeName string) error {
	if pool == "" {
		pool = "default"
	}
	checkCmd := fmt.Sprintf("incus storage volume show %s %s", pool, volumeName)
	if _, err := c.exec.Run(ctx, checkCmd); err == nil {
		return nil // already exists
	}

	createCmd := fmt.Sprintf("incus storage volume create %s %s", pool, volumeName)
	if _, err := c.exec.Run(ctx, createCmd); err != nil {
		return fmt.Errorf("create storage volume %s on pool %s: %w", volumeName, pool, err)
	}
	return nil
}

// AttachVolume attaches a storage volume with security.shifted=true.
func (c *Client) AttachVolume(ctx context.Context, containerName, pool, volumeName, mountPath string, shifted bool) error {
	if pool == "" {
		pool = "default"
	}

	deviceName := strings.ReplaceAll(strings.TrimPrefix(mountPath, "/"), "/", "-")
	if deviceName == "" {
		deviceName = "data-vol"
	}

	shiftedFlag := "false"
	if shifted {
		shiftedFlag = "true"
	}

	cmd := fmt.Sprintf("incus config device add %s %s disk pool=%s source=%s path=%s security.shifted=%s",
		containerName, deviceName, pool, volumeName, mountPath, shiftedFlag)

	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("attach volume %s to %s at %s: %w", volumeName, containerName, mountPath, err)
	}
	return nil
}

// WriteEnvironmentFile writes key-value configuration into /etc/default/<service> safely.
func (c *Client) WriteEnvironmentFile(ctx context.Context, containerName, serviceName string, env map[string]string) error {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Managed by native-ops for %s\n", serviceName))
	for k, v := range env {
		sb.WriteString(fmt.Sprintf("%s=%s\n", k, v))
	}

	envContent := sb.String()
	b64 := base64.StdEncoding.EncodeToString([]byte(envContent))

	cmd := fmt.Sprintf("echo '%s' | base64 -d | incus exec %s -- sh -c 'mkdir -p /etc/default && cat > /etc/default/%s && chmod 600 /etc/default/%s'",
		b64, containerName, serviceName, serviceName)

	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("write /etc/default/%s in %s: %w", serviceName, containerName, err)
	}
	return nil
}

// RestartService triggers a systemd service restart inside the container.
func (c *Client) RestartService(ctx context.Context, containerName, serviceName string) error {
	cmd := fmt.Sprintf("incus exec %s -- systemctl restart %s", containerName, serviceName)
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("restart service %s in container %s: %w", serviceName, containerName, err)
	}
	return nil
}

// ResizeLimits applies live CPU/memory cgroup limits to a running container without restart.
func (c *Client) ResizeLimits(ctx context.Context, containerName string, limits map[string]string) error {
	for k, v := range limits {
		cmd := fmt.Sprintf("incus config set %s %s %s", containerName, k, v)
		if _, err := c.exec.Run(ctx, cmd); err != nil {
			return fmt.Errorf("set %s=%s on %s: %w", k, v, containerName, err)
		}
	}
	return nil
}

// SnapshotVolume creates a snapshot of a storage volume.
func (c *Client) SnapshotVolume(ctx context.Context, pool, volumeName, snapshotName string) error {
	if pool == "" {
		pool = "default"
	}
	if snapshotName == "" {
		snapshotName = fmt.Sprintf("snap-%s", time.Now().UTC().Format("20060102-150405"))
	}

	cmd := fmt.Sprintf("incus storage volume snapshot %s %s %s", pool, volumeName, snapshotName)
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("snapshot volume %s/%s: %w", pool, volumeName, err)
	}
	return nil
}

// MigrateInstance copies/moves a container and volume across Incus hosts.
func (c *Client) MigrateInstance(ctx context.Context, srcRemote, targetRemote, name string, volumeName string) error {
	// Copy storage volume first
	if volumeName != "" {
		volCmd := fmt.Sprintf("incus storage volume copy %s:default/%s %s:default/%s --refresh",
			srcRemote, volumeName, targetRemote, volumeName)
		if _, err := c.exec.Run(ctx, volCmd); err != nil {
			return fmt.Errorf("migrate volume %s: %w", volumeName, err)
		}
	}

	// Copy instance
	instCmd := fmt.Sprintf("incus copy %s:%s %s:%s --mode=push --refresh",
		srcRemote, name, targetRemote, name)
	if _, err := c.exec.Run(ctx, instCmd); err != nil {
		return fmt.Errorf("migrate instance %s: %w", name, err)
	}

	return nil
}

// HealthGate probes the container until it returns HTTP 200 or times out.
func (c *Client) HealthGate(ctx context.Context, ip string, hc config.HealthCheckConfig) error {
	if hc.Path == "" {
		return nil
	}

	port := hc.Port
	if port <= 0 {
		port = 80
	}
	timeoutSec := hc.Timeout
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	intervalSec := hc.Interval
	if intervalSec <= 0 {
		intervalSec = 2
	}

	url := fmt.Sprintf("http://%s:%d%s", ip, port, hc.Path)
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)

	for time.Now().Before(deadline) {
		cmd := fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' --max-time 2 '%s'", url)
		code, err := c.exec.Run(ctx, cmd)
		if err == nil && strings.TrimSpace(code) == "200" {
			return nil
		}
		time.Sleep(time.Duration(intervalSec) * time.Second)
	}

	return fmt.Errorf("healthcheck failed: %s did not return 200 within %ds", url, timeoutSec)
}
