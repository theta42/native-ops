#!/bin/bash
# Gitea image: Gitea + Actions runner for CI/CD.
set -euo pipefail

GITEA_VERSION="${GITEA_VERSION:-1.22.3}"

apt-get update -qq
apt-get install -y -qq --no-install-recommends \
  ca-certificates curl git sqlite3

# Install Gitea
curl -fsSL "https://dl.gitea.com/gitea/${GITEA_VERSION}/gitea-${GITEA_VERSION}-linux-amd64" \
  -o /usr/local/bin/gitea
chmod +x /usr/local/bin/gitea

# Create git user (Gitea convention)
useradd --system --shell /bin/bash --create-home git
mkdir -p /var/lib/gitea/{custom,data,log}
chown -R git:git /var/lib/gitea
chmod 750 /var/lib/gitea
mkdir -p /etc/gitea
chown root:git /etc/gitea
chmod 770 /etc/gitea

# Gitea systemd service
cat > /etc/systemd/system/gitea.service <<'SVC'
[Unit]
Description=Gitea (git hosting)
After=network.target

[Service]
Type=simple
User=git
Group=git
WorkingDirectory=/var/lib/gitea
ExecStart=/usr/local/bin/gitea web --config /etc/gitea/app.ini
Restart=on-failure
RestartSec=5
Environment=USER=git
Environment=HOME=/home/git
Environment=GITEA_WORK_DIR=/var/lib/gitea

[Install]
WantedBy=multi-user.target
SVC

systemctl enable gitea

# Default config — operator overrides via /etc/gitea/app.ini
cat > /etc/gitea/app.ini <<'INI'
APP_NAME = Opsavor Git
RUN_USER = git
RUN_MODE = prod
WORK_PATH = /var/lib/gitea

[server]
DOMAIN = git.opsavor.app
ROOT_URL = https://git.opsavor.app/
HTTP_PORT = 3000
SSH_PORT = 22
DISABLE_SSH = false
START_SSH_SERVER = true
LFS_START_SERVER = true

[database]
DB_TYPE = sqlite3
PATH = /var/lib/gitea/data/gitea.db

[actions]
ENABLED = true

[security]
INSTALL_LOCK = false
SECRET_KEY = CHANGE_ME
INTERNAL_TOKEN = CHANGE_ME

[service]
DISABLE_REGISTRATION = true
REQUIRE_SIGNIN_VIEW = true

[repository]
DEFAULT_BRANCH = main
INI

# Cleanup
apt-get clean
rm -rf /var/lib/apt/lists/*
