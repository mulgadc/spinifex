#!/bin/bash
# node-reset.sh — return one Spinifex node to a freshly-installed state.
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

# Directories and symlinks are structure, not state: setup.sh builds both and
# nothing in a cluster is stored as either. Two of those symlinks are load
# bearing and easy to miss — /var/lib/spinifex/config and awsgw/config both
# point at /etc/spinifex, the second nested a level down, and awsgw reads its
# IAM master key through it. Keeping every symlink covers them without this
# script having to know where setup.sh put them.
#
# keep_args adds the regular files worth keeping: the service helper scripts
# and the env files the units load, at the top level of $1 only. A *.sh deeper
# in the tree is cluster state, not an installed helper.
#
# firewall/custom.nft is kept too, one level down: it is an operator's SSH
# allowlist, and losing it silently returns the node to accepting ssh from
# every source. Parenthesised so the -o groups against the whole top-level arm.
KEEP_TOP=()
keep_args() {
    KEEP_TOP=('(' -path "$1/*" ! -path "$1/*/*" '(' -name '*.sh' -o -name '*.env' ')' ')' \
              -o -path "$1/firewall/custom.nft")
}

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

# Stopping the target is not the same as its services being stopped.
# spinifex-shutdown.service is ordered After= the storage services so its drain
# runs while they are still up, which queues their stop jobs behind its
# ExecStop, and `systemctl stop` returns as soon as the target itself is
# inactive. NATS surviving that is what lets JetStream rewrite its index after
# the wipe and before the verification, failing the reset on a file the script
# had already deleted. update-nodes.sh carries the same wait.
if ! $DRY_RUN; then
    for unit in spinifex-predastore spinifex-viperblock spinifex-nats; do
        if systemctl is-active --quiet "$unit"; then
            sudo systemctl stop "$unit" || true
        fi
    done
    elapsed=0
    while [ -n "$(pgrep -x spx || true)" ]; do
        if [ "$elapsed" -ge 120 ]; then
            echo "ERROR: spx still running after 120s:" >&2
            pgrep -ax spx >&2 || true
            echo "  Wiping under a live service races JetStream, which rewrites" >&2
            echo "  its index after the rm. Stop them manually and re-run." >&2
            exit 1
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
fi

# `pgrep -x` and `pkill -x` match against comm, which the kernel truncates to 15
# characters, so `qemu-system-x86_64` is stored as `qemu-system-x86` and an -x
# match on the full name can never fire. Match the prefix instead: it is 11
# characters, well under the limit, and covers both x86_64 and aarch64. This is
# the same rule qemuProcessPrefix applies in spinifex/vm/orphan_scan.go.
#
# Deliberately not -f. That matches the whole command line, where argv[0] may
# carry a path, so an anchored pattern misses the process it was written for.
#
# Scoped by owner, because a host carrying libvirt guests alongside Spinifex
# ones has both under the same comm and an unscoped kill would take the wrong
# set down. The daemon runs guests as its own service user on an installed node;
# a dev install runs them as whoever invoked spx.
QEMU_PGREP=('^qemu-system')
for qemu_user in spinifex-daemon spinifex "${SUDO_USER:-$(id -un)}"; do
    if id -u "$qemu_user" >/dev/null 2>&1; then
        QEMU_PGREP=(-u "$qemu_user" "${QEMU_PGREP[@]}")
        break
    fi
done
qemu_pids() { sudo pgrep "${QEMU_PGREP[@]}" 2>/dev/null || true; }

if [ -n "$(qemu_pids)" ]; then
    log "signalling QEMU guests owned by $qemu_user"
    run sudo pkill "${QEMU_PGREP[@]}" 2>/dev/null || true
fi

# Viperblock state must not be torn out from under a live guest, so wait for
# QEMU to actually exit rather than assuming the signal was enough.
if ! $DRY_RUN; then
    elapsed=0
    while [ -n "$(qemu_pids)" ]; do
        if [ "$elapsed" -ge 30 ]; then
            echo "ERROR: QEMU still running after 30s:" >&2
            sudo pgrep -a "${QEMU_PGREP[@]}" >&2 || true
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
# The firewall policy under $ETC_DIR/firewall is installed state, not cluster
# state, but the wipe below cannot tell them apart and takes the lot. Losing it
# is silent and permanent: `spinifex-firewall-apply set-peers` fails on every
# attempt with "no firewall policy", so the daemon never re-arms and the node
# stays open forever. Read the arming mode now and regenerate the stage after
# the wipe. peers.nft is the one file here that really is cluster state, and
# regenerating does not bring it back.
#
# Reading it from $ETC_DIR alone is not enough, because a run that fails between
# the wipe and the restore has already deleted the file. The retry then reads
# the absence as "this node was never armed", skips the restore, and
# install-node.sh honours that and forms the cluster with an open node on a
# public address. Stash the mode outside every wiped tree so the retry can still
# find it, and keep it there until the policy is back.
FIREWALL_STASH=/var/lib/spinifex-reset/firewall-mode

FIREWALL_MODE=""
if [ -r "$ETC_DIR/firewall/mode" ]; then
    FIREWALL_MODE=$(sudo cat "$ETC_DIR/firewall/mode" 2>/dev/null | tr -d '[:space:]')
elif [ -r "$FIREWALL_STASH" ]; then
    FIREWALL_MODE=$(sudo cat "$FIREWALL_STASH" 2>/dev/null | tr -d '[:space:]')
    log "recovered the firewall mode from an interrupted reset: $FIREWALL_MODE"
fi

# Absent from both means this node genuinely has no policy — a fresh install or
# a dev tree — and arming one here would be inventing a decision nobody made.
if [ -n "$FIREWALL_MODE" ] && ! $DRY_RUN; then
    sudo mkdir -p "$(dirname "$FIREWALL_STASH")"
    printf '%s\n' "$FIREWALL_MODE" | sudo tee "$FIREWALL_STASH" >/dev/null
fi

# Named in the transcript because the risk runs the other way too: an allowlist
# naming the old cluster's management network locks an operator out of the new one.
if [ -e "$ETC_DIR/firewall/custom.nft" ]; then
    log "keeping the operator ssh allowlist in $ETC_DIR/firewall/custom.nft"
fi

log "removing OVN databases"
run sudo systemctl stop ovn-central 2>/dev/null || true
run sudo systemctl stop ovn-controller 2>/dev/null || true
run sudo rm -f /var/lib/ovn/ovnnb_db.db /var/lib/ovn/ovnsb_db.db

# The RAFT membership flags live in this file, not in the databases just
# deleted. Left behind, ovn-central recreates the DB as a cluster member and
# blocks dialling a peer that has itself been reset.
run sudo rm -f /etc/default/ovn-central

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

# `rm -rf` is wrong here on two counts. On an ISO-installed node several of
# these are separate ZFS datasets — rpool/log and rpool/data/{nats,viperblock,
# predastore,predastore-db,predastore-nodes} — and a mountpoint cannot be
# removed at all. And the directory tree is itself install state worth keeping:
# it carries the ownership setup.sh assigned per service.
log "wiping ${WIPE_DIRS[*]}"
for dir in "${WIPE_DIRS[@]}"; do
    [ -d "$dir" ] || continue
    if $DRY_RUN; then
        echo "  would empty: $dir"
        continue
    fi
    keep_args "$dir"
    # Regular files only, so directories and symlinks survive along with the
    # per-service ownership setup.sh assigned. Nothing then has to be
    # reinstalled afterwards, which is what lets this script install nothing
    # and leave a node on the exact build it was already running.
    sudo find "$dir" -mindepth 1 \( -type f -o -type s -o -type p \) \
        ! \( "${KEEP_TOP[@]}" \) -delete 2>/dev/null || true
done

# The wipe above keeps directories, so the pre-cutover cluster/host-N/node-M
# skeleton would survive beside the node-<id> dirs written now. predastore owns
# this tree, not setup.sh, so removing it outright loses no installed ownership.
if ! $KEEP_DATA && [ -d "$DATA_DIR/predastore/cluster" ]; then
    log "removing the predastore cluster tree"
    run sudo rm -rf "$DATA_DIR/predastore/cluster"
fi

# Directories may survive — a mountpoint cannot be removed, and neither can the
# path leading down to one. Files may not: a surviving file is state that would
# carry into the new cluster, which is the exact failure this script exists to
# prevent, so refuse rather than hand back a node that looks clean.
if ! $DRY_RUN; then
    for dir in "${WIPE_DIRS[@]}"; do
        [ -d "$dir" ] || continue
        keep_args "$dir"
        left=$(sudo find "$dir" -mindepth 1 ! -type d ! -type l \
            ! \( "${KEEP_TOP[@]}" \) 2>/dev/null | head -5)
        if [ -n "$left" ]; then
            echo "ERROR: $dir still holds files after the wipe:" >&2
            echo "$left" | sed 's/^/  /' >&2
            exit 1
        fi
    done
fi

# Put the firewall policy back. Only this stage runs, so the node stays on the
# build it was already running — nothing is downloaded or reinstalled.
#
# A node that arrived armed and leaves unarmed is a failure, not a warning. It
# sits on a public address with nothing in front of it, and the only signal used
# to be a log line that reads as cosmetic. Exit non-zero so the caller stops
# rather than forming a cluster around it, and leave the stash in place so the
# retry still knows what mode to restore.
SETUP_SH=/usr/local/share/spinifex/setup.sh
if [ -n "$FIREWALL_MODE" ]; then
    if [ ! -x "$SETUP_SH" ]; then
        echo "ERROR: $SETUP_SH is missing, cannot restore the firewall policy." >&2
        echo "  This node was armed ($FIREWALL_MODE) and is now open. Reinstall it" >&2
        echo "  or arm it by hand before forming a cluster." >&2
        $DRY_RUN || exit 1
    fi
    log "restoring the host firewall policy (mode: $FIREWALL_MODE)"
    if run sudo env SETUP_STAGES=firewall "$SETUP_SH" --firewall "$FIREWALL_MODE"; then
        run sudo rm -f "$FIREWALL_STASH"
    else
        echo "ERROR: could not restore the firewall policy." >&2
        echo "  This node was armed ($FIREWALL_MODE) and is now open on its public" >&2
        echo "  addresses. Repair with:" >&2
        echo "    sudo env SETUP_STAGES=firewall $SETUP_SH --firewall $FIREWALL_MODE" >&2
        echo "    sudo systemctl restart spinifex-daemon" >&2
        $DRY_RUN || exit 1
    fi
fi

if [ ! -d /etc/spinifex ]; then
    echo "ERROR: /etc/spinifex is missing and could not be restored." >&2
    echo "  spx would treat this node as a dev install and build it under ~/spinifex," >&2
    echo "  forming a cluster whose services can never start. Re-run the installer." >&2
    $DRY_RUN || exit 1
fi

# A node that has already been through that fallback carries a stray dev-layout
# tree holding its own keys and configs. It is not user data — it is wreckage
# from a misdetected layout — and leaving it invites a later dev-mode run to
# adopt it.
DEV_ROOT="$(getent passwd spinifex | cut -d: -f6)/spinifex"
if [ -n "${DEV_ROOT#/spinifex}" ] && [ -d "$DEV_ROOT" ]; then
    log "removing the dev-layout fallback tree at $DEV_ROOT"
    run sudo rm -rf "$DEV_ROOT"
fi

# The next init writes a fresh CA. Leaving the old one trusted means the host
# trusts a CA nobody holds the key for.
if [ -f /usr/local/share/ca-certificates/spinifex-ca.crt ]; then
    log "removing the stale CA from the trust store"
    run sudo rm -f /usr/local/share/ca-certificates/spinifex-ca.crt
    run sudo update-ca-certificates
fi

log "done — node is installed but uninitialized, ready to init or join"
