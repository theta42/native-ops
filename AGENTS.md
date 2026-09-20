# AGENTS.md — native-ops

## What this repo is

Infrastructure-as-files for opsavor.app: Incus/LXC containers, image builds,
edge proxy, fleet orchestration, DNS, and runbooks. Replaces the old do-ops
(DO/Docker) model with a portable, provider-agnostic Incus-native stack.

## Build and test

```bash
# All scripts are bash. Run them on the Incus host itself.
./scripts/provision-host.sh        # bootstrap a fresh VPS
./scripts/build-image.sh base       # build the base image
./scripts/build-image.sh edge       # build edge (Caddy) image
./scripts/build-image.sh manager    # build manager image at tag
./scripts/deploy-manager.sh         # deploy/replace manager container
./scripts/onboard-restaurant.sh test "Test" owner@test.com pass123  # onboard
```

## Key files

- `incus/preseed.yml` — Incus init config (dir storage, incusbr0 bridge)
- `incus/profiles/` — 5 profiles: base, edge, restaurant, service, ci
- `images/*/build.sh` — Image build recipes (run inside temp containers)
- `edge/Caddyfile` — Edge reverse proxy config (wildcard TLS via DO DNS-01)
- `scripts/*.sh` — Host provisioning, image building, deploy, onboarding
- `providers/digitalocean/` — DO-specific scripts (DNS only — compute is gone)

## Principles

1. Containers are immutable. Rebuild and replace, never patch.
2. The manager is the orchestrator. It talks to the Incus API.
3. No Docker in the deploy path. Incus native only.
4. DNS is the only provider-specific piece (DO API → dns.sh).
5. UFW must allow incusbr0 traffic; otherwise containers get no network.

## Common gotchas

- `incus config set environment.*` does NOT affect systemd services. Use
  EnvironmentFile in the unit instead.
- Volume attach: `incus config device add` needs full host path, not just
  volume name. Path must not contain `/` in the device name.
- UFW drops Incus bridge traffic by default. The provision script adds
  `ufw allow in on incusbr0` and route rules.
- Image aliases with `:` (e.g., `opsavor-manager:latest`) confuse the
  `incus launch` CLI. Use the image fingerprint or split with =.
