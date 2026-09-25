# Git organization & repository best practices

How to lay out repositories and protect them so that **changes reach
production only through review**, and so that a credential leak or an
unreviewed workflow cannot quietly deploy. This is generic guidance for any
`native-ops` fleet; the [reference implementation](#reference-implementation-opsavor)
at the bottom is the Opsavor setup.

## Repository roles

Keep four kinds of repository, and never mix them:

| Repo | Contains | Never contains |
|------|----------|----------------|
| **engine** (`theta42/native-ops`) | the Go engine and generic scripts/images (`base`, `edge`), no application specifics | fleet config, app images, secrets |
| **config / IaC** (`<org>/native-ops-conf`) | `fleet.yml`, `services/`, `templates/`, per-app image recipes, deploy scripts | the engine source |
| **application** (one per app) | the app's source and its `release`/`preview` workflows | infra config |
| **control plane** (optional, `<org>/management`) | the fleet API/dashboard | the engine or the app |

The engine is generic; everything fleet-specific is declarative config; every
app owns its own workflows.

## The review gate: branch protection

On the **default branch** (`main`), and on every branch that can deploy:

- **Require a pull request** with **at least one approval**.
- **Disable direct pushes** and **force-pushes**.
- **Require a status check** — add a `pull_request` CI workflow (e.g. the test
  suite) and name that job as the required context.
- Prefer **"enforce admins"** so the approval rule cannot be bypassed.

A PR test workflow is what makes "required status checks" meaningful. Without
one, a required check is either unsatisfiable (blocks everything) or absent.

## The release gate: tag protection

Publish releases as **tags**, and protect the tag pattern:

- Protect e.g. `app-v*` (and `manager-v*`, …).
- Whitelist only the people allowed to cut a release.
- Production deploys run **only on tags** — never on a branch push.

Combined with branch protection, this means production is touched only by an
authorized person performing a deliberate act.

## Deploy lanes

Run two lanes, and give each instance a lane so a roll can never cross:

- **staging** — triggered by a merge to `main` (reviewed), rolls `class=staging`.
- **production** — triggered by a protected release tag, rolls `class=production`.

The lane is a property of the instance, not of the workflow, so a staging roll
cannot touch production and a release cannot touch staging.

## Secrets

- **Scope to the repository** that needs it — not the whole organization. A
  powerful org-wide secret is inherited by *every* repo, including any new one.
- **Least privilege.** The CI credential should be the narrowest thing that
  works: a cloud token scoped to DNS (not the whole account), an SSH user that
  can reach only what it deploys (not `incus-admin`).
- **Rotate on exposure**, and **never commit a credential** in a clone URL,
  config, or script.
- **Encoding gotcha (Gitea):** the secret API stores the value **verbatim** —
  set it **raw**. A base64 value is delivered to the runner as-is and fails
  authentication in a way that looks like an invalid token.

## Runner scoping

A runner registered **instance-wide** executes jobs for **any** repository on
the instance. Prefer a **repository- or organization-scoped** runner so only
trusted repositories can run on it — the runner is what receives the secrets.

## The trust-boundary principle (important)

Before Gitea **1.28**, there is no deployment-environment gate: repository and
organization secrets are injected into **any** workflow run in the repo —
including a `push` to an ordinary branch, not just a reviewed merge to `main`.
So branch protection alone does **not** stop an unreviewed workflow from using
the deploy secrets.

The effective boundary is therefore **"who can write/push workflows."** Treat
"can push a workflow to a deploy repo" as "can deploy." Restrict write access
to trusted reviewers, and scope the runner and secrets accordingly.

**On Gitea 1.28+**, use **deployment environments**: put the deploy secrets in
an environment (e.g. `production`) with **required reviewers** and a branch
rule, and set `environment: production` on the prod job. Secrets are then only
injected after approval, for the allowed branch — which is exactly "code
review + branch protection gates production."

## Reference implementation (Opsavor)

- **engine**: `theta42/native-ops` (public, github).
- **config**: `opsavor/native-ops-conf` (git.opsavor.work).
- **apps / control plane**: `opsavor/platform`, `opsavor/management`.
- `main` protected on all deploy repos: 1 approval, direct push disabled, and
  the `test` status check required.
- Release tags protected: `platform-v*`, `manager-v*`.
- Secrets **repo-scoped, none org-wide**: `MANAGER_TOKEN` (a scoped manager API
  token) on `platform`; `DO_API_TOKEN` and `SSH_PRIVATE_KEY` on
  `native-ops-conf`.
- Lanes: merge to `main` → staging (`class=staging`); tag `platform-v*` →
  production (`class=production`), both via the manager REST API.

## Checklist

- [ ] Default branch: PR required, ≥1 approval, no direct/force push.
- [ ] A PR CI workflow exists and is a required status check.
- [ ] Release tag pattern protected; only authorized users can tag.
- [ ] Production deploys run only on protected tags; staging only on reviewed `main`.
- [ ] Instances carry a lane; rolls target one lane.
- [ ] No powerful secrets at the organization level; secrets are repo-scoped.
- [ ] CI credentials are least-privilege; exposed ones rotated.
- [ ] The runner is scoped to trusted repositories.
- [ ] On Gitea ≥ 1.28: deploy secrets live in a reviewer-gated environment.
