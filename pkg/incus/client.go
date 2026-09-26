package incus

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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

// containerIPTimeout is how long GetContainerIP waits for a lease (a variable so tests can shorten it).
var containerIPTimeout = 30 * time.Second

// GetContainerIP waits for the container to get a global IPv4 address from the
// bridge's DHCP and returns it. It only reads: it used to also push a
// hash-derived static address, a default route and a rewritten resolv.conf into
// the container on every poll, which left containers with two addresses (so the
// "current" one was arbitrary), collided across services (the hash was mod 200),
// vanished on restart, and hardcoded the 10.0.100.0/24 subnet. A container that
// cannot get a DHCP address has a host firewall or bridge problem, and the error
// says so instead of papering over it. When several addresses exist the lowest is returned.
func (c *Client) GetContainerIP(ctx context.Context, name string) (string, error) {
	ip, err := c.ContainerIPv4(ctx, name, containerIPTimeout)
	if err != nil {
		info, _ := c.exec.Run(ctx, "incus info "+ShQuote(name))
		return "", fmt.Errorf("%w: the container did not get an address from the bridge (is DHCP allowed on it in the host firewall?). Diagnostic:\n%s", err, info)
	}
	return ip, nil
}

// InstanceExists reports whether an instance exists on the default remote. An
// unreachable or failing incus is an error, not "absent", so a caller can
// never mistake a transient failure for a missing instance.
func (c *Client) InstanceExists(ctx context.Context, name string) (bool, error) {
	if !ValidName(name) {
		return false, fmt.Errorf("invalid instance name %q", name)
	}
	out, err := c.exec.Run(ctx, "incus list "+ShQuote(name)+" --format json")
	if err != nil {
		return false, fmt.Errorf("list instances: %w", err)
	}
	var instances []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &instances); err != nil {
		return false, fmt.Errorf("parse instance list: %w", err)
	}
	for _, inst := range instances {
		if inst.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// ContainerExists checks if an instance exists (false on any error; prefer InstanceExists).
func (c *Client) ContainerExists(ctx context.Context, name string) bool {
	ok, err := c.InstanceExists(ctx, name)
	return ok && err == nil
}

// NetworkSetIfChanged returns a shell command that sets one Incus network key
// only when its current value differs, so repeated runs do not re-apply it.
func NetworkSetIfChanged(network, key, want string) string {
	return fmt.Sprintf(`[ "$(incus network get %s %s 2>/dev/null)" = %s ] || incus network set %s %s || true`,
		ShQuote(network), ShQuote(key), ShQuote(want), ShQuote(network), ShQuote(key+"="+want))
}

// EnsureProfile ensures a named profile exists, creating and configuring standard profiles if needed.
func (c *Client) EnsureProfile(ctx context.Context, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid profile name %q", name)
	}
	// Ensure default storage and root disk device are active
	_, _ = c.exec.Run(ctx, "incus storage list | grep -q default || incus storage create default dir || true")
	_, _ = c.exec.Run(ctx, "incus profile device show default | grep -q 'path: /' || incus profile device add default root disk path=/ pool=default || true")
	_, _ = c.exec.Run(ctx, "incus network show incusbr0 >/dev/null 2>&1 || incus network create incusbr0 || true")
	_, _ = c.exec.Run(ctx, NetworkSetIfChanged("incusbr0", "ipv4.address", "10.0.100.1/24"))
	_, _ = c.exec.Run(ctx, NetworkSetIfChanged("incusbr0", "ipv4.nat", "true"))
	_, _ = c.exec.Run(ctx, NetworkSetIfChanged("incusbr0", "ipv6.address", "none"))
	_, _ = c.exec.Run(ctx, "incus profile device show default | grep -q 'network: incusbr0' || incus profile device add default eth0 nic network=incusbr0 name=eth0 || true")

	if _, err := c.exec.Run(ctx, "incus profile show "+ShQuote(name)); err == nil {
		return nil
	}

	if _, err := c.exec.Run(ctx, "incus profile create "+ShQuote(name)); err != nil {
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
// Every argument is shell-quoted and config keys are emitted in sorted order,
// so the command is safe for arbitrary values (e.g. multi-word user.* config
// replayed from another instance) and identical for identical input.
func (c *Client) LaunchContainer(ctx context.Context, image string, name string, profiles []string, limits map[string]string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid instance name %q", name)
	}
	// Normalize image for Incus: if not prefixed with images:, docker:, or local remote, prefix with docker:
	if !strings.HasPrefix(image, "images:") && !strings.HasPrefix(image, "docker:") && !strings.HasPrefix(image, "local:") && len(image) != 64 {
		image = "docker:" + image
	}

	if strings.HasPrefix(image, "docker:") {
		_, _ = c.exec.Run(ctx, "incus remote list | grep -q ' docker ' || incus remote add docker https://docker.io --protocol=oci --public || true")
	}

	// Ensure all required profiles exist
	for _, p := range profiles {
		if p != "default" {
			if err := c.EnsureProfile(ctx, p); err != nil {
				return err
			}
		}
	}

	args := []string{"incus", "launch", ShQuote(image), ShQuote(name)}

	// Any --profile replaces Incus's implicit default profile, and the default
	// profile carries the root disk and the NIC. So it must always be named
	// explicitly: first when the caller did not list it, otherwise where the
	// caller put it (later profiles override earlier ones, so order matters).
	hasDefault := false
	for _, p := range profiles {
		if p == "default" {
			hasDefault = true
			break
		}
	}
	ordered := make([]string, 0, len(profiles)+1)
	if !hasDefault {
		ordered = append(ordered, "default")
	}
	ordered = append(ordered, profiles...)
	for _, p := range ordered {
		args = append(args, "--profile", ShQuote(p))
	}

	keys := make([]string, 0, len(limits))
	for k := range limits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--config", ShQuote(k+"="+limits[k]))
	}

	if _, err := c.exec.Run(ctx, strings.Join(args, " ")); err != nil {
		return fmt.Errorf("launch container %s: %w", name, err)
	}
	return nil
}

// StopAndDeleteContainer gracefully stops and purges a container.
func (c *Client) StopAndDeleteContainer(ctx context.Context, name string) error {
	if !c.ContainerExists(ctx, name) {
		return nil
	}
	// Best effort: `delete --force` stops a running container itself.
	_, _ = c.exec.Run(ctx, fmt.Sprintf("incus stop %s --force", ShQuote(name)))
	if _, err := c.exec.Run(ctx, fmt.Sprintf("incus delete %s --force", ShQuote(name))); err != nil {
		// A failed delete is only fine if the container is in fact gone.
		if c.ContainerExists(ctx, name) {
			return fmt.Errorf("delete container %s: %w", name, err)
		}
	}
	return nil
}

// EnsureVolume creates a storage volume if it does not already exist.
func (c *Client) EnsureVolume(ctx context.Context, pool, volumeName string) error {
	if pool == "" {
		pool = "default"
	}
	if !ValidName(pool) || !ValidName(volumeName) {
		return fmt.Errorf("invalid pool/volume name %q/%q", pool, volumeName)
	}
	if _, err := c.exec.Run(ctx, fmt.Sprintf("incus storage volume show %s %s", ShQuote(pool), ShQuote(volumeName))); err == nil {
		return nil // already exists
	}
	if _, err := c.exec.Run(ctx, fmt.Sprintf("incus storage volume create %s %s", ShQuote(pool), ShQuote(volumeName))); err != nil {
		return fmt.Errorf("create storage volume %s on pool %s: %w", volumeName, pool, err)
	}
	return nil
}

// volumeDeviceName is the device name used for a volume mounted at mountPath.
func volumeDeviceName(mountPath string) string {
	name := strings.ReplaceAll(strings.TrimPrefix(mountPath, "/"), "/", "-")
	if name == "" {
		return "data-vol"
	}
	return name
}

// AttachVolume attaches a storage volume. It fails if a device of that name
// already exists; use EnsureVolumeAttached for an idempotent attach.
func (c *Client) AttachVolume(ctx context.Context, containerName, pool, volumeName, mountPath string, shifted bool) error {
	if pool == "" {
		pool = "default"
	}
	if !ValidName(containerName) || !ValidName(pool) || !ValidName(volumeName) {
		return fmt.Errorf("invalid container/pool/volume name %q/%q/%q", containerName, pool, volumeName)
	}
	cmd := fmt.Sprintf("incus config device add %s %s disk %s %s %s",
		ShQuote(containerName), ShQuote(volumeDeviceName(mountPath)),
		ShQuote("pool="+pool), ShQuote("source="+volumeName), ShQuote("path="+mountPath))
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("attach volume %s to %s at %s: %w", volumeName, containerName, mountPath, err)
	}
	return nil
}

// EnsureVolumeAttached attaches the volume unless it is already attached at
// that path. A device of the same name that points somewhere else is an
// error, never silently replaced.
func (c *Client) EnsureVolumeAttached(ctx context.Context, containerName, pool, volumeName, mountPath string, shifted bool) error {
	if pool == "" {
		pool = "default"
	}
	st, err := c.CaptureInstanceState(ctx, containerName)
	if err != nil {
		return err
	}
	for _, d := range st.Devices {
		if d["type"] == "disk" && d["pool"] == pool && d["source"] == volumeName && d["path"] == mountPath {
			return nil
		}
	}
	if d, clash := st.Devices[volumeDeviceName(mountPath)]; clash {
		return fmt.Errorf("device %q on %s already exists (source=%s path=%s) and does not match volume %s at %s",
			volumeDeviceName(mountPath), containerName, d["source"], d["path"], volumeName, mountPath)
	}
	return c.AttachVolume(ctx, containerName, pool, volumeName, mountPath, shifted)
}

// WriteEnvironmentFile writes key-value configuration into /etc/default/<service>
// (mode 0600, keys sorted so identical input gives identical bytes).
func (c *Client) WriteEnvironmentFile(ctx context.Context, containerName, serviceName string, env map[string]string) error {
	if !ValidName(containerName) || !ValidName(serviceName) {
		return fmt.Errorf("invalid container/service name %q/%q", containerName, serviceName)
	}
	if err := c.PushFile(ctx, containerName, "/etc/default/"+serviceName, RenderEnv(serviceName, env), "0600"); err != nil {
		return fmt.Errorf("write /etc/default/%s in %s: %w", serviceName, containerName, err)
	}
	return nil
}

// systemdWait and systemdPoll bound how long RestartService waits for systemd
// to come up in a freshly started container (variables so tests can shorten them).
var (
	systemdWait = 60 * time.Second
	systemdPoll = time.Second
)

// WaitSystemd waits until systemd inside the container is answering. `incus
// launch` returns as soon as the container has started, but PID 1 needs a moment
// before `systemctl` works: until then it fails with "Failed to connect to system
// scope bus". A container still booting ("starting") is fine for a restart; only
// "not up at all" is retried, and a container with no systemctl fails at once.
func (c *Client) WaitSystemd(ctx context.Context, container string) error {
	deadline := time.Now().Add(systemdWait)
	cmd := "incus exec " + ShQuote(container) + " -- systemctl is-system-running"
	for {
		out, err := c.exec.Run(ctx, cmd)
		if err == nil {
			return nil
		}
		switch strings.TrimSpace(out) {
		case "running", "degraded", "starting", "initializing":
			return nil // systemd is up (is-system-running exits non-zero for anything but "running")
		}
		if strings.Contains(err.Error(), "ommand not found") {
			return fmt.Errorf("%s has no systemctl: %w", container, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("systemd in %s did not come up within %s: %w", container, systemdWait, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(systemdPoll):
		}
	}
}

// RestartService triggers a systemd service restart inside the container,
// first waiting for systemd itself to be up (see WaitSystemd).
func (c *Client) RestartService(ctx context.Context, containerName, serviceName string) error {
	if !ValidName(containerName) || !ValidName(serviceName) {
		return fmt.Errorf("invalid container/service name %q/%q", containerName, serviceName)
	}
	if err := c.WaitSystemd(ctx, containerName); err != nil {
		return fmt.Errorf("restart service %s in container %s: %w", serviceName, containerName, err)
	}
	cmd := fmt.Sprintf("incus exec %s -- systemctl restart %s", ShQuote(containerName), ShQuote(serviceName))
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("restart service %s in container %s: %w", serviceName, containerName, err)
	}
	return nil
}

// ResizeLimits applies live config keys (CPU/memory cgroup limits) to a running container without restart.
func (c *Client) ResizeLimits(ctx context.Context, containerName string, limits map[string]string) error {
	if !ValidName(containerName) {
		return fmt.Errorf("invalid container name %q", containerName)
	}
	keys := make([]string, 0, len(limits))
	for k := range limits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := c.exec.Run(ctx, fmt.Sprintf("incus config set %s %s", ShQuote(containerName), ShQuote(k+"="+limits[k]))); err != nil {
			return fmt.Errorf("set %s=%s on %s: %w", k, limits[k], containerName, err)
		}
	}
	return nil
}

// SnapshotVolume creates a snapshot of a storage volume, generating a
// timestamped name when none is given. It uses the `snapshot create` form of
// the Incus CLI (the bare `snapshot <pool> <vol> <name>` form is LXD-era and
// is not accepted by Incus).
func (c *Client) SnapshotVolume(ctx context.Context, pool, volumeName, snapshotName string) error {
	if snapshotName == "" {
		snapshotName = fmt.Sprintf("snap-%s", time.Now().UTC().Format("20060102-150405"))
	}
	return c.CreateVolumeSnapshot(ctx, pool, volumeName, snapshotName)
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
		if err == nil {
			trimmed := strings.TrimSpace(code)
			if trimmed == "200" || trimmed == "301" || trimmed == "302" || trimmed == "308" || trimmed == "404" {
				return nil
			}
		}
		time.Sleep(time.Duration(intervalSec) * time.Second)
	}

	return fmt.Errorf("healthcheck failed: %s did not return 200 within %ds", url, timeoutSec)
}
