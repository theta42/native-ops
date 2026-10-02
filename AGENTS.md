# AGENTS.md — theta42/native-ops

## Project Overview

`native-ops` is a generic, open-source Infrastructure-as-Code (IaC) and fleet orchestration engine written in Go. It automates:
1. **Level 0 (Cloud/Hypervisor VMs)**: Host provisioning, resizing, and destruction on DigitalOcean Droplets, Proxmox VE KVM/LXC, or on-prem nodes.
2. **Level 1 (Incus Workloads & Edge Proxy)**: Immutable container lifecycle, `security.shifted=true` persistent storage volumes, live cgroup limits, and dynamic Caddy edge routing.
3. **Level 2 (Workload Mobility)**: Cross-host container and volume migration (`native-ops instance migrate`).

## Core Architecture Principles

1. **Immutable Workloads**: Never live-patch a running container. Updates rebuild/pull a new image, snapshot persistent volumes, delete the old container, launch the replacement, reattach data volumes, and health-gate before updating edge routes.
2. **Persistent Volumes with ID Mapping**: Volumes use `security.shifted=true` and are attached to container mount points *before* writing configuration files or starting services.
3. **Environment Injection**: Configuration and secrets are injected via `/etc/default/<service>` (EnvironmentFile pattern), never `incus config set environment.*`.
4. **Declarative Manifests**: The engine is driven by a separate configuration repository (`native-ops-conf`) containing `fleet.yml`, `services/*/service.yml`, `templates/*/template.yml`, and optional provider plugins.
5. **Git + CI is the only control path; a daemon runs on each host**: nobody runs a control app on their own machine. Each host runs `native-ops serve` (installed by cloud-init or IaC); CI drives it over HTTPS with scoped API tokens (`native-ops remote ...`), uploading the checked-out config tree. Changes to a host are gated: an apply runs only an admin-approved plan hash, an image build only an admin-approved recipe digest, and a scoped token only touches its own instances. `reconcile` brings hosts up (provision, DNS, Incus); it deploys services only with the break-glass `--deploy-services`.
6. **Generic, not one deployment**: nothing deployment-specific (names, domains, image prefixes) belongs in the engine; it comes from the config repo or daemon flags.

## Build and Test

```bash
# Run unit tests
go test -v ./...

# Build local binary
go build -o bin/native-ops ./cmd/native-ops

# Check CLI commands
./bin/native-ops --help
```

## Repository Layout

```
native-ops/
├── cmd/native-ops/           # CLI entrypoint (incl. `serve` and the CI client `remote`)
├── pkg/
│   ├── config/               # YAML schema parser for fleet.yml, services, templates
│   ├── provider/             # ComputeProvider & DNSProvider interfaces
│   │   ├── digitalocean/     # DigitalOcean API client
│   │   ├── proxmox/          # Proxmox VE REST API client
│   │   └── plugin/           # External Python/Bash DNS provider plugin runner
│   ├── remote/               # Local and SSH command execution engines
│   ├── incus/                # Incus control plane client
│   ├── caddy/                # Caddy reverse proxy router & reload triggers
│   ├── engine/               # Orchestration pipelines (deployer, instance, host, migration)
│   ├── server/               # The daemon: API, auth (tokens, sessions, OIDC), plans, jobs, UI
│   ├── backup/ s3/           # Volume backup/restore to S3-compatible storage
│   └── status/               # Read-only host snapshot
├── deploy/                   # systemd unit for the daemon (embedded into cloud-init)
├── incus/                    # Default Incus base profiles (base, service, edge, ci)
├── images/                   # Generic base and edge container recipes
└── scripts/                  # Generic host provisioning and helper scripts
```
