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
}

func NewInstanceManager(exec remote.Executor) *InstanceManager {
	return &InstanceManager{
		incus: incus.NewClient(exec),
		caddy: caddy.NewEdgeManager(exec, "edge"),
		exec:  exec,
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

// Launch provisions a new instance from a template blueprint.
func (m *InstanceManager) Launch(ctx context.Context, p LaunchParams) (string, error) {
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

	// 3. Launch container
	profiles := p.Template.Profiles
	if len(profiles) == 0 {
		profiles = []string{"base", "service"}
	}
	if err := m.incus.LaunchContainer(ctx, imageRef, p.Name, profiles, limits); err != nil {
		return "", fmt.Errorf("launch container %s: %w", p.Name, err)
	}

	// 4. Attach persistent data volume
	for _, vol := range p.Template.Volumes {
		actualVolName := strings.ReplaceAll(vol.Name, "{slug}", p.Slug)
		if err := m.incus.EnsureVolume(ctx, vol.Pool, actualVolName); err != nil {
			return "", fmt.Errorf("ensure volume %s: %w", actualVolName, err)
		}
		if err := m.incus.AttachVolume(ctx, p.Name, vol.Pool, actualVolName, vol.Path, vol.Shifted); err != nil {
			return "", fmt.Errorf("attach volume %s: %w", actualVolName, err)
		}
	}

	// 5. Build and write env
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
		if err := m.incus.WriteEnvironmentFile(ctx, p.Name, serviceName, env); err != nil {
			return "", fmt.Errorf("write env file: %w", err)
		}
		_ = m.incus.RestartService(ctx, p.Name, serviceName)
	}

	// 6. Health gate
	ip, err := m.incus.GetContainerIP(ctx, p.Name)
	if err != nil {
		return "", fmt.Errorf("get container IP: %w", err)
	}

	if p.Template.HealthCheck.Path != "" {
		if err := m.incus.HealthGate(ctx, ip, p.Template.HealthCheck); err != nil {
			return "", fmt.Errorf("instance health gate failed: %w", err)
		}
	}

	// 7. Publish Caddy Route
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
		if err := m.caddy.PublishSite(ctx, p.Name, routing, ip); err != nil {
			return "", fmt.Errorf("publish caddy site: %w", err)
		}
	}

	log.Printf("==> [Instance] Successfully launched instance %s at %s\n", p.Name, ip)
	return ip, nil
}

// Update replaces an existing instance's container with a new image while preserving volume data.
func (m *InstanceManager) Update(ctx context.Context, name string, newImageRef string, serviceName string) error {
	log.Printf("==> [Instance] Immutable update for instance: %s\n", name)

	// Snapshot volume first
	volumeName := name + "-data"
	snapName := fmt.Sprintf("pre-update-%s", time.Now().UTC().Format("20060102-150405"))
	_ = m.incus.SnapshotVolume(ctx, "default", volumeName, snapName)

	// Delete container
	if err := m.incus.StopAndDeleteContainer(ctx, name); err != nil {
		return fmt.Errorf("delete container for update: %w", err)
	}

	// Launch new container
	profiles := []string{"base", "service"}
	if err := m.incus.LaunchContainer(ctx, newImageRef, name, profiles, nil); err != nil {
		return fmt.Errorf("launch updated container: %w", err)
	}

	// Reattach volume
	if err := m.incus.AttachVolume(ctx, name, "default", volumeName, "/app/.data", true); err != nil {
		return fmt.Errorf("reattach volume %s: %w", volumeName, err)
	}

	// Restart
	if serviceName == "" {
		serviceName = "platform"
	}
	_ = m.incus.RestartService(ctx, name, serviceName)

	return nil
}

// Resize applies live CPU/memory cgroup updates without container downtime.
func (m *InstanceManager) Resize(ctx context.Context, name string, limits map[string]string) error {
	return m.incus.ResizeLimits(ctx, name, limits)
}

// Destroy tears down an instance, removes its Caddy route, and optionally purges its data volume.
func (m *InstanceManager) Destroy(ctx context.Context, name string, purgeVolume bool) error {
	log.Printf("==> [Instance] Destroying instance: %s\n", name)
	_ = m.caddy.RemoveSite(ctx, name)
	if err := m.incus.StopAndDeleteContainer(ctx, name); err != nil {
		return fmt.Errorf("delete container: %w", err)
	}
	if purgeVolume {
		volName := name + "-data"
		_, _ = m.exec.Run(ctx, fmt.Sprintf("incus storage volume delete default %s", volName))
	}
	return nil
}
