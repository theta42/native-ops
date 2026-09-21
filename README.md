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
- **Fail closed.** Public listeners are the edge proxy (ports 80/443
  forwarded via Incus `proxy` device) plus one deliberate exception: gitea's
  own `proxy` device on port 2222 for git-over-ssh, which Caddy can't
  reverse-proxy the way it does HTTP. All inter-container traffic is on
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
  │    ├─ docs.opsavor.app   → outline.incus:3000
  │    ├─ *.opsavor.app (LE wildcard, DO DNS-01)
  │    └─ /opt/sites/<slug>.caddy (manager-written via incus file push)
  │
  ├─ manager       10.0.100.11   Fleet orchestrator (:3001)
  │    ├─ fleet.db (ZFS volume: manager-data)
  │    ├─ Incus socket mounted → launches/stops/snapshots containers
  │    └─ Caddy sites written via incus file push → caddy reload
  │
  ├─ gitea         10.0.100.12   Git hosting + Actions. opsavor/opsavor.ai
  │    │                          now lives here (the home image clones it);
  │    │                          restaurant/management/native-ops still live
  │    │                          on git.theta42.com — see "CI/CD" below.
  │
  ├─ home          10.0.100.15   opsavor.ai home page — Node static server
  │                                (:3000, via edge Caddy). Repo
  │                                opsavor/opsavor.ai; image opsavor-home.
  │
  ├─ (no ct-runner container — CI runs as act_runner directly on THIS host,
  │    a systemd service, not a separate Incus instance; see "CI/CD" below)
  │
  ├─ plane         10.0.100.14   Project management
  ├─ outline       10.0.100.1x   Internal wiki/docs (wiki.opsavor.work +
  │                                docs.opsavor.app)
  ├─ outline-db     10.0.100.1x   Outline's Postgres
  ├─ outline-redis  10.0.100.1x   Outline's Redis/valkey
  │
  ├─ rest-sicily   10.0.100.101  Next.js standalone + SQLite (:3000)
  ├─ rest-<slug>   10.0.100.1xx  one container per restaurant
  │                                ↑ replaced on update, never patched
  │
  └─ custom storage volumes (on the `default` pool — `dir` driver, not ZFS;
     see "Storage" below):
       manager-data, gitea-data, plane-data,
       outline-data, outline-db-data, outline-redis-data,
       rest-sicily-data, rest-<slug>-data
```

- **DNS** (`opsavor.app` in DO DNS): apex + `*` A-records → node-1 public IP
  (`scripts/add-edge-dns.sh`). Per-site DNS is never needed (wildcard).
  `opsavor.ai` (the public home page) is also in DO DNS now, apex + `*`, set
  with the same script (`DOMAIN=opsavor.ai`) — see the `home` deploy runbook.
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
| `service` | 2 | 2GB | yes | no | gitea, plane, outline |
| `ci` | 2 | 4GB | yes | **yes** | ct-runner (Incus-in-Incus for image builds) |
| `restaurant` | 1 | 1GB | yes | no | per-site Next.js + SQLite |

The `ci` profile mounts the host Incus socket into the container so the CI
runner can launch sibling containers for image builds. It is the only
privileged container on the host.

### Projects

| project | containers | purpose |
|---|---|---|
| `default` | edge, manager, gitea, ct-runner | core platform |
| `services` | plane, outline | internal tooling |
| `tenants` | rest-sicily, rest-\<slug\> | customer workload isolation |

Projects provide RBAC scoping: a leaked tenant token cannot read the
`default` project's containers. The manager holds a client certificate
scoped to `default + tenants`.

## Container inventory

| container | project | profiles | ip | resources | serves |
|---|---|---|---|---|---|
| `edge` | default | base, edge | 10.0.100.10 | 1C / 256MB | ports 80/443 (proxy devices) |
| `manager` | default | base, service | 10.0.100.11 | 2C / 2GB | :3001 (via edge Caddy) |
| `gitea` | default | base, service | 10.0.100.12 | 2C / 2GB | :3000 (via edge Caddy), :2222 (SSH, own proxy device) |
| `home` | default | base, service | 10.0.100.15 | 2C / 2GB | :3000 (via edge Caddy) — opsavor.ai home page |
| `ct-runner` | default | base, ci | 10.0.100.13 | 2C / 4GB | outbound-only |
| `plane` | services | base, service | 10.0.100.14 | 2C / 2GB | :3001 (via edge Caddy) |
| `outline` | services | base, service | 10.0.100.1x | 2C / 1GB | :3000 (via edge Caddy) |
| `outline-db` | services | base, service | 10.0.100.1x | 1C / 512MB | Postgres (internal only) |
| `outline-redis` | services | base, service | 10.0.100.1x | 1C / 512MB | Redis/valkey (internal only) |
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
| `opsavor-home` | `opsavor-base` | `images/home/` — clone opsavor/opsavor.ai + `server.mjs` | ct-runner |
| `opsavor-restaurant` | `opsavor-base` | `images/restaurant/` — `npm ci && npm run build` + standalone | ct-runner |
| `opsavor-gitea` | `opsavor-base` | `images/gitea/` — Gitea binary + config | ct-runner |
| `opsavor-plane` | `opsavor-base` | `images/plane/` — Plane distribution | one-time manual |

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

**Pool**: `default`, `dir` driver, on the droplet's root disk — not ZFS.
`zfs-dkms` fails to build against DO's custom kernel (see the day-1 gotcha
in `9-20-2026_todo.md`); `dir` still supports per-volume snapshots (rsync-
based, not copy-on-write), which is what `scripts/snapshot-all.sh` uses.

**Custom volumes** (one per service that has persistent data):

| volume | attached to | contents |
|---|---|---|
| `manager-data` | manager at `/app/.data` | fleet.db |
| `gitea-data` | gitea at `/data` | repos, DB, config |
| `plane-db-data` / `plane-minio-data` / `plane-mq-data` / `plane-redis-data` | plane's Postgres/MinIO/RabbitMQ/Valkey containers | Plane's own state |
| `outline-data` / `outline-db-data` / `outline-redis-data` | outline / its Postgres / its Redis container | docs+uploads, DB, queue state |
| `rest-<slug>-data` | rest-\<slug\> at `/app/.data` | instance SQLite + bucket |

**Snapshots and backups** (three layers, same model as do-ops):

1. **In-app backups** — the restaurant app takes its own SQLite online
   backup + bucket tarball, same as before. Lives on the container's own
   data volume. Survives operator error, not volume loss.
2. **Volume snapshots** — each `deploy-*.sh` takes a `pre-update-*`
   snapshot of a service's own volume before replacing its container,
   plus `scripts/snapshot-all.sh` snapshots every custom volume daily
   (`daily-*`, pruned to the newest 14 — see "Storage" above; there is no
   `zfs send` equivalent on the `dir` driver, so this is local-only, not
   off-site). `incus storage volume snapshot restore` (or `incus config
   device override` to mount an old snapshot into a recovery container)
   for surgical restore.
3. **Host backup** — the droplet's own DO backup (whole-disk, opt-in, not
   currently confirmed enabled). Restores everything, but is
   all-or-nothing. Last resort, and the only layer that's actually
   off-box.

Layer 1 is always on. Layer 2 is scripted and running (systemd timer
`snapshot-all.timer`, daily at 06:00 UTC). Layer 3 needs a human to check
the DO console.

## CI/CD

The repos (`opsavor/restaurant`, `opsavor/management`, `opsavor/native-ops`)
are hosted on `git.theta42.com`, a separate Gitea instance — not the
`gitea` container in the topology diagram above. That container is
provisioned and reachable but not yet populated; the repos will move there
eventually, just not yet.

CI runs as `act_runner` v3.5.0, installed directly on the Incus host as a
systemd service (`act-runner.service`, working directory
`/root/act-runner/`) — not inside a container. It's registered against
`git.theta42.com` with label `incus-host` (`--labels incus-host:host`: no
Docker/container executor, it just runs `run:` steps in its own root shell,
which is all either workflow needs, since both call `native-ops` scripts
that talk to the Incus socket directly). To re-register after a host
rebuild: get a fresh org-level token with
`tea api -X POST /orgs/opsavor/actions/runners/registration-token`, then
`act_runner register --no-interactive --instance https://git.theta42.com
--token <token> --name incus-host --labels incus-host:host` from
`/root/act-runner`, then `systemctl enable --now act-runner`.

Both releases replace the running instance **in place** — same container
name, same data volume, new image. There is no blue-green pair, no second
"-new" container, and no Caddy route to swap: Caddy already targets
containers by name (`manager.incus`, `<slug>.incus`) over the bridge's own
DNS, and that name keeps resolving to whatever IP the replacement container
gets. The tradeoff: the site really is down for the few seconds between
`incus delete` and the replacement passing its health-gate — not the
zero-downtime swap an earlier draft of this section described. Both
`deploy-manager.sh` and the restaurant fleet roll (via
`management/lib/incus.mjs`'s `buildUpdateScript`) build the new image
*before* deleting anything, so a failed build never touches the running
instance; a failure between delete and health-gate is not automatically
rolled back (a `manager-data` ZFS snapshot is taken first for manual
recovery — see "Storage" above; restaurant sites don't currently get an
equivalent pre-replace snapshot).

### Manager release (`manager-vX.Y.Z`)

```
git.theta42.com receives tag manager-v1.2.3
  │
  v
act-runner picks it up (management/.gitea/workflows/deploy.yml)
  │
  └─ deploy-manager.sh manager-v1.2.3
       ├─ 1. Build image: build-image.sh manager manager-v1.2.3
       │     (temp container clones management @ the tag, npm ci
       │      --production, publish as opsavor-manager:manager-v1.2.3)
       ├─ 2. Snapshot the manager-data volume (if a manager already exists)
       ├─ 3. incus delete manager --force
       ├─ 4. incus launch <built image> manager --profile base --profile service
       ├─ 5. Reattach manager-data at /app/.data, push the Incus SSH control
       │     key, write /etc/default/manager from /root/.env
       ├─ 6. incus restart manager
       └─ 7. Health-gate: incus exec manager -- curl 127.0.0.1:3001/health
             (24 tries, 5s apart; exit 1 + journalctl dump on failure)
```

`branches: [main]` + `paths: [.gitea/workflows/deploy.yml]` also fires this
same job on an ordinary push to `main` that touches the workflow file
itself, with `ref` resolving to `refs/heads/main` — intended to just prove
the runner is reachable, but it runs the real deploy script with that ref,
which only works if `opsavor-manager:refs/heads/main` can actually be
built and launched. Worth tightening later; not blocking today.

### Restaurant release (`restaurant-vX.Y.Z`)

```
git.theta42.com receives tag restaurant-v1.2.3
  │
  v
act-runner picks it up (restaurant/.gitea/workflows/release.yml)
  │
  ├─ 1. Prove green: test-restaurant.sh restaurant-v1.2.3
  │     (temp container clones restaurant @ the tag, npm ci, npm test —
  │     which runs `next build` + the full suite; container always deleted
  │     after, pass or fail)
  │
  ├─ 2. Build image: build-image.sh restaurant restaurant-v1.2.3
  │     → publish as opsavor-restaurant:restaurant-v1.2.3, repoint
  │       opsavor-restaurant:latest at it (so fresh on-boards use it too)
  │
  └─ 3. Roll the fleet: POST /api/update-all {ref} to the manager
        (reached at its live bridge IP — container_ip() in lib.sh, since
        `.incus` names don't resolve from the host act-runner runs on)
        — the manager then, for each ACTIVE restaurant, in sequence:
          incus delete rest-<slug> --force
          incus launch opsavor-restaurant:restaurant-v1.2.3 rest-<slug>
            --profile base --profile restaurant --config limits.cpu=<size>
            --config limits.memory=<size>
          reattach rest-<slug>-data at /app/.data
          incus restart rest-<slug>
          health-gate: incus exec rest-<slug> -- curl 127.0.0.1:3000/api/health
            (36 tries, 5s apart)
```

`/api/update-all` is synchronous and per-site: the manager walks every
active instance in turn, and one instance's failure doesn't abort the
rest of the fleet or the ones already done.

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
| `gitea_deploy` | host `/root/.ssh` (600) | read-only repo deploy keys for ci-runner (git.theta42.com) | git |
| `gitea_opsavor_deploy` | host `/root/.ssh` (600) | read-only deploy key for the home image build (git.opsavor.work) | git |
| per-site `SERVICE_TOKEN` / `OLLAMA_*` / owner pw | fleet DB / incus config env at launch | instance auth + Savy | git, list/get responses |
| Incus client cert | manager container `/root/.config/incus/` | manager→Incus API authentication | git |

Secrets are injected into containers at launch time via `incus config set
<ct> environment.<KEY> <value>`. They are visible to anyone with exec access
to the container, but they are never written to the image, never in git,
and never in CI logs. `/root/.env` on the host is the single source of
truth, chmod 600, gitignored.

## Firewall

`ufw` (`provision-host.sh`; despite an earlier draft of this section
claiming nftables directly — ufw is what's actually configured, plus the
explicit `ufw allow in on incusbr0` rules below that keep it from
dropping bridge traffic):

| port | protocol | source | destination | purpose |
|---|---|---|---|---|
| 22 | tcp | admin CIDR | host | SSH admin |
| 80 | tcp | any | edge (proxy) | HTTP → Caddy (redirect to HTTPS) |
| 443 | tcp | any | edge (proxy) | HTTPS → Caddy |
| 2222 | tcp | any | gitea (proxy device on gitea itself, not edge) | git-over-ssh — see "Deploy Gitea" |
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
| outline (+ its Postgres/Redis) | ~1.5GB | 12.3 | |
| rest-sicily | 1GB | 13.3 | |
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

#### Google Workspace SSO for the manager dashboard

Reuses the same "Internal" Google Cloud OAuth client already used for
BookStack/Gitea/Plane — same domain-restriction gate (Google itself, via
the Internal app-type, only lets Workspace org members reach the consent
screen at all), plus a server-side check in `resolveGoogleEmail`
(`management/lib/google-auth.mjs`) that the returned email actually ends
in `GOOGLE_ALLOWED_DOMAIN` (default `opsavor.ai`). Unlike BookStack/Gitea,
Google login here doesn't just create a normal-privilege user — every
`ops_users` row IS a full fleet-management operator (there's no lower
role), so this only auto-provisions for the allowed domain, and there's no
group-sync/promotion step needed afterward.

Add the redirect URI to the existing OAuth client, then set
`GOOGLE_CLIENT_ID`/`GOOGLE_CLIENT_SECRET` in `/root/.env` and redeploy:

```
https://manage.opsavor.app/auth/google/callback
```

```sh
echo 'GOOGLE_CLIENT_ID=<client id>' >> /root/.env
echo 'GOOGLE_CLIENT_SECRET=<client secret>' >> /root/.env
echo 'GOOGLE_ALLOWED_DOMAIN=opsavor.ai' >> /root/.env   # default; only set if different
./scripts/deploy-manager.sh main
```

`deploy-manager.sh` writes these into `/etc/default/manager` on every
deploy (same `EnvironmentFile` mechanism as `MANAGER_TOKEN`), so they
survive redeploys. Leaving `GOOGLE_CLIENT_ID` unset keeps SSO off
entirely — `/api/meta`'s `google_sso` field reflects this, and
`login.html` only shows the "Sign in with Google" button when it's true.

### Deploy the opsavor.ai home page (`home`)

The public marketing/home page (`https://opsavor.ai`, apex + `www`) is a
small Node static server from the `opsavor/opsavor.ai` repo, running in the
`home` container and reverse-proxied by edge Caddy. It replaced a ChatGPT
Sites deployment; its DNS moved from GoDaddy/Cloudflare to DigitalOcean so
TLS uses the same DNS-01 provider as the rest of the edge.

```sh
# 1. DNS: point opsavor.ai (apex + wildcard) at the host, in DO DNS —
#    same provider/script as opsavor.app, different DOMAIN:
DO_API_TOKEN=… DOMAIN=opsavor.ai ./providers/digitalocean/dns.sh <node-1-public-ip>

# 2. Deploy key: the build container clones from the in-fleet Gitea over SSH
#    (git.opsavor.work:2222). Add a read-only deploy key to that repo
#    (repo → Settings → Deploy Keys) and drop the private half on the host:
#      /root/.ssh/gitea_opsavor_deploy   (chmod 600)
#    Separate from gitea_deploy (which is for git.theta42.com).

# 3. Build + launch + health-gate:
./scripts/deploy-home.sh main        # or a tag, e.g. home-v0.1.0

# 4. Only if edge/Caddyfile changed (the opsavor.ai block lives there):
./scripts/sync-edge-caddyfile.sh
```

The site is stateless — there is no data volume, so a deploy is just
build → delete `home` → launch → health-gate `http://127.0.0.1:3000/health`.
Caddy reaches it as `home:3000` over the bridge. To change page content,
edit the `opsavor/opsavor.ai` repo and re-run `deploy-home.sh`.

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

### Deploy Outline (wiki.opsavor.work, docs.opsavor.app)

Replaced Wiki.js (which had itself replaced BookStack) after a short trial
at `outline.opsavor.work` — that trial domain is retired now that
`wiki.opsavor.work` and `docs.opsavor.app` both serve Outline directly.
No dual-domain period: Outline's own URL/cookie/OAuth-callback config only
cleanly supports one canonical hostname. Same "upstream OCI image, no
build from source" approach as Plane: `scripts/deploy-outline.sh`
provisions Postgres (`outline-db`) and Redis/valkey (`outline-redis`) once
each (left alone on redeploys), generates `SECRET_KEY`/`UTILS_SECRET`/the DB
password on first run (`/root/.outline-secrets.env`), and always replaces
the `outline` app container itself. Unlike Wiki.js, Outline's own image
supports local-disk attachment storage (`FILE_STORAGE=local`) — no MinIO
needed. Uploads persist on a volume at `/var/lib/outline/data` (chowned to
uid/gid 1001 after attach — Outline's own `nodejs` user, not the 1000 other
images here use); everything else (docs, users, revisions) lives in
Postgres.

```sh
./scripts/deploy-outline.sh
./scripts/sync-edge-caddyfile.sh   # only needed if edge/Caddyfile changed
```

**Google sign-in**: unlike Wiki.js/BookStack, Outline reads
`GOOGLE_CLIENT_ID`/`GOOGLE_CLIENT_SECRET` directly from its container env
(no admin-UI step) — `deploy-outline.sh` already reuses the same shared
"Internal" Google Cloud OAuth client from `/root/.env`. The one manual step
is adding this redirect URI to that client in Google Cloud Console (there
is no API for it):

```
https://wiki.opsavor.work/auth/google.callback
```

Outline comes up and is reachable without this; the Google sign-in button
just won't complete the flow until it's added.

**No first-run wizard**: there's no separate setup step like Wiki.js's —
just sign in with Google once the redirect URI above is added. Confirmed
live: the first account to sign in gets `role = admin` automatically
(`SELECT email, role FROM users;` against `outline-db` if you need to
check without the UI) — nothing to promote by hand.

### Deploy Gitea (git.opsavor.work)

Same singleton-service shape as Outline: `scripts/deploy-gitea.sh` builds
the image, creates+attaches the persistent `gitea-data` volume (SQLite DB,
repos, and generated secrets all live there — see
`images/gitea/gitea-entrypoint.sh`), launches, health-gates. No separate
DNS or Caddy step needed — `git.opsavor.work` is already a host matcher in
the existing `*.opsavor.work` wildcard block in `edge/Caddyfile`, so it's
covered the moment `./scripts/sync-edge-caddyfile.sh` has been run once
(see "Deploy Outline" above if it hasn't).

```sh
./scripts/deploy-gitea.sh
```

A local `admin` account is created on first boot with a generated
password (never a well-known default) — `incus exec gitea -- cat
/data/gitea-secrets.env` to read it.

#### Git over SSH (port 2222)

Gitea's own built-in SSH server (not the host's OpenSSH, and not the
system container's — `START_SSH_SERVER = true` in the generated
`app.ini`), listening on 2222 inside the container. `deploy-gitea.sh`
attaches an Incus `proxy` device straight from the host's public interface
to the container (`ufw allow 2222/tcp` in `provision-host.sh`) — a
deliberate exception to "public traffic only through edge" (see Design
principles / Firewall above), since Caddy can't reverse-proxy a raw TCP
protocol like git-over-ssh the way it does gitea's HTTP traffic.

Clone/push URLs are `ssh://git@git.opsavor.work:2222/<owner>/<repo>.git`.
Each user adds their own public key through the Gitea UI (Settings → SSH /
GPG Keys) — there's no host-level SSH user or key involved; Gitea's SSH
server authenticates against keys stored in its own database and maps the
connection to `git-upload-pack`/`git-receive-pack` internally.

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

The `plane` container needs far more than the fleet's default 512MB
(`deploy-plane.sh` overrides it to 3GB/2 vCPU at the instance level): under
the default limit, the kernel OOM-killer inside the container's memcg kills
the API/worker/beat/migrator processes every few seconds, which looks
exactly like a stuck DB connection (`python manage.py wait_for_db`
appearing to hang forever) rather than what it actually is. Confirm with
`dmesg -T | grep oom-kill` on the host if this ever recurs.

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

# 7. Re-register the CI runner (act_runner, systemd service `act-runner`,
#    /root/act-runner/) with git.theta42.com — the registration token is
#    per-host and one-time-use; get a fresh one via
#    `tea api -X POST /orgs/opsavor/actions/runners/registration-token`,
#    then `act_runner register --no-interactive --instance
#    https://git.theta42.com --token <token> --name incus-host --labels
#    incus-host:host` from /root/act-runner, then
#    `systemctl enable --now act-runner`.
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
      service.yml              ← internal tooling (gitea, plane, outline)
      ci.yml                   ← CI runner (privileged, Incus socket mounted)
      restaurant.yml           ← per-site Next.js + SQLite
  images/
    base/                      ← Debian 13 + node:22 + common tools (build.sh)
    edge/                      ← Caddy binary + Caddyfile (build.sh)
    manager/                   ← management app (build.sh)
    home/                      ← opsavor/opsavor.ai home page (build.sh)
    restaurant/                ← restaurant app (build.sh)
    gitea/                     ← Gitea binary + config (build.sh)
    plane/                     ← Plane distribution (build.sh)
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
  while read; then stop non-essential services (plane, outline) or stop
  ct-runner if it is mid-build. Consider whether you have hit the Phase 2
  trigger.
