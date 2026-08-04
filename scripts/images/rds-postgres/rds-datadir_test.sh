#!/bin/sh
# Self-contained POSIX test for rds-datadir: discovery of the hot-plugged data
# volume by virtio-blk serial, formatting a blank volume, mounting an existing
# one as found, and the refusals — no volume, two volumes, a partition table.
# No block devices and no root: a fake sysfs tree stands in for /sys/block, and
# blkid, mkfs.ext4, mount and chown are stubbed on PATH.
#
# Run: sh scripts/images/rds-postgres/rds-datadir_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${SCRIPT_DIR}/rds-datadir"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

# blkid stub: prints the requested filesystem type value for any device listed
# in FS_TABLE as "<dev> <fstype>", and exits non-zero when none is present.
cat > "${STUBBIN}/blkid" <<'EOF'
#!/bin/sh
# BLKID_RC simulates a probe that could not run at all — 127 is a blkid missing
# from the image, 4 an internal error. Both print nothing, which is precisely
# what a genuinely blank disk prints, so only the exit status separates them.
[ "${BLKID_RC:-0}" = "0" ] || exit "${BLKID_RC}"
[ "$1" = "-o" ] && [ "$2" = "value" ] && [ "$3" = "-s" ] && [ "$4" = "TYPE" ] || exit 4
dev="$5"
fstype=$(awk -v d="${dev}" '$1 == d { print $2 }' "${FS_TABLE}" 2>/dev/null)
[ -n "${fstype}" ] || exit 2
printf '%s\n' "${fstype}"
EOF

# mkfs.ext4 stub: records the call and marks the device formatted, so a mount
# in the same run sees the filesystem the format created.
cat > "${STUBBIN}/mkfs.ext4" <<'EOF'
#!/bin/sh
echo "mkfs.ext4 $*" >> "${MKFS_CALLS}"
[ "${MKFS_FAIL:-0}" = "1" ] && exit 1
dev=""
for a in "$@"; do dev="${a}"; done
echo "${dev} ext4" >> "${FS_TABLE}"
exit 0
EOF

# mount stub: records the call only. The script re-reads its mounts file rather
# than the mount table, so nothing else has to be simulated.
cat > "${STUBBIN}/mount" <<'EOF'
#!/bin/sh
echo "mount $*" >> "${MOUNT_CALLS}"
exit "${MOUNT_RC:-0}"
EOF

# chown stub: file ownership is a no-op outside the guest.
cat > "${STUBBIN}/chown" <<'EOF'
#!/bin/sh
echo "chown $*" >> "${CHOWN_CALLS}"
exit 0
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

FS_TABLE="${WORK}/fs.table"
MKFS_CALLS="${WORK}/mkfs.calls"
MOUNT_CALLS="${WORK}/mount.calls"
CHOWN_CALLS="${WORK}/chown.calls"
export FS_TABLE MKFS_CALLS MOUNT_CALLS CHOWN_CALLS

# add_disk <name> [serial]: materialise a block device in the fake sysfs. A disk
# with no serial stands in for the boot volume, which Spinifex attaches without
# one.
add_disk() {
    mkdir -p "${SYS_BLOCK}/$1" "${DEV_DIR}"
    : > "${DEV_DIR}/$1"
    [ -n "${2:-}" ] && printf '%s\n' "$2" > "${SYS_BLOCK}/$1/serial"
    return 0
}

# add_partition <disk> <n>: the sysfs child directory the kernel creates for a
# partition, which is how the script detects a partition table.
add_partition() { mkdir -p "${SYS_BLOCK}/$1/$1$2"; }

# reset_state clears the fake sysfs, the filesystem table and the stub call logs.
# The mounts file starts with the datadir unmounted, as it is at boot.
reset_state() {
    rm -rf "${WORK}/sys" "${DEV_DIR}" "${DATA_MOUNT}"
    mkdir -p "${SYS_BLOCK}" "${DEV_DIR}" "${DATA_MOUNT}"
    printf '/dev/vda1 / ext4 rw,relatime 0 0\n' > "${MOUNTS}"
    : > "${FS_TABLE}"
    : > "${MKFS_CALLS}"
    : > "${MOUNT_CALLS}"
    : > "${CHOWN_CALLS}"
    unset MKFS_FAIL MOUNT_RC BLKID_RC || true
}

# run: invoke rds-datadir against the fake sysfs and a wait short enough that
# the "never attached" case does not stall the suite.
run() {
    env RDS_DATA_MOUNT="${DATA_MOUNT}" \
        RDS_SYS_BLOCK="${SYS_BLOCK}" \
        RDS_DEV_DIR="${DEV_DIR}" \
        RDS_MOUNTS_FILE="${MOUNTS}" \
        RDS_DATA_VOLUME_WAIT="${RDS_DATA_VOLUME_WAIT:-2}" \
        MKFS_FAIL="${MKFS_FAIL:-0}" \
        MOUNT_RC="${MOUNT_RC:-0}" \
        BLKID_RC="${BLKID_RC:-0}" \
        sh "${SCRIPT}" </dev/null
}

run_ok() { run > "${WORK}/out" 2>&1 || { fail "$1: non-zero exit: $(cat "${WORK}/out")"; return 1; }; }
run_fails() { run > "${WORK}/out" 2>&1 && fail "$1: expected a non-zero exit" || pass "$1: refused"; }

# --- Case 1: a blank data volume is formatted and mounted ---
reset_state
add_disk vda
add_disk vdb vol0123456789abcdef
if run_ok "blank"; then
    grep -q "mkfs.ext4 .*${DEV_DIR}/vdb" "${MKFS_CALLS}" \
        && pass "blank: formatted the data volume" || fail "blank: no mkfs"
    grep -q -- '-m 0' "${MKFS_CALLS}" \
        && pass "blank: no reserved blocks" || fail "blank: reserved blocks left on"
    grep -q "mount ${DEV_DIR}/vdb ${DATA_MOUNT}" "${MOUNT_CALLS}" \
        && pass "blank: mounted at the datadir mount point" || fail "blank: not mounted"
    grep -q "postgres:postgres ${DATA_MOUNT}" "${CHOWN_CALLS}" \
        && pass "blank: mount point handed to postgres" || fail "blank: ownership not set"
fi

# --- Case 2: the boot volume is never a candidate ---
reset_state
add_disk vda
if run_fails "boot-only"; then :; fi
grep -q . "${MKFS_CALLS}" \
    && fail "boot-only: formatted a disk with no volume serial" || pass "boot-only: nothing formatted"

# --- Case 3: an existing volume is mounted as found, never re-made ---
reset_state
add_disk vda
add_disk vdb vol0123456789abcdef
echo "${DEV_DIR}/vdb ext4" > "${FS_TABLE}"
if run_ok "existing"; then
    grep -q . "${MKFS_CALLS}" \
        && fail "existing: reformatted a volume that already held a filesystem" \
        || pass "existing: not reformatted"
    grep -q "mount ${DEV_DIR}/vdb ${DATA_MOUNT}" "${MOUNT_CALLS}" \
        && pass "existing: mounted as found" || fail "existing: not mounted"
fi

# --- Case 4: a partition table is somebody else's disk ---
reset_state
add_disk vda
add_disk vdb vol0123456789abcdef
add_partition vdb 1
run_fails "partitioned"
grep -q . "${MKFS_CALLS}" \
    && fail "partitioned: formatted over a partition table" || pass "partitioned: nothing formatted"

# --- Case 5: two data volumes is an error, not a guess ---
reset_state
add_disk vda
add_disk vdb vol0123456789abcdef
add_disk vdc vol0fedcba987654321
run_fails "ambiguous"
grep -q . "${MKFS_CALLS}" \
    && fail "ambiguous: formatted one of two candidates" || pass "ambiguous: nothing formatted"

# --- Case 6: an already-mounted datadir is a no-op, so a restart is safe ---
reset_state
add_disk vda
add_disk vdb vol0123456789abcdef
printf '%s %s ext4 rw,relatime 0 0\n' "${DEV_DIR}/vdb" "${DATA_MOUNT}" >> "${MOUNTS}"
if run_ok "already-mounted"; then
    grep -q . "${MOUNT_CALLS}" \
        && fail "already-mounted: mounted over an existing mount" || pass "already-mounted: no-op"
fi

# --- Case 7: a failed format does not go on to mount a raw device ---
reset_state
add_disk vda
add_disk vdb vol0123456789abcdef
MKFS_FAIL=1 run_fails "mkfs-fail"
grep -q . "${MOUNT_CALLS}" \
    && fail "mkfs-fail: mounted after a failed format" || pass "mkfs-fail: not mounted"

# --- Case 8: a probe that could not run is never read as a blank disk ---
# The volume below holds a customer's filesystem. Deciding from blkid's empty
# output rather than its exit status reformats it.
for rc in 127 4; do
    reset_state
    add_disk vda
    add_disk vdb vol0123456789abcdef
    echo "${DEV_DIR}/vdb ext4" > "${FS_TABLE}"
    BLKID_RC="${rc}" run_fails "blkid-exit-${rc}"
    grep -q . "${MKFS_CALLS}" \
        && fail "blkid-exit-${rc}: formatted a volume it could not probe" \
        || pass "blkid-exit-${rc}: nothing formatted"
    grep -q . "${MOUNT_CALLS}" \
        && fail "blkid-exit-${rc}: mounted a volume it could not probe" \
        || pass "blkid-exit-${rc}: not mounted"
done

# --- Case 9: sysfs listing a disk before its device node exists is not blank ---
reset_state
add_disk vda
add_disk vdb vol0123456789abcdef
rm -f "${DEV_DIR}/vdb"
run_fails "no-device-node"
grep -q . "${MKFS_CALLS}" \
    && fail "no-device-node: formatted a device that does not exist yet" \
    || pass "no-device-node: nothing formatted"

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all rds-datadir cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
