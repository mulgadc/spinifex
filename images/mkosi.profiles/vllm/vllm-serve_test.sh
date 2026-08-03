#!/bin/sh
# Self-contained POSIX test for vllm-serve (mkosi.extra/usr/local/sbin/vllm-serve):
# the env-file wait, the three-tier weights-device resolution (named device,
# by-id fallback, ext4-scan fallback), the bounded waits' timeouts, and the
# final read-only mount + exec. No root and no real block devices: blkid,
# mount and the venv's vllm binary are stubbed on PATH, and every path the
# script touches is redirected into a temp dir via its env knobs. Not shipped
# into the image — it sits beside the profile, not under mkosi.extra/.
#
# Run: sh images/mkosi.profiles/vllm/vllm-serve_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
# VLLM_TEST_SCRIPT: override to point at an alternate copy of the wrapper
# (e.g. a saved pre-fix version), so a regression case can be run against
# both and its before/after behaviour compared directly.
SCRIPT="${VLLM_TEST_SCRIPT:-${SCRIPT_DIR}/mkosi.extra/usr/local/sbin/vllm-serve}"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

# blkid stub: prints a real blkid's line for any device listed in FS_TABLE as
# "<dev> <fstype>", nothing (rc=2) for one that holds none.
cat > "${STUBBIN}/blkid" <<'EOF'
#!/bin/sh
dev="$1"
fstype=$(awk -v d="${dev}" '$1 == d { print $2 }' "${FS_TABLE}" 2>/dev/null)
[ -n "${fstype}" ] || exit 2
echo "${dev}: UUID=\"1234-5678\" TYPE=\"${fstype}\""
EOF

# mount stub: records the call and, on success, appends the mount to
# MOUNTS_FILE so weights_mounted() sees the effect on a later invocation
# (idempotent-restart case), the way a real kernel's /proc/mounts would.
cat > "${STUBBIN}/mount" <<'EOF'
#!/bin/sh
echo "mount $*" >> "${MOUNT_CALLS}"
[ "${MOUNT_FAIL:-0}" = "1" ] && exit 1
dev=""; mnt=""
for a in "$@"; do
    case "$a" in -*) continue ;; esac
    if [ -z "$dev" ]; then dev="$a"; else mnt="$a"; fi
done
echo "${dev} ${mnt} ext4 ro 0 0" >> "${MOUNTS_FILE}"
exit 0
EOF

# vllm stub: the venv binary the wrapper execs into. Records its argv so the
# test can assert the mount point, port and served-model-name reached it.
cat > "${STUBBIN}/vllm" <<'EOF'
#!/bin/sh
echo "vllm $*" >> "${VLLM_CALLS}"
exit 0
EOF

chmod +x "${STUBBIN}"/*
PATH="${STUBBIN}:${PATH}"
export PATH

FAILS=0
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

ENV_FILE="${WORK}/vllm-serve.env"
WEIGHTS_MOUNT="${WORK}/weights"
SYS_BLOCK="${WORK}/sys/block"
DEV_DIR="${WORK}/dev"
BYID_DIR="${WORK}/by-id"
MOUNTS_FILE="${WORK}/mounts"
FS_TABLE="${WORK}/fs.table"
MOUNT_CALLS="${WORK}/mount.calls"
VLLM_CALLS="${WORK}/vllm.calls"
export FS_TABLE MOUNT_CALLS VLLM_CALLS MOUNTS_FILE

write_env() {
    # write_env <device> [extra-args]: lay down the bootstrap handoff
    # cloud-init would have written from buildServeUserData.
    cat > "${ENV_FILE}" <<EOF
VLLM_MODEL_ID=test-model
VLLM_ARGS=${2:-}
VLLM_WEIGHTS_DEVICE=$1
VLLM_SERVE_PORT=8000
EOF
}

# add_disk <name>: materialise a whole disk in the fake sysfs + /dev.
add_disk() {
    mkdir -p "${SYS_BLOCK}/$1" "${DEV_DIR}"
    : > "${DEV_DIR}/$1"
}

reset_state() {
    rm -rf "${WORK}/sys" "${DEV_DIR}" "${BYID_DIR}" "${WEIGHTS_MOUNT}" "${ENV_FILE}"
    mkdir -p "${SYS_BLOCK}" "${DEV_DIR}" "${BYID_DIR}"
    # Root is vda2, so root_disk() must resolve to "vda" and never treat it as
    # a weights candidate.
    printf '/dev/vda2 / ext4 rw,relatime 0 0\n' > "${MOUNTS_FILE}"
    : > "${FS_TABLE}"
    : > "${MOUNT_CALLS}"
    : > "${VLLM_CALLS}"
    add_disk vda
}

# run: invoke vllm-serve with every path knob pointed into the temp dir and
# short bounded waits, so the "nothing ever appears" case does not stall the
# suite.
run() {
    env VLLM_ENV_FILE="${ENV_FILE}" \
        VLLM_ENV_WAIT="${VLLM_ENV_WAIT:-2}" \
        VLLM_WEIGHTS_MOUNT="${WEIGHTS_MOUNT}" \
        VLLM_VENV_BIN="${STUBBIN}" \
        VLLM_DEVICE_WAIT="${VLLM_DEVICE_WAIT:-2}" \
        VLLM_SYS_BLOCK="${SYS_BLOCK}" \
        VLLM_DEV_DIR="${DEV_DIR}" \
        VLLM_BYID_DIR="${BYID_DIR}" \
        VLLM_MOUNTS_FILE="${MOUNTS_FILE}" \
        sh "${SCRIPT}" </dev/null
}

run_ok() { run > "${WORK}/out" 2>&1 || { fail "$1: non-zero exit: $(cat "${WORK}/out")"; return 1; }; }
run_fails() { run > "${WORK}/out" 2>&1 && fail "$1: expected a non-zero exit" || pass "$1: refused"; }

# --- Case 1: named device present ---
reset_state
add_disk sdf
write_env "${DEV_DIR}/sdf"
if run_ok "named-device"; then
    grep -q "mount -o ro ${DEV_DIR}/sdf ${WEIGHTS_MOUNT}" "${MOUNT_CALLS}" \
        && pass "named-device: mounted read-only from the named device" || fail "named-device: not mounted from named device"
    grep -q "vllm serve ${WEIGHTS_MOUNT} --port 8000 --served-model-name test-model" "${VLLM_CALLS}" \
        && pass "named-device: vllm exec'd with mount point, port and model name" || fail "named-device: vllm not invoked correctly"
fi

# --- Case 2: named device absent, mulga-ebs-byid's by-id link used instead ---
reset_state
add_disk vdb
: > "${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_vol0123456789abcdef"
write_env "/dev/sdf"
if run_ok "byid-fallback"; then
    grep -q "mount -o ro ${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_vol0123456789abcdef ${WEIGHTS_MOUNT}" "${MOUNT_CALLS}" \
        && pass "byid-fallback: mounted from the by-id link" || fail "byid-fallback: not mounted from by-id link"
fi

# --- Case 3: named device and by-id both absent, ext4-scan fallback used ---
reset_state
add_disk vdb
echo "${DEV_DIR}/vdb ext4" > "${FS_TABLE}"
write_env "/dev/sdf"
if run_ok "ext4-fallback"; then
    grep -q "mount -o ro ${DEV_DIR}/vdb ${WEIGHTS_MOUNT}" "${MOUNT_CALLS}" \
        && pass "ext4-fallback: mounted from the sole non-root ext4 disk" || fail "ext4-fallback: not mounted from ext4 disk"
fi

# --- Case 3b: the root disk is never an ext4-scan candidate ---
reset_state
echo "${DEV_DIR}/vda ext4" > "${FS_TABLE}"
write_env "/dev/sdf"
run_fails "ext4-fallback-root-excluded"
grep -q . "${MOUNT_CALLS}" \
    && fail "ext4-fallback-root-excluded: mounted the root disk as weights" || pass "ext4-fallback-root-excluded: root disk excluded"

# --- Case 4: nothing ever appears ---
# Times the run to prove the wrapper actually spun in the retry loop for the
# full DEVICE_WAIT_SECS bound rather than dying on the first "not found yet"
# — an exit-status-only check cannot tell a real timeout from an instant
# death (that was exactly how the set -e regression below slipped through).
reset_state
write_env "/dev/sdf"
_t0=$(date +%s)
VLLM_DEVICE_WAIT=3 run_fails "nothing-appears"
_t1=$(date +%s)
_elapsed=$((_t1 - _t0))
[ "${_elapsed}" -ge 3 ] \
    && pass "nothing-appears: waited the full ${_elapsed}s bound before giving up" \
    || fail "nothing-appears: returned after only ${_elapsed}s, expected >= 3s (instant death, not a real wait)"
grep -q . "${MOUNT_CALLS}" \
    && fail "nothing-appears: mounted something that should not exist" || pass "nothing-appears: refused, nothing mounted"

# --- Case 4b/c/d: device appears part-way through the bounded wait, for each
# of the three resolution tiers in turn. This is the case the exit-status-only
# "nothing-appears" case above cannot distinguish from an instant set -e
# death: only a run that keeps retrying past the first failed probe and then
# succeeds proves the retry loop itself works, not just that it eventually
# gives up.

# 4b: named device absent on the first probe, appears mid-wait.
reset_state
write_env "${DEV_DIR}/sdf"
( sleep 1; add_disk sdf ) &
BGPID=$!
if VLLM_DEVICE_WAIT=5 run_ok "named-device-appears-midwait"; then
    grep -q "mount -o ro ${DEV_DIR}/sdf ${WEIGHTS_MOUNT}" "${MOUNT_CALLS}" \
        && pass "named-device-appears-midwait: retried until the named device showed up, then mounted it" \
        || fail "named-device-appears-midwait: did not mount the named device once it appeared"
fi
wait "${BGPID}" 2>/dev/null || true

# 4c: by-id link absent on the first probe, appears mid-wait.
reset_state
add_disk vdb
write_env "/dev/sdf"
( sleep 1; : > "${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_vol0123456789abcdef" ) &
BGPID=$!
if VLLM_DEVICE_WAIT=5 run_ok "byid-appears-midwait"; then
    grep -q "mount -o ro ${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_vol0123456789abcdef ${WEIGHTS_MOUNT}" "${MOUNT_CALLS}" \
        && pass "byid-appears-midwait: retried until the by-id link showed up, then mounted it" \
        || fail "byid-appears-midwait: did not mount the by-id link once it appeared"
fi
wait "${BGPID}" 2>/dev/null || true

# 4d: ext4-scan candidate absent on the first probe (neither the disk nor its
# filesystem exists yet), appears mid-wait.
reset_state
write_env "/dev/sdf"
( sleep 1; add_disk vdb; echo "${DEV_DIR}/vdb ext4" >> "${FS_TABLE}" ) &
BGPID=$!
if VLLM_DEVICE_WAIT=5 run_ok "ext4-appears-midwait"; then
    grep -q "mount -o ro ${DEV_DIR}/vdb ${WEIGHTS_MOUNT}" "${MOUNT_CALLS}" \
        && pass "ext4-appears-midwait: retried until the ext4 disk showed up, then mounted it" \
        || fail "ext4-appears-midwait: did not mount the ext4 disk once it appeared"
fi
wait "${BGPID}" 2>/dev/null || true

# --- Case 5: two by-id links is ambiguous, refused rather than guessed ---
# Ambiguity is never worth retrying (more waiting cannot turn two candidates
# into one), so this must fail fast, well inside the wait bound — timed here
# to positively confirm the 2 (ambiguous) path is distinct from the 1 (not
# yet) retry path, not just that both eventually return non-zero.
reset_state
: > "${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_vol0000000000000001"
: > "${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_vol0000000000000002"
write_env "/dev/sdf"
_t0=$(date +%s)
VLLM_DEVICE_WAIT=30 run_fails "byid-ambiguous"
_t1=$(date +%s)
_elapsed=$((_t1 - _t0))
[ "${_elapsed}" -lt 15 ] \
    && pass "byid-ambiguous: failed fast in ${_elapsed}s rather than waiting out the 30s bound" \
    || fail "byid-ambiguous: took ${_elapsed}s, expected a fast refusal instead of retrying an unresolvable ambiguity"
grep -q . "${MOUNT_CALLS}" \
    && fail "byid-ambiguous: mounted one of two candidates" || pass "byid-ambiguous: nothing mounted"

# --- Case 6: two non-root ext4 disks is likewise ambiguous ---
reset_state
add_disk vdb
add_disk vdc
echo "${DEV_DIR}/vdb ext4" > "${FS_TABLE}"
echo "${DEV_DIR}/vdc ext4" >> "${FS_TABLE}"
write_env "/dev/sdf"
run_fails "ext4-ambiguous"
grep -q . "${MOUNT_CALLS}" \
    && fail "ext4-ambiguous: mounted one of two candidates" || pass "ext4-ambiguous: nothing mounted"

# --- Case 7: env file absent, then appears mid-wait ---
reset_state
add_disk sdf
VLLM_ENV_WAIT=5
( sleep 1; write_env "${DEV_DIR}/sdf" ) &
BGPID=$!
if VLLM_ENV_WAIT=5 run_ok "env-appears-midwait"; then
    pass "env-appears-midwait: wrapper picked up the handoff once cloud-init wrote it"
fi
wait "${BGPID}" 2>/dev/null || true
unset VLLM_ENV_WAIT

# --- Case 7b: the file exists but is empty/partial, then gets completed a
# moment later — deterministic reproduction (not dependent on catching a real
# `cat >file <<EOF` race window) of the bug where `[ -f "${ENV_FILE}" ]`
# alone cleared the wait's gate: cloud-init's write_files is not atomic from
# this reader's point of view, so a file that exists with none of its keys
# yet must be retried, not treated as a malformed handoff.
reset_state
add_disk sdf
: > "${ENV_FILE}"
( sleep 1; write_env "${DEV_DIR}/sdf" ) &
BGPID=$!
if VLLM_ENV_WAIT=5 run_ok "env-partial-write-midwait"; then
    pass "env-partial-write-midwait: retried past an empty handoff file until it was fully written"
fi
wait "${BGPID}" 2>/dev/null || true

# --- Case 8: env file never appears ---
# Timed for the same reason as the device-tier "nothing-appears" case: an
# exit-status-only check cannot tell a real ENV_WAIT_SECS wait from an
# instant death.
reset_state
_t0=$(date +%s)
VLLM_ENV_WAIT=3 run_fails "env-never-appears"
_t1=$(date +%s)
_elapsed=$((_t1 - _t0))
[ "${_elapsed}" -ge 3 ] \
    && pass "env-never-appears: waited the full ${_elapsed}s bound before giving up" \
    || fail "env-never-appears: returned after only ${_elapsed}s, expected >= 3s (instant death, not a real wait)"

# --- Case 9: already-mounted weights point is a no-op restart ---
reset_state
add_disk sdf
write_env "${DEV_DIR}/sdf"
printf '%s %s ext4 ro 0 0\n' "${DEV_DIR}/sdf" "${WEIGHTS_MOUNT}" >> "${MOUNTS_FILE}"
if run_ok "already-mounted"; then
    grep -q . "${MOUNT_CALLS}" \
        && fail "already-mounted: mounted over an existing mount" || pass "already-mounted: no-op, straight to exec"
    grep -q "vllm serve" "${VLLM_CALLS}" \
        && pass "already-mounted: still execs vllm" || fail "already-mounted: vllm never invoked"
fi

# --- Case 10: VLLM_ARGS passes each flag through as its own argument ---
reset_state
add_disk sdf
write_env "${DEV_DIR}/sdf" "--dtype=bfloat16 --max-model-len=4096"
if run_ok "extra-args"; then
    grep -q -- "--dtype=bfloat16 --max-model-len=4096" "${VLLM_CALLS}" \
        && pass "extra-args: VLLM_ARGS flags reached vllm serve" || fail "extra-args: flags missing"
fi

# --- Case 11: a non-numeric port is refused rather than passed through ---
# A malformed-but-present value must fail fast rather than retry — timed here
# against a generous ENV_WAIT to positively distinguish it from the "keys not
# there yet" retry path above (case 7b), the same distinction
# resolve_weights_device's ambiguous-tier cases make for the device wait.
reset_state
add_disk sdf
cat > "${ENV_FILE}" <<'EOF'
VLLM_MODEL_ID=test-model
VLLM_ARGS=
VLLM_WEIGHTS_DEVICE=/dev/sdf
VLLM_SERVE_PORT=not-a-number
EOF
_t0=$(date +%s)
VLLM_ENV_WAIT=30 run_fails "bad-port"
_t1=$(date +%s)
_elapsed=$((_t1 - _t0))
[ "${_elapsed}" -lt 15 ] \
    && pass "bad-port: failed fast in ${_elapsed}s rather than retrying an unfixable value" \
    || fail "bad-port: took ${_elapsed}s, expected a fast refusal instead of waiting out the 30s bound"
grep -q . "${VLLM_CALLS}" \
    && fail "bad-port: vllm invoked with a bad port" || pass "bad-port: refused before exec"

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all vllm-serve cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
