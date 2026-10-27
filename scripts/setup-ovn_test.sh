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

# --------------------------------------------------------------------------
# OVN central options file and management role.
# --------------------------------------------------------------------------
# The functions run on their own against scratch paths, through the stub sudo.
for fn in write_ovn_central_opts record_management_role management_role_markers cap_logrotate; do
    awk -v f="$fn() {" '$0 == f { on = 1 } on { print } on && $0 == "}" { exit }' "$SETUP" >>"$TMP/ovn_fn.sh"
done

# ovn_fn <cmd>... — runs the functions against scratch role files; sets RC, OUT.
ovn_fn() {
    RC=0
    OUT=$(PATH="$TMP/bin:$PATH" OVN_ROLE_FILE="$TMP/role/etc/spinifex/ovn-role" \
        LEGACY_OVN_ROLE_FILE="$TMP/role/etc/default/ovn-central" \
        bash -c '. "$1"; shift; "$@"' _ "$TMP/ovn_fn.sh" "$@" 2>&1) || RC=$?
}

ovn_fn env OVN_CENTRAL_OPTS_FILE="$TMP/sysconfig-ovn-northd" OVN_CENTRAL_OPTS_VAR=OVN_NORTHD_OPTS \
    bash -c '. "$1"; write_ovn_central_opts "--db-nb-addr=10.0.0.1 --db-nb-create-insecure-remote=yes"' _ "$TMP/ovn_fn.sh"
if [ "$RC" = 0 ] && [ "$(cat "$TMP/sysconfig-ovn-northd")" = 'OVN_NORTHD_OPTS="--db-nb-addr=10.0.0.1 --db-nb-create-insecure-remote=yes"' ]; then
    pass "opts: EL file holds OVN_NORTHD_OPTS"
else
    fail "opts: rc=$RC $(cat "$TMP/sysconfig-ovn-northd" 2>/dev/null)"
fi

rm -rf "$TMP/role"
ovn_fn management_role_markers
if [ "$RC" != 0 ] && [ -z "$OUT" ]; then pass "role: a fresh node has no marker"; else fail "role: fresh node rc=$RC out=$OUT"; fi

ovn_fn record_management_role
ovn_fn management_role_markers
if [ "$RC" = 0 ] && [ "$OUT" = "$TMP/role/etc/spinifex/ovn-role" ]; then pass "role: recorded marker is found"; else fail "role: recorded marker rc=$RC out=$OUT"; fi

mkdir -p "$TMP/role/etc/default"
echo 'OVN_CTL_OPTS=""' >"$TMP/role/etc/default/ovn-central"
ovn_fn management_role_markers
if [ "$RC" = 0 ] && [ "$OUT" = "$TMP/role/etc/spinifex/ovn-role
$TMP/role/etc/default/ovn-central" ]; then pass "role: both markers are named, so the demotion hint removes both"; else fail "role: both markers rc=$RC out=$OUT"; fi

rm -f "$TMP/role/etc/spinifex/ovn-role"
ovn_fn management_role_markers
if [ "$RC" = 0 ] && [ "$OUT" = "$TMP/role/etc/default/ovn-central" ]; then pass "role: the legacy Debian marker alone still counts"; else fail "role: legacy marker rc=$RC out=$OUT"; fi

# The package's own file sets only commented defaults; it is not a marker.
printf '# OVN_CTL_OPTS="--db-nb-addr=0.0.0.0"\n' >"$TMP/role/etc/default/ovn-central"
echo compute >"$TMP/role/etc/spinifex/ovn-role"
ovn_fn management_role_markers
if [ "$RC" != 0 ]; then pass "role: a package-default options file and another role are not markers"; else fail "role: non-markers gave rc=$RC out=$OUT"; fi

# --------------------------------------------------------------------------
# OVS and OVN log rotation.
# --------------------------------------------------------------------------
# Fixtures are the stanzas the Debian 13 and NFV SIG el10s packages ship.
mkdir -p "$TMP/lr"
cat >"$TMP/lr/ovn-common" <<'EOF'
/var/log/ovn/*.log {
    su root root
    daily
    compress
    sharedscripts
    missingok
}
EOF
cat >"$TMP/lr/openvswitch-switch" <<'EOF'
/var/log/openvswitch/*.log {
    daily
    compress
    sharedscripts
    create 640 root adm
    missingok
    rotate 30
}
EOF
cat >"$TMP/lr/ovn" <<'EOF'
/var/log/ovn/*.log {
    su openvswitch openvswitch
    daily
    compress
    sharedscripts
    missingok
}
EOF
printf '/var/log/other.log {\n    daily\n}\n' >"$TMP/lr/odd"

# directives <file> — the stanza's directives, sorted, one per line.
directives() { sed -n 's/^    //p' "$1" | sort; }

ovn_fn cap_logrotate "$TMP/lr/ovn-common"
if [ "$RC" = 0 ] && [ "$(directives "$TMP/lr/ovn-common")" = "$(printf 'compress\ndaily\nmaxsize 100M\nmissingok\nrotate 5\nsharedscripts\nsu root root\n')" ]; then
    pass "logrotate: Debian OVN gets rotate 5 and maxsize, as before"
else
    fail "logrotate: Debian OVN rc=$RC: $(cat "$TMP/lr/ovn-common")"
fi

ovn_fn cap_logrotate "$TMP/lr/openvswitch-switch"
if [ "$RC" = 0 ] && grep -qx '    maxsize 100M' "$TMP/lr/openvswitch-switch" &&
    [ "$(grep -c '^ *rotate ' "$TMP/lr/openvswitch-switch")" = 1 ] && grep -qx '    rotate 30' "$TMP/lr/openvswitch-switch"; then
    pass "logrotate: Debian OVS keeps its rotate 30 and gains maxsize"
else
    fail "logrotate: Debian OVS rc=$RC: $(cat "$TMP/lr/openvswitch-switch")"
fi

ovn_fn cap_logrotate "$TMP/lr/ovn"
if [ "$RC" = 0 ] && [ "$(directives "$TMP/lr/ovn")" = "$(printf 'compress\ndaily\nmaxsize 100M\nmissingok\nrotate 5\nsharedscripts\nsu root root\n')" ]; then
    pass "logrotate: EL OVN rotates as root, with rotate 5 and maxsize"
else
    fail "logrotate: EL OVN rc=$RC: $(cat "$TMP/lr/ovn")"
fi

before=$(cat "$TMP/lr/ovn")
ovn_fn cap_logrotate "$TMP/lr/ovn"
if [ "$RC" = 0 ] && [ "$(cat "$TMP/lr/ovn")" = "$before" ]; then pass "logrotate: a rerun changes nothing"; else fail "logrotate: rerun changed $TMP/lr/ovn: $(cat "$TMP/lr/ovn")"; fi

ovn_fn cap_logrotate "$TMP/lr/odd"
case "$RC:$OUT" in
    0:*"left uncapped"*) pass "logrotate: a file without the expected stanza warns" ;;
    *) fail "logrotate: unexpected stanza gave rc=$RC out=$OUT" ;;
esac

# --------------------------------------------------------------------------
# Clustered DB storage, and what --recreate-db purges.
# --------------------------------------------------------------------------
# install-node.sh passes --recreate-db on every database node on every run, so
# re-forming a cluster has to replace a clustered DB as readily as a standalone
# one. Left in place, ovn-ctl serves it, ignores the RAFT flags and rejoins the
# previous cluster under the member list the old DB carries.
for fn in stop_ovn_databases ensure_clustered_db_storage; do
    awk -v f="$fn() {" '$0 == f { on = 1 } on { print } on && $0 == "}" { exit }' "$SETUP" >>"$TMP/db_fn.sh"
done

# db-is-clustered reports on the fixture's own contents, so a test writes
# "clustered" or "standalone" into the file to choose the format.
printf '#!/bin/sh\n[ "$1" = db-is-clustered ] || exit 2\ngrep -q clustered "$2"\n' >"$TMP/bin/ovsdb-tool"
printf '#!/bin/sh\nexit 0\n' >"$TMP/bin/systemctl"
printf '#!/bin/sh\nexit 1\n' >"$TMP/bin/pgrep"
printf '#!/bin/sh\necho "pkill $*" >>"%s"\nexit 0\n' "$TMP/pkill.log" >"$TMP/bin/pkill"
chmod +x "$TMP/bin/ovsdb-tool" "$TMP/bin/systemctl" "$TMP/bin/pgrep" "$TMP/bin/pkill"

# db_fixture <format> — a DB directory in <format> with every leftover present.
db_fixture() {
    rm -rf "$TMP/dbdir"
    mkdir -p "$TMP/dbdir/standalone-backup"
    for f in ovnnb_db.db ovnsb_db.db; do
        echo "$1" >"$TMP/dbdir/$f"
        echo "$1" >"$TMP/dbdir/$f.standalone-disabled"
        echo "$1" >"$TMP/dbdir/standalone-backup/$f"
        : >"$TMP/dbdir/.$f.~lock~"
    done
}

# ensure_db <recreate:true|false> — runs ensure_clustered_db_storage; sets RC, OUT.
ensure_db() {
    RC=0
    OUT=$(PATH="$TMP/bin:$PATH" bash -c '
        OVN_DBDIR="$1"; RECREATE_DB="$2"; OVN_CENTRAL_UNITS=(ovn-central)
        OVN_DB_STOP_WAIT=2; OVN_DB_KILL_WAIT=1
        . "$3"; ensure_clustered_db_storage' _ "$TMP/dbdir" "$1" "$TMP/db_fn.sh" 2>&1) || RC=$?
}

db_fixture clustered
ensure_db true
if [ "$RC" = 0 ] && [ ! -e "$TMP/dbdir/ovnnb_db.db" ] && [ ! -e "$TMP/dbdir/ovnsb_db.db" ]; then
    pass "recreate-db: a clustered DB is discarded, so re-forming does not rejoin the old cluster"
else
    fail "recreate-db: clustered DB rc=$RC left [$(ls -A "$TMP/dbdir")] out=$OUT"
fi
if [ -z "$(ls -A "$TMP/dbdir")" ]; then
    pass "recreate-db: the lock files, move-asides and standalone backup go with it"
else
    fail "recreate-db: leftovers survived: $(ls -A "$TMP/dbdir" | tr '\n' ' ')"
fi

db_fixture standalone
ensure_db true
if [ "$RC" = 0 ] && [ ! -e "$TMP/dbdir/ovnnb_db.db" ]; then
    pass "recreate-db: a standalone DB is discarded as before"
else
    fail "recreate-db: standalone DB rc=$RC left [$(ls -A "$TMP/dbdir")] out=$OUT"
fi

db_fixture clustered
ensure_db false
if [ "$RC" = 0 ] && [ -e "$TMP/dbdir/ovnnb_db.db" ]; then
    pass "no recreate-db: a clustered DB is the steady state and is kept"
else
    fail "no recreate-db: clustered DB rc=$RC left [$(ls -A "$TMP/dbdir")] out=$OUT"
fi

db_fixture standalone
ensure_db false
case "$RC:$OUT" in
    0:*) fail "no recreate-db: a standalone DB was accepted, so the RAFT flags would be ignored" ;;
    *:*"standalone format"*) pass "no recreate-db: a standalone DB is refused by name" ;;
    *) fail "no recreate-db: standalone DB rc=$RC out=$OUT" ;;
esac

# --------------------------------------------------------------------------
# A rerun returns only once both of this node's RAFT members have rejoined.
# --------------------------------------------------------------------------
# A rerun restarts the node's NB and SB members together, so returning early
# lets the next database node's rerun take a second member of each down.
awk '$0 == "wait_for_raft_member() {" { on = 1 } on { print } on && $0 == "}" { exit }' "$SETUP" >"$TMP/raft_fn.sh"
mkdir -p "$TMP/raftbin"
printf '#!/bin/sh\nexit 0\n' >"$TMP/raftbin/sleep"
printf '#!/bin/sh\n[ -f "%s" ] || exit 1\ncat "%s"\n' "$TMP/cluster-status" "$TMP/cluster-status" >"$TMP/raftbin/ovs-appctl"
chmod +x "$TMP/raftbin/sleep" "$TMP/raftbin/ovs-appctl"

# raft_member <Status:> <Leader:> — runs wait_for_raft_member against a
# cluster/status in that state, or none when Status is "absent"; sets RC.
raft_member() {
    rm -f "$TMP/cluster-status"
    if [ "$1" != absent ]; then
        printf 'a1b2\nName: OVN_Northbound\nStatus: %s\nRole: follower\nTerm: 4\nLeader: %s\nVote: c3d4\n\nServers:\n    a1b2 (a1b2 at tcp:10.0.0.1:6643) (self)\n    c3d4 (c3d4 at tcp:10.0.0.2:6643)\n' \
            "$1" "$2" >"$TMP/cluster-status"
    fi
    RC=0
    PATH="$TMP/raftbin:$PATH" bash -c 'sudo() { "$@"; }; OVN_RAFT_REJOIN_WAIT=2; . "$1"
        wait_for_raft_member /var/run/ovn/ovnnb_db.ctl OVN_Northbound' _ "$TMP/raft_fn.sh" >/dev/null 2>&1 || RC=$?
}

if [ ! -s "$TMP/raft_fn.sh" ]; then
    fail "raft: no wait_for_raft_member in setup-ovn.sh"
else
    raft_member "cluster member" c3d4
    if [ "$RC" = 0 ]; then pass "raft: a member following a known leader has rejoined"; else fail "raft: follower with leader rc=$RC"; fi
    raft_member "cluster member" self
    if [ "$RC" = 0 ]; then pass "raft: a member that is the leader has rejoined"; else fail "raft: leader rc=$RC"; fi
    raft_member "cluster member" unknown
    if [ "$RC" != 0 ]; then pass "raft: a member that has not found its leader has not rejoined"; else fail "raft: unknown leader accepted"; fi
    raft_member "disconnected from the cluster (election timeout)" c3d4
    if [ "$RC" != 0 ]; then pass "raft: a disconnected member that remembers a leader has not rejoined"; else fail "raft: disconnected member accepted"; fi
    raft_member "joining cluster" c3d4
    if [ "$RC" != 0 ]; then pass "raft: a member still joining has not rejoined"; else fail "raft: joining member accepted"; fi
    raft_member absent ""
    if [ "$RC" != 0 ]; then pass "raft: a server that does not answer has not rejoined"; else fail "raft: unanswered cluster/status accepted"; fi
fi

rm -rf "$TMP/dbdir"
mkdir -p "$TMP/dbdir"
ensure_db true
if [ "$RC" = 0 ]; then pass "recreate-db: an empty DB directory is already what it wants"; else fail "recreate-db: empty directory rc=$RC out=$OUT"; fi

# A server that outlives its unit must be killed by a pattern anchored on the
# OVN database directory. Matching ovsdb-server alone would reap the OVS
# conf.db server, which runs the switch.
db_fixture clustered
printf '#!/bin/sh\nexit 0\n' >"$TMP/bin/pgrep"
: >"$TMP/pkill.log"
ensure_db true
if grep -q -- "$TMP/dbdir/ovn" "$TMP/pkill.log" 2>/dev/null; then
    pass "recreate-db: a surviving server is killed by a pattern scoped to the OVN database directory"
else
    fail "recreate-db: pkill was called as [$(cat "$TMP/pkill.log" 2>/dev/null)]"
fi
if [ "$RC" != 0 ] && [ -e "$TMP/dbdir/ovnnb_db.db" ]; then
    pass "recreate-db: a server that survives SIGKILL fails the run rather than unlinking under it"
else
    fail "recreate-db: unkillable server gave rc=$RC and left [$(ls -A "$TMP/dbdir")]"
fi
printf '#!/bin/sh\nexit 1\n' >"$TMP/bin/pgrep"

if [ "$fails" -ne 0 ]; then
    echo "  setup-ovn.sh: $fails failure(s)"
    exit 1
fi
echo "  setup-ovn.sh ok"
