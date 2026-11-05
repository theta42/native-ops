package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
)

// ChangeSetSnapshots is a plan change to a volume's scheduled-snapshot config.
const ChangeSetSnapshots = "set-snapshots"

// Local snapshot schedules are Incus' own: a custom volume's snapshots.schedule, snapshots.expiry and
// snapshots.pattern make Incus take and expire the snapshots itself, so the host needs no timer. A volume
// gets its own `snapshots:` policy, else fleet.yml's `snapshots:` default; with neither, its config is
// left exactly as it is. Plan and apply decide with the same function (volumeSnapshotDrift).

// volumeSnapshotDrift is the snapshot keys whose live value differs from the policy, with the value each
// should have ("" means unset). A nil policy drifts nowhere.
func volumeSnapshotDrift(live map[string]string, policy *config.SnapshotPolicy) map[string]string {
	if policy == nil {
		return nil
	}
	want := policy.VolumeConfig()
	drift := map[string]string{}
	for _, k := range config.SnapshotKeys {
		if live[k] != want[k] {
			drift[k] = want[k]
		}
	}
	return drift
}

// fleetSnapshotDefault is fleet.yml's `snapshots:` default, or nil (none, or no readable fleet.yml).
func fleetSnapshotDefault(configDir string) *config.SnapshotPolicy {
	fleet, err := config.LoadFleetConfig(configDir)
	if err != nil {
		return nil
	}
	return fleet.Snapshots
}

// planVolumeSnapshots adds a change for every volume whose snapshot config differs from its policy. A
// volume that does not exist yet (it is created first) has no config.
func (d *Deployer) planVolumeSnapshots(ctx context.Context, p *ServicePlan, vols []config.VolumeMount, configDir string) error {
	def := fleetSnapshotDefault(configDir)
	for _, vol := range vols {
		pol := config.SnapshotPolicyFor(vol, def)
		if pol == nil {
			continue
		}
		live, _, err := d.incus.VolumeConfig(ctx, vol.Pool, vol.Name)
		if err != nil {
			return err
		}
		if drift := volumeSnapshotDrift(live, pol); len(drift) > 0 {
			var parts []string
			for _, k := range config.SnapshotKeys {
				if v, ok := drift[k]; ok {
					parts = append(parts, fmt.Sprintf("%s: %s -> %s", k, orNone(live[k]), orNone(v)))
				}
			}
			p.add(ChangeSetSnapshots, "%s: %s", vol.Name, strings.Join(parts, ", "))
		}
	}
	return nil
}

// ensureVolumeSnapshots makes every volume's snapshot config what its policy says. It reports whether it
// changed anything; a volume that already matches is not touched.
func (d *Deployer) ensureVolumeSnapshots(ctx context.Context, vols []config.VolumeMount, configDir string) (bool, error) {
	def := fleetSnapshotDefault(configDir)
	changed := false
	for _, vol := range vols {
		c, err := ensureSnapshotPolicy(ctx, d.incus, vol.Pool, vol.Name, config.SnapshotPolicyFor(vol, def), d.log)
		if err != nil {
			return changed, err
		}
		changed = changed || c
	}
	return changed, nil
}

// ensureSnapshotPolicy sets one volume's snapshot keys to the policy (nil: leave them alone).
func ensureSnapshotPolicy(ctx context.Context, c *incus.Client, pool, volume string, pol *config.SnapshotPolicy, logf func(string, ...any)) (bool, error) {
	if pol == nil {
		return false, nil
	}
	live, exists, err := c.VolumeConfig(ctx, pool, volume)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, fmt.Errorf("volume %s does not exist, so its snapshot schedule cannot be set", volume)
	}
	drift := volumeSnapshotDrift(live, pol)
	changed := false
	for _, k := range config.SnapshotKeys {
		v, ok := drift[k]
		if !ok {
			continue
		}
		logf("    Volume %s: %s %s -> %s\n", volume, k, orNone(live[k]), orNone(v))
		if err := c.SetVolumeConfig(ctx, pool, volume, k, v); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}
