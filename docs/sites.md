# Sites: control plane, ingress, and bringing a workload up elsewhere

**Status:** design draft, not implemented. Written 2026-11-20 by william-opencode-agent, for William.
**Reads with:** [docs/daemon.md](daemon.md) — that document is the reference for what exists; this one is about
what the fleet has to become, and only states a new mechanism where none exists.

`daemon.md` already covers: several hosts and per-host plans (`## Several hosts`), the WireGuard mesh
(`## Mesh between hosts`), per-host DNS ownership (`### DNS with several hosts`), moving an instance and its
volumes (`### Moving an instance to another host`), backups and restore drills (`## Backups and
retention`). None of that is redesigned here. What is missing for a fleet that must survive losing a whole
site is: a **site** as a failure boundary, **ingress as a host capability** rather than a hard-coded name,
**RPO as a declaration**, and **a conductor** that brings a workload up somewhere else without an operator
doing five API calls by hand.

---

## 1. Two layers, and they are not the same layer

The word "fleet" has been doing two jobs:

- **native-ops** — the product: daemons, the configuration repository, and the control plane that operates
  them.
- **workloads** — everything the fleet runs: tenants, and the operator's own applications.

**DECIDED:** Mattermost, Plane, Outline and Twenty are **not** control plane. If Mattermost is down,
native-ops still operates. Only what native-ops needs to run is control plane.

The distinction is load-bearing: triplicating an application stack is impossible; triplicating four small
pieces plus DNS is cheap.

---

## 2. A site is a failure boundary

**DECIDED:** a **site** is an independently operable stack: it can run, deploy and serve its own hosts
without reaching any other site. It is not a region, a provider or a data centre.

**DECIDED:** the provider must not matter. A site may be a rack with one public IP, a VPS, a droplet, or a
machine under a desk. Nothing in the control plane may depend on a provider API, a provider private
network, a floating IP, or provider metadata. `provider: static` plus SSH and the mesh is the floor.

### 2.1 Shape in `fleet.yml` (PROPOSED)

```yaml
sites:
  va: { mesh_cidr: 10.99.1.0/24 }
  or: { mesh_cidr: 10.99.2.0/24 }

hosts:
  node-01:
    site: va
    address: "192.0.2.10"
    ingress: self                     # section 3
  node-02:
    site: va
    address: "192.0.2.11"
    ingress: { via: node-01 }
```

`hosts:` stays flat — a site is a label on a host, not a new nesting level — so the existing `hosts:`
parser and every existing manifest are unaffected. `mesh:` stays exactly as `daemon.md` documents it
(one mesh per fleet, addresses derived from it); a site's `mesh_cidr` is a proposed refinement for
grouping, not a change to the mesh.

### 2.2 What a site must own

Its own configuration mirror, its own daemon, its own deploy runner, its own ingress if it serves
anything, and a mesh peer. Section 5.6 is the checklist.

---

## 3. Ingress is a capability, not one host's job

Today an **edge is an instance** (`--edge-container`, default `edge`), its Caddyfile is in the plan
(`edge/Caddyfile`), and a host without one *notes* that instead of being blocked (`## Several hosts`). So
the daemon already models an edge as something pinned to a host. What it cannot express is: *this host
has no public ingress; publish its routes on that host* — which is why our fleet hard-codes
`EDGE_HOST: opsavor-node-01` in a workflow and refuses to write a tenant route when the tenant happens to
run on the edge host.

### 3.1 Host side (PROPOSED)

```yaml
ingress: self                    # public 80/443; terminates its own TLS
ingress: { via: colo-rack-a }    # no public ingress; routes are published on that host
ingress: none                    # nothing inbound, ever
```

### 3.2 Service side — the vocabulary already exists

- **`routing:`** — HTTP vhost (`pkg/config/config.go` `RoutingConfig`: `domain`, `upstream_port`, `tls`,
  `extra_directives`, `cache`, `rate_limit`, `external`), published by the host's daemon through
  `caddy.PublishSiteFor`. Implemented; not yet exercised in our configuration repository.
- **`forwards:`** — raw TCP/UDP into the container (`PortForward`: `protocol`, `listen`, `target`), already
  used for SMTP and git-over-SSH.
- **neither** — no ingress at all. A runner, a batch worker, a database.

**DECIDED:** there is no new per-service exposure field. A service that needs nothing inbound has neither
block. A plan **refuses** a service whose exposure its host cannot serve (`ingress: none` with `routing:`
or `forwards:` is a blocker, not a warning).

### 3.3 Mail is a port, not a vhost

`forwards:` covers it today. Any `via` ingress must therefore forward **TCP**, not only write site files
(Caddy's layer-4 plugin, or a small forwarder). That is why `forwards:` is part of this model rather than
an afterthought, and why `ingress: { via: … }` cannot be implemented as "scp a Caddyfile" alone.

### 3.4 What `via` needs

Exactly one thing from a target: **an address it can reach** (section 4). Same segment → the LAN address;
otherwise the mesh address. `via` never implies a shared provider network.

### 3.5 What this deletes from the fleet's configuration

- `EDGE_HOST` as a hard-coded workflow variable.
- The "tenant runs on the edge host → refuse" branch, because a `self` host publishes its own route.
- The dependency on a provider VPC for tenant HTTP.
- The failure mode "one edge down takes every tenant down".

### 3.6 The enabling change

Declared routing (theta42/native-ops#8): the hand-maintained `edge/Caddyfile` is still the source of truth
for every hostname, and **no service manifest uses `routing:` today**. Until routes are declared per
service, `ingress:` has nothing to resolve, and every hostname remains a file that only one host has.

---

## 4. Addresses: LAN first, mesh second, public never

The mesh exists (`## Mesh between hosts`): one `wg0` per host, a peer for every other host, routes to
every other host's bridge subnet, keys generated on the host with only public keys in `fleet.yml`.

**DECIDED:** enable it on every host, whether or not it is used. An idle tunnel costs nothing, and a mesh
you deploy the night you need it is a mesh you cannot rely on.

Resolution for an ingress is a priority, not a mode:

1. **lan** — same segment or provider network: no crypto overhead, highest throughput.
2. **mesh** — everything else, including colo-to-cloud and any private host without a shared segment.
3. **public** — only for endpoints meant to be public (an ingress itself). Never for tenant or daemon
   traffic without an explicit opt-in.

No new key handling is proposed: the mesh's own rules (private key never leaves the host, public key in
`fleet.yml`, plan blocked until it is committed) are the model to copy if anything else ever needs a key.

---

## 5. The control plane: hot-hot at three sites

**DECIDED:** the control plane runs hot-hot at **at least two sites other than the primary** — three
sites minimum — so losing two still leaves one able to deploy.

### 5.1 What is in it

| piece | why |
|---|---|
| **native-ops daemon** | applies every change |
| **Gitea (configuration repo)** | desired state and deploy tags; `pkg/gitsource` resolves tags against it and keeps **no local config cache** |
| **Caddy** | the daemon's own API and TLS |
| **deploy runner** | runs `gitops`, `deploy`, and the release dispatchers |
| **DNS** (third party) | how every other property is made true, and the one dependency outside all three sites |
| **daemon secret store** | `env_from` values, backup keys, DNS token |

**Not control plane:** Mattermost, Plane, Outline, Twenty, fleet managers, tenants.

### 5.2 Durable state, and what may be lost

| state | where | essential? |
|---|---|---|
| configuration repo + deploy tags | Gitea | **yes** — mirrored per site; nightly `git bundle` to object storage as a last resort |
| `env_from` / backup / DNS secrets | daemon store, per host | **yes** — synced per site |
| users, CI deployer tokens | daemon store | yes, re-issuable |
| jobs, plans, audit | JSON files in a `0700` dir (`pkg/server/jobs.go`) | no — history |
| TLS material | Caddy storage | reissuable |

Everything essential is in git or re-creatable from a credential. That is what makes hot-hot a bootstrap
problem rather than a replication problem.

### 5.3 Git: federated, single writer

**DECIDED:** federated. Each site runs its own Gitea with a **push mirror**; each daemon's `source` points
at its **site-local** URL, so a deploy never crosses sites to read a tag.

- **Single writer.** One site's Gitea is authoritative; the others mirror. No multi-writer git, no quorum,
  no consensus.
- **Promotion is manual.** Flip DNS and a local flag on the promoted site. It cannot be a commit, because
  the commit path is what is down.
- **Mirror lag is a deploy gate.** `gitsource` refuses a tag it cannot resolve, so a site behind its mirror
  cannot deploy that tag — correct, but invisible unless reported. Site status must show *"mirror behind
  primary: N commits / T s"*.
- **Last resort:** nightly `git bundle` into object storage. Mirroring needs a living source; if the
  primary dies the mirrors are the copies, and the bundle is what remains if promotion also goes wrong.

### 5.4 DNS: third party, two credentials

**DECIDED:** public DNS is hosted with a third party, not with any site. With DNS inside a site, that
site's outage also removes the ability to recover.

| credential | scope | held by |
|---|---|---|
| **ACME TXT-only** (`_acme-challenge` create/delete) | cannot move traffic | every site that issues certificates |
| **record writes** (A/AAAA/CNAME/MX) | can point a hostname anywhere | the promotion path only |

A compromised tenant host can renew its own certificate and still cannot redirect a customer's hostname —
the same rule as `env_from`: manifests name secrets, they do not carry them.

**Consequence:** with DNS-01 against a third-party zone, the destination can hold a valid certificate
**before** DNS points at it. The chicken-and-egg (no cert until traffic, no traffic until cert) is gone;
cutover is a record flip with no TLS gap.

Note `### DNS with several hosts` already splits record ownership per host. That stays; the change is
which credential writes the "point at a host" records, and where the zone lives.

### 5.5 The deploy runner is not a CI runner

**DECIDED:** the control-plane runner belongs to the configuration repository and is used **only** for
deployments, never arbitrary jobs.

- **Repo-scoped registration**: registered to the configuration repo only, so an application repository's
  workflow cannot execute on it. Application CI builds images; the deploy runner runs `gitops`, `deploy`
  and the release dispatchers.
- **Per site**, registered to that site's mirror, so a site can deploy while disconnected.
- **It holds the deployer token.** Everything above exists so that only this runner can reach the daemon
  with it.

Fan-out already exists as a CI matrix, one job per host with that host's URL and token
(`## Reconcile with several hosts`). A site is the same idea with the matrix narrowed to that site's hosts.

### 5.6 Bootstrap checklist — "hot" means all eight are true

1. Gitea up; configuration repo mirrored; **tags resolve locally**.
2. Daemon up; `source` = the site-local Gitea; secrets synced.
3. Runner registered to this site's Gitea; workflows target this site's daemons.
4. Caddy serving the daemon's API and TLS.
5. Mesh peer up and routable **before** it is needed.
6. Images refreshed at every release — a bring-up must not contain a build.
7. Both DNS credentials present and scoped.
8. `native-ops plan` returns **"no changes"** against the site. That is the proof it is converged.

---

## 6. RPO is per service, and it is reported

**DECIDED:** RPO is declared per service, with a class default; the snapshot/backup schedule derives its
interval from it.

**PROPOSED** starting points:

| class | RPO | mechanism |
|---|---|---|
| configuration repo | seconds | push mirror |
| images | one release | refresh on the standby site at release |
| secrets | on change | `secret-sync` |
| control-plane services | hours | volume snapshot + off-host copy (`## Backups and retention`) |
| tenant database | **15 min** | host-side `sqlite3 .backup` → object storage in another provider |
| tenant artifacts | object store's own | the platform's bucket, not native-ops |

Tenant databases are SQLite; `.backup` is the consistent, atomic copy, needs no WAL protocol, and needs no
change to the platform containers. Litestream-class shipping is the upgrade path if 15 minutes ever stops
being enough.

**The backup destination is per daemon and pinned** (`NATIVE_OPS_BACKUP_ENDPOINT`/`_BUCKET`), and object
keys carry the host, so hosts sharing a bucket never overwrite each other. **A standby that restores only
from the primary's provider is not a standby**: a copy must exist that does not depend on the site being
replaced.

**The stronger half:** every service reports the **age of its newest restorable copy**, and status surfaces
`rpo_violated` when it exceeds the declaration. That turns DR into a number — and it is the same field the
standby and the migration consume.

The rehearsal primitive already exists:

```bash
native-ops remote restore --volume gitea-data --as gitea-drill
```

A scheduled drill on a standby site is the difference between a standby and a hope.

---

## 7. One primitive, three modes

Bringing a workload up elsewhere is the same operation whether it is a rehearsal, a migration or a
disaster.

```
bring-up(spec, data, secrets, host) -> running instance + route
  standby : bring-up, never enabled          (rehearsed on a schedule)
  migrate : bring-up, stop the source, final sync, fence the source, cutover
  promote : bring-up, fence the source, cutover      (the source is gone or must not serve)
```

Reuse, do not reimplement — `### Moving an instance to another host` already defines the steps:

- `migrate` (`pkg/engine/migration.go`) — snapshot, warm copy, verified stop, final incremental copy,
  start, health-probe inside the container; any failure after the stop restarts the source.
- `export-spec` / `adopt` — the spec is recorded on the instance (without `env`) and adopted by the target;
  adopt never creates or replaces, it checks and takes over.
- `finalize-migration` (`pkg/engine/migration.go`) — refuses unless the target runs with every volume and
  passes health; keeps the source volumes by default.
- `MigratedTo` — a source refuses to manage a copy that moved.

**Missing, and small:** a conductor that walks a set of workloads with a journal (so a re-run skips what is
done), and an ingress-aware route step. The per-instance primitives need no change.

**"Up" is a definition, not a feeling.** All four:

1. the database passes an integrity check and its record counts match the last known-good copy;
2. the application's health endpoint answers **from inside the container**;
3. its hostname answers through the intended ingress;
4. one smoke write succeeds and reads back.

The same four serve a standby rehearsal, a migration cutover and a promotion.

**Secrets are a promotion step.** Adopt restores the spec; `env` is deliberately never exported, because
the instance's environment file moved with it — but `env_from` values live in the *host's* secret store.
Syncing every tenant's secrets to every standby widens the blast radius for no gain; sync them when a
tenant is actually promoted.

**Fencing is the only coordination need.** Two live copies of one tenant is the failure mode of every DR
design. The authoritative fact is *"slug X is served by host Y as of epoch Z"* — a compare-and-set on the
tenant row, not a new database. Manual promotion is the current answer; automation later must still respect
the epoch.

---

## 8. Failure matrix

| failure | effect | what saves you |
|---|---|---|
| one site down | its tenants unavailable; other sites unaffected | mesh + per-site git/runner; promote only what must move |
| primary Gitea down | no new deploys anywhere | mirrors serve reads; promote a mirror (manual) |
| DNS credential lost | no new certs, no cutover | the two credentials rotate independently |
| mirror lag | that site cannot deploy the newest tag | status reports lag; deploy from another site |
| daemon state lost | jobs/plans history gone | secrets re-sync, tokens re-issue; nothing customer-facing |
| split brain | two writers to one tenant DB | tenant-row epoch + fencing before cutover |
| provider outage | only that provider's site | sites must not share a provider, or DNS, with each other |

---

## 9. Phasing

**Phase 0 — configuration only, no daemon release**
`sites:` and `host.site`; `ingress: self` on hosts that already terminate TLS; `rpo:` defaults; per-site
mirror and runner (operational). Existing `via` behaviour (the fleet's own route workflow) keeps working
unchanged; `self` hosts are verified by hand.

**Phase 1 — daemon**
`ingress` resolution for `routing:`/`forwards:`; plan refusals for exposure a host cannot serve;
refuse-if-migrated for **services** (today only instances have the marker — the deployer has no
equivalent); RPO reporting in status.

**Phase 2 — the conductor**
`bring-up` with a journal and the `standby | migrate | promote` modes; the DR drill as a scheduled job.

**Phase 3 — fleet managers**
Placement by site, promotion policy, DNS writes through the scoped credential.

---

## 10. Explicitly not in scope

- **Automatic failover.** Promotion is a manual runbook; every automatic design is a split-brain design
  until fencing is proven in anger.
- **Multi-writer git.** One writer, mirrors, manual promotion.
- **Quorum or consensus.** A site is not a vote.
- **Cross-region object-store consistency.** The object store is the platform's business.
- **Data residency as a blocking constraint.** Absent a contract naming a country, site choice is latency
  and price — the best position to be in.

---

## 11. Open

1. **`host.site` tag vs nesting hosts under `sites:`** — tag proposed, to keep `hosts:` flat.
2. **Do control-plane *workloads* need a live copy per site, or only recoverability?** The six
   control-plane pieces are settled; this is about, e.g., whether Mattermost runs at two sites.
3. **Scheduling declared routing** (theta42/native-ops#8). It gates section 3; without it, `via` remains
   the fleet's own workflow rather than the daemon's model.
4. **Standby sizing** — one shared standby sized for the top-K tenants, rather than one per tenant. A
   capacity number, not a mechanism.
