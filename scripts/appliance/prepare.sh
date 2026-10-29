#!/bin/bash
# Runs as root on the Oracle Linux build system, one stage per call, driven by
# build-oracle-appliance.sh from the payload directory it copied over.
#   prepare.sh check      refuse anything but OL 10.2+ on UEK R8, x86_64
#   prepare.sh update     dnf update, SELinux permissive; prints PREPARE_REBOOT=1 if
#                         the newest kernel is not the running one
#   prepare.sh install    install Spinifex, uninitialised and not enabled
#   prepare.sh appliance  spinifex login, first-boot service, console placeholder
#   prepare.sh seal       wipe this system's identity, then power off
# INSTALL_SPINIFEX_CHANNEL, INSTALL_SPINIFEX_VERSION and INSTALL_SPINIFEX_TARBALL
# pick the release tarball to install. BUILD_USER is the SSH user to lock at seal.
set -euo pipefail

# Shared by every unit built from this image; first login forces a change.
DEFAULT_PASSWORD=spinifex

HERE=$(cd "$(dirname "$0")" && pwd)
SHARE=/usr/local/share/spinifex

log() { echo "[prepare] $*"; }
die() { echo "[prepare] ERROR: $*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run as root"

stage_check() {
    # shellcheck source=/dev/null
    . /etc/os-release
    [ "${ID:-}" = ol ] || die "not Oracle Linux (ID=${ID:-unset})"
    local major=${VERSION_ID%%.*} minor=${VERSION_ID#*.}
    [ "$minor" = "$VERSION_ID" ] && minor=0
    if [ "$major" -lt 10 ] || { [ "$major" -eq 10 ] && [ "$minor" -lt 2 ]; }; then
        # A 10.0 or 10.1 system reaches 10.2 through the update stage, which
        # calls this again with "strict" once it has run.
        if [ "$major" -ne 10 ] || [ "${1:-}" = strict ]; then
            die "Oracle Linux $VERSION_ID; 10.2 or later is required"
        fi
        log "Oracle Linux $VERSION_ID; the update stage brings it to the current minor"
    fi
    [ "$(uname -m)" = x86_64 ] || die "$(uname -m); only x86_64 is supported"
    case $(uname -r) in
        6.12.0-*uek*) ;;
        *) die "running kernel $(uname -r) is not UEK R8; install kernel-uek and boot it first" ;;
    esac
    command -v nmcli >/dev/null || die "NetworkManager (nmcli) is required"
    log "OK: $PRETTY_NAME, kernel $(uname -r)"
}

stage_update() {
    setenforce 0 2>/dev/null || true
    sed -i 's/^SELINUX=.*/SELINUX=permissive/' /etc/selinux/config
    [ "$(getenforce)" != Enforcing ] || die "SELinux is still enforcing"
    dnf update -y
    stage_check strict >/dev/null
    local newest
    newest=$(grubby --default-kernel | sed 's|^/boot/vmlinuz-||')
    case $newest in
        6.12.0-*uek*) ;;
        *) die "the default boot kernel is $newest, not UEK R8; set a kernel-uek default with grubby" ;;
    esac
    if [ "$newest" != "$(uname -r)" ]; then
        echo PREPARE_REBOOT=1
    fi
}

stage_install() {
    [ ! -e /etc/spinifex/spinifex.toml ] || die "/etc/spinifex/spinifex.toml exists; this system has been initialised, start from a clean OL install"
    # install.mulgadc.com serves the latest stable setup.sh, which may predate
    # Oracle Linux support, so run the setup.sh inside the release tarball.
    local dir tarball
    dir=$(mktemp -d)
    if [ -n "${INSTALL_SPINIFEX_TARBALL:-}" ]; then
        tarball=$INSTALL_SPINIFEX_TARBALL
    else
        local url="https://install.mulgadc.com/download/${INSTALL_SPINIFEX_VERSION:-${INSTALL_SPINIFEX_CHANNEL:-latest}}/amd64"
        tarball=$dir/spinifex.tar.gz
        log "downloading $url"
        curl -fsSL -o "$tarball" "$url"
        curl -fsSL -o "$dir/checksum.sha256" "$url.sha256"
        [ "$(awk '{print $1}' "$dir/checksum.sha256")" = "$(sha256sum "$tarball" | awk '{print $1}')" ] \
            || die "checksum mismatch for $url"
    fi
    tar -xzf "$tarball" -C "$dir" ./setup.sh
    INSTALL_SPINIFEX_TARBALL=$tarball INSTALL_SPINIFEX_SKIP_NEWGRP=1 bash "$dir/setup.sh"
    rm -rf "$dir"
    # The installer enables these on a live host. The ISO image ships them off
    # and first boot enables them after init; a clone that started them first
    # would run every service with no config.
    systemctl disable --now spinifex.target spinifex-nats-watchdog.timer
    log "Spinifex $(spx version 2>/dev/null | head -1) installed, not initialised"
}

stage_appliance() {
    # As on the ISO: the existing spinifex system group as primary, so the
    # login can reach the sockets setup.sh group-owns, and passwordless sudo.
    if ! id spinifex >/dev/null 2>&1; then
        useradd --create-home --shell /bin/bash --gid spinifex --groups wheel spinifex
    fi
    echo "spinifex:$DEFAULT_PASSWORD" | chpasswd
    chage -d 0 spinifex
    echo 'spinifex ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/spinifex-admin
    chmod 0440 /etc/sudoers.d/spinifex-admin
    visudo -cf /etc/sudoers.d/spinifex-admin >/dev/null

    # OL's pwquality gives one try per login, then ends the session, so say the
    # rules up front. spx admin banner keeps /etc/motd content above its own
    # section, and agetty shows /etc/issue.d beside the banner in /etc/issue.
    local hint="First login: the new password needs 8+ characters and must not contain 'spinifex'."
    printf '%s\n' "$hint" > /etc/motd
    install -d /etc/issue.d
    printf '%s\n\n' "$hint" > /etc/issue.d/spinifex-password.issue

    # sshd takes the first value it reads, so this sorts ahead of the
    # 50-cloud-init.conf that templates ship with passwords off.
    printf 'PasswordAuthentication yes\n' > /etc/ssh/sshd_config.d/10-spinifex-appliance.conf

    # No DHCP on every port: first boot puts the primary NIC on br-wan and
    # leaves the rest alone, so no second port competes for the default route.
    printf '[main]\nno-auto-default=*\n' > /etc/NetworkManager/conf.d/90-spinifex-appliance.conf

    install -D -m 0644 "$HERE/lib.sh" "$SHARE/appliance/lib.sh"
    install -D -m 0755 "$HERE/firstboot.sh" "$SHARE/appliance/firstboot.sh"
    install -m 0644 "$HERE/spinifex-appliance-firstboot.service" "$HERE/spinifex-banner.service" /etc/systemd/system/
    local getty
    for getty in getty@tty1 serial-getty@ttyS0; do
        install -D -m 0644 "$HERE/getty-wait.conf" "/etc/systemd/system/$getty.service.d/spinifex-wait.conf"
    done
    systemctl daemon-reload
    systemctl enable spinifex-appliance-firstboot.service

    # Cloud-init would re-run on each unit with no datasource and rewrite the
    # network and users first boot owns.
    if [ -d /etc/cloud ]; then
        touch /etc/cloud/cloud-init.disabled
    fi
    log "spinifex login (default password '$DEFAULT_PASSWORD', change forced) and first-boot service installed"
}

stage_seal() {
    dnf clean all

    systemctl stop "$(. "$SHARE/lib/distro.sh" && distro_ovs_unit)" ovn-northd.service ovn-controller.service 2>/dev/null || true
    # OVS writes the chassis UUID on first start; a shared one makes every unit
    # the same chassis in OVN.
    rm -f /etc/openvswitch/system-id.conf /etc/openvswitch/conf.db /etc/openvswitch/.conf.db.~lock~
    rm -f /var/lib/ovn/*.db /etc/ovn/*.db
    rm -f /etc/spinifex/node.conf

    # Profiles for the build system's NICs, its DHCP leases and NM's
    # per-machine secret. First boot writes br-wan for the unit's own NIC.
    rm -f /etc/NetworkManager/system-connections/*
    rm -f /var/lib/NetworkManager/*.lease /var/lib/NetworkManager/secret_key \
        /var/lib/NetworkManager/timestamps /var/lib/NetworkManager/seen-bssids
    if command -v cloud-init >/dev/null; then
        cloud-init clean --logs || true
    fi

    # sshd-keygen regenerates host keys before sshd on the next boot; systemd
    # generates a machine-id when the file is empty.
    rm -f /etc/ssh/ssh_host_*
    truncate -s 0 /etc/machine-id
    rm -f /var/lib/dbus/machine-id /var/lib/systemd/random-seed
    echo localhost > /etc/hostname

    # The build user's access goes; the spinifex login is the way in.
    if [ -n "${BUILD_USER:-}" ] && [ "$BUILD_USER" != spinifex ] && [ "$BUILD_USER" != root ]; then
        local home
        home=$(getent passwd "$BUILD_USER" | cut -d: -f6)
        rm -f "$home/.ssh/authorized_keys" "$home/.bash_history"
        passwd -l "$BUILD_USER" >/dev/null
        rm -f "/etc/sudoers.d/90-cloud-init-users"
    fi
    rm -f /root/.ssh/authorized_keys /root/.bash_history /home/spinifex/.bash_history

    printf '\nSpinifex first boot is in progress. This screen updates when it finishes.\n\n' > /etc/issue
    sed -i '/^# --- Spinifex ---$/,$d' /etc/motd 2>/dev/null || true

    find /var/log -type f -exec truncate -s 0 {} + 2>/dev/null || true
    rm -rf /var/log/journal/* /tmp/* /var/tmp/*
    sync
    log "sealed; powering off"
    systemctl poweroff --no-block
}

case "${1:-}" in
    check) stage_check ;;
    update) stage_update ;;
    install) stage_install ;;
    appliance) stage_appliance ;;
    seal) stage_seal ;;
    *) die "usage: $0 check|update|install|appliance|seal" ;;
esac
