package incus

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ShQuote single-quotes s so it is passed to a POSIX shell as one literal word.
func ShQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidName reports whether s is safe to use as an instance, volume or unit name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// VolumeRef identifies a custom storage volume attached to an instance.
type VolumeRef struct{ Pool, Name string }

// InstanceState is everything an immutable replace must carry over from the
// running container so that only the image changes: profiles, local config,
// local devices (data volumes, proxies, NIC overrides) and the image it was
// launched from (the rollback target).
type InstanceState struct {
	Name      string
	Profiles  []string
	Config    map[string]string            // local config, minus volatile.* and image.*
	Devices   map[string]map[string]string // local devices, each including its "type"
	BaseImage string                       // fingerprint of the image the container was launched from
}

type configShow struct {
	Config   map[string]string            `yaml:"config"`
	Devices  map[string]map[string]string `yaml:"devices"`
	Profiles []string                     `yaml:"profiles"`
}

// ParseInstanceState parses the YAML printed by `incus config show <name>`.
func ParseInstanceState(name, yamlText string) (*InstanceState, error) {
	var cs configShow
	if err := yaml.Unmarshal([]byte(yamlText), &cs); err != nil {
		return nil, fmt.Errorf("parse `incus config show %s`: %w", name, err)
	}
	st := &InstanceState{
		Name:      name,
		Profiles:  cs.Profiles,
		Config:    map[string]string{},
		Devices:   cs.Devices,
		BaseImage: cs.Config["volatile.base_image"],
	}
	if st.Devices == nil {
		st.Devices = map[string]map[string]string{}
	}
	for k, v := range cs.Config {
		// volatile.* is runtime bookkeeping and image.* describes the OLD image; neither may be replayed.
		if strings.HasPrefix(k, "volatile.") || strings.HasPrefix(k, "image.") {
			continue
		}
		st.Config[k] = v
	}
	return st, nil
}

// DeviceNames returns the local device names in a stable order.
func (s *InstanceState) DeviceNames() []string {
	names := make([]string, 0, len(s.Devices))
	for n := range s.Devices {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Volumes returns the custom storage volumes attached as local disk devices —
// the persistent data that must be snapshotted before a replace.
func (s *InstanceState) Volumes() []VolumeRef {
	var out []VolumeRef
	for _, n := range s.DeviceNames() {
		d := s.Devices[n]
		if d["type"] == "disk" && d["pool"] != "" && d["source"] != "" {
			out = append(out, VolumeRef{Pool: d["pool"], Name: d["source"]})
		}
	}
	return out
}

// CaptureInstanceState reads the live configuration of a container.
func (c *Client) CaptureInstanceState(ctx context.Context, name string) (*InstanceState, error) {
	return c.CaptureInstanceStateAt(ctx, "", name)
}

func isMissingFile(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "not found") || strings.Contains(m, "does not exist") || strings.Contains(m, "no such file")
}

// PullFile reads a file out of a container (running or stopped). found is
// false only when the file genuinely does not exist; any other failure is an
// error, so a transient problem can never be mistaken for "nothing to carry over".
func (c *Client) PullFile(ctx context.Context, container, path string) (content string, found bool, err error) {
	out, err := c.exec.Run(ctx, "incus file pull "+ShQuote(container+path)+" -")
	if err != nil {
		if isMissingFile(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s from %s: %w", path, container, err)
	}
	return out, true, nil
}

// PushFile writes content to a path inside a container with the given octal mode, byte for byte.
func (c *Client) PushFile(ctx context.Context, container, path, content, mode string) error {
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	cmd := fmt.Sprintf("printf %%s %s | base64 -d | incus file push --create-dirs --uid 0 --gid 0 --mode %s - %s",
		ShQuote(b64), ShQuote(mode), ShQuote(container+path))
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("write %s in %s: %w", path, container, err)
	}
	return nil
}

// SetDevice adds a local device, or overrides it when a profile already provides one of that name.
func (c *Client) SetDevice(ctx context.Context, container, device string, props map[string]string) error {
	typ := props["type"]
	if typ == "" {
		return fmt.Errorf("device %s has no type", device)
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		if k != "type" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	kv := make([]string, 0, len(keys))
	for _, k := range keys {
		kv = append(kv, ShQuote(k+"="+props[k]))
	}
	add := fmt.Sprintf("incus config device add %s %s %s %s", ShQuote(container), ShQuote(device), ShQuote(typ), strings.Join(kv, " "))
	if _, err := c.exec.Run(ctx, add); err == nil {
		return nil
	} else if !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return fmt.Errorf("add device %s to %s: %w", device, container, err)
	}
	override := fmt.Sprintf("incus config device override %s %s %s", ShQuote(container), ShQuote(device), strings.Join(kv, " "))
	if _, err := c.exec.Run(ctx, override); err != nil {
		return fmt.Errorf("override device %s on %s: %w", device, container, err)
	}
	return nil
}

// GlobalIPv4s returns every global IPv4 address of a container, sorted. It
// makes one query and has no side effects (unlike GetContainerIP).
func (c *Client) GlobalIPv4s(ctx context.Context, name string) ([]string, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid instance name %q", name)
	}
	out, err := c.exec.Run(ctx, "incus list "+ShQuote(name)+" --format json")
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", name, err)
	}
	var list []struct {
		Name  string `json:"name"`
		State *struct {
			Network map[string]struct {
				Addresses []struct{ Family, Address, Scope string } `json:"addresses"`
			} `json:"network"`
		} `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse list of %s: %w", name, err)
	}
	var ips []string
	for _, inst := range list {
		if inst.Name != name || inst.State == nil {
			continue
		}
		for net, info := range inst.State.Network {
			if net == "lo" {
				continue
			}
			for _, a := range info.Addresses {
				if a.Family == "inet" && a.Scope == "global" && a.Address != "" {
					ips = append(ips, a.Address)
				}
			}
		}
	}
	sort.Strings(ips)
	return ips, nil
}

// ContainerIPv4 polls until the container has a global IPv4 address. It has no side effects.
func (c *Client) ContainerIPv4(ctx context.Context, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		if ips, err := c.GlobalIPv4s(ctx, name); err == nil && len(ips) > 0 {
			return ips[0], nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no IPv4 address for %s within %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ResolveImage turns an image reference into something `incus launch` can use
// unambiguously: an explicit remote (images:, docker:, local:) or a fingerprint
// passes through; a local alias such as "app:v2" is resolved to its fingerprint
// (an unresolved alias is an error, never silently re-read as a Docker Hub image).
func (c *Client) ResolveImage(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("image reference is empty")
	}
	for _, p := range []string{"images:", "docker:", "local:", "quay:"} {
		if strings.HasPrefix(ref, p) {
			return ref, nil
		}
	}
	if regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(ref) {
		return ref, nil
	}
	fp, err := c.ResolveImageFingerprint(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("image %q not found locally (build or import it first): %w", ref, err)
	}
	return fp, nil
}
