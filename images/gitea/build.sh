#!/bin/bash
# Gitea image: git hosting. A single static Go binary — unlike bookstack,
# Gitea IS the whole server (no separate DB/web-server processes to
# supervise), which makes this a lot simpler.
set -euo pipefail

GITEA_VERSION="${GITEA_VERSION:-1.27.3}"

apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl git sqlite3

curl -fsSL "https://dl.gitea.com/gitea/${GITEA_VERSION}/gitea-${GITEA_VERSION}-linux-amd64" \
  -o /usr/local/bin/gitea
chmod +x /usr/local/bin/gitea

# Gitea refuses to run as root at all (even `--help` exits with
# mustNotRunAsRoot). Fixed uid/gid so ownership on the persistent volume
# stays valid across an image rebuild, rather than whatever `useradd`
# would assign next from the system range.
groupadd -g 1100 git
useradd -u 1100 -g git -M -d /data -s /usr/sbin/nologin git

install -m 0755 /tmp/build/gitea-entrypoint.sh /usr/local/bin/gitea-entrypoint.sh

# Root, not User=git: the entrypoint needs root to chown the freshly
# security.shifted-mounted /data (same reason as bookstack's — the volume
# is host-root-owned until something inside the container claims it), then
# drops to git itself for every actual gitea invocation (gitea's own
# mustNotRunAsRoot check would refuse otherwise).
cat > /etc/systemd/system/gitea.service <<'SVC'
[Unit]
Description=Gitea (git hosting)
After=network.target

[Service]
Type=simple
Environment=DATA_DIR=/data
ExecStart=/usr/local/bin/gitea-entrypoint.sh
Restart=on-failure
RestartSec=5
TimeoutStartSec=60

[Install]
WantedBy=multi-user.target
SVC

# NOT enabled here on purpose — see images/bookstack/build.sh and
# AGENTS.md "Manager -> Incus control plane" gotchas for why: incus launch
# boots the container immediately, before deploy-gitea.sh attaches the
# persistent volume. deploy-gitea.sh runs `systemctl enable --now gitea`
# itself, after the volume is attached; the entrypoint also independently
# refuses to do anything until `mountpoint -q /data` is true.

apt-get clean
rm -rf /var/lib/apt/lists/*
