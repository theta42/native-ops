package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

// hostSim is a small stateful stand-in for one Incus host (containers,
// volumes, image aliases, files, and a Caddy edge container). It lets the tests
// run a command twice and assert that the second run changes nothing.
type hostSim struct {
	t       *testing.T
	ctrs    map[string]*simCtr
	volumes map[string]bool
	aliases map[string]string
	cmds    []string
	hooks   []string // host hook scripts executed, in order
	inits   []string // container_init scripts executed, in order
	snaps   []string
	nextIP  int
}

type simCtr struct {
	status   string
	config   map[string]string
	devices  map[string]map[string]string
	profiles []string
	files    map[string]string
	ip       string
	restarts int
	reloads  int
}

var _ remote.Executor = (*hostSim)(nil)

func newHostSim(t *testing.T) *hostSim {
	s := &hostSim{t: t, ctrs: map[string]*simCtr{}, volumes: map[string]bool{}, aliases: map[string]string{}, nextIP: 30}
	s.ctrs["edge"] = &simCtr{status: "Running", config: map[string]string{}, devices: map[string]map[string]string{}, profiles: []string{"default"},
		files: map[string]string{"/etc/caddy/Caddyfile": "import /etc/caddy/sites/*.caddy\n"}, ip: "10.0.100.10"}
	return s
}

var (
	simLaunchRe  = regexp.MustCompile(`^incus launch '([^']+)' '([^']+)'(.*)$`)
	simFlagRe    = regexp.MustCompile(`--(profile|config) '([^']*)'`)
	simListRe    = regexp.MustCompile(`^incus list '?([^' ]+)'? --format json$`)
	simDevRe     = regexp.MustCompile(`^incus config device add '([^']+)' '([^']+)' '?(\w+)'?((?: '[^']*')*)$`)
	simPushRe    = regexp.MustCompile(`^printf %s '([^']*)' \| base64 -d \| incus file push .* - '([^/']+)(/[^']*)'$`)
	simPullRe    = regexp.MustCompile(`^incus file pull '([^/']+)(/[^']*)' -$`)
	simExecRe    = regexp.MustCompile(`^incus exec '?([^' ]+)'? -- (.*)$`)
	simHookRe    = regexp.MustCompile(`^echo '([^']*)' \| base64 -d \| (bash|incus exec '([^']+)' -- bash)$`)
	simQuotedRe  = regexp.MustCompile(`'([^']*)'`)
	simMutatorRe = regexp.MustCompile(`^incus (launch|delete|stop|start|restart) |^incus config (set|device add) |^incus storage volume (create|snapshot create) |incus file push|systemctl restart|caddy reload|rm -f '/etc/caddy|ip addr add|\| bash$|-- bash$`)
)

func (s *hostSim) get(name string) (*simCtr, bool) { c, ok := s.ctrs[name]; return c, ok }

func (s *hostSim) Run(_ context.Context, cmd string) (string, error) {
	s.cmds = append(s.cmds, cmd)
	q := simQuotedRe.FindAllStringSubmatch(cmd, -1)
	arg := func(i int) string { return q[i][1] }

	switch {
	case cmd == "incus image alias list --format json":
		type row struct{ Name, Target string }
		var rows []map[string]string
		for n, t := range s.aliases {
			rows = append(rows, map[string]string{"name": n, "target": t})
		}
		b, _ := json.Marshal(rows)
		return string(b), nil
	case cmd == "incus image list --format json":
		return "[]", nil
	case simListRe.MatchString(cmd):
		name := simListRe.FindStringSubmatch(cmd)[1]
		c, ok := s.get(name)
		if !ok {
			return "[]", nil
		}
		b, _ := json.Marshal([]map[string]any{{"name": name, "status": c.status, "state": map[string]any{"network": map[string]any{
			"lo":   map[string]any{"addresses": []map[string]string{{"family": "inet", "address": "127.0.0.1", "scope": "local"}}},
			"eth0": map[string]any{"addresses": []map[string]string{{"family": "inet", "address": c.ip, "scope": "global"}}},
		}}}})
		return string(b), nil
	case strings.HasPrefix(cmd, "incus storage volume show "):
		if !s.volumes[q[len(q)-1][1]] {
			return "", errors.New("Error: Storage volume not found")
		}
	case strings.HasPrefix(cmd, "incus storage volume create "):
		s.volumes[q[len(q)-1][1]] = true
	case strings.HasPrefix(cmd, "incus storage volume delete "):
		delete(s.volumes, q[len(q)-1][1])
	case strings.HasPrefix(cmd, "incus storage volume snapshot create "):
		s.snaps = append(s.snaps, arg(1)+"@"+arg(2))
	case simLaunchRe.MatchString(cmd):
		m := simLaunchRe.FindStringSubmatch(cmd)
		if _, exists := s.ctrs[m[2]]; exists {
			return "", errors.New("Error: Instance already exists")
		}
		c := &simCtr{status: "Running", config: map[string]string{}, devices: map[string]map[string]string{}, profiles: []string{"default"}, files: map[string]string{}}
		c.config["volatile.base_image"] = m[1]
		if len(m[1]) != 64 {
			c.config["volatile.base_image"] = strings.Repeat("d", 64)
		}
		for _, f := range simFlagRe.FindAllStringSubmatch(m[3], -1) {
			if f[1] == "profile" {
				if f[2] != "default" {
					c.profiles = append(c.profiles, f[2])
				}
			} else {
				k, v, _ := strings.Cut(f[2], "=")
				c.config[k] = v
			}
		}
		s.nextIP++
		c.ip = fmt.Sprintf("10.0.100.%d", s.nextIP)
		s.ctrs[m[2]] = c
	case strings.HasPrefix(cmd, "incus config show "):
		c, ok := s.get(arg(0))
		if !ok {
			return "", errors.New("Error: Instance not found")
		}
		var sb strings.Builder
		sb.WriteString("architecture: x86_64\nconfig:\n")
		keys := make([]string, 0, len(c.config))
		for k := range c.config {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&sb, "  %s: %q\n", k, c.config[k])
		}
		sb.WriteString("devices:\n")
		if len(c.devices) == 0 {
			sb.Reset()
			sb.WriteString("architecture: x86_64\nconfig:\n")
			for _, k := range keys {
				fmt.Fprintf(&sb, "  %s: %q\n", k, c.config[k])
			}
			sb.WriteString("devices: {}\n")
		}
		for dn, d := range c.devices {
			fmt.Fprintf(&sb, "  %s:\n", dn)
			for k, v := range d {
				fmt.Fprintf(&sb, "    %s: %s\n", k, v)
			}
		}
		sb.WriteString("profiles:\n")
		for _, p := range c.profiles {
			fmt.Fprintf(&sb, "- %s\n", p)
		}
		return sb.String(), nil
	case simDevRe.MatchString(cmd):
		m := simDevRe.FindStringSubmatch(cmd)
		c, ok := s.get(m[1])
		if !ok {
			return "", errors.New("Error: Instance not found")
		}
		if _, dup := c.devices[m[2]]; dup {
			return "", errors.New("Error: The device already exists")
		}
		d := map[string]string{"type": m[3]}
		for _, kv := range simQuotedRe.FindAllStringSubmatch(m[4], -1) {
			k, v, _ := strings.Cut(kv[1], "=")
			d[k] = v
		}
		c.devices[m[2]] = d
	case strings.HasPrefix(cmd, "incus config set "):
		c, ok := s.get(arg(0))
		if !ok {
			return "", errors.New("Error: Instance not found")
		}
		k, v, _ := strings.Cut(arg(1), "=")
		c.config[k] = v
	case strings.HasPrefix(cmd, "incus delete ") || strings.HasPrefix(cmd, "incus stop "):
		name := arg(0)
		if strings.HasPrefix(cmd, "incus delete ") {
			delete(s.ctrs, name)
		} else if c, ok := s.get(name); ok {
			c.status = "Stopped"
		}
	case simPullRe.MatchString(cmd):
		m := simPullRe.FindStringSubmatch(cmd)
		c, ok := s.get(m[1])
		if !ok {
			return "", errors.New("Error: Instance not found")
		}
		content, has := c.files[m[2]]
		if !has {
			return "", errors.New("Error: Path not found")
		}
		return content, nil
	case simPushRe.MatchString(cmd):
		m := simPushRe.FindStringSubmatch(cmd)
		c, ok := s.get(m[2])
		if !ok {
			return "", errors.New("Error: Instance not found")
		}
		raw, _ := base64.StdEncoding.DecodeString(m[1])
		c.files[m[3]] = string(raw)
	case simHookRe.MatchString(cmd):
		m := simHookRe.FindStringSubmatch(cmd)
		raw, _ := base64.StdEncoding.DecodeString(m[1])
		if m[3] != "" {
			s.inits = append(s.inits, string(raw))
		} else {
			s.hooks = append(s.hooks, string(raw))
		}
	case simExecRe.MatchString(cmd):
		m := simExecRe.FindStringSubmatch(cmd)
		c, ok := s.get(m[1])
		if !ok {
			return "", errors.New("Error: Instance not found")
		}
		switch {
		case strings.HasPrefix(m[2], "systemctl restart"):
			c.restarts++
		case strings.HasPrefix(m[2], "caddy reload"):
			c.reloads++
		case strings.HasPrefix(m[2], "rm -f '/etc/caddy"):
			delete(c.files, strings.Trim(strings.TrimPrefix(m[2], "rm -f "), "'"))
		}
	case strings.HasPrefix(cmd, "curl -s"):
		return "200", nil
	case strings.HasPrefix(cmd, "incus remote list"), strings.HasPrefix(cmd, "incus storage list"),
		strings.HasPrefix(cmd, "incus profile device show"), strings.HasPrefix(cmd, "incus network show"),
		strings.HasPrefix(cmd, "incus profile show"), strings.HasPrefix(cmd, `[ "$(incus network get`):
		// host bootstrap probes and guarded, conditional commands: the guard makes them no-ops here
	default:
		s.t.Errorf("hostSim: unhandled command: %s", cmd)
	}
	return "", nil
}
func (s *hostSim) RunWithInput(context.Context, string, io.Reader) (string, error) { return "", nil }
func (s *hostSim) WriteFile(context.Context, string, []byte, os.FileMode) error    { return nil }
func (s *hostSim) Close() error                                                    { return nil }

func (s *hostSim) mutations() []string {
	var out []string
	for _, c := range s.cmds {
		if simMutatorRe.MatchString(c) {
			out = append(out, c)
		}
	}
	return out
}
func (s *hostSim) count(prefix string) int {
	n := 0
	for _, c := range s.cmds {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}
func (s *hostSim) reset() { s.cmds = nil; s.hooks = nil; s.inits = nil; s.snaps = nil }

var (
	fpA = strings.Repeat("a", 64)
	fpB = strings.Repeat("b", 64)
)

func newTestDeployer(sim *hostSim) *Deployer {
	d := NewDeployer(sim)
	ok := func(context.Context, string, config.HealthCheckConfig) error { return nil }
	d.healthGate = ok
	d.inst.healthGate = ok
	return d
}

func giteaSvc() *config.ServiceConfig {
	return &config.ServiceConfig{
		Name:        "gitea",
		Image:       "gitea:latest",
		Volumes:     []config.VolumeMount{{Name: "gitea-data", Path: "/var/lib/gitea", Pool: "default", Shifted: true}},
		Limits:      map[string]string{"limits.cpu": "2", "limits.memory": "2GB"},
		Env:         map[string]string{"GITEA_PORT": "3000", "GITEA_HOST": "git.example.com"},
		HealthCheck: config.HealthCheckConfig{Path: "/", Port: 3000},
		Routing:     &config.RoutingConfig{Domain: "git.example.com", UpstreamPort: 3000},
		Hooks:       config.ServiceHooks{PreDeploy: "echo pre", ContainerInit: "echo init", PostDeploy: "echo post"},
	}
}

func TestApplyTwiceChangesNothingTheSecondTime(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["gitea:latest"] = fpA
	d := newTestDeployer(sim)
	ctx := context.Background()
	svc := giteaSvc()

	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	c := sim.ctrs["gitea"]
	if c == nil || c.status != "Running" || c.config["volatile.base_image"] != fpA || c.config["limits.cpu"] != "2" {
		t.Fatalf("service not launched as declared: %+v", c)
	}
	if !strings.Contains(c.files["/etc/default/gitea"], "GITEA_HOST=git.example.com\nGITEA_PORT=3000\n") {
		t.Fatalf("env file: %q", c.files["/etc/default/gitea"])
	}
	if !strings.Contains(sim.ctrs["edge"].files["/etc/caddy/sites/gitea.caddy"], "reverse_proxy "+c.ip+":3000") {
		t.Fatalf("route not published: %v", sim.ctrs["edge"].files)
	}
	if len(sim.hooks) != 2 || len(sim.inits) != 1 {
		t.Fatalf("a fresh deploy runs pre, init and post once each: %v %v", sim.hooks, sim.inits)
	}

	for run := 2; run <= 3; run++ {
		sim.reset()
		if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if m := sim.mutations(); len(m) != 0 {
			t.Fatalf("run %d must change nothing, but issued:\n%s", run, strings.Join(m, "\n"))
		}
	}
	if sim.ctrs["edge"].reloads != 1 {
		t.Errorf("Caddy must have been reloaded once in total, got %d", sim.ctrs["edge"].reloads)
	}
}

func TestApplyCorrectsDriftInPlaceWithoutReplacing(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["gitea:latest"] = fpA
	d := newTestDeployer(sim)
	ctx := context.Background()
	svc := giteaSvc()
	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	restartsBefore := sim.ctrs["gitea"].restarts
	sim.reset()

	svc.Limits["limits.cpu"] = "4"
	svc.Env["GITEA_PORT"] = "3001"
	svc.Env["NEW_KEY"] = "n"
	svc.Volumes = append(svc.Volumes, config.VolumeMount{Name: "gitea-repos", Path: "/srv/repos", Pool: "default", Shifted: true})
	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	c := sim.ctrs["gitea"]
	if sim.count("incus launch") != 0 || sim.count("incus delete") != 0 || len(sim.snaps) != 0 {
		t.Fatalf("drift must be fixed without replacing the container: %v", sim.mutations())
	}
	if c.config["limits.cpu"] != "4" || c.restarts != restartsBefore+1 || c.devices["srv-repos"]["source"] != "gitea-repos" {
		t.Fatalf("drift not corrected: cpu=%s restarts=%d devices=%v", c.config["limits.cpu"], c.restarts, c.devices)
	}
	if !strings.Contains(c.files["/etc/default/gitea"], "GITEA_PORT=3001") || !strings.Contains(c.files["/etc/default/gitea"], "NEW_KEY=n") {
		t.Fatalf("env not converged: %q", c.files["/etc/default/gitea"])
	}
	if len(sim.hooks) != 0 || len(sim.inits) != 0 {
		t.Fatalf("hooks are deploy-time; in-place drift must not run them: %v %v", sim.hooks, sim.inits)
	}

	sim.reset()
	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if m := sim.mutations(); len(m) != 0 {
		t.Fatalf("after converging, the next run must change nothing:\n%s", strings.Join(m, "\n"))
	}
}

func TestApplyPreservesEnvKeysItDoesNotDeclare(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["gitea:latest"] = fpA
	d := newTestDeployer(sim)
	ctx := context.Background()
	svc := giteaSvc()
	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// Another system (e.g. the fleet manager) pushes a runtime secret into the env file.
	c := sim.ctrs["gitea"]
	c.files["/etc/default/gitea"] += "CONTROL_TOKEN=runtime-secret\n"

	sim.reset()
	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(sim.mutations()) != 0 {
		t.Fatalf("an undeclared key must not cause a rewrite: %v", sim.mutations())
	}
	svc.Env["GITEA_PORT"] = "9999"
	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	env := c.files["/etc/default/gitea"]
	if !strings.Contains(env, "CONTROL_TOKEN=runtime-secret") || !strings.Contains(env, "GITEA_PORT=9999") {
		t.Fatalf("declared keys converge and runtime keys survive: %q", env)
	}
}

func TestApplyReplacesThroughTheSafeUpdatePathWhenTheImageMoves(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["gitea:latest"] = fpA
	d := newTestDeployer(sim)
	ctx := context.Background()
	svc := giteaSvc()
	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	sim.ctrs["gitea"].files["/etc/default/gitea"] += "CONTROL_TOKEN=runtime-secret\n"
	sim.reset()

	sim.aliases["gitea:latest"] = fpB // the alias now points at a new image
	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	c := sim.ctrs["gitea"]
	if c.config["volatile.base_image"] != fpB {
		t.Fatalf("not replaced with the new image: %v", c.config)
	}
	if len(sim.snaps) != 1 || !strings.HasPrefix(sim.snaps[0], "gitea-data@pre-update-") {
		t.Fatalf("the data volume must be snapshotted before the replace: %v", sim.snaps)
	}
	if c.devices["var-lib-gitea"]["source"] != "gitea-data" || c.config["limits.cpu"] != "2" {
		t.Fatalf("volume and limits must be carried over: %v %v", c.devices, c.config)
	}
	if !strings.Contains(c.files["/etc/default/gitea"], "CONTROL_TOKEN=runtime-secret") {
		t.Fatalf("the live environment must survive the replace: %q", c.files["/etc/default/gitea"])
	}
	if len(sim.hooks) != 2 || len(sim.inits) != 1 {
		t.Fatalf("a real redeploy runs pre, init (before the service starts) and post: %v %v", sim.hooks, sim.inits)
	}

	sim.reset()
	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if m := sim.mutations(); len(m) != 0 {
		t.Fatalf("the run after the replace must change nothing:\n%s", strings.Join(m, "\n"))
	}
}

func TestApplyOCIImagesAreComparedByTheirRecordedReference(t *testing.T) {
	sim := newHostSim(t)
	d := newTestDeployer(sim)
	ctx := context.Background()
	svc := &config.ServiceConfig{Name: "db", Image: "postgres:16", Limits: map[string]string{"limits.cpu": "1"}}

	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if sim.ctrs["db"].config[incus.ImageKey] != "docker:postgres:16" {
		t.Fatalf("the deployed reference must be recorded: %v", sim.ctrs["db"].config)
	}
	sim.reset()
	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if m := sim.mutations(); len(m) != 0 {
		t.Fatalf("the same OCI reference must not be redeployed:\n%s", strings.Join(m, "\n"))
	}

	svc.Image = "postgres:17"
	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if sim.ctrs["db"].config[incus.ImageKey] != "docker:postgres:17" || sim.count("incus launch") != 1 {
		t.Fatalf("a changed reference must replace: %v", sim.ctrs["db"].config)
	}
}

func TestApplyAdoptsAnExistingContainerWithoutABounce(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["gitea:latest"] = fpA
	d := newTestDeployer(sim)
	ctx := context.Background()
	// An instance created before references were recorded (e.g. by a deploy script).
	sim.ctrs["db"] = &simCtr{status: "Running", config: map[string]string{"volatile.base_image": strings.Repeat("c", 64), "limits.cpu": "1"},
		devices: map[string]map[string]string{}, profiles: []string{"default", "base", "service"}, files: map[string]string{}, ip: "10.0.100.99"}

	svc := &config.ServiceConfig{Name: "db", Image: "postgres:16", Limits: map[string]string{"limits.cpu": "1"}}
	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if sim.count("incus launch") != 0 || sim.count("incus delete") != 0 {
		t.Fatalf("adopting what already runs must not restart it: %v", sim.mutations())
	}
	if sim.ctrs["db"].config[incus.ImageKey] != "docker:postgres:16" {
		t.Fatalf("the reference must be recorded on adoption")
	}
	sim.reset()
	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil || len(sim.mutations()) != 0 {
		t.Fatalf("second run must be a no-op: %v %v", err, sim.mutations())
	}
}

func TestApplyResumesAHalfFinishedDeploy(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["gitea:latest"] = fpA
	d := newTestDeployer(sim)
	ctx := context.Background()
	svc := giteaSvc()
	// A previous run launched the container and then died: no volume attached, no env, no route.
	sim.ctrs["gitea"] = &simCtr{status: "Running", config: map[string]string{"volatile.base_image": fpA, "limits.cpu": "2", "limits.memory": "2GB"},
		devices: map[string]map[string]string{}, profiles: []string{"default", "base", "service"}, files: map[string]string{}, ip: "10.0.100.77"}
	sim.ctrs["gitea"].config[incus.ImageKey] = fpA

	if err := d.DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	c := sim.ctrs["gitea"]
	if sim.count("incus launch") != 0 {
		t.Fatalf("an existing container must be completed, not relaunched")
	}
	if c.devices["var-lib-gitea"]["source"] != "gitea-data" || c.files["/etc/default/gitea"] == "" ||
		!strings.Contains(sim.ctrs["edge"].files["/etc/caddy/sites/gitea.caddy"], "10.0.100.77:3000") {
		t.Fatalf("the missing pieces must be filled in: %v %v", c.devices, sim.ctrs["edge"].files)
	}
}

// ---- Launch ----

func platformTemplate() *config.TemplateConfig {
	return &config.TemplateConfig{
		Name:           "platform",
		Image:          "platform:latest",
		Service:        "platform",
		Volumes:        []config.VolumeMount{{Name: "{slug}-data", Path: "/app/.data", Pool: "default", Shifted: true}},
		DefaultLimits:  map[string]string{"limits.cpu": "2"},
		EnvTemplate:    map[string]string{"TENANT": "{slug}"},
		HealthCheck:    config.HealthCheckConfig{Path: "/health", Port: 8787},
		RoutingPattern: "{slug}.example.com",
	}
}

func TestLaunchCanBeRetriedAfterAFailureAndRunTwice(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["platform:latest"] = fpA
	m := NewInstanceManager(sim)
	ctx := context.Background()
	p := LaunchParams{Template: platformTemplate(), Name: "rest-acme", Slug: "acme"}

	m.healthGate = func(context.Context, string, config.HealthCheckConfig) error { return errors.New("not ready yet") }
	if _, err := m.Launch(ctx, p); err == nil || !strings.Contains(err.Error(), "health gate") {
		t.Fatalf("expected the health gate failure, got %v", err)
	}
	if _, exists := sim.ctrs["rest-acme"]; !exists {
		t.Fatal("the container of the failed launch stays, holding its data")
	}

	// The same command again completes the launch instead of failing on "already exists".
	m.healthGate = func(context.Context, string, config.HealthCheckConfig) error { return nil }
	sim.reset()
	ip, err := m.Launch(ctx, p)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if sim.count("incus launch") != 0 {
		t.Fatalf("a retry resumes, it does not launch again")
	}
	if !strings.Contains(sim.ctrs["edge"].files["/etc/caddy/sites/rest-acme.caddy"], "reverse_proxy "+ip+":8787") {
		t.Fatalf("route missing after the retry: %v", sim.ctrs["edge"].files)
	}

	sim.reset()
	if _, err := m.Launch(ctx, p); err != nil {
		t.Fatal(err)
	}
	if mut := sim.mutations(); len(mut) != 0 {
		t.Fatalf("launching a fully launched instance must change nothing:\n%s", strings.Join(mut, "\n"))
	}
}

func TestLaunchNeverAdoptsAnUnrelatedInstanceOfTheSameName(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["platform:latest"] = fpA
	sim.ctrs["rest-acme"] = &simCtr{status: "Running", config: map[string]string{"volatile.base_image": fpA}, devices: map[string]map[string]string{},
		profiles: []string{"default"}, files: map[string]string{}, ip: "10.0.100.88"}
	m := NewInstanceManager(sim)
	m.healthGate = func(context.Context, string, config.HealthCheckConfig) error { return nil }
	_, err := m.Launch(context.Background(), LaunchParams{Template: platformTemplate(), Name: "rest-acme", Slug: "acme"})
	if err == nil || !strings.Contains(err.Error(), "refusing to adopt") {
		t.Fatalf("got %v", err)
	}
	if len(sim.mutations()) != 0 {
		t.Fatalf("an unrelated instance must not be touched: %v", sim.mutations())
	}
}

func TestDestroyIsRepeatable(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["platform:latest"] = fpA
	m := NewInstanceManager(sim)
	m.healthGate = func(context.Context, string, config.HealthCheckConfig) error { return nil }
	ctx := context.Background()
	if _, err := m.Launch(ctx, LaunchParams{Template: platformTemplate(), Name: "rest-acme", Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := m.Destroy(ctx, "rest-acme", true); err != nil {
			t.Fatalf("destroy #%d: %v", i+1, err)
		}
	}
	if _, still := sim.ctrs["rest-acme"]; still {
		t.Fatal("instance should be gone")
	}
	if sim.volumes["rest-acme-data"] {
		t.Fatal("the purged volume should be gone")
	}
	if _, has := sim.ctrs["edge"].files["/etc/caddy/sites/rest-acme.caddy"]; has {
		t.Fatal("the route should be gone")
	}
}
