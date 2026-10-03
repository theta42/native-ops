package engine

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

// InstanceSpec describes one template instance (a tenant): what to launch and how it is reached. It
// comes from a caller over the API, so it is validated strictly (see Validate) before anything runs;
// it can only describe the kinds of thing a tenant needs, not an arbitrary container.
type InstanceSpec struct {
	// Template is the label recorded on the instance (user.native-ops.template). An instance is only
	// ever resumed or changed by a caller that names the same template.
	Template string            `json:"template"`
	Image    string            `json:"image"`
	Profiles []string          `json:"profiles,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"` // limits.cpu and limits.memory only
	Volumes  []VolumeSpec      `json:"volumes,omitempty"`
	// Service is the systemd unit inside the instance; its environment file is /etc/default/<Service>.
	Service string            `json:"service,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Health  *HealthSpec       `json:"health,omitempty"`
	// Domain publishes a route to Health.Port through the edge.
	Domain          string   `json:"domain,omitempty"`
	RouteDirectives []string `json:"route_directives,omitempty"` // "import <snippet>" only, from InstancePolicy.RouteImports
}

// VolumeSpec is a data volume; its name must start with the instance's name and a dash.
type VolumeSpec struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Shifted *bool  `json:"shifted,omitempty"` // default true
	// Owner, if set, is a user inside the instance (already existing there) that the mount point is
	// handed to once attached. A fresh volume attaches root-owned regardless of what the image's own
	// directory was owned by, so a service that runs as another user needs this to write to it.
	Owner string `json:"owner,omitempty"`
}

// HealthSpec is the check that gates a launch and names the port the route points at.
type HealthSpec struct {
	Path    string `json:"path,omitempty"`
	Port    int    `json:"port"`
	Timeout int    `json:"timeout,omitempty"` // seconds, default 45
}

// InstancePolicy is what the daemon's operator allows any instance spec to ask for.
type InstancePolicy struct {
	Profiles     []string // Incus profiles a spec may use
	RouteImports []string // Caddy snippets a route may import
}

// DefaultInstancePolicy allows the two profiles native-ops itself uses and no route imports.
var DefaultInstancePolicy = InstancePolicy{Profiles: []string{"base", "service"}}

var (
	templateRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	imageRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,200}$`)
	cpuRe      = regexp.MustCompile(`^[0-9]{1,3}$`)
	memRe      = regexp.MustCompile(`^[0-9]{1,6}(MB|GB|MiB|GiB)$`)
	envKeyRe   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	healthRe   = regexp.MustCompile(`^/[A-Za-z0-9/_.-]{0,100}$`)
	domainRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	importRe   = regexp.MustCompile(`^import ([a-z0-9][a-z0-9-]{0,60})$`)
)

// Validate reports why a spec for the instance called name cannot be accepted. Everything a caller can
// set is checked against a short list of shapes, so a spec cannot reach a privileged container, a host
// path, another instance's volume, a shell, or a Caddy directive the operator did not allow.
func (s *InstanceSpec) Validate(name string, pol InstancePolicy) error {
	if !incus.ValidName(name) {
		return fmt.Errorf("%q is not a valid instance name", name)
	}
	if !templateRe.MatchString(s.Template) {
		return fmt.Errorf("template must be a short lowercase label")
	}
	if !imageRe.MatchString(s.Image) {
		return fmt.Errorf("image is not a valid image reference")
	}
	for _, p := range s.Profiles {
		if !contains(pol.Profiles, p) {
			return fmt.Errorf("profile %q is not allowed (allowed: %s)", p, strings.Join(pol.Profiles, ", "))
		}
	}
	if err := validateLimits(s.Limits); err != nil {
		return err
	}
	if len(s.Volumes) > 4 {
		return fmt.Errorf("at most 4 volumes")
	}
	for _, v := range s.Volumes {
		if !incus.ValidName(v.Name) || !strings.HasPrefix(v.Name, name+"-") {
			return fmt.Errorf("volume %q must be named %s-<something>", v.Name, name)
		}
		if !path.IsAbs(v.Path) || path.Clean(v.Path) != v.Path || len(v.Path) > 200 || strings.Contains(v.Path, "\x00") {
			return fmt.Errorf("volume path %q must be a clean absolute path", v.Path)
		}
		if v.Owner != "" && !incus.ValidUserName(v.Owner) {
			return fmt.Errorf("volume owner %q is not a valid Unix user name", v.Owner)
		}
	}
	if len(s.Env) > 64 {
		return fmt.Errorf("at most 64 environment variables")
	}
	for k, v := range s.Env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("environment variable name %q is not allowed", k)
		}
		if len(v) > 4096 || strings.ContainsAny(v, "\r\n\x00") {
			return fmt.Errorf("the value of %s is too long or has a line break", k)
		}
	}
	if len(s.Env) > 0 && !incus.ValidName(s.Service) {
		return fmt.Errorf("service (the systemd unit that reads the environment) is required with env")
	}
	if s.Service != "" && !incus.ValidName(s.Service) {
		return fmt.Errorf("service is not a valid unit name")
	}
	if h := s.Health; h != nil {
		if h.Port < 1 || h.Port > 65535 || h.Timeout < 0 || h.Timeout > 300 || (h.Path != "" && !healthRe.MatchString(h.Path)) {
			return fmt.Errorf("health needs a port (1-65535), an optional path such as /health and a timeout of at most 300 seconds")
		}
	}
	if s.Domain != "" {
		if len(s.Domain) > 253 || !domainRe.MatchString(s.Domain) {
			return fmt.Errorf("domain %q is not a valid host name", s.Domain)
		}
		if s.Health == nil {
			return fmt.Errorf("a domain needs health.port (the port the route points at)")
		}
	} else if len(s.RouteDirectives) > 0 {
		return fmt.Errorf("route_directives need a domain")
	}
	for _, d := range s.RouteDirectives {
		m := importRe.FindStringSubmatch(d)
		if m == nil || !contains(pol.RouteImports, m[1]) {
			return fmt.Errorf("route directive %q is not allowed (allowed imports: %s)", d, strings.Join(pol.RouteImports, ", "))
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// validateLimits reports why limits cannot be accepted: only limits.cpu (a plain number) and
// limits.memory (a number with a unit) are ever allowed, on a launch or a resize.
func validateLimits(limits map[string]string) error {
	for k, v := range limits {
		switch {
		case k == "limits.cpu" && cpuRe.MatchString(v):
		case k == "limits.memory" && memRe.MatchString(v):
		default:
			return fmt.Errorf("limit %q=%q is not allowed (only limits.cpu as a number and limits.memory such as 1GB)", k, v)
		}
	}
	return nil
}

// TemplateConfig turns the spec into the engine's template blueprint.
func (s *InstanceSpec) TemplateConfig() *config.TemplateConfig {
	t := &config.TemplateConfig{
		Name:              s.Template,
		Image:             s.Image,
		Service:           s.Service,
		Profiles:          s.Profiles,
		DefaultLimits:     s.Limits,
		RoutingDirectives: s.RouteDirectives,
	}
	for _, v := range s.Volumes {
		shifted := true
		if v.Shifted != nil {
			shifted = *v.Shifted
		}
		t.Volumes = append(t.Volumes, config.VolumeMount{Name: v.Name, Path: v.Path, Pool: "default", Shifted: shifted, Owner: v.Owner})
	}
	if s.Health != nil {
		t.HealthCheck = config.HealthCheckConfig{Path: s.Health.Path, Port: s.Health.Port, Timeout: s.Health.Timeout, Interval: 2}
		if t.HealthCheck.Timeout == 0 {
			t.HealthCheck.Timeout = 45
		}
	}
	return t
}

// UpdateRequest is a request to move an instance to another image through the safe update path
// (volume snapshot first, config and environment carried over, health-gated, rolled back on failure).
type UpdateRequest struct {
	Image   string      `json:"image"`
	Service string      `json:"service"`
	Health  *HealthSpec `json:"health,omitempty"`
	// Env sets these keys in the instance's carried-over /etc/default/<service> (e.g. RELEASE_TAG);
	// every other key is kept. A rollback restores the file as it was.
	Env map[string]string `json:"env,omitempty"`
}

// Validate reports why an update request cannot be accepted.
func (r *UpdateRequest) Validate() error {
	if !imageRe.MatchString(r.Image) {
		return fmt.Errorf("image is not a valid image reference")
	}
	if !incus.ValidName(r.Service) {
		return fmt.Errorf("service (the unit whose environment is carried over) is required")
	}
	if len(r.Env) > 16 {
		return fmt.Errorf("at most 16 environment variables in an update")
	}
	for k, v := range r.Env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("environment variable name %q is not allowed", k)
		}
		if len(v) > 4096 || strings.ContainsAny(v, "\r\n\x00") {
			return fmt.Errorf("the value of %s is too long or has a line break", k)
		}
	}
	if h := r.Health; h != nil {
		if h.Port < 1 || h.Port > 65535 || h.Timeout < 0 || h.Timeout > 300 || (h.Path != "" && !healthRe.MatchString(h.Path)) {
			return fmt.Errorf("health needs a port (1-65535), an optional path such as /health and a timeout of at most 300 seconds")
		}
	}
	return nil
}

// ResizeRequest is a request for a live CPU/memory change: no restart, no image change.
type ResizeRequest struct {
	Limits map[string]string `json:"limits"`
}

// Validate reports why a resize request cannot be accepted.
func (r *ResizeRequest) Validate() error {
	if len(r.Limits) == 0 {
		return fmt.Errorf("limits (limits.cpu and/or limits.memory) is required")
	}
	return validateLimits(r.Limits)
}

// SuspendRequest asks for an instance's published route to be replaced with a static 503 carrying
// Reason, without touching the instance. Domain and RouteDirectives describe the route the same way a
// launch does (Suspend never reads the instance's current site file to recover them), so the request
// is validated with the same instance policy an operator already set for launches.
type SuspendRequest struct {
	Domain          string   `json:"domain"`
	Reason          string   `json:"reason"`
	RouteDirectives []string `json:"route_directives,omitempty"`
}

// Validate reports why a suspend request cannot be accepted.
func (r *SuspendRequest) Validate(pol InstancePolicy) error {
	if len(r.Domain) > 253 || !domainRe.MatchString(r.Domain) {
		return fmt.Errorf("domain %q is not a valid host name", r.Domain)
	}
	if len(r.Reason) == 0 || len(r.Reason) > 200 {
		return fmt.Errorf("reason is required and at most 200 characters")
	}
	for _, d := range r.RouteDirectives {
		m := importRe.FindStringSubmatch(d)
		if m == nil || !contains(pol.RouteImports, m[1]) {
			return fmt.Errorf("route directive %q is not allowed (allowed imports: %s)", d, strings.Join(pol.RouteImports, ", "))
		}
	}
	return nil
}

// Instances performs tenant-instance operations on the host behind exec. Every operation is one
// idempotent engine call; each reports progress to the logf it is given.
type Instances struct {
	exec remote.Executor
	// healthGate is a field so tests can stub the wait.
	healthGate func(ctx context.Context, ip string, hc config.HealthCheckConfig) error
}

func NewInstances(exec remote.Executor) *Instances { return &Instances{exec: exec} }

func (i *Instances) manager(logf func(string, ...any)) *InstanceManager {
	m := NewInstanceManager(i.exec)
	if logf != nil {
		m.logf = logf
	}
	if i.healthGate != nil {
		m.healthGate = i.healthGate
	}
	return m
}

// Template returns the template an instance was launched from. exists is false when there is no such
// instance; an instance that exists but was not launched from a template has template "".
func (i *Instances) Template(ctx context.Context, name string) (template string, exists bool, err error) {
	c := incus.NewClient(i.exec)
	ok, err := c.InstanceExists(ctx, name)
	if err != nil || !ok {
		return "", false, err
	}
	st, err := c.CaptureInstanceState(ctx, name)
	if err != nil {
		return "", true, err
	}
	return st.Config[incus.TemplateKey], true, nil
}

// Launch creates the instance, or converges the one an earlier launch of this template created.
func (i *Instances) Launch(ctx context.Context, name string, spec InstanceSpec, logf func(string, ...any)) (string, error) {
	return i.manager(logf).Launch(ctx, LaunchParams{
		Template: spec.TemplateConfig(), Name: name, Slug: name, Domain: spec.Domain, Env: spec.Env,
	})
}

// Update moves the instance to a new image through the safe update path.
func (i *Instances) Update(ctx context.Context, name string, req UpdateRequest, logf func(string, ...any)) error {
	opts := UpdateOptions{Service: req.Service, SetEnv: req.Env}
	if req.Health != nil {
		opts.HealthCheck = config.HealthCheckConfig{Path: req.Health.Path, Port: req.Health.Port, Timeout: req.Health.Timeout, Interval: 2}
	}
	return i.manager(logf).Update(ctx, name, req.Image, opts)
}

// Resize applies req's limits live; no restart, no image change.
func (i *Instances) Resize(ctx context.Context, name string, req ResizeRequest, logf func(string, ...any)) error {
	return i.manager(logf).Resize(ctx, name, req.Limits)
}

// Suspend replaces the instance's published route with a static 503 carrying req.Reason, without
// touching the instance. Undone by asking for the instance again (Launch republishes the normal route
// unconditionally, so a converge after a suspend clears it).
func (i *Instances) Suspend(ctx context.Context, name string, req SuspendRequest, logf func(string, ...any)) error {
	routing := config.RoutingConfig{Domain: req.Domain, ExtraDirectives: req.RouteDirectives}
	return i.manager(logf).Suspend(ctx, name, routing, req.Reason)
}

// Destroy removes the instance and its route. With purge it also deletes the data volumes attached to
// it that are named after it (<name>-...), and no other volume.
func (i *Instances) Destroy(ctx context.Context, name string, purge bool, logf func(string, ...any)) error {
	c := incus.NewClient(i.exec)
	var volumes []incus.VolumeRef
	if purge {
		st, err := c.CaptureInstanceState(ctx, name)
		if err != nil {
			return err
		}
		for _, d := range st.Devices {
			if d["type"] == "disk" && d["pool"] != "" && strings.HasPrefix(d["source"], name+"-") {
				volumes = append(volumes, incus.VolumeRef{Pool: d["pool"], Name: d["source"]})
			}
		}
	}
	if err := i.manager(logf).Destroy(ctx, name, false); err != nil {
		return err
	}
	for _, v := range volumes {
		if err := c.DeleteVolume(ctx, v.Pool, v.Name); err != nil {
			return fmt.Errorf("purge volume %s: %w", v.Name, err)
		}
		if logf != nil {
			logf("    deleted volume %s\n", v.Name)
		}
	}
	return nil
}
