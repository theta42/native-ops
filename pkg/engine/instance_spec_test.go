package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
)

func platformSpec() InstanceSpec {
	return InstanceSpec{
		Template: "platform", Image: "opsavor-platform:latest", Profiles: []string{"base", "service"},
		Limits:  map[string]string{"limits.cpu": "1", "limits.memory": "1GB"},
		Volumes: []VolumeSpec{{Name: "demo-multi-data", Path: "/app/.data"}},
		Service: "platform",
		Env:     map[string]string{"PORT": "8787", "OPSAVOR_SEED": "multi", "OPSAVOR_CONTROL_TOKEN": "s3cr3t-value"},
		Health:  &HealthSpec{Path: "/health", Port: 8787},
		Domain:  "demo-multi.example.com", RouteDirectives: []string{"import strip-forged-identity"},
	}
}

var testPolicy = InstancePolicy{Profiles: []string{"base", "service"}, RouteImports: []string{"strip-forged-identity"}}

func TestASpecCanOnlyAskForWhatATenantNeeds(t *testing.T) {
	if err := (&InstanceSpec{}).Validate("x", testPolicy); err == nil {
		t.Fatal("an empty spec is not valid")
	}
	good := platformSpec()
	if err := good.Validate("demo-multi", testPolicy); err != nil {
		t.Fatalf("the platform spec must be valid: %v", err)
	}
	bad := map[string]func(s *InstanceSpec){
		"a profile that is not allowed":        func(s *InstanceSpec) { s.Profiles = []string{"base", "privileged"} },
		"an uppercase template":                func(s *InstanceSpec) { s.Template = "Platform" },
		"a template with a space":              func(s *InstanceSpec) { s.Template = "a b" },
		"an image with a shell in it":          func(s *InstanceSpec) { s.Image = "img; rm -rf /" },
		"an image with a quote":                func(s *InstanceSpec) { s.Image = "img'x" },
		"an empty image":                       func(s *InstanceSpec) { s.Image = "" },
		"an unknown limit":                     func(s *InstanceSpec) { s.Limits = map[string]string{"limits.processes": "1"} },
		"a privileged flag as a limit":         func(s *InstanceSpec) { s.Limits = map[string]string{"security.privileged": "true"} },
		"a cpu that is not a number":           func(s *InstanceSpec) { s.Limits = map[string]string{"limits.cpu": "all"} },
		"a memory without a unit":              func(s *InstanceSpec) { s.Limits = map[string]string{"limits.memory": "1000000"} },
		"another instance's volume":            func(s *InstanceSpec) { s.Volumes[0].Name = "gitea-data" },
		"a volume that only shares a prefix":   func(s *InstanceSpec) { s.Volumes[0].Name = "demo-multiplayer-data" },
		"a relative volume path":               func(s *InstanceSpec) { s.Volumes[0].Path = "app/.data" },
		"a volume path that climbs":            func(s *InstanceSpec) { s.Volumes[0].Path = "/app/../etc" },
		"an unclean volume path":               func(s *InstanceSpec) { s.Volumes[0].Path = "/app//.data" },
		"an uppercase volume owner":            func(s *InstanceSpec) { s.Volumes[0].Owner = "Platform" },
		"a volume owner with a shell in it":    func(s *InstanceSpec) { s.Volumes[0].Owner = "x; rm -rf /" },
		"a volume owner starting with a digit": func(s *InstanceSpec) { s.Volumes[0].Owner = "1platform" },
		"too many volumes": func(s *InstanceSpec) {
			for i := 0; i < 5; i++ {
				s.Volumes = append(s.Volumes, VolumeSpec{Name: "demo-multi-v", Path: "/v" + string(rune('a'+i))})
			}
		},
		"a lowercase environment name":            func(s *InstanceSpec) { s.Env = map[string]string{"path": "/x"}; s.Service = "platform" },
		"an environment name with a dash":         func(s *InstanceSpec) { s.Env = map[string]string{"A-B": "x"} },
		"an environment value with a line break":  func(s *InstanceSpec) { s.Env = map[string]string{"A": "x\nEVIL=1"} },
		"an environment value that is huge":       func(s *InstanceSpec) { s.Env = map[string]string{"A": strings.Repeat("x", 5000)} },
		"an environment without a unit":           func(s *InstanceSpec) { s.Service = "" },
		"a unit that is not a name":               func(s *InstanceSpec) { s.Service = "a b" },
		"a health port of zero":                   func(s *InstanceSpec) { s.Health.Port = 0 },
		"a health path that is not a path":        func(s *InstanceSpec) { s.Health.Path = "health" },
		"a health path with a query":              func(s *InstanceSpec) { s.Health.Path = "/h?x=1" },
		"a health timeout of an hour":             func(s *InstanceSpec) { s.Health.Timeout = 3600 },
		"a wildcard domain":                       func(s *InstanceSpec) { s.Domain = "*.example.com" },
		"a domain that is not a host name":        func(s *InstanceSpec) { s.Domain = "a b.example.com" },
		"a bare host name":                        func(s *InstanceSpec) { s.Domain = "localhost" },
		"a domain with no health to point at":     func(s *InstanceSpec) { s.Health = nil },
		"a route directive that is not an import": func(s *InstanceSpec) { s.RouteDirectives = []string{"reverse_proxy evil:80"} },
		"an import that is not allowed":           func(s *InstanceSpec) { s.RouteDirectives = []string{"import admin-only"} },
		"a directive with a brace":                func(s *InstanceSpec) { s.RouteDirectives = []string{"import strip-forged-identity }"} },
		"directives without a domain":             func(s *InstanceSpec) { s.Domain = ""; s.Health = &HealthSpec{Port: 8787} },
	}
	for name, mutate := range bad {
		s := platformSpec()
		s.Volumes = append([]VolumeSpec(nil), s.Volumes...)
		s.Health = &HealthSpec{Path: "/health", Port: 8787}
		mutate(&s)
		if err := s.Validate("demo-multi", testPolicy); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	for _, name := range []string{"", "-x", "a b", "x;y", "../x", strings.Repeat("a", 64)} {
		s := platformSpec()
		if err := s.Validate(name, testPolicy); err == nil {
			t.Errorf("instance name %q must be refused", name)
		}
	}
	// A spec that allows no imports cannot use one.
	s := platformSpec()
	if err := s.Validate("demo-multi", DefaultInstancePolicy); err == nil {
		t.Error("with no imports allowed, an import is refused")
	}
	// A plausible owner is accepted, and reaches the template's volume, unchanged.
	s = platformSpec()
	s.Volumes[0].Owner = "www-data"
	if err := s.Validate("demo-multi", testPolicy); err != nil {
		t.Fatalf("a plausible volume owner must be accepted: %v", err)
	}
	if got := s.TemplateConfig().Volumes[0].Owner; got != "www-data" {
		t.Fatalf("the volume owner must reach the template config, got %q", got)
	}
}

func TestAnUpdateRequestIsValidated(t *testing.T) {
	ok := UpdateRequest{Image: "opsavor-platform:v2", Service: "platform", Health: &HealthSpec{Path: "/health", Port: 8787}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]UpdateRequest{
		"no image":      {Service: "platform"},
		"a shell image": {Image: "x y", Service: "platform"},
		"no unit":       {Image: "img"},
		"a bad port":    {Image: "img", Service: "platform", Health: &HealthSpec{Port: 70000}},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestAResizeRequestIsValidated(t *testing.T) {
	if err := (&ResizeRequest{Limits: map[string]string{"limits.cpu": "2", "limits.memory": "2GB"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]ResizeRequest{
		"nothing at all":     {},
		"an unknown limit":   {Limits: map[string]string{"security.privileged": "true"}},
		"a cpu not a number": {Limits: map[string]string{"limits.cpu": "all"}},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestASuspendRequestIsValidated(t *testing.T) {
	ok := SuspendRequest{Domain: "demo-old.opsavor.app", Reason: "non-payment", RouteDirectives: []string{"import strip-forged-identity"}}
	if err := ok.Validate(testPolicy); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]SuspendRequest{
		"no domain":                 {Reason: "x"},
		"a wildcard domain":         {Domain: "*.opsavor.app", Reason: "x"},
		"no reason":                 {Domain: "demo-old.opsavor.app"},
		"an import not allowed":     {Domain: "demo-old.opsavor.app", Reason: "x", RouteDirectives: []string{"import admin-only"}},
		"a directive not an import": {Domain: "demo-old.opsavor.app", Reason: "x", RouteDirectives: []string{"reverse_proxy evil:80"}},
	} {
		if err := r.Validate(testPolicy); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func newTestInstances(sim *hostSim) *Instances {
	return &Instances{exec: sim, healthGate: func(context.Context, string, config.HealthCheckConfig) error { return nil }}
}

func TestLaunchingATenantThroughTheAdapterIsComplete(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["opsavor-platform:latest"] = fpA
	in := newTestInstances(sim)
	ctx := context.Background()
	spec := platformSpec()

	ip, err := in.Launch(ctx, "demo-multi", spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := sim.ctrs["demo-multi"]
	if c == nil || c.ip != ip || c.config["user.native-ops.template"] != "platform" || c.config["limits.memory"] != "1GB" {
		t.Fatalf("launched as specified: %+v", c)
	}
	if c.devices["app-.data"]["source"] != "demo-multi-data" {
		t.Fatalf("its volume is attached: %v", c.devices)
	}
	env := c.files["/etc/default/platform"]
	if !strings.Contains(env, "OPSAVOR_SEED=multi") || !strings.Contains(env, "OPSAVOR_CONTROL_TOKEN=s3cr3t-value") {
		t.Fatalf("the environment is written where the unit reads it: %q", env)
	}
	if c.restarts != 1 {
		t.Fatalf("the unit is started once, after the environment exists: %d", c.restarts)
	}
	site := sim.ctrs["edge"].files["/etc/caddy/sites/demo-multi.caddy"]
	if !strings.HasPrefix(site, "demo-multi.example.com {") || !strings.Contains(site, "reverse_proxy "+ip+":8787") || !strings.Contains(site, "import strip-forged-identity") {
		t.Fatalf("the route, with the snippet the operator allowed:\n%s", site)
	}

	shifted := false
	for _, m := range sim.mutations() {
		shifted = shifted || strings.Contains(m, "incus storage volume create 'default' 'demo-multi-data' security.shifted=true")
	}
	if !shifted {
		t.Fatalf("the tenant's volume must be created with security.shifted=true:\n%s", strings.Join(sim.mutations(), "\n"))
	}

	// Running it again changes nothing.
	sim.reset()
	if _, err := in.Launch(ctx, "demo-multi", spec, nil); err != nil {
		t.Fatal(err)
	}
	if m := sim.mutations(); len(m) != 0 {
		t.Fatalf("a repeated launch must change nothing:\n%s", strings.Join(m, "\n"))
	}
}

func TestResizeAndSuspendWireThroughToTheHostAndTheEdge(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["opsavor-platform:latest"] = fpA
	in := newTestInstances(sim)
	ctx := context.Background()
	if _, err := in.Launch(ctx, "demo-multi", platformSpec(), nil); err != nil {
		t.Fatal(err)
	}
	sim.reset()
	restartsBefore := sim.ctrs["demo-multi"].restarts

	if err := in.Resize(ctx, "demo-multi", ResizeRequest{Limits: map[string]string{"limits.cpu": "4", "limits.memory": "4GB"}}, nil); err != nil {
		t.Fatal(err)
	}
	if c := sim.ctrs["demo-multi"]; c.config["limits.cpu"] != "4" || c.config["limits.memory"] != "4GB" {
		t.Fatalf("resize did not reach the container: %+v", c.config)
	}
	if c := sim.ctrs["demo-multi"]; c.restarts != restartsBefore {
		t.Fatalf("a resize must never restart the unit, got %d more restarts", c.restarts-restartsBefore)
	}

	if err := in.Suspend(ctx, "demo-multi", SuspendRequest{Domain: "demo-multi.example.com", Reason: "non-payment", RouteDirectives: []string{"import strip-forged-identity"}}, nil); err != nil {
		t.Fatal(err)
	}
	site := sim.ctrs["edge"].files["/etc/caddy/sites/demo-multi.caddy"]
	if strings.Contains(site, "reverse_proxy") || !strings.Contains(site, "503") || !strings.Contains(site, "non-payment") {
		t.Fatalf("expected a 503 responder naming the reason, got:\n%s", site)
	}

	// Asking for the instance again (what fleet-manager's redeploy already does) undoes the suspension.
	if _, err := in.Launch(ctx, "demo-multi", platformSpec(), nil); err != nil {
		t.Fatal(err)
	}
	site = sim.ctrs["edge"].files["/etc/caddy/sites/demo-multi.caddy"]
	if !strings.Contains(site, "reverse_proxy") || strings.Contains(site, "503") {
		t.Fatalf("re-launching must restore the normal route:\n%s", site)
	}
}

func TestLaunchGivesAFreshVolumeToItsOwnerAndRepairsAResumedOneOnlyOnce(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["opsavor-platform:latest"] = fpA
	in := newTestInstances(sim)
	ctx := context.Background()
	spec := platformSpec()
	spec.Volumes[0].Owner = "platform"

	if _, err := in.Launch(ctx, "demo-multi", spec, nil); err != nil {
		t.Fatal(err)
	}
	c := sim.ctrs["demo-multi"]
	if c.owners["/app/.data"] != "platform" || c.chowns != 1 {
		t.Fatalf("a fresh volume must be handed to its owner once: owners=%v chowns=%d", c.owners, c.chowns)
	}

	// Asking again (a resumed instance) finds it already right: no second chown.
	sim.reset()
	if _, err := in.Launch(ctx, "demo-multi", spec, nil); err != nil {
		t.Fatal(err)
	}
	if c.chowns != 1 {
		t.Fatalf("a volume that already belongs to its owner must not be chowned again, got %d", c.chowns)
	}
	if m := sim.mutations(); len(m) != 0 {
		t.Fatalf("nothing to fix, so nothing may run: %v", m)
	}

	// A spec with no owner never asks: a caller that does not use this field sees no new behavior.
	sim2 := newHostSim(t)
	sim2.aliases["opsavor-platform:latest"] = fpA
	in2 := newTestInstances(sim2)
	if _, err := in2.Launch(ctx, "demo-bad", platformSpec(), nil); err != nil {
		t.Fatal(err)
	}
	if got := sim2.ctrs["demo-bad"].chowns; got != 0 {
		t.Fatalf("no owner was asked for, so nothing must be chowned, got %d", got)
	}
}

func TestAnInstanceThatIsNotTheCallersTemplateIsNeverAdopted(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["opsavor-platform:latest"] = fpA
	in := newTestInstances(sim)
	ctx := context.Background()

	// gitea is a static service: it carries no template.
	svc := giteaSvc()
	sim.aliases["gitea:latest"] = fpA
	if err := newTestDeployer(sim).DeployService(ctx, svc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	sim.reset()
	spec := platformSpec()
	spec.Volumes[0].Name = "gitea-data"
	spec.Domain, spec.RouteDirectives = "", nil
	if _, err := in.Launch(ctx, "gitea", spec, nil); err == nil {
		t.Fatal("an instance that was not launched from a template must be refused")
	}
	if m := sim.mutations(); len(m) != 0 {
		t.Fatalf("nothing may change:\n%s", strings.Join(m, "\n"))
	}

	// A tenant of another template is refused too.
	if _, err := in.Launch(ctx, "demo-multi", platformSpec(), nil); err != nil {
		t.Fatal(err)
	}
	other := platformSpec()
	other.Template = "restaurant"
	sim.reset()
	if _, err := in.Launch(ctx, "demo-multi", other, nil); err == nil || len(sim.mutations()) != 0 {
		t.Fatalf("another template must not take an instance over: %v %v", err, sim.mutations())
	}

	tpl, exists, err := in.Template(ctx, "demo-multi")
	if err != nil || !exists || tpl != "platform" {
		t.Fatalf("template of a tenant: %q %v %v", tpl, exists, err)
	}
	if tpl, exists, _ := in.Template(ctx, "gitea"); !exists || tpl != "" {
		t.Fatalf("a static service exists and has no template: %q %v", tpl, exists)
	}
	if _, exists, _ := in.Template(ctx, "nope"); exists {
		t.Fatal("a missing instance does not exist")
	}
}

func TestDestroyingATenantRemovesItsRouteAndOnlyItsOwnVolumesWhenPurging(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["opsavor-platform:latest"] = fpA
	in := newTestInstances(sim)
	ctx := context.Background()
	if _, err := in.Launch(ctx, "demo-multi", platformSpec(), nil); err != nil {
		t.Fatal(err)
	}
	sim.volumes["someone-elses-data"] = true
	if !sim.volumes["demo-multi-data"] {
		t.Fatal("precondition: the tenant's volume exists")
	}

	// Without purge the data stays.
	if err := in.Destroy(ctx, "demo-multi", false, nil); err != nil {
		t.Fatal(err)
	}
	if sim.ctrs["demo-multi"] != nil || sim.ctrs["edge"].files["/etc/caddy/sites/demo-multi.caddy"] != "" {
		t.Fatal("the instance and its route are gone")
	}
	if !sim.volumes["demo-multi-data"] {
		t.Fatal("without purge its data is kept")
	}

	// Purging removes what is named after the instance and nothing else.
	if _, err := in.Launch(ctx, "demo-multi", platformSpec(), nil); err != nil {
		t.Fatal(err)
	}
	if err := in.Destroy(ctx, "demo-multi", true, nil); err != nil {
		t.Fatal(err)
	}
	if sim.volumes["demo-multi-data"] || !sim.volumes["someone-elses-data"] {
		t.Fatalf("purge removes the tenant's volume and no other: %v", sim.volumes)
	}
}

func TestUpdatingATenantGoesThroughTheSafePath(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["opsavor-platform:latest"] = fpA
	sim.aliases["opsavor-platform:v2"] = fpB
	in := newTestInstances(sim)
	ctx := context.Background()
	if _, err := in.Launch(ctx, "demo-multi", platformSpec(), nil); err != nil {
		t.Fatal(err)
	}
	sim.reset()
	if err := in.Update(ctx, "demo-multi", UpdateRequest{Image: "opsavor-platform:v2", Service: "platform", Health: &HealthSpec{Path: "/health", Port: 8787}}, nil); err != nil {
		t.Fatal(err)
	}
	c := sim.ctrs["demo-multi"]
	if c.config["volatile.base_image"] != fpB || len(sim.snaps) != 1 || !strings.Contains(c.files["/etc/default/platform"], "OPSAVOR_SEED=multi") {
		t.Fatalf("replaced with a snapshot first and the environment carried over: %v %v %q", c.config, sim.snaps, c.files["/etc/default/platform"])
	}
}

func TestPurgeNeverTakesAVolumeThatOnlySharesAPrefix(t *testing.T) {
	sim := newHostSim(t)
	sim.aliases["opsavor-platform:latest"] = fpA
	in := newTestInstances(sim)
	ctx := context.Background()
	if _, err := in.Launch(ctx, "demo-multi", platformSpec(), nil); err != nil {
		t.Fatal(err)
	}
	// Somebody attached another tenant's volume, and a shared one, to this instance by hand.
	sim.volumes["demo-multiplayer-data"], sim.volumes["shared"] = true, true
	sim.ctrs["demo-multi"].devices["extra1"] = map[string]string{"type": "disk", "pool": "default", "source": "demo-multiplayer-data", "path": "/x"}
	sim.ctrs["demo-multi"].devices["extra2"] = map[string]string{"type": "disk", "pool": "default", "source": "shared", "path": "/y"}
	if err := in.Destroy(ctx, "demo-multi", true, nil); err != nil {
		t.Fatal(err)
	}
	if sim.volumes["demo-multi-data"] || !sim.volumes["demo-multiplayer-data"] || !sim.volumes["shared"] {
		t.Fatalf("only volumes named <instance>-... are purged: %v", sim.volumes)
	}
}
