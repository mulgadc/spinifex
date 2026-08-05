#!/bin/sh
# Self-contained POSIX tests for exact RDS data-volume selection and formatting
# authorization. Fake sysfs and command stubs avoid root and block devices.
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${SCRIPT_DIR}/rds-datadir"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT
STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

cat > "${STUBBIN}/blkid" <<'EOF'
#!/bin/sh
[ "$1" = "-p" ] && [ "$2" = "-o" ] && [ "$3" = "export" ] || exit 4
dev="$4"
kind=$(awk -v d="${dev}" '$1 == d { print $2; exit }' "${PROBE_TABLE}" 2>/dev/null)
value=$(awk -v d="${dev}" '$1 == d { print $3; exit }' "${PROBE_TABLE}" 2>/dev/null)
case "${kind}" in
    fs) printf 'DEVNAME=%s\nTYPE=%s\n' "${dev}" "${value}" ;;
    pttable) printf 'DEVNAME=%s\nPTTYPE=%s\n' "${dev}" "${value:-gpt}" ;;
    fspt) printf 'DEVNAME=%s\nTYPE=%s\nPTTYPE=gpt\n' "${dev}" "${value}" ;;
    ambiguous) exit 8 ;;
    unreadable) echo "read error" >&2; exit 2 ;;
    error) exit 4 ;;
    *) exit 2 ;;
esac
EOF

cat > "${STUBBIN}/mkfs.ext4" <<'EOF'
#!/bin/sh
echo "mkfs.ext4 $*" >> "${MKFS_CALLS}"
[ "${MKFS_FAIL:-0}" = "1" ] && exit 1
dev=""
for arg in "$@"; do dev="${arg}"; done
printf '%s fs ext4\n' "${dev}" >> "${PROBE_TABLE}"
EOF

cat > "${STUBBIN}/mount" <<'EOF'
#!/bin/sh
echo "mount $*" >> "${MOUNT_CALLS}"
exit "${MOUNT_RC:-0}"
EOF

cat > "${STUBBIN}/chown" <<'EOF'
#!/bin/sh
echo "chown $*" >> "${CHOWN_CALLS}"
EOF

chmod +x "${STUBBIN}"/*
PATH="${STUBBIN}:${PATH}"
export PATH

FAILS=0
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

DATA_MOUNT="${WORK}/data"
SYS_BLOCK="${WORK}/sys/block"
DEV_DIR="${WORK}/dev"
MOUNTS="${WORK}/mounts"
HANDOFF="${WORK}/handoff/bootstrap.env"
PROBE_TABLE="${WORK}/probe.table"
MKFS_CALLS="${WORK}/mkfs.calls"
MOUNT_CALLS="${WORK}/mount.calls"
CHOWN_CALLS="${WORK}/chown.calls"
export PROBE_TABLE MKFS_CALLS MOUNT_CALLS CHOWN_CALLS

add_disk() {
    mkdir -p "${SYS_BLOCK}/$1" "${DEV_DIR}"
    : > "${DEV_DIR}/$1"
    [ -n "${2:-}" ] && printf '%s\n' "$2" > "${SYS_BLOCK}/$1/serial"
    return 0
}

add_partition() { mkdir -p "${SYS_BLOCK}/$1/$1$2"; }

write_handoff() {
    mkdir -p "$(dirname "${HANDOFF}")"
    cat > "${HANDOFF}" <<EOF
RDS_MODE='initialize'
RDS_MASTER_USERNAME='master'
RDS_MASTER_PASSWORD='still-needed-by-rds-init'
RDS_DATA_VOLUME_ID='${1:-vol-expected}'
RDS_DATA_VOLUME_SERIAL='${2:-volexpected}'
RDS_VM_GENERATION='${3:-1}'
RDS_FORMAT_AUTHORIZED='${4:-false}'
RDS_PORT='5432'
EOF
    chmod 0600 "${HANDOFF}"
}

reset_state() {
    rm -rf "${WORK}/sys" "${DEV_DIR}" "${DATA_MOUNT}" "$(dirname "${HANDOFF}")"
    mkdir -p "${SYS_BLOCK}" "${DEV_DIR}" "${DATA_MOUNT}"
    printf '/dev/vda1 / ext4 rw,relatime 0 0\n' > "${MOUNTS}"
    : > "${PROBE_TABLE}"
    : > "${MKFS_CALLS}"
    : > "${MOUNT_CALLS}"
    : > "${CHOWN_CALLS}"
    unset MKFS_FAIL MOUNT_RC || true
    write_handoff vol-expected volexpected 1 false
}

run() {
    env RDS_DATA_MOUNT="${DATA_MOUNT}" \
        RDS_SYS_BLOCK="${SYS_BLOCK}" \
        RDS_DEV_DIR="${DEV_DIR}" \
        RDS_MOUNTS_FILE="${MOUNTS}" \
        RDS_HANDOFF_ENV="${HANDOFF}" \
        RDS_DATA_VOLUME_WAIT=1 \
        MKFS_FAIL="${MKFS_FAIL:-0}" \
        MOUNT_RC="${MOUNT_RC:-0}" \
        sh "${SCRIPT}" </dev/null
}

run_ok() {
    if run > "${WORK}/out" 2>&1; then
        pass "$1"
        return 0
    fi
    fail "$1: non-zero exit: $(cat "${WORK}/out")"
    return 1
}

run_fails() {
    if run > "${WORK}/out" 2>&1; then
        fail "$1: expected a non-zero exit"
        return 1
    fi
    pass "$1: refused"
}

nothing_formatted() {
    if grep -q . "${MKFS_CALLS}"; then fail "$1: unexpectedly ran mkfs"; else pass "$1: no mkfs"; fi
}

# A matching create grant formats only the exact disk and consumes only the
# authorization line. An unrelated generic volume is ignored.
reset_state
add_disk vda
add_disk vdb volunrelated
add_disk vdc volexpected
write_handoff vol-expected volexpected 1 true
if run_ok "authorized blank exact volume"; then
    grep -q "mkfs.ext4 .*${DEV_DIR}/vdc" "${MKFS_CALLS}" && pass "authorized: exact disk formatted" || fail "authorized: exact disk not formatted"
    grep -q -- '-m 0' "${MKFS_CALLS}" && pass "authorized: no reserved blocks" || fail "authorized: reserved blocks left on"
    grep -q "mount -t ext4 ${DEV_DIR}/vdc ${DATA_MOUNT}" "${MOUNT_CALLS}" && pass "authorized: mounted" || fail "authorized: not mounted"
    grep -q '^RDS_MASTER_PASSWORD=' "${HANDOFF}" && pass "authorized: bootstrap material preserved" || fail "authorized: bootstrap material removed"
    if grep -q '^RDS_FORMAT_AUTHORIZED=' "${HANDOFF}"; then fail "authorized: grant retained"; else pass "authorized: grant consumed"; fi
fi

# A lone vol* disk with the wrong serial never becomes a fallback candidate.
reset_state
add_disk vda
add_disk vdb volwrong
write_handoff vol-expected volexpected 1 true
run_fails "serial mismatch"
nothing_formatted "serial mismatch"

# Duplicate exact serials are ambiguous even if both name generic data disks.
reset_state
add_disk vdb volexpected
add_disk vdc volexpected
write_handoff vol-expected volexpected 1 true
run_fails "duplicate expected serial"
nothing_formatted "duplicate expected serial"

# Existing supported filesystems mount without any grant.
for fs in ext4 xfs; do
    reset_state
    add_disk vdb volexpected
    printf '%s fs %s\n' "${DEV_DIR}/vdb" "${fs}" > "${PROBE_TABLE}"
    if run_ok "existing ${fs}"; then
        nothing_formatted "existing ${fs}"
        grep -q "mount -t ${fs} ${DEV_DIR}/vdb ${DATA_MOUNT}" "${MOUNT_CALLS}" && pass "existing ${fs}: mounted by type" || fail "existing ${fs}: not mounted"
    fi
done

# Blank restore/start/replacement volumes have no grant and fail closed.
reset_state
add_disk vdb volexpected
run_fails "blank without grant"
nothing_formatted "blank without grant"

# Partition tables, visible partitions, unsupported filesystems, ambiguity, and
# operational probe errors all fail without formatting.
for kind in pttable fspt unsupported ambiguous unreadable error; do
    reset_state
    add_disk vdb volexpected
    case "${kind}" in
        pttable) printf '%s pttable gpt\n' "${DEV_DIR}/vdb" > "${PROBE_TABLE}" ;;
        fspt) printf '%s fspt ext4\n' "${DEV_DIR}/vdb" > "${PROBE_TABLE}" ;;
        unsupported) printf '%s fs btrfs\n' "${DEV_DIR}/vdb" > "${PROBE_TABLE}" ;;
        ambiguous) printf '%s ambiguous x\n' "${DEV_DIR}/vdb" > "${PROBE_TABLE}" ;;
        unreadable) printf '%s unreadable x\n' "${DEV_DIR}/vdb" > "${PROBE_TABLE}" ;;
        error) printf '%s error x\n' "${DEV_DIR}/vdb" > "${PROBE_TABLE}" ;;
    esac
    write_handoff vol-expected volexpected 1 true
    run_fails "probe ${kind}"
    nothing_formatted "probe ${kind}"
done

reset_state
add_disk vdb volexpected
add_partition vdb 1
printf '%s fs ext4\n' "${DEV_DIR}/vdb" > "${PROBE_TABLE}"
write_handoff vol-expected volexpected 1 true
run_fails "sysfs partition plus filesystem"
nothing_formatted "sysfs partition plus filesystem"

# Missing identity and invalid generation cannot authorize even when true.
for field in id serial generation; do
    reset_state
    add_disk vdb volexpected
    write_handoff vol-expected volexpected 1 true
    case "${field}" in
        id) sed -i "s/RDS_DATA_VOLUME_ID='vol-expected'/RDS_DATA_VOLUME_ID=''/" "${HANDOFF}" ;;
        serial) sed -i "s/RDS_DATA_VOLUME_SERIAL='volexpected'/RDS_DATA_VOLUME_SERIAL=''/" "${HANDOFF}" ;;
        generation) sed -i "s/RDS_VM_GENERATION='1'/RDS_VM_GENERATION='0'/" "${HANDOFF}" ;;
    esac
    run_fails "invalid ${field}"
    nothing_formatted "invalid ${field}"
done

# A failed format retains the one boot's grant for a legitimate retry.
reset_state
add_disk vdb volexpected
write_handoff vol-expected volexpected 1 true
MKFS_FAIL=1 run_fails "mkfs failure"
grep -q '^RDS_FORMAT_AUTHORIZED=' "${HANDOFF}" && pass "mkfs failure: grant retained" || fail "mkfs failure: grant lost"
[ ! -s "${MOUNT_CALLS}" ] && pass "mkfs failure: not mounted" || fail "mkfs failure: mounted raw disk"

# If mkfs succeeded but the guest died before consuming the line, observing the
# supported filesystem consumes it without formatting again.
reset_state
add_disk vdb volexpected
printf '%s fs ext4\n' "${DEV_DIR}/vdb" > "${PROBE_TABLE}"
write_handoff vol-expected volexpected 1 true
if run_ok "lingering grant on filesystem"; then
    nothing_formatted "lingering grant"
    if grep -q '^RDS_FORMAT_AUTHORIZED=' "${HANDOFF}"; then fail "lingering grant: retained"; else pass "lingering grant: consumed"; fi
fi

# A service restart verifies the mounted source is the expected exact device and
# removes any lingering authorization.
reset_state
add_disk vdb volexpected
printf '%s %s ext4 rw 0 0\n' "${DEV_DIR}/vdb" "${DATA_MOUNT}" >> "${MOUNTS}"
write_handoff vol-expected volexpected 1 true
if run_ok "already mounted exact device"; then
    [ ! -s "${MOUNT_CALLS}" ] && pass "already mounted: no duplicate mount" || fail "already mounted: mounted twice"
    if grep -q '^RDS_FORMAT_AUTHORIZED=' "${HANDOFF}"; then fail "already mounted: grant retained"; else pass "already mounted: grant consumed"; fi
fi

reset_state
add_disk vdb volexpected
printf '%s %s ext4 rw 0 0\n' "${DEV_DIR}/wrong" "${DATA_MOUNT}" >> "${MOUNTS}"
run_fails "already mounted wrong device"
nothing_formatted "already mounted wrong device"

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all rds-datadir cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
