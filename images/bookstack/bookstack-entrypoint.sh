#!/bin/bash
# BookStack entrypoint: initialize the persistent data volume (first boot
# only), render the real Laravel .env from persisted secrets, run pending
# migrations, then hand off to supervisord for the long-running
# mysqld/php-fpm/nginx trio.
#
# Runs on EVERY boot (systemd ExecStart), not just the first — migrations
# must be idempotent (Laravel's own migrate command already is), and the
# persisted-state checks below all use "if missing, initialize" so a
# normal restart is a fast no-op.
set -euo pipefail

DATA_DIR="${DATA_DIR:-/data}"

# Refuse to do anything until $DATA_DIR is genuinely the mounted custom
# volume, not the container's own ephemeral rootfs directory at that path.
# This guards against exactly the race that happened in practice: something
# (systemd's own preset reconciliation on a container's first boot appears
# to re-enable a unit that was explicitly disabled at image-build time,
# independent of deploy-bookstack.sh's own enable/disable choreography)
# started this unit before `incus config device add` had attached the
# volume. Without this check, that early run would happily
# mariadb-install-db and launch supervisord against the ephemeral
# directory, and when the real volume mounted moments later (shadowing it
# from underneath), that first generation's still-running mysqld/nginx/
# php-fpm would linger and fight a second, correct generation for the same
# ports/sockets — which is exactly what was observed live. Exiting fast
# here instead (systemd's Restart=on-failure retries every few seconds)
# means an early invocation does nothing at all rather than something
# that has to be untangled later.
if ! mountpoint -q "$DATA_DIR" 2>/dev/null; then
  echo "[bookstack] $DATA_DIR is not a mounted volume yet — waiting for it to be attached" >&2
  exit 1
fi

mkdir -p "$DATA_DIR/mysql" "$DATA_DIR/storage"

# Point MariaDB at the persistent volume regardless of who starts it later
# (the temporary migration instance below, or supervisord's long-running
# one) — one place to say where the data actually lives.
cat > /etc/mysql/mariadb.conf.d/60-datadir.cnf <<CNF
[mysqld]
datadir = $DATA_DIR/mysql
CNF

if [ ! -d "$DATA_DIR/mysql/mysql" ]; then
  echo "[bookstack] initializing MariaDB datadir on the data volume..."
  mariadb-install-db --datadir="$DATA_DIR/mysql" --user=mysql >/dev/null
fi
chown -R mysql:mysql "$DATA_DIR/mysql"

# Secrets that must never change once generated (APP_KEY especially —
# rotating it invalidates every existing session and any encrypted
# column). First touch only; every later boot just reads what's here.
if [ ! -f "$DATA_DIR/env" ]; then
  echo "[bookstack] first boot: generating APP_KEY and DB password..."
  {
    echo "APP_URL=${BOOKSTACK_URL:-https://wiki.opsavor.work}"
    echo "APP_KEY=base64:$(openssl rand -base64 32)"
    echo "DB_PASSWORD=$(openssl rand -hex 24)"
  } > "$DATA_DIR/env"
  chmod 600 "$DATA_DIR/env"
fi
set -a
# shellcheck disable=SC1091
. "$DATA_DIR/env"
set +a

# AUTH_METHOD/OIDC_* are OPERATOR-supplied, not auto-generated: unlike
# APP_KEY/DB_PASSWORD above, nothing here writes them into $DATA_DIR/env
# on first boot. To enable SSO, add AUTH_METHOD=oidc plus OIDC_CLIENT_ID
# and OIDC_CLIENT_SECRET to $DATA_DIR/env directly (see README "Google
# Workspace SSO for BookStack") and `incus restart bookstack` — this
# script re-sources that file on every boot, so whatever's there just
# flows through into the real .env below. Defaults below match a Google
# Workspace OIDC setup; override any of them the same way if needed.
cat > /var/www/bookstack/.env <<ENVEOF
APP_ENV=production
APP_DEBUG=false
APP_URL=${APP_URL}
APP_KEY=${APP_KEY}
DB_HOST=localhost
DB_DATABASE=bookstack
DB_USERNAME=bookstack
DB_PASSWORD=${DB_PASSWORD}
AUTH_METHOD=${AUTH_METHOD:-standard}
OIDC_NAME=${OIDC_NAME:-Google Workspace}
OIDC_CLIENT_ID=${OIDC_CLIENT_ID:-null}
OIDC_CLIENT_SECRET=${OIDC_CLIENT_SECRET:-null}
OIDC_ISSUER=${OIDC_ISSUER:-https://accounts.google.com}
OIDC_ISSUER_DISCOVER=${OIDC_ISSUER_DISCOVER:-true}
OIDC_DISPLAY_NAME_CLAIMS=${OIDC_DISPLAY_NAME_CLAIMS:-name}
OIDC_END_SESSION_ENDPOINT=${OIDC_END_SESSION_ENDPOINT:-true}
ENVEOF
chown www-data:www-data /var/www/bookstack/.env
chmod 640 /var/www/bookstack/.env

# storage/ (uploads, framework cache, logs) must survive an image replace.
# First boot seeds the volume from the image's own baked skeleton
# (migrations, default themes, etc. that ship inside storage/); every boot
# after that, the image's copy is discarded in favor of the symlink.
if [ -z "$(ls -A "$DATA_DIR/storage" 2>/dev/null)" ]; then
  cp -a /var/www/bookstack/storage/. "$DATA_DIR/storage/"
fi
rm -rf /var/www/bookstack/storage
ln -s "$DATA_DIR/storage" /var/www/bookstack/storage
chown -R www-data:www-data "$DATA_DIR/storage"

# Bring up a temporary mysqld to migrate against, then hand off to
# supervisord for the long-running instance — simpler and more robust than
# trying to background/exec-replace around a single shared mysqld process.
mkdir -p /run/mysqld
chown mysql:mysql /run/mysqld
mysqld --user=mysql --skip-networking --socket=/run/mysqld/mysqld.sock &
MYSQLD_PID=$!
echo "[bookstack] waiting for MariaDB..."
for i in $(seq 1 30); do
  mysqladmin --socket=/run/mysqld/mysqld.sock ping >/dev/null 2>&1 && break
  sleep 1
done
mysql --socket=/run/mysqld/mysqld.sock -e "CREATE DATABASE IF NOT EXISTS bookstack;"
mysql --socket=/run/mysqld/mysqld.sock -e "CREATE USER IF NOT EXISTS 'bookstack'@'localhost' IDENTIFIED BY '${DB_PASSWORD}';"
mysql --socket=/run/mysqld/mysqld.sock -e "ALTER USER 'bookstack'@'localhost' IDENTIFIED BY '${DB_PASSWORD}';"
mysql --socket=/run/mysqld/mysqld.sock -e "GRANT ALL ON bookstack.* TO 'bookstack'@'localhost'; FLUSH PRIVILEGES;"

echo "[bookstack] running migrations..."
(cd /var/www/bookstack && php artisan migrate --force)

mysqladmin --socket=/run/mysqld/mysqld.sock shutdown
wait "$MYSQLD_PID" 2>/dev/null || true

# A prior crashed attempt (e.g. after a hard host reboot) can leave
# supervisord's own unix socket file behind, which makes the next
# supervisord refuse to start ("Another program is already listening on a
# port that one of our HTTP servers is configured to use") even though
# nothing is actually still running. But that refusal is also supervisord's
# ONLY defense against two genuinely simultaneous instances — unconditionally
# removing the file here previously defeated it and let a duplicate,
# still-unexplained second invocation of this entrypoint run its own
# mysqld/nginx/php-fpm alongside a real, already-running first one, which
# is what was actually causing the crash-loop this guards against. Only
# clean up the socket when nothing is actually listening on it.
if ! pgrep -x supervisord >/dev/null 2>&1; then
  rm -f /var/run/supervisor.sock
fi

echo "[bookstack] starting mysqld/php-fpm/nginx..."
exec /usr/bin/supervisord -n -c /etc/supervisor/supervisord.conf
