package config

import (
	"fmt"
	"regexp"
	"time"
)

// RouteCache caches responses at the edge (Caddy's cache-handler) for the paths listed. Caching follows
// HTTP: an upstream's Cache-Control (no-store, private, max-age) is honoured, and TTL is how long a
// response that does not say is kept. Paths are required, so a whole site -- with its signed-in pages --
// is never cached by accident; ["/*"] says so explicitly.
type RouteCache struct {
	TTL   string   `yaml:"ttl,omitempty" json:"ttl,omitempty"` // a duration, e.g. "5m"; default "2m"
	Paths []string `yaml:"paths" json:"paths"`                 // path matchers, e.g. "/static/*"
}

// RouteRateLimit limits how many requests one client may make in a window (caddy-ratelimit), keyed on
// the client's address as Caddy sees it ({client_ip}: the peer, or the forwarded address from a
// trusted proxy when the Caddyfile names trusted_proxies). Over the limit a client gets 429 with
// Retry-After. Paths default to the whole route.
type RouteRateLimit struct {
	Requests int      `yaml:"requests" json:"requests"`               // allowed per window, per client
	Window   string   `yaml:"window" json:"window"`                   // a duration, e.g. "1m"
	Paths    []string `yaml:"paths,omitempty" json:"paths,omitempty"` // path matchers; empty is every path
}

// routePathRe is what a path matcher may be: a path with at most wildcards, nothing that could end the
// Caddyfile token (space, brace, quote) or start another directive.
var routePathRe = regexp.MustCompile(`^/[A-Za-z0-9._~%/*-]{0,200}$`)

const maxRoutePaths = 20

func validRoutePaths(field string, paths []string) error {
	if len(paths) > maxRoutePaths {
		return fmt.Errorf("%s: at most %d paths", field, maxRoutePaths)
	}
	for _, p := range paths {
		if !routePathRe.MatchString(p) {
			return fmt.Errorf("%s: %q is not a path matcher (a path starting with /, letters, digits, . _ ~ %% - / and *)", field, p)
		}
	}
	return nil
}

func boundedDuration(field, v string, min, max time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil || d < min || d > max {
		return 0, fmt.Errorf("%s must be a duration from %s to %s (e.g. 30s, 5m, 1h), not %q", field, min, max, v)
	}
	return d, nil
}

// Validate reports why the cache settings cannot be used.
func (c *RouteCache) Validate() error {
	if c == nil {
		return nil
	}
	if len(c.Paths) == 0 {
		return fmt.Errorf(`cache.paths is required: list what may be cached (e.g. "/static/*"), or "/*" for the whole site`)
	}
	if err := validRoutePaths("cache.paths", c.Paths); err != nil {
		return err
	}
	if c.TTL != "" {
		if _, err := boundedDuration("cache.ttl", c.TTL, time.Second, 7*24*time.Hour); err != nil {
			return err
		}
	}
	return nil
}

// TTLOrDefault is the TTL as Caddy reads it.
func (c *RouteCache) TTLOrDefault() string {
	if c.TTL == "" {
		return "2m"
	}
	return c.TTL
}

// Validate reports why the rate limit cannot be used.
func (r *RouteRateLimit) Validate() error {
	if r == nil {
		return nil
	}
	if r.Requests < 1 || r.Requests > 1_000_000 {
		return fmt.Errorf("rate_limit.requests must be from 1 to 1000000, not %d", r.Requests)
	}
	if _, err := boundedDuration("rate_limit.window", r.Window, time.Second, 24*time.Hour); err != nil {
		return err
	}
	return validRoutePaths("rate_limit.paths", r.Paths)
}

// ValidateEdgeOptions checks the route's cache and rate limit.
func (r *RoutingConfig) ValidateEdgeOptions() error {
	if err := r.Cache.Validate(); err != nil {
		return err
	}
	return r.RateLimit.Validate()
}
