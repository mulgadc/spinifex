#!/bin/bash
# dev-install.sh — Full local development setup via production installer.
# Builds from source, assembles a tarball, runs setup.sh for scaffolding
# (user creation, directories, systemd units), initializes the cluster,
# and starts services via systemd.
#
# Environment variables:
#   SPINIFEX_REGION            Region for spx admin init (default: ap-southeast-2)
#   SPINIFEX_AZ                AZ for spx admin init (default: ${SPINIFEX_REGION}a)
#   DEV_INSTALL_SKIP_INIT      Internal flag used by reset-dev-env.sh. When 1,
#                              stops after setup.sh and skips setup-ovn, init,
#                              CA trust, service start, and LB image import —
#                              caller owns those steps. Do not set manually.
#
# Usage: ./scripts/dev-install.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
MULGA_ROOT="$(cd "$PROJECT_ROOT/.." && pwd)"

echo "=== Building ==="
cd "$PROJECT_ROOT" && make build
cd "$MULGA_ROOT/viperblock" && make go_build_nbd

echo "=== Assembling tarball for setup.sh ==="
STAGING=$(mktemp -d)
trap 'rm -rf "$STAGING"' EXIT
cp "$PROJECT_ROOT/bin/spx" "$STAGING/spx"
cp "$MULGA_ROOT/viperblock/lib/nbdkit-viperblock-plugin.so" "$STAGING/"
cp "$PROJECT_ROOT/scripts/setup-ovn.sh" "$STAGING/"
mkdir -p "$STAGING/systemd"
cp "$PROJECT_ROOT/build/systemd/"* "$STAGING/systemd/"
mkdir -p "$STAGING/scripts"
cp "$PROJECT_ROOT/build/scripts/"* "$STAGING/scripts/"
cp "$PROJECT_ROOT/build/logrotate/spinifex" "$STAGING/logrotate-spinifex"
tar czf /tmp/spinifex-local.tar.gz -C "$STAGING" .

echo "=== Cleaning stale state from previous installs ==="
# Stop any running services before modifying files
sudo systemctl stop spinifex.target 2>/dev/null || true
sudo systemctl reset-failed 'spinifex-*' 2>/dev/null || true

# Remove stale files owned by the dev user in production paths.
# Previous dev-mode installs (admin init without sudo, start-dev.sh) leave
# files owned by tf-user that service users (spinifex-nats, etc.) can't read
# under systemd's ProtectSystem=strict sandboxing.
for dir in /var/lib/spinifex /var/log/spinifex /etc/spinifex; do
    if [ -d "$dir" ]; then
        # Remove PID files, stale logs, and the legacy ~/spinifex/config symlink
        sudo find "$dir" -name '*.pid' -delete 2>/dev/null || true
        sudo find "$dir" -name '*.log' -delete 2>/dev/null || true
        sudo find "$dir" -name '*.log.*' -delete 2>/dev/null || true
    fi
done
# Remove legacy data dir contents that conflict with production layout
if [ -d /var/lib/spinifex/config ]; then
    sudo rm -rf /var/lib/spinifex/config
fi

echo "=== Running setup.sh (creates users, dirs, systemd units) ==="
sudo INSTALL_SPINIFEX_TARBALL=/tmp/spinifex-local.tar.gz INSTALL_SPINIFEX_SKIP_NEWGRP=1 bash "$PROJECT_ROOT/scripts/setup.sh"
rm -f /tmp/spinifex-local.tar.gz

if [ "${DEV_INSTALL_SKIP_INIT:-0}" = "1" ]; then
    echo "=== DEV_INSTALL_SKIP_INIT=1 — stopping after setup.sh ==="
    echo "Caller owns: setup-ovn, spx admin init, CA trust, service start, LB image."
    exit 0
fi

echo "=== Setting up OVN ==="
sudo /usr/local/share/spinifex/setup-ovn.sh --management

SPINIFEX_REGION="${SPINIFEX_REGION:-ap-southeast-2}"
SPINIFEX_AZ="${SPINIFEX_AZ:-${SPINIFEX_REGION}a}"

echo "=== Initializing (region=$SPINIFEX_REGION az=$SPINIFEX_AZ) ==="
sudo spx admin init --force --region "$SPINIFEX_REGION" --az "$SPINIFEX_AZ" --node node1 --nodes 1

echo "=== Installing CA certificate ==="
sudo cp /etc/spinifex/ca.pem /usr/local/share/ca-certificates/spinifex-ca.crt
sudo update-ca-certificates

echo "=== Starting services ==="
sudo systemctl start spinifex.target

# Check if user needs to re-login for spinifex group membership
if ! id -Gn 2>/dev/null | grep -qw spinifex; then
    newgrp spinifex
fi

echo "=== Building and importing LB image ==="
cd "$PROJECT_ROOT" && make build-lb-agent
"$PROJECT_ROOT/scripts/build-system-image.sh" "$PROJECT_ROOT/scripts/images/lb.conf" --import --quiet

echo "=== Done ==="
echo "Services: sudo systemctl status spinifex.target"
echo "Logs:     journalctl -u 'spinifex-*' -f"
echo "Test:     AWS_PROFILE=spinifex aws ec2 describe-instances"
echo "Iterate:  make deploy (rebuild + restart all)"
