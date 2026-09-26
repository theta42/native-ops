package incus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/theta42/native-ops/pkg/config"
)

// Ref returns the Incus "remote:name" reference. An empty remote means the
// default remote, so the bare name is returned.
func Ref(remote, name string) string {
	if remote == "" {
		return name
	}
	return remote + ":" + name
}

func checkRemote(remote string) error {
	if !ValidName(remote) {
		return fmt.Errorf("invalid remote name %q", remote)
	}
	return nil
}

// CaptureInstanceStateAt is CaptureInstanceState for an instance on a named
// remote (an empty remote reads from the default remote).
func (c *Client) CaptureInstanceStateAt(ctx context.Context, remote, name string) (*InstanceState, error) {
	if remote != "" {
		if err := checkRemote(remote); err != nil {
			return nil, err
		}
	}
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid instance name %q", name)
	}
	out, err := c.exec.Run(ctx, "incus config show "+ShQuote(Ref(remote, name)))
	if err != nil {
		return nil, fmt.Errorf("read config of %s: %w", Ref(remote, name), err)
	}
	return ParseInstanceState(name, out)
}

// InstanceStatuses maps every instance on a remote to its status ("Running",
// "Stopped", ...). It doubles as a reachability check for that remote.
func (c *Client) InstanceStatuses(ctx context.Context, remote string) (map[string]string, error) {
	if err := checkRemote(remote); err != nil {
		return nil, err
	}
	out, err := c.exec.Run(ctx, "incus list "+ShQuote(remote+":")+" --format json")
	if err != nil {
		return nil, fmt.Errorf("list instances on %s: %w", remote, err)
	}
	var list []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse instance list of %s: %w", remote, err)
	}
	res := make(map[string]string, len(list))
	for _, i := range list {
		res[i.Name] = i.Status
	}
	return res, nil
}

// CustomVolumes returns the names of the custom storage volumes in a pool on a
// remote. An unknown pool or an unreachable remote is an error.
func (c *Client) CustomVolumes(ctx context.Context, remote, pool string) (map[string]bool, error) {
	if err := checkRemote(remote); err != nil {
		return nil, err
	}
	if !ValidName(pool) {
		return nil, fmt.Errorf("invalid pool name %q", pool)
	}
	out, err := c.exec.Run(ctx, "incus storage volume list "+ShQuote(remote+":"+pool)+" --format json")
	if err != nil {
		return nil, fmt.Errorf("list volumes of pool %s on %s: %w", pool, remote, err)
	}
	var list []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse volume list of %s:%s: %w", remote, pool, err)
	}
	res := map[string]bool{}
	for _, v := range list {
		if v.Type == "custom" {
			res[v.Name] = true
		}
	}
	return res, nil
}

// SnapshotVolumeAt snapshots a custom volume on a named remote.
func (c *Client) SnapshotVolumeAt(ctx context.Context, remote, pool, volume, snapshot string) error {
	if err := checkRemote(remote); err != nil {
		return err
	}
	cmd := fmt.Sprintf("incus storage volume snapshot create %s %s %s", ShQuote(remote+":"+pool), ShQuote(volume), ShQuote(snapshot))
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("snapshot %s:%s/%s@%s: %w", remote, pool, volume, snapshot, err)
	}
	return nil
}

// StopInstance stops an instance and waits up to timeoutSec for a clean shutdown.
func (c *Client) StopInstance(ctx context.Context, remote, name string, timeoutSec int) error {
	if err := checkRemote(remote); err != nil {
		return err
	}
	cmd := "incus stop " + ShQuote(Ref(remote, name))
	if timeoutSec > 0 {
		cmd += fmt.Sprintf(" --timeout %d", timeoutSec)
	}
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("stop %s: %w", Ref(remote, name), err)
	}
	return nil
}

// StartInstance starts an instance.
func (c *Client) StartInstance(ctx context.Context, remote, name string) error {
	if err := checkRemote(remote); err != nil {
		return err
	}
	if _, err := c.exec.Run(ctx, "incus start "+ShQuote(Ref(remote, name))); err != nil {
		return fmt.Errorf("start %s: %w", Ref(remote, name), err)
	}
	return nil
}

// DeleteInstanceAt deletes a (stopped) instance on a remote. Errors are returned.
func (c *Client) DeleteInstanceAt(ctx context.Context, remote, name string) error {
	if err := checkRemote(remote); err != nil {
		return err
	}
	if _, err := c.exec.Run(ctx, "incus delete "+ShQuote(Ref(remote, name))); err != nil {
		return fmt.Errorf("delete %s: %w", Ref(remote, name), err)
	}
	return nil
}

// DeleteVolumeAt deletes a custom volume on a remote. Errors are returned.
func (c *Client) DeleteVolumeAt(ctx context.Context, remote, pool, volume string) error {
	if err := checkRemote(remote); err != nil {
		return err
	}
	cmd := fmt.Sprintf("incus storage volume delete %s %s", ShQuote(remote+":"+pool), ShQuote(volume))
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("delete volume %s:%s/%s: %w", remote, pool, volume, err)
	}
	return nil
}

// CopyVolume copies a custom volume between remotes without its snapshots
// (snapshots such as daily-* would otherwise be dragged across the WAN).
// refresh makes it an incremental copy onto an existing volume.
func (c *Client) CopyVolume(ctx context.Context, srcRemote, dstRemote, pool, volume string, refresh bool) error {
	if err := checkRemote(srcRemote); err != nil {
		return err
	}
	if err := checkRemote(dstRemote); err != nil {
		return err
	}
	cmd := fmt.Sprintf("incus storage volume copy %s %s --volume-only",
		ShQuote(srcRemote+":"+pool+"/"+volume), ShQuote(dstRemote+":"+pool+"/"+volume))
	if refresh {
		cmd += " --refresh"
	}
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("copy volume %s/%s from %s to %s: %w", pool, volume, srcRemote, dstRemote, err)
	}
	return nil
}

// CopyInstance copies an instance (config and root filesystem, no snapshots)
// between remotes in push mode: the source host pushes to the target directly,
// so the two hosts must be able to reach each other (e.g. over WireGuard).
// The copy is created stopped. refresh makes it incremental onto an existing copy.
func (c *Client) CopyInstance(ctx context.Context, srcRemote, dstRemote, name string, refresh bool) error {
	if err := checkRemote(srcRemote); err != nil {
		return err
	}
	if err := checkRemote(dstRemote); err != nil {
		return err
	}
	cmd := fmt.Sprintf("incus copy %s %s --mode=push --instance-only",
		ShQuote(Ref(srcRemote, name)), ShQuote(Ref(dstRemote, name)))
	if refresh {
		cmd += " --refresh"
	}
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("copy instance %s from %s to %s: %w", name, srcRemote, dstRemote, err)
	}
	return nil
}

// probeScript runs inside the container: curl or wget, whichever exists, and
// a distinct message + exit 127 when neither does, so that is never retried.
const probeScript = `if command -v curl >/dev/null 2>&1; then curl -fsS -o /dev/null --max-time 3 "$1"; ` +
	`elif command -v wget >/dev/null 2>&1; then wget -q -O /dev/null -T 3 "$1"; ` +
	`else echo "no curl or wget in the container" >&2; exit 127; fi`

// ProbeHTTPInContainer makes one HTTP request to 127.0.0.1 from inside the
// container, so it works no matter which host or network the container is on.
// Any status of 400 or above is a failure.
func (c *Client) ProbeHTTPInContainer(ctx context.Context, remote, name string, port int, path string) error {
	if err := checkRemote(remote); err != nil {
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	cmd := fmt.Sprintf("incus exec %s -- sh -c %s probe %s", ShQuote(Ref(remote, name)), ShQuote(probeScript), ShQuote(url))
	_, err := c.exec.Run(ctx, cmd)
	return err
}

// WaitHTTPInContainer polls ProbeHTTPInContainer until it succeeds or the
// health check's timeout passes. It is a no-op when no path is configured.
func (c *Client) WaitHTTPInContainer(ctx context.Context, remote, name string, hc config.HealthCheckConfig) error {
	if hc.Path == "" {
		return nil
	}
	port := hc.Port
	if port <= 0 {
		port = 80
	}
	timeout := hc.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	interval := hc.Interval
	if interval <= 0 {
		interval = 2
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for {
		err := c.ProbeHTTPInContainer(ctx, remote, name, port, hc.Path)
		if err == nil {
			return nil
		}
		if strings.Contains(err.Error(), "no curl or wget") {
			return fmt.Errorf("cannot health check %s: %w", Ref(remote, name), err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("health check failed: %s:%d%s not OK within %ds: %w", Ref(remote, name), port, hc.Path, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(interval) * time.Second):
		}
	}
}
