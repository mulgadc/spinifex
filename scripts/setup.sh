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
#                                deps, aws, users, sudoers, firewall, timesync, files,
#                                directories, env, systemd, logrotate, udev,
#                                fixown, migrations
#                              Unset = run every stage appropriate for the current mode.

set -e

INSTALL_SPINIFEX_CHANNEL="${INSTALL_SPINIFEX_CHANNEL:-latest}"
INSTALL_BASE_URL="${INSTALL_BASE_URL:-https://install.mulgadc.com}"

# Referenced by both the sudoers grant and the daemon, so the paths are fixed here.
ENDPOINT_SYSCTL_HELPER="/usr/local/lib/spinifex/spinifex-set-endpoint-sysctl"
IPSEC_STATE_HELPER="/usr/local/lib/spinifex/spinifex-set-ipsec-state"

# The firewall policy and the peer addresses it scopes cluster ports to. The
# policy ships with the node; the peers are written by spinifex-daemon, which is
# the only component that knows every node's resolved planes.
FIREWALL_DIR="/etc/spinifex/firewall"
FIREWALL_RULES="${FIREWALL_DIR}/spinifex.nft"
FIREWALL_PEERS="${FIREWALL_DIR}/peers.nft"
FIREWALL_LOCAL="${FIREWALL_DIR}/local.nft"
FIREWALL_OPEN="${FIREWALL_DIR}/open-ports.nft"
FIREWALL_MODE_FILE="${FIREWALL_DIR}/mode"
FIREWALL_APPLY="/usr/local/lib/spinifex/spinifex-firewall-apply"

# Whether the policy is armed, not merely installed. The ISO is an appliance on
# hardware we define, so a default-deny policy is the product; a curl-to-bash
# install lands on a machine that was already doing something, where arming a
# drop policy uninvited can cut off services we know nothing about.
INSTALL_SPINIFEX_FIREWALL="${INSTALL_SPINIFEX_FIREWALL:-}"

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
# sudo-rs (Ubuntu's default sudo since 25.10) implements only a subset of
# Defaults and rejects an unknown one outright, so pam_session must be omitted
# there rather than emitted and ignored.
sudo_is_sudo_rs() {
    sudo --version 2>/dev/null | head -1 | grep -qi 'sudo-rs'
}

# The daemon only ever sets rp_filter and accept_local on an endpoint it just
# created. A fixed-verb helper keeps that grant expressible without a wildcard,
# which sudo-rs forbids inside command arguments.
install_endpoint_sysctl_helper() {
    $SUDO install -d -m 0755 /usr/local/lib/spinifex
    $SUDO tee "${ENDPOINT_SYSCTL_HELPER}" > /dev/null << 'HELPER'
#!/bin/sh
# Sets one of two per-endpoint sysctls for the asymmetric IMDS reply path.
# Interface, key and value are all constrained: this runs as root under a
# NOPASSWD grant, so it must never pass an arbitrary key through to sysctl.
set -eu

if [ "$#" -ne 3 ]; then
    echo "usage: $0 <interface> <rp_filter|accept_local> <0|1>" >&2
    exit 2
fi

iface=$1
key=$2
value=$3

# Reject anything that is not a plain interface name, so the key cannot be
# steered elsewhere in the sysctl tree with a "." or a "/".
case "${iface}" in
    ''|*[!A-Za-z0-9_-]*) echo "invalid interface: ${iface}" >&2; exit 2 ;;
esac

case "${key}" in
    rp_filter|accept_local) ;;
    *) echo "key not permitted: ${key}" >&2; exit 2 ;;
esac

case "${value}" in
    0|1) ;;
    *) echo "value not permitted: ${value}" >&2; exit 2 ;;
esac

exec sysctl -qw "net.ipv4.conf.${iface}.${key}=${value}"
HELPER
    $SUDO chown root:root "${ENDPOINT_SYSCTL_HELPER}"
    $SUDO chmod 0755 "${ENDPOINT_SYSCTL_HELPER}"
}

# The daemon turns OVN IPsec on and off as the cluster topology resolves, so it
# needs unit control. A NOPASSWD systemctl grant takes an arbitrary unit name and
# is root-equivalent, so the units are fixed here and only the state is an input.
install_ipsec_state_helper() {
    $SUDO install -d -m 0755 /usr/local/lib/spinifex
    $SUDO tee "${IPSEC_STATE_HELPER}" > /dev/null << 'HELPER'
#!/bin/sh
# Turns the host's OVN IPsec services on or off. Runs as root under a NOPASSWD
# grant, so it must never act on a unit the caller names.
set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: $0 <on|off>" >&2
    exit 2
fi

# mask/unmask rather than disable/enable. The daemon's unit sets
# ProtectSystem=full, so /etc is read-only in the namespace its children inherit,
# and disabling a unit that also ships a SysV script shells out to update-rc.d
# there and fails. Masking is done by PID 1 over D-Bus and is unaffected.

# ovs-monitor-ipsec execs the strongSwan starter itself rather than going through
# this unit, so leaving it enabled only contends for UDP 500/4500. Off in both
# states. Absent on a host without strongswan, which is not an error.
systemctl mask --now strongswan-starter.service >/dev/null 2>&1 || true

case "$1" in
    on)
        systemctl unmask openvswitch-ipsec.service
        exec systemctl start openvswitch-ipsec.service
        ;;
    off)
        exec systemctl mask --now openvswitch-ipsec.service
        ;;
    *)
        echo "state not permitted: $1" >&2
        exit 2
        ;;
esac
HELPER
    $SUDO chown root:root "${IPSEC_STATE_HELPER}"
    $SUDO chmod 0755 "${IPSEC_STATE_HELPER}"
}

# Every port sshd actually listens on, so a host hardened onto a non-standard
# port is not locked out by its own firewall. Reads the Include drop-ins too,
# since Debian's default config ends with one. Falls back to 22, which is what
# sshd itself does when no Port is set.
sshd_ports() {
    _cfg="/etc/ssh/sshd_config"
    _files="$_cfg"
    if [ -r "$_cfg" ]; then
        for _inc in $(awk '$1 == "Include" { $1 = ""; print }' "$_cfg" 2>/dev/null); do
            for _f in $_inc; do
                [ -r "$_f" ] && _files="$_files $_f"
            done
        done
    fi

    # shellcheck disable=SC2086
    _ports=$(awk '$1 == "Port" && $2 ~ /^[0-9]+$/ && $2+0 > 0 && $2+0 < 65536 { print $2 }' \
        $_files 2>/dev/null | sort -un | tr '\n' ' ')
    [ -n "$_ports" ] || _ports="22"

    _out=""
    for _p in $_ports; do
        if [ -z "$_out" ]; then _out="$_p"; else _out="$_out, $_p"; fi
    done
    printf '%s' "$_out"
}

# The policy is written here rather than shipped in the tarball so the ISO chroot
# and the ansible path get an identical file without a packaging step. Installed
# but NOT enabled: the peer set is empty until spinifex-daemon resolves the
# cluster's planes, and a drop policy with no peers would break formation.
install_firewall() {
    stage "installing host firewall policy"
    info "Installing host firewall policy..."

    $SUDO install -d -m 0755 "$FIREWALL_DIR" /usr/local/lib/spinifex
    $SUDO tee "$FIREWALL_RULES" > /dev/null << 'RULES'
#!/usr/sbin/nft -f
# Spinifex host firewall. Managed by scripts/setup.sh — edits are overwritten.
#
# Only `table inet spinifex_filter` is created, deleted and replaced. vpcd writes
# MASQUERADE and per-EIP FORWARD ACCEPT rules into the `ip nat` and `ip filter`
# tables and reinstalls them only when the service starts, so anything that
# flushes the whole ruleset breaks elastic IPs silently until the next restart.
#
# INPUT only. nftables runs every table registered on a hook and any drop is
# final, so a forward-hook policy here could not be rescued by vpcd's ACCEPTs in
# the other table. OUTPUT is untouched: the IMDS reply path egresses under
# per-tap policy routing.

include "/etc/spinifex/firewall/local.nft"
include "/etc/spinifex/firewall/open-ports.nft"
include "/etc/spinifex/firewall/peers.nft"

# Create-then-delete so the replace is atomic and the delete cannot fail on a
# first run. nft applies the whole file as one transaction or none of it.
table inet spinifex_filter
delete table inet spinifex_filter

table inet spinifex_filter {
    chain input {
        type filter hook input priority filter; policy drop;

        # First, or every reply to a connection this node opened is dropped.
        ct state established,related accept
        ct state invalid drop
        iif lo accept

        # Guest metadata and VPC DNS terminate on per-ENI OVS internal ports in
        # the host netns, so guest traffic to 169.254.169.254 and .253 arrives
        # here. Without this, cloud-init, instance-role credentials and all
        # guest DNS break. The ports exist only while instances run, so this
        # matches the prefix rather than an enumerated list.
        iifname "ime-*" accept

        # PMTUD is not optional under a Geneve overlay.
        icmp type { echo-request, destination-unreachable, time-exceeded, parameter-problem } accept
        icmpv6 type { echo-request, destination-unreachable, packet-too-big, time-exceeded, parameter-problem, nd-neighbor-solicit, nd-neighbor-advert, nd-router-solicit, nd-router-advert } accept

        # A broadcast DHCP reply does not reliably match an established
        # conntrack entry, so the uplink and external-pool leases need this.
        udp sport 67 udp dport 68 accept

        # Public plane. 9999 is also how every guest agent (lb, eks, ecs, rds)
        # reaches its control plane over br-mgmt: they speak SigV4 to the AWS
        # gateway, never NATS, so no cluster port needs to face the guests.
        # 443 has no listener yet and is held for the nginx TLS edge, so the
        # rollout does not need a firewall change on every node.
        tcp dport $spinifex_ssh_ports accept
        tcp dport { 443, 3000, 8443, 9999 } accept

        # Transiently open to any source, for cluster formation. A node dialling
        # in to join is not a peer yet, so the peer-scoped rule below cannot let
        # it in; `spx admin init` opens the formation port here for the length of
        # the formation window and closes it again afterwards. Normally holds
        # only the sentinel, which no packet can match.
        tcp dport $spinifex_open_ports accept

        # 53 is open on every node, deliberately: northstar serves public
        # authoritative DNS and binds its advertise address. A node running no
        # public zone answers here too, which is the accepted cost of not
        # templating the policy per node.
        tcp dport 53 accept
        udp dport 53 accept

        # Cluster plane, peer-scoped. On a single-NIC node the planes collapse
        # onto the public address, which is why these are peer addresses rather
        # than an interface or a CIDR. 4432 stays here rather than joining the
        # public rule above: outside formation it is the daemon cluster manager,
        # whose /health and /local/* routes report node topology, service
        # inventory and running instances to anyone who asks.
        ip saddr $spinifex_peers tcp dport { 4222, 4248, 4432, 5300, 6641, 6642, 6643, 6644 } accept
        ip saddr $spinifex_peers udp dport { 5300, 6660, 7660 } accept

        # Encap plane, peer-scoped: Geneve, IKE, NAT-T and ESP. Geneve is a
        # kernel UDP-tunnel socket, so encapsulated packets are delivered
        # locally and pass this hook before the OVS vport sees them.
        ip saddr $spinifex_encap_peers udp dport { 6081, 500, 4500 } accept
        ip saddr $spinifex_encap_peers meta l4proto esp accept

        # Rate-limited so a scan cannot fill the journal. This is the only way
        # to tell a policy gap from an application fault after the fact.
        limit rate 5/minute burst 10 packets log prefix "spinifex-fw drop: " level info
    }
}
RULES
    $SUDO chmod 0644 "$FIREWALL_RULES"
    info "  $FIREWALL_RULES"

    # Host-local facts the policy needs that the daemon has no view of. Separate
    # from peers.nft because the daemon rewrites that file and would drop this.
    # Refreshed on every setup.sh run, so an sshd port change is picked up by an
    # upgrade rather than needing the file edited by hand.
    _ssh_ports="$(sshd_ports)"
    $SUDO tee "$FIREWALL_LOCAL" > /dev/null << LOCAL
# Managed by scripts/setup.sh — edits are overwritten on upgrade.
define spinifex_ssh_ports = { ${_ssh_ports} }
LOCAL
    $SUDO chmod 0644 "$FIREWALL_LOCAL"
    info "  $FIREWALL_LOCAL (sshd ports: ${_ssh_ports})"

    # Reset closed on every run, so an install or upgrade that interrupts a
    # formation window cannot leave the port open indefinitely.
    $SUDO tee "$FIREWALL_OPEN" > /dev/null << 'OPENPORTS'
# Managed by spinifex-firewall-apply. Rewritten for the formation window only.
define spinifex_open_ports = { 0 }
OPENPORTS
    $SUDO chmod 0644 "$FIREWALL_OPEN"
    info "  $FIREWALL_OPEN (closed)"

    # Armed or merely installed. The daemon reads this when the config carries no
    # explicit firewall_enabled, which is the normal case on both install paths.
    printf '%s\n' "$INSTALL_SPINIFEX_FIREWALL" | $SUDO tee "$FIREWALL_MODE_FILE" > /dev/null
    $SUDO chmod 0644 "$FIREWALL_MODE_FILE"
    info "  $FIREWALL_MODE_FILE ($INSTALL_SPINIFEX_FIREWALL)"

    $SUDO tee "$FIREWALL_APPLY" > /dev/null << 'APPLY'
#!/bin/sh
# Applies the spinifex host firewall. Fails without changing the ruleset when
# the peer file is missing, so a node whose daemon has not yet resolved cluster
# membership is left reachable rather than cut off from a cluster it cannot name.
set -eu

RULES="/etc/spinifex/firewall/spinifex.nft"
PEERS="/etc/spinifex/firewall/peers.nft"
OPEN="/etc/spinifex/firewall/open-ports.nft"

# 0 is a sentinel that keeps the set non-empty, because nft rejects an empty
# one. No packet can match it: the kernel has no destination port 0.
write_open_ports() {
    _tmp=$(mktemp)
    printf '%s\n' \
        '# Managed by spinifex-firewall-apply. Rewritten for the formation window only.' \
        "define spinifex_open_ports = { $1 }" > "$_tmp"
    install -m 0644 "$_tmp" "$OPEN"
    rm -f "$_tmp"
}

# set-peers reads two comma-separated address lists on stdin (cluster peers,
# then encap peers) and rewrites the peer file before applying. Every address is
# re-validated here as a bare dotted quad: this runs as root under a NOPASSWD
# grant, so a caller must not be able to inject nft syntax through it.
# disable removes spinifex's own table and the peer file, leaving every other
# table alone. Idempotent, so it is safe on a node that never had the policy.
# Deliberately ahead of the policy-file check below: turning the firewall off is
# the recovery path, and refusing to run it because the policy is missing leaves
# an operator with a node they can neither arm nor disarm.
if [ "${1:-}" = "disable" ]; then
    nft delete table inet spinifex_filter 2>/dev/null || true
    rm -f "$PEERS"
    systemctl disable --now spinifex-firewall.service >/dev/null 2>&1 || true
    exit 0
fi

# Every remaining verb writes the policy, so it has to exist. Failing here
# changes nothing, which leaves a node whose daemon has not yet resolved cluster
# membership reachable rather than cut off from a cluster it cannot name.
[ -r "$RULES" ] || { echo "no firewall policy at $RULES" >&2; exit 1; }

# open-port and close-port hold a single port open to any source while a cluster
# forms. One slot, not a list: the only caller is `spx admin init` opening its
# formation port, and a set that can only ever hold one entry cannot be grown
# into a general-purpose hole in the policy.
if [ "${1:-}" = "open-port" ] || [ "${1:-}" = "close-port" ]; then
    port="${2:-}"
    case "$port" in
        '' | *[!0-9]*) echo "port must be a number: '$port'" >&2; exit 2 ;;
    esac
    [ "$port" -gt 0 ] && [ "$port" -lt 65536 ] || {
        echo "port out of range: $port" >&2
        exit 2
    }

    if [ "$1" = "open-port" ]; then write_open_ports "0, $port"; else write_open_ports "0"; fi

    # Nothing to reload on a node that is installed but not armed: the table is
    # not loaded, so every port is already open and the file is enough.
    [ -r "$PEERS" ] || exit 0
    exec nft -f "$RULES"
fi

if [ "${1:-}" = "set-peers" ]; then
    read -r peers_in || peers_in=""
    read -r encap_in || encap_in=""

    # Octet ranges are checked, not just the shape: nft rejects 10.0.0.999 and
    # the whole transaction with it, which would leave a peer file that fails on
    # every boot. A loop, not a pipeline — a rejection in a `while read` subshell
    # cannot fail the function.
    OCTET='(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])'
    validate() {
        _out=""
        for a in $(echo "$1" | tr ',' ' '); do
            echo "$a" | grep -Eq "^${OCTET}(\.${OCTET}){3}$" || {
                echo "address not permitted: $a" >&2
                return 2
            }
            if [ -z "$_out" ]; then _out="$a"; else _out="$_out, $a"; fi
        done
        printf '%s' "$_out"
    }

    peers=$(validate "$peers_in") || exit 2
    encap=$(validate "$encap_in") || exit 2
    [ -n "$peers" ] && [ -n "$encap" ] || {
        echo "set-peers needs a non-empty peer and encap list" >&2
        exit 2
    }

    tmp=$(mktemp)
    bak=$(mktemp)
    printf '%s\n' \
        '# Managed by spinifex-daemon. Regenerated from cluster membership.' \
        "define spinifex_peers = { $peers }" \
        "define spinifex_encap_peers = { $encap }" > "$tmp"
    if [ -r "$PEERS" ]; then cp "$PEERS" "$bak"; fi
    install -m 0644 "$tmp" "$PEERS"

    # Roll the peer file back if the ruleset will not load, so the boot-time
    # unit never inherits a file already known to fail.
    if ! nft -f "$RULES"; then
        if [ -s "$bak" ]; then install -m 0644 "$bak" "$PEERS"; else rm -f "$PEERS"; fi
        rm -f "$tmp" "$bak"
        exit 1
    fi
    rm -f "$tmp" "$bak"

    # The policy is loaded, so make it survive a reboot too. `disable` turns this
    # unit off, and forming a cluster means disabling the firewall on every node
    # first; without this, re-arming would look complete while silently leaving
    # the node bare after its next boot. Idempotent and quiet in the normal case.
    systemctl enable spinifex-firewall.service >/dev/null 2>&1 || true
    exit 0
fi

if [ ! -r "$PEERS" ]; then
    echo "no peer file at $PEERS: the daemon writes it once cluster membership" >&2
    echo "resolves. Ruleset left unchanged." >&2
    exit 1
fi

exec nft -f "$RULES"
APPLY
    $SUDO chown root:root "$FIREWALL_APPLY"
    $SUDO chmod 0755 "$FIREWALL_APPLY"
    info "  $FIREWALL_APPLY"

    if [ "$INSTALL_SPINIFEX_FIREWALL" = "on" ]; then
        info "Host firewall installed and armed (applied by spinifex-daemon once peers resolve)"
    else
        info "Host firewall installed but NOT armed — this machine may already be"
        info "running services a default-deny policy would cut off."
        info "  Allowed if armed: ssh (${_ssh_ports}), 53, 443, 3000, 8443, 9999"
        info "  Everything else on this host stops accepting new connections."
        info "  Arm it later by setting network.firewall_enabled = true in"
        info "  /etc/spinifex/spinifex.toml, or re-run this installer with --firewall=on"
    fi
}

# Debian's stock chrony.conf carries `makestep 1 3`: step the clock only for the
# first three updates, then slew at ~83us/s, which is roughly 7 seconds of
# correction per day. A node that burns those three before the network is usable
# then crawls toward correct time for days, and SigV4 rejects its requests the
# whole time. A drop-in rather than an edit to chrony.conf, which is a dpkg
# conffile and would prompt on every future upgrade.
install_chrony_conf() {
    stage "installing chrony drop-in"

    if [ ! -d /etc/chrony/conf.d ]; then
        warn "No /etc/chrony/conf.d — skipping chrony drop-in"
        return
    fi

    $SUDO tee /etc/chrony/conf.d/spinifex.conf > /dev/null << 'CHRONY'
# Managed by scripts/setup.sh — edits are overwritten.
# Step at any offset over a second, however many times it takes. The clock can
# jump backwards under load, which is the accepted cost: everything
# latency-sensitive here uses the monotonic clock, and a node minutes out of
# step fails every signed request until it converges.
makestep 1 -1
CHRONY
    $SUDO chmod 0644 /etc/chrony/conf.d/spinifex.conf
    info "  /etc/chrony/conf.d/spinifex.conf"
}

install_sudoers() {
    stage "installing scoped sudoers rules"
    install_endpoint_sysctl_helper
    install_ipsec_state_helper

    if sudo_is_sudo_rs; then
        $SUDO tee /etc/sudoers.d/spinifex-network > /dev/null << 'SUDOERS'
# pam_session is omitted: this host runs sudo-rs, which does not implement it.
SUDOERS
    else
        $SUDO tee /etc/sudoers.d/spinifex-network > /dev/null << 'SUDOERS'
# No PAM session for the service users. Every sudo call otherwise logs
# "pam_limits: Could not set limit for 'core'": the units set LimitCORE=0 and
# their CapabilityBoundingSet omits CAP_SYS_RESOURCE, so pam_limits cannot apply
# the limits.conf default and warns. It also means sudo children inherit the
# unit's rlimits rather than the system defaults, which is the RG-9 intent.
Defaults:spinifex-daemon !pam_session
SUDOERS
    fi

    $SUDO tee -a /etc/sudoers.d/spinifex-network > /dev/null << 'SUDOERS'

# The OVS/OVN client tools are deliberately absent. They do all their work over
# control sockets that setup-ovn.sh group-owns to `spinifex`, so they run as the
# service user. Granting them would be worse than pointless: a NOPASSWD rule for
# them takes unrestricted args, and ovs-vsctl/ovn-nbctl/ovn-appctl all accept
# --log-file=PATH, which writes a root-owned file wherever the caller points it.
#
# ovs-ofctl is the exception that keeps its grant: it talks to a per-bridge
# <bridge>.mgmt socket created by ovs-vswitchd when the bridge appears, including
# bridges spinifex creates at runtime, so it cannot be group-owned up front.

# spinifex-vpcd has no rules at all. Its ip/iptables/sysctl/arping work is done
# under the CAP_NET_ADMIN and CAP_NET_RAW its unit grants ambiently, which the
# kernel passes to each child on exec. Those grants were also root-equivalent:
# `sudo ip netns exec <ns> /bin/sh` is a root shell.

# Spinifex daemon: tap devices and OpenFlow rules. Its unit holds no
# CAP_NET_ADMIN, so unlike vpcd it cannot drop these. ovs-ofctl installs the
# per-tap IMDS datapath flows on br-imds; the sysctl helper sets the
# per-endpoint rp_filter/accept_local the asymmetric reply path needs.
spinifex-daemon ALL=(root) NOPASSWD: /sbin/ip, /usr/sbin/ip
spinifex-daemon ALL=(root) NOPASSWD: /usr/bin/ovs-ofctl
SUDOERS

    # Separate append so the prose above can stay in a quoted heredoc: it carries
    # backticks that an expanding one would run as command substitution.
    $SUDO tee -a /etc/sudoers.d/spinifex-network > /dev/null << SUDOERS
spinifex-daemon ALL=(root) NOPASSWD: ${ENDPOINT_SYSCTL_HELPER}
spinifex-daemon ALL=(root) NOPASSWD: ${IPSEC_STATE_HELPER}
spinifex-daemon ALL=(root) NOPASSWD: ${FIREWALL_APPLY}
SUDOERS
    $SUDO chmod 0440 /etc/sudoers.d/spinifex-network
    $SUDO visudo -cf /etc/sudoers.d/spinifex-network || fatal "Invalid sudoers syntax in spinifex-network"
    info "Scoped sudoers rules installed for spinifex-daemon (spinifex-vpcd needs none)"
}

# --- Install apt dependencies ---
# NOTE: Runtime deps must stay in sync with the ISO package list at
# scripts/iso-builder/build/packages.list in the mulga repo. When you add,
# remove, or change a runtime package here, review that file too — drift means
# the ISO and `curl | bash` paths install different software.
#
# Hoisted out of the apt-get call so scripts/lint-apt-packages.sh can resolve the
# same list against each supported suite. The arch-specific qemu package is
# added by install_apt_deps from detect_arch.
APT_RUNTIME_PACKAGES="nbdkit
qemu-utils gdisk ovmf qemu-efi-aarch64 less
libvirt-daemon-system libvirt-clients
pciutils
jq curl iproute2 netcat-openbsd wget unzip xz-utils file
ovn-central ovn-host openvswitch-switch openvswitch-ipsec strongswan-charon dhcpcd-base
chrony nftables"

install_apt_deps() {
    stage "installing apt dependencies"
    if [ "${INSTALL_SPINIFEX_SKIP_APT}" = "1" ]; then
        info "Skipping apt dependencies (INSTALL_SPINIFEX_SKIP_APT=1)"
    else
        info "Installing system dependencies..."
        $SUDO apt-get update -qq

        # Unquoted on purpose: both variables are whitespace-separated package lists.
        # shellcheck disable=SC2086
        DEBIAN_FRONTEND=noninteractive $SUDO apt-get install -y -qq \
            $QEMU_PACKAGES $APT_RUNTIME_PACKAGES \
            > /dev/null

        info "System dependencies installed"

        # Only on a machine that already boots from ZFS. Pulling zfsutils-linux
        # in unconditionally would drag zfs-dkms onto every dev box and rebuild
        # the module on each kernel upgrade for no benefit.
        if [ "$(findmnt -no FSTYPE /)" = "zfs" ] && ! command -v zpool >/dev/null 2>&1; then
            info "ZFS root detected, installing zfsutils-linux..."
            DEBIAN_FRONTEND=noninteractive $SUDO apt-get install -y -qq zfsutils-linux > /dev/null
        fi
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

    # Symlink so services that expect BaseDir/config/ can find /etc/spinifex/.
    # -n so a re-run replaces the link rather than creating one inside the
    # directory it already points at.
    if $SUDO test ! -e /var/lib/spinifex/config; then
        $SUDO ln -sfn /etc/spinifex /var/lib/spinifex/config
    fi

    # Symlink so services that write logs to BaseDir/logs/ use /var/log/spinifex/
    if $SUDO test ! -e /var/lib/spinifex/logs; then
        $SUDO ln -sfn /var/log/spinifex /var/lib/spinifex/logs
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

    $SUDO mkdir -p /var/lib/spinifex/spinifex-ui
    $SUDO chown "spinifex-ui:$SPINIFEX_GROUP" /var/lib/spinifex/spinifex-ui
    $SUDO chmod 0700 /var/lib/spinifex/spinifex-ui

    # Symlink so awsgw's {BaseDir}/config/ paths resolve to /etc/spinifex/.
    # The test runs under $SUDO because the parent is 0700 spinifex-gw: an
    # unprivileged -e cannot stat through it and always reports "missing", so
    # every re-run reached the ln below. That ln then resolved through the
    # existing link and tried to create /etc/spinifex/spinifex, which
    # CreateServiceDirectories already owns — failing the whole install.
    if $SUDO test ! -e /var/lib/spinifex/awsgw/config; then
        $SUDO ln -sfn /etc/spinifex /var/lib/spinifex/awsgw/config
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
            # setgid on directories only, so subdirectories a root run creates
            # keep the spinifex group instead of falling back to root and locking
            # the admin CLI out of the tree it is meant to write.
            $SUDO find "/var/lib/spinifex/$d" -type d -exec chmod g+s {} + \
                || fatal "Failed to set setgid on /var/lib/spinifex/$d"
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
    # The unit file alone does not self-activate (WantedBy=timers.target only
    # takes effect once enabled) — without this the JetStream ENOSPC-latch
    # watchdog is inert and a full disk requires a manual restart forever.
    $SUDO systemctl enable --now spinifex-nats-watchdog.timer
    # Enabled, not started: its ConditionPathExists holds it inert until the
    # daemon writes a peer file, so this only decides that a node which has one
    # gets the policy back at boot, before any service opens a socket.
    $SUDO systemctl enable spinifex-firewall.service
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

# --- Configure host swap ---
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

# --- Main ---
main() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --firewall=*) INSTALL_SPINIFEX_FIREWALL="${1#*=}" ;;
            --firewall)   INSTALL_SPINIFEX_FIREWALL="${2:-}"; shift ;;
            *) fatal "unknown option: $1" ;;
        esac
        shift
    done

    case "${INSTALL_SPINIFEX_FIREWALL}" in
        on|off) ;;
        # Unset resolves by install path: the ISO arms, everything else does not.
        "") if [ "${ISO_BUILD:-0}" = "1" ]; then
                INSTALL_SPINIFEX_FIREWALL="on"
            else
                INSTALL_SPINIFEX_FIREWALL="off"
            fi ;;
        *) fatal "--firewall must be 'on' or 'off', got: ${INSTALL_SPINIFEX_FIREWALL}" ;;
    esac

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
    stage_enabled firewall   && install_firewall
    stage_enabled timesync   && install_chrony_conf
    stage_enabled files      && install_files
    stage_enabled directories && create_directories
    stage_enabled env        && install_systemd_env
    stage_enabled fixown     && fix_file_ownership
    stage_enabled systemd    && install_systemd
    stage_enabled logrotate  && install_logrotate
    stage_enabled udev       && install_udev
    stage_enabled swap       && setup_swap

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

# Guarded so the package lint can source this file for APT_RUNTIME_PACKAGES
# without running the installer.
if [ "${INSTALL_SPINIFEX_LIB_ONLY:-0}" != "1" ]; then
    main "$@"
fi
