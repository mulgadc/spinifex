# sqs-store-lib.sh — shared by the SQS store guest's prepare, run and stop
# steps. Sourced, never executed. Every path and command is overridable so the
# host test can drive the same code against a fake fw_cfg, sysfs and tools.
#
# Guest contract, read from fw_cfg under opt/spinifex/sqs-store/:
#   data-serial  virtio-blk serial of the already-formatted ext4 data disk
#   listen-addr  IPv4 address on a configured guest NIC; the only listener
#   client-nkey  NATS user NKey public key allowed to connect; never a seed
# Anything absent, malformed or ambiguous refuses the store. Nothing formats.

SQS_FWCFG=${SQS_FWCFG:-/sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/sqs-store}
SQS_SYSBLOCK=${SQS_SYSBLOCK:-/sys/block}
SQS_DEVDIR=${SQS_DEVDIR:-/dev}
# The host test cannot create block nodes, so it relaxes this to -e.
SQS_NODE_TEST=${SQS_NODE_TEST:--b}
SQS_RUN=${SQS_RUN:-/run/sqs-store}
SQS_DATA=${SQS_DATA:-/data}
SQS_CONSOLE=${SQS_CONSOLE:-/dev/console}
SQS_MOUNTS=${SQS_MOUNTS:-/proc/mounts}
SQS_PORT=${SQS_PORT:-4222}
SQS_STOP_BUDGET_TENTHS=${SQS_STOP_BUDGET_TENTHS:-120}
NATS_BIN=${NATS_BIN:-/usr/local/bin/nats-server}
E2FSCK=${E2FSCK:-e2fsck}
MOUNT=${MOUNT:-mount}
UMOUNT=${UMOUNT:-umount}
IP=${IP:-ip}

sqs_log() { echo "[sqs-store] $*" >> "$SQS_CONSOLE"; }
sqs_fatal() { sqs_log "FATAL: $*"; return 1; }

# sqs_fwcfg <name>: print one fw_cfg value, refusing a missing, empty, oversized
# or multi-token file. The value is not echoed on failure.
sqs_fwcfg() {
    f="$SQS_FWCFG/$1/raw"
    [ -f "$f" ] || { sqs_fatal "fw_cfg $1 not supplied"; return 1; }
    [ "$(wc -c < "$f")" -le 128 ] || { sqs_fatal "fw_cfg $1 is oversized"; return 1; }
    v=$(cat "$f")
    case "$v" in
        "" | *[[:space:]]*) sqs_fatal "fw_cfg $1 must be exactly one value"; return 1 ;;
    esac
    printf '%s' "$v"
}

# A volume serial is the volume ID with dashes stripped: vol + 17 hex.
sqs_valid_serial() {
    [ "${#1}" -eq 20 ] || return 1
    case "$1" in vol*) ;; *) return 1 ;; esac
    case "${1#vol}" in *[!0-9a-f]*) return 1 ;; esac
}

# A NATS user public NKey: "U" then 55 base32 characters. The checksum is
# left to the pinned server's own config test.
sqs_valid_user_nkey() {
    [ "${#1}" -eq 56 ] || return 1
    case "$1" in U*) ;; *) return 1 ;; esac
    case "$1" in *[!A-Z2-7]*) return 1 ;; esac
}

sqs_valid_ipv4() {
    case "$1" in *[!0-9.]* | .* | *. | *..*) return 1 ;; esac
    old_ifs=$IFS; IFS=.
    # shellcheck disable=SC2086 # split on dots on purpose
    set -- $1
    IFS=$old_ifs
    [ "$#" -eq 4 ] || return 1
    for o in "$@"; do
        [ "${#o}" -le 3 ] && [ "$o" -le 255 ] || return 1
        case "$o" in 0?*) return 1 ;; esac
    done
}

# sqs_find_disk <serial>: print the /dev node of the one block device whose
# sysfs serial matches exactly. Zero or several matches refuse.
sqs_find_disk() {
    found="" n=0
    for b in "$SQS_SYSBLOCK"/*; do
        [ -r "$b/serial" ] || continue
        [ "$(cat "$b/serial")" = "$1" ] || continue
        found=${b##*/}
        n=$((n + 1))
    done
    [ "$n" -eq 1 ] || { sqs_fatal "expected one disk with serial $1, found $n"; return 1; }
    test "$SQS_NODE_TEST" "$SQS_DEVDIR/$found" || { sqs_fatal "disk $found has no block device node"; return 1; }
    printf '%s' "$SQS_DEVDIR/$found"
}

# sqs_has_addr <ipv4>: the address is assigned to a non-loopback interface.
sqs_has_addr() {
    "$IP" -o -4 addr show 2>/dev/null \
        | awk -v a="$1" '$2 != "lo" { split($4, p, "/"); if (p[1] == a) f = 1 } END { exit !f }'
}

# sqs_alive <pid>: running or stopped, not exited (a zombie counts as exited).
sqs_alive() { [ -r "/proc/$1/status" ] && ! grep -q '^State:[[:space:]]*Z' "/proc/$1/status" 2>/dev/null; }

# sqs_starttime <pid>: kernel start time (stat field 22). It survives exec, and
# pairing it with the pid reduces PID-reuse risk; without a pidfd the check and
# the signal stay separate steps, so it is not an absolute guarantee.
sqs_starttime() { sed 's/.*) //' "/proc/$1/stat" 2>/dev/null | cut -d' ' -f20; }

# sqs_same <pid> <starttime>: the recorded process is still alive.
sqs_same() { [ -n "$2" ] && sqs_alive "$1" && [ "$(sqs_starttime "$1")" = "$2" ]; }

# sqs_prepare: validate the contract, check and mount the data disk, write the
# server config and mark the store ready. Returns non-zero, unready, on any refusal.
sqs_prepare() {
    rm -f "$SQS_RUN/ready"
    mkdir -p "$SQS_RUN"
    serial=$(sqs_fwcfg data-serial) || return 1
    sqs_valid_serial "$serial" || { sqs_fatal "data-serial is not a volume serial"; return 1; }
    addr=$(sqs_fwcfg listen-addr) || return 1
    sqs_valid_ipv4 "$addr" || { sqs_fatal "listen-addr is not an IPv4 address"; return 1; }
    nkey=$(sqs_fwcfg client-nkey) || return 1
    sqs_valid_user_nkey "$nkey" || { sqs_fatal "client-nkey is not a user public NKey"; return 1; }
    sqs_has_addr "$addr" || { sqs_fatal "listen-addr $addr is not on any guest NIC"; return 1; }

    # The server checks the config, including the NKey checksum, before the
    # disk is touched.
    umask 077
    cat > "$SQS_RUN/nats.conf" <<EOF
listen: "$addr:$SQS_PORT"
server_name: "sqs-store"
jetstream {
  store_dir: "$SQS_DATA"
  sync_interval: always
}
authorization {
  users = [ { nkey: "$nkey" } ]
}
EOF
    if ! "$NATS_BIN" -t -c "$SQS_RUN/nats.conf" > "$SQS_RUN/config-test.out" 2>&1; then
        sqs_fatal "server config test failed: $(sed 's/U[A-Z2-7]\{55\}/<nkey>/g' "$SQS_RUN/config-test.out" | tr '\n' ' ')"
        return 1
    fi

    dev=$(sqs_find_disk "$serial") || return 1
    rc=0
    "$E2FSCK" -p "$dev" > "$SQS_RUN/fsck.out" 2>&1 || rc=$?
    sqs_log "e2fsck -p $dev rc=$rc"
    # 0 clean, 1 corrected: proceed. Anything higher, including a disk that is
    # not ext4 at all, refuses; the guest never formats.
    [ "$rc" -le 1 ] || { sqs_fatal "e2fsck refused $dev: $(tr '\n' ' ' < "$SQS_RUN/fsck.out")"; return 1; }
    mkdir -p "$SQS_DATA"
    "$MOUNT" -t ext4 -o nodev,nosuid,noexec "$dev" "$SQS_DATA" || { sqs_fatal "mount $dev failed"; return 1; }
    : > "$SQS_RUN/ready"
    sqs_log "prepared: $dev mounted at $SQS_DATA, config written for $addr:$SQS_PORT; server not started yet"
}

# sqs_run: the respawned supervisor body. Its identity is recorded before the
# stopping check: a stop that misses it set stopping first, so this parks; a
# stop that reads it signals this process whether or not it has exec'd yet.
sqs_run() {
    mkdir -p "$SQS_RUN"
    echo "$$ $(sqs_starttime $$)" > "$SQS_RUN/pid"
    if [ -e "$SQS_RUN/stopping" ]; then
        sqs_log "run: shutdown in progress, not starting"
        return 2
    fi
    if [ ! -e "$SQS_RUN/ready" ]; then
        sqs_log "run: store not prepared, not starting"
        return 2
    fi
    sqs_log "run: starting pid=$$"
    # ADR-0011:S11 bounds the store's Go heap; the guest must be larger.
    GOMEMLIMIT=512MiB exec "$NATS_BIN" -c "$SQS_RUN/nats.conf"
}

# sqs_stop: first shutdown action. Block any respawn, SIGTERM the recorded run
# process (a server, or a start not yet exec'd), SIGKILL only after the budget,
# then sync and unmount. Each signal rechecks the identity; a survivor keeps the disk mounted.
sqs_stop() {
    mkdir -p "$SQS_RUN"
    : > "$SQS_RUN/stopping"
    sqs_log "stop: begin"
    rec=$(cat "$SQS_RUN/pid" 2>/dev/null || true)
    pid=${rec%% *} st=${rec#* }
    [ "$st" != "$rec" ] || st=""
    if [ -n "$pid" ] && sqs_same "$pid" "$st"; then
        kill -TERM "$pid" 2>/dev/null || true
        sqs_log "stop: SIGTERM sent to run pid=$pid"
        i=0
        while sqs_same "$pid" "$st" && [ "$i" -lt "$SQS_STOP_BUDGET_TENTHS" ]; do sleep 0.1; i=$((i + 1)); done
        if sqs_same "$pid" "$st"; then
            sqs_log "stop: run pid=$pid still running after $((SQS_STOP_BUDGET_TENTHS / 10))s, SIGKILL"
            kill -KILL "$pid" 2>/dev/null || true
            j=0
            while sqs_same "$pid" "$st" && [ "$j" -lt 30 ]; do sleep 0.1; j=$((j + 1)); done
        fi
        if sqs_same "$pid" "$st"; then
            sqs_log "stop: run pid=$pid STILL ALIVE, leaving $SQS_DATA mounted"
            return 1
        fi
        sqs_log "stop: store exited after $((i / 10)).$((i % 10))s"
    else
        sqs_log "stop: no running store"
    fi
    sync
    if grep -q " $SQS_DATA " "$SQS_MOUNTS" 2>/dev/null; then
        rc=0
        "$UMOUNT" "$SQS_DATA" || rc=$?
        sqs_log "stop: umount $SQS_DATA rc=$rc"
    fi
}

# sqs_shutdown: the only shutdown action. The generic read-only remount runs
# only after the store stopped; a surviving writer keeps every mount as it is.
sqs_shutdown() {
    if ! sqs_stop; then
        sync
        sqs_log "shutdown: store survived, skipping umount -a -r"
        return 1
    fi
    rc=0
    "$UMOUNT" -a -r || rc=$?
    echo "[init] shutdown: umount -a -r rc=$rc" >> "$SQS_CONSOLE"
}
