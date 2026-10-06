# Deploying through the API (and MCP)

This is the guide for driving a native-ops host from **CI, a script, a dashboard or an AI agent**:
previewing and running deploys, seeing what is live, and following jobs. For running the daemon
itself (flags, tokens, the state directory, upgrades) see [daemon.md](daemon.md).

- **Reference:** every endpoint, parameter and answer is in the OpenAPI 3.1 document the daemon serves
  at `GET /openapi.json` (and `/openapi.yaml`), with no token, and drawn as a readable page at `GET /docs`
  (also no token, nothing loaded from elsewhere). Load it into any OpenAPI viewer or client
  generator. Its source is [`pkg/server/openapi.yaml`](../pkg/server/openapi.yaml); a test fails the
  build when it and the daemon's routes disagree.
- **Agents:** the same operations are MCP tools at `POST /mcp`; an agent connects by having its person
  sign in (OAuth), or with an API token. See [MCP](#mcp).
- **CLI:** `native-ops remote deploy | deploy-plan | deploys | wait` wraps all of this for CI.

## Contents

- [The model](#the-model)
- [Authentication and roles](#authentication-and-roles)
- [Deploy recipes](#deploy-recipes): preview, deploy, what is live, follow a job, roll back
- [What a deploy does](#what-a-deploy-does)
- [When something goes wrong](#when-something-goes-wrong)
- [Jobs](#jobs)
- [MCP](#mcp)
- [Errors](#errors)

## The model

The configuration repository (in git) says what the host should run. **A deploy makes the host match
one commit of it.**

1. A change is reviewed and merged in the configuration repository.
2. Someone allowed to pushes a **deploy tag** (`deploy-2026.10.03`, or whatever `--deploy-tags`
   matches) at that commit. The tag must be **protected** on the git server: the push is the approval.
3. CI (or a person, or an agent) calls `POST /v1/deploy {"tag": "deploy-2026.10.03"}`. The daemon reads
   that commit from the git server itself, so the caller uploads nothing and can deploy nothing but a
   tagged commit.
4. The deploy runs as a **job**. Every change to the host is a job, with a record kept on disk, and
   **one change runs at a time**.

Nothing about a deploy needs SSH, and nothing on the host needs to be touched by hand.

## Authentication and roles

Every `/v1` call takes an API token: `Authorization: Bearer nops_...`. An admin creates tokens
(`POST /v1/tokens`, or `native-ops token create` on the host); the secret is shown once.

| Role | Can |
|---|---|
| `viewer` | read: status, deploys, jobs (no logs), plans, instances |
| `planner` | + preview: `POST /v1/deploy/plan`, `POST /v1/plan` |
| `deployer` | + change: deploy, apply an approved plan, image builds and pruning, edge, backups, DNS; job logs |
| `admin` | + approvals, tokens, users, secrets, restores, daemon upgrades |

Roles are ordered: each can do everything the ones above it can. The least role each operation needs is
`x-native-ops-role` in the OpenAPI document. Give each caller the least it needs: a pull-request job
gets a planner token, the tag pipeline a deployer token, a dashboard a viewer token.

A token can also carry a **scope** (instance names, images, domains). A scoped token, the kind a fleet
manager uses, can only manage tenant instances within it, and sees only its own jobs.

The examples below use:

```bash
export NATIVE_OPS_URL=https://native-ops.example.com
export NATIVE_OPS_TOKEN=nops_...
api() { curl -fsS -H "Authorization: Bearer $NATIVE_OPS_TOKEN" -H 'Content-Type: application/json' "$@"; }
```

## Deploy recipes

### Preview a tag: what would it change?

```bash
api -X POST "$NATIVE_OPS_URL/v1/deploy/plan" -d '{"tag":"deploy-2026.10.03"}' | jq -r .text
# or
native-ops remote deploy-plan --tag deploy-2026.10.03
```

The daemon resolves the tag, reads its commit from the git server, and plans it against the host
**now**, without changing anything and without starting a job (planner). The answer:

```json
{
  "tag": "deploy-2026.10.03",
  "sha": "0f6f9ab...",
  "deployable": true,
  "protected_by": "deploy-*",
  "daemon": { "running": "v1.60.0", "pinned": "v1.60.0", "upgrade": false },
  "hash": "9c1e...",
  "exit": 2,
  "counts": { "none": 16, "update": 1 },
  "plan": { "services": [ ... ] },
  "text": "home: update\n  ~ home: image opsavor-home:latest ... through the safe update path ...\n..."
}
```

- `exit`: `0` nothing to change, `1` **blocked** (the deploy would fail), `2` changes pending.
- `deployable: false` (with `reason`) means `POST /v1/deploy` would refuse the tag, usually because no
  protection rule covers it. The plan is still made, so a tag can be checked before it is protected.
- `daemon.upgrade: true` means the commit's `fleet.yml` pins another daemon release: the deploy will
  upgrade first, and the plan that is applied is made by the pinned release.

The CLI exits 1 for a blocked plan and 2 for a tag that would not deploy, so a pipeline can gate on it.

### Deploy a tag and wait

```bash
job=$(api -X POST "$NATIVE_OPS_URL/v1/deploy" -d '{"tag":"deploy-2026.10.03"}' | jq -r .job.id)
api "$NATIVE_OPS_URL/v1/jobs/$job?wait=60" | jq '{status, result, error}'
# or, all of it, exiting non-zero on failure:
native-ops remote deploy --tag deploy-2026.10.03
```

`202 Accepted` with the job (and `Location: /v1/jobs/<id>`). `409 busy` means another change is
running: the message names its job; wait for it and try again.

### What is live, and what ran before

```bash
api "$NATIVE_OPS_URL/v1/deploys?limit=10" | jq '{current: .current.tag, running: .running.tag}'
native-ops remote deploys
```

```json
{
  "current": { "job": "j-1791054271-18f384d8", "tag": "deploy-2026.10.03.4", "sha": "0f6f9ab...",
               "status": "succeeded", "result": "applied", "actor": "ci-deploy",
               "started": "2026-10-03T14:41:02Z", "finished": "2026-10-03T14:42:30Z",
               "plan_hash": "9c1e...", "counts": { "none": 16, "update": 1 } },
  "running": null,
  "deploys": [ ... newest first ... ]
}
```

`current` is the newest deploy that left the host matching its tag (`result` `applied` or
`no_changes`). Changes made since by other means (an apply, a tenant instance change, an image build)
are not deploys; they are in [`GET /v1/jobs`](#jobs). To check whether the host still matches the
current tag, preview it again: a plan with `exit: 0` means it does.

### Follow a job

```bash
api "$NATIVE_OPS_URL/v1/jobs/$job?wait=60"
```

With `wait` (0 to 60 seconds), the answer comes as soon as the job ends, or when the wait is over with
the job still `running`, so there is no need to poll quickly. Call it again until the status is
`succeeded`, `failed` or `interrupted`. Deployers and admins also get the job's `log`.

### Roll back

A rollback is a deploy of an earlier commit. Push a new tag at it and deploy that:

```bash
git tag deploy-2026.10.03.5 <earlier-commit> && git push origin deploy-2026.10.03.5
native-ops remote deploy-plan --tag deploy-2026.10.03.5     # see what going back changes
native-ops remote deploy --tag deploy-2026.10.03.5
```

`GET /v1/deploys` gives the commit of every earlier deploy. A service whose image moved (a
`replace-image` change) goes back through the same safe path: volume snapshot, environment carried
over, health gate, automatic rollback on failure.

## What a deploy does

`POST /v1/deploy` starts a job of kind `deploy` that:

1. **Resolves the tag** on the git server and checks that a tag protection rule covers it. No rule (or
   one the daemon cannot read) fails the job before anything is fetched.
2. **Downloads the commit's tree** from the git server and records the commit on the job (`sha`).
3. **Follows the daemon pin.** If `fleet.yml`'s `daemon: {version, sha256}` names another release, the
   job installs it (checksum-verified), ends with `result: daemon_upgraded`, and the daemon restarts.
   The new binary resumes the deploy as a **new** `deploy` job for the same tag. Look for it in
   `GET /v1/deploys`.
4. **Plans** the commit and records the plan as approved by the tag (`plan_hash` on the job).
   A **blocked** plan fails the job and nothing changes. **Nothing to change** ends it with
   `result: no_changes`.
5. **Applies** the plan: `result: applied`.

| `status` | `result` | Meaning |
|---|---|---|
| `running` | | in progress |
| `succeeded` | `applied` | the host now matches the tag |
| `succeeded` | `no_changes` | the host already matched the tag |
| `succeeded` | `daemon_upgraded` | the daemon moved to the pin; the deploy continues as a new job |
| `failed` | | see `error` (and the log); what was applied before the failure stays applied |
| `interrupted` | | the daemon stopped mid-job; deploy the same tag again (applies converge) |

## When something goes wrong

| You see | It means | Do |
|---|---|---|
| `409 busy` | another change is running (named in the message) | wait for that job (`GET /v1/jobs/{id}?wait=60`), then retry |
| job failed: `tag ... is not protected` | no protection rule covers the tag | protect the deploy tags in the repository's settings, then push the tag again |
| job failed: `the plan is blocked` | apply would fail (e.g. an image that does not exist yet) | `deploy-plan` shows which service and why; fix the configuration or build the image, tag again |
| `422 tag_unresolved` | the git server has no such tag, or it does not match the deploy tag pattern | check the tag was pushed, and its name |
| `502 git_unavailable` | the daemon could not read the commit | check the git server, and the daemon's `NATIVE_OPS_GIT_TOKEN` |
| `daemon_upgraded`, then a failed deploy saying the new binary "did not stay up" | the pinned release did not start and the old one was put back | see `journalctl -u native-ops-serve` on the host; fix or lower the pin, tag again |
| `interrupted` | the daemon restarted mid-job | deploy the same tag again |

## Jobs

`GET /v1/jobs?kind=&status=&limit=` lists jobs newest first, without logs. `kind` is one kind or a family
ending in a colon:

| Kind | Started by |
|---|---|
| `deploy` | `POST /v1/deploy` (and a deploy resumed after a daemon upgrade) |
| `apply` | `POST /v1/apply` (an approved plan of an uploaded tree) |
| `instance:put`, `instance:update`, `instance:resize`, `instance:suspend`, `instance:delete` | the tenant instance endpoints |
| `image:build`, `image:prune` | image builds and retention |
| `edge:apply`, `backup`, `restore`, `dns:sync` | maintenance endpoints |
| `daemon:upgrade` | `POST /v1/daemon/upgrade` |

`status` is `running`, `succeeded`, `failed` or `interrupted`. The newest 200 jobs are kept.

## MCP

`POST /mcp` is a [Model Context Protocol](https://modelcontextprotocol.io) server, so an agent can read
the host, preview and run deploys, and follow jobs, with an API token, the way CI does.

**It adds no power of its own.** Each tool is a call to the endpoints above, made with the caller's
token, through the same handler and the same audit log (as the endpoint it called). A tool can do
exactly what the token could do with curl, and `tools/list` shows only the tools the token's role and
scope can use (and that the daemon has enabled).

### Connect by signing in (OAuth)

With sign-in on (`--oidc-issuer` or `--enable-auth`), a person connects an agent by signing in. Nobody
creates, copies or pastes a token:

```bash
claude mcp add --transport http native-ops https://native-ops.example.com/mcp
```

The first call gets a `401` whose `WWW-Authenticate` names the daemon's OAuth metadata. The client
registers itself, opens a browser at the daemon's sign-in (the same as the UI's: your identity provider,
or a local account), shows a consent page naming the client and your role, and receives tokens. In
Claude Code, `/mcp` shows the server and runs the sign-in.

What the agent gets:

- **Your access, live.** It acts as you, with your role as the user store holds it **at each request**.
  If a directory (crew, `PUT /v1/users/by-email`) changes your role or disables you, your agents follow
  at once. The audit log and job records name you, `alice (via Claude Code)`.
- **`/mcp` only.** The tokens are not accepted anywhere else in the API, and the MCP tools never include
  approvals, tokens, users or secrets, whatever your role.
- **Short-lived tokens.** An access token lasts an hour; the client refreshes it. Refresh tokens last 30
  days and rotate on every use; if a spent one is ever presented again (a copy exists), the grant is
  revoked.
- **Revocable.** Admins see every grant (who, which client, when last used) on the UI's **Tokens** page
  or at `GET /v1/oauth/grants`, and revoke one with `DELETE /v1/oauth/grants/{id}`. A client can end its
  own grant at `POST /oauth/revoke`.

The flow is the MCP authorization spec's: protected resource metadata (RFC 9728), authorization server
metadata (RFC 8414), dynamic client registration (RFC 7591, public clients only), the authorization code
flow with PKCE S256 (required), resource indicators (RFC 8707), refresh tokens and revocation (RFC 7009).
Redirect URIs must be `https`, or `http` to `127.0.0.1`, `::1` or `localhost`. The daemon names its
public origin from `--public-url`, else from `--oidc-redirect-url`'s origin, else from each request.
Turn the whole thing off with `--mcp-oauth=false`.

### Connect with an API token

For an agent with no browser (a scheduled job, a server-side bot), use an API token instead:

```bash
claude mcp add --transport http native-ops "$NATIVE_OPS_URL/mcp" \
  --header "Authorization: Bearer $NATIVE_OPS_TOKEN"
```

Any MCP client that speaks Streamable HTTP works the same way: the URL is `<daemon>/mcp`, and the
header is the bearer token. The server is stateless and answers in JSON (no event stream, no sessions).
It takes a bearer token only, never the UI's session cookie, and refuses requests from a browser page of
another origin.

**Which token:** a **planner** token lets an agent look at everything and preview deploys, but not change
the host. That is the right default. Give it a **deployer** token only when it should run deploys itself.
Either way the agent can only deploy tags that someone already pushed and protected in git.

### Tools

| Tool | Least role | What it does |
|---|---|---|
| `whoami` | viewer (scoped too) | the token's name, role, scope, and the daemon version |
| `host_status` | viewer | instances, volumes, images, metrics, warnings, edge routes, certificates, DNS |
| `list_deploys` | viewer | what the host runs (`current`), the deploy in progress, the history |
| `plan_deploy` | planner | what deploying a tag would change, changing nothing |
| `deploy` | deployer | deploy a tag as a job; `wait_seconds` (≤ 60) waits for it |
| `list_jobs` | viewer (scoped too) | jobs, filtered by `kind`, `status`, `limit` |
| `get_job` | viewer (scoped too) | one job; `wait_seconds` waits for it to end; the log for deployers |
| `list_plans`, `get_plan` | viewer | plans and where each stands |
| `list_instances`, `get_instance` | viewer (scoped too) | tenant instances the token may see |
| `prune_images` | deployer | image retention as a job; `dry_run` defaults to true |

**Deliberately not tools:** approving plans or recipes, tokens, users, secrets, restores and daemon
upgrades. Those are the human gates. A daemon upgrade still happens through a deploy, when a reviewed
change to `fleet.yml`'s pin is tagged.

Tool results carry the endpoint's JSON answer (as text and as `structuredContent`), plus a plan's
`text` or a job's `log` as plain text. An endpoint's refusal (`403`, `409 busy`, `422`) is a tool
result with `isError: true` that names the HTTP status and the endpoint's error, so the agent sees why.

### A typical agent session

> *"Ship the merged change."*

1. `list_deploys`: the host runs `deploy-2026.10.03.4`.
2. The person pushes `deploy-2026.10.03.5` in git (an agent with push rights may too, if the
   repository's tag protection allows that user).
3. `plan_deploy {tag: "deploy-2026.10.03.5"}`: one service, `home`, moves to a new image; `exit: 2`,
   `deployable: true`. The agent shows the plan.
4. `deploy {tag: "deploy-2026.10.03.5", wait_seconds: 60}`: the job comes back `succeeded`,
   `result: applied`.
5. `list_deploys`: `current` is `deploy-2026.10.03.5`.

### Protocol details

- Revisions `2025-06-18`, `2025-03-26` and `2024-11-05`. The newest is answered when the client asks for
  another.
- Methods: `initialize`, `ping`, `tools/list`, `tools/call`. Notifications get `202` with no body.
  Batches are refused (`-32600`).
- `GET` and `DELETE /mcp` answer `405`.
- A tool call that waits is bounded (60 s) to stay inside the daemon's write timeout. Follow a longer
  job with `get_job`.

## Errors

Every error is `{"error": "<for a person>", "code": "<for a program>"}`:

| Code | Status | Meaning |
|---|---|---|
| `bad_request` | 400 | the request is malformed; `error` says how |
| `bad_config` | 400 | the configuration tree does not load |
| `unauthorized` | 401 | no valid token (`WWW-Authenticate` says how) |
| `forbidden` | 403 | the token's role or scope cannot do that |
| `not_found` | 404 | no such endpoint, job, plan or instance (or the feature is off on this daemon) |
| `busy` | 409 (429 for plans) | another change, or too many plans, is running; retry |
| `tag_unresolved` | 422 | the tag does not resolve on the git server |
| `git_unavailable` | 502 | the commit could not be read from the git server |
| `host_unavailable` | 502 | the host could not be read (never echoes internal text) |
| `plan_changed`, `blocked`, `stale`, `not_approvable` | 409 | an apply or approval was refused for the plan's state |
| `not_approved`, `approval_used`, `approval_expired` | 403 | an apply needs a (fresh) admin approval |
| `not_managed` | 403 | a tenant-instance change to something that is not a tenant instance |
