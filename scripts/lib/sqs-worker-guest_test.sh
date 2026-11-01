#!/bin/sh
# sqs-worker-guest_test.sh — the SQS guest's store starts only on a config
# prepared after the receipt key settled, its readiness watcher never leaves a
# store-ready marker behind a dead watcher, and shutdown unmounts the data disk
# only once both the watcher and the store are gone.
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
# The fake server checks its config, or notes how it was started and stays.
cat > "$TMP/bin/nats" <<'EOF'
#!/bin/sh
if [ "$1" = -t ]; then [ -z "${NATS_BAD:-}" ]; exit; fi
echo "$GOMEMLIMIT $*" >> "$T/servers"
exec sleep 60
EOF
# The fake boot takes its exit codes in order from $T/boot-rcs, and like the
# real one writes the receipt marker only on success; "hold" blocks like a slow call.
cat > "$TMP/bin/boot" <<'EOF'
#!/bin/sh
echo "$*" >> "$T/boots"
rc=$(head -n1 "$T/boot-rcs"); sed -i 1d "$T/boot-rcs"
if [ "$rc" = hold ]; then echo "$$" > "$T/boot-child"; exec sleep 60; fi
[ "$rc" != 0 ] || echo FP > "$T/run/receipt-ready"
exit "$rc"
EOF
chmod +x "$TMP/bin"/*

fresh() {
    T="$TMP/$1"
    export T
    mkdir -p "$T/run" "$T/data"
    echo FP > "$T/run/receipt-ready"
    echo "listen: 127.0.0.1:4222" > "$T/run/store.conf"
    for f in calls.log console mounts; do : > "$T/$f"; done
}
gone() { [ ! -r "/proc/$1/status" ] || grep -q '^State:[[:space:]]*Z' "/proc/$1/status" 2>/dev/null; }
waitfor() { i=0; while [ "$i" -lt 100 ] && ! eval "$1"; do i=$((i + 1)); sleep 0.05; done; eval "$1"; }
# start_run: one init respawn of the readiness step, in the background.
start_run() {
    SQSW_RUN="$T/run" SQSW_CONSOLE="$T/console" SQSW_BIN="$TMP/bin/watcher" SQSW_RESPAWN_GAP="${GAP:-0}" SQSW_PARK="sleep 60" \
        sh -c '. "$1"; sqsw_run' sh "$LIB" & pid=$!
    CHILDREN="$CHILDREN $pid"
}
# start_store: one init respawn of the store step, in the background.
start_store() {
    SQSW_RUN="$T/run" SQSW_DATA="$T/data" SQSW_CONSOLE="$T/console" SQSW_NATS_BIN="$TMP/bin/nats" \
        SQSW_BIN="$TMP/bin/boot" SQSW_BOOT_RETRY=0 SQSW_PARK="sleep 60" SQSW_LIB="$LIB" \
        sh -c '. "$1"; sqsw_store' sh "$LIB" & spid=$!
    CHILDREN="$CHILDREN $spid"
}
# prepare_store: the sysinit step's store preparation, after boot.
prepare_store() {
    SQSW_RUN="$T/run" SQSW_DATA="$T/data" SQSW_CONSOLE="$T/console" SQSW_NATS_BIN="$TMP/bin/nats" \
        sh -c '. "$1"; sqsw_prepare_store' sh "$LIB"
}
# record <pid>: write the identity sqs_run would record for that process.
# shellcheck source=../../build/microvm/sqs-worker/sqs-worker-lib.sh
record() { echo "$1 $( . "$LIB"; sqsw_starttime "$1" )" > "$T/run/${2:-ready}.pid"; }
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

check "init respawns the store and readiness steps rather than running them once" \
    'grep -qx "::respawn:/usr/local/lib/sqs-worker/sqs-worker-store" "$INITTAB" &&
     grep -qx "::respawn:/usr/local/lib/sqs-worker/sqs-worker-ready" "$INITTAB" && ! grep -q "::once:" "$INITTAB"'

# --- The store is prepared only after boot settled the receipt key ---
fresh prepare-ok; rm -f "$T/run/store.conf"
check "a settled receipt key prepares the store" 'prepare_store && [ -s "$T/run/store.conf" ]'
check "the store listens only on loopback, with no auth, monitor, route or cluster listener" \
    'grep -qx "listen: \"127.0.0.1:4222\"" "$T/run/store.conf" && grep -qx "  store_dir: \"$T/data/jetstream\"" "$T/run/store.conf" &&
     ! grep -Eq "authorization|http|monitor|cluster|routes|leafnodes|websocket|mqtt" "$T/run/store.conf"'
fresh prepare-no-key; rm -f "$T/run/store.conf" "$T/run/receipt-ready"
check "a receipt key boot did not settle leaves the store unprepared" '! prepare_store && [ ! -e "$T/run/store.conf" ]'
fresh prepare-bad; rm -f "$T/run/store.conf"
check "a config the server rejects leaves the store unprepared" '! NATS_BAD=1 prepare_store && [ ! -e "$T/run/store.conf" ]'

# --- The store step starts the server only on a prepared config ---
fresh store-run
start_store
check "a prepared store starts the dedicated server with its heap bound" \
    'waitfor "[ -s \"\$T/servers\" ]" && grep -qx "512MiB -c $T/run/store.conf" "$T/servers"'
kill -KILL "$spid" 2>/dev/null || true
fresh store-refused; : > "$T/run/boot-refused"
start_store; waitfor "grep -q 'store: boot refused' \"\$T/console\"" || true
check "a store whose boot was refused parks instead of starting or respawning" \
    '! gone "$spid" && [ ! -e "$T/servers" ] && [ ! -e "$T/boots" ]'
kill -KILL "$spid" 2>/dev/null || true

# --- Boot is supervised: a transient failure is retried, a refusal is final ---
fresh boot-transient; rm -f "$T/run/store.conf" "$T/run/receipt-ready"; printf '75\n0\n' > "$T/boot-rcs"
start_store
check "a transient boot failure exits so init runs the step again, starting nothing" \
    'waitfor "gone $spid" && [ ! -e "$T/servers" ] && [ ! -e "$T/run/boot-refused" ]'
start_store
check "the respawn after recovery boots again and starts the store" \
    'waitfor "[ -s \"\$T/servers\" ]" && [ "$(wc -l < "$T/boots")" -eq 2 ] && [ -s "$T/run/store.conf" ]'
kill -KILL "$spid" 2>/dev/null || true
fresh boot-final; rm -f "$T/run/store.conf" "$T/run/receipt-ready"; printf '1\n0\n' > "$T/boot-rcs"
start_store; waitfor "grep -q 'boot refused (rc=1)' \"\$T/console\"" || true
check "a final boot refusal parks the store and records the refusal" \
    '! gone "$spid" && [ -e "$T/run/boot-refused" ] && [ ! -e "$T/servers" ]'
kill -KILL "$spid" 2>/dev/null || true
start_store; waitfor "grep -q 'store: boot refused, not starting' \"\$T/console\"" || true
check "a later respawn never boots again after a final refusal" '[ "$(wc -l < "$T/boots")" -eq 1 ]'
kill -KILL "$spid" 2>/dev/null || true
fresh boot-settled; rm -f "$T/run/store.conf"; : > "$T/boot-rcs"
start_store
check "a respawn after the key settled starts the store without booting again" \
    'waitfor "[ -s \"\$T/servers\" ]" && [ ! -e "$T/boots" ]'
kill -KILL "$spid" 2>/dev/null || true
fresh boot-held; rm -f "$T/run/store.conf" "$T/run/receipt-ready"; echo hold > "$T/boot-rcs"
echo "/dev/vdb $T/data ext4 rw 0 0" > "$T/mounts"
start_store; waitfor '[ -s "$T/boot-child" ]' || true
bchild=$(cat "$T/boot-child" 2>/dev/null); CHILDREN="$CHILDREN $bchild"
shutdown_step
check "shutdown stops a boot still in progress before it unmounts the disk" \
    'waitfor "gone $bchild" && grep -q "umount $T/data" "$T/calls.log"'
kill -KILL "$spid" "$bchild" 2>/dev/null || true
fresh boot-held-survivor; rm -f "$T/run/store.conf" "$T/run/receipt-ready"; echo hold > "$T/boot-rcs"
echo "/dev/vdb $T/data ext4 rw 0 0" > "$T/mounts"
start_store; waitfor '[ -s "$T/boot-child" ]' || true
bchild=$(cat "$T/boot-child" 2>/dev/null); CHILDREN="$CHILDREN $bchild"
check "a boot that survives SIGKILL fails shutdown and keeps the disk mounted" \
    '! shutdown_step "$bchild" && ! grep -q umount "$T/calls.log" && grep -q "boot pid=$bchild STILL ALIVE" "$T/console"'
kill -KILL "$spid" "$bchild" 2>/dev/null || true
# Shutdown sets stopping after the store's own check but before the boot child
# runs: the child must still record itself and never start sqs-worker boot.
fresh boot-window; rm -f "$T/run/store.conf" "$T/run/receipt-ready"; echo 0 > "$T/boot-rcs"
: > "$T/run/stopping"
SQSW_RUN="$T/run" SQSW_CONSOLE="$T/console" SQSW_BIN="$TMP/bin/boot" SQSW_BOOT_RETRY=0 SQSW_PARK="sleep 60" SQSW_LIB="$LIB" \
    sh -c '. "$1"; sqsw_boot' sh "$LIB" & wpid=$!
CHILDREN="$CHILDREN $wpid"
waitfor "grep -q 'shutdown during boot' \"\$T/console\"" || true
check "a boot child started after shutdown began never runs boot" \
    'grep -q "shutdown during boot" "$T/console" && [ ! -e "$T/boots" ] && [ ! -e "$T/run/boot-refused" ]'
kill -KILL "$wpid" 2>/dev/null || true

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
# A run that cannot start parks rather than exiting, so init does not respawn
# it in a loop; a second is many park intervals, and it must log only once.
fresh run-stopping; : > "$T/run/stopping"; echo stale > "$T/run/store-ready"
start_run; waitfor "grep -q 'shutdown in progress' \"\$T/console\"" || true
check "a start during shutdown parks instead of exiting and respawning" '! gone "$pid"'
check "a parked start during shutdown starts no watcher and removes the marker" \
    '[ ! -e "$T/watchers" ] && [ ! -e "$T/run/store-ready" ]'
check "a parked start during shutdown logs once" '[ "$(grep -c "shutdown in progress" "$T/console")" -eq 1 ]'
shutdown_step
check "shutdown stops the parked start" 'waitfor "gone $pid"'

fresh run-unprepared; rm -f "$T/run/store.conf"
start_run
check "a watcher whose store is not prepared yet exits to be respawned and never reports ready" \
    'waitfor "gone $pid" && [ ! -e "$T/watchers" ] && [ ! -e "$T/run/store-ready" ]'
fresh run-refused; rm -f "$T/run/store.conf"; : > "$T/run/boot-refused"
start_run; waitfor "grep -q 'ready: boot refused' \"\$T/console\"" || true
check "a guest whose boot was refused parks its watcher and never reports ready" \
    '! gone "$pid" && [ ! -e "$T/watchers" ] && [ ! -e "$T/run/store-ready" ]'
shutdown_step
check "shutdown stops a watcher parked after a refused boot" 'waitfor "gone $pid" && [ ! -e "$T/watchers" ]'

# --- Shutdown: unmount only once the watcher and the store are gone ---
fresh shutdown-orderly; echo "dev $T/data ext4 rw 0 0" > "$T/mounts"
start_store; start_run; waitfor "[ -s \"\$T/run/store-ready\" ]"; waitfor "[ -s \"\$T/servers\" ]"
check "orderly shutdown succeeds" 'shutdown_step'
check "orderly shutdown stops the watcher, then the store, removes the marker, then unmounts" \
    'gone "$pid" && gone "$spid" && [ ! -e "$T/run/store-ready" ] &&
     [ "$(grep "exited" "$T/console" | sed "s/.*stop: \([a-z]*\) pid.*/\1/" | tr "\n" "|")" = "ready|store|" ] &&
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

fresh shutdown-store-survivor; echo "dev $T/data ext4 rw 0 0" > "$T/mounts"
(trap '' TERM; exec sleep 60) & pid=$!; CHILDREN="$CHILDREN $pid"; record "$pid" store
check "shutdown with a surviving store reports failure and leaves every mount alone" \
    '! shutdown_step "$pid" && ! grep -q "^umount" "$T/calls.log" && grep -q "store pid=$pid STILL ALIVE" "$T/console"'
kill -KILL "$pid" 2>/dev/null || true

fresh shutdown-reused; echo "dev $T/data ext4 rw 0 0" > "$T/mounts"
(exec sleep 60) & pid=$!; CHILDREN="$CHILDREN $pid"; echo "$pid 1" > "$T/run/ready.pid"
shutdown_step
check "a recorded PID with another start time is never signalled" '! gone "$pid" && grep -q "no running ready" "$T/console"'
kill -KILL "$pid" 2>/dev/null || true

if [ "$fails" -ne 0 ]; then
    echo "  $fails failure(s)"
    exit 1
fi
