// Package status builds a read-only picture of one Incus host: its instances,
// data volumes and images, plus the things an operator would want flagged.
//
// It is the single source for `native-ops status` and for the daemon's
// GET /v1/status. It never reads or reports configuration VALUES other than
// resource limits and native-ops' own bookkeeping keys: OCI containers keep
// their secrets in `environment.*` and cloud-init data in `user.*`, so those
// are reported by key name only.
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

// Snapshot is one point-in-time view of a host.
type Snapshot struct {
	Time      time.Time    `json:"time"`
	Host      string       `json:"host,omitempty"`
	Incus     string       `json:"incus_version,omitempty"`
	Pool      string       `json:"pool"`
	Instances []Instance   `json:"instances"`
	Volumes   []Volume     `json:"volumes"`
	Images    ImageSummary `json:"images"`
	Warnings  []string     `json:"warnings,omitempty"`
}

type Device struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Pool     string `json:"pool,omitempty"`
	Source   string `json:"source,omitempty"`
	Path     string `json:"path,omitempty"`
	Listen   string `json:"listen,omitempty"`
	Connect  string `json:"connect,omitempty"`
	HostPath bool   `json:"host_path,omitempty"` // a disk device backed by a host path, not a storage volume
}

type Instance struct {
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	Type       string            `json:"type"`
	CreatedAt  string            `json:"created_at"`
	Image      string            `json:"image,omitempty"`      // description of the image it was created from
	BaseImage  string            `json:"base_image,omitempty"` // first 12 chars of its fingerprint
	Profiles   []string          `json:"profiles"`
	IPv4       []string          `json:"ipv4,omitempty"`
	Limits     map[string]string `json:"limits,omitempty"`
	Recorded   map[string]string `json:"recorded,omitempty"`    // user.native-ops.* bookkeeping (image reference, template)
	ConfigKeys []string          `json:"config_keys,omitempty"` // names only
	EnvKeys    []string          `json:"env_keys,omitempty"`    // names only, never values
	Devices    []Device          `json:"devices,omitempty"`
	Snapshots  int               `json:"snapshots"`
}

type Volume struct {
	Pool        string   `json:"pool"`
	Name        string   `json:"name"`
	UsedBy      []string `json:"used_by"`
	Shifted     string   `json:"security_shifted,omitempty"`
	Snapshots   int      `json:"snapshots"`
	LatestDaily string   `json:"latest_daily,omitempty"`
}

type Alias struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	UploadedAt  string `json:"uploaded_at"`
}

type ImageSummary struct {
	Count      int     `json:"count"`
	TotalBytes int64   `json:"total_bytes"`
	Unaliased  int     `json:"unaliased"`
	Aliases    []Alias `json:"aliases"`
}

// ---- parsing (pure) ----

type rawInstance struct {
	Name      string                       `json:"name"`
	Status    string                       `json:"status"`
	Type      string                       `json:"type"`
	CreatedAt string                       `json:"created_at"`
	Profiles  []string                     `json:"profiles"`
	Config    map[string]string            `json:"config"`
	Devices   map[string]map[string]string `json:"devices"`
	Snapshots []json.RawMessage            `json:"snapshots"`
	State     *struct {
		Network map[string]struct {
			Addresses []struct {
				Family  string `json:"family"`
				Address string `json:"address"`
				Scope   string `json:"scope"`
			} `json:"addresses"`
		} `json:"network"`
	} `json:"state"`
}

// ParseInstances parses `incus list --format json`.
func ParseInstances(out string) ([]Instance, error) {
	var raw []rawInstance
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse instance list: %w", err)
	}
	res := make([]Instance, 0, len(raw))
	for _, r := range raw {
		in := Instance{
			Name: r.Name, Status: r.Status, Type: r.Type, CreatedAt: r.CreatedAt,
			Profiles: r.Profiles, Snapshots: len(r.Snapshots),
			Image: r.Config["image.description"],
		}
		if fp := r.Config["volatile.base_image"]; fp != "" {
			in.BaseImage = fp[:min(12, len(fp))]
		}
		for k, v := range r.Config {
			switch {
			case strings.HasPrefix(k, "volatile."), strings.HasPrefix(k, "image."):
			case strings.HasPrefix(k, "environment."):
				in.EnvKeys = append(in.EnvKeys, strings.TrimPrefix(k, "environment."))
			case strings.HasPrefix(k, "limits."):
				if in.Limits == nil {
					in.Limits = map[string]string{}
				}
				in.Limits[k] = v
			case strings.HasPrefix(k, "user.native-ops."):
				if in.Recorded == nil {
					in.Recorded = map[string]string{}
				}
				in.Recorded[strings.TrimPrefix(k, "user.native-ops.")] = v
			default:
				in.ConfigKeys = append(in.ConfigKeys, k)
			}
		}
		sort.Strings(in.ConfigKeys)
		sort.Strings(in.EnvKeys)
		for name, d := range r.Devices {
			dev := Device{Name: name, Type: d["type"]}
			switch d["type"] {
			case "disk":
				dev.Pool, dev.Source, dev.Path = d["pool"], d["source"], d["path"]
				dev.HostPath = d["pool"] == "" && d["source"] != ""
			case "proxy":
				dev.Listen, dev.Connect = d["listen"], d["connect"]
			}
			in.Devices = append(in.Devices, dev)
		}
		sort.Slice(in.Devices, func(i, j int) bool { return in.Devices[i].Name < in.Devices[j].Name })
		if r.State != nil {
			for nic, n := range r.State.Network {
				if nic == "lo" {
					continue
				}
				for _, a := range n.Addresses {
					if a.Family == "inet" && a.Scope == "global" && a.Address != "" {
						in.IPv4 = append(in.IPv4, a.Address)
					}
				}
			}
			sort.Strings(in.IPv4)
		}
		res = append(res, in)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Name < res[j].Name })
	return res, nil
}

// ParseVolumes parses `incus storage volume list <pool> --format json`, keeping custom volumes.
func ParseVolumes(pool, out string) ([]Volume, error) {
	var raw []struct {
		Name   string            `json:"name"`
		Type   string            `json:"type"`
		UsedBy []string          `json:"used_by"`
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse volume list: %w", err)
	}
	var res []Volume
	for _, v := range raw {
		if v.Type != "custom" {
			continue
		}
		vol := Volume{Pool: pool, Name: v.Name, Shifted: v.Config["security.shifted"], UsedBy: []string{}}
		for _, u := range v.UsedBy {
			if i := strings.LastIndex(u, "/"); i >= 0 && strings.Contains(u, "/instances/") {
				vol.UsedBy = append(vol.UsedBy, strings.SplitN(u[i+1:], "?", 2)[0])
			}
		}
		sort.Strings(vol.UsedBy)
		res = append(res, vol)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Name < res[j].Name })
	return res, nil
}

// ParseSnapshotNames parses `incus storage volume snapshot list ... --format csv`
// (first column = snapshot name) and returns the count and the newest daily-* name.
func ParseSnapshotNames(csv string) (count int, latestDaily string) {
	for _, line := range strings.Split(csv, "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), ",")
		if name == "" {
			continue
		}
		count++
		if strings.HasPrefix(name, "daily-") && name > latestDaily {
			latestDaily = name
		}
	}
	return count, latestDaily
}

// ParseImages parses `incus image list --format json`.
func ParseImages(out string) (ImageSummary, error) {
	var raw []struct {
		Fingerprint string `json:"fingerprint"`
		Size        int64  `json:"size"`
		UploadedAt  string `json:"uploaded_at"`
		Aliases     []struct {
			Name string `json:"name"`
		} `json:"aliases"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return ImageSummary{}, fmt.Errorf("parse image list: %w", err)
	}
	sum := ImageSummary{Count: len(raw), Aliases: []Alias{}}
	for _, i := range raw {
		sum.TotalBytes += i.Size
		if len(i.Aliases) == 0 {
			sum.Unaliased++
		}
		for _, a := range i.Aliases {
			sum.Aliases = append(sum.Aliases, Alias{Name: a.Name, Fingerprint: i.Fingerprint[:min(12, len(i.Fingerprint))], UploadedAt: i.UploadedAt})
		}
	}
	sort.Slice(sum.Aliases, func(i, j int) bool { return sum.Aliases[i].Name < sum.Aliases[j].Name })
	return sum, nil
}

var dailyRe = regexp.MustCompile(`^daily-(\d{8})-(\d{6})$`)

// Analyze returns the observations worth an operator's attention.
func Analyze(s *Snapshot, now time.Time) []string {
	var w []string
	for _, in := range s.Instances {
		if in.Status != "Running" {
			w = append(w, fmt.Sprintf("instance %s is %s", in.Name, in.Status))
		}
		for _, d := range in.Devices {
			if d.HostPath {
				w = append(w, fmt.Sprintf("instance %s mounts the host path %s at %s: it is not a storage volume, so it is not snapshotted, backed up or migrated by volume tooling", in.Name, d.Source, d.Path))
			}
		}
	}
	for _, v := range s.Volumes {
		if v.Shifted != "true" && len(v.UsedBy) > 0 {
			w = append(w, fmt.Sprintf("volume %s does not have security.shifted=true", v.Name))
		}
		if v.LatestDaily == "" {
			w = append(w, fmt.Sprintf("volume %s has no daily snapshot", v.Name))
			continue
		}
		if m := dailyRe.FindStringSubmatch(v.LatestDaily); m != nil {
			if t, err := time.Parse("20060102150405", m[1]+m[2]); err == nil && now.Sub(t) > 36*time.Hour {
				w = append(w, fmt.Sprintf("volume %s: newest daily snapshot is %s old", v.Name, now.Sub(t).Round(time.Hour)))
			}
		}
	}
	if s.Images.Unaliased > 10 {
		w = append(w, fmt.Sprintf("%d unaliased images (%d MB in total across %d) could be pruned", s.Images.Unaliased, s.Images.TotalBytes/1_000_000, s.Images.Count))
	}
	return w
}

// Collect gathers a Snapshot through ex (a local shell, or SSH to the host).
// Only the instance list is required; the rest degrade to warnings.
func Collect(ctx context.Context, ex remote.Executor, pool string) (*Snapshot, error) {
	if !incus.ValidName(pool) {
		return nil, fmt.Errorf("invalid pool name %q", pool)
	}
	s := &Snapshot{Time: time.Now().UTC(), Pool: pool, Instances: []Instance{}, Volumes: []Volume{}, Images: ImageSummary{Aliases: []Alias{}}}
	if out, err := ex.Run(ctx, "hostname"); err == nil {
		s.Host = strings.TrimSpace(out)
	}
	if out, err := ex.Run(ctx, "incus --version"); err == nil {
		s.Incus = strings.TrimSpace(out)
	}

	out, err := ex.Run(ctx, "incus list --format json")
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	if s.Instances, err = ParseInstances(out); err != nil {
		return nil, err
	}

	if out, err := ex.Run(ctx, "incus storage volume list "+incus.ShQuote(pool)+" --format json"); err != nil {
		s.Warnings = append(s.Warnings, "could not list volumes: "+err.Error())
	} else if s.Volumes, err = ParseVolumes(pool, out); err != nil {
		s.Warnings = append(s.Warnings, err.Error())
	}
	for i := range s.Volumes {
		if !incus.ValidName(s.Volumes[i].Name) {
			continue
		}
		out, err := ex.Run(ctx, fmt.Sprintf("incus storage volume snapshot list %s %s --format csv", incus.ShQuote(pool), incus.ShQuote(s.Volumes[i].Name)))
		if err != nil {
			s.Warnings = append(s.Warnings, fmt.Sprintf("could not list snapshots of %s: %v", s.Volumes[i].Name, err))
			continue
		}
		s.Volumes[i].Snapshots, s.Volumes[i].LatestDaily = ParseSnapshotNames(out)
	}

	if out, err := ex.Run(ctx, "incus image list --format json"); err != nil {
		s.Warnings = append(s.Warnings, "could not list images: "+err.Error())
	} else if s.Images, err = ParseImages(out); err != nil {
		s.Warnings = append(s.Warnings, err.Error())
	}

	s.Warnings = append(s.Warnings, Analyze(s, s.Time)...)
	return s, nil
}
