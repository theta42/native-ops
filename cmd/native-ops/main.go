package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/theta42/native-ops/pkg/backup"
	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/engine"
	"github.com/theta42/native-ops/pkg/provider"
	"github.com/theta42/native-ops/pkg/provider/digitalocean"
	"github.com/theta42/native-ops/pkg/provider/plugin"
	"github.com/theta42/native-ops/pkg/remote"
	"github.com/theta42/native-ops/pkg/s3"
)

var Version = "v1.0.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	ctx := context.Background()
	subcommand := os.Args[1]

	switch subcommand {
	case "version", "-v", "--version":
		fmt.Printf("native-ops %s (MIT License, theta42)\n", Version)

	case "validate":
		handleValidateCommand(ctx, os.Args[2:])

	case "reconcile":
		handleReconcileCommand(ctx, os.Args[2:])

	case "host":
		handleHostCommand(ctx, os.Args[2:])

	case "apply":
		handleApplyCommand(ctx, os.Args[2:])
	case "plan":
		handlePlanCommand(ctx, os.Args[2:])

	case "instance":
		handleInstanceCommand(ctx, os.Args[2:])

	case "backup":
		handleBackupCommand(ctx, os.Args[2:])

	case "image":
		handleImageCommand(ctx, os.Args[2:])

	case "edge":
		handleEdgeCommand(ctx, os.Args[2:])

	case "preview":
		handlePreviewCommand(ctx, os.Args[2:])

	case "dns":
		handleDNSCommand(ctx, os.Args[2:])

	case "status":
		handleStatusCommand(ctx, os.Args[2:])

	case "serve":
		handleServeCommand(ctx, os.Args[2:])

	case "token":
		handleTokenCommand(os.Args[2:])

	case "user":
		handleUserCommand(os.Args[2:])

	case "remote":
		handleRemoteCommand(ctx, os.Args[2:])

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", subcommand)
		printUsage()
		os.Exit(1)
	}
}

// usageText is the command list, printed by `native-ops` with no arguments. README.md carries a copy
// (TestREADMECarriesTheUsage keeps them the same).
const usageText = `native-ops - Generic Incus & Cloud Fleet Orchestration Engine (theta42)

Usage:
  native-ops <command> [options]

GitOps Commands:
  validate         Check the manifests parse and are consistent (no host access; used in PRs)
  reconcile        Provision the fleet's hosts, sync DNS and prepare Incus (services: via the daemon;
                   --deploy-services deploys them directly, bypassing approval)

The daemon (runs on each host; CI drives it over HTTPS):
  status           Read-only view of the host's instances, volumes and images (--json)
  serve            Run the authenticated API + UI daemon (needs a state dir; see README)
  token create     Create an API token (also: token list, token revoke)
  user create      Create a local UI user (also: user list, user passwd, user role, user disable)
  remote           Drive a daemon from CI: plan, apply, edge-apply, backup, restore, dns-sync, wait,
                   token-create, recipe-approve (uploads the checked-out tree; NATIVE_OPS_URL,
                   NATIVE_OPS_TOKEN)

Core Commands:
  host create      Provision a new cloud host (DigitalOcean; Proxmox is experimental)
  host destroy     Tear down a host VM
  host list        List active hosts for a provider
  plan             Show what apply would change, without changing anything
  apply            Declaratively apply services from native-ops-conf
  instance launch  Launch a dynamic workload from a template
  instance update  Immutable container update for an instance
  instance resize  Live CPU/memory cgroup resizing
  instance destroy Delete an instance and its Caddy route
  instance migrate Move an instance and its volumes across Incus remotes (--finalize removes the source)
  backup init      Create the destination bucket if it does not exist
  backup create    Back up one custom volume to S3-compatible object storage
  backup all       Back up every (allowlisted) custom volume
  backup list      List stored backups for a volume
  backup restore   Restore a volume from a stored backup
  backup prune     Apply retention to a volume's stored backups
  image build      Build + publish an app image from a git ref (conf recipe)
  image recipe-digest  Print the digest of scripts/ + images/ a daemon must have approved to build
  edge apply       Apply the config repo's edge/Caddyfile to the edge container (validated, with rollback)
  preview launch   Deploy an ephemeral preview from a template + ref
  preview list     List active previews (with TTL)
  preview destroy  Tear down a preview (container + volume + route)
  preview gc       Destroy expired previews
  dns sync         Sync DNS records through the configured provider or a script plugin
  version          Print version information`

func printUsage() {
	fmt.Println(usageText)
}

func handleValidateCommand(ctx context.Context, args []string) {
	flags := flag.NewFlagSet("validate", flag.ExitOnError)
	configDir := flags.String("config-dir", ".", "Path to native-ops-conf")
	deployServices := flags.Bool("deploy-services", false, "Also deploy every service directly over SSH (break-glass: bypasses the daemon's plan approval, host lock and job record). Normally services go through `native-ops remote plan` / `apply`")
	_ = flags.Parse(args)

	exec := remote.NewLocalExecutor()
	rec := engine.NewReconciler(*configDir, exec)
	rec.DeployServices = *deployServices

	summary, err := rec.Validate(ctx)
	if err != nil {
		log.Fatalf("Validation failed: %v", err)
	}

	fmt.Printf("✅ Validation Successful!\n")
	fmt.Printf("  • Fleet:     %s (%s)\n", summary.FleetName, summary.Domain)
	fmt.Printf("  • DNS:       %s\n", summary.DNSProvider)
	fmt.Printf("  • Hosts:     %d (%v)\n", len(summary.Hosts), summary.Hosts)
	fmt.Printf("  • Services:  %d (%v)\n", len(summary.Services), summary.Services)
	fmt.Printf("  • Templates: %d (%v)\n", len(summary.Templates), summary.Templates)
}

func handleReconcileCommand(ctx context.Context, args []string) {
	flags := flag.NewFlagSet("reconcile", flag.ExitOnError)
	configDir := flags.String("config-dir", ".", "Path to native-ops-conf")
	deployServices := flags.Bool("deploy-services", false, "Also deploy every service directly over SSH (break-glass: bypasses the daemon's plan approval, host lock and job record). Normally services go through `native-ops remote plan` / `apply`")
	_ = flags.Parse(args)

	exec := remote.NewLocalExecutor()
	rec := engine.NewReconciler(*configDir, exec)
	rec.DeployServices = *deployServices

	if err := rec.Reconcile(ctx); err != nil {
		log.Fatalf("Reconciliation failed: %v", err)
	}
	fmt.Println("Fleet reconciliation completed.")
}

func handleHostCommand(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: native-ops host [create|destroy|list]")
		os.Exit(1)
	}

	hm := engine.NewHostManager()
	action := args[0]
	flags := flag.NewFlagSet("host "+action, flag.ExitOnError)

	switch action {
	case "create":
		providerName := flags.String("provider", "digitalocean", "Provider (digitalocean, proxmox)")
		name := flags.String("name", "", "Host name")
		size := flags.String("size", "s-4vcpu-8gb", "Host size slug or specs")
		region := flags.String("region", "nyc1", "Provider region")
		daemonVersion := flags.String("daemon-version", "", "Install this native-ops release as the host's daemon from cloud-init (e.g. v1.54.0); needs --daemon-sha256 and a bootstrap token")
		daemonSHA := flags.String("daemon-sha256", "", "SHA-256 of the release's linux tarball (from its checksums.txt)")
		daemonArch := flags.String("daemon-arch", "amd64", "Architecture of the daemon release: amd64 or arm64")
		daemonFlags := flags.String("daemon-flags", "", "Extra `native-ops serve` flags for the daemon, e.g. '--enable-apply --enable-edge-apply'")
		_ = flags.Parse(args[1:])

		if *name == "" {
			log.Fatal("Error: --name is required")
		}

		spec := config.HostSpec{
			Name:     *name,
			Provider: *providerName,
			Size:     *size,
			Region:   *region,
		}

		_, pub, generated := engine.LoadSSHCredentials()
		if *daemonVersion != "" {
			// The token stays with the caller (a CI secret); the host only learns its hash.
			di, err := engine.DaemonInstallFor(&config.DaemonConfig{Version: *daemonVersion, SHA256: *daemonSHA, Arch: *daemonArch, Flags: *daemonFlags})
			if err != nil {
				log.Fatalf("Error: %v", err)
			}
			ud, err := engine.GenerateCloudInitUserDataWith(pub, di)
			if err != nil {
				log.Fatalf("Error: %v", err)
			}
			spec.UserData = ud
		}

		// Register the operator's SSH key first, so the host can be logged in to. A key
		// generated on the fly is refused: it would be lost when this command exits.
		if err := hm.PrepareAccess(ctx, &spec, pub, generated, false); err != nil {
			log.Fatalf("Host creation failed: %v", err)
		}

		host, err := hm.CreateHost(ctx, spec)
		if err != nil {
			log.Fatalf("Host creation failed: %v", err)
		}
		fmt.Printf("Created host %s (%s) at IP: %s\n", host.Name, host.ID, host.PublicIP)
		fmt.Printf("Log in with the private key whose public half was registered: ssh root@%s\n", host.PublicIP)
		if *daemonVersion != "" {
			fmt.Printf("The native-ops daemon %s is being installed by cloud-init; it listens on 127.0.0.1:8686 (put the edge in front of it) and accepts the bootstrap token.\n", *daemonVersion)
		}

	case "destroy":
		providerName := flags.String("provider", "digitalocean", "Provider (digitalocean, proxmox)")
		id := flags.String("id", "", "Host ID to destroy")
		_ = flags.Parse(args[1:])

		if *id == "" {
			log.Fatal("Error: --id is required")
		}

		if err := hm.DestroyHost(ctx, *providerName, *id); err != nil {
			log.Fatalf("Host destruction failed: %v", err)
		}
		fmt.Printf("Destroyed host %s\n", *id)
	}
}

func handleApplyCommand(ctx context.Context, args []string) {
	flags := flag.NewFlagSet("apply", flag.ExitOnError)
	configDir := flags.String("config-dir", ".", "Path to native-ops-conf directory")
	targetService := flags.String("service", "", "Optional specific service to apply (e.g. gitea)")
	_ = flags.Parse(args)

	fleet, err := config.LoadFleetConfig(*configDir)
	if err != nil {
		log.Fatalf("Failed to load fleet config: %v", err)
	}
	fmt.Printf("Applying infrastructure for fleet: %s (%s)\n", fleet.Name, fleet.Domain)

	services, err := config.LoadServices(*configDir, *targetService)
	if err != nil {
		log.Fatalf("%v", err)
	}
	exec := remote.NewLocalExecutor()
	deployer := engine.NewDeployer(exec)
	for _, svcCfg := range services {
		if err := deployer.DeployService(ctx, svcCfg, *configDir); err != nil {
			log.Fatalf("Failed to deploy service %s: %v", svcCfg.Name, err)
		}
	}
	fmt.Println("Apply completed successfully.")
}

func handlePlanCommand(ctx context.Context, args []string) {
	os.Exit(runPlan(ctx, args, os.Stdout, os.Stderr))
}

// runPlan shows what apply would do. It runs behind remote.ReadOnly, so it cannot change the
// host: any command that could is refused. It returns the exit status: 0 nothing to change,
// 2 changes pending, 1 apply would fail on some service or the host could not be read.
func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configDir := flags.String("config-dir", ".", "Path to native-ops-conf directory")
	targetService := flags.String("service", "", "Optional specific service to plan (e.g. gitea)")
	asJSON := flags.Bool("json", false, "Print the plan as JSON")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	fleet, err := config.LoadFleetConfig(*configDir)
	if err != nil {
		fmt.Fprintf(stderr, "Failed to load fleet config: %v\n", err)
		return 1
	}
	fp, err := engine.PlanFleet(ctx, remote.NewLocalExecutor(), *configDir, *targetService)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	if *asJSON {
		out, _ := json.MarshalIndent(fp, "", "  ")
		fmt.Fprintln(stdout, string(out))
	} else {
		who := fleet.Name
		if fleet.Domain != "" {
			who += " (" + fleet.Domain + ")"
		}
		fmt.Fprintf(stdout, "Plan for fleet: %s. Nothing has been changed.\n\n", who)
		fmt.Fprint(stdout, fp.Render())
	}
	return fp.ExitStatus()
}

func handleInstanceCommand(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: native-ops instance [launch|update|resize|destroy|migrate]")
		os.Exit(1)
	}

	action := args[0]
	flags := flag.NewFlagSet("instance "+action, flag.ExitOnError)
	exec := remote.NewLocalExecutor()
	mgr := engine.NewInstanceManager(exec)

	switch action {
	case "launch":
		configDir := flags.String("config-dir", ".", "Path to native-ops-conf")
		templateName := flags.String("template", "platform", "Template name")
		name := flags.String("name", "", "Instance container name (e.g. rest-bistro)")
		slug := flags.String("slug", "", "Tenant slug (e.g. bistro)")
		domain := flags.String("domain", "", "Public domain for Caddy routing")
		_ = flags.Parse(args[1:])

		if *name == "" || *slug == "" {
			log.Fatal("Error: --name and --slug are required")
		}

		tmplPath := filepath.Join(*configDir, "templates", *templateName, "template.yml")
		tmplCfg, err := config.LoadTemplateConfig(tmplPath)
		if err != nil {
			log.Fatalf("Failed to load template %s: %v", tmplPath, err)
		}

		ip, err := mgr.Launch(ctx, engine.LaunchParams{
			Template: tmplCfg,
			Name:     *name,
			Slug:     *slug,
			Domain:   *domain,
		})
		if err != nil {
			log.Fatalf("Launch failed: %v", err)
		}
		fmt.Printf("Instance %s launched successfully at IP %s\n", *name, ip)

	case "update":
		name := flags.String("name", "", "Instance container name")
		image := flags.String("image", "", "New image alias or fingerprint (e.g. my-app:v2)")
		service := flags.String("service", "platform", "systemd unit whose /etc/default/<service> env file is carried over")
		healthPath := flags.String("health-path", "", "HTTP path to gate the update on (enables automatic rollback)")
		healthPort := flags.Int("health-port", 0, "Port for --health-path")
		healthTimeout := flags.Int("health-timeout", 60, "Seconds to wait for the health check")
		noSnapshot := flags.Bool("no-snapshot", false, "Skip the pre-update volume snapshot (not recommended)")
		force := flags.Bool("force", false, "Replace the instance even if it already runs the requested image")
		_ = flags.Parse(args[1:])

		if *name == "" || *image == "" {
			log.Fatal("Error: --name and --image are required")
		}
		if *healthPath != "" && *healthPort <= 0 {
			log.Fatal("Error: --health-port is required with --health-path")
		}

		opts := engine.UpdateOptions{Service: *service, SkipSnapshot: *noSnapshot, Force: *force}
		if *healthPath != "" {
			opts.HealthCheck = config.HealthCheckConfig{Path: *healthPath, Port: *healthPort, Timeout: *healthTimeout}
		}
		if err := mgr.Update(ctx, *name, *image, opts); err != nil {
			log.Fatalf("Update failed: %v", err)
		}
		fmt.Printf("Instance %s updated to %s\n", *name, *image)

	case "resize":
		name := flags.String("name", "", "Instance name")
		cpu := flags.String("cpu", "", "CPU limit (e.g. 2)")
		mem := flags.String("memory", "", "Memory limit (e.g. 2GB)")
		_ = flags.Parse(args[1:])

		if *name == "" {
			log.Fatal("Error: --name is required")
		}

		limits := make(map[string]string)
		if *cpu != "" {
			limits["limits.cpu"] = *cpu
		}
		if *mem != "" {
			limits["limits.memory"] = *mem
		}

		if err := mgr.Resize(ctx, *name, limits); err != nil {
			log.Fatalf("Resize failed: %v", err)
		}
		fmt.Printf("Instance %s resized.\n", *name)

	case "destroy":
		name := flags.String("name", "", "Instance name")
		purgeVol := flags.Bool("purge-volume", false, "Delete associated storage volume")
		_ = flags.Parse(args[1:])

		if *name == "" {
			log.Fatal("Error: --name is required")
		}

		if err := mgr.Destroy(ctx, *name, *purgeVol); err != nil {
			log.Fatalf("Destroy failed: %v", err)
		}
		fmt.Printf("Instance %s destroyed.\n", *name)

	case "migrate":
		src := flags.String("source", "", "Source Incus remote")
		dst := flags.String("target", "", "Target Incus remote")
		name := flags.String("name", "", "Instance name")
		vol := flags.String("volume", "", "Optional: a storage volume name to cross-check against the instance's attached volumes")
		healthPath := flags.String("health-path", "", "HTTP path probed from inside the migrated container (e.g. /health); recommended")
		healthPort := flags.Int("health-port", 80, "Health check port inside the container")
		healthTimeout := flags.Int("health-timeout", 60, "Health check timeout in seconds")
		resume := flags.Bool("resume", false, "Allow an existing stopped copy on the target (interrupted run, or rollback by swapping --source/--target)")
		noSnapshot := flags.Bool("no-snapshot", false, "Skip the pre-migrate snapshot of the source volumes")
		stopTimeout := flags.Int("stop-timeout", 30, "Seconds to wait for the source to stop cleanly")
		finalize := flags.Bool("finalize", false, "Delete the stopped source instance once the target is verified")
		purge := flags.Bool("purge-source-volumes", false, "With --finalize: also delete the source's data volumes")
		_ = flags.Parse(args[1:])

		if *src == "" || *dst == "" || *name == "" {
			log.Fatal("Error: --source, --target, and --name are required")
		}

		var hc config.HealthCheckConfig
		if *healthPath != "" {
			hc = config.HealthCheckConfig{Path: *healthPath, Port: *healthPort, Timeout: *healthTimeout}
		}

		mig := engine.NewMigrationManager(exec)
		if *finalize {
			if err := mig.Finalize(ctx, engine.FinalizeParams{
				SourceRemote: *src, TargetRemote: *dst, InstanceName: *name,
				HealthCheck: hc, PurgeSourceVolumes: *purge,
			}); err != nil {
				log.Fatalf("Finalize failed: %v", err)
			}
			fmt.Printf("Instance %s removed from %s (now running on %s).\n", *name, *src, *dst)
			return
		}
		if err := mig.Migrate(ctx, engine.MigrationParams{
			SourceRemote: *src,
			TargetRemote: *dst,
			InstanceName: *name,
			VolumeName:   *vol,
			HealthCheck:  hc,
			Resume:       *resume,
			SkipSnapshot: *noSnapshot,
			StopTimeout:  *stopTimeout,
		}); err != nil {
			log.Fatalf("Migration failed: %v", err)
		}
		fmt.Printf("Instance %s is running on %s; %s on %s is stopped and intact. Repoint DNS/edge, then run with --finalize.\n", *name, *dst, *name, *src)
	}
}

func handleDNSCommand(ctx context.Context, args []string) {
	flags := flag.NewFlagSet("dns sync", flag.ExitOnError)
	configDir := flags.String("config-dir", ".", "Path to native-ops-conf")
	domain := flags.String("domain", "", "Domain to sync")
	targetIP := flags.String("ip", "", "Target IP address for A records")
	_ = flags.Parse(args)

	fleet, err := config.LoadFleetConfig(*configDir)
	if err != nil {
		log.Fatalf("Load fleet config: %v", err)
	}

	var dnsProv provider.DNSProvider
	if fleet.DNSProvider == "digitalocean" || fleet.DNSProvider == "do" {
		do, err := digitalocean.New("")
		if err != nil {
			log.Fatalf("DigitalOcean provider error: %v", err)
		}
		dnsProv = do
	} else {
		p, err := plugin.NewScriptDNSProvider(fleet.DNSProvider, *configDir)
		if err != nil {
			log.Fatalf("Plugin provider error: %v", err)
		}
		dnsProv = p
	}

	// The computed apex + wildcard A for one domain, when a target IP is given (the original behaviour).
	dom := *domain
	if dom == "" {
		dom = fleet.Domain
	}
	if *targetIP != "" && dom != "" {
		records := []provider.DNSRecord{
			{Type: "A", Name: "@", Value: *targetIP},
			{Type: "A", Name: "*", Value: *targetIP},
		}
		if err := dnsProv.SyncRecords(ctx, dom, records); err != nil {
			log.Fatalf("DNS sync failed: %v", err)
		}
		fmt.Printf("DNS records synced for %s (apex and wildcard -> %s)\n", dom, *targetIP)
	}

	logf := func(format string, a ...any) { fmt.Printf(format+"\n", a...) }
	if err := syncDeclaredRecords(ctx, fleet, dnsProv, logf); err != nil {
		log.Fatal(err)
	}
}

// syncDeclaredRecords creates or updates the records declared in fleet.yml (`dns_records:`), zone by
// zone. A sync never deletes a record it was not told about. `dns sync` and the daemon's
// POST /v1/dns/sync both use it.
func syncDeclaredRecords(ctx context.Context, fleet *config.FleetConfig, dnsProv provider.DNSProvider, logf func(string, ...any)) error {
	byZone, err := fleet.DNSRecordsByZone()
	if err != nil {
		return fmt.Errorf("dns_records: %w", err)
	}
	zones := make([]string, 0, len(byZone))
	for z := range byZone {
		zones = append(zones, z)
	}
	sort.Strings(zones)
	for _, zone := range zones {
		var records []provider.DNSRecord
		var names []string
		for _, r := range byZone[zone] {
			records = append(records, provider.DNSRecord{Type: r.Type, Name: r.Name, Value: r.Value, TTL: r.TTL, Priority: r.Priority})
			names = append(names, fmt.Sprintf("%s %s", r.Type, r.Name))
		}
		if err := dnsProv.SyncRecords(ctx, zone, records); err != nil {
			return fmt.Errorf("DNS sync for %s failed: %w", zone, err)
		}
		logf("DNS records synced for %s: %s", zone, strings.Join(names, ", "))
	}
	if len(zones) == 0 {
		logf("fleet.yml declares no dns_records; nothing to sync")
	}
	return nil
}

func loadBackupStore(configDir string) (*backup.Manager, error) {
	return loadBackupStoreWith(configDir, remote.NewLocalExecutor())
}

// loadBackupStoreWith builds the backup manager from fleet.yml's `backup:` section; the object store's
// credentials come from this process's environment, never from the tree.
func loadBackupStoreWith(configDir string, exec remote.Executor) (*backup.Manager, error) {
	fleet, err := config.LoadFleetConfig(configDir)
	if err != nil {
		return nil, err
	}
	if fleet.Backup == nil {
		return nil, fmt.Errorf("no 'backup:' section in %s/fleet.yml", configDir)
	}
	cfg := fleet.Backup
	if err := cfg.ValidateForBackup(); err != nil {
		return nil, err
	}
	access, secret, err := cfg.Credentials()
	if err != nil {
		return nil, err
	}
	store, err := s3.New(s3.Config{
		Endpoint:  cfg.Endpoint,
		Region:    cfg.Region,
		Bucket:    cfg.Bucket,
		AccessKey: access,
		SecretKey: secret,
		PathStyle: cfg.S3PathStyle(),
	})
	if err != nil {
		return nil, err
	}
	return backup.New(exec, store, cfg), nil
}

func handleBackupCommand(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: native-ops backup [create <volume>|all|list [volume]|restore <volume>|prune [volume]]")
		os.Exit(1)
	}
	action := args[0]
	flags := flag.NewFlagSet("backup "+action, flag.ExitOnError)
	configDir := flags.String("config-dir", ".", "Path to native-ops-conf")
	pool := flags.String("pool", "default", "Incus storage pool")
	from := flags.String("from", "latest", "Object key or 'latest'")
	as := flags.String("as", "", "Import under a new volume name (non-destructive)")
	force := flags.Bool("force", false, "Stop dependent containers to restore in place")
	_ = flags.Parse(args[1:])

	mgr, err := loadBackupStore(*configDir)
	if err != nil {
		log.Fatalf("backup config: %v", err)
	}

	switch action {
	case "init":
		if err := mgr.EnsureBucket(ctx); err != nil {
			log.Fatalf("Bucket init failed: %v", err)
		}
		fmt.Println("Backup bucket is ready.")

	case "create":
		if flags.NArg() < 1 {
			log.Fatal("Error: backup create requires a <volume> name")
		}
		man, err := mgr.CreateVolume(ctx, *pool, flags.Arg(0))
		if err != nil {
			log.Fatalf("Backup failed: %v", err)
		}
		fmt.Printf("Backed up %s (%d bytes, sha256 %s)\n  -> %s\n", man.Volume, man.SizeBytes, man.SHA256[:12], man.Key)

	case "all":
		mans, err := mgr.CreateVolumes(ctx, *pool)
		for _, man := range mans {
			fmt.Printf("  ok  %-24s %10d bytes  %s\n", man.Volume, man.SizeBytes, man.Key)
		}
		if err != nil {
			log.Fatalf("Backup failed: %v", err)
		}
		fmt.Printf("Backed up %d volume(s).\n", len(mans))

	case "list":
		objs, man, err := mgr.ListVolume(ctx, flags.Arg(0))
		if err != nil {
			log.Fatalf("List failed: %v", err)
		}
		if man != nil {
			fmt.Printf("  latest: %s (%d bytes, sha256 %s)\n", man.Key, man.SizeBytes, man.SHA256[:12])
		}
		for _, o := range objs {
			fmt.Printf("  %10d  %s  %s\n", o.Size, o.LastModified.UTC().Format(time.RFC3339), o.Key)
		}

	case "restore":
		if flags.NArg() < 1 {
			log.Fatal("Error: backup restore requires a <volume> name")
		}
		err := mgr.Restore(ctx, backup.RestoreOptions{Pool: *pool, Volume: flags.Arg(0), FromKey: *from, AsName: *as, Force: *force})
		if err != nil {
			log.Fatalf("Restore failed: %v", err)
		}
		fmt.Printf("Restored %s from %s\n", flags.Arg(0), *from)

	case "prune":
		volumes := flags.Args()
		if len(volumes) == 0 {
			del, err := mgr.PruneAll(ctx, *pool)
			if err != nil {
				log.Fatalf("Prune failed: %v", err)
			}
			for _, k := range del {
				fmt.Printf("  deleted %s\n", k)
			}
			break
		}
		for _, v := range volumes {
			del, err := mgr.Prune(ctx, v)
			if err != nil {
				log.Fatalf("Prune failed: %v", err)
			}
			for _, k := range del {
				fmt.Printf("  deleted %s\n", k)
			}
		}

	default:
		fmt.Fprintf(os.Stderr, "Unknown backup action: %s\n", action)
		os.Exit(1)
	}
}

func handleImageCommand(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: native-ops image build <app> <ref> --config-dir <dir>\n       native-ops image recipe-digest --config-dir <dir>")
		os.Exit(1)
	}
	action := args[0]
	flags := flag.NewFlagSet("image "+action, flag.ExitOnError)
	configDir := flags.String("config-dir", ".", "Path to native-ops-conf")
	_ = flags.Parse(args[1:])
	if action == "recipe-digest" {
		// What a daemon checks before it builds from this tree (see POST /v1/images/recipes/{digest}/approve).
		d, err := engine.RecipeDigest(*configDir)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(d)
		return
	}
	if action != "build" {
		fmt.Fprintf(os.Stderr, "Unknown image action: %s\n", action)
		os.Exit(1)
	}
	if flags.NArg() < 2 {
		log.Fatal("Error: image build requires <app> and <ref>")
	}
	exec := remote.NewLocalExecutor()
	logf := func(format string, a ...any) { fmt.Printf(format, a...) }
	if err := engine.BuildImage(ctx, exec, *configDir, flags.Arg(0), flags.Arg(1), logf); err != nil {
		log.Fatalf("Image build failed: %v", err)
	}
	fmt.Printf("Built image %s@%s\n", flags.Arg(0), flags.Arg(1))
}

func handlePreviewCommand(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: native-ops preview [launch <app> <ref>|list|destroy <name>|gc]")
		os.Exit(1)
	}
	action := args[0]
	flags := flag.NewFlagSet("preview "+action, flag.ExitOnError)
	configDir := flags.String("config-dir", ".", "Path to native-ops-conf")
	ttl := flags.Duration("ttl", 72*time.Hour, "Preview lifetime")
	_ = flags.Parse(args[1:])

	exec := remote.NewLocalExecutor()
	mgr := engine.NewPreviewManager(exec)

	switch action {
	case "launch", "create":
		if flags.NArg() < 2 {
			log.Fatal("Error: preview launch requires <app> and <ref>")
		}
		ip, name, err := mgr.Launch(ctx, engine.PreviewParams{
			ConfigDir: *configDir, App: flags.Arg(0), Ref: flags.Arg(1), TTL: *ttl,
		})
		if err != nil {
			log.Fatalf("Preview launch failed: %v", err)
		}
		fmt.Printf("Preview %s live at %s (ttl %s)\n", name, ip, ttl.String())

	case "list":
		list, err := mgr.List(ctx)
		if err != nil {
			log.Fatalf("Preview list failed: %v", err)
		}
		for _, p := range list {
			exp := "-"
			if !p.Expires.IsZero() {
				exp = p.Expires.UTC().Format(time.RFC3339)
			}
			fmt.Printf("  %-40s app=%-10s ref=%-20s expires=%s running=%v\n", p.Name, p.App, p.Ref, exp, p.Running)
		}

	case "destroy":
		if flags.NArg() < 1 {
			log.Fatal("Error: preview destroy requires <name>")
		}
		if err := mgr.Destroy(ctx, flags.Arg(0)); err != nil {
			log.Fatalf("Preview destroy failed: %v", err)
		}
		fmt.Printf("Destroyed preview %s\n", flags.Arg(0))

	case "gc":
		deleted, err := mgr.Gc(ctx, time.Now().UTC())
		if err != nil {
			log.Fatalf("Preview gc failed: %v", err)
		}
		for _, n := range deleted {
			fmt.Printf("  removed %s\n", n)
		}
		fmt.Printf("Removed %d expired preview(s).\n", len(deleted))

	default:
		fmt.Fprintf(os.Stderr, "Unknown preview action: %s\n", action)
		os.Exit(1)
	}
}
