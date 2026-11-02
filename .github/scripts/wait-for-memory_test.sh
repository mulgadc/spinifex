#!/bin/sh
# wait-for-memory_test.sh — drives wait-for-memory.sh against a fake meminfo
# that a background writer changes mid-wait, so each verdict is checked without
# a loaded host.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
WAIT="$SCRIPT_DIR/wait-for-memory.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

meminfo() { printf 'MemTotal:       65536000 kB\nMemAvailable:   %s kB\n' "$(( $1 * 1024 ))" > "$TMP/meminfo"; }
MEMINFO="$TMP/meminfo"
MEM_WAIT_POLL_S=1
export MEMINFO MEM_WAIT_POLL_S

meminfo 30000
if MEM_WAIT_TIMEOUT_S=5 bash "$WAIT" 26624 > "$TMP/out" 2>&1; then
  pass "enough memory returns at once"
else
  fail "enough memory returns at once: $(cat "$TMP/out")"
fi

meminfo 21577
( sleep 2; meminfo 30000 ) &
if MEM_WAIT_TIMEOUT_S=20 bash "$WAIT" 26624 > "$TMP/out" 2>&1 && grep -q waiting "$TMP/out"; then
  pass "memory freed mid-wait is admitted"
else
  fail "memory freed mid-wait is admitted: $(cat "$TMP/out")"
fi
wait

meminfo 21577
if MEM_WAIT_TIMEOUT_S=2 bash "$WAIT" 26624 > "$TMP/out" 2>&1; then
  fail "memory that never frees fails: exited 0"
elif grep -q '::error::need 26624 MiB available, have 21577 MiB' "$TMP/out" && grep -q 'RSS' "$TMP/out"; then
  pass "memory that never frees fails and names the biggest processes"
else
  fail "memory that never frees fails and names the biggest processes: $(cat "$TMP/out")"
fi

[ "$fails" -eq 0 ] || { echo "wait-for-memory: $fails failed"; exit 1; }
echo "wait-for-memory: all passed"
