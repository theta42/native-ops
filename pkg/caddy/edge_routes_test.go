package caddy

import (
	"context"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
)

const bothModules = "http.handlers.reverse_proxy\nhttp.handlers.cache\nhttp.handlers.rate_limit\n"

func TestARouteWithoutCacheOrRateLimitRendersAsItAlwaysHas(t *testing.T) {
	r := config.RoutingConfig{Domain: "git.example.com", UpstreamPort: 3000, TLS: "internal", ExtraDirectives: []string{"import strip-forged-identity"}}
	want := "git.example.com {\n    tls internal\n    reverse_proxy 10.0.100.21:3000\n    import strip-forged-identity\n}\n"
	if got := RenderSite("gitea", r, "10.0.100.21"); got != want {
		t.Fatalf("an existing site must not change:\n%s\nwant:\n%s", got, want)
	}
	if got := RenderSiteBlock(r.Domain, "10.0.100.21", 3000, "internal", r.ExtraDirectives); got != want {
		t.Fatalf("RenderSiteBlock: %s", got)
	}
}

func TestCacheAndRateLimitRenderInARouteInTheirOrder(t *testing.T) {
	r := config.RoutingConfig{Domain: "www.example.com", UpstreamPort: 3000, ExtraDirectives: []string{"encode gzip"},
		Cache:     &config.RouteCache{TTL: "5m", Paths: []string{"/static/*", "/assets/*"}},
		RateLimit: &config.RouteRateLimit{Requests: 120, Window: "1m", Paths: []string{"/api/*"}}}
	want := `www.example.com {
    encode gzip
    route {
        rate_limit {
            zone web {
                match {
                    path /api/*
                }
                key {client_ip}
                events 120
                window 1m
            }
        }
        @native_ops_cache path /static/* /assets/*
        cache @native_ops_cache {
            ttl 5m
        }
        reverse_proxy 10.0.100.5:3000
    }
}
`
	if got := RenderSite("web", r, "10.0.100.5"); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// A limit on the whole route has no matcher; a cache without a TTL takes the default.
	got := RenderSite("api", config.RoutingConfig{Domain: "api.example.com", UpstreamPort: 8080,
		RateLimit: &config.RouteRateLimit{Requests: 10, Window: "10s"}, Cache: &config.RouteCache{Paths: []string{"/*"}}}, "10.0.100.6")
	if strings.Contains(got, "match {") || !strings.Contains(got, "ttl 2m") || !strings.Contains(got, "zone api {") {
		t.Fatalf("whole-route limit and default TTL:\n%s", got)
	}
}

func TestARouteWithCacheOrRateLimitNeedsThePluginsOnTheEdge(t *testing.T) {
	ctx := context.Background()
	r := config.RoutingConfig{Domain: "www.example.com", UpstreamPort: 3000,
		RateLimit: &config.RouteRateLimit{Requests: 60, Window: "1m"}}

	sim := newEdgeSim()
	sim.modules = "http.handlers.reverse_proxy\nhttp.handlers.cache\n"
	e := NewEdgeManager(sim, "edge")
	if _, err := e.PlanSiteFor(ctx, "web", r, []string{"10.0.100.5"}); err == nil || !strings.Contains(err.Error(), "http.handlers.rate_limit") {
		t.Fatalf("a plan must say the edge lacks the plugin: %v", err)
	}
	if err := e.PublishSite(ctx, "web", r, "10.0.100.5"); err == nil || sim.writes != 0 {
		t.Fatalf("nothing may be written for an edge without the plugin: %v, %d writes", err, sim.writes)
	}

	sim.modules = bothModules
	if sp, err := e.PlanSiteFor(ctx, "web", r, []string{"10.0.100.5"}); err != nil || sp.Change != "create" {
		t.Fatalf("with the plugin: %+v %v", sp, err)
	}
	if err := e.PublishSite(ctx, "web", r, "10.0.100.5"); err != nil || !strings.Contains(sim.files["/etc/caddy/sites/web.caddy"], "rate_limit {") {
		t.Fatalf("publish: %v\n%s", err, sim.files["/etc/caddy/sites/web.caddy"])
	}

	// A route without the options never asks the edge for its modules.
	sim2 := newEdgeSim()
	if err := NewEdgeManager(sim2, "edge").PublishSite(ctx, "gitea", route, "10.0.100.21"); err != nil {
		t.Fatal(err)
	}
	for _, c := range sim2.cmds {
		if strings.Contains(c, "list-modules") {
			t.Fatal("a plain route must not depend on the edge's modules")
		}
	}
}

func TestInvalidCacheOrRateLimitIsRefusedBeforeTheEdgeIsTouched(t *testing.T) {
	ctx := context.Background()
	for name, r := range map[string]config.RoutingConfig{
		"cache without paths":       {Cache: &config.RouteCache{TTL: "1m"}},
		"a path that ends the line": {Cache: &config.RouteCache{Paths: []string{"/a }\n    respond 200"}}},
		"a path with a space":       {Cache: &config.RouteCache{Paths: []string{"/a /b"}}},
		"a relative path":           {Cache: &config.RouteCache{Paths: []string{"static/*"}}},
		"a bad ttl":                 {Cache: &config.RouteCache{Paths: []string{"/*"}, TTL: "forever"}},
		"no requests":               {RateLimit: &config.RouteRateLimit{Window: "1m"}},
		"a window too long":         {RateLimit: &config.RouteRateLimit{Requests: 5, Window: "48h"}},
		"a placeholder in a path":   {RateLimit: &config.RouteRateLimit{Requests: 5, Window: "1m", Paths: []string{"/{env.X}"}}},
	} {
		r.Domain, r.UpstreamPort = "www.example.com", 3000
		sim := newEdgeSim()
		sim.modules = bothModules
		if err := NewEdgeManager(sim, "edge").PublishSite(ctx, "web", r, "10.0.100.5"); err == nil || sim.writes != 0 {
			t.Errorf("%s: %v, %d writes", name, err, sim.writes)
		}
	}
}
