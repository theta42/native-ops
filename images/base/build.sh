#!/bin/bash
# Base image: Debian 13 + Node.js 22 + common tools.
# All other images build FROM this one.
set -euo pipefail

apt-get update -qq
apt-get install -y -qq --no-install-recommends \
  ca-certificates curl git gnupg

# Node.js 22 (NodeSource)
mkdir -p /etc/apt/keyrings
curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
  | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg
echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main" \
  > /etc/apt/sources.list.d/nodesource.list
apt-get update -qq
apt-get install -y -qq nodejs

# Verify
node --version
npm --version

# Cleanup
apt-get clean
rm -rf /var/lib/apt/lists/*
