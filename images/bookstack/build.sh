#!/bin/bash
# BookStack image: wiki/documentation. PHP-FPM + MariaDB + nginx, supervised
# under one systemd unit (BookStack's upstream architecture is genuinely
# multi-process; supervisord multiplexes them the way an all-in-one Docker
# image would, but natively — see AGENTS.md principle 3, still no Docker).
#
# Versionless package names (php-fpm, php-cli, ...) on purpose: Debian
# aliases them to whatever the release's default PHP is (8.4 on trixie —
# NOT 8.2, which doesn't exist in trixie's repos and was this script's
# original, never-tested assumption). Pinning "php8.2-*" would have failed
# outright on this host.
set -euo pipefail

BOOKSTACK_VERSION="${BOOKSTACK_VERSION:-v26.05.5}"

apt-get update -qq
apt-get install -y -qq --no-install-recommends \
  ca-certificates curl git unzip \
  php-fpm php-cli php-curl php-mbstring php-xml php-mysql \
  php-gd php-zip php-tidy php-intl php-bcmath \
  mariadb-server mariadb-client nginx supervisor openssl

curl -sS https://getcomposer.org/installer | php -- --install-dir=/usr/local/bin --filename=composer

git clone --depth 1 --branch "$BOOKSTACK_VERSION" https://github.com/BookStackApp/BookStack.git /var/www/bookstack
cd /var/www/bookstack
COMPOSER_ALLOW_SUPERUSER=1 composer install --no-dev --no-interaction --optimize-autoloader

chown -R www-data:www-data /var/www/bookstack
chmod -R 755 /var/www/bookstack/storage /var/www/bookstack/bootstrap/cache

# nginx: BookStack's public/ as webroot, PHP handed off to php-fpm over the
# versionless socket alias php-fpm's own package maintains
# (/run/php/php-fpm.sock -> /etc/alternatives/php-fpm.sock -> the real
# versioned socket) so this never needs updating across a PHP version bump.
mkdir -p /etc/nginx/sites-available
cat > /etc/nginx/sites-available/bookstack <<'NGX'
server {
    listen 80;
    server_name _;
    root /var/www/bookstack/public;
    index index.php index.html;
    client_max_body_size 50M;

    location / {
        try_files $uri $uri/ /index.php?$query_string;
    }

    location ~ \.php$ {
        fastcgi_pass unix:/run/php/php-fpm.sock;
        fastcgi_index index.php;
        fastcgi_param SCRIPT_FILENAME $realpath_root$fastcgi_script_name;
        include fastcgi_params;
    }
}
NGX
ln -sf /etc/nginx/sites-available/bookstack /etc/nginx/sites-enabled/bookstack
rm -f /etc/nginx/sites-enabled/default

# The php-fpm package aliases its SOCKET path (/run/php/php-fpm.sock) to a
# stable name via update-alternatives, but not the binary or the systemd
# unit name — those stay version-qualified (php-fpm8.4, php8.4-fpm.service).
# Resolve them instead of hardcoding a version that WILL go stale on the
# next Debian release.
PHP_FPM_BIN="$(ls /usr/sbin/php-fpm[0-9]* | head -1)"
PHP_FPM_UNIT="$(systemctl list-unit-files --no-legend 'php*-fpm.service' | awk '{print $1}' | head -1)"
# supervisord manages php-fpm, mariadb, and nginx instead — the
# mariadb-server, nginx, and php-fpm packages all auto-enable (and their
# postinst scripts auto-START) their own systemd units. Left enabled, the
# stock mariadb.service starts mysqld against our /data-redirected datadir
# on every boot BEFORE the entrypoint's own temporary migration mysqld or
# supervisord's long-running one gets a chance to, and they collide on the
# same Aria/InnoDB lock files; the stock nginx.service does the same thing
# to port 80. Disable all three package units; supervisord owns them now.
systemctl disable "$PHP_FPM_UNIT" mariadb nginx
systemctl stop "$PHP_FPM_UNIT" mariadb nginx 2>/dev/null || true

# supervisord runs all three long-lived processes; the entrypoint (below)
# does one-time/per-boot setup (persisted volume init, migrations) BEFORE
# handing off to it, the same shape images/restaurant/build.sh uses.
mkdir -p /etc/supervisor/conf.d
cat > /etc/supervisor/conf.d/bookstack.conf <<SUP
[program:bookstack-mysql]
command=/usr/sbin/mysqld --skip-networking --socket=/run/mysqld/mysqld.sock
user=mysql
autostart=true
autorestart=true
stopsignal=TERM

[program:bookstack-php]
command=$PHP_FPM_BIN -F
autostart=true
autorestart=true

[program:bookstack-nginx]
command=/usr/sbin/nginx -g "daemon off;"
autostart=true
autorestart=true
SUP

install -m 0755 /tmp/build/bookstack-entrypoint.sh /usr/local/bin/bookstack-entrypoint.sh

# Systemd unit: root (the entrypoint chowns things and starts mysqld/nginx,
# which then drop their own privileges the normal way — supervisord's own
# `user=` lines above, and php-fpm's/nginx's pool/worker config, same as
# they'd do outside a container).
cat > /etc/systemd/system/bookstack.service <<'SVC'
[Unit]
Description=BookStack wiki
After=network.target

[Service]
Type=simple
Environment=DATA_DIR=/data
ExecStart=/usr/local/bin/bookstack-entrypoint.sh
Restart=on-failure
RestartSec=5
TimeoutStartSec=180

[Install]
WantedBy=multi-user.target
SVC

# NOT enabled here on purpose: `incus launch` boots the container
# immediately, and an enabled unit would auto-start against the
# not-yet-attached data volume (an ephemeral, pre-mount /data) before
# deploy-bookstack.sh gets a chance to attach the real one — a race that
# actually bit this: mariadb-install-db plus a backgrounded temporary
# mysqld is heavy/slow enough that it was often still mid-flight when the
# subsequent `incus restart` fired, leaving that first attempt's processes
# only partially cleaned up and colliding with the second boot's (multiple
# supervisord instances, both nginx and php-fpm fighting over the same
# port/socket). deploy-bookstack.sh instead does
# `systemctl enable --now bookstack` itself, AFTER the volume is attached.

apt-get clean
rm -rf /var/lib/apt/lists/*
