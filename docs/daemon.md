# The native-ops daemon

`native-ops serve` is an optional daemon that runs **on the host it manages** and exposes an
authenticated HTTP API plus a small web UI. It exists so that a deployment can be driven
entirely from CI over HTTPS with an API token:

- Nobody installs anything locally to operate a fleet. There is no client to install.
- CI never needs an SSH key or a runner on the host.
- The **CLI stays the source of truth** and keeps working without the daemon. The daemon adds
  what the CLI cannot: API tokens, an audit log, and (next) job history and locking.

The daemon itself is installed and updated by IaC/CI, like everything else. It is not something
a person sets up by hand.

## What it does today (read-only)

| Endpoint | Auth | |
|---|---|---|
| `GET /healthz` | none | liveness (`{"ok":true,"version":...}`), no state |
| `GET /v1/whoami` | any token | which token and role you are |
| `GET /v1/status` | viewer | instances, data volumes, images, host metrics (load, memory, swap, pool disk, uptime) and warnings -- plus, when configured, the edge's routes and certificates and the DNS records in the configured zones |
| `POST /v1/plan` | planner | what `apply` would change for the configuration you upload (never changes the host) |
| `GET /v1/plans`, `GET /v1/plans/{hash}` | viewer | the plans that were made, and where each stands (pending, approved, used, expired, blocked) |
| `POST /v1/plans/{hash}/approve`, `DELETE /v1/plans/{hash}/approval` | admin | approve one apply of exactly this plan, or take the approval back |
| `POST /v1/apply` | deployer | apply an **approved** plan, as a job (only with `--enable-apply`) |
| `GET /v1/jobs`, `GET /v1/jobs/{id}` | viewer | jobs (apply and instance changes) and their outcome; the log is shown to deployers and admins only |
| `PUT /v1/instances/{name}`, `POST /v1/instances/{name}/update`, `DELETE /v1/instances/{name}` | deployer | create, move to another image, or remove a tenant instance, as a job (only with `--enable-instances`) |
| `POST /v1/instances/{name}/resize` | deployer | a live `limits.cpu`/`limits.memory` change, as a job; no restart |
| `POST /v1/instances/{name}/suspend` | deployer | replace the published route with a static 503 naming a reason, as a job, without touching the instance; undone by asking for the instance again (`PUT`), which always republishes the normal route |
| `POST /v1/images/build` | deployer | build and publish an application image from an uploaded configuration tree, as a job (only with `--enable-image-build`) |
| `GET /v1/instances`, `GET /v1/instances/{name}` | viewer | the tenant instances the token may see |
| `GET /` | none | the UI (Overview, Instances, Volumes, Network). It holds no data; it signs in (or takes a token) and calls `/v1/status` |

`native-ops status [--json]` prints the same snapshot from the CLI. It reports **key names only**
for instance config (OCI containers keep their secrets in `environment.*`); values are never read
into the output, other than resource limits and `user.native-ops.*` bookkeeping.

Beyond the Incus inventory the snapshot carries the host's own **metrics** (load, memory, swap, the
pool's disk, uptime), the **edge's routes** (each hostname and the upstreams it points at) and
**certificates** (names, issuer, expiry), and the **DNS records** in configured zones. The daemon reads
the edge's live Caddy config and certificate store through Incus (`--edge-container`, default `edge`),
and lists DNS through a provider (`--dns-provider digitalocean --dns-domains opsavor.app,opsavor.work`,
using `DO_API_TOKEN`). `native-ops status` includes the metrics; the daemon adds the edge and DNS when
they are configured.

Errors are `{"error": ..., "code": ...}`; a failure to read the host is a generic `502` and never
echoes internal text.

## Security model

The daemon runs as an unprivileged user in the `incus-admin` group, which is powerful (that group
can create privileged containers), so it is deliberately small:

- Every `/v1/` route needs a bearer token. An unknown `/v1/` path also needs one, so the API does
  not reveal which routes exist.
- Tokens are `nops_` plus 64 hex characters. Only the SHA-256 is stored, in a `0600` file inside a
  `0700` state directory; a world-readable token file is refused at startup. The secret is shown
  once, at creation, and compared in constant time.
- Roles: `viewer` < `planner` < `deployer` < `admin`. Reading needs `viewer`; sending a configuration
  to be planned needs `planner` (a plan never changes the host, so this is the role for a pull-request
  pipeline: the secrets of a repository are readable by any branch of it); applying needs `deployer`;
  managing tokens needs `admin`. The gate is tested so the write endpoints that follow inherit it.
- Every request is appended to `audit.log` (JSON lines): time, token name, method, path, status,
  remote address, duration. Never the query string, never a credential.
- The UI is served with a strict Content-Security-Policy (`default-src 'none'`, no inline script or
  style) and inserts all data as text, never as HTML.
- The UI uses the same shell as the other theta-suite apps (proxy, jump-host, sso-manager): Bootstrap 5.3
  and Font Awesome Free, vendored under `pkg/server/ui/static-modules/` with their licenses and served
  from the binary, since the CSP allows `'self'` only. Nothing is fetched from a CDN, so it works on a
  host with no outbound access. To update them, copy the new `dist` files over the old ones (strip the
  `sourceMappingURL` comments, the maps are not shipped) and run `go test ./pkg/server`: the tests fail if
  the page names a file the binary does not serve.
- It listens on loopback (or the Incus bridge address) and must sit behind TLS, e.g. the Caddy
  edge. Do not expose the port directly.

## Planning from CI

A pipeline sends the tree it checked out; the daemon needs no git access and holds no
credentials for your repository. Give a pull-request pipeline a `planner` token: it can plan, and
can never apply even if a branch rewrites the workflow to try. The answer is what `native-ops plan` prints, plus a hash of it:

```bash
tar -czf - -C . fleet.yml services templates \
  | curl -fsS -X POST "$NATIVE_OPS_URL/v1/plan?sha=$COMMIT&service=gitea" \
      -H "Authorization: Bearer $NATIVE_OPS_TOKEN" -H "Content-Type: application/gzip" \
      --data-binary @- -o plan.json
jq -r .text plan.json            # the plan, ready to read (or post to the pull request)
jq -e '.exit != 1' plan.json     # fail the job only when apply would fail
```

`exit` follows `native-ops plan`: `0` nothing to change, `2` changes pending, `1` blocked. `hash`
identifies exactly what the plan would do (the same changes give the same hash), so a later
apply can be tied to the plan somebody reviewed. `sha` and `service` are optional and only
recorded in the audit log.

The upload is untrusted, and is handled that way:

- It must be a gzip-compressed tar of regular files and directories. A symlink, hard link,
  device, an absolute name or one that climbs out with `..` is refused, not skipped; nothing can
  be written outside the private temporary directory it is unpacked into, which is removed
  afterwards. Permission bits in the archive are ignored.
- At most 32 MiB uploaded, 128 MiB unpacked (counted as bytes actually written) and 5000
  entries, and at most 4 plans at a time (`429` beyond that).
- A manifest path (`env_file`, a hook file) that leaves the configuration directory is an error,
  so a pull request cannot make the plan read a file elsewhere on the host.
- The plan runs behind the same read-only executor as the CLI: the uploaded tree can be
  planned against the host but nothing in it can change the host. Environment values never
  appear in the answer, only key names.
- A configuration the daemon cannot use is `400` with the reason (`bad_config`, `bad_archive`);
  a host it cannot read is a generic `502`.
- The audit log records who planned, the commit, the service, the outcome and the hash (never
  the archive, and never a query string).

## Applying from CI

Applying is off unless the daemon is started with `--enable-apply`: without it the daemon can
read the host and plan an uploaded tree, and nothing more. With it, an apply is harder than a plan
on purpose:

```bash
# 1. plan (a pull request can post .text); keep the hash of the plan that was reviewed
curl ... "$URL/v1/plan?sha=$COMMIT"  --data-binary @conf.tgz -o plan.json
HASH=$(jq -r .hash plan.json)

# 2. an admin approves that plan hash (the UI's Plans page, or POST /v1/plans/$HASH/approve)

# 3. apply that plan: the same tree, and the hash. 202 with a job; poll it
curl ... -X POST "$URL/v1/apply?sha=$COMMIT&expect=$HASH" --data-binary @conf.tgz -o job.json
curl ... "$URL/v1/jobs/$(jq -r .job.id job.json)"      # status: running | succeeded | failed | interrupted
```

- **`expect` is required.** The daemon plans the upload again, and unless that plan has the hash
  you gave it answers `409 plan_changed` with the plan it would run now, and touches nothing. What
  was reviewed is what runs; if the host or the configuration moved since, you review again. The
  hash is keyed with a secret the daemon keeps (`plan.key` in the state directory, stable across
  restarts), so it also covers the parts of a manifest the plan does not print: environment
  values and hook bodies. Approving one secret and uploading another is refused, and the hash tells
  nobody anything about the value.
- **An admin has to approve that plan first.** The deployer token that CI holds is not enough to
  change the host, because this Git host shows a repository's secrets to every branch of it. Every
  plan the daemon makes is recorded (`plans/<hash>.json`, 0600). An admin approves one in the UI
  (Plans, then Approve) or with `POST /v1/plans/<hash>/approve`; the approval names who gave it,
  **lets exactly one apply of exactly that plan through, and expires** (an hour; `--approval-ttl`).
  Without it the apply answers `403 not_approved` with the plan and where to approve it, and
  nothing runs; an expired one is `403 approval_expired`. Presenting the same plan again after it
  was applied is a new occurrence and needs a new approval. A plan that is blocked, or that has
  nothing to change, cannot be approved. Approvals survive a restart of the daemon and are audited.
- **A blocked plan is never applied** (`409 blocked`), and a plan with nothing to change starts no
  job (`200 nothing_to_do`).
- **One apply at a time** on the host (`409 busy`, naming the running job), taken before the
  upload is read and released even when the request is refused.
- **Only what the plan lists is applied**, in plan order, stopping at the first failure. Services
  after it are untouched; running the same apply again is safe and continues.
- **The job is a record.** It outlives the request and is written to `jobs/<id>.json` (0600) as it
  runs: who, which commit, the plan hash, timestamps, the log and the outcome. The last 200 are
  kept. A job that was still running when the daemon stopped is marked `interrupted` on the next
  start, with the log so far: the host may be part-way through it, and the fix is to apply again.
  A stop waits up to 5 minutes for a running apply to finish first (`TimeoutStopSec=6min` in the unit).
- The audit log records the request (`apply job=... sha=... hash=...`, or why it was refused) and
  the end of the job.

The UI (`/`) has a Plans page (each plan, what it would do, who asked, and an Approve or Withdraw
button for admins) and, when apply is on, a Jobs page with each job's outcome and log.

An apply runs `incus` as the daemon's user (which is in `incus-admin`, so it can do to instances
whatever a deployer's manifest says) and host hooks (`pre_deploy`, `post_deploy`) as that user,
inside the unit's sandbox (`ProtectSystem=strict`; only its state directory is writable). A
`deployer` token is therefore as powerful as the manifests an admin approves it to apply: use one
token per pipeline, plan from pull requests with a `planner` token, and apply from the protected
branch. Because a Git host that shows secrets to every branch cannot keep a deployer token from a
pull request, the approval is what stops that token from changing the host on its own: the most a
stolen or misused deployer token can do is apply a plan an admin already looked at and approved,
once, within the hour.

## Tenant instances (for a fleet manager)

Static services (gitea, plane, ...) are declared in git and applied. **Tenants** (one instance per customer,
created and removed while the product runs) are managed by a system that knows about customers, through
this API, without holding a key to the host. Off unless the daemon is started with `--enable-instances`.

```bash
# a token that can only ever manage demo-*/rest-* instances, from one image family, on one zone
native-ops token create --state-dir /var/lib/native-ops --name fleet-manager --role deployer \
    --names 'demo-*,rest-*' --images 'opsavor-platform:*' --domains '*.opsavor.app'

curl -X PUT "$URL/v1/instances/demo-multi" -H "Authorization: Bearer $TOKEN" -d '{
  "template": "platform", "image": "opsavor-platform:latest",
  "limits": {"limits.cpu": "1", "limits.memory": "1GB"},
  "volumes": [{"name": "demo-multi-data", "path": "/app/.data", "owner": "platform"}],
  "service": "platform", "env": {"OPSAVOR_SEED": "multi", "PORT": "8787"},
  "health": {"path": "/health", "port": 8787},
  "domain": "demo-multi.opsavor.app", "route_directives": ["import strip-forged-identity"]
}'                                          # 202 and a job; poll GET /v1/jobs/{id}
```

`PUT` makes the instance exist as described: it creates it (volume, environment file at
`/etc/default/<service>`, the unit started once the environment is there, health gate, route), or resumes and
converges the one an earlier call created, so it is safe to repeat. `update` moves it to another image through
the safe path (volume snapshot first, environment carried over, health-gated, rolled back on failure).
`resize` is a live `limits.cpu`/`limits.memory` change (no restart, no image change). `suspend` (`{domain,
reason,route_directives?}`) replaces the published route with a static 503 naming `reason`, without
stopping or otherwise touching the instance; there is deliberately no `unsuspend` -- `PUT` always
republishes the normal route, whatever was there before, so asking for the instance again is how a
suspension ends. `DELETE` removes it and its route; `?purge_volumes=true` also deletes the data volumes
named after it.

What stops a fleet manager, or anyone holding its token, from doing more than that:

- **A scope.** A token created with `--names`/`--images`/`--domains` (globs) can call only these endpoints, and
  only for instances, images and domains that match; it is refused everywhere else (plan, apply, status). It
  must be a `deployer` token and needs names and images (no domains means no route may be published).
- **A strict spec.** A request is checked against a short list of shapes before anything runs: only the
  profiles the operator allows (`--instance-profiles`, default `base,service`), only `limits.cpu` and
  `limits.memory`, data volumes named `<instance>-...` at clean absolute paths (`owner`, if given, only a
  plausible Unix user name -- a fresh volume attaches root-owned, so a service that runs as another user
  needs its mount point, never its contents, handed over once), environment names in `[A-Z_][A-Z0-9_]*`
  with no line breaks, a health check, a host name, and route directives that are only `import <snippet>`
  from the operator's list (`--instance-route-imports`, default none). Unknown fields are refused. There is
  no way to ask for a privileged container, a host path, another instance's volume, or a shell. `resize`
  and `suspend` are checked against the same limit and route-import rules.
- **Only tenants.** An instance can be changed or removed here only if it was launched from a template
  (`user.native-ops.template`). gitea, plane and every other static service carry none and are refused, for
  every token, an admin's included; an instance of another template is never taken over.
- **One change at a time**, shared with apply: `409 busy` while a job runs, and a finished job means the host
  is free. Each change is a job with a record; tenant secrets in `env` never appear in a job log, the audit log
  or any answer.

## Tokens and bootstrapping

```
native-ops token create --state-dir /var/lib/native-ops --name ci-plan --role planner
native-ops token list   --state-dir /var/lib/native-ops
native-ops token revoke --state-dir /var/lib/native-ops --id <id>
```

A running daemon honours tokens created this way immediately (it notices the file change).

To avoid running any command on the host, IaC/CI can provision the first credential as a secret:
set `NATIVE_OPS_BOOTSTRAP_TOKEN=nops_...` (at least 32 characters after the prefix) in
`/etc/native-ops/serve.env`. The daemon loads it as an in-memory `admin` token named `bootstrap`;
it is never written to disk. Rotate it by changing the secret and restarting.

## UI sign-in: local users and OIDC

By default the UI takes a pasted API token (kept for the browser tab). Two sign-in methods can be turned
on instead, so people reach the console without holding a token:

- `--enable-auth` serves local sign-in: a username and password against `users.json` in the state dir,
  managed with the CLI below. Passwords are bcrypt hashes; each user carries a role (viewer, planner,
  deployer, admin).
- The OIDC flags add a generic OpenID Connect sign-in: `--oidc-issuer`, `--oidc-client-id`,
  `--oidc-client-secret`, `--oidc-redirect-url`, and optional `--oidc-allowed-domain`, `--oidc-role` and
  `--oidc-label`. Discovery, the authorization redirect, the code exchange and the userinfo call are all
  the daemon's; a person is matched to a local user by their verified email (created at `--oidc-role` on
  first sight).

Either way a sign-in sets an HttpOnly session cookie (HMAC-signed with a key in the state dir), and the
UI calls the API with it. API tokens keep working unchanged, for CI and machines.

Every auth/OIDC setting also has an environment form (`NATIVE_OPS_ENABLE_AUTH`, `NATIVE_OPS_OIDC_*`;
see `native-ops serve --help`), so the OIDC client secret can live in the root-only
`/etc/native-ops/serve.env` instead of a unit file.

```
# omit --password to read one line from stdin (so it is not in the shell history)
echo "a long enough password" | native-ops user create --state-dir /var/lib/native-ops --username sam --role deployer --name "Sam"
native-ops user list   --state-dir /var/lib/native-ops
native-ops user passwd --state-dir /var/lib/native-ops --username sam       # reads the new password from stdin
native-ops user role   --state-dir /var/lib/native-ops --username sam --role admin
native-ops user disable --state-dir /var/lib/native-ops --username sam
```

A running daemon honours user changes immediately (it notices the file change).

## Running it

`deploy/systemd/native-ops-serve.service` is the unit CI/IaC installs (hardened; state in
`/var/lib/native-ops`; env in `/etc/native-ops/serve.env`). The user needs to exist and be in
`incus-admin`:

```
useradd --system --home-dir /var/lib/native-ops --shell /usr/sbin/nologin -G incus-admin native-ops
```

Then route it through the edge, e.g. a Caddy site `native-ops.example.com { reverse_proxy 10.0.100.1:8686 }`
with the daemon bound to the bridge address, or `127.0.0.1` if Caddy runs on the host.

## Roadmap

Instance update/migrate/backup as jobs; a live view of a running job; and per-user API tokens
(now that people sign in, a token could be tied to the person who made it). The API is versioned (`/v1`) so a separate fleet manager
can depend on it.
