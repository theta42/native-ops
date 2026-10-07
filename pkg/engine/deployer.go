package engine

import (
	"context"
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

	logf func(format string, a ...any)
	// bind, when set, is mixed into a plan's hash: a digest of everything the manifest says that the
	// plan does not print (environment values, hook bodies, health checks), so that the hash of a
	// plan covers what apply would really do. See WithBindKey.
	bind func(data []byte) string
	// secrets resolves a service's env_from names. The daemon gives it its secret store (and nothing
	// else); on a host run without a daemon it is the process environment.
	secrets SecretLookup
}

// SecretLookup reads a secret by name; ok is false when it is not set.
type SecretLookup func(name string) (value string, ok bool)

// SetSecrets makes env_from read from lookup (the daemon's secret store) instead of the process
// environment.
func (d *Deployer) SetSecrets(lookup SecretLookup) {
	if lookup != nil {
		d.secrets = lookup
	}
}

func NewDeployer(exec remote.Executor) *Deployer {
	ic := incus.NewClient(exec)
	return &Deployer{
		incus:      ic,
		caddy:      caddy.NewEdgeManager(exec, "edge"),
		exec:       exec,
		inst:       NewInstanceManager(exec),
		healthGate: ic.HealthGate,
		logf:       log.Printf,
		secrets:    os.LookupEnv,
	}
}

// SetLogger sends the progress this deployer (and its instance manager) reports to logf instead of
// the process log. A daemon uses it to keep each job's log to itself.
func (d *Deployer) SetLogger(logf func(format string, a ...any)) {
	d.logf = logf
	d.inst.logf = logf
}

func (d *Deployer) log(format string, a ...any) {
	if d.logf != nil {
		d.logf(format, a...)
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
	// The docker remote is Docker Hub, so a full Docker Hub reference drops its registry:
	// docker.io/library/caddy:2 is docker:library/caddy:2, not docker:docker.io/library/caddy:2.
	return "docker:" + strings.TrimPrefix(ref, "docker.io/")
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

// MissingSecretsError says which env_from secrets are not set, by name (never a value).
type MissingSecretsError struct {
	Service string
	Names   []string
}

func (e *MissingSecretsError) Error() string {
	return fmt.Sprintf("%s: env_from needs secret(s) %s, which are not set; add them to the git server's secret store and sync them (`native-ops remote secret-sync`)",
		e.Service, strings.Join(e.Names, ", "))
}

// declaredEnv is everything a service declares for /etc/default/<service>: env_file, env, then env_from
// resolved through d.secrets. A secret that is not set is an error (*MissingSecretsError), never an empty
// value or a key silently left out.
func (d *Deployer) declaredEnv(svc *config.ServiceConfig, configDir string) (map[string]string, error) {
	env := serviceEnv(svc, configDir)
	var missing []string
	for _, key := range sortedKeys(svc.EnvFrom) {
		name := svc.EnvFrom[key]
		v, ok := d.secrets(name)
		if !ok || v == "" {
			missing = append(missing, name)
			continue
		}
		env[key] = v
	}
	if len(missing) > 0 {
		return env, &MissingSecretsError{Service: svc.Name, Names: missing}
	}
	return env, nil
}

// envKeyLabels names keys for a plan, saying which secret an env_from key comes from (its name only).
func envKeyLabels(svc *config.ServiceConfig, keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		if name, ok := svc.EnvFrom[k]; ok {
			out[i] = fmt.Sprintf("%s (from secret %s)", k, name)
		} else {
			out[i] = k
		}
	}
	return out
}

// envState is how the live /etc/default/<service> compares with the declared environment.
type envState struct {
	Found   bool              // the file exists
	Merged  map[string]string // what the file should contain: live keys kept, declared keys set
	Differs bool              // a declared key is missing or has another value
	Added   []string          // declared keys the file lacks (names only)
	Changed []string          // declared keys whose value differs (names only)
	Kept    int               // live keys the manifest does not declare; these are preserved
}

// readEnvState reads the live environment file and works out what converging it would do. It
// never writes, and is shared by ensureEnv (which acts on it) and the plan (which reports it).
func readEnvState(ctx context.Context, ic *incus.Client, container, service string, declared map[string]string) (*envState, error) {
	live, found, err := ic.PullFile(ctx, container, "/etc/default/"+service)
	if err != nil {
		return nil, err
	}
	liveEnv := map[string]string{}
	if found {
		liveEnv = incus.ParseEnv(live)
	}
	merged, differs := incus.MergeEnv(liveEnv, declared)
	st := &envState{Found: found, Merged: merged, Differs: differs}
	for _, k := range sortedKeys(declared) {
		if cur, ok := liveEnv[k]; !ok {
			st.Added = append(st.Added, k)
		} else if cur != declared[k] {
			st.Changed = append(st.Changed, k)
		}
	}
	for k := range liveEnv {
		if _, ok := declared[k]; !ok {
			st.Kept++
		}
	}
	return st, nil
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
	st, err := readEnvState(ctx, ic, container, service, declared)
	if err != nil {
		return false, err
	}
	if st.Found && !st.Differs {
		return false, nil
	}
	if err := ic.WriteEnvironmentFile(ctx, container, service, st.Merged); err != nil {
		return false, err
	}
	return true, nil
}

// hookScript resolves a hook to its script text: a file under services/<svc>/
// or the config root, else the hook value itself is the script.
func hookScript(configDir, svcName, hook string) string {
	// A hook that names a path outside the config directory is never read as a file: it is
	// the script text itself, like any other value that is not a file here.
	if config.RelativeInside(hook) {
		if data, err := os.ReadFile(filepath.Join(configDir, "services", svcName, hook)); err == nil {
			return string(data)
		}
		if data, err := os.ReadFile(filepath.Join(configDir, hook)); err == nil {
			return string(data)
		}
	}
	return hook
}

func (d *Deployer) runHostHook(ctx context.Context, label, svcName, script string) error {
	// The script goes over stdin: a command line is visible to every user on the host, has a size
	// limit, and is quoted in errors.
	out, err := d.exec.RunWithInput(ctx, "bash -s", strings.NewReader(script))
	if err != nil {
		d.log("    %s hook failed for %s: %s (err: %v)\n", label, svcName, out, err)
		return fmt.Errorf("%s hook failed for %s: %w (output: %s)", label, svcName, err, out)
	}
	return nil
}

func (d *Deployer) runContainerHook(ctx context.Context, name, script string) error {
	out, err := d.exec.RunWithInput(ctx, fmt.Sprintf("incus exec %s -- bash -s", incus.ShQuote(name)), strings.NewReader(script))
	if err != nil {
		d.log("    Container init failed for %s: %s (err: %v)\n", name, out, err)
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
	d.log("==> [Deploy] Reconciling service: %s\n", svc.Name)
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
	d.log("    %s does not exist yet; launching\n", svc.Name)
	for _, vol := range svc.Volumes {
		if err := d.incus.EnsureVolume(ctx, vol.Pool, vol.Name, vol.Shifted); err != nil {
			return fmt.Errorf("ensure volume %s: %w", vol.Name, err)
		}
	}
	if svc.Hooks.PreDeploy != "" {
		d.log("    Running pre-deploy hook for %s...\n", svc.Name)
		if err := d.runHostHook(ctx, "pre-deploy", svc.Name, hookScript(configDir, svc.Name, svc.Hooks.PreDeploy)); err != nil {
			return err
		}
	}

	cfg := make(map[string]string, len(svc.Limits)+1)
	for k, v := range svc.Limits {
		cfg[k] = v
	}
	cfg[incus.ImageKey] = deployRef
	for k, v := range svc.Labels {
		cfg[incus.LabelKey(k)] = v
	}
	d.log("    Launching container %s from image %s...\n", svc.Name, deployRef)
	if err := d.incus.LaunchContainer(ctx, deployRef, svc.Name, d.profilesOf(svc), cfg); err != nil {
		return fmt.Errorf("launch container: %w", err)
	}

	// Attach volumes BEFORE writing anything to their mount paths.
	for _, vol := range svc.Volumes {
		d.log("    Attaching volume %s to %s at %s (shifted=%t)...\n", vol.Name, svc.Name, vol.Path, vol.Shifted)
		if err := d.incus.EnsureVolumeAttached(ctx, svc.Name, servicePool(vol.Pool), vol.Name, vol.Path, vol.Shifted); err != nil {
			return fmt.Errorf("attach volume: %w", err)
		}
		if vol.Owner != "" {
			if err := d.incus.EnsurePathOwner(ctx, svc.Name, vol.Path, vol.Owner); err != nil {
				return fmt.Errorf("set owner of volume %s: %w", vol.Name, err)
			}
		}
	}

	// Publish the raw host ports the service declares (a protocol the edge cannot carry, e.g.
	// git-over-SSH).
	for _, fwd := range svc.Forwards {
		d.log("    Publishing host port %s (%s -> %s)...\n", fwd.Name, fwd.Listen, fwd.Target)
	}
	if _, err := ensureForwards(ctx, d.incus, svc.Name, svc.Forwards); err != nil {
		return err
	}

	if svc.Hooks.ContainerInit != "" {
		d.log("    Running container_init hook for %s...\n", svc.Name)
		if err := d.runContainerHook(ctx, svc.Name, hookScript(configDir, svc.Name, svc.Hooks.ContainerInit)); err != nil {
			return err
		}
	}

	freshEnv, err := d.declaredEnv(svc, configDir)
	if err != nil {
		return err
	}
	if changed, err := ensureEnv(ctx, d.incus, svc.Name, svc.Unit(), freshEnv); err != nil {
		return fmt.Errorf("write env file: %w", err)
	} else if changed {
		d.log("    Wrote /etc/default/%s\n", svc.Unit())
		if err := d.incus.RestartService(ctx, svc.Name, svc.Unit()); err != nil {
			d.log("    WARNING: %v\n", err)
		}
	}

	ip, err := d.incus.GetContainerIP(ctx, svc.Name)
	if err != nil {
		return fmt.Errorf("resolve IP for %s: %w", svc.Name, err)
	}
	d.log("    Container IP: %s\n", ip)

	if svc.Name == "edge" {
		// A new edge gets the repo's edge/Caddyfile when there is one (the same file POST
		// /v1/edge/apply applies), else the base Caddyfile that imports the sites directory.
		if repo, err := os.ReadFile(filepath.Join(configDir, "edge", "Caddyfile")); err == nil {
			if _, err := d.caddy.SyncCaddyfile(ctx, string(repo)); err != nil {
				return fmt.Errorf("apply edge/Caddyfile to the new edge: %w", err)
			}
		} else {
			if err := d.caddy.EnsureBaseCaddyfile(ctx); err != nil {
				return err
			}
			if err := d.caddy.Reload(ctx); err != nil {
				return err
			}
		}
	}

	if err := d.checkHealth(ctx, svc, ip); err != nil {
		return err
	}
	if err := d.publishRoute(ctx, svc, ip); err != nil {
		return err
	}
	if svc.Hooks.PostDeploy != "" {
		d.log("    Running post-deploy hook for %s...\n", svc.Name)
		if err := d.runHostHook(ctx, "post-deploy", svc.Name, hookScript(configDir, svc.Name, svc.Hooks.PostDeploy)); err != nil {
			return err
		}
	}
	d.log("==> [Deploy] Deployed %s (%s)\n", svc.Name, ip)
	return nil
}

func (d *Deployer) checkHealth(ctx context.Context, svc *config.ServiceConfig, ip string) error {
	if svc.HealthCheck.Path == "" {
		return nil
	}
	d.log("    Probing healthcheck (%s:%d%s)...\n", ip, svc.HealthCheck.Port, svc.HealthCheck.Path)
	if err := d.healthGate(ctx, ip, svc.HealthCheck); err != nil {
		diag, _ := d.exec.Run(ctx, fmt.Sprintf("incus exec %s -- journalctl -u %s --no-pager -n 30 || true", incus.ShQuote(svc.Name), incus.ShQuote(svc.Name)))
		d.log("    Health gate failed! Container %s logs:\n%s\n", svc.Name, diag)
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

// imageVerdict is what a running instance's image means for the manifest.
type imageVerdict int

const (
	imageCurrent imageVerdict = iota // runs what the manifest names
	imageReplace                     // the manifest names a different image
	imageAdopt                       // launched before references were recorded: record it, change nothing else
	imageUnknown                     // launched from an image that cannot be identified: leave it alone
)

// imageDecision compares the image a service is declared with to the one its instance runs.
// A local alias is compared by fingerprint (fp is set); an OCI reference by the reference
// recorded on the instance, because a tag cannot be resolved without pulling it.
func imageDecision(st *incus.InstanceState, fp, deployRef string) imageVerdict {
	if fp != "" {
		switch {
		case st.BaseImage == "":
			return imageUnknown
		case st.BaseImage != fp:
			return imageReplace
		}
		return imageCurrent
	}
	switch recorded := st.Config[incus.ImageKey]; {
	case recorded == "":
		return imageAdopt
	case recorded != deployRef:
		return imageReplace
	}
	return imageCurrent
}

// limitsDrift returns the declared limits whose live value differs.
func limitsDrift(st *incus.InstanceState, limits map[string]string) map[string]string {
	drift := map[string]string{}
	for _, k := range sortedKeys(limits) {
		if st.Config[k] != limits[k] {
			drift[k] = limits[k]
		}
	}
	return drift
}

func (d *Deployer) converge(ctx context.Context, svc *config.ServiceConfig, configDir, deployRef, fp string) error {
	st, err := d.incus.CaptureInstanceState(ctx, svc.Name)
	if err != nil {
		return err
	}

	// Which image is wanted, and is that what runs?
	replace := false
	switch imageDecision(st, fp, deployRef) {
	case imageUnknown:
		d.log("    WARNING: the image %s was launched from is unknown; not replacing %s\n", svc.Image, svc.Name)
	case imageAdopt:
		// Launched before references were recorded: adopt what runs as the desired state.
		if err := d.incus.SetInstanceConfig(ctx, svc.Name, incus.ImageKey, deployRef); err != nil {
			return err
		}
	case imageReplace:
		replace = true
	}

	declaredEnv, err := d.declaredEnv(svc, configDir)
	if err != nil {
		return err
	}
	changed := false

	if replace {
		d.log("    Image changed; replacing %s through the safe update path\n", svc.Name)
		if svc.Hooks.PreDeploy != "" {
			if err := d.runHostHook(ctx, "pre-deploy", svc.Name, hookScript(configDir, svc.Name, svc.Hooks.PreDeploy)); err != nil {
				return err
			}
		}
		opts := UpdateOptions{Service: svc.Unit(), HealthCheck: svc.HealthCheck}
		if svc.Hooks.ContainerInit != "" {
			script := hookScript(configDir, svc.Name, svc.Hooks.ContainerInit)
			opts.BeforeStart = func(ctx context.Context, name string) error { return d.runContainerHook(ctx, name, script) }
		}
		if err := d.inst.Update(ctx, svc.Name, deployRef, opts); err != nil {
			return err
		}
		changed = true
		// The replacement carried the labels over; make them what the manifest declares (live, no restart).
		if len(labelsDrift(st.Config, svc.Labels)) > 0 {
			d.log("    Applying changed labels to %s\n", svc.Name)
			if err := reconcileLabels(ctx, d.incus, svc.Name, st.Config, svc.Labels); err != nil {
				return err
			}
		}
		// The replacement carries the live env over; now apply what the manifest declares.
		if envChanged, err := ensureEnv(ctx, d.incus, svc.Name, svc.Unit(), declaredEnv); err != nil {
			return err
		} else if envChanged {
			if err := d.incus.RestartService(ctx, svc.Name, svc.Unit()); err != nil {
				return err
			}
		}
	} else {
		// Labels apply live, no restart.
		if drift := labelsDrift(st.Config, svc.Labels); len(drift) > 0 {
			d.log("    Applying changed labels to %s: %s\n", svc.Name, strings.Join(drift, ", "))
			if err := reconcileLabels(ctx, d.incus, svc.Name, st.Config, svc.Labels); err != nil {
				return err
			}
			changed = true
		}
		// Limits apply live, no restart.
		drift := limitsDrift(st, svc.Limits)
		if len(drift) > 0 {
			d.log("    Applying changed limits to %s: %v\n", svc.Name, drift)
			if err := d.incus.ResizeLimits(ctx, svc.Name, drift); err != nil {
				return err
			}
			changed = true
		}
		for _, vol := range svc.Volumes {
			if err := d.incus.EnsureVolume(ctx, vol.Pool, vol.Name, vol.Shifted); err != nil {
				return fmt.Errorf("ensure volume %s: %w", vol.Name, err)
			}
			if err := d.incus.EnsureVolumeAttached(ctx, svc.Name, servicePool(vol.Pool), vol.Name, vol.Path, vol.Shifted); err != nil {
				return fmt.Errorf("attach volume: %w", err)
			}
			if vol.Owner != "" {
				if err := d.incus.EnsurePathOwner(ctx, svc.Name, vol.Path, vol.Owner); err != nil {
					return fmt.Errorf("set owner of volume %s: %w", vol.Name, err)
				}
			}
		}
		if envChanged, err := ensureEnv(ctx, d.incus, svc.Name, svc.Unit(), declaredEnv); err != nil {
			return err
		} else if envChanged {
			d.log("    Environment changed; restarting %s\n", svc.Unit())
			if err := d.incus.RestartService(ctx, svc.Name, svc.Unit()); err != nil {
				return err
			}
			changed = true
		}
		if !sameProfiles(st.Profiles, d.profilesOf(svc)) {
			d.log("    WARNING: profiles of %s differ (live %v, declared %v); apply does not change profiles\n", svc.Name, st.Profiles, d.profilesOf(svc))
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

	// Re-publish the declared host ports, so a forward added to the manifest (or one lost with a
	// replaced container) is created without a rebuild.
	if fwChanged, err := ensureForwards(ctx, d.incus, svc.Name, svc.Forwards); err != nil {
		return err
	} else if fwChanged {
		d.log("    Published host ports for %s\n", svc.Name)
		changed = true
	}

	if err := d.publishRoute(ctx, svc, ""); err != nil {
		return err
	}
	if replace && svc.Hooks.PostDeploy != "" {
		d.log("    Running post-deploy hook for %s...\n", svc.Name)
		if err := d.runHostHook(ctx, "post-deploy", svc.Name, hookScript(configDir, svc.Name, svc.Hooks.PostDeploy)); err != nil {
			return err
		}
	}
	if changed {
		d.log("==> [Deploy] Converged %s\n", svc.Name)
	} else {
		d.log("==> [Deploy] %s already matches its manifest\n", svc.Name)
	}
	return nil
}
