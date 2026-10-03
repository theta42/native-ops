# The native-ops daemon

`native-ops serve` is the daemon that runs **on each host it manages** and exposes an
authenticated HTTP API plus a small web UI. It is how a fleet is operated: git is the source of
truth, CI is the only control path, and the daemon is what CI talks to.

- Nobody installs or runs a control app on their own machine. CI runs `native-ops remote ...`
  (or plain `curl`) against the daemon's HTTPS API with a token.
- CI never needs an SSH key or a runner on the host.
- The daemon adds what a script on a runner cannot: API tokens with roles and scopes, plan
  approval, one change at a time on a host, job records and an audit log.
- The same engine is in the CLI, so `native-ops apply`, `backup`, `dns sync` and the rest still
  work when run on the host itself -- for a host with no daemon yet, or to repair one.

**Driving deploys from CI, scripts or AI agents** (preview a tag, deploy it, see what is live, follow
jobs, the MCP endpoint): see [api.md](api.md). Every endpoint is described by the OpenAPI document the
daemon serves at `/openapi.json`.

The daemon is installed by cloud-init when `native-ops host create --daemon-version ...` creates
the host, or by your IaC. It is not something a person sets up by hand.

## Endpoints

| Endpoint | Auth | |
|---|---|---|
| `GET /healthz` | none | liveness (`{"ok":true,"version":...}`), no state |
| `GET /api/session` | none | how this daemon lets people sign in, and who is signed in (with `--enable-auth` or OIDC) |
| `POST /api/login`, `POST /api/logout` | none / session | local sign-in (sets the session cookie) and sign-out |
| `GET /auth/oidc`, `GET /auth/oidc/callback` | none | the OIDC sign-in redirect and its callback (with the `--oidc-*` flags) |
| `GET /v1/whoami` | any token | which token and role you are |
| `GET /v1/status` | viewer | instances, data volumes, images, host metrics (load, memory, swap, pool disk, uptime) and warnings -- plus, when configured, the edge's routes and certificates and the DNS records in the configured zones |
| `POST /v1/plan` | planner | what `apply` would change for the configuration you upload (never changes the host) |
| `GET /v1/plans`, `GET /v1/plans/{hash}` | viewer | the plans that were made, and where each stands (pending, approved, used, expired, blocked) |
| `POST /v1/plans/{hash}/approve`, `DELETE /v1/plans/{hash}/approval` | admin | approve one apply of exactly this plan, or take the approval back |
| `POST /v1/apply` | deployer | apply an **approved** plan, as a job (only with `--enable-apply`) |
| `GET /v1/jobs?kind=&status=&limit=`, `GET /v1/jobs/{id}?wait=` | viewer | jobs and their outcome, filtered by kind (or a family, `instance:`) and status; `wait` (up to 60 seconds) answers when a running job ends. The log is shown to deployers and admins only. A scoped token sees only jobs it started and jobs about instances its scope allows |
| `POST /v1/edge/apply` | deployer | apply the uploaded tree's `edge/Caddyfile` to the edge container (validated, rolled back if Caddy rejects it), as a job (only with `--enable-edge-apply`) |
| `POST /v1/backups?volume=&prune=1` | deployer | back up one volume, or every volume `fleet.yml` allows, then optionally apply retention, as a job (only with `--enable-backup`) |
| `POST /v1/backups/restore?volume=&from=&as=&force=1` | admin | restore a volume in place (keeping a copy of the current one) or as a new volume, as a job (only with `--enable-backup`) |
| `POST /v1/dns/sync` | deployer | create or update `fleet.yml`'s `dns_records`, as a job (only with `--enable-dns-sync`) |
| `POST /v1/deploy` | deployer | deploy the commit a protected deploy tag points at, read from the git server (`{"tag": ...}`), as a job (with `--enable-apply`, `--git-url` and `--deploy-repo`) |
| `POST /v1/deploy/plan` | planner | what deploying a tag would change, read from the git server and planned against the host now, without a job and without changing anything (`{"tag": ...}`; with the same flags as deploys) |
| `GET /v1/deploys?limit=` | viewer | what the host runs (`current`: the newest deploy that left it matching its tag), the deploy in progress, and the history, each with its tag, commit, result and plan counts |
| `POST /v1/daemon/upgrade` | admin | upgrade this daemon to a pinned release (`{version, sha256}`), as a job; it then restarts on the new binary (when the binary is in `<state-dir>/bin`) |
| `GET/PUT /v1/secrets`, `DELETE /v1/secrets/{name}` | admin | the daemon's own credentials, pushed from the git server's secret store; names only are ever returned |
| `GET/POST /v1/tokens`, `DELETE /v1/tokens/{id}` | admin | list, create (the secret is returned once) and revoke API tokens |
| `GET/POST /v1/users`, `PATCH /v1/users/{username}` | admin | list and create local users; change a role, disable, or set a password (with `--enable-auth` or OIDC) |
| `PUT /v1/users/by-email/{email}` | admin | give the user with this email a role (and `disabled`), creating it to sign in over OIDC if there is none: how a directory grants access before a first sign-in and removes it after someone leaves |
| `GET /v1/images/recipes`, `POST /v1/images/recipes/{digest}/approve`, `DELETE /v1/images/recipes/{digest}/approval` | admin | the image recipes builds were asked for, and approving or withdrawing one (with `--enable-image-build`) |
| `PUT /v1/instances/{name}`, `POST /v1/instances/{name}/update`, `DELETE /v1/instances/{name}` | deployer | create, move to another image, or remove a tenant instance, as a job (only with `--enable-instances`) |
| `POST /v1/instances/{name}/resize` | deployer | a live `limits.cpu`/`limits.memory` change, as a job; no restart |
| `POST /v1/instances/{name}/suspend` | deployer | replace the published route with a static 503 naming a reason, as a job, without touching the instance; undone by asking for the instance again (`PUT`), which always republishes the normal route |
| `POST /v1/images/build` | deployer | build and publish an application image from an uploaded configuration tree, as a job (only with `--enable-image-build`) |
| `POST /v1/images/prune` | deployer (unscoped) | image retention as a job: delete images no instance runs that are orphans or old tags (`?dry_run=1` reports only; with `--enable-image-build`) |
| `GET /v1/instances`, `GET /v1/instances/{name}` | viewer | the tenant instances the token may see |
| `POST /mcp` (`GET`, `DELETE`: 405) | viewer, bearer token only | the Model Context Protocol server: the deploy, job, plan and status endpoints as tools for AI agents, with the caller's token and role ([api.md](api.md#mcp)) |
| `GET /.well-known/oauth-protected-resource` (and `/.well-known/oauth-protected-resource/mcp`), `GET /.well-known/oauth-authorization-server` | none | OAuth discovery for `/mcp` (RFC 9728, RFC 8414), with sign-in on and `--mcp-oauth` (the default) |
| `POST /oauth/register`, `GET`/`POST /oauth/authorize`, `POST /oauth/login`, `POST /oauth/token`, `POST /oauth/revoke` | none (a sign-in, then consent) | an MCP client registers, sends its person through the daemon's sign-in and a consent page, and gets tokens that act as that person, at `/mcp` only ([api.md](api.md#connect-by-signing-in-oauth)) |
| `GET /v1/oauth/grants`, `DELETE /v1/oauth/grants/{id}` | admin | which MCP clients people signed in, and revoking one (also on the UI's Tokens page) |
| `GET /openapi.json`, `GET /openapi.yaml` | none | the OpenAPI 3.1 description of every endpoint here (the same on every daemon; no host data) |
| `GET /` | none | the UI (Overview, Instances, Volumes, Network). It holds no data; it signs in (or takes a token) and calls `/v1/status` |

`native-ops status [--json]` prints the same snapshot from the CLI. It reports **key names only**
for instance config (OCI containers keep their secrets in `environment.*`); values are never read
into the output, other than resource limits and `user.native-ops.*` bookkeeping.

Beyond the Incus inventory the snapshot carries the host's own **metrics** (load, memory, swap, the
pool's disk, uptime), the **edge's routes** (each hostname and the upstreams it points at) and
**certificates** (names, issuer, expiry), and the **DNS records** in configured zones. The daemon reads
the edge's live Caddy config and certificate store through Incus (`--edge-container`, default `edge`),
and lists DNS through a provider (`--dns-provider digitalocean --dns-domains example.com,example.net`,
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
  approving plans and image recipes, restoring backups, and managing tokens, users and secrets need
  `admin`. A token with a **scope** (instance names, images, domains) may only call the instance
  endpoints, for what its scope allows, and sees only its own jobs. Every endpoint's role gate is tested.
- What a deployer token cannot do on its own: apply a plan an admin has not approved (one apply, within
  the approval's lifetime), build from a recipe an admin has not approved, send backups anywhere but the
  pinned destination, or change DNS outside the allowed zones.
- A signed-in person's session is checked against the user store on every request; repeated failed
  sign-ins for an account are throttled; OIDC uses PKCE.
- File contents and scripts reach containers over stdin, never in a command line; errors and job
  records name only the start of a command, so they cannot carry a secret.
- Every request is appended to `audit.log` (JSON lines): time, token name, method, path, status,
  remote address, duration, and what it did (a plan hash, a job, which secrets changed). Never the query
  string, never a credential or secret value.
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
button for admins), a Jobs page with each job's outcome and log whenever anything that changes the
host is enabled, and, for admins, a Tokens page (create a token -- its secret is shown once -- or
revoke one, so CI's tokens are made from a browser, not a shell on the host) and, on a daemon that
builds images, a Recipes page to approve or withdraw image recipes.

An apply runs `incus` as the daemon's user (which is in `incus-admin`, so it can do to instances
whatever a deployer's manifest says) and host hooks (`pre_deploy`, `post_deploy`) as that user,
inside the unit's sandbox (`ProtectSystem=strict`; only its state directory is writable). A
`deployer` token is therefore as powerful as the manifests an admin approves it to apply: use one
token per pipeline, plan from pull requests with a `planner` token, and apply from the protected
branch. Because a Git host that shows secrets to every branch cannot keep a deployer token from a
pull request, the approval is what stops that token from changing the host on its own: the most a
stolen or misused deployer token can do is apply a plan an admin already looked at and approved,
once, within the hour.

## Deploying on a tag

With `--git-url`, `--deploy-repo` and `--enable-apply`, **pushing a protected deploy tag deploys**:

```bash
git tag deploy-2026.10.03 && git push origin deploy-2026.10.03     # someone the repository allows
# CI, on that tag:
NATIVE_OPS_TOKEN=$DEPLOY_TOKEN native-ops remote deploy --tag deploy-2026.10.03
```

Preview first with `native-ops remote deploy-plan --tag ...` (`POST /v1/deploy/plan`, planner: the same
commit, planned against the host now, nothing changed), and see what is live with
`native-ops remote deploys` (`GET /v1/deploys`). [api.md](api.md) has the recipes, the results a deploy
ends with, and what to do when one fails.

`POST /v1/deploy {"tag"}` (deployer) runs a job that:

1. resolves the tag on the git server, and checks that a **tag protection rule** covers it, so only the
   people the repository allows could have pushed it (a rule the daemon cannot read, or no rule, stops
   the deploy);
2. **downloads that commit's tree from the git server**: the caller sends only the tag name, so a CI job
   (or a leaked deployer token) cannot make the daemon apply anything but a tagged commit;
3. plans it, refusing a blocked plan, and records the plan as approved by the tag and used by the job;
4. applies it. A plan with nothing to change ends the job without applying.

If the commit's `fleet.yml` pins another daemon version (`daemon:`), the job upgrades the daemon to it
first and the deploy resumes on the new binary (see [Upgrading the daemon](#upgrading-the-daemon-from-ci)).

The protected tag is the approval, in place of an admin approving a plan in the UI (which still works
for one-off applies). The daemon reads the repository with a token synced as `NATIVE_OPS_GIT_TOKEN`:
it needs to read the repository and its tag protections, which on Gitea means a user who is an admin of
the repository, with a token scoped to read the repository only.

Plans close themselves: one a deploy used shows **used** (with its job); one made before the host last
changed (an apply, a deploy, an instance change, an edge apply, a restore or an image build since) shows
**stale** and cannot be approved; on a daemon without apply, the Plans page says plans are for review
and shows **review only** instead of pending.

## Tenant instances (for a fleet manager)

Static services (gitea, plane, ...) are declared in git and applied. **Tenants** (one instance per customer,
created and removed while the product runs) are managed by a system that knows about customers, through
this API, without holding a key to the host. Off unless the daemon is started with `--enable-instances`.

```bash
# a token that can only ever manage demo-*/cust-* instances, from one image family, on one zone
native-ops remote token-create --name fleet-manager --role deployer \
    --names 'demo-*,cust-*' --images 'acme-app:*' --domains '*.acme.example'

curl -X PUT "$URL/v1/instances/demo-one" -H "Authorization: Bearer $TOKEN" -d '{
  "template": "app", "image": "acme-app:latest",
  "limits": {"limits.cpu": "1", "limits.memory": "1GB"},
  "volumes": [{"name": "demo-one-data", "path": "/app/.data", "owner": "app"}],
  "service": "app", "env": {"APP_MODE": "demo", "PORT": "8787"},
  "health": {"path": "/health", "port": 8787},
  "domain": "demo-one.acme.example", "route_directives": ["import strip-forged-identity"]
}'                                          # 202 and a job; poll GET /v1/jobs/{id}
```

`PUT` makes the instance exist as described: it creates it (volume, environment file at
`/etc/default/<service>`, the unit started once the environment is there, health gate, route), or resumes and
converges the one an earlier call created, so it is safe to repeat. `update` moves it to another image through
the safe path (volume snapshot first, environment carried over, health-gated, rolled back on failure).
`update` (`{image, service, health?, env?}`) may also set keys in the carried-over environment file -- `env: {"RELEASE_TAG": "v1.2.3"}` for a service that reports the release it runs -- keeping every other key; a rollback restores the file as it was. `resize` is a live `limits.cpu`/`limits.memory` change (no restart, no image change). `suspend` (`{domain,
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

## Image builds and recipe approval

`POST /v1/images/build?app=<app>&ref=<ref>` builds `<prefix><app>:<ref>` from the uploaded tree's
recipe: it runs `scripts/build-image.sh <app> <ref>`, which builds in a throwaway container and
publishes the image. Off unless the daemon has `--enable-image-build`; `--image-prefix` is what the
recipe names images before `<app>` (default `app-`, the starter recipe's default), and a scoped
token's `--images` globs are checked against that name. The daemon runs the recipe with
`NATIVE_OPS_IMAGE_PREFIX` set to it, so a recipe that reads it always agrees; one that names images
itself must be matched by setting `--image-prefix` (or `NATIVE_OPS_IMAGE_PREFIX` in
`/etc/native-ops/serve.env`) to what it produces.

That script runs **on the host, as the daemon's user**, so whoever writes it can run anything there.
The daemon therefore builds only from a recipe an admin has approved: the digest of every file under
`scripts/` and `images/` in the upload. An unapproved recipe is refused with
`403 recipe_not_approved` and the digest; an admin approves it once, on the UI's Recipes page or:

```bash
native-ops image recipe-digest --config-dir .                # the digest of a checkout
native-ops remote recipe-approve --config-dir .              # admin token: approve that digest
```

The approval is per recipe, not per build: a release pipeline building a new ref from an unchanged
recipe needs nothing, while any change to a build script waits for an admin -- the same rule an apply
follows for its plan. **After upgrading a daemon to a version with this check, approve the current
recipe once**, or the next build is refused.

### Image retention

Each build moves `<prefix><app>:<ref>` (and `:latest`) to the image it publishes. Rebuilding a ref
would leave the previous image with no alias and nothing to delete it, and every release tag keeps an
image of its own; a host that builds on every merge would fill its disk. So the daemon keeps them in
check, deleting only images that no instance runs (`volatile.base_image`):

- **After every build**, the image the build displaced is deleted if it now has no alias and no
  instance runs it, and that app's tagged images beyond the newest `--image-keep` (default 5) go too.
  `:latest` is always kept.
- **`POST /v1/images/prune`** (deployer; not a scoped token, since it acts on every app) does the same
  for every app, and also deletes images with no alias older than `--image-orphan-age` (default 24h),
  such as those left before this existed. `?dry_run=1` only reports. From CI:
  `native-ops remote image-prune [--dry-run]`, nightly in `ci-examples/maintenance.yml`.

Images whose aliases are not all `<prefix><app>:<ref>` (a base image, one named by hand) and cached
copies of remote images are never touched. A deleted tag can be built again from its git ref.
`--image-keep 0` keeps every tag.

## Tokens and bootstrapping

Tokens are managed over the API by an admin, so nobody needs a shell on the host:

```bash
export NATIVE_OPS_TOKEN=$BOOTSTRAP_TOKEN    # or any admin token
native-ops remote token-create --name ci-plan --role planner          # prints the secret, once
native-ops remote token-create --name ci-release --role deployer --names 'app-*' --images 'acme-app:*'
curl -H "Authorization: Bearer $NATIVE_OPS_TOKEN" "$NATIVE_OPS_URL/v1/tokens"     # list
curl -X DELETE -H "Authorization: Bearer $NATIVE_OPS_TOKEN" "$NATIVE_OPS_URL/v1/tokens/<id>"
```

An admin signed in to the UI can do the same on its Tokens page.

The first admin credential is provisioned with the host, as a secret CI already holds:

- `native-ops host create ... --daemon-version vX.Y.Z --daemon-sha256 <sha>` with
  `NATIVE_OPS_BOOTSTRAP_TOKEN=nops_...` in the environment writes **only the token's SHA-256** into
  the host's cloud-init (`NATIVE_OPS_BOOTSTRAP_TOKEN_SHA256` in `/etc/native-ops/serve.env`). User-data
  can be read back from the provider's metadata service by anything on the host that reaches it,
  containers included, so the token itself never goes there.
- `native-ops reconcile` does the same for a host it creates when `fleet.yml` has a `daemon:` section
  (`version`, `sha256`, optional `arch` and `flags`).
- With your own IaC, set `NATIVE_OPS_BOOTSTRAP_TOKEN=nops_...` (at least 32 characters after the
  prefix), or its hex SHA-256 as `NATIVE_OPS_BOOTSTRAP_TOKEN_SHA256`, in `/etc/native-ops/serve.env`.

The daemon loads it as an in-memory `admin` token named `bootstrap`; it is never written to disk.
Rotate it by changing the secret and restarting. On the host itself, `native-ops token create|list|revoke
--state-dir /var/lib/native-ops` works on the same file, and a running daemon notices the change.

## Backups, retention and DNS from CI

The recurring work is a daemon job that a **scheduled pipeline** starts; the pipeline's schedule is
the schedule, so the daemon needs none of its own.

```bash
native-ops remote backup --prune                 # every volume fleet.yml's backup section allows
native-ops remote backup --volume gitea-data     # one volume
native-ops remote dns-sync                        # fleet.yml's dns_records (create or update only)
native-ops remote restore --volume gitea-data --as gitea-drill    # admin: a restore drill
```

`fleet.yml` in the upload says what to do; the credentials stay with the daemon, in its secret store
(see the next section): the object store's keys (`BACKUP_S3_ACCESS_KEY`/`BACKUP_S3_SECRET_KEY`, or the
names `fleet.yml` gives) and the DNS provider's (`DO_API_TOKEN`). The upload cannot redirect them:

- a backup goes only to the destination pinned on the daemon (`NATIVE_OPS_BACKUP_ENDPOINT` and
  `NATIVE_OPS_BACKUP_BUCKET`); an upload naming another one is refused, or a deployer token could send
  the host's volumes to storage of its own;
- a DNS sync touches only the zones the daemon was given (`--dns-domains`, or `NATIVE_OPS_DNS_ZONES`):
  the provider's token reaches every zone in the account;
- DNS is synced only with a built-in provider: a script plugin from an upload would let a deployer token
  run code on the host without an approved plan (run `native-ops dns sync` on the host for a plugin). An in-place restore stops the
containers that mount the volume (`force=1`), copies the current volume aside to
`<volume>-pre-restore-<time>` first, and always starts the containers again.

## The daemon's own credentials (synced from the git server)

The daemon needs a few credentials of its own: the DNS provider's token, the object store's keys, the
OIDC client secret, and the values that pin what an upload may do with them. Nobody logs in to put them
on the host. People enter them in the **git server's secret store** (repository or environment
secrets), and a CI workflow pushes them to the daemon:

```yaml
# a workflow job, in a protected environment (an admin token is needed)
- env:
    NATIVE_OPS_TOKEN: ${{ secrets.NATIVE_OPS_BOOTSTRAP_TOKEN }}
    DO_API_TOKEN: ${{ secrets.DO_API_TOKEN }}
    BACKUP_S3_ACCESS_KEY: ${{ secrets.BACKUP_S3_ACCESS_KEY }}
    BACKUP_S3_SECRET_KEY: ${{ secrets.BACKUP_S3_SECRET_KEY }}
    NATIVE_OPS_BACKUP_ENDPOINT: ${{ vars.BACKUP_ENDPOINT }}
    NATIVE_OPS_BACKUP_BUCKET: ${{ vars.BACKUP_BUCKET }}
    NATIVE_OPS_DNS_ZONES: ${{ vars.DNS_ZONES }}
  run: native-ops remote secret-sync --prune DO_API_TOKEN BACKUP_S3_ACCESS_KEY BACKUP_S3_SECRET_KEY NATIVE_OPS_BACKUP_ENDPOINT NATIVE_OPS_BACKUP_BUCKET NATIVE_OPS_DNS_ZONES
```

- `PUT /v1/secrets` (admin) stores them in `secrets.json` in the state directory (0600); `GET` lists the
  names, who set each and when, never a value; `DELETE /v1/secrets/{name}` removes one. The UI's Tokens
  page shows the same list.
- A name in the store wins over the daemon's environment, so a host that already has a value in
  `/etc/native-ops/serve.env` keeps working, and a synced value replaces it.
- Values are read when needed -- a backup or DNS job, the status page's DNS listing, each OIDC sign-in --
  so a sync takes effect at once, without a restart. OIDC can be turned on (`--oidc-issuer`,
  `--oidc-client-id`, `--oidc-redirect-url`) before `NATIVE_OPS_OIDC_CLIENT_SECRET` is synced.
- Values never appear in an answer, a job, a log or the audit file; the audit line names what changed.
- `--prune` makes the git server the source of truth: a secret it no longer has is removed.

| Name | Used for |
|---|---|
| `DO_API_TOKEN` | DNS sync, and the DNS records on the status page |
| `BACKUP_S3_ACCESS_KEY`, `BACKUP_S3_SECRET_KEY` | backups and restores (or the names `fleet.yml`'s `backup:` gives) |
| `NATIVE_OPS_BACKUP_ENDPOINT`, `NATIVE_OPS_BACKUP_BUCKET` | the only backup destination an upload may use |
| `NATIVE_OPS_DNS_ZONES` | the zones a DNS sync may change (comma separated; with `--dns-domains`) |
| `NATIVE_OPS_OIDC_CLIENT_SECRET` | OIDC sign-in |
| `NATIVE_OPS_GIT_TOKEN` | reading the configuration repository and its tag protections, for deploys on a tag |

They are stored on the host, in `secrets.json`, because the daemon needs some of them at any time (a
person signing in, someone opening the status page). The protection is the host itself: the daemon's
user is in `incus-admin`, so whoever can read that file already controls every container and volume
there. What matters is how far each credential reaches **beyond** the host, so give each the least it
needs:

- **DNS token:** a scoped token that can only read and update domains, not create or destroy droplets
  (DigitalOcean custom scopes: `domain:read`, `domain:update`). An account-wide token on the host
  could take down every other server in the account.
- **Backup keys:** keys limited to the one backup bucket (per-bucket access keys), so a leaked pair
  cannot read or delete other backups.
- **OIDC client secret:** only usable with the redirect URL registered at the identity provider.

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
  first sight). With `--oidc-role none` nobody is created: only a user that already exists may sign in,
  so access is granted ahead of time, by an admin or by a directory such as a staff app, with
  `PUT /v1/users/by-email/{email} {"role": "deployer"}` (and taken away with `"disabled": true`).

Either way a sign-in sets an HttpOnly session cookie (HMAC-signed with a key in the state dir), and the
UI calls the API with it. API tokens keep working unchanged, for CI and machines.

- A session is checked against the user store on every request: disabling, removing or demoting a
  user takes effect at once, not when the cookie expires.
- After 5 failed sign-ins in a row for one username, further attempts for it are refused (`429`,
  `Retry-After`) for 30 seconds, doubling up to 15 minutes; a success clears it.
- The OIDC flow uses PKCE (S256). Without `--oidc-allowed-domain` the daemon warns at startup: with a
  public identity provider, anyone it vouches for may sign in.
- Admins can manage users over the API too (`/v1/users`), and cannot demote or disable the account
  they are signed in with.

Every auth/OIDC setting also has an environment form (`NATIVE_OPS_ENABLE_AUTH`, `NATIVE_OPS_OIDC_*`;
see `native-ops serve --help`). The OIDC client secret is best synced from the git server
(`NATIVE_OPS_OIDC_CLIENT_SECRET`, see above) rather than put in a unit file or `serve.env`.

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

A host created with `native-ops host create --daemon-version ...` or by `reconcile` with a `daemon:`
section in `fleet.yml` has all of this done by cloud-init. To install it yourself:
`deploy/systemd/native-ops-serve.service` is the unit (hardened; state in `/var/lib/native-ops`; env in
`/etc/native-ops/serve.env`). The user needs to exist and be in `incus-admin`:

```
useradd --system --home-dir /var/lib/native-ops --shell /usr/sbin/nologin -G incus-admin native-ops
```

Then route it through the edge, e.g. a Caddy site `native-ops.example.com { reverse_proxy 10.0.100.1:8686 }`
with the daemon bound to the bridge address, or `127.0.0.1` if Caddy runs on the host.

### Upgrading the daemon (from CI)

The daemon upgrades itself, so nobody logs in to the host for that either. The usual way is **the
`daemon:` pin in `fleet.yml`, deployed with a tag**:

```yaml
# fleet.yml
daemon:
  version: v1.58.0
  sha256: <the linux_amd64 tarball's line in the release's checksums.txt>
```

The same pin is what cloud-init installs on a host that `reconcile` creates. On a running daemon, a
deploy whose commit pins another version upgrades first: it installs the pinned release (steps 2 to 4
below), records the deploy in `<state-dir>/deploy-resume.json`, and restarts. Once the new binary's
upgrade has committed, it resumes the deploy as a new job, so the plan and the apply run on the version
the commit pins. If the new binary does not stay up, the guard puts the old one back, and that one
records the deploy as failed instead of running it. So a daemon upgrade is a reviewed change to
`fleet.yml` and a protected tag, like any other change, and no CI job needs an admin token for it.

- The deploy's job log says what it did; the resumed job is a second `deploy` job for the same tag.
- A daemon that cannot upgrade itself (its binary is not in `<state-dir>/bin`) refuses a deploy that
  pins another version, rather than deploy a commit written for a different release.
- A development build (`dev`, not a release tag) leaves the pin alone.
- A pin older than v1.58.0 is refused: those releases cannot resume the deploy. Move an older daemon
  with `remote daemon-upgrade` once.
- Without a `daemon:` section, deploys never change the daemon.

For a one-off upgrade outside a deploy, an admin can call the endpoint directly:

```bash
NATIVE_OPS_TOKEN=$ADMIN_TOKEN native-ops remote daemon-upgrade --version v1.56.0 --sha256 <from checksums.txt>
```

1. `POST /v1/daemon/upgrade` (admin) starts a job, taking the host lock like any change.
2. The job downloads `native-ops_<version>_linux_<arch>.tar.gz` from the releases, checks it against
   the pinned SHA-256, unpacks it and runs it once: it must report the version asked for. Any failure
   leaves everything as it was.
3. It keeps the current binary as `native-ops.prev`, swaps the new one in, and marks the upgrade
   pending. The daemon then exits with status 75 once running jobs have ended, and systemd starts the
   new binary (`Restart=on-failure`).
4. The unit's guard (`ExecStartPre`) counts the new binary's starts. One that has served for 30 seconds
   commits the upgrade; one that fails to start 3 times is replaced by `native-ops.prev` again, so a bad
   release puts the old daemon back by itself.
5. `remote daemon-upgrade` waits for the job, then for `/healthz` to report the new version, and fails
   if the old one comes back instead.

This needs the binary in the daemon's state directory (`/var/lib/native-ops/bin/native-ops`), which
the daemon's user can write, with the unit shipped in `deploy/systemd`: hosts made by
`host create --daemon-version` or `reconcile` are set up that way. To move an existing install over
(once, on the host):

```bash
install -d -o native-ops -g native-ops -m 0700 /var/lib/native-ops/bin
install -o native-ops -g native-ops -m 0755 /path/to/native-ops /var/lib/native-ops/bin/native-ops
# then use deploy/systemd/native-ops-serve.service's ExecStartPre (the guard), its ExecStart path,
# and StartLimitBurst=10; systemctl daemon-reload && systemctl restart native-ops-serve
```

A daemon started from anywhere else (a root-owned `/usr/local/bin`, say) does not serve the endpoint.

### The state directory

Everything the daemon keeps is in `--state-dir` (default `/var/lib/native-ops`, 0700):

| File | What |
|---|---|
| `tokens.json` | API tokens (SHA-256 of each secret, never the secret) |
| `users.json` | local and OIDC users (bcrypt hashes for local passwords) |
| `secrets.json` | the daemon's own credentials, synced from the git server |
| `session.key` | the key that signs session cookies |
| `plan.key` | the key that binds a plan's hash to values the plan does not print |
| `plans/` | every plan made, its approval and the apply that used it |
| `jobs/` | every job's record and log (the last 200) |
| `recipes.json` | image recipes asked for, and which an admin approved |
| `audit.log` | one JSON line per request and per finished job (never a value or credential) |
| `bin/` | the daemon's binary, the previous one, and the upgrade markers (see above) |
| `deploy-resume.json` | a deploy that upgraded the daemon to `fleet.yml`'s pin, until the new binary resumes it |

All files are 0600. Back the directory up if losing approvals, job history or users matters to you;
tokens and secrets can be re-created from the git server.

### Configuration reference (`native-ops serve`)

Every flag; most also read an environment variable, so they can live in `serve.env` or in `fleet.yml`'s
`daemon.flags`.

| Flag | Env | Default | What |
|---|---|---|---|
| `--addr` | | `127.0.0.1:8686` | listen address; put TLS (the edge) in front of it |
| `--state-dir` | | `/var/lib/native-ops` | where everything above is kept |
| `--pool` | | `default` | the Incus storage pool holding the data volumes |
| `--enable-apply` | | off | `POST /v1/apply` |
| `--approval-ttl` | | `1h` | how long an admin's approval of a plan lasts |
| `--enable-instances` | | off | the tenant-instance endpoints |
| `--instance-profiles` | | `base,service` | Incus profiles a tenant instance spec may use |
| `--instance-route-imports` | | none | Caddy snippets a tenant route may import |
| `--enable-image-build` | | off | `POST /v1/images/build` and the recipe endpoints |
| `--image-prefix` | `NATIVE_OPS_IMAGE_PREFIX` | `app-` | what the recipe names images before `<app>:<ref>` |
| `--image-keep` | `NATIVE_OPS_IMAGE_KEEP` | `5` | tagged images kept per app by image retention (0: every tag) |
| `--image-orphan-age` | | `24h` | how old an unaliased image no instance runs must be before a prune deletes it |
| `--enable-edge-apply` | | off | `POST /v1/edge/apply` |
| `--git-url` | `NATIVE_OPS_GIT_URL` | none | the git server (Gitea) holding the configuration repository; with `--deploy-repo` and `--enable-apply`, `POST /v1/deploy` |
| `--deploy-repo` | `NATIVE_OPS_DEPLOY_REPO` | none | the configuration repository there, as `owner/name` |
| `--deploy-tags` | `NATIVE_OPS_DEPLOY_TAGS` | `deploy-*` | the tags that deploy; the repository must protect them |
| `--enable-backup` | | off | `POST /v1/backups` and `/v1/backups/restore` |
| `--enable-dns-sync` | | off | `POST /v1/dns/sync` |
| `--edge-container` | `NATIVE_OPS_EDGE_CONTAINER` | `edge` | the container whose routes and certificates the status shows |
| `--dns-provider` | `NATIVE_OPS_DNS_PROVIDER` | none | `digitalocean`: DNS records on the status page |
| `--dns-domains` | `NATIVE_OPS_DNS_DOMAINS` | none | zones shown on the status page, and that a DNS sync may change |
| `--enable-auth` | `NATIVE_OPS_ENABLE_AUTH=1` | off | local sign-in for the UI |
| `--oidc-issuer` | `NATIVE_OPS_OIDC_ISSUER` | none | turns on OIDC sign-in |
| `--oidc-client-id` | `NATIVE_OPS_OIDC_CLIENT_ID` | | |
| `--oidc-client-secret` | `NATIVE_OPS_OIDC_CLIENT_SECRET` | | better synced to the secret store |
| `--oidc-redirect-url` | `NATIVE_OPS_OIDC_REDIRECT_URL` | | `https://<daemon>/auth/oidc/callback` |
| `--oidc-allowed-domain` | `NATIVE_OPS_OIDC_ALLOWED_DOMAIN` | any | only emails at this domain may sign in |
| `--oidc-role` | `NATIVE_OPS_OIDC_ROLE` | `viewer` | the role a newly seen OIDC user gets |
| `--oidc-label` | `NATIVE_OPS_OIDC_LABEL` | `single sign-on` | the sign-in button's text |
| `--mcp-oauth` | `NATIVE_OPS_MCP_OAUTH` | on (with sign-in) | MCP clients connect by signing their person in (OAuth), acting with that person's role; `false` turns it off ([api.md](api.md#connect-by-signing-in-oauth)) |
| `--public-url` | `NATIVE_OPS_PUBLIC_URL` | the origin of `--oidc-redirect-url` | the daemon's public origin, named in the OAuth metadata (else each request's) |

Environment only: `NATIVE_OPS_BOOTSTRAP_TOKEN` or `NATIVE_OPS_BOOTSTRAP_TOKEN_SHA256` (the first admin
token). Read through the secret store, then the environment: the names in the credentials table above.

## SSH, for the paths that still use it

The daemon itself needs no SSH. `host create`/`reconcile` (bringing a new host up) and
`instance migrate` do. Set `NATIVE_OPS_SSH_KNOWN_HOSTS=<file>` wherever they run: a host's key is then
checked against the file, a changed key is refused, and a host seen for the first time is recorded (or
refused too, with `NATIVE_OPS_SSH_STRICT=1`). Unset, host keys are not checked and a warning says so.

`reconcile` provisions hosts, syncs DNS and prepares Incus; it deploys services only with
`--deploy-services`, a break-glass path that skips the plan approval, the host lock and the job
record. Normally services reach a host through an approved apply.

## Roadmap

Multi-host targeting (services mapped to hosts in `fleet.yml`, and `native-ops remote` fanning a plan
or apply out to each host's daemon); instance migrate and previews as jobs; a live view of a running
job; and per-user API tokens. The API is versioned (`/v1`) so a separate fleet manager can depend on it.
