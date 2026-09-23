package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/theta42/native-ops/pkg/caddy"
	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

// Deployer orchestrates declarative service deployments.
type Deployer struct {
	incus *incus.Client
	caddy *caddy.EdgeManager
	exec  remote.Executor
}

func NewDeployer(exec remote.Executor) *Deployer {
	return &Deployer{
		incus: incus.NewClient(exec),
		caddy: caddy.NewEdgeManager(exec, "edge"),
		exec:  exec,
	}
}

// DeployService executes the full immutable deployment lifecycle for a service.
func (d *Deployer) DeployService(ctx context.Context, svc *config.ServiceConfig, configDir string) error {
	log.Printf("==> [Deploy] Starting immutable deployment for service: %s\n", svc.Name)

	// 1. Resolve image (fingerprint or OCI)
	imageRef := svc.Image
	if !strings.Contains(imageRef, "docker.io/") && !strings.Contains(imageRef, "/") {
		// Local alias
		fp, err := d.incus.ResolveImageFingerprint(ctx, imageRef)
		if err == nil {
			imageRef = fp
		}
	}

	// 2. Snapshot persistent volumes before replacing
	for _, vol := range svc.Volumes {
		if err := d.incus.EnsureVolume(ctx, vol.Pool, vol.Name); err != nil {
			return fmt.Errorf("ensure volume %s: %w", vol.Name, err)
		}
		if d.incus.ContainerExists(ctx, svc.Name) {
			snapName := fmt.Sprintf("pre-deploy-%s", time.Now().UTC().Format("20060102-150405"))
			log.Printf("    Snapshotting volume %s (%s)...\n", vol.Name, snapName)
			_ = d.incus.SnapshotVolume(ctx, vol.Pool, vol.Name, snapName)
		}
	}

	// 3. Pre-deploy hook
	if svc.Hooks.PreDeploy != "" {
		hookPath := filepath.Join(configDir, svc.Hooks.PreDeploy)
		log.Printf("    Running pre-deploy hook: %s\n", hookPath)
		if _, err := d.exec.Run(ctx, hookPath); err != nil {
			return fmt.Errorf("pre-deploy hook failed: %w", err)
		}
	}

	// 4. Stop and delete old container
	if d.incus.ContainerExists(ctx, svc.Name) {
		log.Printf("    Deleting old container: %s...\n", svc.Name)
		if err := d.incus.StopAndDeleteContainer(ctx, svc.Name); err != nil {
			return fmt.Errorf("delete old container %s: %w", svc.Name, err)
		}
	}

	// 5. Launch new container
	profiles := svc.Profiles
	if len(profiles) == 0 {
		profiles = []string{"base", "service"}
	}
	log.Printf("    Launching container %s from image %s...\n", svc.Name, imageRef)
	if err := d.incus.LaunchContainer(ctx, imageRef, svc.Name, profiles, svc.Limits); err != nil {
		return fmt.Errorf("launch container: %w", err)
	}

	// 6. Attach volumes BEFORE writing anything to mount path
	for _, vol := range svc.Volumes {
		log.Printf("    Attaching volume %s to %s at %s (shifted=%t)...\n", vol.Name, svc.Name, vol.Path, vol.Shifted)
		if err := d.incus.AttachVolume(ctx, svc.Name, vol.Pool, vol.Name, vol.Path, vol.Shifted); err != nil {
			return fmt.Errorf("attach volume: %w", err)
		}
	}

	// 7. Write EnvironmentFile
	env := make(map[string]string)
	if svc.EnvFile != "" {
		envFilePath := filepath.Join(configDir, svc.EnvFile)
		if data, err := os.ReadFile(envFilePath); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					env[parts[0]] = parts[1]
				}
			}
		}
	}
	for k, v := range svc.Env {
		env[k] = v
	}

	if len(env) > 0 {
		log.Printf("    Writing /etc/default/%s...\n", svc.Name)
		if err := d.incus.WriteEnvironmentFile(ctx, svc.Name, svc.Name, env); err != nil {
			return fmt.Errorf("write env file: %w", err)
		}
		_ = d.incus.RestartService(ctx, svc.Name, svc.Name)
	}

	// 8. Health-gate
	ip, err := d.incus.GetContainerIP(ctx, svc.Name)
	if err != nil {
		return fmt.Errorf("resolve IP for %s: %w", svc.Name, err)
	}
	log.Printf("    Container IP: %s\n", ip)

	if svc.HealthCheck.Path != "" {
		log.Printf("    Probing healthcheck (%s:%d%s)...\n", ip, svc.HealthCheck.Port, svc.HealthCheck.Path)
		if err := d.incus.HealthGate(ctx, ip, svc.HealthCheck); err != nil {
			return fmt.Errorf("health gate failed: %w", err)
		}
		log.Printf("    Healthcheck passed!\n")
	}

	// 9. Publish Caddy Route
	if svc.Routing != nil && svc.Routing.Domain != "" {
		log.Printf("    Publishing Caddy route: %s -> %s:%d...\n", svc.Routing.Domain, ip, svc.Routing.UpstreamPort)
		if err := d.caddy.PublishSite(ctx, svc.Name, *svc.Routing, ip); err != nil {
			return fmt.Errorf("publish caddy route: %w", err)
		}
	}

	// 10. Post-deploy hook
	if svc.Hooks.PostDeploy != "" {
		hookPath := filepath.Join(configDir, svc.Hooks.PostDeploy)
		log.Printf("    Running post-deploy hook: %s\n", hookPath)
		if _, err := d.exec.Run(ctx, hookPath); err != nil {
			return fmt.Errorf("post-deploy hook failed: %w", err)
		}
	}

	log.Printf("==> [Deploy] Successfully deployed %s (%s)\n", svc.Name, ip)
	return nil
}
