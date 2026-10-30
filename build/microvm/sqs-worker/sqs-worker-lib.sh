# sqs-worker-lib.sh — shared by the SQS worker guest's readiness and stop
# steps. Sourced, never executed. Every path and command is overridable so the
# host test can drive the same code against test-owned processes and stub tools.
#
# store-ready exists only while the running watcher's last check passed. A
# check proves the store accepted the worker key, not the store server's
# identity, and a store failure is noticed up to one interval plus call time late.

SQSW_RUN=${SQSW_RUN:-/run/sqs-worker}
SQSW_DATA=${SQSW_DATA:-/data}
SQSW_CONSOLE=${SQSW_CONSOLE:-/dev/console}
SQSW_MOUNTS=${SQSW_MOUNTS:-/proc/mounts}
SQSW_BIN=${SQSW_BIN:-/usr/local/bin/sqs-worker}
SQSW_STOP_BUDGET_TENTHS=${SQSW_STOP_BUDGET_TENTHS:-100}
SQSW_RESPAWN_GAP=${SQSW_RESPAWN_GAP:-5}
SQSW_PARK_INTERVAL=${SQSW_PARK_INTERVAL:-30}
UMOUNT=${UMOUNT:-umount}

sqsw_log() { echo "[sqs-worker] $*" >> "$SQSW_CONSOLE"; }

# sqsw_alive <pid>: running or stopped, not exited (a zombie counts as exited).
sqsw_alive() { [ -r "/proc/$1/status" ] && ! grep -q '^State:[[:space:]]*Z' "/proc/$1/status" 2>/dev/null; }

# sqsw_starttime <pid>: kernel start time (stat field 22). It survives exec, and
# pairing it with the pid reduces PID-reuse risk; it is not an absolute guarantee.
sqsw_starttime() { sed 's/.*) //' "/proc/$1/stat" 2>/dev/null | cut -d' ' -f20; }

# sqsw_same <pid> <starttime>: the recorded process is still alive.
sqsw_same() { [ -n "$2" ] && sqsw_alive "$1" && [ "$(sqsw_starttime "$1")" = "$2" ]; }

# sqsw_run: the respawned readiness body. A predecessor that was killed or
# crashed could not remove its marker, so every start removes it first. Its
# identity is recorded before the stopping check, as the store's run step does.
sqsw_run() {
    mkdir -p "$SQSW_RUN"
    echo "$$ $(sqsw_starttime $$)" > "$SQSW_RUN/ready.pid"
    if [ -e "$SQSW_RUN/store-ready" ]; then
        rm -f "$SQSW_RUN/store-ready"
        sqsw_log "ready: removed a store-ready marker left by an earlier watcher"
    fi
    # Returning here would make init respawn and log at once, forever. Park
    # instead, logging once: shutdown stops this recorded process, and a key
    # that appears later lets the watcher start.
    if [ -e "$SQSW_RUN/stopping" ] || [ ! -s "$SQSW_RUN/public-key" ]; then
        if [ -e "$SQSW_RUN/stopping" ]; then
            sqsw_log "ready: shutdown in progress, not starting"
        else
            sqsw_log "ready: no worker key; readiness not started, waiting"
        fi
        while [ -e "$SQSW_RUN/stopping" ] || [ ! -s "$SQSW_RUN/public-key" ]; do
            sleep "$SQSW_PARK_INTERVAL"
        done
    fi
    # Init respawns at once; a watcher that keeps exiting restarts at most
    # once per gap instead of in a tight loop.
    now=$(date +%s)
    last=$(cat "$SQSW_RUN/ready.last" 2>/dev/null || echo 0)
    wait=$((last + SQSW_RESPAWN_GAP - now))
    if [ "$wait" -gt 0 ] && [ "$wait" -le "$SQSW_RESPAWN_GAP" ]; then sleep "$wait"; fi
    date +%s > "$SQSW_RUN/ready.last"
    sqsw_log "ready: starting watcher pid=$$"
    exec "$SQSW_BIN" ready >> "$SQSW_CONSOLE" 2>&1
}

# sqsw_stop: block any respawn, drop the marker, SIGTERM the recorded watcher,
# SIGKILL only after the budget, and unmount the worker disk only once the
# watcher is confirmed gone. A survivor keeps the disk mounted.
sqsw_stop() {
    mkdir -p "$SQSW_RUN"
    : > "$SQSW_RUN/stopping"
    rm -f "$SQSW_RUN/store-ready"
    rec=$(cat "$SQSW_RUN/ready.pid" 2>/dev/null || true)
    pid=${rec%% *} st=${rec#* }
    [ "$st" != "$rec" ] || st=""
    if [ -n "$pid" ] && sqsw_same "$pid" "$st"; then
        kill -TERM "$pid" 2>/dev/null || true
        sqsw_log "stop: SIGTERM sent to readiness watcher pid=$pid"
        i=0
        while sqsw_same "$pid" "$st" && [ "$i" -lt "$SQSW_STOP_BUDGET_TENTHS" ]; do sleep 0.1; i=$((i + 1)); done
        if sqsw_same "$pid" "$st"; then
            sqsw_log "stop: readiness watcher pid=$pid still running after $((SQSW_STOP_BUDGET_TENTHS / 10))s, SIGKILL"
            kill -KILL "$pid" 2>/dev/null || true
            j=0
            while sqsw_same "$pid" "$st" && [ "$j" -lt 30 ]; do sleep 0.1; j=$((j + 1)); done
        fi
        if sqsw_same "$pid" "$st"; then
            rm -f "$SQSW_RUN/store-ready"
            sqsw_log "stop: readiness watcher pid=$pid STILL ALIVE, leaving $SQSW_DATA mounted"
            return 1
        fi
        sqsw_log "stop: readiness watcher pid=$pid exited"
    else
        sqsw_log "stop: no running readiness watcher"
    fi
    rm -f "$SQSW_RUN/store-ready" "$SQSW_RUN/ready.pid"
    sync
    if grep -q " $SQSW_DATA " "$SQSW_MOUNTS" 2>/dev/null; then
        rc=0
        "$UMOUNT" "$SQSW_DATA" || rc=$?
        sqsw_log "stop: umount $SQSW_DATA rc=$rc"
    fi
}

# sqsw_shutdown: the only shutdown action. The generic read-only remount runs
# only after the watcher stopped; a survivor keeps every mount as it is.
sqsw_shutdown() {
    if ! sqsw_stop; then
        sync
        sqsw_log "shutdown: readiness watcher survived, skipping umount -a -r"
        return 1
    fi
    rc=0
    "$UMOUNT" -a -r || rc=$?
    echo "[init] shutdown: umount -a -r rc=$rc" >> "$SQSW_CONSOLE"
}
