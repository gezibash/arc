#!/usr/bin/env bash
# Installed as EC2 user-data for the ARC traversal verification hosts.
# This script deliberately does not receive ARC identities, provider grants, or
# SSH private keys.  The operator deploys the checked-out source separately.
set -euo pipefail

readonly LAB_LOG=/var/log/arc-traversal-lab-bootstrap.log
readonly LAB_HOME=/opt/arc

exec > >(tee -a "$LAB_LOG") 2>&1

# Arm the termination path before downloading or installing any software. EC2
# is configured by the launcher to terminate after this instance shuts down.
systemd-run --unit=arc-lab-terminate --on-active=2h /sbin/shutdown -h now
install -d -m 0755 /etc/arc-traversal-lab
touch /etc/arc-traversal-lab/ready-for-router-setup

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends \
  ca-certificates conntrack curl git iproute2 iptables jq tcpdump

# mise is the only runtime installer. The project pins Erlang/OTP and Elixir
# in mise.toml; apt is used only for operating-system networking utilities.
sudo -u ubuntu -H sh -c 'curl --fail --location --silent --show-error https://mise.run | sh'

install -d -m 0755 "$LAB_HOME"
chown ubuntu:ubuntu "$LAB_HOME"

cat <<'EOF'
The ARC source snapshot is deployed by the lab operator after bootstrap.
Run from that checkout:
  MISE_ERLANG_COMPILE=false MISE_ERLANG_PRECOMPILED_OS=ubuntu-24.04 /home/ubuntu/.local/bin/mise install
  /home/ubuntu/.local/bin/mise exec -- mix deps.get
  /home/ubuntu/.local/bin/mise exec -- mix compile
EOF
