package engine

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

// Kinds of change a plan reports.
const (
	ChangeCreateInstance = "create-instance"
	ChangeReplaceImage   = "replace-image"
	ChangeAdoptImage     = "adopt-image"
	ChangeCreateVolume   = "create-volume"
	ChangeAttachVolume   = "attach-volume"
	ChangeSetLimits      = "set-limits"
	ChangeSetLabels      = "set-labels"
	ChangeSetEnv         = "set-env"
	ChangeRestart        = "restart"
	ChangePublishRoute   = "publish-route"
	ChangeUpdateRoute    = "update-route"
	ChangeBaseCaddyfile  = "create-base-caddyfile"
	ChangeRunHook        = "run-hook"
)

// Actions a service plan can have.
const (
	ActionNone    = "none"    // the service already matches its manifest
	ActionCreate  = "create"  // there is no such instance yet
	ActionUpdate  = "update"  // it exists and apply would change something
	ActionBlocked = "blocked" // apply would fail
)

// Change is one thing `apply` would do. Detail never contains a secret: environment
// values are never read into it, only key names.
type Change struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// ServicePlan is what `apply` would do for one service, worked out without changing anything.
type ServicePlan struct {
	Service string   `json:"service"`
	Action  string   `json:"action"`
	Changes []Change `json:"changes,omitempty"`
	// Notes are things apply will not act on but you should know about.
	Notes []string `json:"notes,omitempty"`
	// Blockers are reasons apply would fail on this service.
	Blockers []string `json:"blockers,omitempty"`

	// bind is a keyed digest of everything the manifest says that the plan does not print (see
	// WithBindKey). It is never serialized; it only goes into FleetPlan.Hash.
	bind string
}

func (p *ServicePlan) add(kind, format string, a ...any) {
	p.Changes = append(p.Changes, Change{Kind: kind, Detail: fmt.Sprintf(format, a...)})
}
func (p *ServicePlan) note(format string, a ...any) {
	p.Notes = append(p.Notes, fmt.Sprintf(format, a...))
}
func (p *ServicePlan) block(format string, a ...any) {
	p.Blockers = append(p.Blockers, fmt.Sprintf(format, a...))
}

func (p *ServicePlan) finish(exists bool) {
	switch {
	case len(p.Blockers) > 0:
		p.Action = ActionBlocked
	case !exists:
		p.Action = ActionCreate
	case len(p.Changes) > 0:
		p.Action = ActionUpdate
	default:
		p.Action = ActionNone
	}
}

// PlanService works out what DeployService would do for a service, without doing it. It
// makes the same decisions as DeployService by calling the same functions (imageDecision,
// limitsDrift, readEnvState, VolumeAttachment, PlanSiteFor), so the two cannot drift apart,
// and it should be given a Deployer built on remote.ReadOnly, which turns any command that
// could change the host into an error. A failure to read the host is an error; a manifest
// that apply would refuse is a Blocker in the plan.
func (d *Deployer) PlanService(ctx context.Context, svc *config.ServiceConfig, configDir string) (*ServicePlan, error) {
	if !incus.ValidName(svc.Name) {
		return nil, fmt.Errorf("invalid service name %q", svc.Name)
	}
	p := &ServicePlan{Service: svc.Name}
	deployRef, fp := d.resolveServiceImage(ctx, svc.Image)
	exists, err := d.incus.InstanceExists(ctx, svc.Name)
	if err != nil {
		return nil, err
	}
	if !exists {
		err = d.planFresh(ctx, p, svc, configDir, deployRef)
	} else {
		err = d.planConverge(ctx, p, svc, configDir, deployRef, fp)
	}
	if err != nil {
		return nil, err
	}
	p.finish(exists)
	p.bind = d.bindOf(svc, configDir)
	return p, nil
}

// bindOf digests what a service's manifest says that its plan does not show: the declared
// environment with its values, the resolved bodies of its hooks, and the rest of the manifest
// (health check, profiles, and so on). It is keyed (HMAC), so it says nothing about a value to
// anyone who lacks the key, and it is empty when no key was given.
func (d *Deployer) bindOf(svc *config.ServiceConfig, configDir string) string {
	if d.bind == nil {
		return ""
	}
	b, _ := json.Marshal(struct {
		Manifest *config.ServiceConfig
		Env      map[string]string
		Hooks    map[string]string
	}{svc, d.bindEnv(svc, configDir), map[string]string{
		"pre_deploy":     hookBody(configDir, svc, svc.Hooks.PreDeploy),
		"container_init": hookBody(configDir, svc, svc.Hooks.ContainerInit),
		"post_deploy":    hookBody(configDir, svc, svc.Hooks.PostDeploy),
	}})
	return d.bind(b)
}

// bindEnv is the environment bindOf digests: with resolved env_from values, so a changed secret changes
// the plan's hash and needs a fresh approval.
func (d *Deployer) bindEnv(svc *config.ServiceConfig, configDir string) map[string]string {
	env, _ := d.declaredEnv(svc, configDir)
	return env
}

func hookBody(configDir string, svc *config.ServiceConfig, hook string) string {
	if hook == "" {
		return ""
	}
	return hookScript(configDir, svc.Name, hook)
}

// PlanOption changes how PlanFleet plans.
type PlanOption func(*Deployer)

// WithSecrets makes the plan resolve env_from through lookup (the daemon's secret store) instead of the
// process environment. A secret that is not there blocks the service's plan.
func WithSecrets(lookup SecretLookup) PlanOption {
	return func(d *Deployer) { d.SetSecrets(lookup) }
}

// WithBindKey makes the plan's Hash cover the parts of a manifest the plan does not print
// (environment values, hook bodies), keyed with key so the hash reveals nothing about them.
// Without it a hash only covers what the plan says, and a changed secret or hook with the same
// key names would give the same hash. A daemon that gates apply on a hash must use it.
func WithBindKey(key []byte) PlanOption {
	return func(d *Deployer) {
		d.bind = func(data []byte) string {
			m := hmac.New(sha256.New, key)
			m.Write(data)
			return hex.EncodeToString(m.Sum(nil))
		}
	}
}

func hooksOf(svc *config.ServiceConfig) []string {
	var h []string
	if svc.Hooks.PreDeploy != "" {
		h = append(h, "pre_deploy")
	}
	if svc.Hooks.ContainerInit != "" {
		h = append(h, "container_init")
	}
	if svc.Hooks.PostDeploy != "" {
		h = append(h, "post_deploy")
	}
	return h
}

func (d *Deployer) planFresh(ctx context.Context, p *ServicePlan, svc *config.ServiceConfig, configDir, deployRef string) error {
	detail := fmt.Sprintf("launch %s from %s with profiles %s", svc.Name, deployRef, strings.Join(d.profilesOf(svc), ","))
	if len(svc.Limits) > 0 {
		detail += " and " + joinKV(svc.Limits)
	}
	if len(svc.Labels) > 0 {
		detail += ", labelled " + FormatLabels(svc.Labels)
	}
	p.add(ChangeCreateInstance, "%s", detail)
	for _, vol := range svc.Volumes {
		ok, err := d.incus.VolumeExists(ctx, vol.Pool, vol.Name)
		if err != nil {
			return err
		}
		if !ok {
			p.add(ChangeCreateVolume, "%s on pool %s", vol.Name, servicePool(vol.Pool))
			p.add(ChangeAttachVolume, "%s at %s", vol.Name, vol.Path)
		} else {
			p.add(ChangeAttachVolume, "the existing volume %s at %s (its data is kept)", vol.Name, vol.Path)
		}
	}
	declared, err := d.declaredEnv(svc, configDir)
	if err != nil {
		p.block("%v", err)
	}
	if len(declared) > 0 || len(svc.EnvFrom) > 0 {
		keys := sortedKeys(declared)
		for k := range svc.EnvFrom {
			if _, ok := declared[k]; !ok {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		p.add(ChangeSetEnv, "write /etc/default/%s with %d keys: %s", svc.Unit(), len(keys), strings.Join(envKeyLabels(svc, keys), ", "))
	}
	if err := d.planRoute(ctx, p, svc, nil, routeFresh); err != nil {
		return err
	}
	for _, h := range hooksOf(svc) {
		p.add(ChangeRunHook, "%s", h)
	}
	return nil
}

func (d *Deployer) planConverge(ctx context.Context, p *ServicePlan, svc *config.ServiceConfig, configDir, deployRef, fp string) error {
	st, err := d.incus.CaptureInstanceState(ctx, svc.Name)
	if err != nil {
		return err
	}

	replace := false
	switch imageDecision(st, fp, deployRef) {
	case imageUnknown:
		p.note("%s was launched from an image that cannot be identified, so apply will not replace it", svc.Name)
	case imageAdopt:
		p.add(ChangeAdoptImage, "record %s as the image %s runs (nothing restarts)", deployRef, svc.Name)
	case imageReplace:
		replace = true
		running := st.Config[incus.ImageKey]
		if fp != "" {
			running = short(st.BaseImage)
			p.add(ChangeReplaceImage, "%s: image %s -> %s, through the safe update path (volume snapshot first, config and env carried over, health-gated, rolled back on failure)", svc.Name, running, short(fp))
		} else {
			p.add(ChangeReplaceImage, "%s: image %s -> %s, through the safe update path (volume snapshot first, config and env carried over, health-gated, rolled back on failure)", svc.Name, running, deployRef)
		}
		for _, h := range hooksOf(svc) {
			p.add(ChangeRunHook, "%s", h)
		}
	}

	// Labels are applied live whether or not the image is replaced (a replacement carries the old ones over,
	// and apply then reconciles them to the manifest).
	if drift := labelsDrift(st.Config, svc.Labels); len(drift) > 0 {
		p.add(ChangeSetLabels, "%s", strings.Join(drift, ", "))
	}

	declared, envErr := d.declaredEnv(svc, configDir)
	if envErr != nil {
		p.block("%v", envErr)
	}
	if !replace {
		if drift := limitsDrift(st, svc.Limits); len(drift) > 0 {
			var parts []string
			for _, k := range sortedKeys(drift) {
				parts = append(parts, fmt.Sprintf("%s: %s -> %s", k, orNone(st.Config[k]), drift[k]))
			}
			p.add(ChangeSetLimits, "%s", strings.Join(parts, ", "))
		}
		for _, vol := range svc.Volumes {
			pool := servicePool(vol.Pool)
			ok, err := d.incus.VolumeExists(ctx, vol.Pool, vol.Name)
			if err != nil {
				return err
			}
			if !ok {
				p.add(ChangeCreateVolume, "%s on pool %s", vol.Name, pool)
			}
			attached, clash := st.VolumeAttachment(pool, vol.Name, vol.Path)
			switch {
			case attached:
			case clash != nil:
				p.block("%v", incus.VolumeClashError(svc.Name, vol.Name, vol.Path, clash))
			default:
				p.add(ChangeAttachVolume, "%s at %s", vol.Name, vol.Path)
			}
		}
	}

	if len(declared) > 0 && envErr == nil {
		es, err := readEnvState(ctx, d.incus, svc.Name, svc.Unit(), declared)
		if err != nil {
			return err
		}
		if !es.Found || es.Differs {
			var parts []string
			if !es.Found {
				parts = append(parts, "the file does not exist yet")
			}
			if len(es.Added) > 0 {
				parts = append(parts, "add "+strings.Join(envKeyLabels(svc, es.Added), ", "))
			}
			if len(es.Changed) > 0 {
				parts = append(parts, "change "+strings.Join(envKeyLabels(svc, es.Changed), ", "))
			}
			if es.Kept > 0 {
				parts = append(parts, fmt.Sprintf("%d keys the manifest does not declare are kept", es.Kept))
			}
			p.add(ChangeSetEnv, "/etc/default/%s: %s", svc.Unit(), strings.Join(parts, "; "))
			p.add(ChangeRestart, "%s restarts to pick up the environment", svc.Unit())
		}
		for k := range st.Config {
			if strings.HasPrefix(k, "environment.") {
				p.note("%s takes its environment from environment.* instance config (an OCI container); apply writes /etc/default/%s, which that container does not read", svc.Name, svc.Unit())
				break
			}
		}
	}

	if !sameProfiles(st.Profiles, d.profilesOf(svc)) {
		p.note("profiles of %s differ (live %v, declared %v); apply does not change profiles", svc.Name, st.Profiles, d.profilesOf(svc))
	}

	// A running instance is health-checked and routed to by address. A stopped one has none, and
	// apply would fail waiting for it, unless it is being replaced (the replacement gets its own).
	var ips []string
	if svc.HealthCheck.Path != "" || (svc.Routing != nil && svc.Routing.Domain != "") {
		ips, err = d.incus.GlobalIPv4s(ctx, svc.Name)
		if err != nil {
			return err
		}
		if len(ips) == 0 && !replace {
			p.block("%s has no IPv4 address (is it stopped?), so apply could not health-check it or route to it", svc.Name)
			return nil
		}
	}
	mode := routeExisting
	if replace {
		mode = routeReplacing
	}
	return d.planRoute(ctx, p, svc, ips, mode)
}

type routeMode int

const (
	routeFresh     routeMode = iota // the instance does not exist yet, so it has no address
	routeExisting                   // it runs, and ips are its addresses
	routeReplacing                  // it is being replaced, and the route follows the new address
)

// planRoute adds the route change, if any.
func (d *Deployer) planRoute(ctx context.Context, p *ServicePlan, svc *config.ServiceConfig, ips []string, mode routeMode) error {
	if svc.Routing == nil || svc.Routing.Domain == "" {
		return nil
	}
	if svc.Name == d.caddy.Container() && mode == routeFresh {
		p.note("%s is the edge: it is created first, and its Caddyfile is written if it has none", svc.Name)
		return nil
	}
	if svc.Name != d.caddy.Container() {
		ok, err := d.incus.InstanceExists(ctx, d.caddy.Container())
		if err != nil {
			return err
		}
		if !ok {
			p.block("the edge instance %q does not exist, so the route for %s cannot be published", d.caddy.Container(), svc.Routing.Domain)
			return nil
		}
	}
	if mode == routeReplacing {
		p.add(ChangeUpdateRoute, "%s follows the replacement's new address", svc.Routing.Domain)
		return nil
	}
	sp, err := d.caddy.PlanSiteFor(ctx, svc.Name, *svc.Routing, ips)
	if err != nil {
		p.block("%v", err)
		return nil
	}
	if sp.CreatesBaseCaddyfile {
		p.add(ChangeBaseCaddyfile, "the edge has no Caddyfile; one that imports the sites directory is written")
	}
	switch sp.Change {
	case "create":
		p.add(ChangePublishRoute, "%s -> %s:%d (%s)", svc.Routing.Domain, svc.Name, svc.Routing.UpstreamPort, sp.Detail)
	case "update":
		p.add(ChangeUpdateRoute, "%s -> %s:%d (%s)", svc.Routing.Domain, svc.Name, svc.Routing.UpstreamPort, sp.Detail)
	}
	return nil
}

func short(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

func orNone(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func joinKV(m map[string]string) string {
	var parts []string
	for _, k := range sortedKeys(m) {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, " ")
}

// FleetPlan is the plan for every service that would be applied.
type FleetPlan struct {
	Services []*ServicePlan `json:"services"`
}

// Counts returns how many services fall in each action.
func (f *FleetPlan) Counts() map[string]int {
	c := map[string]int{}
	for _, s := range f.Services {
		c[s.Action]++
	}
	return c
}

// Pending reports whether apply would change anything.
func (f *FleetPlan) Pending() bool {
	c := f.Counts()
	return c[ActionCreate]+c[ActionUpdate] > 0
}

// Blocked reports whether apply would fail on any service.
func (f *FleetPlan) Blocked() bool { return f.Counts()[ActionBlocked] > 0 }

// Render is the human-readable form.
func (f *FleetPlan) Render() string {
	var sb strings.Builder
	sym := map[string]string{ActionNone: " ", ActionCreate: "+", ActionUpdate: "~", ActionBlocked: "!"}
	svcs := append([]*ServicePlan(nil), f.Services...)
	sort.SliceStable(svcs, func(i, j int) bool { return svcs[i].Service < svcs[j].Service })
	for _, s := range svcs {
		fmt.Fprintf(&sb, "%s %s (%s)\n", sym[s.Action], s.Service, s.Action)
		for _, c := range s.Changes {
			fmt.Fprintf(&sb, "    %-22s %s\n", c.Kind, c.Detail)
		}
		for _, b := range s.Blockers {
			fmt.Fprintf(&sb, "    BLOCKED: %s\n", b)
		}
		for _, n := range s.Notes {
			fmt.Fprintf(&sb, "    note: %s\n", n)
		}
	}
	c := f.Counts()
	fmt.Fprintf(&sb, "\nPlan: %d to create, %d to update, %d unchanged, %d blocked.\n", c[ActionCreate], c[ActionUpdate], c[ActionNone], c[ActionBlocked])
	return sb.String()
}

// ErrBadConfig marks a plan that failed because the configuration is unusable (missing
// fleet.yml, a manifest that does not parse), as opposed to the host being unreadable.
var ErrBadConfig = errors.New("invalid configuration")

// PlanFleet plans every service in a config directory (only one when service is set) against
// the host behind exec, which it wraps in remote.ReadOnly, so the host cannot change.
func PlanFleet(ctx context.Context, exec remote.Executor, configDir, service string, opts ...PlanOption) (*FleetPlan, error) {
	if _, err := config.LoadFleetConfig(configDir); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadConfig, err)
	}
	services, err := config.LoadServices(configDir, service)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadConfig, err)
	}
	d := NewDeployer(remote.ReadOnly(exec))
	for _, o := range opts {
		o(d)
	}
	fp := &FleetPlan{Services: []*ServicePlan{}}
	for _, svc := range services {
		p, err := d.PlanService(ctx, svc, configDir)
		if err != nil {
			return nil, fmt.Errorf("plan service %s: %w", svc.Name, err)
		}
		fp.Services = append(fp.Services, p)
	}
	return fp, nil
}

// ExitStatus is the status `native-ops plan` exits with: 1 when apply would fail on some
// service, 2 when changes are pending, 0 when there is nothing to do.
func (f *FleetPlan) ExitStatus() int {
	switch {
	case f.Blocked():
		return 1
	case f.Pending():
		return 2
	}
	return 0
}

// Hash identifies what a plan would do: the same changes give the same hash, whatever order the
// services were planned in. An apply that is given the hash of a plan somebody reviewed can
// refuse to run if the host or the manifests have changed since.
func (f *FleetPlan) Hash() string {
	svcs := append([]*ServicePlan(nil), f.Services...)
	sort.SliceStable(svcs, func(i, j int) bool { return svcs[i].Service < svcs[j].Service })
	type hashed struct {
		Plan *ServicePlan
		Bind string
	}
	rows := make([]hashed, len(svcs))
	for i, sp := range svcs {
		rows[i] = hashed{sp, sp.bind}
	}
	b, _ := json.Marshal(rows)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ApplyPlan applies what a plan says would change, and nothing else: each service whose action is
// create or update is deployed, in plan order, and it stops at the first failure (services after
// it are left alone; running the same apply again is safe and continues). A blocked plan is
// refused before anything is touched. configDir must be the tree the plan was made from.
func (d *Deployer) ApplyPlan(ctx context.Context, configDir string, plan *FleetPlan) error {
	if plan.Blocked() {
		return errors.New("the plan is blocked; nothing was applied")
	}
	for _, sp := range plan.Services {
		if sp.Action != ActionCreate && sp.Action != ActionUpdate {
			continue
		}
		svcs, err := config.LoadServices(configDir, sp.Service)
		if err != nil || len(svcs) != 1 {
			return fmt.Errorf("service %s is in the plan but its manifest could not be loaded", sp.Service)
		}
		if err := d.DeployService(ctx, svcs[0], configDir); err != nil {
			return fmt.Errorf("apply %s: %w", sp.Service, err)
		}
	}
	return nil
}
