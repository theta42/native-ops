package engine

import (
	"context"
	"encoding/base64"
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

	// 3. Pre-deploy hook (executed on the host)
	if svc.Hooks.PreDeploy != "" {
		log.Printf("    Running pre-deploy hook for %s...\n", svc.Name)
		var scriptContent string
		svcScriptPath := filepath.Join(configDir, "services", svc.Name, svc.Hooks.PreDeploy)
		scriptPath := filepath.Join(configDir, svc.Hooks.PreDeploy)

		if data, err := os.ReadFile(svcScriptPath); err == nil {
			scriptContent = string(data)
		} else if data, err := os.ReadFile(scriptPath); err == nil {
			scriptContent = string(data)
		} else {
			scriptContent = svc.Hooks.PreDeploy
		}

		b64 := base64.StdEncoding.EncodeToString([]byte(scriptContent))
		out, err := d.exec.Run(ctx, fmt.Sprintf("echo '%s' | base64 -d | bash", b64))
		if err != nil {
			log.Printf("    Pre-deploy hook failed for %s: %s (err: %v)\n", svc.Name, out, err)
			return fmt.Errorf("pre-deploy hook failed for %s: %w (output: %s)", svc.Name, err, out)
		}
	}

	// 4. Stop and delete old container
	if d.incus.ContainerExists(ctx, svc.Name) {
		log.Printf("    Deleting old container: %s...\n", svc.Name)
		_ = d.incus.StopAndDeleteContainer(ctx, svc.Name)
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

	// 7. Execute Container Init Hook (if specified)
	if svc.Hooks.ContainerInit != "" {
		log.Printf("    Running container_init hook for %s...\n", svc.Name)
		var scriptContent string
		svcScriptPath := filepath.Join(configDir, "services", svc.Name, svc.Hooks.ContainerInit)
		scriptPath := filepath.Join(configDir, svc.Hooks.ContainerInit)

		if data, err := os.ReadFile(svcScriptPath); err == nil {
			scriptContent = string(data)
		} else if data, err := os.ReadFile(scriptPath); err == nil {
			scriptContent = string(data)
		} else {
			scriptContent = svc.Hooks.ContainerInit
		}

		b64 := base64.StdEncoding.EncodeToString([]byte(scriptContent))
		out, err := d.exec.Run(ctx, fmt.Sprintf("echo '%s' | base64 -d | incus exec %s -- bash", b64, svc.Name))
		if err != nil {
			log.Printf("    Container init failed for %s: %s (err: %v)\n", svc.Name, out, err)
			return fmt.Errorf("container_init hook failed for %s: %w (output: %s)", svc.Name, err, out)
		}
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

	if svc.Name == "edge" {
		_ = d.caddy.EnsureBaseCaddyfile(ctx)
		_ = d.caddy.Reload(ctx)
	}

	if svc.HealthCheck.Path != "" {
		log.Printf("    Probing healthcheck (%s:%d%s)...\n", ip, svc.HealthCheck.Port, svc.HealthCheck.Path)
		if err := d.incus.HealthGate(ctx, ip, svc.HealthCheck); err != nil {
			diag, _ := d.exec.Run(ctx, fmt.Sprintf("incus exec %s -- journalctl -u %s --no-pager -n 30 || true", svc.Name, svc.Name))
			log.Printf("    Health gate failed! Container %s logs:\n%s\n", svc.Name, diag)
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

	// 10. Post-deploy hook (executed on the host)
	if svc.Hooks.PostDeploy != "" {
		log.Printf("    Running post-deploy hook for %s...\n", svc.Name)
		var scriptContent string
		svcScriptPath := filepath.Join(configDir, "services", svc.Name, svc.Hooks.PostDeploy)
		scriptPath := filepath.Join(configDir, svc.Hooks.PostDeploy)

		if data, err := os.ReadFile(svcScriptPath); err == nil {
			scriptContent = string(data)
		} else if data, err := os.ReadFile(scriptPath); err == nil {
			scriptContent = string(data)
		} else {
			scriptContent = svc.Hooks.PostDeploy
		}

		b64 := base64.StdEncoding.EncodeToString([]byte(scriptContent))
		out, err := d.exec.Run(ctx, fmt.Sprintf("echo '%s' | base64 -d | bash", b64))
		if err != nil {
			log.Printf("    Post-deploy hook failed for %s: %s (err: %v)\n", svc.Name, out, err)
			return fmt.Errorf("post-deploy hook failed for %s: %w (output: %s)", svc.Name, err, out)
		}
	}

	log.Printf("==> [Deploy] Successfully deployed %s (%s)\n", svc.Name, ip)
	return nil
}
