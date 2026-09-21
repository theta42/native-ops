#!/bin/bash
# Gitea entrypoint: initialize the persistent data volume (first boot
# only), render app.ini from persisted secrets, ensure a local admin
# account and (if configured) the Google OAuth2 login source exist, then
# run gitea itself in the foreground.
#
# Runs on EVERY boot, not just the first — every step below is written to
# be a fast no-op on a normal restart.
set -euo pipefail

DATA_DIR="${DATA_DIR:-/data}"

# Refuse to run until $DATA_DIR is genuinely the mounted custom volume —
# see images/bookstack/bookstack-entrypoint.sh for why this matters
# (an early start against the container's own ephemeral pre-mount
# directory left two generations of a stateful service fighting each
# other there; cheaper to guard against it here too than find out the
# same way twice).
if ! mountpoint -q "$DATA_DIR" 2>/dev/null; then
  echo "[gitea] $DATA_DIR is not a mounted volume yet — waiting for it to be attached" >&2
  exit 1
fi

chown -R git:git "$DATA_DIR"

GITEA="/usr/local/bin/gitea"
CONF="$DATA_DIR/custom/conf/app.ini"
SECRETS="$DATA_DIR/gitea-secrets.env"

# Secrets that must never change once generated (SECRET_KEY/INTERNAL_TOKEN
# encrypt/sign session and repo data; regenerating them invalidates
# existing sessions and, per Gitea's own docs, can break access to
# anything they were used to encrypt). First touch only.
if [ ! -f "$SECRETS" ]; then
  echo "[gitea] first boot: generating secrets and admin password..."
  SECRET_KEY="$(runuser -u git -- "$GITEA" generate secret SECRET_KEY)"
  INTERNAL_TOKEN="$(runuser -u git -- "$GITEA" generate secret INTERNAL_TOKEN)"
  JWT_SECRET="$(runuser -u git -- "$GITEA" generate secret JWT_SECRET)"
  ADMIN_PASSWORD="$(openssl rand -base64 24)"
  {
    echo "GITEA_URL=${GITEA_URL:-https://git.opsavor.work}"
    echo "SECRET_KEY=${SECRET_KEY}"
    echo "INTERNAL_TOKEN=${INTERNAL_TOKEN}"
    echo "JWT_SECRET=${JWT_SECRET}"
    echo "ADMIN_PASSWORD=${ADMIN_PASSWORD}"
  } > "$SECRETS"
  chown git:git "$SECRETS"
  chmod 600 "$SECRETS"
fi
set -a
# shellcheck disable=SC1091
. "$SECRETS"
set +a

# AUTH: like bookstack's OIDC_*, GITEA_OAUTH_CLIENT_ID/SECRET are
# OPERATOR-supplied — add them to $SECRETS directly (see README "Google
# Workspace SSO for Gitea") and `incus restart gitea` to enable Google
# login later; nothing below requires them to be set.
mkdir -p "$(dirname "$CONF")"
cat > "$CONF" <<INI
APP_NAME = Opsavor Git
RUN_USER = git
RUN_MODE = prod
WORK_PATH = ${DATA_DIR}

[server]
DOMAIN = git.opsavor.work
ROOT_URL = ${GITEA_URL}/
HTTP_PORT = 3000
DISABLE_SSH = true
LFS_START_SERVER = true

[database]
DB_TYPE = sqlite3
PATH = ${DATA_DIR}/gitea.db

[actions]
ENABLED = true

[security]
INSTALL_LOCK = true
SECRET_KEY = ${SECRET_KEY}
INTERNAL_TOKEN = ${INTERNAL_TOKEN}

[oauth2]
JWT_SECRET = ${JWT_SECRET}

[service]
DISABLE_REGISTRATION = true

[oauth2_client]
ENABLE_AUTO_REGISTRATION = true
ACCOUNT_LINKING = auto

[repository]
DEFAULT_BRANCH = main
INI
chown git:git "$CONF"

# Local admin fallback account — created once, with a generated (not
# well-known-default) password from the start. Every `gitea admin ...`
# subcommand initializes the app the same way `gitea web` does (DB
# migrations included), so this and the oauth2 step below both work
# against a freshly-rendered, not-yet-served config with no separate
# migrate step needed.
if ! runuser -u git -- "$GITEA" admin user list --config "$CONF" 2>/dev/null | grep -qw admin; then
  echo "[gitea] first boot: creating local admin account..."
  runuser -u git -- "$GITEA" admin user create --config "$CONF" \
    --username admin --email "admin@$(echo "$GITEA_URL" | sed -E 's#^https?://##')" \
    --password "$ADMIN_PASSWORD" --admin --must-change-password=false
fi

# Google OAuth2 login source — only added once GITEA_OAUTH_CLIENT_ID is
# present in $SECRETS (see above); idempotent, since `add-oauth` errors on
# a duplicate name rather than updating it.
if [ -n "${GITEA_OAUTH_CLIENT_ID:-}" ] && ! runuser -u git -- "$GITEA" admin auth list --config "$CONF" 2>/dev/null | grep -qw google; then
  echo "[gitea] configuring Google OAuth2 login..."
  runuser -u git -- "$GITEA" admin auth add-oauth --config "$CONF" \
    --name google --provider google \
    --key "$GITEA_OAUTH_CLIENT_ID" --secret "$GITEA_OAUTH_CLIENT_SECRET"
fi

echo "[gitea] starting..."
exec runuser -u git -- "$GITEA" web --config "$CONF"
