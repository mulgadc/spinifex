#!/bin/sh
# node-reset_test.sh — tests the QEMU process match node-reset.sh uses to stop
# guests before it deletes Viperblock state, against real processes, and the
# instance and mounted-volume counts it and uninstall-spx.sh report before deleting,
# and the libreswan NSS entries it deletes.
#
# Each fake guest is a copy of sleep named like a QEMU binary, so the kernel
# gives it the same comm, including the 15-character truncation.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
TMP=$(mktemp -d)
pids=""
cleanup() {
    for pid in $pids; do kill "$pid" 2>/dev/null || true; done
    rm -rf "$TMP"
}
trap cleanup EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- node-reset.sh QEMU process match"

# pattern_in <script> — the pgrep pattern the script assigns to QEMU_PGREP.
pattern_in() {
    sed -n "s/^QEMU_PGREP=('\(.*\)')$/\1/p" "$1"
}

PATTERN=$(pattern_in "$SCRIPT_DIR/node-reset.sh")
if [ -z "$PATTERN" ]; then
    echo "  FAIL - no QEMU_PGREP=('...') line in node-reset.sh"
    exit 1
fi

# reset-dev-env.sh stops guests the same way and must not drift from it.
dev_pattern=$(pattern_in "$SCRIPT_DIR/reset-dev-env.sh")
if [ "$dev_pattern" = "$PATTERN" ]; then
    pass "reset-dev-env.sh uses node-reset.sh's pattern"
else
    fail "reset-dev-env.sh pattern '$dev_pattern' differs from node-reset.sh's '$PATTERN'"
fi

# start <name> — runs a process whose comm is <name>, truncated to 15
# characters. Its PID is left in $!.
start() {
    cp "$(command -v sleep)" "$TMP/$1"
    "$TMP/$1" 300 &
    pids="$pids $!"
}
start qemu-system-x86_64; pid_x86=$!
start qemu-system-aarch64; pid_arm=$!
start qemu-kvm; pid_kvm=$!
start qemu-img; pid_img=$!
start qemu-kvm-helper; pid_helper=$!

me=$(id -un)
found=" $(pgrep -u "$me" "$PATTERN" | tr '\n' ' ' || true) "

check_found() {
    case "$found" in
        *" $2 "*) pass "finds a $1 guest owned by the service user" ;;
        *) fail "misses a $1 guest (pid $2) owned by the service user" ;;
    esac
}
check_found qemu-system-x86_64 "$pid_x86"
check_found qemu-system-aarch64 "$pid_arm"
check_found qemu-kvm "$pid_kvm"

check_ignored() {
    case "$found" in
        *" $2 "*) fail "matches $1 (pid $2), which is not a guest" ;;
        *) pass "ignores $1" ;;
    esac
}
check_ignored qemu-img "$pid_img"
check_ignored qemu-kvm-helper "$pid_helper"

# The owner scope: the same guests, asked for as another user, are not found.
other=root
[ "$me" = root ] && other=nobody
other_found=" $(pgrep -u "$other" "$PATTERN" | tr '\n' ' ' || true) "
case "$other_found" in
    *" $pid_kvm "* | *" $pid_x86 "*) fail "a scope of -u $other still matches guests owned by $me" ;;
    *) pass "ignores guests owned by another user" ;;
esac

echo "-- node-reset.sh and uninstall-spx.sh at-stake counts"

# counts_in <script> — the shared count helpers, between their markers.
counts_in() {
    sed -n '/^# --- BEGIN at-stake counts ---$/,/^# --- END at-stake counts ---$/p' "$1"
}
counts_in "$SCRIPT_DIR/node-reset.sh" > "$TMP/counts.sh"
if [ ! -s "$TMP/counts.sh" ]; then
    fail "no at-stake counts block in node-reset.sh"
elif counts_in "$SCRIPT_DIR/uninstall-spx.sh" | cmp -s - "$TMP/counts.sh"; then
    pass "uninstall-spx.sh carries node-reset.sh's count helpers"
else
    fail "uninstall-spx.sh's count helpers differ from node-reset.sh's"
fi

# count <fn> <data dir> — runs a helper with sudo as a no-op, as root would.
count() {
    bash -c 'sudo() { "$@"; }; . "$1"; "$2" "$3"' _ "$TMP/counts.sh" "$1" "$2"
}
expect() {
    if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: got '$2', want '$3'"; fi
}

# The layout a node with three guests has: one volume with its EFI store, an
# AMI, and the system instances' records, which have no volume of their own.
data="$TMP/data"
mkdir -p "$data/state" "$data/viperblock/vol-a" "$data/viperblock/vol-a-efi" \
    "$data/viperblock/vol-b" "$data/viperblock/ami-c" "$data/volumes"
: > "$data/viperblock/viperblock.pid"
printf '{"schema_version":1,"vms":{"i-1":{},"i-2":{},"i-3":{}}}' > "$data/state/instance-state.json"
if command -v jq >/dev/null 2>&1; then
    expect "counts each instance in the local state file" "$(count count_instances "$data")" 3
    printf '{"schema_version":1,"vms":{' > "$data/state/instance-state.json"
    expect "reports an unreadable state file as unknown, not 0" "$(count count_instances "$data")" unknown
else
    echo "  skip - jq is not installed; the instance counts need it"
fi
expect "counts mounted volumes, not EFI stores or AMIs" "$(count count_mounted_volumes "$data")" 2

empty="$TMP/empty"
mkdir -p "$empty"
expect "counts no instances without a state file" "$(count count_instances "$empty")" 0
expect "counts no mounted volumes without a viperblock directory" "$(count count_mounted_volumes "$empty")" 0

echo "-- node-reset.sh libreswan NSS nicknames"

sed -n '/^# --- BEGIN ovs nss nicknames ---$/,/^# --- END ovs nss nicknames ---$/p' \
    "$SCRIPT_DIR/node-reset.sh" > "$TMP/nss.sh"
# `certutil -L` as it prints a database holding an operator's own identity and
# CA beside what ovs-monitor-ipsec imported, one peer listed twice.
cat > "$TMP/certutil-L" <<'EOF'

Certificate Nickname                                         Trust Attributes
                                                             SSL,S/MIME,JAR/XPI

ovs_certkey_node1                                            u,u,u
ovs_cert_cacert                                              CT,,
ovs_cert_node2                                               P,P,P
ovs_cert_node2                                               P,P,P
site-vpn                                                     u,u,u
corp-ca                                                      CT,C,C
ovs_certificate_store                                        ,,
EOF
if [ ! -s "$TMP/nss.sh" ]; then
    fail "no ovs nss nicknames block in node-reset.sh"
else
    got=$(bash -c '. "$1"; ovs_nss_nicknames < "$2"' _ "$TMP/nss.sh" "$TMP/certutil-L" | tr '\n' ' ')
    expect "selects only ovs-monitor-ipsec's entries, once each" "$got" "ovs_cert_cacert ovs_cert_node2 ovs_certkey_node1 "
fi

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
