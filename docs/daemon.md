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
| `GET /v1/status` | viewer | instances, data volumes, images and warnings for the host |
| `GET /` | none | the UI (Overview, Instances, Volumes). It holds no data; it asks for a token and calls `/v1/status` |

`native-ops status [--json]` prints the same snapshot from the CLI. It reports **key names only**
for instance config (OCI containers keep their secrets in `environment.*`); values are never read
into the output, other than resource limits and `user.native-ops.*` bookkeeping.

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
- Roles: `viewer` < `deployer` < `admin`. Nothing needs more than `viewer` yet; the gate is tested
  so the write endpoints that follow inherit it.
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

## Tokens and bootstrapping

```
native-ops token create --state-dir /var/lib/native-ops --name ci --role deployer
native-ops token list   --state-dir /var/lib/native-ops
native-ops token revoke --state-dir /var/lib/native-ops --id <id>
```

A running daemon honours tokens created this way immediately (it notices the file change).

To avoid running any command on the host, IaC/CI can provision the first credential as a secret:
set `NATIVE_OPS_BOOTSTRAP_TOKEN=nops_...` (at least 32 characters after the prefix) in
`/etc/native-ops/serve.env`. The daemon loads it as an in-memory `admin` token named `bootstrap`;
it is never written to disk. Rotate it by changing the secret and restarting.

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

`plan` (a dry-run of `apply`), `apply` at a git ref, instance update/migrate/backup as audited
jobs with per-host locking, and OIDC sign-in for the UI. The API is versioned (`/v1`) so a
separate fleet manager can depend on it.
