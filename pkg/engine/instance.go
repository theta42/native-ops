package engine

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/theta42/native-ops/pkg/caddy"
	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

// InstanceManager handles dynamic template-based instances (e.g. multi-tenant SaaS).
type InstanceManager struct {
	incus *incus.Client
	caddy *caddy.EdgeManager
	exec  remote.Executor
	// healthGate is a field so tests can stub the wait.
	healthGate func(ctx context.Context, ip string, hc config.HealthCheckConfig) error
}

func NewInstanceManager(exec remote.Executor) *InstanceManager {
	ic := incus.NewClient(exec)
	return &InstanceManager{
		incus:      ic,
		caddy:      caddy.NewEdgeManager(exec, "edge"),
		exec:       exec,
		healthGate: ic.HealthGate,
	}
}

type LaunchParams struct {
	Template     *config.TemplateConfig
	Name         string // e.g. "rest-acme"
	Slug         string // e.g. "acme"
	Domain       string // e.g. "acme.example.com"
	CustomDomain string // optional custom domain
	Env          map[string]string
	Limits       map[string]string
}

// Launch provisions an instance from a template blueprint. It is safe to
// re-run: an instance that an earlier run of this template already created
// (recorded in user.native-ops.template) is resumed and converged instead of
// failing on "already exists", so a launch interrupted by a failed health gate
// or a lost connection can simply be run again. An instance of that name that
// did not come from this template is never adopted or touched.
func (m *InstanceManager) Launch(ctx context.Context, p LaunchParams) (string, error) {
	if !incus.ValidName(p.Name) {
		return "", fmt.Errorf("invalid instance name %q", p.Name)
	}
	log.Printf("==> [Instance] Launching dynamic instance: %s (slug=%s)\n", p.Name, p.Slug)

	// 1. Resolve image
	imageRef := p.Template.Image
	fp, err := m.incus.ResolveImageFingerprint(ctx, imageRef)
	if err == nil {
		imageRef = fp
	}

	// 2. Prepare limits (merge default + override)
	limits := make(map[string]string)
	for k, v := range p.Template.DefaultLimits {
		limits[k] = v
	}
	for k, v := range p.Limits {
		limits[k] = v
	}

	// 3. Launch the container, or resume the one an earlier run created.
	profiles := p.Template.Profiles
	if len(profiles) == 0 {
		profiles = []string{"base", "service"}
	}
	exists, err := m.incus.InstanceExists(ctx, p.Name)
	if err != nil {
		return "", err
	}
	if exists {
		st, err := m.incus.CaptureInstanceState(ctx, p.Name)
		if err != nil {
			return "", err
		}
		if st.Config[incus.TemplateKey] != p.Template.Name || p.Template.Name == "" {
			return "", fmt.Errorf("instance %s already exists and was not launched from template %q; refusing to adopt it (use `instance update` to change an existing instance)", p.Name, p.Template.Name)
		}
		log.Printf("    %s already exists from this template; resuming\n", p.Name)
	} else {
		cfg := make(map[string]string, len(limits)+2)
		for k, v := range limits {
			cfg[k] = v
		}
		cfg[incus.TemplateKey] = p.Template.Name
		cfg[incus.ImageKey] = p.Template.Image
		if err := m.incus.LaunchContainer(ctx, imageRef, p.Name, profiles, cfg); err != nil {
			return "", fmt.Errorf("launch container %s: %w", p.Name, err)
		}
	}

	// 4. Attach persistent data volume
	for _, vol := range p.Template.Volumes {
		actualVolName := strings.ReplaceAll(vol.Name, "{slug}", p.Slug)
		if err := m.incus.EnsureVolume(ctx, vol.Pool, actualVolName); err != nil {
			return "", fmt.Errorf("ensure volume %s: %w", actualVolName, err)
		}
		pool := vol.Pool
		if pool == "" {
			pool = "default"
		}
		if err := m.incus.EnsureVolumeAttached(ctx, p.Name, pool, actualVolName, vol.Path, vol.Shifted); err != nil {
			return "", fmt.Errorf("attach volume %s: %w", actualVolName, err)
		}
	}

	// 5. Build and converge env (declared keys are set, other keys are preserved)
	env := make(map[string]string)
	for k, v := range p.Template.EnvTemplate {
		env[k] = strings.ReplaceAll(v, "{slug}", p.Slug)
	}
	for k, v := range p.Env {
		env[k] = v
	}
	if len(env) > 0 {
		serviceName := p.Template.Service
		if serviceName == "" {
			serviceName = strings.TrimPrefix(p.Name, "rest-")
		}
		if serviceName == "" {
			serviceName = "platform"
		}
		changed, err := ensureEnv(ctx, m.incus, p.Name, serviceName, env)
		if err != nil {
			return "", fmt.Errorf("write env file: %w", err)
		}
		if changed {
			if err := m.incus.RestartService(ctx, p.Name, serviceName); err != nil {
				log.Printf("    WARNING: %v\n", err)
			}
		}
	}

	// 6. Health gate. A resumed instance is inspected without touching its network.
	var ip string
	if exists {
		ip, err = m.incus.ContainerIPv4(ctx, p.Name, 30*time.Second)
	} else {
		ip, err = m.incus.GetContainerIP(ctx, p.Name)
	}
	if err != nil {
		return "", fmt.Errorf("get container IP: %w", err)
	}
	if p.Template.HealthCheck.Path != "" {
		if err := m.healthGate(ctx, ip, p.Template.HealthCheck); err != nil {
			return "", fmt.Errorf("instance health gate failed: %w", err)
		}
	}

	// 7. Publish Caddy Route (a no-op when the published route is already right)
	domain := p.Domain
	if domain == "" && p.Template.RoutingPattern != "" {
		domain = strings.ReplaceAll(p.Template.RoutingPattern, "{slug}", p.Slug)
	}
	if domain != "" {
		port := p.Template.HealthCheck.Port
		if port <= 0 {
			port = 8787
		}
		routing := config.RoutingConfig{
			Domain:       domain,
			UpstreamPort: port,
		}
		ips, err := m.incus.GlobalIPv4s(ctx, p.Name)
		if err != nil {
			return "", err
		}
		candidates := []string{ip}
		for _, other := range ips {
			if other != ip {
				candidates = append(candidates, other)
			}
		}
		if err := m.caddy.PublishSiteFor(ctx, p.Name, routing, candidates); err != nil {
			return "", fmt.Errorf("publish caddy site: %w", err)
		}
	}

	log.Printf("==> [Instance] Instance %s is up at %s\n", p.Name, ip)
	return ip, nil
}

// UpdateOptions controls an immutable instance update.
type UpdateOptions struct {
	// Service names the systemd unit whose /etc/default/<Service> environment
	// file is carried over to the replacement container. Default "platform".
	Service string
	// HealthCheck, when Path is set, gates the update: if the new container is
	// not healthy the previous image is relaunched with the same configuration.
	HealthCheck config.HealthCheckConfig
	// SkipSnapshot opts out of the pre-update volume snapshot. By default a
	// snapshot is required, and a failed snapshot aborts the update untouched.
	SkipSnapshot bool
	// Force replaces the instance even when it already runs the requested image.
	Force bool
	// BeforeStart, when set, runs on the replacement after its volumes and
	// environment file are in place and before its service is (re)started.
	BeforeStart func(ctx context.Context, name string) error
}

// isFingerprint reports whether ref is a bare 64-hex image fingerprint.
func isFingerprint(ref string) bool {
	if len(ref) != 64 {
		return false
	}
	for _, c := range ref {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Update replaces an instance's container with a new image. Only the image
// changes: profiles, local config (limits, ...), devices (data volumes, ...)
// and the service's environment file are read from the running container and
// replayed onto the replacement. Every attached custom volume is snapshotted
// first, and a failed snapshot aborts before anything is deleted. If the
// replacement fails to start or to pass its health check, the previous image
// is relaunched with the same configuration, so a bad release does not leave
// the instance down. Errors are always returned (never swallowed), so a CI
// job running this fails visibly.
//
// It is idempotent: an instance that already runs the requested image (same
// fingerprint, or for a non-local reference the same recorded reference) is
// left alone, so a CI re-run does not restart anything. Force overrides that.
func (m *InstanceManager) Update(ctx context.Context, name string, newImageRef string, opts UpdateOptions) error {
	if !incus.ValidName(name) {
		return fmt.Errorf("invalid instance name %q", name)
	}
	service := opts.Service
	if service == "" {
		service = "platform"
	}
	if !incus.ValidName(service) {
		return fmt.Errorf("invalid service name %q", service)
	}
	log.Printf("==> [Instance] Immutable update for instance: %s\n", name)

	// 1. Resolve the target and read the running state. Nothing is changed yet.
	target, err := m.incus.ResolveImage(ctx, newImageRef)
	if err != nil {
		return err
	}
	st, err := m.incus.CaptureInstanceState(ctx, name)
	if err != nil {
		return err
	}

	// Already there? Then there is nothing to replace.
	if !opts.Force {
		current := (isFingerprint(target) && st.BaseImage == target) ||
			(!isFingerprint(target) && st.Config[incus.ImageKey] == newImageRef)
		if current {
			log.Printf("==> [Instance] %s already runs %s; nothing to do\n", name, newImageRef)
			if st.Config[incus.ImageKey] != newImageRef {
				if err := m.incus.SetInstanceConfig(ctx, name, incus.ImageKey, newImageRef); err != nil {
					return err
				}
			}
			if opts.HealthCheck.Path != "" {
				ip, err := m.incus.ContainerIPv4(ctx, name, 30*time.Second)
				if err != nil {
					return fmt.Errorf("%s already runs %s but is not reachable, not replacing it: %w", name, newImageRef, err)
				}
				if err := m.healthGate(ctx, ip, opts.HealthCheck); err != nil {
					return fmt.Errorf("%s already runs %s but is not healthy, not replacing it (use --force to replace anyway): %w", name, newImageRef, err)
				}
			}
			return nil
		}
	}
	envPath := "/etc/default/" + service
	env, hadEnv, err := m.incus.PullFile(ctx, name, envPath)
	if err != nil {
		return err
	}

	// 2. Snapshot every attached custom volume; abort untouched on failure.
	if !opts.SkipSnapshot {
		snap := fmt.Sprintf("pre-update-%s", time.Now().UTC().Format("20060102-150405"))
		for _, v := range st.Volumes() {
			log.Printf("    Snapshotting volume %s/%s (%s)...\n", v.Pool, v.Name, snap)
			if err := m.incus.SnapshotVolume(ctx, v.Pool, v.Name, snap); err != nil {
				return fmt.Errorf("pre-update snapshot failed, nothing was changed: %w", err)
			}
		}
	}

	replace := func(image string, cfg map[string]string) error {
		if err := m.incus.StopAndDeleteContainer(ctx, name); err != nil {
			return err
		}
		if err := m.incus.LaunchContainer(ctx, image, name, st.Profiles, cfg); err != nil {
			return err
		}
		for _, dn := range st.DeviceNames() {
			if err := m.incus.SetDevice(ctx, name, dn, st.Devices[dn]); err != nil {
				return err
			}
		}
		if hadEnv {
			if err := m.incus.PushFile(ctx, name, envPath, env, "0600"); err != nil {
				return err
			}
		}
		if opts.BeforeStart != nil {
			if err := opts.BeforeStart(ctx, name); err != nil {
				return err
			}
		}
		if err := m.incus.RestartService(ctx, name, service); err != nil {
			return err
		}
		if opts.HealthCheck.Path == "" {
			return nil
		}
		ip, err := m.incus.ContainerIPv4(ctx, name, 30*time.Second)
		if err != nil {
			return err
		}
		return m.healthGate(ctx, ip, opts.HealthCheck)
	}

	// 3. Replace, and roll back to the previous image if the new one is not healthy.
	forward := make(map[string]string, len(st.Config)+1)
	for k, v := range st.Config {
		forward[k] = v
	}
	forward[incus.ImageKey] = newImageRef // the rollback keeps the old recorded reference
	updateErr := replace(target, forward)
	if updateErr == nil {
		log.Printf("==> [Instance] Updated %s to %s\n", name, newImageRef)
		return nil
	}
	if st.BaseImage == "" {
		return fmt.Errorf("update failed and the previous image is unknown, so no automatic rollback was possible (volume snapshots are named pre-update-*): %w", updateErr)
	}
	log.Printf("    Update failed (%v); rolling back to the previous image %s...\n", updateErr, st.BaseImage)
	if rbErr := replace(st.BaseImage, st.Config); rbErr != nil {
		return fmt.Errorf("update failed: %v; rollback ALSO failed (volume snapshots are named pre-update-*): %w", updateErr, rbErr)
	}
	return fmt.Errorf("update to %s failed and %s was rolled back to its previous image: %w", newImageRef, name, updateErr)
}

// Resize applies live CPU/memory cgroup updates without container downtime.
func (m *InstanceManager) Resize(ctx context.Context, name string, limits map[string]string) error {
	return m.incus.ResizeLimits(ctx, name, limits)
}

// Destroy tears down an instance, removes its Caddy route, and optionally
// purges its data volume. Destroying something that is already gone succeeds.
func (m *InstanceManager) Destroy(ctx context.Context, name string, purgeVolume bool) error {
	if !incus.ValidName(name) {
		return fmt.Errorf("invalid instance name %q", name)
	}
	log.Printf("==> [Instance] Destroying instance: %s\n", name)
	if err := m.caddy.RemoveSite(ctx, name); err != nil {
		log.Printf("    WARNING: could not remove the Caddy route for %s: %v\n", name, err)
	}
	if err := m.incus.StopAndDeleteContainer(ctx, name); err != nil {
		return fmt.Errorf("delete container: %w", err)
	}
	if purgeVolume {
		volName := name + "-data"
		if _, err := m.exec.Run(ctx, fmt.Sprintf("incus storage volume show default %s", incus.ShQuote(volName))); err == nil {
			if _, err := m.exec.Run(ctx, fmt.Sprintf("incus storage volume delete default %s", incus.ShQuote(volName))); err != nil {
				return fmt.Errorf("purge volume %s: %w", volName, err)
			}
		}
	}
	return nil
}
