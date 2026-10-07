# AGENTS.md — theta42/native-ops

## Project Overview

`native-ops` is a generic, open-source Infrastructure-as-Code (IaC) and fleet orchestration engine written in Go. It automates:
1. **Level 0 (Cloud/Hypervisor VMs)**: Host provisioning, resizing, and destruction on DigitalOcean Droplets (Proxmox VE is experimental, #45), or adopting existing hosts (`provider: static`).
2. **Level 1 (Incus Workloads & Edge Proxy)**: Immutable container lifecycle, `security.shifted=true` persistent storage volumes, live cgroup limits, and dynamic Caddy edge routing.
3. **Level 2 (Workload Mobility)**: Cross-host container and volume migration (`native-ops instance migrate`).

## Core Architecture Principles

1. **Immutable Workloads**: Never live-patch a running container. Updates rebuild/pull a new image, snapshot persistent volumes, delete the old container, launch the replacement, reattach data volumes, and health-gate before updating edge routes.
2. **Persistent Volumes with ID Mapping**: Volumes use `security.shifted=true` and are attached to container mount points *before* writing configuration files or starting services.
3. **Environment Injection**: Configuration and secrets are injected via `/etc/default/<service>` (EnvironmentFile pattern), never `incus config set environment.*`.
4. **Declarative Manifests**: The engine is driven by a separate configuration repository (`native-ops-conf`) containing `fleet.yml`, `services/*/service.yml`, `templates/*/template.yml`, and optional provider plugins.
5. **Git + CI is the only control path; a daemon runs on each host**: nobody runs a control app on their own machine. Each host runs `native-ops serve` (installed by cloud-init or IaC); CI drives it over HTTPS with scoped API tokens (`native-ops remote ...`), uploading the checked-out config tree. Changes to a host are gated: an apply runs only an admin-approved plan hash, an image build only an admin-approved recipe digest, and a scoped token only touches its own instances. `reconcile` brings hosts up (provision, DNS, Incus); it deploys services only with the break-glass `--deploy-services`.
6. **Secrets are entered in the git server, kept by the daemon**: the daemon's own credentials are synced from the git server's secret store (`PUT /v1/secrets`, `native-ops remote secret-sync`) into its state directory and read through `SecretStore.Lookup` (store, then environment). A service gets its secrets the same way: its manifest's `env_from` names `SERVICE_*` secrets, which apply resolves from the store (never the daemon's environment) into `/etc/default/<service>`. Never put a credential in a manifest, a command line, a log, a job record or an answer.
7. **Generic, not one deployment**: nothing deployment-specific (names, domains, image prefixes) belongs in the engine; it comes from the config repo or daemon flags.

## Build and Test

```bash
# What CI runs (.github/workflows/pr-test.yml); all of it must pass before a merge
gofmt -l .                      # must print nothing
go mod tidy && git diff --exit-code go.mod go.sum
go vet ./...
go test -race ./...
node --check pkg/server/ui/static/js/app.js

# Against a real Incus server (on a host; not run in CI)
go test -tags integration ./pkg/incus

# Build local binary
go build -o bin/native-ops ./cmd/native-ops

# Check CLI commands (README.md's CLI Usage block must match this output; a test checks it)
./bin/native-ops
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

## Testing conventions

- Engine code never talks to Incus directly: it runs commands through `remote.Executor`, and tests
  use a simulator (`hostSim` in `pkg/engine`, `edgeSim` in `pkg/caddy`) or a recording fake. File
  contents and scripts go over stdin (`RunWithInput`), never in a command line.
- Provider clients (DigitalOcean, Proxmox, S3) are tested against `httptest` fakes of their APIs.
- Every daemon endpoint has tests for its role gate (no token, viewer, scoped token) and its job.
- Plan and apply share their decisions; the idempotency tests run both against the same states.
