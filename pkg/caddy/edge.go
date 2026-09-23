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

// EnsureBaseCaddyfile ensures /etc/caddy/Caddyfile exists and imports /etc/caddy/sites/*.caddy
func (e *EdgeManager) EnsureBaseCaddyfile(ctx context.Context) error {
	baseConfig := "import /etc/caddy/sites/*.caddy\n"
	b64 := base64.StdEncoding.EncodeToString([]byte(baseConfig))
	cmd := fmt.Sprintf("echo '%s' | base64 -d | incus exec %s -- sh -c 'mkdir -p /etc/caddy /etc/caddy/sites && cat > /etc/caddy/Caddyfile && chmod 644 /etc/caddy/Caddyfile'", b64, e.edgeContainer)
	_, _ = e.exec.Run(ctx, cmd)
	return nil
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

// Reload executes a graceful caddy reload inside the edge container.
func (e *EdgeManager) Reload(ctx context.Context) error {
	cmd := fmt.Sprintf("incus exec %s -- caddy reload --config /etc/caddy/Caddyfile || incus exec %s -- systemctl reload caddy",
		e.edgeContainer, e.edgeContainer)

	if _, err := e.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("reload caddy inside %s: %w", e.edgeContainer, err)
	}
	return nil
}
