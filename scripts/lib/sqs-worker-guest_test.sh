#!/bin/sh
# sqs-worker-guest_test.sh — the SQS worker guest's readiness watcher is
# supervised by init, never leaves a store-ready marker behind a dead watcher,
# and shutdown unmounts the worker disk only once the watcher is gone.
#
# Drives build/microvm/sqs-worker/sqs-worker-lib.sh with test-owned processes
# and a stub umount. Every signal goes only to this test's own children.
# The case settings below are read by the sourced library, not by this file.
# shellcheck disable=SC2034
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
LIB=${SQSW_GUEST_LIB:-"$ROOT/build/microvm/sqs-worker/sqs-worker-lib.sh"}
INITTAB="$ROOT/build/microvm/sqs-worker/inittab"
TMP=$(mktemp -d)
CHILDREN=""
cleanup() {
    for p in $CHILDREN; do kill -KILL "$p" 2>/dev/null || true; done
    rm -rf "$TMP"
}
trap cleanup EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }

echo "-- build/microvm/sqs-worker/sqs-worker-lib.sh"

mkdir -p "$TMP/bin"
cat > "$TMP/bin/umount" <<'EOF'
#!/bin/sh
echo "umount $*" >> "$T/calls.log"
EOF
# The fake watcher notes whether a marker was already present when it
# started, publishes readiness as the real one does, then stays running.
cat > "$TMP/bin/watcher" <<'EOF'
#!/bin/sh
[ ! -e "$T/run/store-ready" ] || echo stale >> "$T/saw-stale"
echo "$$" >> "$T/watchers"
echo "10.0.0.5 UPUB" > "$T/run/store-ready"
exec sleep 60
EOF
chmod +x "$TMP/bin"/*

fresh() {
    T="$TMP/$1"
    export T
    mkdir -p "$T/run" "$T/data"
    echo UPUBLICKEY > "$T/run/public-key"
    : > "$T/calls.log" "$T/console" "$T/mounts"
}
gone() { [ ! -r "/proc/$1/status" ] || grep -q '^State:[[:space:]]*Z' "/proc/$1/status" 2>/dev/null; }
waitfor() { i=0; while [ "$i" -lt 100 ] && ! eval "$1"; do i=$((i + 1)); sleep 0.05; done; eval "$1"; }
# start_run: one init respawn of the readiness step, in the background.
start_run() {
    SQSW_RUN="$T/run" SQSW_CONSOLE="$T/console" SQSW_BIN="$TMP/bin/watcher" SQSW_RESPAWN_GAP="${GAP:-0}" \
        sh -c '. "$1"; sqsw_run' sh "$LIB" & pid=$!
    CHILDREN="$CHILDREN $pid"
}
# record <pid>: write the identity sqs_run would record for that process.
# shellcheck source=../../build/microvm/sqs-worker/sqs-worker-lib.sh
record() { echo "$1 $( . "$LIB"; sqsw_starttime "$1" )" > "$T/run/ready.pid"; }
# shutdown_step [survivor-pid]: with a pid, that process is reported alive
# whatever happens to it, as a watcher stuck past SIGKILL would be.
shutdown_step() {
    (
        SQSW_RUN="$T/run" SQSW_DATA="$T/data" SQSW_CONSOLE="$T/console" SQSW_MOUNTS="$T/mounts"
        UMOUNT="$TMP/bin/umount" SQSW_STOP_BUDGET_TENTHS=5
        # shellcheck source=../../build/microvm/sqs-worker/sqs-worker-lib.sh
        . "$LIB"
        if [ -n "${1:-}" ]; then sqsw_same() { [ "$1" = "$survivor" ]; }; survivor=$1; fi
        sqsw_shutdown
    ) > /dev/null 2>&1
}

check "init respawns the readiness step rather than running it once" \
    'grep -qx "::respawn:/usr/local/lib/sqs-worker/sqs-worker-ready" "$INITTAB" && ! grep -q "::once:" "$INITTAB"'

# --- A watcher killed after readiness: its marker must not outlive it ---
fresh killed-after-ready
start_run; first=$pid
check "the watcher publishes store-ready" 'waitfor "[ -s \"\$T/run/store-ready\" ]"'
kill -KILL "$first"
check "SIGKILL leaves the marker behind, as a crash would" 'waitfor "gone $first" && [ -e "$T/run/store-ready" ]'
start_run; second=$pid
check "the respawned step starts a new watcher" 'waitfor "[ \"\$(wc -l < \"\$T/watchers\")\" -eq 2 ]"'
check "the respawn removed the dead watcher's marker before the new watcher ran" \
    '[ ! -e "$T/saw-stale" ] && grep -q "removed a store-ready marker left by an earlier watcher" "$T/console"'
check "the respawn records the new watcher's identity" 'grep -q "^$second " "$T/run/ready.pid"'
kill -KILL "$second" 2>/dev/null || true

# --- A watcher that keeps exiting is respawned at most once per gap ---
fresh respawn-gap
date +%s > "$T/run/ready.last"
GAP=2 start_run; t0=$(date +%s)
waitfor "[ -s \"\$T/watchers\" ]" || true
t1=$(date +%s)
check "a start right after the previous one waits for the gap" '[ $((t1 - t0)) -ge 1 ]'
kill -KILL "$pid" 2>/dev/null || true

# --- Runs that must not start ---
fresh run-stopping; : > "$T/run/stopping"; echo stale > "$T/run/store-ready"
start_run; waitfor "gone $pid" || true
check "a start during shutdown starts no watcher and removes the marker" \
    'gone "$pid" && [ ! -e "$T/watchers" ] && [ ! -e "$T/run/store-ready" ] && grep -q "shutdown in progress" "$T/console"'

fresh run-no-key; rm -f "$T/run/public-key"
start_run; waitfor "gone $pid" || true
check "a guest without a worker key starts no watcher" 'gone "$pid" && [ ! -e "$T/watchers" ] && grep -q "no worker key" "$T/console"'

# --- Shutdown: unmount only once the watcher is confirmed gone ---
fresh shutdown-orderly; echo "dev $T/data ext4 rw 0 0" > "$T/mounts"
start_run; waitfor "[ -s \"\$T/run/store-ready\" ]"
check "orderly shutdown succeeds" 'shutdown_step'
check "orderly shutdown confirms the watcher exited, removes the marker, then unmounts" \
    'gone "$pid" && grep -q "readiness watcher pid=$pid exited" "$T/console" && [ ! -e "$T/run/store-ready" ] &&
     [ "$(grep "^umount" "$T/calls.log" | tr "\n" "|")" = "umount $T/data|umount -a -r|" ]'
check "shutdown blocks any later respawn" '[ -e "$T/run/stopping" ]'

fresh shutdown-hung; echo "dev $T/data ext4 rw 0 0" > "$T/mounts"
(trap '' TERM; exec sleep 60) & pid=$!; CHILDREN="$CHILDREN $pid"; record "$pid"
echo "10.0.0.5 UPUB" > "$T/run/store-ready"
shutdown_step
check "a watcher ignoring SIGTERM is killed after the budget, then the disk is unmounted" \
    'gone "$pid" && grep -q SIGKILL "$T/console" && grep -qx "umount $T/data" "$T/calls.log"'

fresh shutdown-survivor; echo "dev $T/data ext4 rw 0 0" > "$T/mounts"
(trap '' TERM; exec sleep 60) & pid=$!; CHILDREN="$CHILDREN $pid"; record "$pid"
echo "10.0.0.5 UPUB" > "$T/run/store-ready"
check "shutdown with a surviving watcher reports failure" '! shutdown_step "$pid"'
check "a surviving watcher leaves every mount alone and is never reported stopped" \
    '! grep -q "^umount" "$T/calls.log" && grep -q "STILL ALIVE" "$T/console" && grep -q "skipping umount -a -r" "$T/console" && ! grep -q "exited" "$T/console"'
check "a surviving watcher still leaves no readiness marker" '[ ! -e "$T/run/store-ready" ]'
kill -KILL "$pid" 2>/dev/null || true

fresh shutdown-reused; echo "dev $T/data ext4 rw 0 0" > "$T/mounts"
(exec sleep 60) & pid=$!; CHILDREN="$CHILDREN $pid"; echo "$pid 1" > "$T/run/ready.pid"
shutdown_step
check "a recorded PID with another start time is never signalled" '! gone "$pid" && grep -q "no running readiness watcher" "$T/console"'
kill -KILL "$pid" 2>/dev/null || true

if [ "$fails" -ne 0 ]; then
    echo "  $fails failure(s)"
    exit 1
fi
