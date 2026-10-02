package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/theta42/native-ops/pkg/caddy"
	"github.com/theta42/native-ops/pkg/remote"
)

// ApplyEdgeConfig applies the configuration repo's edge/Caddyfile to the edge
// container: that file is the edge's routes and TLS, so changing the edge is a
// change to the repo, applied here, not a shell on the host.
//
// Per-site files under /etc/caddy/sites/ are left alone: native-ops writes those
// itself for instance routes (PublishSite), so syncing them from the repo would
// fight the engine. The Caddyfile is validated and reloaded, and the previous
// one is restored if Caddy rejects it (see caddy.EdgeManager.SyncCaddyfile).
//
// Usage: native-ops edge apply --config-dir <native-ops-conf>
func ApplyEdgeConfig(ctx context.Context, exec remote.Executor, configDir, edgeContainer string) error {
	path := filepath.Join(configDir, "edge", "Caddyfile")
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("no edge Caddyfile at %s (expected edge/Caddyfile in the config repo): %w", path, err)
	}
	if edgeContainer == "" {
		edgeContainer = "edge"
	}
	em := caddy.NewEdgeManager(exec, edgeContainer)
	changed, err := em.SyncCaddyfile(ctx, string(content))
	if err != nil {
		return err
	}
	if changed {
		log.Printf("==> [Edge] applied %s to %s", path, edgeContainer)
	} else {
		log.Printf("==> [Edge] %s already matches %s", edgeContainer, path)
	}
	return nil
}
