#!/bin/bash
# Plane.so image: project management tool.
# Plane is multi-service (web frontend, API backend, worker, Redis, PostgreSQL).
# All services run inside this single container; managed by supervisord.
set -euo pipefail

PLANE_VERSION="${PLANE_VERSION:-v0.23}"

apt-get update -qq
apt-get install -y -qq --no-install-recommends \
  ca-certificates curl git python3 python3-pip python3-venv \
  postgresql postgresql-client redis-server nginx supervisor gettext-base

# Plane is deployed from Docker Hub images normally.
# For LXC native install, we clone and install from source.
cd /tmp
git clone --depth 1 --branch "$PLANE_VERSION" https://github.com/makeplane/plane.git
cd plane

# --- Frontend (Next.js) ---
cd web
npm ci --ignore-scripts
npm run build

# --- API (Django/Python) ---
cd ../apiserver
python3 -m venv /opt/plane/venv
/opt/plane/venv/bin/pip install -r requirements.txt

# --- Collect static files ---
export DEBUG=0
export PYTHONDONTWRITEBYTECODE=1
cd /tmp/plane/apiserver
/opt/plane/venv/bin/python manage.py collectstatic --noinput

# Copy to final location
mkdir -p /opt/plane
cp -r /tmp/plane /opt/plane/

# Supervisor config for all Plane services
cat > /etc/supervisor/conf.d/plane.conf <<'SUP'
[program:plane-web]
command=/usr/bin/node /opt/plane/web/.next/standalone/server.js
directory=/opt/plane/web
autostart=true
autorestart=true
user=www-data

[program:plane-api]
command=/opt/plane/venv/bin/python manage.py runserver 0.0.0.0:8000
directory=/opt/plane/apiserver
autostart=true
autorestart=true
user=www-data

[program:plane-worker]
command=/opt/plane/venv/bin/celery -A plane worker -l info
directory=/opt/plane/apiserver
autostart=true
autorestart=true
user=www-data

[program:plane-beat]
command=/opt/plane/venv/bin/celery -A plane beat -l info
directory=/opt/plane/apiserver
autostart=true
autorestart=true
user=www-data

[program:plane-redis]
command=/usr/bin/redis-server --bind 127.0.0.1
autostart=true
autorestart=true

[program:plane-postgres]
command=/usr/lib/postgresql/15/bin/postgres -D /var/lib/postgresql/15/main -c config_file=/etc/postgresql/15/main/postgresql.conf
autostart=true
autorestart=true
user=postgres

[program:plane-nginx]
command=/usr/sbin/nginx -g "daemon off;"
autostart=true
autorestart=true
SUP

# PostgreSQL init
service postgresql start
sudo -u postgres createuser plane 2>/dev/null || true
sudo -u postgres createdb plane -O plane 2>/dev/null || true
service postgresql stop

# Nginx config for Plane (serves on :80)
cat > /etc/nginx/sites-available/plane <<'NGX'
server {
    listen 80;
    server_name _;

    location /api/ {
        proxy_pass http://127.0.0.1:8000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }

    location / {
        proxy_pass http://127.0.0.1:3000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }
}
NGX
ln -sf /etc/nginx/sites-available/plane /etc/nginx/sites-enabled/plane
rm -f /etc/nginx/sites-enabled/default

# Cleanup
rm -rf /tmp/plane
apt-get clean
rm -rf /var/lib/apt/lists/*
