# Example pipelines

The complete, maintained set lives in the starter config repository,
[theta42/native-ops-conf](https://github.com/theta42/native-ops-conf) (including Bootstrap and
Release an app); fork that to start. These are the minimal versions, to copy into an existing config
repository (`.github/workflows/` or `.gitea/workflows/`) and adjust the
URL and the pinned version. Each one installs the pinned, checksum-verified `native-ops` binary and
talks to the daemon with `native-ops remote`; none of them holds an SSH key or a cloud credential.

| File | When |
|---|---|
| `plan.yml` | every pull request and push to main: validate, plan, comment the plan |
| `deploy.yml` | a pushed `deploy-*` tag: deploy that commit (the daemon fetches it from the git server) |
| `release.yml` | dispatched by an app repository: build its image on the host (it waits in line if the host is busy) |
| `secrets.yml` | manual: copy service secrets (`SERVICE_*`) from the repository's secret store to the daemon |
| `maintenance.yml` | nightly: back up volumes with retention, prune images |
| `apply.yml` | manual: apply a plan an admin approved (the break-glass path; deploy tags are the normal one) |
| `daemon-upgrade.yml` | manual, one-off: move the daemon to a release. Usually not needed: bump `fleet.yml`'s `daemon:` pin and push a deploy tag (docs/daemon.md). Needs an admin token, so it runs in a protected environment |

Every workflow but the last uses **one secret**, `NATIVE_OPS_TOKEN`: a `deployer` token, created once by
an admin (the daemon's Tokens page, or with the bootstrap token, no shell on the host needed):

```bash
NATIVE_OPS_TOKEN=$BOOTSTRAP native-ops remote token-create --name ci --role deployer
```

One token per repository is not a shortcut. A git server gives a repository's secrets to any workflow
run in it, so tokens split per workflow are all readable by whoever can push a workflow there. The
limits that matter are on the daemon: a deployer deploys only a commit a protected tag points at, builds
only an approved recipe, applies only a plan an admin approved, and sets only service secrets.

Install the tool **outside the checkout** (the examples `cd "$(mktemp -d)"` first): `plan`, `apply`,
`backup` and `image-build` upload the checked-out tree, and the binary and its tarball would go with it.
