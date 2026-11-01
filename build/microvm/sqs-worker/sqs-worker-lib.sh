# sqs-worker-lib.sh — shared by the SQS guest's store, readiness and stop
# steps. Sourced, never executed. Every path and command is overridable so the
# host test can drive the same code against test-owned processes and stub tools.
#
# The guest runs its own SQS nats-server on loopback and the worker beside it.
# The server starts only after boot settled the receipt key; store-ready exists
# only while the running watcher's last check of that server passed.

SQSW_RUN=${SQSW_RUN:-/run/sqs-worker}
SQSW_DATA=${SQSW_DATA:-/data}
SQSW_CONSOLE=${SQSW_CONSOLE:-/dev/console}
SQSW_MOUNTS=${SQSW_MOUNTS:-/proc/mounts}
SQSW_BIN=${SQSW_BIN:-/usr/local/bin/sqs-worker}
SQSW_NATS_BIN=${SQSW_NATS_BIN:-/usr/local/bin/nats-server}
SQSW_STORE_PORT=${SQSW_STORE_PORT:-4222}
SQSW_STOP_BUDGET_TENTHS=${SQSW_STOP_BUDGET_TENTHS:-100}
SQSW_RESPAWN_GAP=${SQSW_RESPAWN_GAP:-5}
SQSW_PARK=${SQSW_PARK:-sleep 2147483647}
SQSW_BOOT_RETRY=${SQSW_BOOT_RETRY:-30}
UMOUNT=${UMOUNT:-umount}

sqsw_log() { echo "[sqs-worker] $*" >> "$SQSW_CONSOLE"; }

# sqsw_alive <pid>: running or stopped, not exited (a zombie counts as exited).
sqsw_alive() { [ -r "/proc/$1/status" ] && ! grep -q '^State:[[:space:]]*Z' "/proc/$1/status" 2>/dev/null; }

# sqsw_starttime <pid>: kernel start time (stat field 22). It survives exec, and
# pairing it with the pid reduces PID-reuse risk; it is not an absolute guarantee.
sqsw_starttime() { sed 's/.*) //' "/proc/$1/stat" 2>/dev/null | cut -d' ' -f20; }

# sqsw_same <pid> <starttime>: the recorded process is still alive.
sqsw_same() { [ -n "$2" ] && sqsw_alive "$1" && [ "$(sqsw_starttime "$1")" = "$2" ]; }

# sqsw_write_config <file>: the guest's own server. It listens only on
# loopback, with no monitor, route or cluster listener. Its max_payload admits
# a 1 MiB SQS message with its headers (ADR-0011:S4); the 1MB default does not.
sqsw_write_config() {
    cat > "$1" <<CONF
listen: "127.0.0.1:$SQSW_STORE_PORT"
server_name: "sqs-store"
max_payload: 2MiB
jetstream {
  store_dir: "$SQSW_DATA/jetstream"
  sync_interval: always
}
CONF
}

# sqsw_prepare_store: write and test the server config once boot has settled
# the receipt key. Returns non-zero, leaving the store unstarted, otherwise.
sqsw_prepare_store() {
    rm -f "$SQSW_RUN/store.conf"
    [ -s "$SQSW_RUN/receipt-ready" ] || { sqsw_log "store: receipt key not settled, store not prepared"; return 1; }
    umask 077
    sqsw_write_config "$SQSW_RUN/store.conf.new" || return 1
    if ! "$SQSW_NATS_BIN" -t -c "$SQSW_RUN/store.conf.new" > "$SQSW_RUN/config-test.out" 2>&1; then
        sqsw_log "store: FATAL: server config test failed: $(tr '\n' ' ' < "$SQSW_RUN/config-test.out")"
        return 1
    fi
    mv "$SQSW_RUN/store.conf.new" "$SQSW_RUN/store.conf"
    sqsw_log "store: config written for 127.0.0.1:$SQSW_STORE_PORT; server not started yet"
}

# sqsw_park <why>: init respawns a step that exits, so a step that must not run
# logs once and waits for shutdown instead of looping.
sqsw_park() {
    sqsw_log "$1"
    exec $SQSW_PARK
}

# sqsw_record <name>: record this process's identity before the stopping
# check, so a stop that misses the record has already set stopping.
sqsw_record() { echo "$$ $(sqsw_starttime $$)" > "$SQSW_RUN/$1.pid"; }

# sqsw_boot: settle the disk and receipt key once per guest boot. A transient
# failure (exit 75) waits and exits so init runs it again; boot never remounts
# a disk already mounted. Any other failure is final for this guest boot.
sqsw_boot() {
    [ ! -e "$SQSW_RUN/boot-refused" ] || sqsw_park "store: boot refused, not starting"
    [ -s "$SQSW_RUN/receipt-ready" ] && return 0
    # Boot is recorded on its own so shutdown stops it before unmounting.
    "$SQSW_BIN" boot >> "$SQSW_CONSOLE" 2>&1 &
    child=$!
    echo "$child $(sqsw_starttime "$child")" > "$SQSW_RUN/boot.pid"
    [ ! -e "$SQSW_RUN/stopping" ] || kill -TERM "$child" 2>/dev/null || true
    rc=0
    wait "$child" || rc=$?
    rm -f "$SQSW_RUN/boot.pid"
    [ ! -e "$SQSW_RUN/stopping" ] || sqsw_park "store: shutdown during boot, not starting"
    case "$rc" in
        0) return 0 ;;
        75) sqsw_log "store: boot not settled, retrying in ${SQSW_BOOT_RETRY}s"
            sleep "$SQSW_BOOT_RETRY"
            exit 1 ;;
        *) : > "$SQSW_RUN/boot-refused"
           sqsw_park "store: boot refused (rc=$rc), not starting" ;;
    esac
}

# sqsw_store: the respawned server body. It starts only on a prepared config.
sqsw_store() {
    mkdir -p "$SQSW_RUN"
    sqsw_record store
    [ ! -e "$SQSW_RUN/stopping" ] || sqsw_park "store: shutdown in progress, not starting"
    sqsw_boot
    if [ ! -s "$SQSW_RUN/store.conf" ] && ! sqsw_prepare_store; then
        : > "$SQSW_RUN/boot-refused"
        sqsw_park "store: not prepared, not starting"
    fi
    sqsw_log "store: starting pid=$$"
    # ADR-0011:S11 bounds the store's Go heap; the guest must be larger.
    GOMEMLIMIT=512MiB exec "$SQSW_NATS_BIN" -c "$SQSW_RUN/store.conf"
}

# sqsw_run: the respawned readiness body. A predecessor that was killed or
# crashed could not remove its marker, so every start removes it first.
sqsw_run() {
    mkdir -p "$SQSW_RUN"
    sqsw_record ready
    if [ -e "$SQSW_RUN/store-ready" ]; then
        rm -f "$SQSW_RUN/store-ready"
        sqsw_log "ready: removed a store-ready marker left by an earlier watcher"
    fi
    [ ! -e "$SQSW_RUN/stopping" ] || sqsw_park "ready: shutdown in progress, not starting"
    [ ! -e "$SQSW_RUN/boot-refused" ] || sqsw_park "ready: boot refused, readiness not started"
    # Boot may still be retrying; check again after a gap rather than parking.
    if [ ! -s "$SQSW_RUN/store.conf" ]; then
        sleep "$SQSW_RESPAWN_GAP"
        exit 0
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

# sqsw_end <name>: SIGTERM the recorded process, SIGKILL only after the
# budget. Returns non-zero while it is still alive.
sqsw_end() {
    rec=$(cat "$SQSW_RUN/$1.pid" 2>/dev/null || true)
    pid=${rec%% *} st=${rec#* }
    [ "$st" != "$rec" ] || st=""
    if [ -z "$pid" ] || ! sqsw_same "$pid" "$st"; then
        sqsw_log "stop: no running $1"
        return 0
    fi
    kill -TERM "$pid" 2>/dev/null || true
    sqsw_log "stop: SIGTERM sent to $1 pid=$pid"
    i=0
    while sqsw_same "$pid" "$st" && [ "$i" -lt "$SQSW_STOP_BUDGET_TENTHS" ]; do sleep 0.1; i=$((i + 1)); done
    if sqsw_same "$pid" "$st"; then
        sqsw_log "stop: $1 pid=$pid still running after $((SQSW_STOP_BUDGET_TENTHS / 10))s, SIGKILL"
        kill -KILL "$pid" 2>/dev/null || true
        j=0
        while sqsw_same "$pid" "$st" && [ "$j" -lt 30 ]; do sleep 0.1; j=$((j + 1)); done
    fi
    if sqsw_same "$pid" "$st"; then
        sqsw_log "stop: $1 pid=$pid STILL ALIVE"
        return 1
    fi
    sqsw_log "stop: $1 pid=$pid exited"
    rm -f "$SQSW_RUN/$1.pid"
}

# sqsw_stop: block any respawn, drop the marker, stop the watcher, the server
# and any boot in progress, and unmount the disk only once all are confirmed
# gone. A survivor keeps the disk mounted.
sqsw_stop() {
    mkdir -p "$SQSW_RUN"
    : > "$SQSW_RUN/stopping"
    rm -f "$SQSW_RUN/store-ready"
    if ! sqsw_end ready || ! sqsw_end store || ! sqsw_end boot; then
        rm -f "$SQSW_RUN/store-ready"
        sqsw_log "stop: leaving $SQSW_DATA mounted"
        return 1
    fi
    rm -f "$SQSW_RUN/store-ready"
    sync
    if grep -q " $SQSW_DATA " "$SQSW_MOUNTS" 2>/dev/null; then
        rc=0
        "$UMOUNT" "$SQSW_DATA" || rc=$?
        sqsw_log "stop: umount $SQSW_DATA rc=$rc"
    fi
}

# sqsw_shutdown: the only shutdown action. The generic read-only remount runs
# only after both processes stopped; a survivor keeps every mount as it is.
sqsw_shutdown() {
    if ! sqsw_stop; then
        sync
        sqsw_log "shutdown: a guest process survived, skipping umount -a -r"
        return 1
    fi
    rc=0
    "$UMOUNT" -a -r || rc=$?
    echo "[init] shutdown: umount -a -r rc=$rc" >> "$SQSW_CONSOLE"
}
