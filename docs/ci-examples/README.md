# Example pipelines

The complete, maintained set lives in the starter config repository,
[theta42/native-ops-conf](https://github.com/theta42/native-ops-conf) (including Bootstrap and
Release an app); fork that to start. These are the minimal versions, to copy into an existing config
repository (`.github/workflows/` or `.gitea/workflows/`) and adjust the
URL and the pinned version. Each one installs the pinned, checksum-verified `native-ops` binary and
talks to the daemon with `native-ops remote`; none of them holds an SSH key or a cloud credential.

| File | When | Token (secret) |
|---|---|---|
| `plan.yml` | every pull request and push to main: validate, plan, comment the plan | `planner` |
| `apply.yml` | manual (or on merge): apply the plan an admin approved | `deployer` |
| `edge.yml` | a change to `edge/`: apply the edge Caddyfile | `deployer` |
| `maintenance.yml` | nightly: back up volumes with retention, sync DNS | `deployer` |
| `deploy.yml` | a pushed `deploy-*` tag: deploy that commit (the daemon fetches it) | `deployer` |
| `daemon-upgrade.yml` | manual: move the daemon to a pinned release | admin (protected environment) |

Create the tokens once with the bootstrap admin token (no shell on the host needed):

```bash
NATIVE_OPS_TOKEN=$BOOTSTRAP native-ops remote token-create --name ci-plan --role planner
NATIVE_OPS_TOKEN=$BOOTSTRAP native-ops remote token-create --name ci-deploy --role deployer
```

A `planner` token can plan and nothing else, so a pull request that rewrites a workflow to try an
apply gets nowhere. A `deployer` token can apply only a plan an admin approved (UI, or
`POST /v1/plans/<hash>/approve`), once, within the approval's lifetime.
