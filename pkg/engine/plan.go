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
	ActionSkipped = "skipped" // it belongs to another host, whose daemon plans and applies it
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
	// Host is the host a skipped service belongs to (ActionSkipped only).
	Host string `json:"host,omitempty"`

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
	if err := d.planVolumeSnapshots(ctx, p, svc.Volumes, configDir); err != nil {
		return err
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
		where := fmt.Sprintf("/etc/default/%s", svc.Unit())
		if ociImageRef(deployRef) {
			where = "environment.*"
		}
		p.add(ChangeSetEnv, "write %s with %d keys: %s", where, len(keys), strings.Join(envKeyLabels(svc, keys), ", "))
	}
	for _, fwd := range svc.Forwards {
		props, err := fwd.DeviceProps()
		if err != nil {
			p.block("%s: %v", svc.Name, err)
			continue
		}
		p.add(ChangePublishPort, "%s: %s -> %s", fwd.DeviceName(), props["listen"], props["connect"])
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

	// Host ports are converged whether or not the image is replaced (a replacement carries its devices
	// over, and apply then reconciles them to the manifest).
	planForwards(p, st, svc.Forwards)

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
	// A volume's snapshot schedule is its own config, converged whether or not the image is replaced (a
	// replacement keeps the same volumes).
	if err := d.planVolumeSnapshots(ctx, p, svc.Volumes, configDir); err != nil {
		return err
	}

	if len(declared) > 0 && envErr == nil {
		if usesOCIEnv(st, deployRef) {
			es := diffEnv(true, ociLiveEnv(st), declared)
			if es.Differs {
				p.add(ChangeSetEnv, "environment.*: %s", envChangeDetail(svc, es, false))
				p.add(ChangeRestart, "%s restarts to pick up the environment", svc.Name)
			}
		} else {
			es, err := readEnvState(ctx, d.incus, svc.Name, svc.Unit(), declared)
			if err != nil {
				return err
			}
			if !es.Found || es.Differs {
				p.add(ChangeSetEnv, "/etc/default/%s: %s", svc.Unit(), envChangeDetail(svc, es, !es.Found))
				p.add(ChangeRestart, "%s restarts to pick up the environment", svc.Unit())
			}
		}
	}

	if !sameProfiles(st.Profiles, d.profilesOf(svc)) {
		p.note("profiles of %s differ (live %v, declared %v); apply does not change profiles", svc.Name, st.Profiles, d.profilesOf(svc))
	}

	// A running instance is health-checked and routed to by address. A stopped one has none, and
	// apply would fail waiting for it, unless it is being replaced (the replacement gets its own).
	var ips []string
	if svc.HealthCheck.Path != "" || routeManaged(svc) {
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
	if svc.Routing != nil && svc.Routing.External {
		return nil
	}
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

// FleetPlan is the plan for every service that would be applied, and for the network they share (DNS,
// the edge's Caddyfile). A plan for a single service has no network part.
type FleetPlan struct {
	Services []*ServicePlan `json:"services"`
	Network  *NetworkPlan   `json:"network,omitempty"`
	// Mesh is this host's side of fleet.yml's WireGuard mesh; nil (and absent from the JSON, the text and
	// the hash) when fleet.yml has no enabled mesh.
	Mesh *MeshPlan `json:"mesh,omitempty"`
	// Host is the host this plan is the share of, when fleet.yml has several (see config.DaemonHost):
	// the services of the other hosts are in Services as skipped, and nothing else of theirs is planned.
	// Empty for a fleet of one host (or none), whose plan is all of it.
	Host string `json:"host,omitempty"`
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
	return c[ActionCreate]+c[ActionUpdate] > 0 || (f.Network != nil && len(f.Network.Changes) > 0) || (f.Mesh != nil && len(f.Mesh.Changes) > 0)
}

// Blocked reports whether apply would fail on any service, or on the network.
func (f *FleetPlan) Blocked() bool {
	return f.Counts()[ActionBlocked] > 0 || (f.Network != nil && len(f.Network.Blockers) > 0) || (f.Mesh != nil && len(f.Mesh.Blockers) > 0)
}

// Render is the human-readable form.
func (f *FleetPlan) Render() string {
	var sb strings.Builder
	sym := map[string]string{ActionNone: " ", ActionCreate: "+", ActionUpdate: "~", ActionBlocked: "!"}
	svcs := append([]*ServicePlan(nil), f.Services...)
	sort.SliceStable(svcs, func(i, j int) bool { return svcs[i].Service < svcs[j].Service })
	if f.Host != "" {
		fmt.Fprintf(&sb, "Host %s: only its own services are planned; those of other hosts are skipped.\n\n", f.Host)
	}
	for _, s := range svcs {
		if s.Action == ActionSkipped {
			fmt.Fprintf(&sb, "- %s (skipped: on host %s)\n", s.Service, s.Host)
			continue
		}
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
	if n := f.Network; n != nil && (len(n.Changes)+len(n.Blockers)+len(n.Notes) > 0) {
		switch {
		case len(n.Blockers) > 0:
			sb.WriteString("! network (blocked)\n")
		case len(n.Changes) > 0:
			sb.WriteString("~ network (update)\n")
		default:
			sb.WriteString("  network (none)\n")
		}
		for _, c := range n.Changes {
			fmt.Fprintf(&sb, "    %-22s %s\n", c.Kind, c.Detail)
		}
		for _, b := range n.Blockers {
			fmt.Fprintf(&sb, "    BLOCKED: %s\n", b)
		}
		for _, note := range n.Notes {
			fmt.Fprintf(&sb, "    note: %s\n", note)
		}
	}
	if m := f.Mesh; m != nil && (len(m.Changes)+len(m.Blockers)+len(m.Notes) > 0) {
		switch {
		case len(m.Blockers) > 0:
			fmt.Fprintf(&sb, "! mesh %s (blocked)\n", m.Interface)
		case len(m.Changes) > 0:
			fmt.Fprintf(&sb, "~ mesh %s (update)\n", m.Interface)
		default:
			fmt.Fprintf(&sb, "  mesh %s (none)\n", m.Interface)
		}
		for _, c := range m.Changes {
			fmt.Fprintf(&sb, "    %-22s %s\n", c.Kind, c.Detail)
		}
		for _, b := range m.Blockers {
			fmt.Fprintf(&sb, "    BLOCKED: %s\n", b)
		}
		for _, note := range m.Notes {
			fmt.Fprintf(&sb, "    note: %s\n", note)
		}
	}
	c := f.Counts()
	fmt.Fprintf(&sb, "\nPlan: %d to create, %d to update, %d unchanged, %d blocked", c[ActionCreate], c[ActionUpdate], c[ActionNone], c[ActionBlocked])
	if c[ActionSkipped] > 0 {
		fmt.Fprintf(&sb, ", %d on other hosts", c[ActionSkipped])
	}
	if n := f.Network; n != nil && len(n.Blockers) > 0 {
		sb.WriteString("; the network is blocked")
	} else if n != nil && len(n.Changes) > 0 {
		if len(n.Changes) == 1 {
			sb.WriteString("; 1 network change")
		} else {
			fmt.Fprintf(&sb, "; %d network changes", len(n.Changes))
		}
	}
	if m := f.Mesh; m != nil && len(m.Blockers) > 0 {
		sb.WriteString("; the mesh is blocked")
	} else if m != nil && len(m.Changes) == 1 {
		sb.WriteString("; 1 mesh change")
	} else if m != nil && len(m.Changes) > 1 {
		fmt.Fprintf(&sb, "; %d mesh changes", len(m.Changes))
	}
	sb.WriteString(".\n")
	return sb.String()
}

// ErrBadConfig marks a plan that failed because the configuration is unusable (missing
// fleet.yml, a manifest that does not parse), as opposed to the host being unreadable.
var ErrBadConfig = errors.New("invalid configuration")

// PlanFleet plans every service in a config directory (only one when service is set) against
// the host behind exec, which it wraps in remote.ReadOnly, so the host cannot change.
//
// When fleet.yml names several hosts, the plan is this daemon's share (WithHostName): the services of
// the other hosts are listed as skipped and never planned against this host, and the fleet-wide work
// that another host's daemon does (dns_records: the default host's) is left to it. Without a host name
// such a fleet is refused, rather than every host's services being planned here.
func PlanFleet(ctx context.Context, exec remote.Executor, configDir, service string, opts ...PlanOption) (*FleetPlan, error) {
	fleet, err := config.LoadFleetConfig(configDir)
	if err != nil {
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
	mine, share, err := shareOf(fleet, services, d.hostName)
	if err != nil {
		return nil, err
	}
	d.share = share
	fp := &FleetPlan{Services: []*ServicePlan{}}
	if share != nil {
		fp.Host = share.host
	}
	for _, svc := range services {
		if h, other := share.elsewhere(svc.Name); other {
			fp.Services = append(fp.Services, &ServicePlan{Service: svc.Name, Action: ActionSkipped, Host: h})
			continue
		}
		p, err := d.PlanService(ctx, svc, configDir)
		if err != nil {
			return nil, fmt.Errorf("plan service %s: %w", svc.Name, err)
		}
		fp.Services = append(fp.Services, p)
	}
	// The network is the whole fleet's: a plan narrowed to one service leaves it out.
	if service == "" {
		if fp.Network, err = d.PlanNetwork(ctx, fleet, configDir, mine); err != nil {
			return nil, fmt.Errorf("plan the network: %w", err)
		}
		host, _, _ := fleet.DaemonHost(d.hostName) // shareOf already refused a host name it would not take
		if fp.Mesh, err = d.PlanMesh(ctx, fleet, host); err != nil {
			return nil, fmt.Errorf("plan the mesh: %w", err)
		}
	}
	return fp, nil
}

// WithHostName says which of fleet.yml's hosts this daemon is (`serve --host-name`). With several hosts
// the plan is that host's share; with one it must be that host (or empty).
func WithHostName(name string) PlanOption {
	return func(d *Deployer) { d.hostName = name }
}

// HostServices is the services of the host a run on it is (hostName, as for WithHostName), for the
// commands that deploy without a plan: the same rule as a plan's, refusal included.
func HostServices(fleet *config.FleetConfig, services []*config.ServiceConfig, hostName string) ([]*config.ServiceConfig, error) {
	mine, _, err := shareOf(fleet, services, hostName)
	return mine, err
}

// hostShare is what a plan of one host's share knows about the rest of the fleet.
type hostShare struct {
	host        string            // the host this daemon is
	defaultHost string            // fleet.yml's default host: its daemon does the fleet-wide work (dns_records)
	others      map[string]string // the services of other hosts, by name, with their host
}

// elsewhere reports whether a service belongs to another host, and which. A nil share (a fleet of one
// host) has none.
func (s *hostShare) elsewhere(service string) (string, bool) {
	if s == nil {
		return "", false
	}
	h, ok := s.others[service]
	return h, ok
}

// shareOf splits the services into this daemon's and the others' (config.DaemonHost decides whether the
// plan is a share at all, or a refusal), after checking each `host:` names a host of fleet.yml.
func shareOf(fleet *config.FleetConfig, services []*config.ServiceConfig, hostName string) ([]*config.ServiceConfig, *hostShare, error) {
	for _, svc := range services {
		if err := fleet.ValidateServiceHost(svc); err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrBadConfig, err)
		}
	}
	host, filtered, err := fleet.DaemonHost(hostName)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrBadConfig, err)
	}
	if !filtered {
		return services, nil, nil
	}
	share := &hostShare{host: host, defaultHost: fleet.DefaultHostName(), others: map[string]string{}}
	var mine []*config.ServiceConfig
	for _, svc := range services {
		if h := fleet.HostOf(svc); h != host {
			share.others[svc.Name] = h
			continue
		}
		mine = append(mine, svc)
	}
	return mine, share, nil
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
	// Only what this daemon would apply is hashed: a service of another host (skipped) is left out, so a
	// change to it does not ask this host for a fresh approval, and each host approves its own share.
	var svcs []*ServicePlan
	for _, sp := range f.Services {
		if sp.Action != ActionSkipped {
			svcs = append(svcs, sp)
		}
	}
	sort.SliceStable(svcs, func(i, j int) bool { return svcs[i].Service < svcs[j].Service })
	type hashed struct {
		Plan *ServicePlan
		Bind string
	}
	rows := make([]hashed, len(svcs))
	for i, sp := range svcs {
		rows[i] = hashed{sp, sp.bind}
	}
	var b []byte
	switch {
	case f.Mesh != nil:
		// Only a fleet with an enabled mesh hashes this shape, so every other plan keeps its hash.
		b, _ = json.Marshal(struct {
			Host        string
			Services    []hashed
			Network     *NetworkPlan
			NetworkBind string
			Mesh        *MeshPlan
			MeshBind    string
		}{f.Host, rows, f.Network, f.Network.bindOrEmpty(), f.Mesh, f.Mesh.bindOrEmpty()})
	case f.Host != "":
		// A share of a fleet of several hosts names its host, so it is never the same plan as another
		// host's share that happens to do the same things.
		b, _ = json.Marshal(struct {
			Host        string
			Services    []hashed
			Network     *NetworkPlan
			NetworkBind string
		}{f.Host, rows, f.Network, f.Network.bindOrEmpty()})
	case f.Network == nil:
		b, _ = json.Marshal(rows) // the same hash a plan had before it had a network part
	default:
		b, _ = json.Marshal(struct {
			Services    []hashed
			Network     *NetworkPlan
			NetworkBind string
		}{rows, f.Network, f.Network.bind})
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ApplyPlan applies what a plan says would change, and nothing else: the network part first (DNS records,
// the edge's Caddyfile), then each service whose action is create or update is deployed, in plan order, and it stops at the first failure (services after
// it are left alone; running the same apply again is safe and continues). A blocked plan is
// refused before anything is touched. configDir must be the tree the plan was made from.
func (d *Deployer) ApplyPlan(ctx context.Context, configDir string, plan *FleetPlan) error {
	if plan.Blocked() {
		return errors.New("the plan is blocked; nothing was applied")
	}
	if err := checkShare(configDir, plan); err != nil {
		return fmt.Errorf("%w; nothing was applied", err)
	}
	// A launch makes sure of this host's bridge, as fleet.yml declares it for the host this daemon is: the
	// host whose share the plan is (checkShare made sure a fleet of several hosts was planned as one).
	fleet, err := config.LoadFleetConfig(configDir)
	if err != nil {
		return err
	}
	if n, ok := fleet.LocalNetwork(plan.Host); ok {
		if err := d.SetNetwork(n); err != nil {
			return fmt.Errorf("network: %w", err)
		}
	}
	// The mesh first: it is this host's own config, and nothing else depends on it being late.
	if err := d.applyMesh(ctx, configDir, plan.Mesh); err != nil {
		return fmt.Errorf("apply the mesh: %w", err)
	}
	// The network next: DNS and the edge's Caddyfile are what the services' routes are reached by.
	if err := d.applyNetwork(ctx, configDir, plan.Network); err != nil {
		return fmt.Errorf("apply the network: %w", err)
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

// checkShare is apply's own check of the host rule, so that a plan can never deploy another host's
// service: a fleet of several hosts must have been planned as one host's share, and every service the plan
// changes must belong to that host.
func checkShare(configDir string, plan *FleetPlan) error {
	if !plan.Pending() {
		return nil
	}
	fleet, err := config.LoadFleetConfig(configDir)
	if err != nil {
		return err
	}
	if len(fleet.Hosts) < 2 {
		if plan.Host != "" {
			return fmt.Errorf("the plan is host %s's share, but fleet.yml has %d hosts", plan.Host, len(fleet.Hosts))
		}
		return nil
	}
	if plan.Host == "" {
		return fmt.Errorf("%w: fleet.yml has %d hosts, and the plan is not one host's share", config.ErrHostNameRequired, len(fleet.Hosts))
	}
	for _, sp := range plan.Services {
		if sp.Action != ActionCreate && sp.Action != ActionUpdate {
			continue
		}
		svcs, err := config.LoadServices(configDir, sp.Service)
		if err != nil || len(svcs) != 1 {
			return fmt.Errorf("service %s is in the plan but its manifest could not be loaded", sp.Service)
		}
		if h := fleet.HostOf(svcs[0]); h != plan.Host {
			return fmt.Errorf("service %s belongs to host %s, and this plan is host %s's share", sp.Service, h, plan.Host)
		}
	}
	return nil
}
