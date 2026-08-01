#!/bin/bash
# node-reset.sh — return one Spinifex node to its pre-install state.
#
# Teardown only: it stops services, removes state, and exits. It installs
# nothing, starts nothing and decides nothing. Callers do the rebuilding —
# reset-dev-env.sh locally, install-node.sh --wipe over SSH.
#
# Usage:
#   sudo scripts/node-reset.sh [--keep-data] [--yes] [--dry-run]
#
# Options:
#   --keep-data   Leave /var/lib/spinifex alone: volumes, S3 objects and
#                 JetStream survive. For when only the control plane is suspect.
#   --yes         Skip the confirmation prompt
#   --dry-run     Print what would be removed and touch nothing
#
# WHAT THIS DESTROYS
#
# Every instance on this node and every volume backing them, the node's CA and
# master key, all OVN logical network state, and the S3 objects held here. None
# of it is recoverable. Data sealed under the master key cannot be read back
# even if the bytes are restored from elsewhere.
set -euo pipefail

ETC_DIR=/etc/spinifex
DATA_DIR=/var/lib/spinifex
LOG_DIR=/var/log/spinifex
RUN_DIR=/run/spinifex

KEEP_DATA=false
ASSUME_YES=false
DRY_RUN=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --keep-data) KEEP_DATA=true ;;
        --yes | -y)  ASSUME_YES=true ;;
        --dry-run)   DRY_RUN=true ;;
        -h | --help)
            sed -n '2,23p' "$0" | sed 's/^# \?//'
            exit 0
            ;;
        *)
            echo "ERROR: unknown option: $1" >&2
            exit 1
            ;;
    esac
    shift
done

log() { echo "[node-reset] $*"; }
run() {
    if $DRY_RUN; then
        echo "  would run: $*"
        return 0
    fi
    "$@"
}

WIPE_DIRS=("$ETC_DIR" "$LOG_DIR" "$RUN_DIR")
$KEEP_DATA || WIPE_DIRS+=("$DATA_DIR")

# Report what is at stake in figures rather than adjectives. An operator who
# reads "3 instances, 12 volumes, 840G" makes a better decision than one who
# reads a warning banner.
# `|| true` on each: under pipefail a find over a directory that was never
# created fails the whole pipeline, and a node with nothing to report is the
# most ordinary case there is.
if [ -d "$DATA_DIR" ]; then
    instances=$(sudo find "$DATA_DIR/instances" -maxdepth 1 -mindepth 1 -type d 2>/dev/null | wc -l || true)
    volumes=$(sudo find "$DATA_DIR/volumes" -maxdepth 1 -mindepth 1 2>/dev/null | wc -l || true)
    size=$(sudo du -sh "$DATA_DIR" 2>/dev/null | cut -f1 || true)
    log "on $(hostname): $instances instance(s), $volumes volume(s), ${size:-unknown} under $DATA_DIR"
    $KEEP_DATA && log "  --keep-data: $DATA_DIR will be preserved"
fi
log "removing: ${WIPE_DIRS[*]}"

if ! $ASSUME_YES && ! $DRY_RUN; then
    read -r -p "Destroy this node's state? Type 'destroy' to continue: " reply
    [ "$reply" = "destroy" ] || { log "aborted"; exit 1; }
fi

log "stopping services"
run sudo systemctl stop spinifex.target 2>/dev/null || true
run sudo systemctl reset-failed 'spinifex-*' 2>/dev/null || true
run sudo pkill -x qemu-system-x86_64 2>/dev/null || true
run sudo pkill -x qemu-system-aarch64 2>/dev/null || true

# Viperblock state must not be torn out from under a live guest, so wait for
# QEMU to actually exit rather than assuming the signal was enough.
if ! $DRY_RUN; then
    elapsed=0
    while pgrep -x 'qemu-system-x86_64|qemu-system-aarch64' >/dev/null 2>&1; do
        if [ "$elapsed" -ge 30 ]; then
            echo "ERROR: QEMU still running after 30s:" >&2
            pgrep -af 'qemu-system-' >&2 || true
            echo "  Kill them manually and re-run." >&2
            exit 1
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
fi

# Clearing external_ids drops system-id along with it. That is the point: a
# node keeping its old system-id across a reset re-registers under the old
# chassis name and collides with the new one over the encap IP.
log "removing OVS bridges and identity"
if command -v ovs-vsctl >/dev/null 2>&1; then
    run sudo systemctl start openvswitch-switch 2>/dev/null || true
    $DRY_RUN || sleep 1
    # Listed even under --dry-run: "which bridges" is the question an operator
    # actually has here, and br-wan is a Linux bridge so it is never in this set.
    for br in $(sudo ovs-vsctl list-br 2>/dev/null || true); do
        log "  deleting bridge: $br"
        run sudo ovs-vsctl --if-exists del-br "$br"
    done
    run sudo ovs-vsctl --if-exists clear Open_vSwitch . external_ids 2>/dev/null || true
    run sudo systemctl stop openvswitch-switch 2>/dev/null || true
fi
run sudo rm -f /etc/openvswitch/system-id.conf

# Delete the OVN DBs outright — a caller re-running setup-ovn.sh gets fresh
# empty ones. This is what clears stale chassis rows and port bindings, which
# otherwise accumulate across resets and wedge ovn-controller in a commit loop.
log "removing OVN databases"
run sudo systemctl stop ovn-central 2>/dev/null || true
run sudo systemctl stop ovn-controller 2>/dev/null || true
run sudo rm -f /var/lib/ovn/ovnnb_db.db /var/lib/ovn/ovnsb_db.db

if ip link show veth-wan-br >/dev/null 2>&1; then
    log "  deleting veth pair: veth-wan-br <-> veth-wan-ovs"
    run sudo ip link del veth-wan-br 2>/dev/null || true
fi

# Without this, systemd-networkd recreates the veth on the next reboot even
# after a full reset.
if [ -e /etc/systemd/network/15-spinifex-veth-wan.netdev ] ||
    [ -e /etc/systemd/network/15-spinifex-veth-wan.network ] ||
    [ -e /etc/systemd/network/16-spinifex-veth-wan-ovs.network ]; then
    log "  deleting veth persistence units"
    run sudo rm -f /etc/systemd/network/15-spinifex-veth-wan.netdev \
        /etc/systemd/network/15-spinifex-veth-wan.network \
        /etc/systemd/network/16-spinifex-veth-wan-ovs.network
    run sudo networkctl reload 2>/dev/null || true
fi

log "wiping ${WIPE_DIRS[*]}"
run sudo rm -rf "${WIPE_DIRS[@]}"

# The next init writes a fresh CA. Leaving the old one trusted means the host
# trusts a CA nobody holds the key for.
if [ -f /usr/local/share/ca-certificates/spinifex-ca.crt ]; then
    log "removing the stale CA from the trust store"
    run sudo rm -f /usr/local/share/ca-certificates/spinifex-ca.crt
    run sudo update-ca-certificates
fi

log "done — node is at its pre-install state"
