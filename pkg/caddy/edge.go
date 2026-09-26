package caddy

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

const (
	caddyfilePath = "/etc/caddy/Caddyfile"
	sitesDir      = "/etc/caddy/sites"
)

var (
	emailRe    = regexp.MustCompile(`^[^\s{}]+@[^\s{}]+$`)
	domainRe   = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	upstreamRe = regexp.MustCompile(`reverse_proxy ([0-9.]+):[0-9]+`)
)

// EdgeManager manages Caddy edge proxy routes inside the edge container.
type EdgeManager struct {
	exec          remote.Executor
	incus         *incus.Client
	edgeContainer string

	// Email is the ACME contact written into a Caddyfile that native-ops has to
	// create. It defaults to $NATIVE_OPS_ACME_EMAIL; when empty no email
	// directive is written. It is never applied to an existing Caddyfile.
	Email string
}

func NewEdgeManager(exec remote.Executor, edgeContainer string) *EdgeManager {
	if edgeContainer == "" {
		edgeContainer = "edge"
	}
	return &EdgeManager{
		exec:          exec,
		incus:         incus.NewClient(exec),
		edgeContainer: edgeContainer,
		Email:         os.Getenv("NATIVE_OPS_ACME_EMAIL"),
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

// BaseCaddyfile is the Caddyfile native-ops creates for an edge that has none.
func BaseCaddyfile(email string) string {
	var sb strings.Builder
	sb.WriteString("{\n")
	if email != "" {
		fmt.Fprintf(&sb, "    email %s\n", email)
	}
	sb.WriteString("    debug\n    log {\n        output file /var/log/caddy.log\n        level DEBUG\n    }\n}\n\nimport " + sitesDir + "/*.caddy\n")
	return sb.String()
}

// EnsureBaseCaddyfile makes sure the edge has a Caddyfile that imports the
// sites directory. It creates one only when there is none: an existing
// Caddyfile is hand-maintained state (custom TLS, per-site blocks) and is never
// overwritten. An existing one that does not import the sites directory is an
// error, because sites published through native-ops would silently never be served.
func (e *EdgeManager) EnsureBaseCaddyfile(ctx context.Context) error {
	cur, found, err := e.incus.PullFile(ctx, e.edgeContainer, caddyfilePath)
	if err != nil {
		return err
	}
	if found {
		if !strings.Contains(cur, "import "+sitesDir) {
			return fmt.Errorf("%s in %s does not import %s/*.caddy, so published sites would never be served; add that import line (native-ops never overwrites an existing Caddyfile)", caddyfilePath, e.edgeContainer, sitesDir)
		}
		return nil
	}
	if e.Email != "" && !emailRe.MatchString(e.Email) {
		return fmt.Errorf("invalid ACME email %q", e.Email)
	}
	return e.incus.PushFile(ctx, e.edgeContainer, caddyfilePath, BaseCaddyfile(e.Email), "0644")
}

func (e *EdgeManager) validate(ctx context.Context) error {
	cmd := fmt.Sprintf("incus exec %s -- caddy validate --config %s --adapter caddyfile", incus.ShQuote(e.edgeContainer), caddyfilePath)
	if _, err := e.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("caddy validate: %w", err)
	}
	return nil
}

func sitePathFor(siteName string) string { return sitesDir + "/" + siteName + ".caddy" }

// PublishSite writes /etc/caddy/sites/<siteName>.caddy and reloads Caddy. It is
// idempotent: when the file already has exactly this content nothing is
// written and Caddy is not reloaded. The new config is validated before the
// reload; if Caddy rejects it the previous file is restored and an error is
// returned, so one bad site can never take the edge down on its next restart.
func (e *EdgeManager) PublishSite(ctx context.Context, siteName string, routing config.RoutingConfig, upstreamIP string) error {
	if !incus.ValidName(siteName) {
		return fmt.Errorf("invalid site name %q", siteName)
	}
	if !domainRe.MatchString(routing.Domain) {
		return fmt.Errorf("invalid domain %q", routing.Domain)
	}
	if net.ParseIP(upstreamIP) == nil {
		return fmt.Errorf("invalid upstream address %q", upstreamIP)
	}
	if routing.UpstreamPort < 1 || routing.UpstreamPort > 65535 {
		return fmt.Errorf("invalid upstream port %d", routing.UpstreamPort)
	}
	if err := e.EnsureBaseCaddyfile(ctx); err != nil {
		return err
	}

	desired := RenderSiteBlock(routing.Domain, upstreamIP, routing.UpstreamPort, routing.TLS, routing.ExtraDirectives)
	prev, hadPrev, err := e.incus.PullFile(ctx, e.edgeContainer, sitePathFor(siteName))
	if err != nil {
		return err
	}
	if hadPrev && prev == desired {
		return nil
	}
	return e.replaceSite(ctx, siteName, desired, prev, hadPrev)
}

// replaceSite writes a site file, validates the whole Caddy config, and reloads.
// If Caddy rejects the config the previous file is restored (or the new one
// removed), so one bad site can never take the edge down on its next restart.
func (e *EdgeManager) replaceSite(ctx context.Context, siteName, desired, prev string, hadPrev bool) error {
	path := sitePathFor(siteName)
	if err := e.incus.PushFile(ctx, e.edgeContainer, path, desired, "0644"); err != nil {
		return fmt.Errorf("write site config %s to %s: %w", path, e.edgeContainer, err)
	}
	if err := e.validate(ctx); err != nil {
		if hadPrev {
			_ = e.incus.PushFile(ctx, e.edgeContainer, path, prev, "0644")
		} else {
			_, _ = e.exec.Run(ctx, fmt.Sprintf("incus exec %s -- rm -f %s", incus.ShQuote(e.edgeContainer), incus.ShQuote(path)))
		}
		return fmt.Errorf("caddy rejected the new config for %s; the previous one was restored: %w", siteName, err)
	}
	return e.Reload(ctx)
}

// RepointUpstream points an already-published site at the instance's current
// address. Replacing a container gives it a new DHCP lease, so without this a
// route keeps pointing at an address nobody has any more. Only the upstream
// address is rewritten (domain, TLS and other directives stay as they are). It
// does nothing, and never asks for the addresses, when the instance has no
// published site; and does nothing when the published address is still one of
// the instance's. changed reports whether the route was rewritten.
func (e *EdgeManager) RepointUpstream(ctx context.Context, siteName string, addrs func(context.Context) ([]string, error)) (changed bool, err error) {
	if !incus.ValidName(siteName) {
		return false, fmt.Errorf("invalid site name %q", siteName)
	}
	cur, found, err := e.incus.PullFile(ctx, e.edgeContainer, sitePathFor(siteName))
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	loc := upstreamRe.FindStringSubmatchIndex(cur)
	if loc == nil {
		return false, nil
	}
	published := cur[loc[2]:loc[3]]
	candidates, err := addrs(ctx)
	if err != nil {
		return false, err
	}
	if len(candidates) == 0 {
		return false, fmt.Errorf("no upstream address for %s", siteName)
	}
	for _, ip := range candidates {
		if ip == published {
			return false, nil
		}
	}
	if net.ParseIP(candidates[0]) == nil {
		return false, fmt.Errorf("invalid upstream address %q", candidates[0])
	}
	desired := cur[:loc[2]] + candidates[0] + cur[loc[3]:]
	if err := e.replaceSite(ctx, siteName, desired, cur, true); err != nil {
		return false, err
	}
	return true, nil
}

// PublishSiteFor is PublishSite for an upstream that may have several
// addresses. If the currently published upstream is still one of them it is
// kept, so re-running never flaps between equivalent addresses.
func (e *EdgeManager) PublishSiteFor(ctx context.Context, siteName string, routing config.RoutingConfig, candidateIPs []string) error {
	if len(candidateIPs) == 0 {
		return fmt.Errorf("no upstream address for %s", siteName)
	}
	choose := candidateIPs[0]
	if incus.ValidName(siteName) {
		cur, found, err := e.incus.PullFile(ctx, e.edgeContainer, sitePathFor(siteName))
		if err != nil {
			return err
		}
		if found {
			if m := upstreamRe.FindStringSubmatch(cur); m != nil {
				for _, ip := range candidateIPs {
					if ip == m[1] {
						choose = ip
					}
				}
			}
		}
	}
	return e.PublishSite(ctx, siteName, routing, choose)
}

// RemoveSite deletes a site file and reloads Caddy. Removing a site that is not
// there is a no-op, so it can be repeated.
func (e *EdgeManager) RemoveSite(ctx context.Context, siteName string) error {
	if !incus.ValidName(siteName) {
		return fmt.Errorf("invalid site name %q", siteName)
	}
	path := sitePathFor(siteName)
	_, found, err := e.incus.PullFile(ctx, e.edgeContainer, path)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if _, err := e.exec.Run(ctx, fmt.Sprintf("incus exec %s -- rm -f %s", incus.ShQuote(e.edgeContainer), incus.ShQuote(path))); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return e.Reload(ctx)
}

// resolvScript rewrites resolv.conf only when it is not already in the expected state.
const resolvScript = `grep -qx 'nameserver 10.0.100.1' /etc/resolv.conf 2>/dev/null || { rm -f /etc/resolv.conf && printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\nnameserver 10.0.100.1\n' > /etc/resolv.conf && chmod 644 /etc/resolv.conf; }`

// Reload executes a graceful caddy reload. If that fails it restarts the edge
// container and tries once more (the edge gets its address from the bridge's
// DHCP like every other container; nothing pins one). A reload that
// still fails is returned as an error: a route that is not live is not "published".
func (e *EdgeManager) Reload(ctx context.Context) error {
	c := incus.ShQuote(e.edgeContainer)
	resolv := fmt.Sprintf("incus exec %s -- sh -c %s || true", c, incus.ShQuote(resolvScript))
	reload := fmt.Sprintf("incus exec %s -- caddy reload --config %s --adapter caddyfile", c, caddyfilePath)

	_, _ = e.exec.Run(ctx, resolv)
	_, firstErr := e.exec.Run(ctx, reload)
	if firstErr == nil {
		return nil
	}
	_, _ = e.exec.Run(ctx, fmt.Sprintf("incus restart %s", c))
	_, _ = e.exec.Run(ctx, resolv)
	if _, err := e.exec.Run(ctx, reload); err != nil {
		return fmt.Errorf("caddy reload failed (%v) and failed again after restarting %s: %w", firstErr, e.edgeContainer, err)
	}
	return nil
}
