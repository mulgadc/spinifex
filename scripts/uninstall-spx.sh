#!/bin/bash
# uninstall-spx.sh — remove a Spinifex install from this node.
#
# The inverse of setup.sh. Where node-reset.sh clears *state* and leaves the
# node installed and ready to re-init, this removes the *software*: the binary,
# the units, the service users, the helper scripts and the configuration setup.sh
# wrote. State teardown is delegated to node-reset.sh rather than reimplemented.
#
# Usage:
#   sudo /usr/local/share/spinifex/uninstall-spx.sh [options]
#
# Options:
#   --purge-data  Also remove /var/lib/spinifex — volumes, S3 objects, JetStream.
#                 Off by default: uninstalling software should not destroy data.
#   --purge-deps  Also remove the packages setup.sh installed, with apt. Refused
#                 on dnf hosts, where it cannot tell those from the host's own.
#                 Names them and confirms first, even under --yes. Off by default
#                 because a curl|bash install lands on hosts already using them.
#   --yes         Skip the confirmation prompt. Does NOT cover --purge-data,
#                 --purge-deps, or the multi-node refusal.
#   --dry-run     Print the whole plan, touch nothing, exit 0.
#
# Environment:
#   SPX_PURGE_CONFIRM=destroy   Unattended confirmation for --purge-data only.
#   SPX_ETC_DIR                 Override /etc/spinifex. Exists so the multi-node
#                               refusal can be tested against a fixture config.
#
# WHAT THIS REMOVES
#
# /usr/local/bin/spx, the nbdkit plugin, /usr/local/{share,lib}/spinifex,
# /usr/share/spinifex, every spinifex systemd unit and slice, the eight
# spinifex-* service users, the sudoers grant, the sysctl/logrotate/chrony/udev
# drop-ins, the host firewall policy and its loaded nft table, the OVS bridges
# and OVN databases, the Spinifex resolved drop-in, and the Spinifex CA from the
# host trust store. Where setup.sh turned on systemd-resolved, it restores
# /etc/resolv.conf and turns resolved off.
#
# WHAT THIS LEAVES ALONE
#
# /var/lib/spinifex unless --purge-data. Every package unless --purge-deps.
# /swapfile, which setup.sh may have created but cannot be told apart from one
# the host already had. Anything the operator configured around us: netplan,
# /etc/iptables/rules.v4, hand-written bridges. firewalld, which setup.sh
# masked, is unmasked but not started. Host settings setup.sh changed and other
# software may now rely on: /etc/modules-load.d/spinifex.conf, the confdir line
# appended to an EL /etc/chrony.conf, the dnf repositories and AppStream QEMU
# exclude, OVS_USER_ID and OVN_USER_ID, the OVS and OVN logrotate edits, masked
# IPsec units, the SELinux mode, and the GPU passthrough config spx admin gpu
# wrote. The run ends by listing each of these it finds.
set -euo pipefail

ETC_DIR="${SPX_ETC_DIR:-/etc/spinifex}"
DATA_DIR=/var/lib/spinifex
LOG_DIR=/var/log/spinifex
RUN_DIR=/run/spinifex
CONFIG_FILE="$ETC_DIR/spinifex.toml"
SHARE_DIR=/usr/local/share/spinifex
NODE_RESET="$SHARE_DIR/node-reset.sh"

PURGE_DATA=false
PURGE_DEPS=false
ASSUME_YES=false
DRY_RUN=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --purge-data) PURGE_DATA=true ;;
        --purge-deps) PURGE_DEPS=true ;;
        --yes | -y)   ASSUME_YES=true ;;
        --dry-run)    DRY_RUN=true ;;
        -h | --help)
            # Header comment block, minus the shebang. Taken up to the first
            # non-comment line rather than by line number, which drifts.
            sed -n '2,/^[^#]/p' "$0" | sed '/^[^#]/d; s/^# \?//'
            exit 0
            ;;
        *)
            echo "ERROR: unknown option: $1" >&2
            exit 1
            ;;
    esac
    shift
done

# Host-OS capability probes: beside this script in a checkout and in the
# installed share dir, else where setup.sh installs them.
DISTRO_LIB=""
for candidate in "$(dirname "${BASH_SOURCE[0]}")/lib/distro.sh" /usr/local/share/spinifex/lib/distro.sh; do
    if [ -f "$candidate" ]; then DISTRO_LIB="$candidate"; break; fi
done
if [ -z "$DISTRO_LIB" ]; then
    echo "ERROR: distro.sh not found at $(dirname "${BASH_SOURCE[0]}")/lib/distro.sh or /usr/local/share/spinifex/lib/distro.sh; re-run setup.sh to install it" >&2
    exit 1
fi
# shellcheck source=lib/distro.sh
. "$DISTRO_LIB"

# Resolved before anything is removed, so --purge-deps on a host with no package
# manager, or two, stops with nothing changed.
PKG_MANAGER=""
if $PURGE_DEPS; then
    PKG_MANAGER=$(distro_pkg_manager) || exit 1
    if [ "$PKG_MANAGER" = dnf ]; then
        echo "ERROR: --purge-deps is not supported on dnf hosts: its package list cannot tell what setup.sh installed from what the host already had, and dnf also removes every package that requires them. Re-run without --purge-deps." >&2
        exit 1
    fi
fi
# A failed OVS unit is restarted after its drop-in goes; absent when OVS is.
OVS_UNIT=$(distro_ovs_unit 2>/dev/null) || OVS_UNIT=""
# Resolved before anything is removed. The CA tools live in sbin, which Debian
# leaves off a non-root PATH, and failing late would stop the uninstall halfway.
CA_TRUST=$(PATH="/usr/sbin:/sbin:$PATH" distro_ca_trust) \
    || { echo "ERROR: cannot tell which trust store to remove the Spinifex CA from" >&2; exit 1; }

log() { echo "[uninstall-spx] $*"; }
run() {
    if $DRY_RUN; then
        echo "  would run: $*"
        return 0
    fi
    "$@"
}
# Paths are removed through one helper so --dry-run reports them uniformly and
# an absent path is never an error. Uninstalling a partial install is the
# ordinary case, not an exception.
#
# The existence test goes through sudo, not a bare [ -e ]. /etc/sudoers.d is
# 0750 root:root, so an unprivileged test reports "absent" for a file that is
# plainly there and the dry run then under-reports what it is about to delete —
# which is the one thing a dry run must never do.
exists() { sudo test -e "$1" || sudo test -L "$1"; }
rm_path() {
    for p in "$@"; do
        exists "$p" || continue
        if $DRY_RUN; then
            echo "  would remove: $p"
        else
            sudo rm -rf "$p"
        fi
    done
}

# ---------------------------------------------------------------------------
# 1. Refuse on a multi-node cluster that is still up.
#
# Uninstalling one node of a running cluster strands its guests and leaves the
# survivors reconciling against a chassis that will never answer. This is a
# structural check, not a prompt, so --yes does not cover it: a flag meaning
# "don't ask me" must not also mean "ignore a safety property".
# ---------------------------------------------------------------------------
NODES=()
if [ -r "$CONFIG_FILE" ]; then
    mapfile -t NODES < <(sudo grep -oP '^\[nodes\.\K[^.\]]+' "$CONFIG_FILE" 2>/dev/null | sort -u || true)
fi

if [ "${#NODES[@]}" -gt 1 ]; then
    cat >&2 <<EOF
ERROR: this node is configured as part of a ${#NODES[@]}-node cluster:
  ${NODES[*]}

Uninstalling one node of a running cluster strands its guests and leaves the
other nodes reconciling against a chassis that will never answer. Take the
cluster down first:

    spx admin cluster shutdown

then run this on each node. --yes does not override this check.
EOF
    exit 1
fi

# --- BEGIN at-stake counts ---
# Copied between node-reset.sh and uninstall-spx.sh; node-reset_test.sh keeps them equal.
# Instances are the daemon's local state records. Only mounted volumes have a
# local directory under viperblock/; the rest live only in Predastore.
count_instances() {
    local state="$1/state/instance-state.json"
    sudo test -e "$state" || { echo 0; return 0; }
    sudo jq -r '.vms // {} | length' "$state" 2>/dev/null || echo unknown
}
count_mounted_volumes() {
    sudo find "$1/viperblock" -maxdepth 1 -mindepth 1 -type d -name 'vol-*' ! -name '*-efi' 2>/dev/null | wc -l || true
}
# --- END at-stake counts ---

# ---------------------------------------------------------------------------
# 2. Report what is at stake, then confirm.
# ---------------------------------------------------------------------------
log "on $(hostname): removing the Spinifex install"
if command -v spx >/dev/null 2>&1; then
    log "  version: $(spx version 2>/dev/null | head -1 || echo unknown)"
fi
if [ -d "$DATA_DIR" ]; then
    size=$(sudo du -sh "$DATA_DIR" 2>/dev/null | cut -f1 || true)
    instances=$(count_instances "$DATA_DIR")
    volumes=$(count_mounted_volumes "$DATA_DIR")
    if $PURGE_DATA; then
        log "  --purge-data: DESTROYING $instances instance(s), $volumes mounted volume(s), ${size:-unknown} under $DATA_DIR"
    else
        log "  keeping $DATA_DIR ($instances instance(s), $volumes mounted volume(s), ${size:-unknown}) — pass --purge-data to remove it"
    fi
fi
$PURGE_DEPS && log "  --purge-deps: $PKG_MANAGER packages will be removed (confirmed separately below)"

if ! $ASSUME_YES && ! $DRY_RUN; then
    read -r -p "Remove the Spinifex install from this node? Type 'uninstall' to continue: " reply || reply=""
    [ "$reply" = "uninstall" ] || { log "aborted"; exit 1; }
fi

# --purge-data is a second decision with a different blast radius, so it takes
# its own confirmation even when --yes was given for the uninstall itself.
#
# SPX_PURGE_CONFIRM=destroy is the unattended escape, matching node-reset.sh's
# SPX_RESET_CONFIRM and install-node.sh's SPX_WIPE_CONFIRM. It is deliberately a
# separate variable from --yes: a caller that wants no prompts still has to name
# the data destruction specifically, so no existing automation acquires it by
# adding --yes.
if $PURGE_DATA && ! $DRY_RUN; then
    if [ "${SPX_PURGE_CONFIRM:-}" = "destroy" ]; then
        log "SPX_PURGE_CONFIRM=destroy: proceeding without a prompt"
    else
        read -r -p "Also destroy every volume and object under $DATA_DIR? Type 'destroy' to continue: " reply || reply=""
        [ "$reply" = "destroy" ] || { log "aborted"; exit 1; }
    fi
fi

# ---------------------------------------------------------------------------
# 3. Stop everything, and confirm it stopped.
#
# `systemctl stop spinifex.target` returns once the target is inactive, which is
# not the same as its services being stopped: spinifex-shutdown.service is
# ordered After= the storage services, so their stop jobs queue behind its
# ExecStop. Removing the binary out from under a running service leaves it
# executing an unlinked image, and removing the users out from under one is
# worse. node-reset.sh and update-nodes.sh both carry this same wait.
# ---------------------------------------------------------------------------
log "stopping services"
run sudo systemctl stop spinifex.target 2>/dev/null || true
if ! $DRY_RUN; then
    for unit in spinifex-predastore spinifex-viperblock spinifex-sqs-store spinifex-nats; do
        if systemctl is-active --quiet "$unit"; then
            sudo systemctl stop "$unit" || true
        fi
    done
    elapsed=0
    while [ -n "$(pgrep -x spx || true)" ]; do
        if [ "$elapsed" -ge 120 ]; then
            echo "ERROR: spx still running after 120s:" >&2
            pgrep -ax spx >&2 || true
            echo "  Removing the install under a live service leaves it running an" >&2
            echo "  unlinked binary. Stop them manually and re-run." >&2
            exit 1
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
fi

# ---------------------------------------------------------------------------
# 4. Delegate state teardown to node-reset.sh.
#
# It handles the guests, the OVS bridges, the OVN databases and the JetStream
# store, and it carries correctness that is invisible in its code: QEMU matched
# by a comm prefix because comm truncates at 15 characters, a wait for QEMU to
# exit rather than for the signal, and the firewall-mode stash that stops an
# interrupted run leaving the node unarmed. A second copy of that would drift,
# and the failure mode of the drift is data loss.
#
# --keep-data unless we are purging: node-reset's default is to wipe, ours is
# not, and the flag is how the two defaults are reconciled.
# ---------------------------------------------------------------------------
if [ -x "$NODE_RESET" ]; then
    log "clearing node state via $NODE_RESET"
    reset_args=(--yes)
    $PURGE_DATA || reset_args+=(--keep-data)
    $DRY_RUN && reset_args+=(--dry-run)
    if ! sudo "$NODE_RESET" "${reset_args[@]}"; then
        echo "ERROR: node-reset.sh failed; stopping before removing the install." >&2
        echo "  The node still has its software, so it can be repaired and retried." >&2
        exit 1
    fi
else
    log "NOTE: $NODE_RESET is not installed — skipping the state teardown"
    log "      guests, OVS bridges and OVN databases are left as they are"
fi

# ---------------------------------------------------------------------------
# 5. Unload the host firewall before deleting the files behind it.
#
# The nft ruleset lives in the kernel; removing spinifex.nft does not unload it.
# A node that uninstalls Spinifex and keeps a default-deny input policy with no
# file behind it is unreachable after the next reboot and unexplainable to
# whoever inherits it.
#
# Only our own table and our own marked rules, never a global flush: setup.sh
# creates exactly `table inet spinifex_filter`, and vpcd writes iptables rules
# carrying the comments below. Anything else on this host belongs to someone
# else.
# ---------------------------------------------------------------------------
log "unloading the host firewall policy"
if command -v nft >/dev/null 2>&1; then
    if sudo nft list table inet spinifex_filter >/dev/null 2>&1; then
        run sudo nft delete table inet spinifex_filter
    else
        log "  no inet spinifex_filter table loaded"
    fi
fi
if command -v iptables-save >/dev/null 2>&1 && ! $DRY_RUN; then
    for marker in spinifex-nat-egress spinifex-eip-ingress spinifex-imds spinifex-imds-remap; do
        while read -r table rule; do
            [ -n "$rule" ] || continue
            # shellcheck disable=SC2086
            sudo iptables -t "$table" -D $rule 2>/dev/null || true
        done < <(sudo iptables-save | awk -v m="$marker" '
            /^\*/ { t = substr($0, 2) }
            $0 ~ m && /^-A/ { sub(/^-A /, ""); print t, $0 }')
    done
elif $DRY_RUN; then
    echo "  would remove iptables rules commented spinifex-nat-egress / spinifex-eip-ingress / spinifex-imds / spinifex-imds-remap"
fi
# setup.sh masks firewalld. Unmasked but left disabled: its rules were never
# migrated, so starting it is the operator's decision. A mask keeps the enable
# links, so without the disable it would start again at the next boot.
FIREWALLD_UNMASKED=false
if [ "$(systemctl is-enabled firewalld.service 2>/dev/null)" = masked ]; then
    run sudo systemctl unmask firewalld.service
    run sudo systemctl disable firewalld.service 2>/dev/null || true
    FIREWALLD_UNMASKED=true
    $DRY_RUN || log "  firewalld unmasked, left disabled"
fi

# ---------------------------------------------------------------------------
# 6. systemd units.
# ---------------------------------------------------------------------------
log "removing systemd units"
if ! $DRY_RUN; then
    sudo systemctl disable --now spinifex.target 2>/dev/null || true
    for u in /etc/systemd/system/spinifex-*.service /etc/systemd/system/spinifex-*.timer; do
        [ -e "$u" ] || continue
        sudo systemctl disable --now "$(basename "$u")" 2>/dev/null || true
    done
fi
rm_path /etc/systemd/system/spinifex-*.service \
        /etc/systemd/system/spinifex-*.timer \
        /etc/systemd/system/spinifex.target \
        /etc/systemd/system/spinifex.slice \
        /etc/systemd/system/spinifex-system.slice \
        /etc/systemd/system/spinifex-guests.slice \
        /etc/systemd/system/system.slice.d/spinifex-reserve.conf

# Drop-ins we wrote into *other people's* units. These are the dangerous ones:
# they are not in a spinifex-named path, so a namespace glob misses them, and
# each holds an ExecStartPost pointing at a helper under /usr/local/lib/spinifex
# that the step below removes. Left behind, openvswitch-switch fails to start
# with `Failed at step EXEC ... ovs-socket-perms.sh: No such file or directory`
# — the uninstall breaks OVS for whatever else on the host was using it. Found
# exactly that way on the first dev-oci round trip.
log "removing drop-ins written into other units"
rm_path /etc/systemd/system/openvswitch-switch.service.d/spinifex-perms.conf \
        /etc/systemd/system/ovs-vswitchd.service.d/spinifex-perms.conf \
        /etc/systemd/system/ovn-controller.service.d/spinifex-perms.conf \
        /etc/systemd/system/ovn-controller.service.d/log-level.conf \
        /etc/systemd/system/openvswitch-ipsec.service.d/spinifex-perms.conf \
        /etc/systemd/system/ovn-northd.service.d/spinifex-perms.conf \
        /etc/systemd/system/ovn-northd.service.d/no-manage-ovsdb.conf \
        /etc/systemd/system/ipsec.service.d/spinifex-guard.conf
# Only if we emptied them — another package may own the same override dir.
for d in openvswitch-switch ovs-vswitchd ovn-controller openvswitch-ipsec ovn-northd ipsec; do
    dir="/etc/systemd/system/${d}.service.d"
    if sudo test -d "$dir" && [ -z "$(sudo ls -A "$dir" 2>/dev/null)" ]; then
        rm_path "$dir"
    fi
done

run sudo systemctl daemon-reload
run sudo systemctl reset-failed 2>/dev/null || true

# The units we just un-broke may be sitting in a failed state from an earlier
# attempt. Restarting them here is not housekeeping: openvswitch-switch failing
# is how the operator discovers the drop-in problem, and leaving it failed after
# an uninstall that claims success is the same silent failure one step later.
# Each unit that carried one of the drop-ins above, under this host's OVS name.
RESTART_UNITS=(ovs-vswitchd ovn-controller ovn-northd)
[ -n "$OVS_UNIT" ] && RESTART_UNITS=("$OVS_UNIT" "${RESTART_UNITS[@]}")
if ! $DRY_RUN; then
    for u in "${RESTART_UNITS[@]}"; do
        if systemctl is-failed --quiet "$u" 2>/dev/null; then
            log "  restarting $u, which our drop-in had failed"
            sudo systemctl reset-failed "$u" 2>/dev/null || true
            sudo systemctl restart "$u" 2>/dev/null || \
                log "  NOTE: $u did not restart — check it by hand"
        fi
    done
fi

# strongswan's charon profile: a site-override file setup-ovn.sh writes whole
# (not appends to) granting reads on /etc/spinifex, which is about to not exist.
if sudo test -f /etc/apparmor.d/local/usr.lib.ipsec.charon; then
    log "removing the AppArmor grant for /etc/spinifex"
    rm_path /etc/apparmor.d/local/usr.lib.ipsec.charon
    run sudo apparmor_parser -r /etc/apparmor.d/usr.lib.ipsec.charon 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# 7. systemd-networkd units written by setup-ovn.sh.
#
# Without these the veth and transit-veth pairs are recreated on the next boot,
# long after the software that used them is gone. Removed by glob because the
# veth-wan set (15/16) and the routed-NAT set (17/18) are both ours and a
# hardcoded list has already missed one of them once.
# ---------------------------------------------------------------------------
if compgen -G "/etc/systemd/network/1[5-8]-spinifex-*" >/dev/null; then
    log "removing systemd-networkd units"
    rm_path /etc/systemd/network/1[5-8]-spinifex-*
    run sudo networkctl reload 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# 8. Installed files.
#
# The nbdkit plugin does not live under any spinifex-owned path — it goes into
# nbdkit's own plugin directory — so it is the one file here that is easy to
# leave behind. Ask nbdkit where that is rather than guessing, and fall back to
# the multiarch paths setup.sh uses when nbdkit has already been removed.
# ---------------------------------------------------------------------------
log "removing installed files"
PLUGINDIR=$(nbdkit --dump-config 2>/dev/null | sed -n 's/^plugindir=//p' || true)
if [ -z "$PLUGINDIR" ]; then
    for d in /usr/lib/x86_64-linux-gnu/nbdkit/plugins /usr/lib/aarch64-linux-gnu/nbdkit/plugins; do
        [ -d "$d" ] && PLUGINDIR="$d" && break
    done
fi
[ -n "$PLUGINDIR" ] && rm_path "$PLUGINDIR/nbdkit-viperblock-plugin.so"

# A leftover guard table drops all Geneve from this host's encap address until
# reboot, and the helper is the one place that names it.
if [ -x /usr/local/lib/spinifex/spinifex-set-ipsec-state ]; then
    run sudo /usr/local/lib/spinifex/spinifex-set-ipsec-state unguard || true
fi
rm_path /usr/local/bin/spx \
        /usr/local/share/spinifex \
        /usr/local/lib/spinifex \
        /usr/share/spinifex
# setup.sh adds this link where sudo's PATH leaves out /usr/local/bin.
if [ "$(readlink /usr/bin/spx 2>/dev/null)" = /usr/local/bin/spx ]; then
    rm_path /usr/bin/spx
fi

# ---------------------------------------------------------------------------
# 9. Drop-ins. Globbed, not listed: 99-spinifex-vpc.conf is written at runtime
# by the daemon through spinifex-set-endpoint-sysctl and does not appear
# anywhere in setup.sh, so an inventory built by reading the installer alone
# leaves behind the drop-in that changes forwarding and rp_filter.
# ---------------------------------------------------------------------------
log "removing configuration drop-ins"
rm_path /etc/sysctl.d/*spinifex* \
        /etc/sudoers.d/spinifex-network \
        /etc/chrony/conf.d/spinifex.conf \
        /etc/chrony.d/spinifex.conf \
        /etc/logrotate.d/spinifex \
        /etc/tmpfiles.d/spinifex.conf \
        /etc/udev/rules.d/*spinifex* \
        /etc/NetworkManager/conf.d/*spinifex*
run sudo sysctl --system >/dev/null 2>&1 || true
if systemctl is-active --quiet NetworkManager.service 2>/dev/null; then
    run sudo nmcli general reload conf 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# 10. Spinifex directories.
#
# node-reset.sh emptied these of files; what is left is the structure setup.sh
# built, which is install state and goes now. /var/lib/spinifex only under
# --purge-data — and even then, only if it is not a mountpoint, because on an
# ISO-installed node several of these are separate ZFS datasets and a
# mountpoint cannot be removed.
# ---------------------------------------------------------------------------
log "removing spinifex directories"
rm_path "$ETC_DIR" "$LOG_DIR" "$RUN_DIR"
if $PURGE_DATA; then
    if mountpoint -q "$DATA_DIR" 2>/dev/null; then
        log "NOTE: $DATA_DIR is a mountpoint — emptying it rather than removing it"
        if ! $DRY_RUN; then
            sudo find "$DATA_DIR" -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true
        else
            echo "  would empty: $DATA_DIR"
        fi
    else
        rm_path "$DATA_DIR"
    fi
else
    log "  keeping $DATA_DIR"
fi

# ---------------------------------------------------------------------------
# 11. Service users and groups.
#
# After the directories, because a user's home directory is under them and
# userdel warns about a missing home. Order here is cosmetic, not correctness.
# ---------------------------------------------------------------------------
log "removing service users and groups"
for svc in nats sqsstore gw daemon storage northstar viperblock vpcd ui; do
    user="spinifex-${svc}"
    if id -u "$user" >/dev/null 2>&1; then
        run sudo userdel "$user" 2>/dev/null || log "  NOTE: could not remove user $user"
    fi
done
for grp in spinifex-viperblock spinifex; do
    if getent group "$grp" >/dev/null 2>&1; then
        run sudo groupdel "$grp" 2>/dev/null || log "  NOTE: could not remove group $grp (still has members?)"
    fi
done

# ---------------------------------------------------------------------------
# 12. The CA in the host trust store. Leaving it means the host trusts a CA
# whose key no longer exists anywhere.
# ---------------------------------------------------------------------------
CA_ANCHOR="${CA_TRUST#* }/spinifex-ca.crt"
if [ -f "$CA_ANCHOR" ]; then
    log "removing the Spinifex CA from the host trust store"
    rm_path "$CA_ANCHOR"
    run sudo "${CA_TRUST%% *}"
fi

# ---------------------------------------------------------------------------
# 13. Host DNS. setup.sh saved /etc/resolv.conf only where it turned on
# systemd-resolved, so the saved copy is what says to turn it off again.
# ---------------------------------------------------------------------------
RESOLV_CONF_SAVED=/var/lib/spinifex-reset/resolv.conf.orig
# The daemon's drop-in routes the cluster zones to a northstar that is gone now.
RESOLVED_DROPIN=/etc/systemd/resolved.conf.d/spinifex-dns.conf
if exists "$RESOLVED_DROPIN"; then
    log "removing the systemd-resolved drop-in for the cluster zones"
    rm_path "$RESOLVED_DROPIN"
    if systemctl is-active --quiet systemd-resolved.service 2>/dev/null; then
        run sudo systemctl restart systemd-resolved.service
    fi
fi
if exists "$RESOLV_CONF_SAVED"; then
    log "restoring /etc/resolv.conf and turning systemd-resolved off, as before the install"
    run sudo systemctl disable --now systemd-resolved.service
    run sudo cp -a --remove-destination "$RESOLV_CONF_SAVED" /etc/resolv.conf
    if systemctl is-active --quiet NetworkManager.service 2>/dev/null; then
        run sudo systemctl restart NetworkManager.service
    fi
    rm_path "$RESOLV_CONF_SAVED"
fi
# node-reset keeps its firewall-mode stash here too, so remove it only once empty.
run sudo rmdir --ignore-fail-on-non-empty "$(dirname "$RESOLV_CONF_SAVED")" 2>/dev/null || true

# ---------------------------------------------------------------------------
# 14. Swap is reported, never removed.
#
# setup_swap creates an 8G /swapfile with the fstab line `/swapfile none swap sw
# 0 0` — which is byte-identical to the line a host would already have if it
# provisioned its own swap the ordinary way. There is no marker, so there is no
# way to tell ours from theirs, and removing a host's swap because we might have
# made it is not a trade worth taking. Say it is there and let the operator
# decide.
# ---------------------------------------------------------------------------
if [ -f /swapfile ] && grep -q '^/swapfile ' /etc/fstab 2>/dev/null; then
    log "NOTE: /swapfile is left in place. setup.sh may have created it, but the"
    log "      fstab line is indistinguishable from a host's own. To remove it:"
    log "        sudo swapoff /swapfile && sudo rm -f /swapfile"
    log "        then delete its line from /etc/fstab"
fi

# ---------------------------------------------------------------------------
# 15. Packages, only when asked, and only after naming them.
#
# This is the one irreversible thing in the script. On a dedicated node these
# are ours; on the host a curl|bash install actually lands on they are someone
# else's, and openvswitch/libvirt/qemu/chrony going away underneath a running
# workload is a far worse outcome than a few megabytes left behind. Confirmed
# separately even under --yes, and never autoremove, which is unbounded.
#
# apt only: dnf hosts are refused up front.
# ---------------------------------------------------------------------------
if $PURGE_DEPS; then
    PACKAGES="nbdkit qemu-utils gdisk ovmf less libvirt-daemon-system libvirt-clients
pciutils jq ethtool netcat-openbsd unzip xz-utils
ovn-central ovn-host openvswitch-switch openvswitch-ipsec strongswan-charon
chrony nftables"
    KEPT="curl, wget, file, iproute2, dhcpcd-base, qemu-system-*"
    REMOVE_CMD="apt-get remove -y"
    log "--purge-deps: these packages would be removed:"
    # shellcheck disable=SC2086
    for p in $PACKAGES; do echo "    $p"; done
    log "  NOT removed: $KEPT — too"
    log "  widely depended on to remove from a host we do not own."
    if [ -z "$PACKAGES" ]; then
        log "  none of them is installed"
    elif $DRY_RUN; then
        echo "  would run: $REMOVE_CMD <the above>"
    else
        read -r -p "Remove these packages? Type 'remove' to continue: " reply || reply=""
        if [ "$reply" = "remove" ]; then
            # shellcheck disable=SC2086
            sudo DEBIAN_FRONTEND=noninteractive apt-get remove -y $PACKAGES || \
                log "NOTE: apt-get remove reported errors; check the output above"
        else
            log "  skipped"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 16. Verify. An uninstaller that reports success over a surviving unit file is
# worse than one that refuses, because the next install silently inherits it.
# ---------------------------------------------------------------------------
if ! $DRY_RUN; then
    log "verifying"
    LEFT=()
    for p in /usr/local/bin/spx /usr/local/share/spinifex /usr/local/lib/spinifex \
             /usr/share/spinifex "$ETC_DIR" "$LOG_DIR" \
             /etc/sudoers.d/spinifex-network /etc/logrotate.d/spinifex /etc/tmpfiles.d/spinifex.conf \
             /etc/chrony/conf.d/spinifex.conf /etc/chrony.d/spinifex.conf "$RESOLV_CONF_SAVED" \
             "$RESOLVED_DROPIN"; do
        exists "$p" && LEFT+=("$p")
    done
    [ "$(readlink /usr/bin/spx 2>/dev/null)" = /usr/local/bin/spx ] && LEFT+=(/usr/bin/spx)
    for p in /etc/systemd/system/spinifex-*.service /etc/systemd/system/spinifex*.slice \
             /etc/systemd/system/spinifex.target /etc/sysctl.d/*spinifex* \
             /etc/systemd/network/1[5-8]-spinifex-* \
             /etc/NetworkManager/conf.d/*spinifex* \
             /etc/systemd/system/*.service.d/spinifex-perms.conf \
             /etc/systemd/system/ovn-northd.service.d/no-manage-ovsdb.conf \
             /etc/systemd/system/ipsec.service.d/spinifex-guard.conf \
             /etc/systemd/system/ovn-controller.service.d/log-level.conf; do
        exists "$p" && LEFT+=("$p")
    done
    $FIREWALLD_UNMASKED && systemctl is-enabled --quiet firewalld.service 2>/dev/null &&
        LEFT+=("firewalld.service is enabled")
    # A unit we broke and could not restart is a failed uninstall, not a wart.
    for u in "${RESTART_UNITS[@]}"; do
        systemctl is-failed --quiet "$u" 2>/dev/null && LEFT+=("${u%.service}.service is failed")
    done
    for svc in nats sqsstore gw daemon storage northstar viperblock vpcd ui; do
        id -u "spinifex-${svc}" >/dev/null 2>&1 && LEFT+=("user spinifex-${svc}")
    done
    if command -v nft >/dev/null 2>&1 && sudo nft list table inet spinifex_filter >/dev/null 2>&1; then
        LEFT+=("nft table inet spinifex_filter")
    fi
    $PURGE_DATA && [ -e "$DATA_DIR" ] && ! mountpoint -q "$DATA_DIR" 2>/dev/null && LEFT+=("$DATA_DIR")

    if [ "${#LEFT[@]}" -gt 0 ]; then
        echo "ERROR: the uninstall did not complete. Still present:" >&2
        printf '  %s\n' "${LEFT[@]}" >&2
        exit 1
    fi
fi

# ---------------------------------------------------------------------------
# 17. Host settings setup.sh changed and this leaves, each listed only where it
# is found, so the operator can see what to undo by hand.
# ---------------------------------------------------------------------------
KEPT_SETTINGS=()
kept() { KEPT_SETTINGS+=("$1"); }
$FIREWALLD_UNMASKED && kept "firewalld: unmasked but disabled, with the rules it had before Spinifex"
exists /etc/modules-load.d/spinifex.conf && kept "/etc/modules-load.d/spinifex.conf: openvswitch geneve vhost_net tun load at boot"
CHRONY_CONF=$(distro_chrony_conf 2>/dev/null) || CHRONY_CONF=""
if [ -n "$CHRONY_CONF" ] && grep -q '^# Added by the Spinifex installer' "$CHRONY_CONF" 2>/dev/null; then
    kept "$CHRONY_CONF: the confdir line the installer appended"
fi
if command -v dnf >/dev/null 2>&1; then
    exists /etc/yum.repos.d/spinifex-nfv-sig.repo &&
        kept "dnf: the CentOS NFV SIG repository (/etc/yum.repos.d/spinifex-nfv-sig.repo) and its key in /etc/pki/rpm-gpg"
    for repo in ol10_kvm_utils ol10_UEKR8; do
        dnf config-manager --dump "$repo" 2>/dev/null | grep -qx 'enabled = 1' && kept "dnf: $repo enabled"
    done
    dnf config-manager --dump ol10_appstream 2>/dev/null | grep -q '^excludepkgs = .*qemu-kvm' &&
        kept "dnf: ol10_appstream excludes qemu-kvm* and qemu-img*, so QEMU stays on KVM Utilities"
fi
for setting in /etc/sysconfig/openvswitch:OVS_USER_ID /etc/sysconfig/ovn:OVN_USER_ID; do
    grep -q "^${setting#*:}=\"root:root\"" "${setting%%:*}" 2>/dev/null &&
        kept "${setting%%:*}: ${setting#*:}=\"root:root\""
done
for lr in "$(distro_ovs_logrotate 2>/dev/null)" "$(distro_ovn_logrotate 2>/dev/null)"; do
    [ -n "$lr" ] && grep -q 'maxsize 100M' "$lr" 2>/dev/null && kept "$lr: the rotate, maxsize and su edits setup-ovn.sh made"
done
IPSEC_UNITS=(openvswitch-ipsec.service strongswan-starter.service)
distro_has_libreswan && IPSEC_UNITS+=(ipsec.service)
for u in "${IPSEC_UNITS[@]}"; do
    [ "$(systemctl is-enabled "$u" 2>/dev/null)" = masked ] && kept "$u: masked, as IPsec was off"
done
command -v getenforce >/dev/null 2>&1 && kept "SELinux: $(getenforce 2>/dev/null); setup.sh refuses enforcing, so it was set for the install"
GPU_FILES=""
for gpu in /etc/modprobe.d/vfio-pci.conf /etc/dracut.conf.d/spinifex-vfio.conf; do
    exists "$gpu" && GPU_FILES="${GPU_FILES:+$GPU_FILES, }$gpu"
done
[ -n "$GPU_FILES" ] && kept "GPU passthrough: $GPU_FILES and the IOMMU kernel arguments, written by spx admin gpu setup"
if [ "${#KEPT_SETTINGS[@]}" -gt 0 ]; then
    log "left in place, as other software on the host may rely on them:"
    for k in "${KEPT_SETTINGS[@]}"; do log "  - $k"; done
fi

log "done — Spinifex is removed from this node"
$PURGE_DATA || { [ -d "$DATA_DIR" ] && log "      $DATA_DIR was kept (--purge-data removes it)"; }
$PURGE_DEPS || log "      package dependencies were kept (--purge-deps removes them)"
exit 0
