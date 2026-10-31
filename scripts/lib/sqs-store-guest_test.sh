#!/bin/sh
# sqs-store-guest_test.sh — the SQS store guest steps start the store only on
# a valid contract and one checked, mounted disk, never format, and stop the
# store before unmounting it.
#
# Drives build/microvm/sqs-store/sqs-store-lib.sh against a fake fw_cfg, sysfs
# and stub tools. The stop cases signal only test-owned children.
# The case settings below are read by the sourced library, not by this file.
# shellcheck disable=SC2034
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
LIB=${SQS_GUEST_LIB:-"$ROOT/build/microvm/sqs-store/sqs-store-lib.sh"}
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

echo "-- build/microvm/sqs-store/sqs-store-lib.sh"

SERIAL=vol0123456789abcdef0
NKEY=UAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAB
ADDR=10.11.12.13

# Stub tools record each call to calls.log; their exit codes come from files.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/e2fsck" <<'EOF'
#!/bin/sh
echo "e2fsck $*" >> "$T/calls.log"
exit "$(cat "$T/fsck.rc")"
EOF
cat > "$TMP/bin/mount" <<'EOF'
#!/bin/sh
echo "mount $*" >> "$T/calls.log"
exit "$(cat "$T/mount.rc")"
EOF
cat > "$TMP/bin/umount" <<'EOF'
#!/bin/sh
echo "umount $*" >> "$T/calls.log"
EOF
for f in mkfs mkfs.ext4 mke2fs; do
    printf '#!/bin/sh\necho "mkfs %s $*" >> "$T/calls.log"\n' "$f" > "$TMP/bin/$f"
done
cat > "$TMP/bin/ip" <<'EOF'
#!/bin/sh
cat "$T/ip.out"
EOF
cat > "$TMP/bin/nats-server" <<'EOF'
#!/bin/sh
echo "nats-server $*" >> "$T/calls.log"
[ "$1" = -t ] || echo "GOMEMLIMIT=${GOMEMLIMIT:-unset}" >> "$T/env.log"
[ "$1" = -t ] && exit "$(cat "$T/configtest.rc")"
exit 0
EOF
chmod +x "$TMP/bin"/*

# fresh <name>: a case directory with a valid contract and one matching disk
# among others. Callers then break one thing.
fresh() {
    T="$TMP/$1"
    export T
    mkdir -p "$T/fwcfg" "$T/sys" "$T/dev" "$T/run" "$T/data"
    for k in data-serial listen-addr client-nkey; do mkdir -p "$T/fwcfg/$k"; done
    printf '%s\n' "$SERIAL" > "$T/fwcfg/data-serial/raw"
    printf '%s\n' "$ADDR" > "$T/fwcfg/listen-addr/raw"
    printf '%s\n' "$NKEY" > "$T/fwcfg/client-nkey/raw"
    disk vda vol00000000000000000
    disk vdb "$SERIAL"
    echo 0 > "$T/fsck.rc"
    echo 0 > "$T/mount.rc"
    echo 0 > "$T/configtest.rc"
    : > "$T/calls.log"
    : > "$T/console"
    printf '1: lo    inet 127.0.0.1/8 scope host lo\n2: eth0    inet %s/24 brd 10.11.12.255 scope global eth0\n' "$ADDR" > "$T/ip.out"
}
disk() { mkdir -p "$T/sys/$1"; printf '%s\n' "$2" > "$T/sys/$1/serial"; : > "$T/dev/$1"; }

# prepare: run sqs_prepare in a subshell with every path pointed at the case.
prepare() {
    (
        SQS_FWCFG="$T/fwcfg" SQS_SYSBLOCK="$T/sys" SQS_DEVDIR="$T/dev" SQS_NODE_TEST=-e
        SQS_RUN="$T/run" SQS_DATA="$T/data" SQS_CONSOLE="$T/console"
        E2FSCK="$TMP/bin/e2fsck" MOUNT="$TMP/bin/mount" IP="$TMP/bin/ip" NATS_BIN="$TMP/bin/nats-server"
        PATH="$TMP/bin:$PATH"
        export PATH
        # shellcheck source=../../build/microvm/sqs-store/sqs-store-lib.sh
        . "$LIB"
        sqs_prepare
    ) > /dev/null 2>&1
}
refused() { [ ! -e "$T/run/ready" ] && grep -q FATAL "$T/console"; }
never_mounted() { ! grep -q '^mount ' "$T/calls.log"; }
never_formatted() { ! grep -q '^mkfs ' "$T/calls.log"; }
never_checked() { ! grep -q '^e2fsck ' "$T/calls.log"; }

# --- A valid contract prepares the store ---
fresh valid
check "valid contract is prepared" 'prepare && [ -e "$T/run/ready" ]'
check "the matching disk is checked, then mounted at the data dir" \
    'grep -qx "e2fsck -p $T/dev/vdb" "$T/calls.log" && grep -qx "mount -t ext4 -o nodev,nosuid,noexec $T/dev/vdb $T/data" "$T/calls.log"'
check "config listens only on the supplied address" \
    'grep -c "listen" "$T/run/nats.conf" | grep -qx 1 && grep -qx "listen: \"$ADDR:4222\"" "$T/run/nats.conf"'
check "config admits only the supplied user NKey" 'grep -q "users = \[ { nkey: \"$NKEY\" } \]" "$T/run/nats.conf"'
check "config syncs JetStream on the data disk" \
    'grep -q "store_dir: \"$T/data\"" "$T/run/nats.conf" && grep -q "sync_interval: always" "$T/run/nats.conf"'
check "config admits a full SQS message (2MiB max_payload)" \
    'grep -c "max_payload" "$T/run/nats.conf" | grep -qx 1 && grep -qx "max_payload: 2MiB" "$T/run/nats.conf"'
check "config is tested by the server before ready" 'grep -qx "nats-server -t -c $T/run/nats.conf" "$T/calls.log"'
check "no auth material on the console" '! grep -q "$NKEY" "$T/console"'
check "prepare reports the disk and config, not a listener" 'grep -q "prepared: $T/dev/vdb mounted" "$T/console" && ! grep -qi "listening" "$T/console"'

fresh fsck-corrected; echo 1 > "$T/fsck.rc"
check "e2fsck exit 1 (corrected) proceeds" 'prepare && [ -e "$T/run/ready" ]'

# --- Disk refusals: nothing mounted, nothing formatted ---
fresh no-disk; rm -rf "$T/sys/vdb"
check "missing disk is refused before any check or mount" '! prepare && refused && never_checked && never_mounted'

fresh wrong-disk; printf 'vol0fffffffffffffff0\n' > "$T/fwcfg/data-serial/raw"
check "disk with another serial is refused" '! prepare && refused && never_checked && never_mounted'

fresh two-disks; disk vdc "$SERIAL"
check "two disks with the serial are refused" '! prepare && refused && never_checked && never_mounted'

fresh bad-serial; printf 'vol-0123456789abcdef\n' > "$T/fwcfg/data-serial/raw"
check "malformed serial is refused" '! prepare && refused && never_mounted'

fresh no-node; rm -f "$T/dev/vdb"
check "disk without a device node is refused" '! prepare && refused && never_mounted'

for rc in 2 4 8; do
    fresh "fsck-$rc"; echo "$rc" > "$T/fsck.rc"
    check "e2fsck exit $rc is refused without mount or format" '! prepare && refused && never_mounted && never_formatted'
done

fresh mount-fail; echo 32 > "$T/mount.rc"
check "failed mount is refused" '! prepare && refused'

# --- Auth and listener refusals happen before the disk is touched ---
fresh no-nkey; rm -rf "$T/fwcfg/client-nkey"
check "missing NKey is refused before the disk" '! prepare && refused && never_checked && never_mounted'

for bad in seed lower short two empty; do
    fresh "nkey-$bad"
    case $bad in
        seed) v=SUAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA ;;
        lower) v=uaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab ;;
        short) v=UAAAA ;;
        two) v="$NKEY
$NKEY" ;;
        empty) v="" ;;
    esac
    printf '%s\n' "$v" > "$T/fwcfg/client-nkey/raw"
    check "NKey ($bad) is refused before the disk, without echoing it" \
        '! prepare && refused && never_checked && { [ -z "$v" ] || ! grep -qF "$v" "$T/console"; }'
done

fresh nkey-checksum; echo 1 > "$T/configtest.rc"
check "NKey the server rejects is refused before the disk" '! prepare && refused && never_checked && never_mounted'

fresh no-addr; rm -rf "$T/fwcfg/listen-addr"
check "missing listen address is refused" '! prepare && refused && never_mounted'

fresh addr-absent; printf '1: lo    inet 127.0.0.1/8 scope host lo\n2: eth0    inet 10.11.12.99/24 scope global eth0\n' > "$T/ip.out"
check "listen address not on a guest NIC is refused" '! prepare && refused && never_mounted'

fresh addr-loopback; printf '127.0.0.1\n' > "$T/fwcfg/listen-addr/raw"
check "loopback listen address is refused" '! prepare && refused && never_mounted'

for bad in 0.0.0.0x 1.2.3 1.2.3.256 01.2.3.4; do
    fresh "addr-$bad"; printf '%s\n' "$bad" > "$T/fwcfg/listen-addr/raw"
    check "malformed listen address $bad is refused" '! prepare && refused && never_mounted'
done

# --- Run: the store starts only when prepared and not stopping ---
run_step() {
    (
        SQS_RUN="$T/run" SQS_CONSOLE="$T/console" NATS_BIN="$TMP/bin/nats-server"
        # shellcheck source=../../build/microvm/sqs-store/sqs-store-lib.sh
        . "$LIB"
        sqs_run
    ) > /dev/null 2>&1
}
fresh run-ready; prepare; : > "$T/calls.log"
check "a prepared store starts the server" 'run_step && grep -qx "nats-server -c $T/run/nats.conf" "$T/calls.log"'
fresh run-memlimit; prepare; : > "$T/env.log"
check "the server runs under the ADR-0011:S11 heap limit" 'run_step && grep -qx "GOMEMLIMIT=512MiB" "$T/env.log"'
fresh run-unready
check "an unprepared store never starts the server" '! run_step && ! grep -q "^nats-server" "$T/calls.log"'
fresh run-stopping; prepare; : > "$T/calls.log"; : > "$T/run/stopping"
check "a stopping store never starts the server" '! run_step && ! grep -q "^nats-server" "$T/calls.log"'

# --- Stop: SIGTERM the verified store, then unmount ---
STORE="$TMP/bin/fake-store"
cp "$(command -v sleep)" "$STORE"
stop_step() {
    (
        SQS_RUN="$T/run" SQS_DATA="$T/data" SQS_CONSOLE="$T/console" SQS_MOUNTS="$T/mounts"
        UMOUNT="$TMP/bin/umount" NATS_BIN="$STORE" SQS_STOP_BUDGET_TENTHS=10
        # shellcheck source=../../build/microvm/sqs-store/sqs-store-lib.sh
        . "$LIB"
        sqs_stop
    ) > /dev/null 2>&1
}
gone() { [ ! -r "/proc/$1/status" ] || grep -q '^State:[[:space:]]*Z' "/proc/$1/status"; }
# record <pid>: write the identity sqs_run would record for that process.
# shellcheck source=../../build/microvm/sqs-store/sqs-store-lib.sh
record() { echo "$1 $( . "$LIB"; sqs_starttime "$1" )" > "$T/run/pid"; }

fresh stop-orderly; echo "dev $T/data ext4 rw 0 0" > "$T/mounts"
"$STORE" 60 & pid=$!; CHILDREN="$CHILDREN $pid"; record "$pid"
stop_step
check "stop sends SIGTERM and the store exits" 'gone "$pid" && grep -q "SIGTERM sent" "$T/console" && ! grep -q SIGKILL "$T/console"'
check "stop blocks respawn and unmounts after the store exits" \
    '[ -e "$T/run/stopping" ] && grep -qx "umount $T/data" "$T/calls.log"'

fresh stop-hung; echo "dev $T/data ext4 rw 0 0" > "$T/mounts"
(trap '' TERM; exec "$STORE" 60) & pid=$!; CHILDREN="$CHILDREN $pid"; record "$pid"
stop_step
check "store ignoring SIGTERM is killed after the budget, then unmounted" \
    'gone "$pid" && grep -q SIGKILL "$T/console" && grep -qx "umount $T/data" "$T/calls.log"'

fresh stop-reused; echo "dev $T/data ext4 rw 0 0" > "$T/mounts"
"$STORE" 60 & pid=$!; CHILDREN="$CHILDREN $pid"; echo "$pid 1" > "$T/run/pid"
stop_step
check "a recorded PID with another start time is never signalled" '! gone "$pid" && grep -q "no running store" "$T/console"'
kill -KILL "$pid"

# A start held after its stopping check and before its exec: the run step
# blocks writing "run: starting" to a FIFO console until the test reads it.
fresh stop-pending-start; echo "dev $T/data ext4 rw 0 0" > "$T/mounts"
: > "$T/run/ready"
mkfifo "$T/run-console"
printf '#!/bin/sh\n: > "%s/started"\nexec "%s" 60\n' "$T" "$STORE" > "$TMP/bin/pending-nats"
chmod +x "$TMP/bin/pending-nats"
SQS_RUN="$T/run" SQS_CONSOLE="$T/run-console" NATS_BIN="$TMP/bin/pending-nats" \
    sh -c '. "$1"; sqs_run' sh "$LIB" & pid=$!; CHILDREN="$CHILDREN $pid"
i=0
while [ "$i" -lt 100 ] && ! grep -q "^$pid " "$T/run/pid" 2>/dev/null; do i=$((i + 1)); sleep 0.05; done
check "pending start is held before its exec" 'grep -q "^$pid " "$T/run/pid" && ! gone "$pid" && [ ! -e "$T/started" ]'
stop_step
# Release the FIFO: a start that survived the stop would exec the server now.
cat "$T/run-console" > /dev/null 2>&1 &
reader=$!; CHILDREN="$CHILDREN $reader"
i=0
while [ "$i" -lt 20 ] && ! gone "$pid"; do i=$((i + 1)); sleep 0.05; done
check "stop ends a pending start before it can exec the server" 'gone "$pid" && [ ! -e "$T/started" ]'
check "stop unmounts only after the pending start is gone" \
    'grep -q "store exited" "$T/console" && grep -qx "umount $T/data" "$T/calls.log"'
kill -KILL "$reader" 2>/dev/null || true

# --- Shutdown: the generic remount runs only once the store is gone ---
# shutdown_step [survivor-pid]: with a pid, that process is reported alive
# whatever happens to it, as a writer stuck past SIGKILL would be.
shutdown_step() {
    (
        SQS_RUN="$T/run" SQS_DATA="$T/data" SQS_CONSOLE="$T/console" SQS_MOUNTS="$T/mounts"
        UMOUNT="$TMP/bin/umount" SQS_STOP_BUDGET_TENTHS=5
        # shellcheck source=../../build/microvm/sqs-store/sqs-store-lib.sh
        . "$LIB"
        if [ -n "${1:-}" ]; then sqs_same() { [ "$1" = "$survivor" ]; }; survivor=$1; fi
        sqs_shutdown
    ) > /dev/null 2>&1
}

fresh shutdown-orderly; echo "dev $T/data ext4 rw 0 0" > "$T/mounts"
"$STORE" 60 & pid=$!; CHILDREN="$CHILDREN $pid"; record "$pid"
check "orderly shutdown succeeds" 'shutdown_step'
check "orderly shutdown unmounts the data disk, then runs umount -a -r" \
    '[ "$(grep "^umount" "$T/calls.log" | tr "\n" "|")" = "umount $T/data|umount -a -r|" ]'

fresh shutdown-survivor; echo "dev $T/data ext4 rw 0 0" > "$T/mounts"
"$STORE" 60 & pid=$!; CHILDREN="$CHILDREN $pid"; record "$pid"
check "shutdown with a surviving store reports failure" '! shutdown_step "$pid"'
check "a surviving store leaves the data disk and every other mount alone" \
    '! grep -q "^umount" "$T/calls.log" && grep -q "STILL ALIVE" "$T/console" && grep -q "skipping umount -a -r" "$T/console"'

fresh stop-unmounted; : > "$T/mounts"
stop_step
check "stop with no data mount does not unmount" '! grep -q "^umount" "$T/calls.log"'

if [ "$fails" -ne 0 ]; then
    echo "  $fails failure(s)"
    exit 1
fi
