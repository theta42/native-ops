package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

// Preview is an ephemeral instance built from a template at a git ref, stamped
// with user.preview.* config so it can be listed and garbage-collected. It uses
// the same template/volume/route/healthcheck path as a normal instance — the
// only difference is lifecycle (TTL) and naming (derived from app+ref).
type Preview struct {
	Name    string
	App     string
	Ref     string
	Expires time.Time
	Running bool
}

// PreviewParams controls how a preview is created.
type PreviewParams struct {
	ConfigDir string
	App       string
	Ref       string
	TTL       time.Duration // default 72h
	Env       map[string]string
	Limits    map[string]string
}

// PreviewManager drives the ephemeral preview lifecycle.
type PreviewManager struct {
	instances *InstanceManager
	incus     *incus.Client
}

func NewPreviewManager(exec remote.Executor) *PreviewManager {
	return &PreviewManager{instances: NewInstanceManager(exec), incus: incus.NewClient(exec)}
}

// defaultPreviewTTL is used when no TTL is given.
const defaultPreviewTTL = 72 * time.Hour

// PreviewName derives a deterministic, Incus-safe instance name from app+ref.
// e.g. ("platform", "feat/savy") -> "preview-platform-feat-savy".
func PreviewName(app, ref string) string {
	raw := strings.ToLower("preview-" + app + "-" + ref)
	var b strings.Builder
	dash := false
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "preview"
	}
	return s
}

// Launch a preview from the app's template and stamp it for GC.
func (m *PreviewManager) Launch(ctx context.Context, p PreviewParams) (ip, name string, err error) {
	if p.App == "" || p.Ref == "" {
		return "", "", fmt.Errorf("preview requires an app and a ref")
	}
	tmplPath := filepath.Join(p.ConfigDir, "templates", p.App, "template.yml")
	tmpl, err := config.LoadTemplateConfig(tmplPath)
	if err != nil {
		return "", "", fmt.Errorf("load template %s: %w", p.App, err)
	}
	name = PreviewName(p.App, p.Ref)
	if m.incus.ContainerExists(ctx, name) {
		return "", name, fmt.Errorf("preview %s already exists (update it, or destroy first)", name)
	}
	ip, err = m.instances.Launch(ctx, LaunchParams{
		Template: tmpl,
		Name:     name,
		Slug:     name,
		Env:      p.Env,
		Limits:   p.Limits,
	})
	if err != nil {
		return "", name, err
	}
	ttl := p.TTL
	if ttl <= 0 {
		ttl = defaultPreviewTTL
	}
	expires := time.Now().UTC().Add(ttl).Format(time.RFC3339)
	_ = m.incus.SetInstanceConfig(ctx, name, "user.preview.app", p.App)
	_ = m.incus.SetInstanceConfig(ctx, name, "user.preview.ref", p.Ref)
	_ = m.incus.SetInstanceConfig(ctx, name, "user.preview.expires", expires)
	return ip, name, nil
}

// List returns every instance carrying preview metadata.
func (m *PreviewManager) List(ctx context.Context) ([]Preview, error) {
	infos, err := m.incus.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	var out []Preview
	for _, i := range infos {
		app := i.Config["user.preview.app"]
		if app == "" {
			continue
		}
		pv := Preview{
			Name:    i.Name,
			App:     app,
			Ref:     i.Config["user.preview.ref"],
			Running: strings.EqualFold(i.Status, "running"),
		}
		if ts := i.Config["user.preview.expires"]; ts != "" {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				pv.Expires = t
			}
		}
		out = append(out, pv)
	}
	return out, nil
}

// Destroy tears down a preview, including its Caddy route and data volume.
func (m *PreviewManager) Destroy(ctx context.Context, name string) error {
	return m.instances.Destroy(ctx, name, true)
}

// Gc destroys every expired preview and returns their names.
func (m *PreviewManager) Gc(ctx context.Context, now time.Time) ([]string, error) {
	list, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, p := range expired(list, now) {
		if err := m.Destroy(ctx, p.Name); err != nil {
			return deleted, fmt.Errorf("destroy %s: %w", p.Name, err)
		}
		deleted = append(deleted, p.Name)
	}
	return deleted, nil
}

// expired returns previews whose TTL has passed (a zero Expires never expires).
func expired(list []Preview, now time.Time) []Preview {
	var out []Preview
	for _, p := range list {
		if !p.Expires.IsZero() && p.Expires.Before(now) {
			out = append(out, p)
		}
	}
	return out
}
