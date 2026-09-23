package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/engine"
	"github.com/theta42/native-ops/pkg/provider"
	"github.com/theta42/native-ops/pkg/provider/digitalocean"
	"github.com/theta42/native-ops/pkg/provider/plugin"
	"github.com/theta42/native-ops/pkg/remote"
)

const Version = "v0.1.0"

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

	case "host":
		handleHostCommand(ctx, os.Args[2:])

	case "apply":
		handleApplyCommand(ctx, os.Args[2:])

	case "instance":
		handleInstanceCommand(ctx, os.Args[2:])

	case "dns":
		handleDNSCommand(ctx, os.Args[2:])

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", subcommand)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`native-ops - Generic Incus & Cloud Fleet Orchestration Engine (theta42)

Usage:
  native-ops <command> [options]

Commands:
  host create      Provision a new cloud host / VM (DigitalOcean, Proxmox)
  host destroy     Tear down a host VM
  host list        List active hosts for a provider
  apply            Declaratively apply services from native-ops-conf
  instance launch  Launch a dynamic workload from a template
  instance update  Immutable container update for an instance
  instance resize  Live CPU/memory cgroup resizing
  instance destroy Delete an instance and its Caddy route
  instance migrate Move instance and volume across Incus remotes
  dns sync         Sync DNS records using configured provider or python plugin
  version          Print version information`)
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

		host, err := hm.CreateHost(ctx, spec)
		if err != nil {
			log.Fatalf("Host creation failed: %v", err)
		}
		fmt.Printf("Created host %s (%s) at IP: %s\n", host.Name, host.ID, host.PublicIP)

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

	exec := remote.NewLocalExecutor()
	deployer := engine.NewDeployer(exec)

	servicesDir := filepath.Join(*configDir, "services")
	entries, err := os.ReadDir(servicesDir)
	if err != nil {
		log.Fatalf("Failed to read services directory %s: %v", servicesDir, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		svcName := entry.Name()
		if *targetService != "" && *targetService != svcName {
			continue
		}

		svcFile := filepath.Join(servicesDir, svcName, "service.yml")
		if _, err := os.Stat(svcFile); os.IsNotExist(err) {
			continue
		}

		svcCfg, err := config.LoadServiceConfig(svcFile)
		if err != nil {
			log.Fatalf("Error loading %s: %v", svcFile, err)
		}
		if svcCfg.Name == "" {
			svcCfg.Name = svcName
		}

		if err := deployer.DeployService(ctx, svcCfg, *configDir); err != nil {
			log.Fatalf("Failed to deploy service %s: %v", svcName, err)
		}
	}
	fmt.Println("Apply completed successfully.")
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
		image := flags.String("image", "", "New image ref or fingerprint")
		service := flags.String("service", "platform", "Service name inside container")
		_ = flags.Parse(args[1:])

		if *name == "" || *image == "" {
			log.Fatal("Error: --name and --image are required")
		}

		if err := mgr.Update(ctx, *name, *image, *service); err != nil {
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
		vol := flags.String("volume", "", "Storage volume name")
		_ = flags.Parse(args[1:])

		if *src == "" || *dst == "" || *name == "" {
			log.Fatal("Error: --source, --target, and --name are required")
		}

		mig := engine.NewMigrationManager(exec)
		if err := mig.Migrate(ctx, engine.MigrationParams{
			SourceRemote: *src,
			TargetRemote: *dst,
			InstanceName: *name,
			VolumeName:   *vol,
		}); err != nil {
			log.Fatalf("Migration failed: %v", err)
		}
		fmt.Printf("Instance %s migrated from %s to %s\n", *name, *src, *dst)
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

	dom := *domain
	if dom == "" {
		dom = fleet.Domain
	}

	records := []provider.DNSRecord{
		{Type: "A", Name: "@", Value: *targetIP},
		{Type: "A", Name: "*", Value: *targetIP},
	}

	var dnsProv provider.DNSProvider
	if fleet.DNSProvider == "digitalocean" || fleet.DNSProvider == "do" {
		do, err := digitalocean.New("")
		if err != nil {
			log.Fatalf("DigitalOcean provider error: %v", err)
		}
		dnsProv = do
	} else {
		// Fallback to python/script plugin
		p, err := plugin.NewScriptDNSProvider(fleet.DNSProvider, *configDir)
		if err != nil {
			log.Fatalf("Plugin provider error: %v", err)
		}
		dnsProv = p
	}

	if err := dnsProv.SyncRecords(ctx, dom, records); err != nil {
		log.Fatalf("DNS sync failed: %v", err)
	}
	fmt.Printf("DNS records synced for %s (apex and wildcard -> %s)\n", dom, *targetIP)
}
