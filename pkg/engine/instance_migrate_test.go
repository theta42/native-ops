package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

// daemonSim is migSim as the source host's daemon sees it: the instance is on this host (remote
// "local", which the daemon's own reads name with a bare instance name), it carries the daemon's
// bookkeeping config, and the host has an edge with the instance's route.
type daemonSim struct {
	*migSim
	cfg   map[string]string // extra config of local rest-x (template, spec, migrated mark)
	sites map[string]string // the edge's site files, by path
}

var _ remote.Executor = (*daemonSim)(nil)

const siteX = "/etc/caddy/sites/rest-x.caddy"

func newDaemonSim(t *testing.T) *daemonSim {
	t.Helper()
	sim := newSim()
	sim.status["local"] = sim.status["src"]
	sim.volumes["local"] = sim.volumes["src"]
	delete(sim.status, "src")
	delete(sim.volumes, "src")
	spec := InstanceSpec{
		Template: "app", Image: "app:latest", Volumes: []VolumeSpec{{Name: "rest-x-data", Path: "/app/.data"}},
		Health: &HealthSpec{Path: "/health", Port: 8787}, Domain: "rest-x.example.com",
	}
	d := &daemonSim{migSim: sim, cfg: map[string]string{incus.TemplateKey: "app", incus.ImageKey: "app:latest"},
		sites: map[string]string{siteX: "rest-x.example.com {\n\treverse_proxy 10.0.0.5:8787\n}\n"}}
	d.setSpec(t, spec)
	return d
}

func (d *daemonSim) setSpec(t *testing.T, spec InstanceSpec) {
	t.Helper()
	in := &Instances{exec: d}
	if err := in.recordSpec(context.Background(), "rest-x", spec, nil); err != nil {
		t.Fatal(err)
	}
	d.cmds = nil
}

func (d *daemonSim) Run(ctx context.Context, cmd string) (string, error) {
	q := quotedArgs(cmd)
	switch {
	case strings.HasPrefix(cmd, "incus list ") && !strings.Contains(q[0], ":"):
		// A read of this host by instance name: the local remote, filtered to that instance.
		d.cmds = append(d.cmds, cmd)
		st, ok := d.status["local"][q[0]]
		if !ok {
			return "[]", nil
		}
		return fmt.Sprintf(`[{"name":%q,"status":%q}]`, q[0], st), nil
	case strings.HasPrefix(cmd, "incus config show ") && !strings.Contains(q[0], ":"),
		strings.HasPrefix(cmd, "incus config show 'local:"):
		name := strings.TrimPrefix(q[0], "local:")
		out, err := d.migSim.Run(ctx, "incus config show "+incus.ShQuote("local:"+name))
		if err != nil || name != "rest-x" {
			return out, err
		}
		keys := make([]string, 0, len(d.cfg))
		for k := range d.cfg {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var extra strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&extra, "  %s: %q\n", k, d.cfg[k])
		}
		return strings.Replace(out, "config:\n", "config:\n"+extra.String(), 1), nil
	case strings.HasPrefix(cmd, "incus config set "):
		d.cmds = append(d.cmds, cmd)
		if d.hook != nil {
			if err := d.hook(cmd); err != nil {
				return "", err
			}
		}
		k, v, _ := strings.Cut(q[1], "=")
		d.cfg[k] = v
		return "", nil
	case strings.HasPrefix(cmd, "incus file pull 'edge"):
		d.cmds = append(d.cmds, cmd)
		c, ok := d.sites[strings.TrimPrefix(q[0], "edge")]
		if !ok {
			return "", errors.New("Error: not found")
		}
		return c, nil
	case strings.HasPrefix(cmd, "incus exec 'edge' -- rm -f "):
		d.cmds = append(d.cmds, cmd)
		if d.hook != nil {
			if err := d.hook(cmd); err != nil {
				return "", err
			}
		}
		delete(d.sites, q[1])
		return "", nil
	case strings.HasPrefix(cmd, "incus exec 'edge'"):
		d.cmds = append(d.cmds, cmd)
		return "", nil
	}
	return d.migSim.Run(ctx, cmd)
}
func (d *daemonSim) RunWithInput(ctx context.Context, cmd string, _ io.Reader) (string, error) {
	return d.Run(ctx, cmd)
}
func (d *daemonSim) WriteFile(context.Context, string, []byte, os.FileMode) error { return nil }
func (d *daemonSim) Close() error                                                 { return nil }

func daemonInstances(d *daemonSim, probeErr error) (*Instances, *[]probeCall) {
	var calls []probeCall
	in := &Instances{exec: d, migrationProbe: func(_ context.Context, r, n string, hc config.HealthCheckConfig) error {
		calls = append(calls, probeCall{r, n})
		if hc.Path != "/health" || hc.Port != 8787 {
			return fmt.Errorf("the recorded health check was not used: %+v", hc)
		}
		return probeErr
	}}
	return in, &calls
}

func TestTheDaemonsMigrateUsesTheEngineAndThenDropsTheRouteAndMarksTheCopy(t *testing.T) {
	d := newDaemonSim(t)
	in, probes := daemonInstances(d, nil)
	var log []string
	logf := func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) }

	summary, err := in.Migrate(context.Background(), "rest-x", MigrateRequest{TargetRemote: "dst"}, logf)
	if err != nil {
		t.Fatal(err)
	}
	if d.status["local"]["rest-x"] != "Stopped" || d.status["dst"]["rest-x"] != "Running" || !d.volumes["dst"]["rest-x-data"] {
		t.Fatalf("the instance and its volume must be on the target, running: %v %v", d.status, d.volumes)
	}
	if len(*probes) != 1 || (*probes)[0].remote != "dst" {
		t.Fatalf("the target must be health-checked with the recorded check: %v", *probes)
	}
	if len(d.snaps) != 1 || !strings.HasPrefix(d.snaps[0], "local:default/rest-x-data@pre-migrate-") {
		t.Fatalf("the volume is snapshotted here first: %v", d.snaps)
	}
	if d.count("incus storage volume copy") != 2 || d.count("incus copy ") != 2 {
		t.Fatalf("a warm copy, then a final one: %v", d.cmds)
	}
	if _, still := d.sites[siteX]; still {
		t.Fatal("the route here must be dropped once the instance runs on the target")
	}
	if d.cfg[incus.MigratedToKey] != "dst" || d.cfg[incus.SpecKey] == "" {
		t.Fatalf("the copy here is marked migrated and keeps its spec: %v", d.cfg)
	}
	if d.count("incus delete") != 0 {
		t.Fatal("migrate never deletes the source; finalize does")
	}
	if !strings.Contains(summary, "snapshots were not moved") || !strings.Contains(summary, "dst") {
		t.Fatalf("the result must say the snapshots were not moved: %q", summary)
	}
	if !strings.Contains(strings.Join(log, "\n"), "Final sync to dst") {
		t.Fatalf("the engine's progress goes to the job's log: %v", log)
	}

	// The record now says migrated; another migrate is refused before anything runs.
	rec, err := in.Record(context.Background(), "rest-x")
	if err != nil || rec.MigratedTo != "dst" || rec.Spec == nil || rec.MigrationRefusal() == "" {
		t.Fatalf("record after migrate: %+v %v", rec, err)
	}
	d.cmds = nil
	if _, err := in.Migrate(context.Background(), "rest-x", MigrateRequest{TargetRemote: "dst"}, nil); err == nil || !strings.Contains(err.Error(), "already migrated") {
		t.Fatalf("a second migrate: %v", err)
	}
	if d.mutating() != 0 {
		t.Fatalf("a refused migrate changes nothing: %v", d.cmds)
	}
}

func TestAFailedMigrationKeepsTheRouteAndTheRecordAsTheyWere(t *testing.T) {
	for name, setup := range map[string]func(d *daemonSim) error{
		"the target is unhealthy": func(*daemonSim) error { return errors.New("503") },
		"the final sync fails": func(d *daemonSim) error {
			n := 0
			d.hook = func(c string) error {
				if strings.HasPrefix(c, "incus copy ") {
					if n++; n == 2 {
						return errors.New("link dropped")
					}
				}
				return nil
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDaemonSim(t)
			probeErr := setup(d)
			in, _ := daemonInstances(d, probeErr)
			_, err := in.Migrate(context.Background(), "rest-x", MigrateRequest{TargetRemote: "dst"}, nil)
			if err == nil || !strings.Contains(err.Error(), "restarted and nothing was lost") {
				t.Fatalf("got %v", err)
			}
			if d.status["local"]["rest-x"] != "Running" {
				t.Fatalf("the source is rolled back: %v", d.status)
			}
			if _, ok := d.sites[siteX]; !ok || d.count("incus exec 'edge' -- rm -f") != 0 {
				t.Fatal("a failed migration must leave the route here alone")
			}
			if d.cfg[incus.MigratedToKey] != "" {
				t.Fatalf("a failed migration must not mark the instance: %v", d.cfg)
			}
		})
	}
}

func TestTheDaemonRefusesToMigrateWhatItCannotMoveWithoutChangingAnything(t *testing.T) {
	cases := map[string]struct {
		setup func(d *daemonSim)
		req   MigrateRequest
		want  string
	}{
		"a host path": {func(d *daemonSim) {
			d.configYAML = "config:\n  volatile.base_image: " + oldFP + "\ndevices:\n  logs:\n    path: /var/log/app\n    source: /srv/logs\n    type: disk\nprofiles:\n- default\n"
		}, MigrateRequest{TargetRemote: "dst"}, "host path"},
		"no recorded spec":  {func(d *daemonSim) { delete(d.cfg, incus.SpecKey) }, MigrateRequest{TargetRemote: "dst"}, "no spec"},
		"not a tenant":      {func(d *daemonSim) { delete(d.cfg, incus.TemplateKey) }, MigrateRequest{TargetRemote: "dst"}, "not a tenant"},
		"no such instance":  {func(d *daemonSim) { delete(d.status["local"], "rest-x") }, MigrateRequest{TargetRemote: "dst"}, "no such instance"},
		"to this host":      {nil, MigrateRequest{TargetRemote: "local"}, "is this host"},
		"a bad remote name": {nil, MigrateRequest{TargetRemote: "dst; rm -rf /"}, "target_remote"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := newDaemonSim(t)
			if tc.setup != nil {
				tc.setup(d)
			}
			in, _ := daemonInstances(d, nil)
			_, err := in.Migrate(context.Background(), "rest-x", tc.req, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if d.mutating() != 0 || d.count("incus config set") != 0 {
				t.Fatalf("nothing may change: %v", d.cmds)
			}
		})
	}
}

func TestFinalizeMigrationDeletesOnlyAMigratedCopyAndCanBeRepeated(t *testing.T) {
	ctx := context.Background()
	d := newDaemonSim(t)
	in, _ := daemonInstances(d, nil)

	if err := in.FinalizeMigration(ctx, "rest-x", FinalizeMigrationRequest{}, nil); err == nil || !strings.Contains(err.Error(), "not migrated") {
		t.Fatalf("an instance that was not migrated: %v", err)
	}
	if d.count("incus delete") != 0 {
		t.Fatal("nothing may be deleted")
	}
	if _, err := in.Migrate(ctx, "rest-x", MigrateRequest{TargetRemote: "dst"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := in.FinalizeMigration(ctx, "rest-x", FinalizeMigrationRequest{TargetRemote: "other"}, nil); err == nil {
		t.Fatal("a finalize naming another target must be refused")
	}
	if err := in.FinalizeMigration(ctx, "rest-x", FinalizeMigrationRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, still := d.status["local"]["rest-x"]; still || d.status["dst"]["rest-x"] != "Running" {
		t.Fatalf("the copy here is deleted, the target runs: %v", d.status)
	}
	if !d.volumes["local"]["rest-x-data"] {
		t.Fatal("the volume here is kept by default")
	}
	// Repeated (a CI retry): the copy here is gone, so the target must be named.
	if err := in.FinalizeMigration(ctx, "rest-x", FinalizeMigrationRequest{}, nil); err == nil {
		t.Fatal("with the copy gone, a finalize must name the target")
	}
	if err := in.FinalizeMigration(ctx, "rest-x", FinalizeMigrationRequest{TargetRemote: "dst"}, nil); err != nil {
		t.Fatalf("a repeated finalize: %v", err)
	}
}

// ---- adopt, on the target ----

// arrived is a host to which demo-multi was just migrated: the instance is there, running, with its
// volume, environment file and recorded spec (all of which a copy carries), and no route on this edge.
func arrived(t *testing.T) (*hostSim, *Instances) {
	t.Helper()
	sim := newHostSim(t)
	sim.aliases["opsavor-platform:latest"] = fpA
	in := newTestInstances(sim)
	if _, err := in.Launch(context.Background(), "demo-multi", platformSpec(), nil); err != nil {
		t.Fatal(err)
	}
	delete(sim.ctrs["edge"].files, "/etc/caddy/sites/demo-multi.caddy")
	sim.ctrs["demo-multi"].config[incus.MigratedToKey] = "old-host" // as if it had once moved away from here
	sim.reset()
	return sim, in
}

func exported(t *testing.T, in *Instances, name string) InstanceSpec {
	t.Helper()
	rec, err := in.Record(context.Background(), name)
	if err != nil || rec.Spec == nil {
		t.Fatalf("record: %+v %v", rec, err)
	}
	return *rec.Spec
}

func TestTheRecordedSpecNeverHoldsTheEnvironmentAndFollowsLaterChanges(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["opsavor-platform:latest"] = fpA
	in := newTestInstances(sim)
	ctx := context.Background()
	if _, err := in.Launch(ctx, "demo-multi", platformSpec(), nil); err != nil {
		t.Fatal(err)
	}
	raw := sim.ctrs["demo-multi"].config[incus.SpecKey]
	if raw == "" || strings.Contains(raw, "s3cr3t-value") || strings.Contains(raw, "OPSAVOR_CONTROL_TOKEN") || strings.Contains(raw, `"env"`) {
		t.Fatalf("the spec is recorded without its environment: %s", raw)
	}
	if err := in.Resize(ctx, "demo-multi", ResizeRequest{Limits: map[string]string{"limits.cpu": "4"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := in.SetLabels(ctx, "demo-multi", map[string]string{"environment": "production"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	spec := exported(t, in, "demo-multi")
	if spec.Env != nil || spec.Limits["limits.cpu"] != "4" || spec.Labels["environment"] != "production" || spec.Domain != "demo-multi.example.com" ||
		len(spec.RouteDirectives) != 1 || len(spec.Volumes) != 1 || spec.Health == nil || spec.Image != "opsavor-platform:latest" {
		t.Fatalf("the exported spec: %+v", spec)
	}
	if err := spec.Validate("demo-multi", testPolicy); err != nil {
		t.Fatalf("an exported spec must be one adopt accepts: %v", err)
	}
}

func TestAdoptPublishesTheRouteRecordsTheSpecAndIsIdempotent(t *testing.T) {
	sim, in := arrived(t)
	ctx := context.Background()
	spec := exported(t, in, "demo-multi")
	sim.reset()

	if err := in.Adopt(ctx, "demo-multi", spec, nil); err != nil {
		t.Fatal(err)
	}
	c := sim.ctrs["demo-multi"]
	site := sim.ctrs["edge"].files["/etc/caddy/sites/demo-multi.caddy"]
	if !strings.HasPrefix(site, "demo-multi.example.com {") || !strings.Contains(site, "reverse_proxy "+c.ip+":8787") || !strings.Contains(site, "import strip-forged-identity") {
		t.Fatalf("the route, rendered as a launch renders it:\n%s", site)
	}
	if c.config[incus.MigratedToKey] != "" {
		t.Fatal("an adopted instance is this host's own: no migrated mark")
	}
	for _, m := range sim.mutations() {
		if strings.HasPrefix(m, "incus launch") || strings.HasPrefix(m, "incus delete") || strings.HasPrefix(m, "incus start") || strings.HasPrefix(m, "incus stop") || strings.Contains(m, "systemctl restart") {
			t.Fatalf("adopt never creates, starts or replaces anything: %s", m)
		}
	}

	sim.reset()
	if err := in.Adopt(ctx, "demo-multi", spec, nil); err != nil {
		t.Fatal(err)
	}
	if m := sim.mutations(); len(m) != 0 {
		t.Fatalf("a repeated adopt must change nothing:\n%s", strings.Join(m, "\n"))
	}
}

func TestAdoptRefusesWhatIsNotHereAsAMigrationLeavesIt(t *testing.T) {
	cases := map[string]struct {
		name   string
		change func(sim *hostSim, spec *InstanceSpec)
		want   string
	}{
		"no such instance":   {"nope", nil, "migrate it here first"},
		"a stopped instance": {"demo-multi", func(sim *hostSim, _ *InstanceSpec) { sim.ctrs["demo-multi"].status = "Stopped" }, "not Running"},
		"another template":   {"demo-multi", func(_ *hostSim, s *InstanceSpec) { s.Template = "other" }, "template"},
		"a missing volume":   {"demo-multi", func(sim *hostSim, _ *InstanceSpec) { delete(sim.volumes, "demo-multi-data") }, "not attached"},
		"an unattached one": {"demo-multi", func(_ *hostSim, s *InstanceSpec) {
			s.Volumes = append(s.Volumes, VolumeSpec{Name: "demo-multi-extra", Path: "/x"})
		}, "not attached"},
		"an environment":      {"demo-multi", func(_ *hostSim, s *InstanceSpec) { s.Env = map[string]string{"K": "v"} }, "no env"},
		"not a tenant at all": {"edge", func(_ *hostSim, s *InstanceSpec) {}, "not a tenant"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sim, in := arrived(t)
			spec := exported(t, in, "demo-multi")
			if tc.change != nil {
				tc.change(sim, &spec)
			}
			sim.reset()
			err := in.Adopt(context.Background(), tc.name, spec, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if m := sim.mutations(); len(m) != 0 {
				t.Fatalf("a refused adopt changes nothing:\n%s", strings.Join(m, "\n"))
			}
		})
	}
}

func TestALaunchIsRefusedOnAMigratedCopy(t *testing.T) {
	sim, in := arrived(t) // carries a migrated mark: this host has the stopped copy of something now elsewhere
	if _, err := in.Launch(context.Background(), "demo-multi", platformSpec(), nil); err == nil || !strings.Contains(err.Error(), "migrated to old-host") {
		t.Fatalf("got %v", err)
	}
	if m := sim.mutations(); len(m) != 0 {
		t.Fatalf("nothing may change:\n%s", strings.Join(m, "\n"))
	}
}
