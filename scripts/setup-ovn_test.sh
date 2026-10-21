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
# The teardown plans the veth steps only when ovs-vsctl exists and the link is
# present, so stub both or the order check depends on the machine running it.
mkdir "$TMP/bin"
printf '#!/bin/sh\nexit 0\n' > "$TMP/bin/ip"
cp "$TMP/bin/ip" "$TMP/bin/ovs-vsctl"
chmod +x "$TMP/bin/ip" "$TMP/bin/ovs-vsctl"
out=$(PATH="$TMP/bin:$PATH" "$SETUP" --teardown --dry-run 2>&1) && rc=0 || rc=$?
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
script_mac=$(sed -n 's/^NAT_TRANSIT_HOST_MAC="\([^"]*\)".*/\1/p' "$SETUP" | head -1)
go_mac=$(sed -n 's/.*NATTransitHostMAC = "\([^"]*\)".*/\1/p' "$SCRIPT_DIR/../spinifex/domains/network/host/routed.go" | head -1)
if [ -n "$script_mac" ] && [ "$script_mac" = "$go_mac" ]; then
    pass "the transit MAC matches host.NATTransitHostMAC"
else
    fail "transit MAC disagrees: script=$script_mac go=$go_mac"
fi

# --------------------------------------------------------------------------
# OVS and OVN service users.
# --------------------------------------------------------------------------
# EL runs OVS and OVN as non-root users and re-chowns their sockets on every
# start. The function is run on its own against scratch files, through a sudo
# that only runs its arguments.
awk '$0 == "set_root_service_user() {" { on = 1 } on { print } on && $0 == "}" { exit }' "$SETUP" >"$TMP/user_fn.sh"
mkdir -p "$TMP/bin"
printf '#!/bin/sh\nexec "$@"\n' >"$TMP/bin/sudo"
chmod +x "$TMP/bin/sudo"

# set_user <file> — runs the function on <file> for OVS_USER_ID; sets RC.
set_user() {
    RC=0
    PATH="$TMP/bin:$PATH" bash -c '. "$1"; set_root_service_user "$2" OVS_USER_ID' _ "$TMP/user_fn.sh" "$1" >/dev/null || RC=$?
}

set_user "$TMP/absent"
if [ "$RC" != 0 ] && [ ! -e "$TMP/absent" ]; then pass "service user: no file, no change"; else fail "service user: absent file gave rc=$RC or was created"; fi

printf '# OVS_USER_ID="openvswitch:openvswitch"\nOPTIONS=""\nOVS_USER_ID="openvswitch:hugetlbfs"\n' >"$TMP/el"
set_user "$TMP/el"
want='# OVS_USER_ID="openvswitch:openvswitch"
OPTIONS=""
OVS_USER_ID="root:root"'
if [ "$RC" = 0 ] && [ "$(cat "$TMP/el")" = "$want" ]; then pass "service user: EL default replaced in place, comments and other lines kept"; else fail "service user: EL file rc=$RC: $(cat "$TMP/el")"; fi

set_user "$TMP/el"
if [ "$RC" != 0 ] && [ "$(cat "$TMP/el")" = "$want" ]; then pass "service user: already root reports no change"; else fail "service user: second run rc=$RC: $(cat "$TMP/el")"; fi

printf 'OPTIONS=""\n' >"$TMP/unset"
set_user "$TMP/unset"
if [ "$RC" = 0 ] && [ "$(cat "$TMP/unset")" = 'OPTIONS=""
OVS_USER_ID="root:root"' ]; then pass "service user: appended when the file does not set it"; else fail "service user: unset file rc=$RC: $(cat "$TMP/unset")"; fi

# --------------------------------------------------------------------------
# Host network persistence backend.
# --------------------------------------------------------------------------
# The backend is picked once and recorded; every later run reads the record.
# The functions run on their own against scratch paths, with systemctl stubbed.
for fn in select_hostnet_backend write_nm_unmanaged; do
    awk -v f="$fn() {" '$0 == f { on = 1 } on { print } on && $0 == "}" { exit }' "$SETUP" >>"$TMP/hostnet_fn.sh"
done

# backend <networkd-enabled:yes|no> — runs select_hostnet_backend; sets RC, OUT.
backend() {
    printf '#!/bin/sh
[ "$1 $2" = "is-enabled --quiet" ] && [ "%s" = yes ]
' "$1" >"$TMP/bin/systemctl"
    chmod +x "$TMP/bin/systemctl"
    RC=0
    OUT=$(PATH="$TMP/bin:$PATH" HOSTNET_BACKEND_FILE="$TMP/hn/etc/hostnet-backend" NETWORKD_DIR="$TMP/hn/network" \
        bash -c '. "$1"; select_hostnet_backend' _ "$TMP/hostnet_fn.sh" 2>&1) || RC=$?
}

reset_hn() { rm -rf "$TMP/hn"; mkdir -p "$TMP/hn/network"; }

reset_hn
backend no
if [ "$RC" = 0 ] && [ "$OUT" = hostnet ] && [ "$(cat "$TMP/hn/etc/hostnet-backend")" = hostnet ]; then pass "backend: no networkd picks and records hostnet"; else fail "backend: no networkd gave rc=$RC out=$OUT"; fi

reset_hn
backend yes
if [ "$RC" = 0 ] && [ "$OUT" = networkd ] && [ "$(cat "$TMP/hn/etc/hostnet-backend")" = networkd ]; then pass "backend: networkd enabled picks networkd"; else fail "backend: networkd enabled gave rc=$RC out=$OUT"; fi

reset_hn
: >"$TMP/hn/network/10-spinifex-mgmt.network"
backend no
if [ "$RC" = 0 ] && [ "$OUT" = networkd ]; then pass "backend: legacy Spinifex networkd files keep networkd"; else fail "backend: legacy files gave rc=$RC out=$OUT"; fi

reset_hn
mkdir -p "$TMP/hn/etc"
echo hostnet >"$TMP/hn/etc/hostnet-backend"
backend yes
if [ "$RC" = 0 ] && [ "$OUT" = hostnet ]; then pass "backend: the record wins over networkd being enabled"; else fail "backend: recorded hostnet gave rc=$RC out=$OUT"; fi

echo ifupdown >"$TMP/hn/etc/hostnet-backend"
backend yes
case "$RC:$OUT" in
    0:*) fail "backend: an unknown record was accepted: $OUT" ;;
    *"expected networkd or hostnet"*) pass "backend: an unknown record is refused" ;;
    *) fail "backend: an unknown record gave rc=$RC out=$OUT" ;;
esac

# --------------------------------------------------------------------------
# NetworkManager unmanaged config.
# --------------------------------------------------------------------------
# NetworkManager globs have no character classes, so the management TAPs go to
# a udev rule; the keyfile lists the rest. The host uplink bridge stays managed.
printf '#!/bin/sh
exit 0
' >"$TMP/bin/udevadm"
cp "$TMP/bin/udevadm" "$TMP/bin/nmcli"
chmod +x "$TMP/bin/udevadm" "$TMP/bin/nmcli"
glob=$(sed -n "s/^MGMT_TAP_GLOB='\([^']*\)'$/\1/p" "$SETUP")
RC=0
PATH="$TMP/bin:$PATH" NM_UNMANAGED_CONF="$TMP/nm/conf.d/00-spinifex-unmanaged.conf" NM_UNMANAGED_RULE="$TMP/nm/rules.d/90.rules" \
    MGMT_TAP_GLOB="$glob" WAN_BRIDGE=br-ext WAN_BRIDGE_MODE=direct WAN_IFACE=eth3 LINUX_BRIDGE=br-wan \
    MGMT_BRIDGE_ENABLED=true MGMT_BRIDGE=br-mgmt MGMT_IFACE=eth2 \
    bash -c '. "$1"; write_nm_unmanaged' _ "$TMP/hostnet_fn.sh" >/dev/null 2>&1 || RC=$?
conf=$(cat "$TMP/nm/conf.d/00-spinifex-unmanaged.conf" 2>/dev/null || true)
line=$(printf '%s\n' "$conf" | grep '^unmanaged-devices+=' || true)
if [ "$RC" = 0 ] && printf '%s\n' "$conf" | grep -qx '\[keyfile\]' && [ -n "$line" ]; then pass "nm: keyfile section appends to unmanaged-devices"; else fail "nm: rc=$RC conf=$conf"; fi
for want in br-int br-ext 'veth-wan-*' 'spx-nat-*' 'tap*' br-mgmt eth2 eth3; do
    case ";${line#unmanaged-devices+=};" in
        *";interface-name:$want;"*) pass "nm: $want unmanaged" ;;
        *) fail "nm: $want missing from $line" ;;
    esac
done
case "$line" in
    *br-wan*) fail "nm: the host uplink bridge br-wan was made unmanaged" ;;
    *'['*) fail "nm: keyfile uses a character class NetworkManager cannot match: $line" ;;
    *) pass "nm: host uplink left managed and no character classes" ;;
esac
rule=$(cat "$TMP/nm/rules.d/90.rules" 2>/dev/null || true)
case "$rule" in
    *"KERNEL==\"$glob\", ENV{NM_UNMANAGED}=\"1\""*) pass "nm: udev rule unmanages management TAPs" ;;
    *) fail "nm: udev rule is $rule" ;;
esac

if [ "$fails" -ne 0 ]; then
    echo "  setup-ovn.sh: $fails failure(s)"
    exit 1
fi
echo "  setup-ovn.sh ok"
