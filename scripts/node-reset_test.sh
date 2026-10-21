#!/bin/sh
# node-reset_test.sh — tests the QEMU process match node-reset.sh uses to stop
# guests before it deletes Viperblock state, against real processes.
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

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
