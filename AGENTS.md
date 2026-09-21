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
2. The manager is the orchestrator. It controls Incus by SSHing back to the
   host as `manager-ctl` (incus-admin group, no sudo) and running the
   `incus` CLI there — see "Manager -> Incus control plane" below for why.
3. No Docker in the deploy path. Incus native only.
4. DNS is the only provider-specific piece (DO API → dns.sh).
5. UFW must allow incusbr0 traffic; otherwise containers get no network.

## Manager -> Incus control plane

The manager container has no Incus socket or CLI of its own. It reaches
Incus over SSH to the host's `manager-ctl` user (created by
`provision-host.sh`, in the `incus-admin` group so it can talk to
`/var/lib/incus/unix.socket` without sudo or root). `deploy-manager.sh`
pushes the private half of `/root/.ssh/manager_incus_ed25519` into the
container on every deploy, at `/home/manager/.ssh/incus_ctl`.

This was chosen over bind-mounting `/var/lib/incus` (the socket) into the
manager container, which would need root or a group mapping fight inside an
unprivileged container to actually use, and would hand the manager the same
blast radius as host root. SSH as a scoped, no-sudo, incus-admin-only user
is an equivalent, easier-to-reason-about restriction: the account can drive
`incus`, and nothing else. It also mirrors the (already load-bearing)
pattern of the `fleet` user model DO-era instances used. See
`management/lib/incus.mjs` for the client side.

## Common gotchas

- `incus config set environment.*` does NOT affect systemd services. Use
  EnvironmentFile in the unit instead.
- Volume attach: `incus config device add` needs full host path, not just
  volume name. Path must not contain `/` in the device name.
- Order matters when a device's mount path is one a container write targets
  (e.g. `/app/.data/env`): attach the device *before* writing under it. A
  write that lands first goes to the container's ephemeral rootfs copy of
  that path, which the device mount then shadows — the write is silently
  lost, not merged or errored.
- UFW drops Incus bridge traffic by default. The provision script adds
  `ufw allow in on incusbr0` and route rules.
- Image aliases with `:` (e.g., `opsavor-manager:latest`) confuse the
  `incus launch` CLI. Use the image fingerprint — but note
  `incus image alias list`'s box-drawn table output is NOT safe to `awk
  '{print $N}'` on: the │ separators are their own whitespace-split fields,
  so a naive column index silently grabs the wrong column (this cost real
  hours — three scripts had it grabbing the alias back instead of the
  fingerprint, which produced the exact colon string this gotcha warns
  about). Use `scripts/lib.sh`'s `image_fingerprint()` (parses `--format
  json`) instead of `awk` on the table.
- `incus snapshot create <name> <snapshot>` snapshots an *instance*.
  Snapshotting a custom storage volume is a different command:
  `incus storage volume snapshot create <pool> <volume> [<snapshot>]`.
