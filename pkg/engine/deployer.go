package engine

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
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
	inst  *InstanceManager

	// healthGate is a field so tests can stub the wait.
	healthGate func(ctx context.Context, ip string, hc config.HealthCheckConfig) error
}

func NewDeployer(exec remote.Executor) *Deployer {
	ic := incus.NewClient(exec)
	return &Deployer{
		incus:      ic,
		caddy:      caddy.NewEdgeManager(exec, "edge"),
		exec:       exec,
		inst:       NewInstanceManager(exec),
		healthGate: ic.HealthGate,
	}
}

func hasRemotePrefix(ref string) bool {
	for _, p := range []string{"images:", "docker:", "local:", "quay:"} {
		if strings.HasPrefix(ref, p) {
			return true
		}
	}
	return false
}

// normalizeRef gives a non-local reference an explicit remote (Docker Hub by default).
func normalizeRef(ref string) string {
	if hasRemotePrefix(ref) || isFingerprint(ref) {
		return ref
	}
	return "docker:" + ref
}

// resolveServiceImage returns the reference to deploy. A local image alias
// resolves to its fingerprint (fp is set); anything else is an OCI/remote
// reference, compared by its recorded string because a tag cannot be resolved
// without pulling it.
func (d *Deployer) resolveServiceImage(ctx context.Context, ref string) (deploy, fp string) {
	if !strings.Contains(ref, "docker.io/") && !strings.Contains(ref, "/") && !hasRemotePrefix(ref) && !isFingerprint(ref) {
		if f, err := d.incus.ResolveImageFingerprint(ctx, ref); err == nil {
			return f, f
		}
	}
	return normalizeRef(ref), ""
}

// serviceEnv is the environment a service declares: env_file first, then inline env.
func serviceEnv(svc *config.ServiceConfig, configDir string) map[string]string {
	env := make(map[string]string)
	if svc.EnvFile != "" {
		if data, err := os.ReadFile(filepath.Join(configDir, svc.EnvFile)); err == nil {
			for k, v := range incus.ParseEnv(string(data)) {
				env[k] = v
			}
		}
	}
	for k, v := range svc.Env {
		env[k] = v
	}
	return env
}

// ensureEnv converges /etc/default/<service> to the declared environment:
// declared keys are set, keys that are not declared are preserved (they may be
// runtime secrets pushed by another system), and the file is only written when
// something actually differs. changed reports whether it was written, so the
// caller knows a restart is due.
func ensureEnv(ctx context.Context, ic *incus.Client, container, service string, declared map[string]string) (changed bool, err error) {
	if len(declared) == 0 {
		return false, nil
	}
	live, found, err := ic.PullFile(ctx, container, "/etc/default/"+service)
	if err != nil {
		return false, err
	}
	liveEnv := map[string]string{}
	if found {
		liveEnv = incus.ParseEnv(live)
	}
	merged, differs := incus.MergeEnv(liveEnv, declared)
	if found && !differs {
		return false, nil
	}
	if err := ic.WriteEnvironmentFile(ctx, container, service, merged); err != nil {
		return false, err
	}
	return true, nil
}

// hookScript resolves a hook to its script text: a file under services/<svc>/
// or the config root, else the hook value itself is the script.
func hookScript(configDir, svcName, hook string) string {
	if data, err := os.ReadFile(filepath.Join(configDir, "services", svcName, hook)); err == nil {
		return string(data)
	}
	if data, err := os.ReadFile(filepath.Join(configDir, hook)); err == nil {
		return string(data)
	}
	return hook
}

func (d *Deployer) runHostHook(ctx context.Context, label, svcName, script string) error {
	b64 := base64.StdEncoding.EncodeToString([]byte(script))
	out, err := d.exec.Run(ctx, fmt.Sprintf("echo '%s' | base64 -d | bash", b64))
	if err != nil {
		log.Printf("    %s hook failed for %s: %s (err: %v)\n", label, svcName, out, err)
		return fmt.Errorf("%s hook failed for %s: %w (output: %s)", label, svcName, err, out)
	}
	return nil
}

func (d *Deployer) runContainerHook(ctx context.Context, name, script string) error {
	b64 := base64.StdEncoding.EncodeToString([]byte(script))
	out, err := d.exec.Run(ctx, fmt.Sprintf("echo '%s' | base64 -d | incus exec %s -- bash", b64, incus.ShQuote(name)))
	if err != nil {
		log.Printf("    Container init failed for %s: %s (err: %v)\n", name, out, err)
		return fmt.Errorf("container_init hook failed for %s: %w (output: %s)", name, err, out)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func servicePool(pool string) string {
	if pool == "" {
		return "default"
	}
	return pool
}

// candidateIPs returns the container's global IPv4 addresses with preferred first.
func (d *Deployer) candidateIPs(ctx context.Context, name, preferred string) ([]string, error) {
	ips, err := d.incus.GlobalIPv4s(ctx, name)
	if err != nil {
		return nil, err
	}
	if preferred == "" {
		return ips, nil
	}
	out := []string{preferred}
	for _, ip := range ips {
		if ip != preferred {
			out = append(out, ip)
		}
	}
	return out, nil
}

// DeployService converges a service to its manifest. It is idempotent: a
// service that already matches is left completely alone (no restart, no
// snapshot, no Caddy reload), so this can run on every CI push.
//
//   - Not there yet: launched from scratch.
//   - Image changed (a local alias now points at another fingerprint, or the
//     declared OCI reference changed): replaced through the same safe path as
//     `instance update` — volume snapshot first, config and env carried over,
//     health-gated, rolled back to the previous image on failure.
//   - Anything else that drifted (limits, an unattached volume, declared env
//     values, the Caddy route) is corrected in place without replacing.
//
// pre_deploy, container_init and post_deploy hooks run only when the service
// is actually (re)deployed. Env keys that are not declared are preserved, and
// profile drift is reported but not changed.
func (d *Deployer) DeployService(ctx context.Context, svc *config.ServiceConfig, configDir string) error {
	if !incus.ValidName(svc.Name) {
		return fmt.Errorf("invalid service name %q", svc.Name)
	}
	log.Printf("==> [Deploy] Reconciling service: %s\n", svc.Name)
	deployRef, fp := d.resolveServiceImage(ctx, svc.Image)
	exists, err := d.incus.InstanceExists(ctx, svc.Name)
	if err != nil {
		return err
	}
	if !exists {
		return d.deployFresh(ctx, svc, configDir, deployRef)
	}
	return d.converge(ctx, svc, configDir, deployRef, fp)
}

func (d *Deployer) profilesOf(svc *config.ServiceConfig) []string {
	if len(svc.Profiles) == 0 {
		return []string{"base", "service"}
	}
	return svc.Profiles
}

func (d *Deployer) deployFresh(ctx context.Context, svc *config.ServiceConfig, configDir, deployRef string) error {
	log.Printf("    %s does not exist yet; launching\n", svc.Name)
	for _, vol := range svc.Volumes {
		if err := d.incus.EnsureVolume(ctx, vol.Pool, vol.Name); err != nil {
			return fmt.Errorf("ensure volume %s: %w", vol.Name, err)
		}
	}
	if svc.Hooks.PreDeploy != "" {
		log.Printf("    Running pre-deploy hook for %s...\n", svc.Name)
		if err := d.runHostHook(ctx, "pre-deploy", svc.Name, hookScript(configDir, svc.Name, svc.Hooks.PreDeploy)); err != nil {
			return err
		}
	}

	cfg := make(map[string]string, len(svc.Limits)+1)
	for k, v := range svc.Limits {
		cfg[k] = v
	}
	cfg[incus.ImageKey] = deployRef
	log.Printf("    Launching container %s from image %s...\n", svc.Name, deployRef)
	if err := d.incus.LaunchContainer(ctx, deployRef, svc.Name, d.profilesOf(svc), cfg); err != nil {
		return fmt.Errorf("launch container: %w", err)
	}

	// Attach volumes BEFORE writing anything to their mount paths.
	for _, vol := range svc.Volumes {
		log.Printf("    Attaching volume %s to %s at %s (shifted=%t)...\n", vol.Name, svc.Name, vol.Path, vol.Shifted)
		if err := d.incus.EnsureVolumeAttached(ctx, svc.Name, servicePool(vol.Pool), vol.Name, vol.Path, vol.Shifted); err != nil {
			return fmt.Errorf("attach volume: %w", err)
		}
	}

	if svc.Hooks.ContainerInit != "" {
		log.Printf("    Running container_init hook for %s...\n", svc.Name)
		if err := d.runContainerHook(ctx, svc.Name, hookScript(configDir, svc.Name, svc.Hooks.ContainerInit)); err != nil {
			return err
		}
	}

	if changed, err := ensureEnv(ctx, d.incus, svc.Name, svc.Name, serviceEnv(svc, configDir)); err != nil {
		return fmt.Errorf("write env file: %w", err)
	} else if changed {
		log.Printf("    Wrote /etc/default/%s\n", svc.Name)
		if err := d.incus.RestartService(ctx, svc.Name, svc.Name); err != nil {
			log.Printf("    WARNING: %v\n", err)
		}
	}

	ip, err := d.incus.GetContainerIP(ctx, svc.Name)
	if err != nil {
		return fmt.Errorf("resolve IP for %s: %w", svc.Name, err)
	}
	log.Printf("    Container IP: %s\n", ip)

	if svc.Name == "edge" {
		if err := d.caddy.EnsureBaseCaddyfile(ctx); err != nil {
			return err
		}
		if err := d.caddy.Reload(ctx); err != nil {
			return err
		}
	}

	if err := d.checkHealth(ctx, svc, ip); err != nil {
		return err
	}
	if err := d.publishRoute(ctx, svc, ip); err != nil {
		return err
	}
	if svc.Hooks.PostDeploy != "" {
		log.Printf("    Running post-deploy hook for %s...\n", svc.Name)
		if err := d.runHostHook(ctx, "post-deploy", svc.Name, hookScript(configDir, svc.Name, svc.Hooks.PostDeploy)); err != nil {
			return err
		}
	}
	log.Printf("==> [Deploy] Deployed %s (%s)\n", svc.Name, ip)
	return nil
}

func (d *Deployer) checkHealth(ctx context.Context, svc *config.ServiceConfig, ip string) error {
	if svc.HealthCheck.Path == "" {
		return nil
	}
	log.Printf("    Probing healthcheck (%s:%d%s)...\n", ip, svc.HealthCheck.Port, svc.HealthCheck.Path)
	if err := d.healthGate(ctx, ip, svc.HealthCheck); err != nil {
		diag, _ := d.exec.Run(ctx, fmt.Sprintf("incus exec %s -- journalctl -u %s --no-pager -n 30 || true", incus.ShQuote(svc.Name), incus.ShQuote(svc.Name)))
		log.Printf("    Health gate failed! Container %s logs:\n%s\n", svc.Name, diag)
		return fmt.Errorf("health gate failed: %w", err)
	}
	return nil
}

func (d *Deployer) publishRoute(ctx context.Context, svc *config.ServiceConfig, preferredIP string) error {
	if svc.Routing == nil || svc.Routing.Domain == "" {
		return nil
	}
	ips, err := d.candidateIPs(ctx, svc.Name, preferredIP)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("no IPv4 address to route %s to", svc.Name)
	}
	if err := d.caddy.PublishSiteFor(ctx, svc.Name, *svc.Routing, ips); err != nil {
		return fmt.Errorf("publish caddy route: %w", err)
	}
	return nil
}

func sameProfiles(live, declared []string) bool {
	norm := func(p []string) string {
		var out []string
		for _, x := range p {
			if x != "default" {
				out = append(out, x)
			}
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	return norm(live) == norm(declared)
}

func (d *Deployer) converge(ctx context.Context, svc *config.ServiceConfig, configDir, deployRef, fp string) error {
	st, err := d.incus.CaptureInstanceState(ctx, svc.Name)
	if err != nil {
		return err
	}

	// Which image is wanted, and is that what runs?
	replace := false
	if fp != "" {
		switch {
		case st.BaseImage == "":
			log.Printf("    WARNING: the image %s was launched from is unknown; not replacing %s\n", svc.Image, svc.Name)
		case st.BaseImage != fp:
			replace = true
		}
	} else {
		switch recorded := st.Config[incus.ImageKey]; {
		case recorded == "":
			// Launched before references were recorded: adopt what runs as the desired state.
			if err := d.incus.SetInstanceConfig(ctx, svc.Name, incus.ImageKey, deployRef); err != nil {
				return err
			}
		case recorded != deployRef:
			replace = true
		}
	}

	declaredEnv := serviceEnv(svc, configDir)
	changed := false

	if replace {
		log.Printf("    Image changed; replacing %s through the safe update path\n", svc.Name)
		if svc.Hooks.PreDeploy != "" {
			if err := d.runHostHook(ctx, "pre-deploy", svc.Name, hookScript(configDir, svc.Name, svc.Hooks.PreDeploy)); err != nil {
				return err
			}
		}
		opts := UpdateOptions{Service: svc.Name, HealthCheck: svc.HealthCheck}
		if svc.Hooks.ContainerInit != "" {
			script := hookScript(configDir, svc.Name, svc.Hooks.ContainerInit)
			opts.BeforeStart = func(ctx context.Context, name string) error { return d.runContainerHook(ctx, name, script) }
		}
		if err := d.inst.Update(ctx, svc.Name, deployRef, opts); err != nil {
			return err
		}
		changed = true
		// The replacement carries the live env over; now apply what the manifest declares.
		if envChanged, err := ensureEnv(ctx, d.incus, svc.Name, svc.Name, declaredEnv); err != nil {
			return err
		} else if envChanged {
			if err := d.incus.RestartService(ctx, svc.Name, svc.Name); err != nil {
				return err
			}
		}
	} else {
		// Limits apply live, no restart.
		drift := map[string]string{}
		for _, k := range sortedKeys(svc.Limits) {
			if st.Config[k] != svc.Limits[k] {
				drift[k] = svc.Limits[k]
			}
		}
		if len(drift) > 0 {
			log.Printf("    Applying changed limits to %s: %v\n", svc.Name, drift)
			if err := d.incus.ResizeLimits(ctx, svc.Name, drift); err != nil {
				return err
			}
			changed = true
		}
		for _, vol := range svc.Volumes {
			if err := d.incus.EnsureVolume(ctx, vol.Pool, vol.Name); err != nil {
				return fmt.Errorf("ensure volume %s: %w", vol.Name, err)
			}
			if err := d.incus.EnsureVolumeAttached(ctx, svc.Name, servicePool(vol.Pool), vol.Name, vol.Path, vol.Shifted); err != nil {
				return fmt.Errorf("attach volume: %w", err)
			}
		}
		if envChanged, err := ensureEnv(ctx, d.incus, svc.Name, svc.Name, declaredEnv); err != nil {
			return err
		} else if envChanged {
			log.Printf("    Environment changed; restarting %s\n", svc.Name)
			if err := d.incus.RestartService(ctx, svc.Name, svc.Name); err != nil {
				return err
			}
			changed = true
		}
		if !sameProfiles(st.Profiles, d.profilesOf(svc)) {
			log.Printf("    WARNING: profiles of %s differ (live %v, declared %v); apply does not change profiles\n", svc.Name, st.Profiles, d.profilesOf(svc))
		}
		if svc.HealthCheck.Path != "" {
			ip, err := d.incus.ContainerIPv4(ctx, svc.Name, 30*time.Second)
			if err != nil {
				return fmt.Errorf("%s is not reachable: %w", svc.Name, err)
			}
			if err := d.checkHealth(ctx, svc, ip); err != nil {
				return err
			}
		}
	}

	if err := d.publishRoute(ctx, svc, ""); err != nil {
		return err
	}
	if replace && svc.Hooks.PostDeploy != "" {
		log.Printf("    Running post-deploy hook for %s...\n", svc.Name)
		if err := d.runHostHook(ctx, "post-deploy", svc.Name, hookScript(configDir, svc.Name, svc.Hooks.PostDeploy)); err != nil {
			return err
		}
	}
	if changed {
		log.Printf("==> [Deploy] Converged %s\n", svc.Name)
	} else {
		log.Printf("==> [Deploy] %s already matches its manifest\n", svc.Name)
	}
	return nil
}
