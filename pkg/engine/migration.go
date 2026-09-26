package engine

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

// MigrationManager orchestrates cross-host workload transfers.
type MigrationManager struct {
	exec  remote.Executor
	incus *incus.Client

	// healthProbe is a field so tests can stub the wait. The default probes
	// from inside the container, so it works whatever host the container is on.
	healthProbe func(ctx context.Context, remote, name string, hc config.HealthCheckConfig) error
}

func NewMigrationManager(exec remote.Executor) *MigrationManager {
	ic := incus.NewClient(exec)
	return &MigrationManager{exec: exec, incus: ic, healthProbe: ic.WaitHTTPInContainer}
}

type MigrationParams struct {
	SourceRemote string // e.g. "droplet-nyc1-01"
	TargetRemote string // e.g. "pve-node-02"
	InstanceName string // e.g. "rest-acme"

	// VolumeName is optional. The volumes to move are read from the instance's
	// own disk devices; if set, this must be one of them (a typo guard).
	VolumeName string

	// HealthCheck, when Path is set, gates the migration: the target must
	// answer it (from inside the container) or the source is put back.
	HealthCheck config.HealthCheckConfig

	// Resume allows the target to already hold a stopped copy (an interrupted
	// earlier run, or the old source when rolling back), which is then updated
	// incrementally. Without it an existing target copy is an error, so a
	// migration can never overwrite an unrelated instance or volume.
	Resume bool

	// SkipSnapshot opts out of the pre-migrate snapshot of the source volumes.
	SkipSnapshot bool

	// StopTimeout is how long to wait for the source to shut down cleanly (default 30s).
	StopTimeout int
}

var healthPathRe = regexp.MustCompile(`^/[^\s'"\\]*$`)

func validateMigrationArgs(src, dst, name, vol string, hc config.HealthCheckConfig) error {
	for _, f := range []struct{ what, v string }{{"source remote", src}, {"target remote", dst}, {"instance name", name}} {
		if !incus.ValidName(f.v) {
			return fmt.Errorf("invalid %s %q", f.what, f.v)
		}
	}
	if src == dst {
		return fmt.Errorf("source and target are the same remote (%s)", src)
	}
	if vol != "" && !incus.ValidName(vol) {
		return fmt.Errorf("invalid volume name %q", vol)
	}
	if hc.Path != "" && !healthPathRe.MatchString(hc.Path) {
		return fmt.Errorf("invalid health check path %q", hc.Path)
	}
	if hc.Port < 0 || hc.Port > 65535 {
		return fmt.Errorf("invalid health check port %d", hc.Port)
	}
	return nil
}

// migratableVolumes returns the custom volumes to move and refuses an
// instance that mounts a host path, which a copy would silently leave behind.
func migratableVolumes(st *incus.InstanceState) ([]incus.VolumeRef, error) {
	for _, dn := range st.DeviceNames() {
		d := st.Devices[dn]
		if d["type"] == "disk" && d["pool"] == "" && d["source"] != "" {
			return nil, fmt.Errorf("device %q mounts the host path %s, which cannot be migrated; move that data to a storage volume first", dn, d["source"])
		}
	}
	vols := st.Volumes()
	for _, v := range vols {
		if !incus.ValidName(v.Pool) || !incus.ValidName(v.Name) {
			return nil, fmt.Errorf("volume %s/%s has a name that is not safe to migrate", v.Pool, v.Name)
		}
	}
	return vols, nil
}

type volKey struct{ pool, name string }

// Migrate moves an instance and its data volumes from one Incus remote to
// another with as little downtime and as little risk as a copy allows:
//
//  1. Preflight (read-only): both remotes reachable, the source instance and
//     its volumes identified, and nothing on the target that would be overwritten.
//  2. Snapshot the source volumes (abort before any change if that fails).
//  3. Warm copy while the source is still serving, so the downtime window only
//     covers the changes since then.
//  4. Stop the source and verify it is really stopped.
//  5. Final incremental copy, start the target, and health-check it from inside.
//
// If any step after the stop fails, the target is stopped and the source is
// started again, so a failed migration leaves the service running where it was.
// The source is never deleted here: it stays, stopped and intact, so the
// operator can move DNS/edge routing, watch the target, and either Finalize
// or roll back (run Migrate the other way with Resume).
func (m *MigrationManager) Migrate(ctx context.Context, p MigrationParams) error {
	src, dst, name := p.SourceRemote, p.TargetRemote, p.InstanceName
	if err := validateMigrationArgs(src, dst, name, p.VolumeName, p.HealthCheck); err != nil {
		return err
	}
	log.Printf("==> [Migration] Moving %s from %s to %s...\n", name, src, dst)

	// 1. Preflight. Nothing is changed.
	srcInst, err := m.incus.InstanceStatuses(ctx, src)
	if err != nil {
		return err
	}
	srcStatus, ok := srcInst[name]
	if !ok {
		return fmt.Errorf("instance %s not found on %s", name, src)
	}
	dstInst, err := m.incus.InstanceStatuses(ctx, dst)
	if err != nil {
		return err
	}
	st, err := m.incus.CaptureInstanceStateAt(ctx, src, name)
	if err != nil {
		return err
	}
	vols, err := migratableVolumes(st)
	if err != nil {
		return err
	}
	if p.VolumeName != "" {
		found := false
		for _, v := range vols {
			found = found || v.Name == p.VolumeName
		}
		if !found {
			return fmt.Errorf("volume %q is not attached to %s (attached: %s)", p.VolumeName, name, volumeNames(vols))
		}
	}

	dstInstExists := false
	if s, exists := dstInst[name]; exists {
		if !p.Resume {
			return fmt.Errorf("%s already exists on %s; if it is a copy from an earlier migration of this instance, rerun with --resume", name, dst)
		}
		if s != "Stopped" {
			return fmt.Errorf("%s on %s is %s; refusing to overwrite a running instance", name, dst, s)
		}
		dstInstExists = true
	}
	dstVolExists := map[volKey]bool{}
	pools := map[string]map[string]bool{}
	for _, v := range vols {
		existing, seen := pools[v.Pool]
		if !seen {
			if existing, err = m.incus.CustomVolumes(ctx, dst, v.Pool); err != nil {
				return fmt.Errorf("target %s cannot receive volume %s/%s: %w", dst, v.Pool, v.Name, err)
			}
			pools[v.Pool] = existing
		}
		if existing[v.Name] {
			if !p.Resume {
				return fmt.Errorf("volume %s/%s already exists on %s; if it is a copy from an earlier migration, rerun with --resume", v.Pool, v.Name, dst)
			}
			dstVolExists[volKey{v.Pool, v.Name}] = true
		}
	}

	// 2. Snapshot the source volumes; abort untouched on failure.
	if !p.SkipSnapshot {
		snap := fmt.Sprintf("pre-migrate-%s", time.Now().UTC().Format("20060102-150405"))
		for _, v := range vols {
			log.Printf("    Snapshotting %s:%s/%s (%s)...\n", src, v.Pool, v.Name, snap)
			if err := m.incus.SnapshotVolumeAt(ctx, src, v.Pool, v.Name, snap); err != nil {
				return fmt.Errorf("pre-migrate snapshot failed, nothing was changed: %w", err)
			}
		}
	}

	copyAll := func() error {
		for _, v := range vols {
			k := volKey{v.Pool, v.Name}
			if err := m.incus.CopyVolume(ctx, src, dst, v.Pool, v.Name, dstVolExists[k]); err != nil {
				return err
			}
			dstVolExists[k] = true
		}
		if err := m.incus.CopyInstance(ctx, src, dst, name, dstInstExists); err != nil {
			return err
		}
		dstInstExists = true
		return nil
	}

	// 3. Warm copy while the source is still serving. A failure here changes nothing on the source.
	wasRunning := srcStatus == "Running"
	if wasRunning {
		log.Printf("    Warm copy to %s while %s keeps serving...\n", dst, name)
		if err := copyAll(); err != nil {
			return fmt.Errorf("warm copy failed, the source was not touched (rerun with --resume to continue): %w", err)
		}
	}

	// 4. Quiesce the source and verify it, so no live database is ever copied.
	if wasRunning {
		log.Printf("    Stopping %s on %s...\n", name, src)
		stopErr := m.incus.StopInstance(ctx, src, name, p.StopTimeout)
		after, verr := m.incus.InstanceStatuses(ctx, src)
		if verr != nil || after[name] != "Stopped" {
			return fmt.Errorf("source %s did not stop (stop: %v; status check: %v); it was left as it was and no data was moved off it", name, stopErr, verr)
		}
	}

	targetStarted := false
	rollback := func(stage string, cause error) error {
		base := fmt.Errorf("%s failed: %w", stage, cause)
		if targetStarted {
			if err := m.incus.StopInstance(ctx, dst, name, p.StopTimeout); err != nil {
				log.Printf("    WARNING: could not stop the target copy on %s: %v\n", dst, err)
			}
		}
		if !wasRunning {
			return fmt.Errorf("%w; the source %s on %s was already stopped and is left stopped", base, name, src)
		}
		if err := m.incus.StartInstance(ctx, src, name); err != nil {
			return fmt.Errorf("%w; rollback ALSO failed to restart the source %s on %s (its data is intact, start it manually): %v", base, name, src, err)
		}
		return fmt.Errorf("%w; the source %s on %s was restarted and nothing was lost", base, name, src)
	}

	// 5. Final incremental copy, start, verify.
	log.Printf("    Final sync to %s...\n", dst)
	if err := copyAll(); err != nil {
		return rollback("final sync", err)
	}
	log.Printf("    Starting %s on %s...\n", name, dst)
	if err := m.incus.StartInstance(ctx, dst, name); err != nil {
		return rollback("start on target", err)
	}
	targetStarted = true
	if p.HealthCheck.Path != "" {
		log.Printf("    Health check %s on %s...\n", p.HealthCheck.Path, dst)
		if err := m.healthProbe(ctx, dst, name, p.HealthCheck); err != nil {
			return rollback("health check on target", err)
		}
	}

	log.Printf("==> [Migration] %s is running on %s. %s on %s is stopped and intact.\n", name, dst, name, src)
	log.Printf("    Next: point DNS / edge routing at %s and watch it. Then run `instance migrate --finalize` to delete the source.\n", dst)
	log.Printf("    To roll back instead: `instance migrate --source %s --target %s --name %s --resume` (copies changes back).\n", dst, src, name)
	return nil
}

// FinalizeParams identifies a completed migration to finish.
type FinalizeParams struct {
	SourceRemote string
	TargetRemote string
	InstanceName string

	// HealthCheck, when Path is set, must pass on the target before anything is deleted.
	HealthCheck config.HealthCheckConfig

	// PurgeSourceVolumes also deletes the source's data volumes. By default they
	// are kept (unattached), because they are the last copy taken before the move.
	PurgeSourceVolumes bool
}

// Finalize deletes the stopped source instance after a migration, once the
// target is verified: the target must be running, hold every data volume, and
// pass the health check; the source must be stopped. Any doubt refuses.
func (m *MigrationManager) Finalize(ctx context.Context, p FinalizeParams) error {
	src, dst, name := p.SourceRemote, p.TargetRemote, p.InstanceName
	if err := validateMigrationArgs(src, dst, name, "", p.HealthCheck); err != nil {
		return err
	}
	log.Printf("==> [Migration] Finalizing: removing %s from %s (running on %s)...\n", name, src, dst)

	srcInst, err := m.incus.InstanceStatuses(ctx, src)
	if err != nil {
		return err
	}
	s, ok := srcInst[name]
	if !ok {
		return fmt.Errorf("instance %s not found on %s (already finalized?)", name, src)
	}
	if s != "Stopped" {
		return fmt.Errorf("%s on %s is %s; refusing to delete a live instance", name, src, s)
	}
	dstInst, err := m.incus.InstanceStatuses(ctx, dst)
	if err != nil {
		return err
	}
	if dstInst[name] != "Running" {
		return fmt.Errorf("%s on %s is %q, not Running; refusing to delete the source", name, dst, dstInst[name])
	}
	st, err := m.incus.CaptureInstanceStateAt(ctx, src, name)
	if err != nil {
		return err
	}
	vols, err := migratableVolumes(st)
	if err != nil {
		return err
	}
	pools := map[string]map[string]bool{}
	for _, v := range vols {
		if _, seen := pools[v.Pool]; !seen {
			if pools[v.Pool], err = m.incus.CustomVolumes(ctx, dst, v.Pool); err != nil {
				return err
			}
		}
		if !pools[v.Pool][v.Name] {
			return fmt.Errorf("volume %s/%s is missing on %s; refusing to delete the source", v.Pool, v.Name, dst)
		}
	}
	if p.HealthCheck.Path != "" {
		if err := m.healthProbe(ctx, dst, name, p.HealthCheck); err != nil {
			return fmt.Errorf("target is not healthy, refusing to delete the source: %w", err)
		}
	}

	if err := m.incus.DeleteInstanceAt(ctx, src, name); err != nil {
		return err
	}
	if p.PurgeSourceVolumes {
		for _, v := range vols {
			if err := m.incus.DeleteVolumeAt(ctx, src, v.Pool, v.Name); err != nil {
				return err
			}
		}
		log.Printf("==> [Migration] Finalized: %s and its volumes were removed from %s.\n", name, src)
		return nil
	}
	log.Printf("==> [Migration] Finalized: %s was removed from %s. Its data volumes were kept: %s (delete them when you no longer need the rollback copy).\n", name, src, volumeNames(vols))
	return nil
}

func volumeNames(vols []incus.VolumeRef) string {
	names := make([]string, 0, len(vols))
	for _, v := range vols {
		names = append(names, v.Pool+"/"+v.Name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
