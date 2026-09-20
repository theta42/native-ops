#!/bin/bash
# BookStack image: documentation/wiki.
# PHP + MySQL in a single container.
set -euo pipefail

BOOKSTACK_VERSION="${BOOKSTACK_VERSION:-v24.05}"

apt-get update -qq
apt-get install -y -qq --no-install-recommends \
  ca-certificates curl git \
  php8.2-cli php8.2-curl php8.2-mbstring php8.2-xml php8.2-mysql \
  php8.2-gd php8.2-zip php8.2-tidy php8.2-fpm \
  mariadb-server mariadb-client nginx supervisor

# Install composer
curl -sS https://getcomposer.org/installer | php -- --install-dir=/usr/local/bin --filename=composer

# Clone BookStack
git clone --depth 1 --branch "$BOOKSTACK_VERSION" https://github.com/BookStackApp/BookStack.git /var/www/bookstack
cd /var/www/bookstack
composer install --no-dev --no-interaction

# Database setup
service mariadb start
mysql -e "CREATE DATABASE IF NOT EXISTS bookstack;"
mysql -e "CREATE USER IF NOT EXISTS 'bookstack'@'localhost' IDENTIFIED BY 'bookstack';"
mysql -e "GRANT ALL ON bookstack.* TO 'bookstack'@'localhost';"
mysql -e "FLUSH PRIVILEGES;"

# BookStack .env
cat > /var/www/bookstack/.env <<'ENV'
APP_ENV=production
APP_DEBUG=false
APP_URL=https://docs.opsavor.app
DB_HOST=localhost
DB_DATABASE=bookstack
DB_USERNAME=bookstack
DB_PASSWORD=bookstack
ENV

chown -R www-data:www-data /var/www/bookstack
chmod -R 755 /var/www/bookstack/storage /var/www/bookstack/bootstrap/cache

# Run migrations
sudo -u www-data php artisan migrate --force

# Nginx config
cat > /etc/nginx/sites-available/bookstack <<'NGX'
server {
    listen 80;
    server_name _;
    root /var/www/bookstack/public;
    index index.php index.html;

    location / {
        try_files $uri $uri/ /index.php?$query_string;
    }

    location ~ \.php$ {
        fastcgi_pass unix:/var/run/php/php8.2-fpm.sock;
        fastcgi_index index.php;
        fastcgi_param SCRIPT_FILENAME $realpath_root$fastcgi_script_name;
        include fastcgi_params;
    }
}
NGX
ln -sf /etc/nginx/sites-available/bookstack /etc/nginx/sites-enabled/bookstack
rm -f /etc/nginx/sites-enabled/default

# Supervisor for php-fpm + mysql + nginx
cat > /etc/supervisor/conf.d/bookstack.conf <<'SUP'
[program:bookstack-php]
command=/usr/sbin/php-fpm8.2 -F
autostart=true
autorestart=true

[program:bookstack-mysql]
command=/usr/sbin/mysqld
autostart=true
autorestart=true
user=mysql

[program:bookstack-nginx]
command=/usr/sbin/nginx -g "daemon off;"
autostart=true
autorestart=true
SUP

cat > /etc/systemd/system/bookstack.service <<'SVC'
[Unit]
Description=BookStack wiki
After=network.target

[Service]
Type=forking
ExecStart=/usr/bin/supervisord -c /etc/supervisor/supervisord.conf
ExecStop=/usr/bin/supervisorctl shutdown
Restart=on-failure

[Install]
WantedBy=multi-user.target
SVC

systemctl enable bookstack
service mariadb stop

# Cleanup
apt-get clean
rm -rf /var/lib/apt/lists/*
