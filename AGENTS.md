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
5. **Runner-Centric Control Plane**: The engine runs directly in CI/CD runners (GitHub Actions, Gitea Actions) or operator workstations, executing commands over Cloud APIs and SSH/Incus remotes.

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
├── cmd/native-ops/           # CLI entrypoint
├── pkg/
│   ├── config/               # YAML schema parser for fleet.yml, services, templates
│   ├── provider/             # ComputeProvider & DNSProvider interfaces
│   │   ├── digitalocean/     # DigitalOcean API client
│   │   ├── proxmox/          # Proxmox VE REST API client
│   │   └── plugin/           # External Python/Bash DNS provider plugin runner
│   ├── remote/               # Local and SSH command execution engines
│   ├── incus/                # Incus control plane client
│   ├── caddy/                # Caddy reverse proxy router & reload triggers
│   └── engine/               # Orchestration pipelines (deployer, instance, host, migration)
├── incus/                    # Default Incus base profiles (base, service, edge, ci)
├── images/                   # Generic base and edge container recipes
└── scripts/                  # Generic host provisioning and helper scripts
```
