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

cat > /var/www/bookstack/.env <<ENVEOF
APP_ENV=production
APP_DEBUG=false
APP_URL=${APP_URL}
APP_KEY=${APP_KEY}
DB_HOST=localhost
DB_DATABASE=bookstack
DB_USERNAME=bookstack
DB_PASSWORD=${DB_PASSWORD}
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

echo "[bookstack] starting mysqld/php-fpm/nginx..."
exec /usr/bin/supervisord -n -c /etc/supervisor/supervisord.conf
