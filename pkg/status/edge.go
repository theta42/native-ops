package status

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

// Route is one hostname the edge serves and the upstreams it points at ("where it points").
type Route struct {
	Host      string   `json:"host"`
	Upstreams []string `json:"upstreams,omitempty"`
}

// Cert is one TLS certificate the edge holds: the names it covers, who issued it, and when it expires.
type Cert struct {
	Names     []string  `json:"names"`
	Issuer    string    `json:"issuer,omitempty"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
}

// Edge is what the edge container is actually serving.
type Edge struct {
	Container string   `json:"container"`
	Routes    []Route  `json:"routes"`
	Certs     []Cert   `json:"certs"`
	Warnings  []string `json:"warnings,omitempty"`
}

// collectEdge reads the edge's Caddy configuration (adapted from the Caddyfile it runs, which imports
// the per-site files native-ops publishes) and the certificates it holds. The edge image has no wget,
// so the config comes from `caddy adapt` rather than the admin API.
func collectEdge(ctx context.Context, ex remote.Executor, container string) *Edge {
	e := &Edge{Container: container, Routes: []Route{}, Certs: []Cert{}}
	base := "incus exec " + incus.ShQuote(container) + " -- "
	if out, err := ex.Run(ctx, base+"caddy adapt --config /etc/caddy/Caddyfile --adapter caddyfile"); err != nil {
		e.Warnings = append(e.Warnings, "could not read the edge config: "+err.Error())
	} else if routes, err := parseCaddyRoutes(out); err != nil {
		e.Warnings = append(e.Warnings, err.Error())
	} else {
		e.Routes = routes
	}
	paths, err := ex.Run(ctx, base+`sh -c 'find /root/.local/share/caddy /data/caddy -name "*.crt" -type f 2>/dev/null || true'`)
	if err != nil {
		e.Warnings = append(e.Warnings, "could not list edge certificates: "+err.Error())
	} else {
		for _, p := range strings.Fields(paths) {
			out, err := ex.Run(ctx, base+"cat "+incus.ShQuote(p))
			if err != nil {
				continue
			}
			if c, ok := parseCert(out); ok {
				e.Certs = append(e.Certs, c)
			}
		}
		sort.Slice(e.Certs, func(i, j int) bool { return e.Certs[i].NotAfter.Before(e.Certs[j].NotAfter) })
	}
	return e
}

// parseCaddyRoutes pulls (host -> upstream dials) out of a live Caddy config. The nesting varies by
// server, so it walks the JSON generically: any object with a host matcher contributes its hosts, and
// every "dial" found beneath its handlers is an upstream for those hosts.
func parseCaddyRoutes(s string) ([]Route, error) {
	var cfg any
	if err := json.Unmarshal([]byte(s), &cfg); err != nil {
		return nil, fmt.Errorf("parse edge config: %w", err)
	}
	seen := map[string]map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if hosts := hostMatches(t); len(hosts) > 0 {
				ups := map[string]bool{}
				collectDials(t["handle"], ups)
				for _, h := range hosts {
					if seen[h] == nil {
						seen[h] = map[string]bool{}
					}
					for d := range ups {
						seen[h][d] = true
					}
				}
			}
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(cfg)
	out := make([]Route, 0, len(seen))
	for h, ups := range seen {
		r := Route{Host: h}
		for d := range ups {
			r.Upstreams = append(r.Upstreams, d)
		}
		sort.Strings(r.Upstreams)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

func hostMatches(m map[string]any) []string {
	match, ok := m["match"].([]any)
	if !ok {
		return nil
	}
	var hosts []string
	for _, mm := range match {
		mo, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		hs, ok := mo["host"].([]any)
		if !ok {
			continue
		}
		for _, h := range hs {
			if s, ok := h.(string); ok {
				hosts = append(hosts, s)
			}
		}
	}
	return hosts
}

func collectDials(v any, out map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if k == "upstreams" {
				if arr, ok := child.([]any); ok {
					for _, u := range arr {
						if um, ok := u.(map[string]any); ok {
							if d, ok := um["dial"].(string); ok {
								out[d] = true
							}
						}
					}
				}
			}
			collectDials(child, out)
		}
	case []any:
		for _, child := range t {
			collectDials(child, out)
		}
	}
}

// parseCert reads the first certificate in a PEM bundle.
func parseCert(s string) (Cert, bool) {
	for {
		block, rest := pem.Decode([]byte(s))
		if block == nil {
			return Cert{}, false
		}
		if block.Type == "CERTIFICATE" {
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return Cert{}, false
			}
			names := append([]string{}, c.DNSNames...)
			if len(names) == 0 && c.Subject.CommonName != "" {
				names = []string{c.Subject.CommonName}
			}
			sort.Strings(names)
			return Cert{Names: names, Issuer: c.Issuer.CommonName, NotBefore: c.NotBefore.UTC(), NotAfter: c.NotAfter.UTC()}, true
		}
		s = string(rest)
	}
}
