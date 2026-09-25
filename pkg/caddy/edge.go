package caddy

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/remote"
)

// EdgeManager manages Caddy edge proxy routes inside the edge container.
type EdgeManager struct {
	exec          remote.Executor
	edgeContainer string
}

func NewEdgeManager(exec remote.Executor, edgeContainer string) *EdgeManager {
	if edgeContainer == "" {
		edgeContainer = "edge"
	}
	return &EdgeManager{
		exec:          exec,
		edgeContainer: edgeContainer,
	}
}

// RenderSiteBlock generates a Caddy site block.
func RenderSiteBlock(domain, upstreamIP string, upstreamPort int, tls string, extra []string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s {\n", domain))
	if tls != "" {
		sb.WriteString(fmt.Sprintf("    tls %s\n", tls))
	}
	sb.WriteString(fmt.Sprintf("    reverse_proxy %s:%d\n", upstreamIP, upstreamPort))
	for _, line := range extra {
		sb.WriteString(fmt.Sprintf("    %s\n", line))
	}
	sb.WriteString("}\n")
	return sb.String()
}

// EnsureBaseCaddyfile seeds a minimal Caddyfile ONLY when the edge has none.
// It must never overwrite an existing Caddyfile: the edge is shared and may be
// hand-maintained (per-site blocks, wildcard TLS matchers, landing routes), and
// clobbering it would take down every route and certificate behind it.
func (e *EdgeManager) EnsureBaseCaddyfile(ctx context.Context) error {
	if _, err := e.exec.Run(ctx, fmt.Sprintf("incus exec %s -- test -f /etc/caddy/Caddyfile", e.edgeContainer)); err == nil {
		return nil
	}
	baseConfig := "{\n    email wmantly@gmail.com\n    debug\n    log {\n        output file /var/log/caddy.log\n        level DEBUG\n    }\n}\n\nimport /etc/caddy/sites/*.caddy\n"
	b64 := base64.StdEncoding.EncodeToString([]byte(baseConfig))
	cmd := fmt.Sprintf("echo '%s' | base64 -d | incus exec %s -- sh -c 'mkdir -p /etc/caddy /etc/caddy/sites && cat > /etc/caddy/Caddyfile && chmod 644 /etc/caddy/Caddyfile'", b64, e.edgeContainer)
	_, err := e.exec.Run(ctx, cmd)
	return err
}

// PublishSite writes a .caddy file into /etc/caddy/sites/ and reloads Caddy.
func (e *EdgeManager) PublishSite(ctx context.Context, siteName string, routing config.RoutingConfig, upstreamIP string) error {
	_ = e.EnsureBaseCaddyfile(ctx)
	content := RenderSiteBlock(routing.Domain, upstreamIP, routing.UpstreamPort, routing.TLS, routing.ExtraDirectives)
	b64 := base64.StdEncoding.EncodeToString([]byte(content))

	sitePath := fmt.Sprintf("/etc/caddy/sites/%s.caddy", siteName)
	cmd := fmt.Sprintf("echo '%s' | base64 -d | incus exec %s -- sh -c 'mkdir -p /etc/caddy/sites && cat > %s && chmod 644 %s'",
		b64, e.edgeContainer, sitePath, sitePath)

	if _, err := e.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("write site config %s to %s: %w", sitePath, e.edgeContainer, err)
	}

	return e.Reload(ctx)
}

// RemoveSite deletes a .caddy file from /etc/caddy/sites/ and reloads Caddy.
func (e *EdgeManager) RemoveSite(ctx context.Context, siteName string) error {
	sitePath := fmt.Sprintf("/etc/caddy/sites/%s.caddy", siteName)
	cmd := fmt.Sprintf("incus exec %s -- rm -f %s", e.edgeContainer, sitePath)
	_, _ = e.exec.Run(ctx, cmd)
	return e.Reload(ctx)
}

// Reload executes a graceful caddy reload or container restart inside the edge container.
func (e *EdgeManager) Reload(ctx context.Context) error {
	// Graceful reload only. Do NOT rewrite the edge's resolv.conf and do NOT
	// restart the shared edge container: a failed reload leaves Caddy running
	// on its previous (valid) config, while a restart/misconfigured network
	// would drop every site behind the edge.
	cmd := fmt.Sprintf("incus exec %s -- caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile", e.edgeContainer)
	if out, err := e.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("caddy reload failed (edge left unchanged): %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}
