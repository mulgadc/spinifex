#!/bin/sh
# setup-ovn_test.sh — tests for the teardown path and its documentation.
#
# Only --teardown --dry-run and --help are exercised. Both return before the
# script changes anything, which matters more here than usual: the script under
# test wires the node's datapath, and a test that ran the real teardown would
# delete the bridges of the machine running CI.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SETUP="$SCRIPT_DIR/setup-ovn.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- setup-ovn.sh"

# --------------------------------------------------------------------------
# Help.
# --------------------------------------------------------------------------
help_out=$("$SETUP" --help 2>&1) && rc=0 || rc=$?
[ "$rc" = 0 ] && pass "--help exits 0" || fail "--help exited $rc, want 0"

for flag in --teardown --dry-run --management --nat-uplink; do
    case "$help_out" in
        *"$flag"*) pass "--help documents $flag" ;;
        *) fail "--help does not document $flag" ;;
    esac
done

# --teardown is the flag an operator reaches for when a node is already
# tangled, so the help has to say what it keeps. Somebody who reads it as
# "removes everything" will not run it, and will hand-edit OVS instead.
case "$help_out" in
    *"NB/SB databases"*) pass "--help says what --teardown preserves" ;;
    *) fail "--help does not say what --teardown preserves" ;;
esac

# --------------------------------------------------------------------------
# Teardown, dry run. Every mutation must go through run(), so a dry run can
# report the whole plan without performing any of it.
# --------------------------------------------------------------------------
out=$("$SETUP" --teardown --dry-run 2>&1) && rc=0 || rc=$?
[ "$rc" = 0 ] && pass "--teardown --dry-run exits 0" || fail "--teardown --dry-run exited $rc, want 0"

case "$out" in
    *"nothing is changed"*) pass "dry run says it changes nothing" ;;
    *) fail "dry run does not announce itself" ;;
esac

# The four groups the teardown exists to remove. A silently skipped group is
# the failure mode that leaves a node still tangled after a clean-looking run.
for step in "Removing veth pairs" "Removing systemd-networkd persistence" \
    "Removing routed-NAT egress rules" "Removing OVS bridges"; do
    case "$out" in
        *"$step"*) pass "dry run covers: $step" ;;
        *) fail "dry run skips: $step" ;;
    esac
done

# The OVS port goes before the veth: deleting one end of a pair deletes the
# peer, and an OVS port left naming a device the kernel no longer has is
# exactly the state a re-run cannot unpick.
port_line=$(printf '%s\n' "$out" | grep -n 'del-port veth-wan-ovs' | head -1 | cut -d: -f1 || true)
link_line=$(printf '%s\n' "$out" | grep -n 'ip link del veth-wan-br' | head -1 | cut -d: -f1 || true)
if [ -n "$port_line" ] && [ -n "$link_line" ] && [ "$port_line" -lt "$link_line" ]; then
    pass "the OVS port is removed before its veth"
else
    fail "veth teardown order is wrong (port=$port_line link=$link_line)"
fi

# A dry run that reported a real mutation would be the worst kind of bug here.
if printf '%s\n' "$out" | grep -E '^[[:space:]]+(sudo|ip|ovs-vsctl|iptables) ' >/dev/null 2>&1; then
    fail "dry run printed a bare command, not a 'would run:' line"
else
    pass "every dry-run mutation is reported, not executed"
fi

# --------------------------------------------------------------------------
# Static checks on the teardown body.
# --------------------------------------------------------------------------
# Comments and echoed prose name the things the teardown deliberately spares,
# so match against the commands alone or every such sentence reads as a hit.
body=$(sed -n '/^teardown() {/,/^}/p' "$SETUP")
body_cmds=$(printf '%s\n' "$body" | grep -vE '^[[:space:]]*(#|echo )' || true)

# Deleting a Linux bridge would take the host's own addressing with it: on a
# cloud node br-wan is a netplan-owned Linux bridge holding the VNIC's address
# and the default route, and this script never created it.
case "$body" in
    *is_linux_bridge*) pass "teardown refuses to delete a Linux bridge" ;;
    *) fail "teardown does not check for a Linux bridge before del-br" ;;
esac

# The databases are the control plane's state. A teardown that took them could
# not be used for its actual purpose, which is re-running this script.
for forbidden in "ovnnb_db" "ovnsb_db" "/etc/spinifex" "rm -rf"; do
    case "$body_cmds" in
        *"$forbidden"*) fail "teardown touches $forbidden" ;;
        *) pass "teardown leaves $forbidden alone" ;;
    esac
done

# --------------------------------------------------------------------------
# The transit veth MAC.
# --------------------------------------------------------------------------
# OVN keeps one MAC binding for the transit nexthop across the cluster, so a
# random per-node MAC sends every chassis's egress to whichever node seeded it.
# The address must therefore be a literal the script sets, and it must survive a
# reboot or the node goes dark the next time systemd-networkd recreates the pair.
nat_mode_body=$(sed -n '/^        nat)/,/^            ;;/p' "$SETUP")

case "$nat_mode_body" in
    *'ip link set spx-nat-host address'*) pass "nat mode pins the transit host MAC" ;;
    *) fail "nat mode does not set a MAC on spx-nat-host" ;;
esac

case "$nat_mode_body" in
    *'MACAddress=$NAT_TRANSIT_HOST_MAC'*) pass "the netdev unit persists the transit MAC" ;;
    *) fail "the netdev unit does not persist the transit MAC" ;;
esac

# The script and the Go constant must agree: vpcd repairs a veth made before
# this existed, and a disagreement would have the two fighting every pass.
script_mac=$(printf '%s\n' "$nat_mode_body" | sed -n 's/.*NAT_TRANSIT_HOST_MAC="\([^"]*\)".*/\1/p' | head -1)
go_mac=$(sed -n 's/.*NATTransitHostMAC = "\([^"]*\)".*/\1/p' "$SCRIPT_DIR/../spinifex/network/host/routed.go" | head -1)
if [ -n "$script_mac" ] && [ "$script_mac" = "$go_mac" ]; then
    pass "the transit MAC matches host.NATTransitHostMAC"
else
    fail "transit MAC disagrees: script=$script_mac go=$go_mac"
fi

if [ "$fails" -ne 0 ]; then
    echo "  setup-ovn.sh: $fails failure(s)"
    exit 1
fi
echo "  setup-ovn.sh ok"
