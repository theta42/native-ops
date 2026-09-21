#!/bin/bash
# Provision a fresh Debian VPS as an Incus host.
# Idempotent: safe to re-run. Installs Incus, initializes ZFS + bridge,
# applies profiles from this repo, and locks down the host firewall.
#
# Usage: ./scripts/provision-host.sh
# Requires: root, Debian 13, curl, git
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"

echo "[provision] Installing Incus..."

# Zabbly repository (official Incus stable)
if [ ! -f /etc/apt/keyrings/zabbly.asc ]; then
  mkdir -p /etc/apt/keyrings
  curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc
fi

if [ ! -f /etc/apt/sources.list.d/zabbly-incus-stable.list ]; then
  . /etc/os-release
  echo "deb [signed-by=/etc/apt/keyrings/zabbly.asc] https://pkgs.zabbly.com/incus/stable ${VERSION_CODENAME} main" \
    > /etc/apt/sources.list.d/zabbly-incus-stable.list
fi

apt-get update -qq
apt-get install -y -qq incus incus-client incus-tools zfsutils-linux nftables ufw

# Initialize Incus (idempotent — skips if already initialized)
if ! incus info >/dev/null 2>&1; then
  echo "[provision] Initializing Incus..."
  incus admin init --preseed < "$REPO_DIR/incus/preseed.yml"
else
  echo "[provision] Incus already initialized, skipping init."
fi

# Apply profiles
echo "[provision] Applying profiles..."
for profile_file in "$REPO_DIR"/incus/profiles/*.yml; do
  profile_name=$(basename "$profile_file" .yml)
  if incus profile show "$profile_name" >/dev/null 2>&1; then
    echo "  updating profile: $profile_name"
    incus profile edit "$profile_name" < "$profile_file" || true
  else
    echo "  creating profile: $profile_name"
    incus profile create "$profile_name" </dev/null 2>/dev/null || true
    incus profile edit "$profile_name" < "$profile_file" || true
  fi
done

# OCI remotes for services run as pre-built images rather than built from
# source (e.g. Plane — see scripts/deploy-plane.sh). docker.io is the
# default registry; quay.io is needed too since some images (MinIO, as of
# this writing) have moved off Docker Hub entirely.
echo "[provision] Adding OCI remotes..."
incus remote list --format csv | grep -q '^docker,' || incus remote add docker https://docker.io --protocol=oci
incus remote list --format csv | grep -q '^quay,' || incus remote add quay https://quay.io --protocol=oci

# Firewall (host level)
echo "[provision] Configuring firewall..."
ufw default deny incoming
ufw default allow outgoing
ufw allow 22/tcp comment "SSH"
ufw allow 80/tcp comment "HTTP"
ufw allow 443/tcp comment "HTTPS"
# 8443 for Incus API (multi-host phase). Commented out for single-node.
# ufw allow 8443/tcp comment "Incus API"
ufw --force enable

# CRITICAL: UFW blocks Incus bridge traffic by default (drops DHCP and
# forwarded packets). These rules allow containers on incusbr0 to reach
# the host (DHCP, DNS) and be NATed out to the internet.
ufw allow in on incusbr0 comment "Incus bridge input"
ufw route allow in on incusbr0 comment "Incus bridge forward in"
ufw route allow out on incusbr0 comment "Incus bridge forward out"
ufw route allow in on incusbr0 out on eth0 comment "Incus bridge NAT out"
ufw reload

# The manager container controls Incus (launching/replacing restaurant and
# service containers) by SSHing back to this host as a dedicated,
# low-privilege user rather than mounting the Incus socket into the
# container or giving it a root key: `manager-ctl` can run `incus` (it's in
# the incus-admin group, which owns /var/lib/incus/unix.socket) but has no
# sudo and no other access. deploy-manager.sh pushes the private half of
# this key into the manager container on every deploy.
echo "[provision] Setting up the manager-ctl Incus control user..."
id manager-ctl >/dev/null 2>&1 || useradd -m -s /bin/bash -G incus-admin manager-ctl
mkdir -p /home/manager-ctl/.ssh
chmod 700 /home/manager-ctl/.ssh
if [ ! -f /root/.ssh/manager_incus_ed25519 ]; then
  ssh-keygen -t ed25519 -f /root/.ssh/manager_incus_ed25519 -N "" -C "manager-incus-ctl"
fi
cp /root/.ssh/manager_incus_ed25519.pub /home/manager-ctl/.ssh/authorized_keys
chown -R manager-ctl:manager-ctl /home/manager-ctl/.ssh
chmod 600 /home/manager-ctl/.ssh/authorized_keys

echo "[provision] Host provisioning complete."
echo ""
echo "Next steps:"
echo "  1. Verify: incus info"
echo "  2. Build base image: ./scripts/build-image.sh base"
echo "  3. Launch edge + manager: ./scripts/deploy-manager.sh"
