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
#   ISO_BUILD                  Set to 1 when running inside a debootstrap chroot from the ISO
#                              builder: skip handle_upgrade/restart/migrations/newgrp/print_summary,
#                              skip systemctl daemon-reload + enable, short-circuit setup_sudo.
#   VERBOSE                    Set to 1 to echo "[setup] <stage>" before each top-level step.
#   SETUP_STAGES               Comma-separated subset of stages to run:
#                                deps, aws, users, sudoers, files, directories,
#                                env, systemd, logrotate, udev, fixown, resolved,
#                                migrations
#                              Unset = run every stage appropriate for the current mode.

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

stage() {
    [ "${VERBOSE:-0}" = "1" ] && echo "[setup] $*"
    return 0
}

stage_enabled() {
    [ -z "${SETUP_STAGES:-}" ] && return 0
    case ",${SETUP_STAGES}," in
        *",$1,"*) return 0 ;;
        *) return 1 ;;
    esac
}

# --- Sudo setup ---
setup_sudo() {
    # Inside a debootstrap chroot we are already root and sudo may not be installed.
    if [ "${ISO_BUILD:-0}" = "1" ]; then
        SUDO=""
        return
    fi
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
    stage "creating spinifex group and per-service users"
    SPINIFEX_GROUP="spinifex"

    # Create shared group
    if ! getent group "$SPINIFEX_GROUP" > /dev/null 2>&1; then
        $SUDO groupadd --system "$SPINIFEX_GROUP"
    fi

    # Producer-typed access group for viperblock's runtime resources
    # (/run/spinifex/nbd socket dir). Viperblock writes as owner; daemon reads
    # via supplementary membership; every other spinifex-* service is excluded.
    if ! getent group spinifex-viperblock > /dev/null 2>&1; then
        $SUDO groupadd --system spinifex-viperblock
    fi

    # Create per-service users with correct home directories
    declare -A SERVICE_HOMES=(
        [nats]="/var/lib/spinifex/nats"
        [gw]="/var/lib/spinifex/awsgw"
        [daemon]="/var/lib/spinifex/spinifex"
        [storage]="/var/lib/spinifex/predastore"
        [northstar]="/var/lib/spinifex/northstar"
        [viperblock]="/var/lib/spinifex/viperblock"
        [vpcd]="/var/lib/spinifex"
        [ui]="/var/lib/spinifex"
    )
    for svc in nats gw daemon storage northstar viperblock vpcd ui; do
        local user="spinifex-${svc}"
        if ! id "$user" > /dev/null 2>&1; then
            $SUDO useradd --system --no-create-home \
                --home-dir "${SERVICE_HOMES[$svc]}" \
                --gid "$SPINIFEX_GROUP" \
                --shell /usr/sbin/nologin \
                "$user"
        fi
    done

    # Add invoking user to spinifex group for admin CLI access.
    # Skip under ISO_BUILD: the chroot has no invoking user (tf-user,
    # whoever ran `sudo make`, etc. don't exist in the rootfs). The ISO's
    # interactive 'spinifex' login account is created later in Phase 4 of
    # build-rootfs.sh with spinifex as its primary gid. In curl|bash mode
    # guard against a stale/missing SUDO_USER too.
    if [ "${ISO_BUILD:-0}" != "1" ]; then
        ADMIN_USER="${SUDO_USER:-$(whoami)}"
        if [ "$ADMIN_USER" != "root" ] && id -u "$ADMIN_USER" > /dev/null 2>&1; then
            $SUDO usermod -aG "$SPINIFEX_GROUP" "$ADMIN_USER"
        fi
    fi

    # KVM access for daemon
    if getent group kvm > /dev/null 2>&1; then
        $SUDO usermod -aG kvm spinifex-daemon
    fi

    # Daemon consumes viperblock's NBD socket — join the producer-typed group.
    $SUDO usermod -aG spinifex-viperblock spinifex-daemon

    info "Service users created (spinifex-{nats,gw,daemon,storage,northstar,viperblock,vpcd,ui})"
}

# --- Install scoped sudoers rules ---
install_sudoers() {
    stage "installing scoped sudoers rules"
    $SUDO tee /etc/sudoers.d/spinifex-network > /dev/null << 'SUDOERS'
# Spinifex daemon: tap devices, OVS bridge management, and DHCP for external IPs
# ovs-ofctl installs the per-tap IMDS datapath flows on br-imds; sysctl sets the
# per-endpoint rp_filter/accept_local the asymmetric reply path needs.
spinifex-daemon ALL=(root) NOPASSWD: /sbin/ip, /usr/sbin/ip
spinifex-daemon ALL=(root) NOPASSWD: /usr/bin/ovs-vsctl, /usr/bin/ovs-appctl, /usr/bin/ovs-ofctl
spinifex-daemon ALL=(root) NOPASSWD: /usr/sbin/sysctl -qw net.ipv4.conf.*
spinifex-daemon ALL=(root) NOPASSWD: /usr/sbin/dhcpcd
spinifex-daemon ALL=(root) NOPASSWD: /usr/bin/systemctl is-active openvswitch-ipsec.service
spinifex-daemon ALL=(root) NOPASSWD: /usr/bin/ovn-nbctl set NB_Global . ipsec=true

# Spinifex VPC daemon: OVN and OVS read/write, OVN controller status check and DHCP
spinifex-vpcd ALL=(root) NOPASSWD: /usr/sbin/dhcpcd
spinifex-vpcd ALL=(root) NOPASSWD: /usr/bin/ovs-vsctl, /usr/bin/ovs-appctl
spinifex-vpcd ALL=(root) NOPASSWD: /usr/bin/ovn-nbctl, /usr/bin/ovn-sbctl, /usr/bin/ovn-appctl
spinifex-vpcd ALL=(root) NOPASSWD: /usr/bin/systemctl is-active --quiet ovn-controller
spinifex-vpcd ALL=(root) NOPASSWD: /sbin/ip, /usr/sbin/ip
# Routed-NAT mode: transit masquerade + per-EIP FORWARD accepts, proxy-ARP
# delay tune, and gratuitous ARP announcements for host-delivered EIPs.
spinifex-vpcd ALL=(root) NOPASSWD: /usr/sbin/iptables, /sbin/iptables
spinifex-vpcd ALL=(root) NOPASSWD: /usr/sbin/sysctl -w net.ipv4.neigh.*
spinifex-vpcd ALL=(root) NOPASSWD: /usr/sbin/arping, /usr/bin/arping
SUDOERS
    $SUDO chmod 0440 /etc/sudoers.d/spinifex-network
    $SUDO visudo -cf /etc/sudoers.d/spinifex-network || fatal "Invalid sudoers syntax in spinifex-network"
    info "Scoped sudoers rules installed for spinifex-daemon and spinifex-vpcd"
}

# --- Install apt dependencies ---
# NOTE: Runtime deps must stay in sync with the ISO package list at
# scripts/iso-builder/build/packages.list in the mulga repo. When you add,
# remove, or change a runtime package here, review that file too — drift means
# the ISO and `curl | bash` paths install different software.
install_apt_deps() {
    stage "installing apt dependencies"
    if [ "${INSTALL_SPINIFEX_SKIP_APT}" = "1" ]; then
        info "Skipping apt dependencies (INSTALL_SPINIFEX_SKIP_APT=1)"
    else
        info "Installing system dependencies..."
        $SUDO apt-get update -qq

        DEBIAN_FRONTEND=noninteractive $SUDO apt-get install -y -qq \
            nbdkit \
            $QEMU_PACKAGES qemu-utils gdisk qemu-kvm ovmf qemu-efi-aarch64 less \
            libvirt-daemon-system libvirt-clients \
            pciutils \
            jq curl iproute2 netcat-openbsd wget unzip xz-utils file \
            ovn-central ovn-host openvswitch-switch openvswitch-ipsec strongswan-charon dhcpcd-base \
            chrony \
            > /dev/null

        info "System dependencies installed"
    fi

    # Mask the standalone dhcpcd.service auto-enabled on Debian Trixie when
    # dhcpcd-base is present. It binds br-wan and competes with vpcd's
    # nclient4 for OFFERs, draining the upstream pool and causing
    # intermittent DORA failures. Must run even when apt is skipped (CI
    # bootstrap runs with INSTALL_SPINIFEX_SKIP_APT=1 against runners that
    # already have dhcpcd-base preinstalled). The ISO installer does the
    # same mask (cmd/installer/install/install.go).
    $SUDO systemctl disable --now dhcpcd.service 2>/dev/null || true
    $SUDO systemctl mask dhcpcd.service 2>/dev/null || true
}

# --- Install AWS CLI ---
install_aws_cli() {
    stage "installing AWS CLI v2"
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
    stage "downloading/extracting spinifex release tarball"
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
    stage "installing binaries and scripts"
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

    # Install setup.sh itself so firstboot and future re-runs can find it at a
    # stable path on both ISO and curl|bash installs.
    if [ -f "$EXTRACT_DIR/setup.sh" ]; then
        $SUDO install -m 0755 "$EXTRACT_DIR/setup.sh" /usr/local/share/spinifex/setup.sh
        info "  /usr/local/share/spinifex/setup.sh"
    fi

    # microVM kernel + initramfs
    $SUDO install -d /usr/share/spinifex/microvm
    if [ -d "$EXTRACT_DIR/microvm" ]; then
        $SUDO cp "$EXTRACT_DIR/microvm/"* /usr/share/spinifex/microvm/
        info "  /usr/share/spinifex/microvm/*"
    fi
}

# --- Create directories ---
create_directories() {
    stage "creating spinifex directory layout"
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

    # /run/spinifex and /run/spinifex/nbd are declared via tmpfiles.d because
    # /run is tmpfs — direct mkdir doesn't survive reboot, and units have
    # ReadWritePaths= on these paths which fails namespace setup with ENOENT
    # if the dirs are absent at service start.
    _tmpf=$(mktemp)
    cat > "$_tmpf" <<'TMPEOF'
# Type  Path               Mode  User                 Group                Age
d       /run/spinifex      0770  root                 spinifex             -
d       /run/spinifex/nbd  0770  spinifex-viperblock  spinifex-viperblock  -
TMPEOF
    $SUDO install -m 0644 "$_tmpf" /etc/tmpfiles.d/spinifex.conf
    rm -f "$_tmpf"
    if [ "${ISO_BUILD:-0}" != "1" ]; then
        # Live systems: materialise the runtime dirs immediately so a re-run of
        # setup.sh on an existing host has correct /run state without a reboot.
        $SUDO systemd-tmpfiles --create /etc/tmpfiles.d/spinifex.conf 2>/dev/null || true
    fi

    # Per-service config directories
    $SUDO mkdir -p /etc/spinifex/nats
    $SUDO chown "spinifex-nats:$SPINIFEX_GROUP" /etc/spinifex/nats
    $SUDO chmod 0750 /etc/spinifex/nats

    $SUDO mkdir -p /etc/spinifex/predastore
    $SUDO chown "spinifex-storage:$SPINIFEX_GROUP" /etc/spinifex/predastore
    $SUDO chmod 0750 /etc/spinifex/predastore

    # Northstar holds northstar.toml (bucket-scoped S3 creds, written 0600 by
    # `spx admin init`); fix_file_ownership reassigns it to spinifex-northstar.
    $SUDO mkdir -p /etc/spinifex/northstar
    $SUDO chown "spinifex-northstar:$SPINIFEX_GROUP" /etc/spinifex/northstar
    $SUDO chmod 0750 /etc/spinifex/northstar

    $SUDO mkdir -p /etc/spinifex/awsgw
    $SUDO chown "spinifex-gw:$SPINIFEX_GROUP" /etc/spinifex/awsgw
    $SUDO chmod 0750 /etc/spinifex/awsgw

    # Viperblock's at-rest encryption key dir. 0750 group-traversable; the key
    # itself is set to root:spinifex 0640 by SetServiceOwnership because both
    # viperblockd (spinifex-viperblock) and the awsgw handlers (spinifex-gw)
    # load it.
    $SUDO mkdir -p /etc/spinifex/viperblock
    $SUDO chown "spinifex-viperblock:$SPINIFEX_GROUP" /etc/spinifex/viperblock
    $SUDO chmod 0750 /etc/spinifex/viperblock

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

    $SUDO mkdir -p /var/lib/spinifex/northstar
    $SUDO chown "spinifex-northstar:$SPINIFEX_GROUP" /var/lib/spinifex/northstar
    $SUDO chmod 0700 /var/lib/spinifex/northstar

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

    # Service helper scripts (root-owned, group-executable by all service users)
    if [ -d "$EXTRACT_DIR/scripts" ]; then
        for script in "$EXTRACT_DIR"/scripts/*.sh; do
            $SUDO install -o root -g "$SPINIFEX_GROUP" -m 0755 \
                "$script" "/var/lib/spinifex/$(basename "$script")"
            info "  /var/lib/spinifex/$(basename "$script")"
        done
    fi
}

# --- Generate systemd environment file ---
# Split out of create_directories so the `env` stage can be refreshed
# independently (e.g. by inject-bins.sh when the plugin path changes).
install_systemd_env() {
    stage "generating /etc/spinifex/systemd.env"

    # nbdkit plugin path is arch-dependent (`nbdkit --dump-config` returns
    # /usr/lib/{x86_64,aarch64}-linux-gnu/nbdkit/plugins). Resolve it here so
    # systemd.env always matches wherever install_files placed the .so.
    local plugindir
    plugindir=$(nbdkit --dump-config 2>/dev/null | grep ^plugindir= | cut -d= -f2)
    if [ -z "$plugindir" ]; then
        if [ "${ARCH:-}" = "arm64" ]; then
            plugindir="/usr/lib/aarch64-linux-gnu/nbdkit/plugins"
        else
            plugindir="/usr/lib/x86_64-linux-gnu/nbdkit/plugins"
        fi
    fi

    $SUDO mkdir -p /etc/spinifex
    $SUDO tee /etc/spinifex/systemd.env > /dev/null << EOF
# Generated by setup.sh — install-specific environment variables
SPINIFEX_VIPERBLOCK_PLUGIN_PATH=${plugindir}/nbdkit-viperblock-plugin.so
EOF
    $SUDO chown "spinifex-viperblock:${SPINIFEX_GROUP:-spinifex}" /etc/spinifex/systemd.env
    $SUDO chmod 0640 /etc/spinifex/systemd.env
    info "Generated /etc/spinifex/systemd.env"
}

# --- Fix file ownership for upgrades from v1 ---
# Also invoked from firstboot via SETUP_STAGES=fixown to correct ownership of
# files that `spx admin init` wrote as root:root.
fix_file_ownership() {
    stage "fixing file ownership for privilege separation"
    # create_service_users normally sets this; if we're invoked via
    # SETUP_STAGES=fixown on a host that already has the group, users isn't
    # run and SPINIFEX_GROUP is unset — default it here.
    SPINIFEX_GROUP="${SPINIFEX_GROUP:-spinifex}"
    info "Fixing file ownership for privilege separation..."

    # Per-service data dirs — recursive chown so existing files are accessible
    for entry in \
        nats:spinifex-nats \
        predastore:spinifex-storage \
        northstar:spinifex-northstar \
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
    if [ -d /etc/spinifex/northstar ]; then
        $SUDO chown -R "spinifex-northstar:$SPINIFEX_GROUP" /etc/spinifex/northstar \
            || fatal "Failed to set ownership on /etc/spinifex/northstar"
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
    stage "installing systemd units"
    info "Installing systemd units..."

    if [ ! -d "$EXTRACT_DIR/systemd" ]; then
        fatal "Systemd unit files not found in tarball (expected systemd/ directory)"
    fi

    for unit in "$EXTRACT_DIR"/systemd/*; do
        $SUDO install -m 0644 "$unit" "/etc/systemd/system/$(basename "$unit")"
        info "  /etc/systemd/system/$(basename "$unit")"
    done

    # Reserve RAM + CPU priority for system.slice (sshd, journald, the operator)
    # so a maxed spinifex.slice cannot starve them — the "stay sshable" guarantee.
    # Generated here rather than shipped as a staged file because the packaging
    # globs flatten the systemd/ dir and would skip a nested drop-in directory;
    # this mirrors the sshd-keygen drop-in pattern in build-rootfs.sh. MemoryMin
    # is a guaranteed-unreclaimable floor, not a cap.
    $SUDO mkdir -p /etc/systemd/system/system.slice.d
    $SUDO tee /etc/systemd/system/system.slice.d/spinifex-reserve.conf > /dev/null << 'EOF'
[Slice]
MemoryMin=1G
CPUWeight=300
EOF
    info "  /etc/systemd/system/system.slice.d/spinifex-reserve.conf"

    # daemon-reload / enable require a running systemd — skip inside the ISO
    # chroot. Unit files are still dropped into place; firstboot enables them.
    if [ "${ISO_BUILD:-0}" = "1" ]; then
        info "Systemd units installed (ISO_BUILD=1, skipping daemon-reload/enable)"
        return
    fi
    $SUDO systemctl daemon-reload
    $SUDO systemctl enable spinifex.target
    info "Systemd units installed and enabled (per-service users)"
}

# --- Install logrotate ---
install_logrotate() {
    stage "installing logrotate config"
    if [ -f "$EXTRACT_DIR/logrotate-spinifex" ]; then
        $SUDO install -m 0644 "$EXTRACT_DIR/logrotate-spinifex" /etc/logrotate.d/spinifex
    else
        warn "Logrotate config not found in tarball, skipping"
        return
    fi
    info "Logrotate config installed"
}

# --- Install udev rules ---
install_udev() {
    stage "installing udev rules"
    if [ ! -d "$EXTRACT_DIR/udev" ]; then
        return
    fi
    $SUDO install -d /etc/udev/rules.d
    for rule in "$EXTRACT_DIR"/udev/*; do
        $SUDO install -m 0644 "$rule" "/etc/udev/rules.d/$(basename "$rule")"
        info "  /etc/udev/rules.d/$(basename "$rule")"
    done
    if [ "${ISO_BUILD:-0}" != "1" ]; then
        $SUDO udevadm control --reload-rules
        $SUDO udevadm trigger --subsystem-match=vfio 2>/dev/null || true
    fi
    info "udev rules installed"
}

# --- Upgrade handling ---
handle_upgrade() {
    if $SUDO systemctl is-active --quiet spinifex.target 2>/dev/null; then
        warn "Spinifex services are running. Stopping for upgrade..."
        $SUDO systemctl stop spinifex.target
        RESTART_AFTER=true
    fi

    # Pre-tightening hosts have /run/spinifex/nbd as root:spinifex 0770, which
    # grants access to every spinifex-* service. While services are stopped,
    # bring it up to the new spinifex-viperblock:spinifex-viperblock 0770
    # policy so restart picks up the fresh group membership without a reboot.
    if [ -d /run/spinifex/nbd ]; then
        $SUDO chown spinifex-viperblock:spinifex-viperblock /run/spinifex/nbd 2>/dev/null || true
        $SUDO chmod 0770 /run/spinifex/nbd 2>/dev/null || true
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

# --- Configure host swap (mulga-siv-251) ---
# Provisions an 8G swapfile and lowers vm.swappiness so spinifex.slice
# (MemorySwapMax=100%) has a backing device for graceful degradation under
# memory pressure. Reverses the historical swap=0 assumption. Idempotent.
setup_swap() {
    stage "configuring host swap"

    # ISO build runs in a chroot — cannot swapon, and baking an 8G file into the
    # rootfs bloats the image. ISO hosts provision swap at firstboot instead.
    if [ "${ISO_BUILD:-0}" = "1" ]; then
        info "Swap setup skipped (ISO_BUILD=1; firstboot provisions swap)"
        return
    fi

    local swapfile=/swapfile size_mb=8192

    # Swap is a safety buffer, not a routine path: reclaim page cache first.
    $SUDO tee /etc/sysctl.d/99-spinifex-swap.conf > /dev/null << 'EOF'
# Spinifex: minimise swapping; swap backs spinifex.slice graceful degradation.
vm.swappiness = 10
EOF
    $SUDO chmod 0644 /etc/sysctl.d/99-spinifex-swap.conf
    $SUDO sysctl -q -w vm.swappiness=10 || true

    if swapon --show=NAME --noheadings 2>/dev/null | grep -qx "$swapfile"; then
        info "Swap already active ($swapfile)"
        return
    fi

    if [ ! -f "$swapfile" ]; then
        info "Creating ${size_mb}MiB $swapfile"
        $SUDO fallocate -l "${size_mb}M" "$swapfile" 2>/dev/null \
            || $SUDO dd if=/dev/zero of="$swapfile" bs=1M count="$size_mb" status=none
        $SUDO chmod 0600 "$swapfile"
        $SUDO mkswap "$swapfile" > /dev/null
    fi
    $SUDO swapon "$swapfile"
    grep -q "^$swapfile " /etc/fstab 2>/dev/null \
        || echo "$swapfile none swap sw 0 0" | $SUDO tee -a /etc/fstab > /dev/null
    info "Swap enabled: $swapfile (${size_mb}MiB), vm.swappiness=10"
}

# --- Parse the northstar.toml :53 listener host ---
# northstar.toml's `listen` is a comma-separated host:port list (e.g.
# "0.0.0.0:5300,10.0.0.5:53"). The ":53" entry is the address Northstar actually
# binds for the authoritative service — the rendered AdvertiseIP — so host DNS
# reads it here instead of independently re-detecting the WAN IP. Prints the host
# and returns 0 on success, or returns 1 when no ":53" endpoint is present.
northstar_listen_ip() {
    local toml="$1" listen_val entry
    listen_val=$($SUDO grep -E '^[[:space:]]*listen[[:space:]]*=' "$toml" 2>/dev/null \
        | head -1 | sed -E 's/.*=[[:space:]]*"([^"]*)".*/\1/' | tr -d '[:space:]')
    local IFS=','
    for entry in $listen_val; do
        case "$entry" in
            *:53) printf '%s' "${entry%:53}"; return 0 ;;
        esac
    done
    return 1
}

# --- Read a quoted string value from a TOML file ---
# Matches `key = "value"`, tolerating leading whitespace and trailing comments.
northstar_toml_string() {
    local key="$1" toml="$2"
    $SUDO grep -E "^[[:space:]]*$key[[:space:]]*=" "$toml" 2>/dev/null \
        | head -1 | sed -E 's/.*=[[:space:]]*"?([^"#]*[^"# ])"?.*/\1/'
}

# --- Confirm systemd-resolved is serving the Spinifex zones via Northstar ---
# `resolvectl status` reports the effective global DNS server and routing domains
# once the drop-in is loaded, so this catches a restart that silently failed to
# apply the config. Returns non-zero when any expected value is missing.
resolved_state_current() {
    local ns_ip="$1" base_domain="$2" internal_domain="$3" status
    status=$($SUDO resolvectl status 2>/dev/null) || return 1
    printf '%s\n' "$status" | grep -qF "$ns_ip" || return 1
    printf '%s\n' "$status" | grep -qF "$base_domain" || return 1
    printf '%s\n' "$status" | grep -qF "$internal_domain" || return 1
    return 0
}

# --- Confirm resolvconf placed Northstar first in the generated resolv.conf ---
# Local-first ordering means Northstar must be the first `nameserver` line; the
# retained upstream fallbacks follow it. Returns non-zero when Northstar is not
# first or the Spinifex search domain is missing.
resolvconf_state_current() {
    local ns_ip="$1" base_domain="$2" resolv_conf="$3" first
    $SUDO test -f "$resolv_conf" || return 1
    first=$($SUDO grep -E '^[[:space:]]*nameserver[[:space:]]+' "$resolv_conf" 2>/dev/null \
        | head -1 | awk '{print $2}')
    [ "$first" = "$ns_ip" ] || return 1
    $SUDO grep -qF "$base_domain" "$resolv_conf" || return 1
    return 0
}

# --- Route the Spinifex zones through systemd-resolved ---
# The "~" routing-domain prefix restricts the DNS= server to exactly the Spinifex
# zones (resolved.conf(5)); every other name keeps using the link/DHCP DNS.
# Idempotent, and verifies the running resolver before reporting success.
setup_host_dns_resolved() {
    local ns_ip="$1" base_domain="$2" internal_domain="$3"
    local dropin_dir="${RESOLVED_DROPIN_DIR:-/etc/systemd/resolved.conf.d}"
    local dropin="$dropin_dir/spinifex-dns.conf"

    local tmp
    tmp=$(mktemp)
    cat > "$tmp" << EOF
# Generated by setup.sh — route the Spinifex authoritative zones to the local
# Northstar resolver. The "~" routing-domain prefix restricts this DNS= server to
# exactly these zones; all other names use the link DNS. The address is the ":53"
# listener from northstar.toml (the rendered AdvertiseIP).
[Resolve]
DNS=${ns_ip}
Domains=~${base_domain} ~${internal_domain}
EOF

    # Skip the restart when the drop-in is unchanged and the resolver already
    # reflects it (idempotent re-runs).
    if $SUDO test -f "$dropin" && $SUDO cmp -s "$tmp" "$dropin" \
        && resolved_state_current "$ns_ip" "$base_domain" "$internal_domain"; then
        rm -f "$tmp"
        info "Host DNS already current: ${base_domain} + ${internal_domain} -> ${ns_ip}:53 (systemd-resolved)"
        return
    fi

    $SUDO mkdir -p "$dropin_dir"
    $SUDO install -m 0644 "$tmp" "$dropin"
    rm -f "$tmp"
    $SUDO systemctl restart systemd-resolved \
        || fatal "systemd-resolved restart failed — host DNS not applied"

    resolved_state_current "$ns_ip" "$base_domain" "$internal_domain" \
        || fatal "systemd-resolved did not apply the Spinifex zones (${base_domain}, ${internal_domain} -> ${ns_ip})"
    info "Host DNS: ${base_domain} + ${internal_domain} -> ${ns_ip}:53 (systemd-resolved)"
}

# --- Make Northstar the first resolver via resolvconf ---
# resolvconf assembles /etc/resolv.conf head-first, so writing Northstar to the
# head fragment puts it ahead of the retained upstream fallbacks (resolv.conf.d/
# base and any DHCP-supplied servers). Northstar forwards or REFUSEs
# non-authoritative names, so glibc falls through to those fallbacks. Idempotent,
# and verifies the regenerated resolv.conf before reporting success.
setup_host_dns_resolvconf() {
    local ns_ip="$1" base_domain="$2" internal_domain="$3"
    local head="${RESOLVCONF_HEAD:-/etc/resolvconf/resolv.conf.d/head}"
    local resolv_conf="${RESOLV_CONF:-/etc/resolv.conf}"

    local tmp
    tmp=$(mktemp)
    cat > "$tmp" << EOF
# Generated by setup.sh — Northstar is the first host resolver so the Spinifex
# zones resolve locally. The retained upstream nameservers (resolv.conf.d/base
# and DHCP) stay in place as fallbacks for names Northstar forwards or REFUSEs.
nameserver ${ns_ip}
search ${base_domain} ${internal_domain}
EOF

    if $SUDO test -f "$head" && $SUDO cmp -s "$tmp" "$head" \
        && resolvconf_state_current "$ns_ip" "$base_domain" "$resolv_conf"; then
        rm -f "$tmp"
        info "Host DNS already current: Northstar ${ns_ip} first in ${resolv_conf} (resolvconf)"
        return
    fi

    $SUDO mkdir -p "$(dirname "$head")"
    $SUDO install -m 0644 "$tmp" "$head"
    rm -f "$tmp"
    $SUDO resolvconf -u || fatal "resolvconf -u failed — host DNS not applied"

    resolvconf_state_current "$ns_ip" "$base_domain" "$resolv_conf" \
        || fatal "resolvconf did not place Northstar (${ns_ip}) first in ${resolv_conf}"
    info "Host DNS: Northstar ${ns_ip} first in ${resolv_conf}, upstream retained (resolvconf)"
}

# --- Configure host DNS to reach the local Northstar resolver ---
# Points the host resolver at the node-local Northstar listener so `kubectl`, the
# AWS SDK and other host tooling can resolve the Spinifex authoritative zones. The
# listener address and zones come from the rendered northstar.toml, so this stage
# requires it — without the config (pre-init, or controller-owned formation) host
# DNS is deferred until it exists. systemd-resolved gets route-only zones;
# resolvconf hosts get Northstar as the first resolver. Failures are surfaced,
# not suppressed.
setup_host_dns() {
    stage "configuring host DNS for the Spinifex zones"

    # ISO chroot has no live resolver to reconfigure; firstboot runs this stage
    # after formation writes northstar.toml.
    if [ "${ISO_BUILD:-0}" = "1" ]; then
        info "Host DNS skipped (ISO_BUILD=1; firstboot configures it after formation)"
        return
    fi

    local ns_toml="${NORTHSTAR_TOML:-/etc/spinifex/northstar/northstar.toml}"
    if ! $SUDO test -f "$ns_toml"; then
        info "northstar.toml not found — deferring host DNS until after cluster formation"
        info "  Re-run once Northstar is configured: sudo SETUP_STAGES=resolved $0"
        return
    fi

    # The ":53" listener is the address Northstar binds (the rendered
    # AdvertiseIP); use it rather than re-detecting the WAN IP.
    local ns_ip
    ns_ip=$(northstar_listen_ip "$ns_toml") \
        || fatal "Could not parse a :53 listener from $ns_toml — host DNS not configured"

    # Zones Northstar is authoritative for; fall back to the `spx admin init`
    # defaults if the keys are somehow absent.
    local base_domain internal_domain
    base_domain=$(northstar_toml_string default_domain "$ns_toml")
    internal_domain=$(northstar_toml_string internal_domain "$ns_toml")
    [ -z "$base_domain" ] && base_domain="spx3.net"
    [ -z "$internal_domain" ] && internal_domain="compute.internal"

    # systemd-resolved is preferred when active; otherwise fall back to
    # resolvconf, the manager the ISO ships. A host with neither must be pointed
    # at the node :53 by hand — surface that rather than skipping silently.
    if $SUDO systemctl is-active --quiet systemd-resolved 2>/dev/null; then
        setup_host_dns_resolved "$ns_ip" "$base_domain" "$internal_domain"
    elif command -v resolvconf >/dev/null 2>&1; then
        setup_host_dns_resolvconf "$ns_ip" "$base_domain" "$internal_domain"
    else
        warn "No supported resolver manager (systemd-resolved or resolvconf) — host DNS not configured"
        warn "  Forward ${base_domain} and ${internal_domain} to ${ns_ip}:53 on this host manually"
    fi
}

# --- Main ---
main() {
    info "Spinifex installer"
    echo ""

    # Always needed: sudo resolution, OS/arch detection.
    setup_sudo
    detect_os
    detect_arch

    # Orchestration (stop running services, fix stale /run/spinifex/nbd perms)
    # only applies in full-install mode on a live host.
    if [ "${ISO_BUILD:-0}" != "1" ] && [ -z "${SETUP_STAGES:-}" ]; then
        handle_upgrade
    fi

    # Stages that need EXTRACT_DIR: files, directories, systemd, logrotate, udev.
    # Only download/extract when at least one such stage is enabled.
    if stage_enabled files || stage_enabled directories \
        || stage_enabled systemd || stage_enabled logrotate \
        || stage_enabled udev; then
        download_spinifex
    fi

    stage_enabled deps       && install_apt_deps
    stage_enabled aws        && install_aws_cli
    stage_enabled users      && create_service_users
    stage_enabled sudoers    && install_sudoers
    stage_enabled files      && install_files
    stage_enabled directories && create_directories
    stage_enabled env        && install_systemd_env
    stage_enabled fixown     && fix_file_ownership
    stage_enabled systemd    && install_systemd
    stage_enabled logrotate  && install_logrotate
    stage_enabled udev       && install_udev
    stage_enabled swap       && setup_swap
    stage_enabled resolved   && setup_host_dns

    # Migrations are only safe on a live system (need a running NATS and a
    # persisted config file). Skip under ISO_BUILD and under any explicit
    # SETUP_STAGES filter that doesn't list migrations.
    if [ "${ISO_BUILD:-0}" != "1" ] && stage_enabled migrations; then
        run_migrations
    fi

    [ -n "${SPINIFEX_TMPDIR:-}" ] && rm -rf "$SPINIFEX_TMPDIR"

    # Post-install orchestration (service restart, summary, newgrp) only in
    # full-install mode on a live host.
    if [ "${ISO_BUILD:-0}" = "1" ] || [ -n "${SETUP_STAGES:-}" ]; then
        return
    fi

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
