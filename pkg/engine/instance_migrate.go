package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
)

// Moving a tenant instance to another host, driven by the daemons (theta42/native-ops#87). Each daemon
// acts only on its own host, and no daemon calls another:
//
//  1. the source host's daemon migrates the instance (Instances.Migrate: the same steps and rollback as
//     `instance migrate`), then drops its route and marks its stopped copy as migrated;
//  2. the orchestrator reads the instance's spec from the source (Instances.Record, export-spec);
//  3. the target host's daemon adopts the instance it now runs (Instances.Adopt): it checks it, records
//     the spec as its own and publishes the route;
//  4. the orchestrator points DNS at the target, and later asks the source to finalize (delete its copy).

// LocalRemote is the Incus remote that names the host Incus itself runs on: the source of a migration
// the daemon runs.
const LocalRemote = "local"

// InstanceRecord is what this host records about an instance.
type InstanceRecord struct {
	Exists   bool
	Status   string // "Running", "Stopped", ...
	Template string // "" for an instance not launched from a template
	Labels   map[string]string
	// MigratedTo is the remote the instance was migrated to, when this host holds only its stopped copy.
	MigratedTo string
	// Spec is the spec recorded when the instance was launched, with what has changed since overlaid (the
	// image, limits and labels it has now). It never carries Env. nil when none is recorded: the instance
	// was launched before specs were recorded (asking for it again with the same spec records one), or not
	// by the daemon.
	Spec *InstanceSpec
	// HostPaths are the host directories the instance mounts. Such an instance cannot be migrated.
	HostPaths []string
}

// Record returns what this host records about the instance called name (Exists false when there is none).
func (i *Instances) Record(ctx context.Context, name string) (InstanceRecord, error) {
	rec, _, err := i.record(ctx, name)
	return rec, err
}

func (i *Instances) record(ctx context.Context, name string) (InstanceRecord, *incus.InstanceState, error) {
	c := incus.NewClient(i.exec)
	status, exists, err := c.InstanceStatus(ctx, name)
	if err != nil || !exists {
		return InstanceRecord{}, nil, err
	}
	st, err := c.CaptureInstanceState(ctx, name)
	if err != nil {
		return InstanceRecord{Exists: true, Status: status}, nil, err
	}
	rec, err := recordOf(status, st)
	return rec, st, err
}

func recordOf(status string, st *incus.InstanceState) (InstanceRecord, error) {
	rec := InstanceRecord{Exists: true, Status: status, Template: st.Config[incus.TemplateKey], MigratedTo: st.Config[incus.MigratedToKey], Labels: incus.LabelsOf(st.Config)}
	for _, dn := range st.DeviceNames() {
		if d := st.Devices[dn]; d["type"] == "disk" && d["pool"] == "" && d["source"] != "" {
			rec.HostPaths = append(rec.HostPaths, d["source"])
		}
	}
	raw := st.Config[incus.SpecKey]
	if raw == "" {
		return rec, nil
	}
	var spec InstanceSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return rec, fmt.Errorf("the spec recorded on %s does not parse: %w", st.Name, err)
	}
	spec.Env = nil // never recorded; cleared again so nothing can ever answer with it
	if img := st.Config[incus.ImageKey]; img != "" {
		spec.Image = img // an update moves the image without a new spec
	}
	for _, k := range []string{"limits.cpu", "limits.memory"} {
		if v := st.Config[k]; v != "" { // a resize changes the limits live
			if spec.Limits == nil {
				spec.Limits = map[string]string{}
			}
			spec.Limits[k] = v
		}
	}
	spec.Labels = incus.LabelsOf(st.Config) // the labels it carries now
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	rec.Spec = &spec
	return rec, nil
}

// recordSpec records spec on the instance, without its environment (which may hold secrets and lives in
// the instance's environment file). It writes nothing when the same spec is recorded already.
func (i *Instances) recordSpec(ctx context.Context, name string, spec InstanceSpec, logf func(string, ...any)) error {
	spec.Env = nil
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	c := incus.NewClient(i.exec)
	st, err := c.CaptureInstanceState(ctx, name)
	if err != nil {
		return err
	}
	if st.Config[incus.SpecKey] == string(b) {
		return nil
	}
	if err := c.SetInstanceConfig(ctx, name, incus.SpecKey, string(b)); err != nil {
		return err
	}
	if logf != nil {
		logf("    recorded the spec of %s\n", name)
	}
	return nil
}

// MigrateRequest asks the daemon to move one of its instances to another host.
type MigrateRequest struct {
	// TargetRemote is the Incus remote, configured on this host, of the host to move the instance to.
	TargetRemote string `json:"target_remote"`
	// Resume allows the target to hold a stopped copy already (an interrupted earlier migration), which
	// is then refreshed instead of being an error.
	Resume bool `json:"resume,omitempty"`
}

// Validate reports why a migrate request cannot be accepted.
func (r *MigrateRequest) Validate() error {
	if !incus.ValidName(r.TargetRemote) {
		return fmt.Errorf("target_remote must be the name of an Incus remote configured on this host")
	}
	if r.TargetRemote == LocalRemote {
		return fmt.Errorf("target_remote %q is this host", LocalRemote)
	}
	return nil
}

// MigrationRefusal is why an instance cannot be migrated from this host as it is ("" when it can).
func (r InstanceRecord) MigrationRefusal() string {
	switch {
	case !r.Exists:
		return "no such instance"
	case r.Template == "":
		return "that is not a tenant instance: it was not launched from a template"
	case r.MigratedTo != "":
		return "it was already migrated to " + r.MigratedTo
	case r.Spec == nil:
		return "no spec is recorded on it (it was launched before specs were recorded): ask for it again with PUT and the same spec, which records one, then migrate"
	case len(r.HostPaths) > 0:
		return fmt.Sprintf("it mounts the host path %s, which cannot be migrated; move that data to a storage volume first", r.HostPaths[0])
	}
	return ""
}

func healthOf(spec *InstanceSpec) config.HealthCheckConfig {
	if spec == nil || spec.Health == nil || spec.Health.Path == "" {
		return config.HealthCheckConfig{}
	}
	return spec.TemplateConfig().HealthCheck
}

func (i *Instances) migrationManager(logf func(string, ...any)) *MigrationManager {
	mm := NewMigrationManager(i.exec)
	if logf != nil {
		mm.logf = logf
	}
	if i.migrationProbe != nil {
		mm.healthProbe = i.migrationProbe
	}
	return mm
}

// Migrate moves the instance from this host to req.TargetRemote with the steps and rollback of
// MigrationManager.Migrate, health-checking the target with the instance's recorded health check. Only on
// success does it mark the copy left here as migrated (MigratedToKey) and remove the instance's route from
// this host's edge; a failed migration leaves both as they were. The summary it returns says what happened,
// including that volume snapshots were not moved.
func (i *Instances) Migrate(ctx context.Context, name string, req MigrateRequest, logf func(string, ...any)) (summary string, err error) {
	if err := req.Validate(); err != nil {
		return "", err
	}
	rec, err := i.Record(ctx, name)
	if err != nil {
		return "", err
	}
	if why := rec.MigrationRefusal(); why != "" {
		return "", fmt.Errorf("cannot migrate %s: %s (nothing was changed)", name, why)
	}
	target := req.TargetRemote
	err = i.migrationManager(logf).Migrate(ctx, MigrationParams{
		SourceRemote: LocalRemote, TargetRemote: target, InstanceName: name,
		HealthCheck: healthOf(rec.Spec), Resume: req.Resume,
	})
	if err != nil {
		return "", err // its route here was not touched
	}
	c := incus.NewClient(i.exec)
	if err := c.SetInstanceConfig(ctx, name, incus.MigratedToKey, target); err != nil {
		return "", fmt.Errorf("%s runs on %s now, but the stopped copy here could not be marked as migrated, and its route here still points at that copy: %w", name, target, err)
	}
	if err := i.manager(logf).caddy.RemoveSite(ctx, name); err != nil {
		return "", fmt.Errorf("%s runs on %s now and the copy here is marked as migrated, but its route could not be removed from this host's edge (finalize-migration removes it): %w", name, target, err)
	}
	if logf != nil {
		logf("    removed the route of %s from this host's edge\n", name)
	}
	return fmt.Sprintf("%s runs on %s. The copy here is stopped, marked as migrated, and its route here was removed. "+
		"Volume snapshots were not moved: they stay on this host with its copy of the volumes. "+
		"Next: adopt the instance on the target with this host's export-spec, point DNS at the target, then finalize-migration here.", name, target), nil
}

// FinalizeMigrationRequest asks the daemon to delete its stopped copy of an instance it migrated away.
type FinalizeMigrationRequest struct {
	// TargetRemote, when set, must be the remote the instance was migrated to. It is needed only when the
	// copy here is gone already (a repeated finalize), to say where the instance must be running.
	TargetRemote string `json:"target_remote,omitempty"`
	// PurgeSourceVolumes also deletes this host's copies of its data volumes (and their snapshots). By
	// default they are kept, unattached, as the last copy taken before the move.
	PurgeSourceVolumes bool `json:"purge_source_volumes,omitempty"`
}

// FinalizeMigration deletes the stopped copy of an instance this host migrated away, once the target is
// verified (MigrationManager.Finalize). It refuses an instance that was not migrated. Repeating it after
// it succeeded succeeds, as long as req names the target and the instance is running there.
func (i *Instances) FinalizeMigration(ctx context.Context, name string, req FinalizeMigrationRequest, logf func(string, ...any)) error {
	rec, err := i.Record(ctx, name)
	if err != nil {
		return err
	}
	target := req.TargetRemote
	switch {
	case rec.Exists && rec.MigratedTo == "":
		return fmt.Errorf("%s was not migrated away from this host; refusing to delete it", name)
	case rec.Exists && target != "" && target != rec.MigratedTo:
		return fmt.Errorf("%s was migrated to %s, not %s", name, rec.MigratedTo, target)
	case rec.Exists:
		target = rec.MigratedTo
	case target == "":
		return fmt.Errorf("there is no copy of %s on this host; name target_remote to check that a finalize already ran", name)
	}
	if err := (&MigrateRequest{TargetRemote: target}).Validate(); err != nil {
		return err
	}
	// Migrate removed the route; if that failed, this is where it goes (a no-op otherwise).
	if err := i.manager(logf).caddy.RemoveSite(ctx, name); err != nil {
		return err
	}
	return i.migrationManager(logf).Finalize(ctx, FinalizeParams{
		SourceRemote: LocalRemote, TargetRemote: target, InstanceName: name,
		HealthCheck: healthOf(rec.Spec), PurgeSourceVolumes: req.PurgeSourceVolumes,
	})
}

// Adopt takes over an instance that a migration put on this host: the instance must exist here, be
// running, come from spec's template, and have every volume spec names attached. It records spec as this
// host's own, clears a migrated mark it may carry (an instance migrated back), health-checks it when the
// spec has a check, and publishes its route the way a launch does. It never creates, starts or replaces
// anything, and repeating it changes nothing. spec must not carry Env: the environment file moved with
// the instance.
func (i *Instances) Adopt(ctx context.Context, name string, spec InstanceSpec, logf func(string, ...any)) error {
	if len(spec.Env) > 0 {
		return fmt.Errorf("adopt takes no env: the instance's environment file moved with it")
	}
	rec, st, err := i.record(ctx, name)
	switch {
	case err != nil:
		return err
	case !rec.Exists:
		return fmt.Errorf("there is no instance %s on this host: migrate it here first", name)
	case rec.Template == "":
		return fmt.Errorf("%s is not a tenant instance: it was not launched from a template", name)
	case rec.Template != spec.Template:
		return fmt.Errorf("%s here was launched from template %q, not %q", name, rec.Template, spec.Template)
	case rec.Status != "Running":
		return fmt.Errorf("%s here is %s, not Running; a migration leaves the instance running on its target", name, rec.Status)
	}
	attached := map[incus.VolumeRef]bool{}
	for _, v := range st.Volumes() {
		attached[v] = true
	}
	c := incus.NewClient(i.exec)
	for _, v := range spec.Volumes {
		ok, err := c.VolumeExists(ctx, "default", v.Name)
		if err != nil {
			return err
		}
		if !ok || !attached[incus.VolumeRef{Pool: "default", Name: v.Name}] {
			return fmt.Errorf("volume %s is not attached to %s on this host; refusing to adopt it", v.Name, name)
		}
	}
	m := i.manager(logf)
	tpl := spec.TemplateConfig()
	ip, err := c.ContainerIPv4(ctx, name, 30*time.Second)
	if err != nil {
		return err
	}
	if tpl.HealthCheck.Path != "" {
		if err := m.healthGate(ctx, ip, tpl.HealthCheck); err != nil {
			return fmt.Errorf("%s is not healthy here, so its route was not published: %w", name, err)
		}
	}
	if err := i.recordSpec(ctx, name, spec, logf); err != nil {
		return err
	}
	if rec.MigratedTo != "" {
		if err := c.UnsetInstanceConfig(ctx, name, incus.MigratedToKey); err != nil {
			return err
		}
	}
	if spec.Domain != "" {
		if err := m.publishRoute(ctx, name, ip, spec.Domain, tpl); err != nil {
			return err
		}
	}
	m.log("==> [Instance] %s is adopted by this host\n", name)
	return nil
}
