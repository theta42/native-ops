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
  EnvironmentFile in the unit instead. The same is true of `incus exec`:
  it does NOT forward the calling shell's environment into the container
  either (only `incus config set environment.*` or an explicit `--env
  KEY=VALUE` on the exec call itself reaches the executed command) — so
  `FOO=bar incus exec ct -- some-script` does NOT make `$FOO` visible
  inside `some-script`, the same way it wouldn't for a plain systemd unit.
- Attaching a custom volume by its raw host path (`source=/var/lib/incus/
  storage-pools/<pool>/custom/<pool>_<vol>`) bind-mounts it directly and
  skips Incus's own volume config — notably `security.shifted`, which is
  what lets an unprivileged container's own uid/gid mapping apply to the
  volume's contents. Attach it by name instead — `disk pool=<pool>
  source=<vol> path=<path>` — and the volume's own config (including
  security.shifted) actually takes effect at mount time. Without shifting,
  the volume directory is host-root-owned (typically mode 711), and
  neither a non-root user inside the container nor an unprivileged host
  SSH user (see "Manager -> Incus control plane" above) can write to it;
  WITH it, root inside the container — which `incus exec` always runs
  as — can freely chown/chmod its own view of the mount for whatever
  user actually needs it. (The device name itself still can't contain
  `/`, regardless of which form `source=` takes.)
- One Caddy site block gets one cert. A wildcard block (`*.opsavor.work
  { tls { dns ... } ... }`) and a second, more specific block for one of
  its own subdomains (`wiki.opsavor.work { tls { dns ... } ... }`) are
  TWO separate site definitions to Caddy — the specific one issues its
  OWN individual cert rather than reusing the wildcard, silently defeating
  the point of having one. To serve multiple names under one wildcard
  cert, keep them all in the SAME site block and split on `@matcher` /
  `handle` by Host header instead (see `*.opsavor.app` and `*.opsavor.work`
  in edge/Caddyfile).
- Order matters when a device's mount path is one a container write targets
  (e.g. `/app/.data/env`): attach the device *before* writing under it. A
  write that lands first goes to the container's ephemeral rootfs copy of
  that path, which the device mount then shadows — the write is silently
  lost, not merged or errored.
- UFW drops Incus bridge traffic by default. The provision script adds
  `ufw allow in on incusbr0` and route rules.
- Image aliases with `:` (e.g., `opsavor-manager:latest`) confuse the
  `incus launch` CLI, AND `incus image alias create`'s own `<new alias
  name>` argument — both parse it as `[<remote>:]<name>` on the first
  colon, so `opsavor-restaurant:latest` becomes remote
  "opsavor-restaurant" (which doesn't exist) rather than a literal alias.
  Prefix an explicit `local:` remote to make a colon-bearing alias name
  parse as intended: `incus image alias create local:opsavor-restaurant:latest
  <fingerprint>`. (`incus publish --alias` does NOT have this problem —
  it accepts the colon-bearing string directly.) Also note `incus image
  alias create` has no `--reuse` flag (only `incus publish` does); to
  repoint an existing alias, delete then create.

  For `incus launch`/`incus config`, sidestep all of this by using the image
  fingerprint instead of the alias — but note
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
- The `base` profile's 512MB memory limit is sized for single-process
  services. A multi-process OCI app container (e.g. Plane's all-in-one
  image, which runs API + worker + beat + 3 frontends + a realtime server
  + Caddy all in one instance) will get OOM-killed inside its own memcg
  under that limit — and it does NOT look like an OOM kill from inside the
  container: processes just die and get respawned by supervisor/whatever
  entrypoint loop over and over (e.g. Django's `wait_for_db` appearing to
  hang forever, when actually each attempt is getting SIGKILLed a few
  seconds in). The container's own stderr logs show `Killed` with no
  further explanation. Confirm with `dmesg -T | grep oom-kill` on the
  *host* (filter by `cpuset=lxc.payload.<name>`) before assuming a
  networking/DB issue. Fix by overriding `limits.memory`/`limits.cpu` at
  the instance level (`incus launch ... --config limits.memory=3GB`),
  not by raising the shared profile.
