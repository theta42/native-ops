package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/theta42/native-ops/pkg/engine"
	"github.com/theta42/native-ops/pkg/remote"
)

// handleEdgeCommand runs `native-ops edge apply --config-dir <native-ops-conf>`:
// apply the config repo's edge/Caddyfile to the edge container, on the host. It
// is the same work the daemon does for POST /v1/edge/apply, for running locally.
func handleEdgeCommand(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: native-ops edge apply [--config-dir <native-ops-conf>] [--edge-container edge]")
		os.Exit(1)
	}
	action := args[0]
	if action != "apply" {
		fmt.Fprintf(os.Stderr, "Unknown edge action: %s\n", action)
		os.Exit(1)
	}
	flags := flag.NewFlagSet("edge "+action, flag.ExitOnError)
	configDir := flags.String("config-dir", ".", "Path to the native-ops-conf checkout")
	edgeContainer := flags.String("edge-container", envOr("NATIVE_OPS_EDGE_CONTAINER", "edge"), "Incus container running the edge (Caddy)")
	_ = flags.Parse(args[1:])

	if err := engine.ApplyEdgeConfig(ctx, remote.NewLocalExecutor(), *configDir, *edgeContainer); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Edge config applied.")
}
