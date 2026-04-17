#!/bin/bash
# Spinifex binary installer
# Usage: curl -sfL https://install.mulgadc.com | bash
#
# Environment variables:
#   INSTALL_SPINIFEX_CHANNEL   Release channel: latest (default), dev
#   INSTALL_SPINIFEX_VERSION   Pin to specific version (overrides channel)
#   INSTALL_SPINIFEX_TARBALL   Path to local tarball (skips download, for testing/air-gapped)
#   INSTALL_SPINIFEX_SKIP_APT  Set to 1 to skip apt dependency install
#   INSTALL_SPINIFEX_SKIP_AWS  Set to 1 to skip AWS CLI install
#   INSTALL_SPINIFEX_SKIP_NEWGRP  Set to 1 to skip newgrp exec at end (for callers like dev-install.sh)

set -e

INSTALL_SPINIFEX_CHANNEL="${INSTALL_SPINIFEX_CHANNEL:-latest}"
INSTALL_BASE_URL="${INSTALL_BASE_URL:-https://install.mulgadc.com}"

# --- Colors ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
fatal() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }

# --- Sudo setup ---
setup_sudo() {
    if [ "$(id -u)" -eq 0 ]; then
        SUDO=""
    elif command -v sudo >/dev/null 2>&1; then
        SUDO="sudo"
        if ! $SUDO -n true 2>/dev/null; then
            info "This installer requires sudo access for system-level operations"
            $SUDO true || fatal "Failed to obtain sudo access"
        fi
    else
        fatal "This script requires root or sudo access"
    fi
}

# --- OS detection ---
detect_os() {
    if [ ! -f /etc/os-release ]; then
        fatal "Cannot detect OS: /etc/os-release not found"
    fi

    . /etc/os-release

    case "$ID" in
        debian)
            if [ "${VERSION_ID%%.*}" -lt 13 ] 2>/dev/null; then
                fatal "Debian $VERSION_ID is not supported. Minimum: Debian 13"
            fi
            ;;
        ubuntu)
            major="${VERSION_ID%%.*}"
            if [ "$major" -lt 22 ] 2>/dev/null; then
                fatal "Ubuntu $VERSION_ID is not supported. Minimum: Ubuntu 22.04"
            fi
            ;;
        *)
            fatal "Unsupported OS: $ID $VERSION_ID. Spinifex requires Debian 13+ or Ubuntu 22.04+"
            ;;
    esac

    info "Detected OS: $PRETTY_NAME"
}

# --- Architecture detection ---
detect_arch() {
    MACHINE=$(uname -m)
    case "$MACHINE" in
        x86_64)
            ARCH="amd64"
            QEMU_PACKAGES="qemu-system-x86"
            AWS_ARCH="x86_64"
            ;;
        aarch64|arm64)
            ARCH="arm64"
            QEMU_PACKAGES="qemu-system-arm"
            AWS_ARCH="aarch64"
            ;;
        *)
            fatal "Unsupported architecture: $MACHINE. Spinifex requires x86_64 or aarch64"
            ;;
    esac

    info "Detected architecture: $MACHINE ($ARCH)"
}

# --- Create per-service system users ---
create_service_users() {
    SPINIFEX_GROUP="spinifex"

    # Create shared group
    if ! getent group "$SPINIFEX_GROUP" > /dev/null 2>&1; then
        $SUDO groupadd --system "$SPINIFEX_GROUP"
    fi

    # Create per-service users with correct home directories
    declare -A SERVICE_HOMES=(
        [nats]="/var/lib/spinifex/nats"
        [gw]="/var/lib/spinifex/awsgw"
        [daemon]="/var/lib/spinifex/spinifex"
        [storage]="/var/lib/spinifex/predastore"
        [viperblock]="/var/lib/spinifex/viperblock"
        [vpcd]="/var/lib/spinifex"
        [ui]="/var/lib/spinifex"
    )
    for svc in nats gw daemon storage viperblock vpcd ui; do
        local user="spinifex-${svc}"
        if ! id "$user" > /dev/null 2>&1; then
            $SUDO useradd --system --no-create-home \
                --home-dir "${SERVICE_HOMES[$svc]}" \
                --gid "$SPINIFEX_GROUP" \
                --shell /usr/sbin/nologin \
                "$user"
        fi
    done

    # Add invoking user to spinifex group for admin CLI access
    ADMIN_USER="${SUDO_USER:-$(whoami)}"
    if [ "$ADMIN_USER" != "root" ]; then
        $SUDO usermod -aG "$SPINIFEX_GROUP" "$ADMIN_USER"
    fi

    # KVM access for daemon
    if getent group kvm > /dev/null 2>&1; then
        $SUDO usermod -aG kvm spinifex-daemon
    fi

    info "Service users created (spinifex-{nats,gw,daemon,storage,viperblock,vpcd,ui})"
}

# --- Install scoped sudoers rules ---
install_sudoers() {
    $SUDO tee /etc/sudoers.d/spinifex-network > /dev/null << 'SUDOERS'
# Spinifex daemon: tap devices, OVS bridge management, and DHCP for external IPs
spinifex-daemon ALL=(root) NOPASSWD: /sbin/ip, /usr/sbin/ip
spinifex-daemon ALL=(root) NOPASSWD: /usr/bin/ovs-vsctl, /usr/bin/ovs-appctl
spinifex-daemon ALL=(root) NOPASSWD: /usr/sbin/dhcpcd

# Spinifex VPC daemon: OVN and OVS read/write, OVN controller status check and DHCP
spinifex-vpcd ALL=(root) NOPASSWD: /usr/sbin/dhcpcd
spinifex-vpcd ALL=(root) NOPASSWD: /usr/bin/ovs-vsctl, /usr/bin/ovs-appctl
spinifex-vpcd ALL=(root) NOPASSWD: /usr/bin/ovn-nbctl, /usr/bin/ovn-sbctl
spinifex-vpcd ALL=(root) NOPASSWD: /usr/bin/systemctl is-active --quiet ovn-controller
SUDOERS
    $SUDO chmod 0440 /etc/sudoers.d/spinifex-network
    $SUDO visudo -cf /etc/sudoers.d/spinifex-network || fatal "Invalid sudoers syntax in spinifex-network"
    info "Scoped sudoers rules installed for spinifex-daemon and spinifex-vpcd"
}

# --- Install apt dependencies ---
install_apt_deps() {
    if [ "${INSTALL_SPINIFEX_SKIP_APT}" = "1" ]; then
        info "Skipping apt dependencies (INSTALL_SPINIFEX_SKIP_APT=1)"
        return
    fi

    info "Installing system dependencies..."
    $SUDO apt-get update -qq

    DEBIAN_FRONTEND=noninteractive $SUDO apt-get install -y -qq \
        nbdkit \
        $QEMU_PACKAGES qemu-utils qemu-kvm \
        libvirt-daemon-system libvirt-clients \
        jq curl iproute2 netcat-openbsd wget unzip xz-utils file \
        ovn-central ovn-host openvswitch-switch dhcpcd-base \
        > /dev/null

    info "System dependencies installed"
}

# --- Install AWS CLI ---
install_aws_cli() {
    if [ "${INSTALL_SPINIFEX_SKIP_AWS}" = "1" ]; then
        info "Skipping AWS CLI (INSTALL_SPINIFEX_SKIP_AWS=1)"
        return
    fi

    if command -v aws >/dev/null 2>&1; then
        info "AWS CLI already installed: $(aws --version 2>&1 | head -1)"
        return
    fi

    info "Installing AWS CLI v2..."
    AWS_TMPDIR=$(mktemp -d)
    curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-${AWS_ARCH}.zip" -o "$AWS_TMPDIR/awscliv2.zip"
    unzip -q "$AWS_TMPDIR/awscliv2.zip" -d "$AWS_TMPDIR"
    $SUDO "$AWS_TMPDIR/aws/install" --update > /dev/null
    rm -rf "$AWS_TMPDIR"

    info "AWS CLI installed: $(aws --version 2>&1 | head -1)"
}

# --- Download tarball ---
download_spinifex() {
    SPINIFEX_TMPDIR=$(mktemp -d)
    TARBALL="$SPINIFEX_TMPDIR/spinifex.tar.gz"

    # Local tarball override — skip download (for testing and air-gapped installs)
    if [ -n "$INSTALL_SPINIFEX_TARBALL" ]; then
        info "Using local tarball: $INSTALL_SPINIFEX_TARBALL"
        cp "$INSTALL_SPINIFEX_TARBALL" "$TARBALL"
        info "Extracting..."
        tar -xzf "$TARBALL" -C "$SPINIFEX_TMPDIR"
        EXTRACT_DIR="$SPINIFEX_TMPDIR"
        return
    fi

    if [ -n "$INSTALL_SPINIFEX_VERSION" ]; then
        DOWNLOAD_URL="${INSTALL_BASE_URL}/download/${INSTALL_SPINIFEX_VERSION}/${ARCH}"
        info "Downloading Spinifex $INSTALL_SPINIFEX_VERSION ($ARCH)..."
    else
        DOWNLOAD_URL="${INSTALL_BASE_URL}/download/${INSTALL_SPINIFEX_CHANNEL}/${ARCH}"
        info "Downloading Spinifex ($INSTALL_SPINIFEX_CHANNEL channel, $ARCH)..."
    fi

    HTTP_CODE=$(curl -fsSL -w '%{http_code}' -o "$TARBALL" "$DOWNLOAD_URL" 2>/dev/null) || true
    if [ ! -f "$TARBALL" ] || [ "$HTTP_CODE" -ge 400 ] 2>/dev/null; then
        rm -rf "$SPINIFEX_TMPDIR"
        fatal "Failed to download Spinifex from $DOWNLOAD_URL (HTTP $HTTP_CODE)"
    fi

    # Verify checksum if available
    CHECKSUM_URL="${DOWNLOAD_URL}.sha256"
    if curl -fsSL -o "$SPINIFEX_TMPDIR/checksum.sha256" "$CHECKSUM_URL" 2>/dev/null; then
        info "Verifying checksum..."
        EXPECTED=$(awk '{print $1}' "$SPINIFEX_TMPDIR/checksum.sha256")
        ACTUAL=$(sha256sum "$TARBALL" | awk '{print $1}')
        if [ "$EXPECTED" != "$ACTUAL" ]; then
            rm -rf "$SPINIFEX_TMPDIR"
            fatal "Checksum verification failed. Expected: $EXPECTED, Got: $ACTUAL"
        fi
        info "Checksum verified"
    else
        rm -rf "$SPINIFEX_TMPDIR"
        fatal "Checksum not available at $CHECKSUM_URL. Cannot verify download integrity."
    fi

    # Extract
    info "Extracting..."
    tar -xzf "$TARBALL" -C "$SPINIFEX_TMPDIR"
    EXTRACT_DIR="$SPINIFEX_TMPDIR"
}

# --- Place files ---
install_files() {
    info "Installing files..."

    # Binary
    $SUDO install -m 0755 "$EXTRACT_DIR/spx" /usr/local/bin/spx
    info "  /usr/local/bin/spx"

    # nbdkit plugin
    PLUGINDIR=$(nbdkit --dump-config 2>/dev/null | grep ^plugindir= | cut -d= -f2)
    if [ -z "$PLUGINDIR" ]; then
        warn "Could not detect nbdkit plugin directory, using default"
        if [ "$ARCH" = "arm64" ]; then
            PLUGINDIR="/usr/lib/aarch64-linux-gnu/nbdkit/plugins"
        else
            PLUGINDIR="/usr/lib/x86_64-linux-gnu/nbdkit/plugins"
        fi
    fi
    $SUDO mkdir -p "$PLUGINDIR"
    $SUDO install -m 0755 "$EXTRACT_DIR/nbdkit-viperblock-plugin.so" "$PLUGINDIR/nbdkit-viperblock-plugin.so"
    info "  $PLUGINDIR/nbdkit-viperblock-plugin.so"

    # Setup scripts
    $SUDO mkdir -p /usr/local/share/spinifex
    if [ -f "$EXTRACT_DIR/setup-ovn.sh" ]; then
        $SUDO install -m 0755 "$EXTRACT_DIR/setup-ovn.sh" /usr/local/share/spinifex/setup-ovn.sh
        info "  /usr/local/share/spinifex/setup-ovn.sh"
    fi

}

# --- Create directories ---
create_directories() {
    info "Creating directories..."

    # Top-level directories (root-owned, group-readable by spinifex)
    $SUDO mkdir -p /etc/spinifex
    $SUDO chmod 0750 /etc/spinifex
    $SUDO chown "root:$SPINIFEX_GROUP" /etc/spinifex

    $SUDO mkdir -p /var/lib/spinifex
    $SUDO chmod 0750 /var/lib/spinifex
    $SUDO chown "root:$SPINIFEX_GROUP" /var/lib/spinifex

    # Symlink so services that expect BaseDir/config/ can find /etc/spinifex/
    if [ ! -e /var/lib/spinifex/config ]; then
        $SUDO ln -s /etc/spinifex /var/lib/spinifex/config
    fi

    # Symlink so services that write logs to BaseDir/logs/ use /var/log/spinifex/
    if [ ! -e /var/lib/spinifex/logs ]; then
        $SUDO ln -s /var/log/spinifex /var/lib/spinifex/logs
    fi

    $SUDO mkdir -p /var/log/spinifex
    $SUDO chmod 0775 /var/log/spinifex
    $SUDO chown "root:$SPINIFEX_GROUP" /var/log/spinifex

    $SUDO mkdir -p /run/spinifex
    $SUDO chmod 0775 /run/spinifex
    $SUDO chown "root:$SPINIFEX_GROUP" /run/spinifex

    $SUDO mkdir -p /run/spinifex/nbd
    $SUDO chmod 0770 /run/spinifex/nbd
    $SUDO chown "root:$SPINIFEX_GROUP" /run/spinifex/nbd

    # Per-service config directories
    $SUDO mkdir -p /etc/spinifex/nats
    $SUDO chown "spinifex-nats:$SPINIFEX_GROUP" /etc/spinifex/nats
    $SUDO chmod 0750 /etc/spinifex/nats

    $SUDO mkdir -p /etc/spinifex/predastore
    $SUDO chown "spinifex-storage:$SPINIFEX_GROUP" /etc/spinifex/predastore
    $SUDO chmod 0750 /etc/spinifex/predastore

    $SUDO mkdir -p /etc/spinifex/awsgw
    $SUDO chown "spinifex-gw:$SPINIFEX_GROUP" /etc/spinifex/awsgw
    $SUDO chmod 0750 /etc/spinifex/awsgw

    # Per-service data directories
    $SUDO mkdir -p /var/lib/spinifex/nats
    $SUDO chown "spinifex-nats:$SPINIFEX_GROUP" /var/lib/spinifex/nats
    $SUDO chmod 0700 /var/lib/spinifex/nats

    $SUDO mkdir -p /var/lib/spinifex/spinifex
    $SUDO chown "spinifex-daemon:$SPINIFEX_GROUP" /var/lib/spinifex/spinifex
    $SUDO chmod 0700 /var/lib/spinifex/spinifex

    $SUDO mkdir -p /var/lib/spinifex/predastore
    $SUDO chown "spinifex-storage:$SPINIFEX_GROUP" /var/lib/spinifex/predastore
    $SUDO chmod 0700 /var/lib/spinifex/predastore

    $SUDO mkdir -p /var/lib/spinifex/viperblock
    $SUDO chown "spinifex-viperblock:$SPINIFEX_GROUP" /var/lib/spinifex/viperblock
    $SUDO chmod 0700 /var/lib/spinifex/viperblock

    $SUDO mkdir -p /var/lib/spinifex/vpcd
    $SUDO chown "spinifex-vpcd:$SPINIFEX_GROUP" /var/lib/spinifex/vpcd
    $SUDO chmod 0700 /var/lib/spinifex/vpcd

    $SUDO mkdir -p /var/lib/spinifex/awsgw
    $SUDO chown "spinifex-gw:$SPINIFEX_GROUP" /var/lib/spinifex/awsgw
    $SUDO chmod 0700 /var/lib/spinifex/awsgw

    # Symlink so awsgw's {BaseDir}/config/ paths resolve to /etc/spinifex/
    if [ ! -e /var/lib/spinifex/awsgw/config ]; then
        $SUDO ln -s /etc/spinifex /var/lib/spinifex/awsgw/config
    fi

    # Top-level config files (root-owned, group-readable)
    # Generate environment file with install-specific values (e.g. arch-dependent paths)
    $SUDO tee /etc/spinifex/systemd.env > /dev/null << EOF
# Generated by setup.sh — install-specific environment variables
SPINIFEX_VIPERBLOCK_PLUGIN_PATH=${PLUGINDIR}/nbdkit-viperblock-plugin.so
EOF
    $SUDO chown "spinifex-viperblock:$SPINIFEX_GROUP" /etc/spinifex/systemd.env
    $SUDO chmod 0640 /etc/spinifex/systemd.env
    info "Generated /etc/spinifex/systemd.env"

    # Service helper scripts (root-owned, group-executable by all service users)
    if [ -d "$EXTRACT_DIR/scripts" ]; then
        for script in "$EXTRACT_DIR"/scripts/*.sh; do
            $SUDO install -o root -g "$SPINIFEX_GROUP" -m 0755 \
                "$script" "/var/lib/spinifex/$(basename "$script")"
            info "  /var/lib/spinifex/$(basename "$script")"
        done
    fi
}

# --- Fix file ownership for upgrades from v1 ---
fix_file_ownership() {
    info "Fixing file ownership for privilege separation..."

    # Per-service data dirs — recursive chown so existing files are accessible
    for entry in \
        nats:spinifex-nats \
        predastore:spinifex-storage \
        spinifex:spinifex-daemon \
        viperblock:spinifex-viperblock \
        vpcd:spinifex-vpcd \
        awsgw:spinifex-gw; do
        IFS=: read -r dir svc_user <<< "$entry"
        if [ -d "/var/lib/spinifex/$dir" ]; then
            $SUDO chown -R "$svc_user:$SPINIFEX_GROUP" "/var/lib/spinifex/$dir" \
                || fatal "Failed to set ownership on /var/lib/spinifex/$dir"
        fi
    done

    # Per-service config dirs — recursive chown
    if [ -d /etc/spinifex/nats ]; then
        $SUDO chown -R "spinifex-nats:$SPINIFEX_GROUP" /etc/spinifex/nats \
            || fatal "Failed to set ownership on /etc/spinifex/nats"
    fi
    if [ -d /etc/spinifex/predastore ]; then
        $SUDO chown -R "spinifex-storage:$SPINIFEX_GROUP" /etc/spinifex/predastore \
            || fatal "Failed to set ownership on /etc/spinifex/predastore"
    fi
    if [ -d /etc/spinifex/awsgw ]; then
        $SUDO chown -R "spinifex-gw:$SPINIFEX_GROUP" /etc/spinifex/awsgw \
            || fatal "Failed to set ownership on /etc/spinifex/awsgw"
    fi

    # Shared config files — root:spinifex with per-file modes
    for f in spinifex.toml master.key server.key; do
        if [ -f "/etc/spinifex/$f" ]; then
            $SUDO chown "root:$SPINIFEX_GROUP" "/etc/spinifex/$f" \
                || fatal "Failed to set ownership on /etc/spinifex/$f"
            $SUDO chmod 0640 "/etc/spinifex/$f" \
                || fatal "Failed to set permissions on /etc/spinifex/$f"
        fi
    done
    for f in server.pem ca.pem; do
        if [ -f "/etc/spinifex/$f" ]; then
            $SUDO chown "root:$SPINIFEX_GROUP" "/etc/spinifex/$f" \
                || fatal "Failed to set ownership on /etc/spinifex/$f"
            $SUDO chmod 0644 "/etc/spinifex/$f" \
                || fatal "Failed to set permissions on /etc/spinifex/$f"
        fi
    done

    # CA private key — root-only
    if [ -f /etc/spinifex/ca.key ]; then
        $SUDO chown root:root /etc/spinifex/ca.key \
            || fatal "Failed to set ownership on /etc/spinifex/ca.key"
        $SUDO chmod 0600 /etc/spinifex/ca.key \
            || fatal "Failed to set permissions on /etc/spinifex/ca.key"
    fi

    # Shared data dirs — root:spinifex 0770 (daemon + admin CLI write, services read).
    # chmod must be recursive so pre-existing files (e.g. imported images originally
    # written as 0600 by another user) become group-readable.
    for d in images amis volumes state; do
        if [ -d "/var/lib/spinifex/$d" ]; then
            $SUDO chown -R "root:$SPINIFEX_GROUP" "/var/lib/spinifex/$d" \
                || fatal "Failed to set ownership on /var/lib/spinifex/$d"
            $SUDO chmod -R u+rwX,g+rwX,o-rwx "/var/lib/spinifex/$d" \
                || fatal "Failed to set permissions on /var/lib/spinifex/$d"
        fi
    done

    info "File ownership updated"
}

# --- Run config migrations ---
run_migrations() {
    # Only run if spinifex is already initialized (config exists)
    if [ ! -f /etc/spinifex/spinifex.toml ]; then
        info "Fresh install detected, skipping migrations"
        return
    fi

    if [ "${INSTALL_SPINIFEX_SKIP_MIGRATE:-0}" = "1" ]; then
        info "INSTALL_SPINIFEX_SKIP_MIGRATE=1, skipping migrations"
        info "Run 'spx admin upgrade' manually to apply pending migrations"
        return
    fi

    info "Running config migrations..."
    $SUDO /usr/local/bin/spx admin upgrade --yes \
        || fatal "Config migration failed. See errors above."
}

# --- Install systemd units ---
install_systemd() {
    info "Installing systemd units..."

    if [ ! -d "$EXTRACT_DIR/systemd" ]; then
        fatal "Systemd unit files not found in tarball (expected systemd/ directory)"
    fi

    for unit in "$EXTRACT_DIR"/systemd/*; do
        $SUDO install -m 0644 "$unit" "/etc/systemd/system/$(basename "$unit")"
        info "  /etc/systemd/system/$(basename "$unit")"
    done

    $SUDO systemctl daemon-reload
    $SUDO systemctl enable spinifex.target
    info "Systemd units installed and enabled (per-service users)"
}

# --- Install logrotate ---
install_logrotate() {
    if [ -f "$EXTRACT_DIR/logrotate-spinifex" ]; then
        $SUDO install -m 0644 "$EXTRACT_DIR/logrotate-spinifex" /etc/logrotate.d/spinifex
    else
        warn "Logrotate config not found in tarball, skipping"
        return
    fi
    info "Logrotate config installed"
}

# --- Upgrade handling ---
handle_upgrade() {
    if $SUDO systemctl is-active --quiet spinifex.target 2>/dev/null; then
        warn "Spinifex services are running. Stopping for upgrade..."
        $SUDO systemctl stop spinifex.target
        RESTART_AFTER=true
    fi
}

restart_if_needed() {
    if [ "${RESTART_AFTER}" = "true" ]; then
        info "Restarting Spinifex services..."
        $SUDO systemctl start spinifex.target
    fi
}

# --- Print summary ---
print_summary() {
    INSTALLED_VERSION=$(/usr/local/bin/spx version 2>/dev/null || echo "unknown")

    echo ""
    echo "============================================"
    echo "  Spinifex installed successfully"
    echo "============================================"
    echo ""
    echo "  Version:      $INSTALLED_VERSION"
    echo "  Architecture: $ARCH"
    echo "  Service users: spinifex-{nats,gw,daemon,storage,viperblock,vpcd,ui}"
    echo "  Binary:       /usr/local/bin/spx"
    echo "  Config:       /etc/spinifex/"
    echo "  Data:         /var/lib/spinifex/"
    echo "  Logs:         /var/log/spinifex/"
    echo ""
    echo "  Next steps:"
    echo ""
    echo "  1. Setup OVN networking:"
    echo "     If your WAN interface is already a bridge (e.g. br-wan):"
    echo "       sudo /usr/local/share/spinifex/setup-ovn.sh --management"
    echo ""
    echo "     If your WAN is a physical NIC:"
    echo "       # Dedicated WAN NIC (not your SSH connection):"
    echo "       sudo /usr/local/share/spinifex/setup-ovn.sh --management --wan-bridge=br-wan --wan-iface=eth1"
    echo ""
    echo "  2. Initialize:"
    echo "     sudo spx admin init --node node1 --nodes 1"
    echo ""
    echo "  3. Start services:"
    echo "     sudo systemctl start spinifex.target"
    echo ""
    echo "  4. Verify:"
    echo "     export AWS_PROFILE=spinifex"
    echo "     aws ec2 describe-instance-types"
    echo ""
}

# --- Main ---
main() {
    info "Spinifex installer"
    echo ""

    setup_sudo
    detect_os
    detect_arch
    handle_upgrade
    install_apt_deps
    install_aws_cli
    create_service_users
    install_sudoers
    download_spinifex
    install_files
    create_directories
    fix_file_ownership
    install_systemd
    run_migrations
    install_logrotate
    rm -rf "$SPINIFEX_TMPDIR"
    restart_if_needed
    print_summary

    # Activate spinifex group membership in the invoking shell. Under curl|bash
    # stdin is the drained pipe, so redirect from /dev/tty and exec so the new
    # shell becomes the foreground process. Skip when we can't actually open
    # /dev/tty (CI, cloud-init, ssh -T — stat passes but open fails with ENXIO).
    if [ "${INSTALL_SPINIFEX_SKIP_NEWGRP:-0}" != "1" ] \
        && ! id -Gn 2>/dev/null | grep -qw "$SPINIFEX_GROUP" \
        && ( : </dev/tty ) 2>/dev/null; then
        echo ""
        echo "  Activating '$SPINIFEX_GROUP' group in a subshell — type 'exit' when done."
        echo ""
        exec newgrp "$SPINIFEX_GROUP" < /dev/tty
    fi
}

main "$@"
