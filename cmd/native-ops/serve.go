package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/theta42/native-ops/pkg/remote"
	"github.com/theta42/native-ops/pkg/server"
	"github.com/theta42/native-ops/pkg/status"
)

func handleStatusCommand(ctx context.Context, args []string) {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := flags.Bool("json", false, "Print the snapshot as JSON")
	pool := flags.String("pool", "default", "Storage pool holding the data volumes")
	_ = flags.Parse(args)

	snap, err := status.Collect(ctx, remote.NewLocalExecutor(), *pool)
	if err != nil {
		log.Fatalf("status: %v", err)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
		return
	}
	printStatus(snap)
}

func printStatus(s *status.Snapshot) {
	fmt.Printf("%s  incus %s  pool %s  (%s)\n\n", s.Host, s.Incus, s.Pool, s.Time.Format("2006-01-02 15:04:05Z"))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "INSTANCES (%d)\tSTATUS\tIMAGE\tIPV4\tVOLUMES\tSNAPS\n", len(s.Instances))
	for _, i := range s.Instances {
		img := i.Recorded["image"]
		if img == "" {
			img = i.Image
		}
		if len(img) > 44 {
			img = img[:43] + "…"
		}
		var vols []string
		for _, d := range i.Devices {
			if d.Type == "disk" && d.Source != "" {
				v := d.Source
				if d.HostPath {
					v += " (host path)"
				}
				vols = append(vols, v)
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\n", i.Name, i.Status, img, strings.Join(i.IPv4, ","), strings.Join(vols, ","), i.Snapshots)
	}
	fmt.Fprintf(w, "\nVOLUMES (%d)\tUSED BY\tSHIFTED\tSNAPS\tLATEST DAILY\t\n", len(s.Volumes))
	for _, v := range s.Volumes {
		used := strings.Join(v.UsedBy, ",")
		if used == "" {
			used = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t\n", v.Name, used, v.Shifted, v.Snapshots, v.LatestDaily)
	}
	_ = w.Flush()
	fmt.Printf("\nimages: %d (%d MB), %d without an alias\n", s.Images.Count, s.Images.TotalBytes/1_000_000, s.Images.Unaliased)
	if len(s.Warnings) > 0 {
		fmt.Printf("\n%d thing(s) to look at:\n", len(s.Warnings))
		for _, m := range s.Warnings {
			fmt.Printf("  - %s\n", m)
		}
	}
}

func stateDirFlag(flags *flag.FlagSet) *string {
	return flags.String("state-dir", "/var/lib/native-ops", "Directory for the daemon's tokens and audit log")
}

func openStateDir(dir string) string {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatalf("state dir %s: %v", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		log.Fatalf("state dir %s: %v", dir, err)
	}
	return dir
}

func handleServeCommand(ctx context.Context, args []string) {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := flags.String("addr", "127.0.0.1:8686", "Listen address (put TLS in front of it, e.g. the Caddy edge)")
	pool := flags.String("pool", "default", "Storage pool holding the data volumes")
	stateDir := stateDirFlag(flags)
	_ = flags.Parse(args)

	dir := openStateDir(*stateDir)
	tokens, err := server.OpenTokenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		log.Fatalf("tokens: %v", err)
	}
	if bt := os.Getenv("NATIVE_OPS_BOOTSTRAP_TOKEN"); bt != "" {
		if err := tokens.SetBootstrap(bt); err != nil {
			log.Fatalf("NATIVE_OPS_BOOTSTRAP_TOKEN: %v", err)
		}
		log.Printf("bootstrap admin token loaded from NATIVE_OPS_BOOTSTRAP_TOKEN")
	}
	audit, err := server.OpenAudit(filepath.Join(dir, "audit.log"))
	if err != nil {
		log.Fatalf("audit log: %v", err)
	}
	defer audit.Close()

	exec := remote.NewLocalExecutor()
	srv, err := server.New(server.Options{
		Addr: *addr, Tokens: tokens, Audit: audit, Version: Version,
		Status: func(ctx context.Context) (*status.Snapshot, error) { return status.Collect(ctx, exec, *pool) },
	})
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("native-ops %s serving on http://%s (state: %s)", Version, *addr, dir)
	if err := srv.ListenAndServe(ctx); err != nil {
		log.Fatalf("serve: %v", err)
	}
	log.Printf("stopped")
}

func handleTokenCommand(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: native-ops token [create|list|revoke] --state-dir DIR ...")
		os.Exit(1)
	}
	action := args[0]
	flags := flag.NewFlagSet("token "+action, flag.ExitOnError)
	stateDir := stateDirFlag(flags)
	name := flags.String("name", "", "Token name (create)")
	role := flags.String("role", "viewer", "viewer, deployer or admin (create)")
	id := flags.String("id", "", "Token id (revoke)")
	_ = flags.Parse(args[1:])

	store, err := server.OpenTokenStore(filepath.Join(openStateDir(*stateDir), "tokens.json"))
	if err != nil {
		log.Fatalf("tokens: %v", err)
	}
	switch action {
	case "create":
		secret, t, err := store.Create(*name, server.Role(*role))
		if err != nil {
			log.Fatalf("token create: %v", err)
		}
		fmt.Printf("Created %s token %q (id %s). It is shown once; store it in your CI secret store:\n\n%s\n", t.Role, t.Name, t.ID, secret)
	case "list":
		ts, err := store.List()
		if err != nil {
			log.Fatalf("token list: %v", err)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tROLE\tCREATED")
		for _, t := range ts {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.ID, t.Name, t.Role, t.Created.Format("2006-01-02"))
		}
		_ = w.Flush()
	case "revoke":
		if err := store.Revoke(*id); err != nil {
			log.Fatalf("token revoke: %v", err)
		}
		fmt.Printf("Revoked %s\n", *id)
	default:
		fmt.Println("Usage: native-ops token [create|list|revoke] --state-dir DIR ...")
		os.Exit(1)
	}
}
