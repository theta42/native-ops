package incus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// CreateVolumeSnapshot creates a named snapshot of a custom volume.
func (c *Client) CreateVolumeSnapshot(ctx context.Context, pool, volume, snapshot string) error {
	if pool == "" {
		pool = "default"
	}
	_, err := c.exec.Run(ctx, fmt.Sprintf("incus storage volume snapshot create %s %s %s", ShQuote(pool), ShQuote(volume), ShQuote(snapshot)))
	if err != nil {
		return fmt.Errorf("snapshot %s/%s@%s: %w", pool, volume, snapshot, err)
	}
	return nil
}

// DeleteVolumeSnapshot removes a named snapshot.
func (c *Client) DeleteVolumeSnapshot(ctx context.Context, pool, volume, snapshot string) error {
	if pool == "" {
		pool = "default"
	}
	_, err := c.exec.Run(ctx, fmt.Sprintf("incus storage volume snapshot delete %s %s %s", pool, volume, snapshot))
	if err != nil {
		return fmt.Errorf("delete snapshot %s/%s@%s: %w", pool, volume, snapshot, err)
	}
	return nil
}

// ExportVolumeSnapshot writes a custom volume (or one of its snapshots, via
// "volume/snapshot") to a compressed backup file. --volume-only excludes any
// other snapshots so the artifact is a single clean filesystem.
func (c *Client) ExportVolumeSnapshot(ctx context.Context, pool, volumeRef, targetFile string) error {
	if pool == "" {
		pool = "default"
	}
	_, err := c.exec.Run(ctx, fmt.Sprintf(
		"incus storage volume export %s %s %s --volume-only", pool, volumeRef, targetFile))
	if err != nil {
		return fmt.Errorf("export %s/%s: %w", pool, volumeRef, err)
	}
	return nil
}

// ImportVolume creates a custom volume named newName from a backup file.
func (c *Client) ImportVolume(ctx context.Context, pool, backupFile, newName string) error {
	if pool == "" {
		pool = "default"
	}
	_, err := c.exec.Run(ctx, fmt.Sprintf(
		"incus storage volume import %s %s %s", pool, backupFile, newName))
	if err != nil {
		return fmt.Errorf("import %s as %s: %w", backupFile, newName, err)
	}
	return nil
}

// DeleteVolume removes a custom volume.
func (c *Client) DeleteVolume(ctx context.Context, pool, volume string) error {
	if pool == "" {
		pool = "default"
	}
	_, err := c.exec.Run(ctx, fmt.Sprintf("incus storage volume delete %s %s", pool, volume))
	if err != nil {
		return fmt.Errorf("delete volume %s/%s: %w", pool, volume, err)
	}
	return nil
}

// ListCustomVolumes returns the names of custom volumes in a pool.
func (c *Client) ListCustomVolumes(ctx context.Context, pool string) ([]string, error) {
	if pool == "" {
		pool = "default"
	}
	out, err := c.exec.Run(ctx, fmt.Sprintf("incus storage volume list %s --format json", pool))
	if err != nil {
		return nil, fmt.Errorf("list volumes in %s: %w", pool, err)
	}
	var rows []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("parse volume list: %w", err)
	}
	var names []string
	for _, r := range rows {
		if r.Type == "custom" {
			names = append(names, r.Name)
		}
	}
	return names, nil
}

// VolumeDependents returns the running containers that mount a given custom
// volume as a disk device. Used to stop/start them around a restore.
func (c *Client) VolumeDependents(ctx context.Context, volume string) ([]string, error) {
	out, err := c.exec.Run(ctx, "incus list --format json")
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	var rows []struct {
		Name    string `json:"name"`
		Devices map[string]struct {
			Type   string `json:"type"`
			Source string `json:"source"`
		} `json:"devices"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("parse instance list: %w", err)
	}
	var names []string
	for _, r := range rows {
		for _, d := range r.Devices {
			if d.Type == "disk" && d.Source == volume {
				names = append(names, r.Name)
				break
			}
		}
	}
	return names, nil
}

// StopContainer stops an instance, ignoring an already-stopped state.
func (c *Client) StopContainer(ctx context.Context, name string) error {
	_, err := c.exec.Run(ctx, fmt.Sprintf("incus stop %s", name))
	if err != nil && !strings.Contains(err.Error(), "already stopped") {
		return fmt.Errorf("stop %s: %w", name, err)
	}
	return nil
}

// StartContainer starts an instance, ignoring an already-running state.
func (c *Client) StartContainer(ctx context.Context, name string) error {
	_, err := c.exec.Run(ctx, fmt.Sprintf("incus start %s", name))
	if err != nil && !strings.Contains(err.Error(), "already running") {
		return fmt.Errorf("start %s: %w", name, err)
	}
	return nil
}
