# Opsavor native ops (`native-ops`)

Infrastructure-as-files for `opsavor.app` on Incus/LXC: edge proxy, fleet
orchestrator, internal services, and per-restaurant containers — all on a
single host (Phase 1), scaling to multi-host with live migration (Phase 2).
No Docker, no droplet-per-restaurant. Containers are replaced, never patched.
This repo replaces `do-ops`; code lives in `opsavor/restaurant` (app) and
`opsavor/management` (fleet API); this repo is everything around them.

## Design principles

- **Immutable.** Every workload runs from an Incus image. Updates mean: build
  a new image, launch a new container, delete the old one. No `apt upgrade`
  inside a running container, no `docker compose build` on a live box.
- **Portable.** Every container is an LXC instance managed by the Incus API.
  Any container can be snapshotted, copied, or moved between hosts with
  `incus move`. Phase 2 adds live migration for zero-downtime host
  maintenance.
- **Manager-as-orchestrator.** The `manager` container talks to the Incus
  Unix socket (`/var/lib/incus/unix.socket`) to launch, stop, snapshot, and
  destroy containers. What `do-ops` did with the DigitalOcean API and SSH,
  the manager now does with the Incus API — same dashboard, same endpoints,
  different substrate.
- **Fail closed.** No public listener except the edge proxy (ports 80/443
  forwarded via Incus `proxy` device). All inter-container traffic is on
  `incusbr0` (10.0.100.0/24). A container with a missing profile has no
  network and no disk — it does not fail open.
- **Secrets on the host, injected at launch.** All tokens live in
  `/root/.env` (chmod 600, gitignored). The manager reads them via
  `incus config set` at container create time. No `.env` file inside any
  container image.

## Topology — Phase 1 (single host)

```
                        git.opsavor.app (Gitea container, self-hosted)
                        org `opsavor`: restaurant / management / native-ops
                                     |
            +------------------------+--------------------------------+
            | push manager-v* / restaurant-v* (tags deploy)           |
            v                                                         v
opsavor-node-1 (nyc1, 4vCPU / 8GB / 160GB, Debian 13)        temp build ct
  Incus daemon (ZFS pool `default`, incusbr0 10.0.100.0/24)  (per release,
  │                                                            then deleted)
  ├─ edge          10.0.100.10   Caddy, DNS-01 wildcard       v
  │    ├─ ports 80/443 via incus proxy devices          image: restaurant
  │    ├─ manage.opsavor.app → manager.incus:3001         vX.Y.Z
  │    ├─ git.opsavor.app    → gitea.incus:3000    (published to local
  │    ├─ plane.opsavor.app  → plane.incus:3001      image store)
  │    ├─ wiki.opsavor.app   → bookstack.incus:80
  │    ├─ *.opsavor.app (LE wildcard, DO DNS-01)
  │    └─ /opt/sites/<slug>.caddy (manager-written via incus file push)
  │
  ├─ manager       10.0.100.11   Fleet orchestrator (:3001)
  │    ├─ fleet.db (ZFS volume: manager-data)
  │    ├─ Incus socket mounted → launches/stops/snapshots containers
  │    └─ Caddy sites written via incus file push → caddy reload
  │
  ├─ gitea         10.0.100.12   Git hosting + Actions
  │    ├─ repos: restaurant, management, native-ops
  │    └─ Actions dispatcher → ct-runner (below)
  │
  ├─ ct-runner     10.0.100.13   Gitea Actions runner (privileged)
  │    ├─ runs test suites, builds Incus images
  │    └─ talks to Incus socket for temp build containers
  │
  ├─ plane         10.0.100.14   Project management
  ├─ bookstack     10.0.100.15   Internal wiki/docs
  │
  ├─ rest-sicily   10.0.100.101  Next.js standalone + SQLite (:3000)
  ├─ rest-<slug>   10.0.100.1xx  one container per restaurant
  │                                ↑ replaced on update, never patched
  │
  └─ ZFS volumes (custom, on pool `default`):
       manager-data, gitea-data, plane-data, bookstack-data,
       rest-sicily-data, rest-<slug>-data
```

- **DNS** (`opsavor.app` in DO DNS): apex + `*` A-records → node-1 public IP
  (`scripts/add-edge-dns.sh`). Per-site DNS is never needed (wildcard).
- **TLS**: Caddy LE wildcard via DNS-01 (`DO_API_TOKEN`), covers `manage.*`,
  `git.*`, `plane.*`, `wiki.*`, `*.opsavor.app`. Per-site blocks re-declare
  `tls { dns … }`.
- **Private traffic only**: Caddy proxies to container IPs on `incusbr0`.
  No container has a public IP. The host firewall (nftables) allows `:80`,
  `:443`, and `:22` from admin CIDR only. No other inbound.
- **No Docker iptables**: LXC containers bind to their own interfaces inside
  the bridge. Incus `proxy` devices handle public port forwarding at the
  host level. The old Docker-bypasses-ufw footgun does not exist.

## Topology — Phase 2 (multi-host)

```
opsavor-node-1 (control plane)          opsavor-node-2 … N (workers)
  ├─ edge                                ├─ rest-<slug> (migrated)
  ├─ manager ── Incus API ──────────────→│    live migration via
  ├─ gitea                               │    incus move --target
  ├─ ct-runner                           ├─ rest-<slug>
  └─ rest-sicily (stable tenants)        └─ overflow capacity

  OVN overlay network (replaces incusbr0 for cross-host routing)
  Ceph RBD or ZFS replication for cross-host volumes
```

Phase 2 triggers when node-1 is capacity-bound (see Capacity section). The
manager learns `--target <node>` on launch calls. Edge and gitea stay on
node-1; restaurant containers are the movable units. Live migration
(`incus move`) requires CRIU on both endpoints and a shared storage pool.

## Incus setup

Provisioned by `incus/preseed.yml` (non-interactive `incus admin init
--preseed`). ZFS pool on the root disk; `incusbr0` managed bridge with NAT.

### Profiles

| profile | cpu | mem | nesting | privileged | purpose |
|---|---|---|---|---|---|
| `base` | 1 | 512MB | no | no | inherited by everything |
| `edge` | 1 | 256MB | no | no | Caddy + proxy devices for :80/:443 |
| `service` | 2 | 2GB | yes | no | gitea, plane, bookstack |
| `ci` | 2 | 4GB | yes | **yes** | ct-runner (Incus-in-Incus for image builds) |
| `restaurant` | 1 | 1GB | yes | no | per-site Next.js + SQLite |

The `ci` profile mounts the host Incus socket into the container so the CI
runner can launch sibling containers for image builds. It is the only
privileged container on the host.

### Projects

| project | containers | purpose |
|---|---|---|
| `default` | edge, manager, gitea, ct-runner | core platform |
| `services` | plane, bookstack | internal tooling |
| `tenants` | rest-sicily, rest-\<slug\> | customer workload isolation |

Projects provide RBAC scoping: a leaked tenant token cannot read the
`default` project's containers. The manager holds a client certificate
scoped to `default + tenants`.

## Container inventory

| container | project | profiles | ip | resources | serves |
|---|---|---|---|---|---|
| `edge` | default | base, edge | 10.0.100.10 | 1C / 256MB | ports 80/443 (proxy devices) |
| `manager` | default | base, service | 10.0.100.11 | 2C / 2GB | :3001 (via edge Caddy) |
| `gitea` | default | base, service | 10.0.100.12 | 2C / 2GB | :3000 (via edge Caddy) |
| `ct-runner` | default | base, ci | 10.0.100.13 | 2C / 4GB | outbound-only |
| `plane` | services | base, service | 10.0.100.14 | 2C / 2GB | :3001 (via edge Caddy) |
| `bookstack` | services | base, service | 10.0.100.15 | 1C / 1GB | :80 (via edge Caddy) |
| `rest-sicily` | tenants | base, restaurant | 10.0.100.101 | 1C / 1GB | :3000 (via edge Caddy) |
| `rest-<slug>` | tenants | base, restaurant | 10.0.100.1xx | 1C / 1GB | :3000 (via edge Caddy) |

Container IPs are assigned by the manager at create time via
`incus config set <name> volatile.eth0.ipv4.address …`. The edge Caddyfile
references `manager.incus:3001`, `gitea.incus:3000`, etc. — Incus's managed
bridge resolves `<name>.incus` to the container IP automatically.

## Images

No Docker Hub. Images are built locally on the Incus host and published to
the local image store (`incus image list`).

| image alias | base | built from | built by |
|---|---|---|---|
| `opsavor-base` | `images:debian/13` | `images/base/` (build script) | ct-runner |
| `opsavor-edge` | `opsavor-base` | `images/edge/` — Caddy binary + Caddyfile | ct-runner |
| `opsavor-manager` | `opsavor-base` | `images/manager/` — `npm ci` + `server.mjs` | ct-runner |
| `opsavor-restaurant` | `opsavor-base` | `images/restaurant/` — `npm ci && npm run build` + standalone | ct-runner |
| `opsavor-gitea` | `opsavor-base` | `images/gitea/` — Gitea binary + config | ct-runner |
| `opsavor-plane` | `opsavor-base` | `images/plane/` — Plane distribution | one-time manual |
| `opsavor-bookstack` | `opsavor-base` | `images/bookstack/` — BookStack + PHP | one-time manual |

### Build pipeline (no Docker)

```
ct-runner receives: build image opsavor-restaurant from tag restaurant-v1.2.3
  │
  ├─ 1. incus launch images:debian/13 tmp-build-<tag> --profile base --profile ci
  │
  ├─ 2. incus file push (or git clone) the repo at the tag into the build ct
  │
  ├─ 3. incus exec tmp-build-<tag> -- bash /build/build.sh
  │       (npm ci, npm run build, prune to standalone, write metadata.yaml)
  │
  ├─ 4. Verify: incus exec tmp-build-<tag> -- curl -fsS localhost:3000/api/health
  │       (boots the standalone briefly, health-checks, stops)
  │
  ├─ 5. incus publish tmp-build-<tag> --alias opsavor-restaurant-vX.Y.Z
  │       compression: zstd (fast, ~2x better than gzip)
  │
  └─ 6. incus delete tmp-build-<tag>
```

The build container is temporary and deleted after publish. The published
image is the immutable artifact. A restaurant container update means:
`incus launch opsavor-restaurant-vX.Y.Z rest-<slug>-new` → health-gate →
swap Caddy route → `incus delete rest-<slug>-old`.

The `opsavor-base` image (Debian 13 + node:22 + curl + ca-certificates +
zfsutils) is built once and cached locally. All service images extend it.
The ct-runner cache is warm between builds via a persistent profile setting
on the CI container.

## Networking

**Bridge**: `incusbr0` (10.0.100.1/24, NAT, managed DNS). All containers get
eth0 on this bridge. `<name>.incus` resolves via the bridge's dnsmasq.

**Public exposure**: only the `edge` container, via Incus `proxy` devices
(listen 0.0.0.0:80 → 127.0.0.1:80, 0.0.0.0:443 → 127.0.0.1:443 on the host).
All other services are internal-only, reached through edge Caddy.

**Internal naming**: Caddy proxies use `<container-name>.incus:<port>`.
No hardcoded IPs in config — the bridge DNS handles container IP changes.

**Phase 2**: replace per-host `incusbr0` with OVN overlay so a container on
node-2 can be reached by `rest-sicily.incus` from edge on node-1. Incus
bridge DNS becomes cluster-aware automatically once OVN is configured.

## Storage

**Pool**: ZFS on `rpool/incus` (the droplet's root disk). Datasets are
snapshottable, compressible, and quota-able natively.

**Custom volumes** (one per service that has persistent data):

| volume | attached to | contents |
|---|---|---|
| `manager-data` | manager at `/app/.data` | fleet.db |
| `gitea-data` | gitea at `/data` | repos, DB, config |
| `plane-data` | plane at `/app/data` | uploads, attachments |
| `bookstack-data` | bookstack at `/var/www/bookstack` | uploads, DB |
| `rest-<slug>-data` | rest-\<slug\> at `/app/.data` | instance SQLite + bucket |

**Snapshots and backups** (three layers, same model as do-ops):

1. **In-app backups** — the restaurant app takes its own SQLite online
   backup + bucket tarball, same as before. Lives on the container's own
   ZFS volume. Survives operator error, not volume loss.
2. **ZFS snapshots** — `incus snapshot create <ct> snap0` before every
   container replacement, plus a cron that snapshots all volumes daily.
   `zfs send` to an off-site target for disaster recovery.
   `incus config device override` to mount an old volume into a recovery
   container for surgical restore.
3. **Host backup** — the droplet's own DO backup (whole-disk). Restores
   everything, but is all-or-nothing. Last resort.

Layer 1 is always on. Layer 2 is scripted (see `scripts/snapshot-all.sh`).
Layer 3 is opt-in via DO console.

## CI/CD

CI runs as the `ct-runner` container (Gitea Actions `act_runner` inside an
Incus container with the `ci` profile, which gives it the host Incus
socket). Self-hosted, label `[native]`, same trust model as the old edge
runner (outbound-only HTTPS to Gitea, no SSH keys for CI).

### Manager release (`manager-vX.Y.Z`)

```
gitea receives tag manager-v1.2.3
  │
  v
ct-runner picks it up (native-ops workflow or management/.gitea/workflows/)
  │
  ├─ 1. Check out the tag
  │
  ├─ 2. Build image: tmp-build-manager from images/manager/
  │     → publish as opsavor-manager-v1.2.3
  │
  ├─ 3. Launch new: incus launch opsavor-manager-v1.2.3 manager-new
  │       attach manager-data volume, inject env from /root/.env
  │
  ├─ 4. Health-gate: curl manager-new.incus:3001/health
  │
  ├─ 5. Swap: update Caddy route manager.incus:3001 → manager-new.incus:3001
  │       incus file push + incus exec edge -- caddy reload
  │
  └─ 6. Clean up: incus delete manager-old (rename old one first for
        rollback window; keep it stopped, not deleted, for 15 min)
```

No downtime: the new container is healthy before Caddy switches. Old
container is kept stopped for a rollback window, then deleted by sweep.

### Restaurant release (`restaurant-vX.Y.Z`)

```
gitea receives tag restaurant-v1.2.3
  │
  v
ct-runner picks it up
  │
  ├─ 1. Prove green: npm ci && npm test in the build container
  │
  ├─ 2. Build image: tmp-build-restaurant from images/restaurant/
  │     → publish as opsavor-restaurant-v1.2.3
  │
  ├─ 3. For each ACTIVE restaurant (manager API /api/update-all):
  │     ├─ incus launch opsavor-restaurant-v1.2.3 rest-<slug>-new
  │     │     attach rest-<slug>-data volume, inject per-site env
  │     ├─ health-gate rest-<slug>-new.incus:3000/api/health
  │     ├─ swap Caddy route <slug>.opsavor.app → rest-<slug>-new
  │     ├─ incus stop rest-<slug>-old (keep for rollback window)
  │     └─ sweep deletes old after window
  │
  └─ On-boards use the new image alias immediately (GOLDEN_IMAGE_ALIAS
      in manager env: opsavor-restaurant-v1.2.3)
```

Restaurant releases are synchronous like the old `/api/update-all`: the
manager walks each tenant, launches new, health-gates, swaps, and only then
moves to the next. Failures are per-site and never abort mid-fleet.

### Onboard

```
Manager API: POST /api/restaurants {slug, name, ownerEmail, ownerPassword}
  │
  ├─ 1. Validate slug, check host capacity (incus info resources)
  │
  ├─ 2. Launch: incus launch $GOLDEN_IMAGE_ALIAS rest-<slug>
  │       --profile base --profile restaurant
  │       attach new ZFS volume rest-<slug>-data
  │       incus config set rest-<slug> environment.BASE_URL …
  │       incus config set rest-<slug> environment.OWNER_EMAIL …
  │       (etc., from manager-generated per-site env)
  │
  ├─ 3. Wait for: incus exec rest-<slug> -- systemctl is-system-running
  │       then health-gate on the app's /api/health
  │
  ├─ 4. Write Caddy site: incus file push <slug>.caddy edge/etc/caddy/sites/
  │       incus exec edge -- caddy reload
  │
  └─ 5. Record in fleet.db (container name, IP, image alias, created_at)
```

No DO API call, no droplet wait, no cloud-init poll. Container launches are
sub-second from a cached image.

## Secrets inventory

| secret | lives | used by | never in |
|---|---|---|---|
| `DO_API_TOKEN` | host `/root/.env` (600) | edge Caddy DNS-01, `scripts/add-edge-dns.sh` | git, CI logs, containers |
| `MANAGER_TOKEN` | host `/root/.env` (600) | manager API auth (injected at launch) | git, CI logs |
| `OLLAMA_DEFAULT_TOKEN` | host `/root/.env` (600) | fleet Savy default (injected per-restaurant) | git, CI logs |
| `GOLDEN_IMAGE_ALIAS` | host `/root/.env` (600) | manager: which image alias to launch for onboards | git |
| `GITEA_ADMIN_TOKEN` | host `/root/.env` (600) | manager: user provisioning via Gitea API | git, CI logs |
| `opsavor_ed25519` | operator laptop `~/.ssh` | break-glass SSH to the host | git |
| `manager_fleet` | host `/root/.ssh` (600) | manager SSH into tenant containers (rare; Incus exec preferred) | git |
| `gitea_deploy` | host `/root/.ssh` (600) | read-only repo deploy keys for ci-runner | git |
| per-site `SERVICE_TOKEN` / `OLLAMA_*` / owner pw | fleet DB / incus config env at launch | instance auth + Savy | git, list/get responses |
| Incus client cert | manager container `/root/.config/incus/` | manager→Incus API authentication | git |

Secrets are injected into containers at launch time via `incus config set
<ct> environment.<KEY> <value>`. They are visible to anyone with exec access
to the container, but they are never written to the image, never in git,
and never in CI logs. `/root/.env` on the host is the single source of
truth, chmod 600, gitignored.

## Firewall

Host nftables (not ufw — Incus manages its own bridge NAT rules, and ufw
fights with them):

| port | protocol | source | destination | purpose |
|---|---|---|---|---|
| 22 | tcp | admin CIDR | host | SSH admin |
| 80 | tcp | any | edge (proxy) | HTTP → Caddy (redirect to HTTPS) |
| 443 | tcp | any | edge (proxy) | HTTPS → Caddy |
| 8443 | tcp | admin CIDR | host Incus API | remote management (optional; disabled by default) |

Container-internal traffic on `incusbr0` is unrestricted (same trust domain).
Container-to-internet is NAT'd outbound by default (required for Gitea pulls,
npm installs, Ollama Cloud).

## Capacity planning

Host: **opsavor-node-1** (DigitalOcean droplet, 4 vCPU, 8 GB RAM, 160 GB
SSD, NYC1).

| workload | RAM | cum. | headroom |
|---|---|---|---|
| host OS + Incus daemon | ~500MB | 0.5/8GB | |
| edge | 256MB | 0.8 | |
| manager | 2GB | 2.8 | |
| gitea | 2GB | 4.8 | |
| ct-runner | 4GB | 8.8 | ⚠️ over limit when active |
| plane | 2GB | 10.8 | |
| bookstack | 1GB | 11.8 | |
| rest-sicily | 1GB | 12.8 | |
| each rest-\<slug\> | 1GB | +1 | |

**Reality**: ct-runner is idle 99% of the time. When it runs, it needs 4GB
temporarily, and its limits only apply while it exists. The `ci` profile's
4GB is a ceiling, not a reservation. In practice: stop ct-runner when not
deploying, or accept that a running deploy + all services + 2-3 restaurants
will swap briefly.

**Phase 2 trigger**: sustained > 6GB RSS with ct-runner idle, or > 5
restaurant containers. At that point, move restaurants to worker nodes and
keep control plane on node-1.

**Disk**: 160GB. Images are ~2-4GB each. Keep last 3 per service type.
ZFS compression (`zstd`) on the pool. Old images are pruned by
`scripts/prune-images.sh` (keep 3 latest per alias).

## What changes per repo

### do-ops → native-ops

The repo is renamed. Everything in it is rewritten for Incus. DigitalOcean
API scripts (`do.sh`, `make-golden-image.sh`, `onboard-restaurant.sh`,
`ensure-firewall.sh`) are replaced by Incus-native equivalents:

| do-ops script | native-ops replacement |
|---|---|
| `do.sh` | (dropped — no DO API calls for compute) |
| `add-edge-dns.sh` | kept (still DO DNS) |
| `ensure-firewall.sh` | dropped — replaced by host nftables + Incus proxy |
| `make-golden-image.sh` | `scripts/build-image.sh` — Incus build + publish |
| `onboard-restaurant.sh` | kept, fixed to match `lib/incus.mjs`'s onboarding flow (env path, volume attach order) — a manual/CLI fallback; the manager's `POST /api/restaurants` is the normal path |
| `deploy-manager.sh` | kept under the same name — builds via `scripts/build-image.sh manager`, replaces the container, keeps its data volume |
| `edge/cloud-init.yml` | `scripts/provision-host.sh` — host setup (not cloud-init) |
| `restaurant/cloud-init.yml` | (dropped — containers boot from images, not cloud-init) |

The `edge/` directory keeps `Caddyfile` (updated for `.incus` upstream
names), `site.caddy.tmpl`, `Dockerfile.caddy` → `images/edge/build.sh`, and
`sites/` (now pushed via `incus file push`).

### management

**As actually implemented** (this section originally described a plan
drafted before the migration; updated to match what shipped — see
`management`'s own README and `lib/incus.mjs` for the full picture):

`lib/do.mjs` and `lib/fleet-ssh.mjs` were both replaced by a single
`lib/incus.mjs`, plus `lib/container-config.mjs` in place of
`lib/userdata.mjs`. The manager has no Incus socket and no `incus` CLI
inside its own container — it drives Incus by SSHing back to the host as a
scoped, no-sudo `manager-ctl` user (incus-admin group) and running `incus`
there, rather than over a mounted Unix socket or the HTTPS API (see
"Manager -> Incus control plane" in `AGENTS.md` for why). `lib/db.mjs`,
`lib/caddy.mjs` (rewritten to push Caddy site files to `edge` directly
over that same SSH path instead of a shared bind mount), `lib/sites.mjs`,
`lib/ops-auth.mjs`, `lib/api-tokens.mjs`, and the dashboard carry over.

Key endpoint changes:

| old behavior (droplet) | new behavior (container) |
|---|---|
| `POST /api/restaurants` → deploy droplet from snapshot | launch container from the `opsavor-restaurant:latest` image fingerprint |
| `POST /:slug/resize` → power off → DO resize → power on | live `incus config set limits.cpu/memory` — no stop/start at all |
| rolling update → SSH in, `git fetch` + rebuild in place | delete + relaunch from a pre-built `opsavor-restaurant:<ref>` image, keeping the same data volume (containers are immutable — see AGENTS.md principle 1) |
| `POST /:slug/backup` → instance self-backup | unchanged (in-app backup); no ZFS snapshot layer (host uses the `dir` storage backend, not ZFS — see gotcha) |
| `DELETE /:slug` → destroy droplet | `incus delete <ct>` + delete its custom volume |
| `GET/POST /:slug/droplet` | renamed `/:slug/container`; actions are `start\|stop\|restart` |

### restaurant

The app itself does not change. The entrypoint, health check, and
Dockerfile logic move into `images/restaurant/build.sh` and a systemd
unit — its `ExecStart` runs the same `scripts/docker-entrypoint.sh` the
Docker image used (staged restore → migrate → optional owner seed →
`server.js`), not `node server.js` directly. Per-instance config (owner
email/password, service token, Ollama settings) lands in
`/app/.data/env` on the container's data volume — NOT via `incus config
set environment.*`, which (per the gotcha above) never reaches a
systemd-managed process; the systemd unit reads it via
`EnvironmentFile=-/app/.data/env`. `AGENTS.md` / `README.md` deployment
sections reference `native-ops` instead of `do-ops`.

## Runbooks

### Provision the host (first time)

As actually run against `opsavor-node-1` (a DO droplet — see the gotcha
about `dir` storage: DO's custom kernel can't DKMS-build ZFS):

```sh
# 1. Create the droplet: 4 vCPU / 8GB / 160GB / Debian 13 / NYC1,
#    private networking on.

# 2. SSH in as root. Clone this repo (SSH deploy key against
#    git.theta42.com — git.opsavor.app doesn't exist yet, see "Build
#    Gitea container" in the pilot todo):
git clone ssh://gitea@git.theta42.com:2222/opsavor/native-ops.git /root/native-ops

# 3. Provision: installs Incus from the Zabbly repo, runs
#    `incus admin init --preseed` from incus/preseed.yml, applies
#    incus/profiles/*.yml, locks down UFW (including the incusbr0 bridge
#    rules — see gotcha), and creates the manager-ctl Incus control user.
cd /root/native-ops && ./scripts/provision-host.sh

# 4. Write /root/.env (chmod 600) with MANAGER_TOKEN, OLLAMA_DEFAULT_TOKEN.
#    (No DO_API_TOKEN needed here — that's a fleet-DB secret set from the
#    manager dashboard once it's up, for the edge's DNS-01 challenge.)

# 5. Build and launch the core containers:
./scripts/build-image.sh base
./scripts/build-image.sh edge
git clone ssh://gitea@git.theta42.com:2222/opsavor/management.git /root/management
./scripts/deploy-manager.sh main
incus launch opsavor-edge edge --profile base --profile edge

# 6. Point DNS at the host:
DO_API_TOKEN=… ./providers/digitalocean/dns.sh <node-1-public-ip>
```

### Deploy a manager update

```sh
git -C /root/management pull origin main   # or fetch a specific ref
./scripts/deploy-manager.sh main           # or a tag/branch name
```

Builds the image via `scripts/build-image.sh manager`, replaces the
running container, and reattaches its existing data volume (fleet.db is
never recreated). Not yet wired to Gitea Actions — see `.gitea/workflows/
deploy.yml` in the management repo, which needs a runner registered on
this host first.

### Onboard a restaurant

```sh
# Build the image once per release tag (build-image.sh restaurant, needs
# real memory — see the build-container-memory gotcha):
./scripts/build-image.sh restaurant restaurant-v0.2.2

# Via the manager UI: dashboard → Onboard card → slug, name, owner, size
# Via the manager API:
curl -X POST -H "Authorization: Bearer $MANAGER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"slug":"acme","name":"Acme Pizza","ownerEmail":"...","ownerPassword":"...","size":"medium"}' \
  https://manage.opsavor.app/api/restaurants

# Watch provisioning:
incus list
incus exec rest-acme -- journalctl -u restaurant --no-pager -n 40
```

### Deploy BookStack (wiki.opsavor.work)

BookStack is a singleton internal service (unlike restaurants, there's only
ever one), so it isn't run through the manager — `scripts/deploy-bookstack.sh`
is the whole story: build the image, create+attach its persistent data
volume (MariaDB datadir, uploads, and the generated `APP_KEY`/DB password
all live there — see `images/bookstack/bookstack-entrypoint.sh`), launch
the container, health-gate.

```sh
# 1. One-time: point DNS at this host. opsavor.work is a separate domain
#    from opsavor.app (added to the same DO account) — apex + wildcard, so
#    a single Caddy wildcard cert can cover every opsavor.work subdomain,
#    not just wiki.
set -a; . /root/.env; set +a
DOMAIN=opsavor.work ./providers/digitalocean/dns.sh <host-public-ip>

# 2. Build + launch:
./scripts/deploy-bookstack.sh

# 3. Route wiki.opsavor.work at it and pick up the wildcard cert. The edge
#    Caddyfile's `*.opsavor.work` block (see edge/Caddyfile) already has a
#    host matcher for wiki.opsavor.work → bookstack:80; this just pushes
#    that file to the running edge container (there was previously no
#    script for this at all — the live Caddyfile was hand-pushed once and
#    never kept in sync):
./scripts/sync-edge-caddyfile.sh

# First boot takes a couple of minutes (MariaDB datadir init + Laravel
# migrations run inline before the app can serve). Watch it:
incus exec bookstack -- journalctl -u bookstack --no-pager -n 40
```

Adding a THIRD opsavor.work subdomain later (say `status.opsavor.work`)
needs no new DNS record (the wildcard already covers it) and no new
top-level Caddy site block (that would issue it a separate, non-wildcard
cert) — just another `@matcher`/`handle` pair inside the existing
`*.opsavor.work` block, then `./scripts/sync-edge-caddyfile.sh`.

To change BookStack's public URL later: `incus exec bookstack -- vi
/data/env` (edit `APP_URL`), then `incus restart bookstack` — the
entrypoint only ever WRITES that file on the volume's first boot, so this
is the one supported way to change it afterward.

#### Google Workspace SSO for BookStack

BookStack's login method is one of `standard`, `ldap`, `saml2`, or `oidc` —
exclusive, not layered (switching to `oidc` replaces the password login
form entirely, it doesn't add a button alongside it). Google Workspace
accounts authenticate via OIDC; Google itself, not BookStack, is what
restricts sign-in to your Workspace domain.

**1. Create the OAuth client in Google Cloud Console** (needs a Google
Cloud project belonging to the Workspace org — a personal/consumer Google
account can't create an "Internal" app):

- APIs & Services → OAuth consent screen → User type: **Internal**. This
  is the actual domain restriction — an Internal app can only be signed
  into by accounts in your Workspace org; there is no equivalent setting
  on the BookStack side (`app/Config/oidc.php` has no domain/`hd` option).
- APIs & Services → Credentials → Create Credentials → OAuth client ID →
  Application type: Web application.
- Authorized redirect URI: `https://wiki.opsavor.work/oidc/callback`
- Save; copy the Client ID and Client Secret.

**2. Configure BookStack** — edit the persisted config, not the image:

```sh
incus exec bookstack -- vi /data/env
```

Add:

```
AUTH_METHOD=oidc
OIDC_CLIENT_ID=<client id from Google Cloud>
OIDC_CLIENT_SECRET=<client secret from Google Cloud>
```

(`OIDC_ISSUER`, `OIDC_ISSUER_DISCOVER`, `OIDC_NAME`, `OIDC_DISPLAY_NAME_CLAIMS`,
and `OIDC_END_SESSION_ENDPOINT` all default to working Google values in
`bookstack-entrypoint.sh` — only override them in `/data/env` if you need
something different.) Then:

```sh
incus restart bookstack
```

**3. First real login and admin access.** A newly-provisioned OIDC user
gets BookStack's default role, not Admin — there's no Workspace group
sync configured (`OIDC_USER_TO_GROUPS` needs group claims in the ID token,
which Google's default OIDC scopes don't include). After your first
Google login creates your user row, promote it directly:

```sh
incus exec bookstack -- bash -c "cd /var/www/bookstack && php artisan tinker --execute=\"
\\\$u = \\\\BookStack\\\\Users\\\\Models\\\\User::where('email','you@yourdomain.com')->first();
\\\$u->attachRole(\\\\BookStack\\\\Users\\\\Models\\\\Role::where('system_name','admin')->first());
\\\$u->save();
\""
```

**Recovery**: switching `AUTH_METHOD` to `oidc` removes the password
login form from `/login` entirely — there's no UI fallback if the OIDC
setup is wrong. Recovery is always available directly on the host,
regardless of what `/login` shows:
`incus exec bookstack -- mysql --socket=/run/mysqld/mysqld.sock -D
bookstack -e "..."` against the `users` table, or set `AUTH_METHOD=standard`
back in `/data/env` and `incus restart bookstack` to get the password form
back. The seeded default admin (`admin@admin.com`) is NOT left on its
well-known default password on this instance — it was rotated the same
way (`php artisan tinker`, `Hash::make()`) the first time this was set up;
the new one is saved at `/root/.bookstack-admin-pw.tmp` on the host.

### Deploy Gitea (git.opsavor.work)

Same singleton-service shape as BookStack: `scripts/deploy-gitea.sh` builds
the image, creates+attaches the persistent `gitea-data` volume (SQLite DB,
repos, and generated secrets all live there — see
`images/gitea/gitea-entrypoint.sh`), launches, health-gates. No separate
DNS or Caddy step needed — `git.opsavor.work` is already a host matcher in
the existing `*.opsavor.work` wildcard block in `edge/Caddyfile`, so it's
covered the moment `./scripts/sync-edge-caddyfile.sh` has been run once
(see the BookStack section above if it hasn't).

```sh
./scripts/deploy-gitea.sh
```

A local `admin` account is created on first boot with a generated
password (never a well-known default) — `incus exec gitea -- cat
/data/gitea-secrets.env` to read it.

#### Google Workspace SSO for Gitea

Unlike BookStack, Gitea layers OAuth2 login alongside the local
`admin`/password form rather than replacing it — no separate recovery
path needed here, the standard login box is always still there at
`/user/login`. Gitea stores OAuth2 login sources in its own database
(the volume), not a config file, so once added it's just there — no
env-file plumbing to keep in sync.

**1. Add a redirect URI to your existing Google Cloud OAuth client**
(reuse the same one from the BookStack setup — Google OAuth clients
support multiple redirect URIs, no need for a second client or a new
Client ID/Secret): APIs & Services → Credentials → your OAuth client →
Authorized redirect URIs → add:

```
https://git.opsavor.work/user/oauth2/google/callback
```

**2. Configure Gitea** — edit the persisted secrets, not the image:

```sh
incus exec gitea -- vi /data/gitea-secrets.env
```

Add:

```
GITEA_OAUTH_CLIENT_ID=<client id>
GITEA_OAUTH_CLIENT_SECRET=<client secret>
```

Then `incus restart gitea` — the entrypoint adds the `google` OAuth2
source via `gitea admin auth add-oauth` on that boot (idempotent: it
checks `gitea admin auth list` first, so this is safe to leave in place
across every later restart too). New Google logins get
`ENABLE_AUTO_REGISTRATION`-provisioned Gitea accounts. Gitea's admin CLI
has no "promote to admin" subcommand (only `create`, `list`,
`change-password`, `delete`, `must-change-password`) — either use the web
UI (sign in as the local `admin` account → Site Administration → Users →
edit the user → Is Admin), or:

```sh
incus exec gitea -- sqlite3 /data/gitea.db \
  "UPDATE user SET is_admin = 1 WHERE name = '<gitea-username>';"
incus restart gitea
```

### Deploy Plane (tickets.opsavor.work)

Unlike every other service in this fleet, Plane is NOT built from source.
It's a genuinely large multi-service stack — Django API + Celery worker +
beat scheduler + three separate Next.js frontends + a realtime
collaboration server, normally run as a 13-container docker-compose.
Rebuilding all of that natively would take far longer and be far more
fragile than using Plane's own maintained images, so this runs them as
Incus OCI application containers instead — Incus can pull and run OCI
images directly, no separate Docker daemon needed. `scripts/deploy-plane.sh`
runs Plane's official All-In-One image (`makeplane/plane-aio-community`,
which bundles web/space/admin/api/live/worker/beat/an internal Caddy into
one container) plus Postgres/Redis/RabbitMQ/MinIO as its four required
external services.

```sh
./scripts/deploy-plane.sh
./scripts/sync-edge-caddyfile.sh   # if not already run for wiki/git
```

Generated secrets (DB/queue/storage passwords, Django secret keys) live in
`/root/.plane-secrets.env` on the host — there's no custom entrypoint here
to persist them on a volume the way bookstack/gitea's do, since these are
all upstream images running their own. **The four infra containers
(`plane-db`, `plane-redis`, `plane-mq`, `plane-minio`) are only ever
created once** — re-running this script leaves them alone if they already
exist and only replaces the `plane` app container. Force-deleting a
running Postgres to "redeploy" it sends it a hard kill instead of a
graceful shutdown; doing that repeatedly while first getting this working
corrupted its data directory for real, which is the specific thing this
guards against.

#### Google Workspace SSO for Plane

Plane has a native Google OAuth provider
(`apps/api/plane/authentication/provider/oauth/google.py`) that reads
`GOOGLE_CLIENT_ID`/`GOOGLE_CLIENT_SECRET` as plain environment variables —
no admin-UI configuration step needed, unlike BookStack/Gitea. The
redirect URI is fixed by Plane's own code (not something you choose via
`--name` the way Gitea's is):

```
https://tickets.opsavor.work/auth/google/callback/
```

Add that as an authorized redirect URI on the same Google Cloud OAuth
client already used for BookStack/Gitea, then:

```sh
echo 'GOOGLE_CLIENT_ID=<client id>' >> /root/.plane-secrets.env
echo 'GOOGLE_CLIENT_SECRET=<client secret>' >> /root/.plane-secrets.env
./scripts/deploy-plane.sh   # only replaces the plane container; infra is untouched
```

### Migrate a restaurant between nodes (Phase 2)

```sh
# On the manager (which talks to both nodes' Incus):
incus move rest-sicily --target opsavor-node-2

# Or stop-copy-start (works on any storage backend):
incus snapshot create rest-sicily pre-migrate
incus copy rest-sicily/pre-migrate opsavor-node-2:rest-sicily
# Update Caddy route on edge to the new container IP
incus stop rest-sicily  # on node-1
incus start rest-sicily  # on node-2 (now the live one)
# Verify, then delete the node-1 copy
```

### Destroy a restaurant

```sh
# Via the manager UI: dashboard → Manage → Decommission
# The manager:
#   1. Creates a final ZFS snapshot (incus snapshot create rest-<slug> final)
#   2. Removes the Caddy site file (incus exec edge -- rm /etc/caddy/sites/<slug>.caddy; reload)
#   3. Deletes the container (incus delete --force rest-<slug>)
#   4. Exports the volume to a compressed tarball → ZFS send or S3
#   5. Deletes the volume (after retention period)
#   6. Removes the fleet.db row
```

### Backup / restore

**In-app** (same as do-ops): the restaurant instance takes its own SQLite
online backup + bucket tarball daily. Lives on the container's volume.

**ZFS snapshot** (structural):
```sh
# Snapshot all tenant volumes:
./scripts/snapshot-all.sh                    # cron: daily

# Restore a specific container from snapshot:
incus snapshot create rest-sicily pre-restore   # safety copy
incus restore rest-sicily snap0                 # roll back to snapshot
# The app sees the restored DB on next boot; the entrypoint skips
# migrations if the schema is already at the running release.
```

**Volume-level restore** (surgical):
```sh
# The volume survives container deletion. Attach it to a fresh container:
incus launch opsavor-restaurant-vX.Y.Z rest-sicily-recovered
incus storage volume attach default rest-sicily-data rest-sicily-recovered /app/.data
incus start rest-sicily-recovered
```

**Full host failure**: new droplet → provision → `zfs receive` from off-site
backup → re-launch containers from images. See "Rebuild from scratch" below.

### Rebuild from scratch

```sh
# 1. New droplet (4vCPU/8GB/160GB, Debian 13, NYC1)
# 2. SSH in:
apt update && apt install -y incus zfsutils-linux curl git
incus admin init --preseed < native-ops/incus/preseed.yml

# 3. Restore /root/.env from operator password manager (or re-enter).

# 4. Restore ZFS volumes from backup (if available):
zfs receive rpool/incus/custom/rest-sicily-data < backup/rest-sicily-data.zfs

# 5. Build images and launch:
./scripts/provision-host.sh
./scripts/build-image.sh base && ./scripts/build-image.sh edge
./scripts/build-image.sh manager && ./scripts/build-image.sh gitea
./scripts/launch-core.sh
./scripts/launch-tenants.sh    # reads fleet.db backup or relaunches from volume list

# 6. DNS:
DO_API_TOKEN=… ./scripts/add-edge-dns.sh <new-ip>

# 7. Re-register ct-runner with Gitea (the runner token is per-host).
```

## File layout

```
native-ops/
  README.md                    ← this file
  incus/
    preseed.yml                ← incus admin init --preseed
    profiles/
      base.yml                 ← least-privilege defaults (inherited by all)
      edge.yml                 ← Caddy + proxy devices for :80/:443
      service.yml              ← internal tooling (gitea, plane, bookstack)
      ci.yml                   ← CI runner (privileged, Incus socket mounted)
      restaurant.yml           ← per-site Next.js + SQLite
  images/
    base/                      ← Debian 13 + node:22 + common tools (build.sh)
    edge/                      ← Caddy binary + Caddyfile (build.sh)
    manager/                   ← management app (build.sh)
    restaurant/                ← restaurant app (build.sh)
    gitea/                     ← Gitea binary + config (build.sh)
    plane/                     ← Plane distribution (build.sh)
    bookstack/                 ← BookStack + PHP (build.sh)
  edge/
    sites/                     ← per-site Caddy blocks (manager-written, gitignored)
  providers/
    digitalocean/              ← DO API scripts (DNS only, no compute)
  scripts/
    provision-host.sh          ← one-time host setup (profiles, firewall, dirs)
    build-image.sh             ← generic image build pipeline (tmp ct → publish)
    deploy-service.sh          ← generic rolling update (launch → swap → cleanup)
    launch-core.sh             ← boot edge + manager + gitea + ct-runner
    launch-tenants.sh          ← boot all restaurant containers from fleet.db
    add-edge-dns.sh            ← idempotent apex + wildcard A-records
    snapshot-all.sh            ← ZFS snapshot every volume (cron)
    prune-images.sh            ← keep 3 latest per alias
    ensure-firewall.sh         ← nftables rules for the host
```

`edge/.env` is gitignored. `edge/sites/*.caddy` is gitignored (manager-
generated). Image build scripts write nothing to this repo at runtime.

## Troubleshooting

- **Container won't start**: `incus info <ct>` shows last error. Most common
  cause: profile not applied (`incus profile assign <ct> base,restaurant`).
  Second: ZFS pool full (`zfs list` / `incus storage info default`).

- **`.incus` DNS not resolving**: the managed bridge runs dnsmasq. Verify
  `incus network get incusbr0 dns.mode` is `managed`. If a container's
  hostname changed, dnsmasq needs a moment; restart the container or wait
  for the DHCP lease refresh.

- **Caddy reload fails after `incus file push`**: the site file has a
  syntax error (bad suspend reason, malformed IP). Check:
  `incus exec edge -- caddy validate --config /etc/caddy/Caddyfile`.
  Fix or remove the offending file, then `incus exec edge -- caddy reload`.

- **ct-runner OOM during image build**: the `ci` profile caps at 4GB.
  Next.js builds with `NODE_OPTIONS=--max-old-space-size=2560` and ZFS
  compression on. If that's not enough, raise the limit temporarily:
  `incus config set ct-runner limits.memory 6GB`, downgrade after.

- **Manager can't reach Incus socket**: the manager container needs the
  `ci` profile or an explicit `incus config device add manager incus-socket
  disk source=/var/lib/incus/unix.socket path=/var/lib/incus/unix.socket`.
  Without it, `incus` commands inside the manager fail with a socket-
  not-found error.

- **`incus publish` hangs or is very slow**: ZFS snapshot + export of a
  large container. Normal for the restaurant image (~3-4GB uncompressed,
  ~1.5GB zstd). Do not Ctrl-C; the published image will be corrupt.

- **Host out of RAM**: `incus list --format csv | awk -F, '{print $1}'`
  while read; then stop non-essential services (plane, bookstack) or stop
  ct-runner if it is mid-build. Consider whether you have hit the Phase 2
  trigger.
