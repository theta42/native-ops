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
	baseConfig := "{\n    email wmantly@gmail.com\n    debug\n    log {\n        output file /var/log/caddy.log\n        level DEBUG\n    }\n}\n\nimport /etc/caddy/sites/*.caddy\n"
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

// Reload executes a graceful caddy reload or container restart inside the edge container.
func (e *EdgeManager) Reload(ctx context.Context) error {
	// 1. Ensure resolv.conf has valid nameservers and is world-readable
	_, _ = e.exec.Run(ctx, fmt.Sprintf("incus exec %s -- sh -c \"rm -f /etc/resolv.conf && printf 'nameserver 1.1.1.1\\nnameserver 8.8.8.8\\nnameserver 10.0.100.1\\n' > /etc/resolv.conf && chmod 644 /etc/resolv.conf\" || true", e.edgeContainer))

	// 2. Try caddy reload
	cmd := fmt.Sprintf("incus exec %s -- caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile", e.edgeContainer)
	if _, err := e.exec.Run(ctx, cmd); err != nil {
		// If reload fails, restart edge and re-apply networking + resolv.conf
		_, _ = e.exec.Run(ctx, fmt.Sprintf("incus restart %s", e.edgeContainer))
		_, _ = e.exec.Run(ctx, fmt.Sprintf("incus exec %s -- ip link set eth0 up || true", e.edgeContainer))
		_, _ = e.exec.Run(ctx, fmt.Sprintf("incus exec %s -- ip addr add 10.0.100.10/24 dev eth0 || true", e.edgeContainer))
		_, _ = e.exec.Run(ctx, fmt.Sprintf("incus exec %s -- ip route replace default via 10.0.100.1 || true", e.edgeContainer))
		_, _ = e.exec.Run(ctx, fmt.Sprintf("incus exec %s -- sh -c \"rm -f /etc/resolv.conf && printf 'nameserver 1.1.1.1\\nnameserver 8.8.8.8\\nnameserver 10.0.100.1\\n' > /etc/resolv.conf && chmod 644 /etc/resolv.conf\" || true", e.edgeContainer))
	}
	return nil
}

