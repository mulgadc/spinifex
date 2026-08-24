#!/bin/sh
# Self-contained POSIX test for bedrock-serve (mkosi.extra/usr/local/sbin/
# bedrock-serve): the env-file wait, engine dispatch (vllm vs tei), the
# ordinal-indexed weights-device resolution across a multi-member bundle (the
# three tiers: named device, by-id-sorted-by-target, ext4-scan), the bounded
# waits' timeouts, and the final read-only mount + exec. No root and no real
# block devices: blkid, mount and both engines' entrypoints are stubbed on
# PATH, and every path the script touches is redirected into a temp dir via
# its env knobs. Not shipped into the image -- it sits beside the profile,
# not under mkosi.extra/.
#
# Run: sh images/mkosi.profiles/vllm/bedrock-serve_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
# BEDROCK_TEST_SCRIPT: override to point at an alternate copy of the wrapper
# (e.g. a saved pre-fix version), so a regression case can be run against
# both and its before/after behaviour compared directly.
SCRIPT="${BEDROCK_TEST_SCRIPT:-${SCRIPT_DIR}/mkosi.extra/usr/local/sbin/bedrock-serve}"
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

# vllm stub: the venv binary the wrapper execs into for a vllm-engine member.
# Records its argv so the test can assert the mount point, port and
# served-model-name reached it.
cat > "${STUBBIN}/vllm" <<'EOF'
#!/bin/sh
echo "vllm $*" >> "${VLLM_CALLS}"
exit 0
EOF

# tei-router-stub: stands in for text-embeddings-router, which the wrapper
# now execs directly on BEDROCK_SERVE_PORT for a tei-engine member -- no
# front-door in between. Records its argv (model id/path, port, extra args).
cat > "${STUBBIN}/tei-router-stub" <<'EOF'
#!/bin/sh
echo "tei-router-stub $*" >> "${TEI_CALLS}"
exit 0
EOF

chmod +x "${STUBBIN}"/*
PATH="${STUBBIN}:${PATH}"
export PATH

FAILS=0
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

ENV_DIR="${WORK}/bedrock-bundle"
WEIGHTS_ROOT="${WORK}/weights-root"
SYS_BLOCK="${WORK}/sys/block"
DEV_DIR="${WORK}/dev"
BYID_DIR="${WORK}/by-id"
MOUNTS_FILE="${WORK}/mounts"
FS_TABLE="${WORK}/fs.table"
MOUNT_CALLS="${WORK}/mount.calls"
VLLM_CALLS="${WORK}/vllm.calls"
TEI_CALLS="${WORK}/tei.calls"
export FS_TABLE MOUNT_CALLS VLLM_CALLS TEI_CALLS MOUNTS_FILE

write_env() {
    # write_env <instance> <engine> <device> [extra-args]: lay down the
    # bootstrap handoff buildBundleUserData would have cloud-init write.
    mkdir -p "${ENV_DIR}"
    cat > "${ENV_DIR}/$1.env" <<EOF
BEDROCK_GROUP_ID=test-group
BEDROCK_ENGINE=$2
BEDROCK_MODEL_ID=test-model-$1
BEDROCK_ARGS=${4:-}
BEDROCK_WEIGHTS_DEVICE=$3
BEDROCK_SERVE_PORT=8000
EOF
}

# add_disk <name>: materialise a whole disk in the fake sysfs + /dev.
add_disk() {
    mkdir -p "${SYS_BLOCK}/$1" "${DEV_DIR}"
    : > "${DEV_DIR}/$1"
}

reset_state() {
    rm -rf "${WORK}/sys" "${DEV_DIR}" "${BYID_DIR}" "${WEIGHTS_ROOT}" "${ENV_DIR}"
    mkdir -p "${SYS_BLOCK}" "${DEV_DIR}" "${BYID_DIR}" "${ENV_DIR}"
    # Root is vda2, so root_disk() must resolve to "vda" and never treat it as
    # a weights candidate.
    printf '/dev/vda2 / ext4 rw,relatime 0 0\n' > "${MOUNTS_FILE}"
    : > "${FS_TABLE}"
    : > "${MOUNT_CALLS}"
    : > "${VLLM_CALLS}"
    : > "${TEI_CALLS}"
    add_disk vda
}

# run <instance>: invoke bedrock-serve for one member with every path knob
# pointed into the temp dir and short bounded waits, so the "nothing ever
# appears" case does not stall the suite.
run() {
    env BEDROCK_ENV_DIR="${ENV_DIR}" \
        BEDROCK_ENV_WAIT="${BEDROCK_ENV_WAIT:-2}" \
        BEDROCK_WEIGHTS_MOUNT="${WEIGHTS_ROOT}/$1" \
        BEDROCK_VLLM_VENV_BIN="${STUBBIN}" \
        BEDROCK_TEI_BIN="${STUBBIN}/tei-router-stub" \
        BEDROCK_DEVICE_WAIT="${BEDROCK_DEVICE_WAIT:-2}" \
        BEDROCK_SYS_BLOCK="${SYS_BLOCK}" \
        BEDROCK_DEV_DIR="${DEV_DIR}" \
        BEDROCK_BYID_DIR="${BYID_DIR}" \
        BEDROCK_MOUNTS_FILE="${MOUNTS_FILE}" \
        sh "${SCRIPT}" "$1" </dev/null
}

run_ok() { run "$1" > "${WORK}/out" 2>&1 || { fail "$1: non-zero exit: $(cat "${WORK}/out")"; return 1; }; }
run_fails() { run "$1" > "${WORK}/out" 2>&1 && fail "$1: expected a non-zero exit" || pass "$1: refused"; }

# --- Case 1: vllm engine, named device present ---
reset_state
add_disk sdf
write_env m1 vllm "${DEV_DIR}/sdf"
if run_ok "m1"; then
    grep -q "mount -o ro ${DEV_DIR}/sdf ${WEIGHTS_ROOT}/m1" "${MOUNT_CALLS}" \
        && pass "vllm-named-device: mounted read-only from the named device" || fail "vllm-named-device: not mounted from named device"
    grep -q "vllm serve ${WEIGHTS_ROOT}/m1 --port 8000 --served-model-name test-model-m1" "${VLLM_CALLS}" \
        && pass "vllm-named-device: vllm exec'd with mount point, port and model name" || fail "vllm-named-device: vllm not invoked correctly"
fi

# --- Case 2: tei engine, named device present, TEI exec'd directly on the
# serve port, not vllm and not any front-door ---
reset_state
add_disk sdf
write_env m1 tei "${DEV_DIR}/sdf"
if run_ok "m1"; then
    grep -q "mount -o ro ${DEV_DIR}/sdf ${WEIGHTS_ROOT}/m1" "${MOUNT_CALLS}" \
        && pass "tei-named-device: mounted read-only from the named device" || fail "tei-named-device: not mounted from named device"
    grep -q -- "--model-id ${WEIGHTS_ROOT}/m1 --port 8000" "${TEI_CALLS}" \
        && pass "tei-named-device: TEI exec'd directly with mount point and port" || fail "tei-named-device: TEI not invoked correctly"
    [ -s "${VLLM_CALLS}" ] && fail "tei-named-device: vllm was invoked for a tei member" || pass "tei-named-device: vllm never invoked"
fi

# --- Case 3: two-member bundle, by-id fallback disambiguated by ordinal ---
# sdf (ordinal 0) and sdg (ordinal 1) both absent under their launcher names;
# two by-id links exist, resolving to vdb and vdc respectively. Each
# member's own wrapper invocation must pick its OWN ordinal-indexed device,
# not just "the sole candidate" (which no longer holds once there is more
# than one member).
reset_state
add_disk vdb
add_disk vdc
: > "${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_volAAAAAAAAAAAAAAAAA"
: > "${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_volBBBBBBBBBBBBBBBBB"
ln -sf "../dev/vdb" "${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_volAAAAAAAAAAAAAAAAA"
ln -sf "../dev/vdc" "${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_volBBBBBBBBBBBBBBBBB"
write_env m1 vllm "/dev/sdf"
write_env m2 tei "/dev/sdg"
# resolve_weights_device mounts THROUGH the by-id link path itself (same as
# the single-model image always did), not a readlink-resolved target -- the
# link is only used, sorted by its target name, to pick which member gets
# which candidate.
if run_ok "m1"; then
    grep -q "mount -o ro ${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_volAAAAAAAAAAAAAAAAA ${WEIGHTS_ROOT}/m1" "${MOUNT_CALLS}" \
        && pass "byid-ordinal: member 0 (sdf) mounted from the first by-id link (-> vdb)" || fail "byid-ordinal: member 0 mounted the wrong device: $(cat "${MOUNT_CALLS}")"
fi
: > "${MOUNT_CALLS}"
if run_ok "m2"; then
    grep -q "mount -o ro ${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_volBBBBBBBBBBBBBBBBB ${WEIGHTS_ROOT}/m2" "${MOUNT_CALLS}" \
        && pass "byid-ordinal: member 1 (sdg) mounted from the second by-id link (-> vdc)" || fail "byid-ordinal: member 1 mounted the wrong device: $(cat "${MOUNT_CALLS}")"
fi

# --- Case 4: two-member bundle, ext4-scan fallback disambiguated by ordinal
# (by-id bridge has not minted anything yet) ---
reset_state
add_disk vdb
add_disk vdc
echo "${DEV_DIR}/vdb ext4" > "${FS_TABLE}"
echo "${DEV_DIR}/vdc ext4" >> "${FS_TABLE}"
write_env m1 vllm "/dev/sdf"
write_env m2 vllm "/dev/sdg"
if run_ok "m1"; then
    grep -q "mount -o ro ${DEV_DIR}/vdb ${WEIGHTS_ROOT}/m1" "${MOUNT_CALLS}" \
        && pass "ext4-ordinal: member 0 (sdf) mounted from the first ext4 candidate (vdb)" || fail "ext4-ordinal: member 0 mounted the wrong device"
fi
: > "${MOUNT_CALLS}"
if run_ok "m2"; then
    grep -q "mount -o ro ${DEV_DIR}/vdc ${WEIGHTS_ROOT}/m2" "${MOUNT_CALLS}" \
        && pass "ext4-ordinal: member 1 (sdg) mounted from the second ext4 candidate (vdc)" || fail "ext4-ordinal: member 1 mounted the wrong device"
fi

# --- Case 5: bundle-of-one degenerates to the old single-candidate behaviour
# via the by-id tier (ordinal 0 == "the sole candidate") ---
reset_state
add_disk vdb
: > "${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_vol0123456789abcdef"
ln -sf "../dev/vdb" "${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_vol0123456789abcdef"
write_env solo vllm "/dev/sdf"
if run_ok "solo"; then
    grep -q "mount -o ro ${BYID_DIR}/nvme-Amazon_Elastic_Block_Store_vol0123456789abcdef ${WEIGHTS_ROOT}/solo" "${MOUNT_CALLS}" \
        && pass "bundle-of-one: mounted from the sole by-id link, same as the single-model image" || fail "bundle-of-one: not mounted from the sole by-id link"
    grep -q "vllm serve ${WEIGHTS_ROOT}/solo --port 8000 --served-model-name test-model-solo" "${VLLM_CALLS}" \
        && pass "bundle-of-one: vllm exec'd on port 8000, unchanged from today's single-model serve" || fail "bundle-of-one: vllm invocation regressed"
fi

# --- Case 6: the root disk is never an ext4-scan candidate ---
reset_state
echo "${DEV_DIR}/vda ext4" > "${FS_TABLE}"
write_env m1 vllm "/dev/sdf"
run_fails "m1"
grep -q . "${MOUNT_CALLS}" \
    && fail "ext4-root-excluded: mounted the root disk as weights" || pass "ext4-root-excluded: root disk excluded"

# --- Case 7: nothing ever appears for this member ---
# Times the run to prove the wrapper actually spun in the retry loop for the
# full DEVICE_WAIT_SECS bound rather than dying on the first "not found yet".
reset_state
write_env m1 vllm "/dev/sdf"
_t0=$(date +%s)
BEDROCK_DEVICE_WAIT=3 run_fails "m1"
_t1=$(date +%s)
_elapsed=$((_t1 - _t0))
[ "${_elapsed}" -ge 3 ] \
    && pass "nothing-appears: waited the full ${_elapsed}s bound before giving up" \
    || fail "nothing-appears: returned after only ${_elapsed}s, expected >= 3s"

# --- Case 8: a malformed BEDROCK_WEIGHTS_DEVICE suffix is refused, not
# retried (weights_device_ordinal's tier-2 refusal) ---
reset_state
mkdir -p "${ENV_DIR}"
cat > "${ENV_DIR}/m1.env" <<'EOF'
BEDROCK_GROUP_ID=test-group
BEDROCK_ENGINE=vllm
BEDROCK_MODEL_ID=test-model-m1
BEDROCK_ARGS=
BEDROCK_WEIGHTS_DEVICE=/dev/sdzz
BEDROCK_SERVE_PORT=8000
EOF
_t0=$(date +%s)
BEDROCK_DEVICE_WAIT=30 run_fails "m1"
_t1=$(date +%s)
_elapsed=$((_t1 - _t0))
[ "${_elapsed}" -lt 15 ] \
    && pass "bad-device-suffix: failed fast rather than retrying an unresolvable suffix" \
    || fail "bad-device-suffix: took ${_elapsed}s, expected a fast refusal"

# --- Case 9: an unknown BEDROCK_ENGINE is refused, not silently defaulted ---
reset_state
add_disk sdf
write_env m1 not-a-real-engine "${DEV_DIR}/sdf"
run_fails "m1"
[ -s "${VLLM_CALLS}" ] && fail "bad-engine: vllm was invoked despite an unknown engine" || pass "bad-engine: nothing invoked"

# --- Case 10: already-mounted weights point is a no-op restart ---
reset_state
add_disk sdf
write_env m1 vllm "${DEV_DIR}/sdf"
printf '%s %s ext4 ro 0 0\n' "${DEV_DIR}/sdf" "${WEIGHTS_ROOT}/m1" >> "${MOUNTS_FILE}"
if run_ok "m1"; then
    grep -q . "${MOUNT_CALLS}" \
        && fail "already-mounted: mounted over an existing mount" || pass "already-mounted: no-op, straight to exec"
    grep -q "vllm serve" "${VLLM_CALLS}" \
        && pass "already-mounted: still execs vllm" || fail "already-mounted: vllm never invoked"
fi

# --- Case 11: BEDROCK_ARGS passes each flag through as its own argument ---
reset_state
add_disk sdf
write_env m1 vllm "${DEV_DIR}/sdf" "--dtype=bfloat16 --max-model-len=4096"
if run_ok "m1"; then
    grep -q -- "--dtype=bfloat16 --max-model-len=4096" "${VLLM_CALLS}" \
        && pass "extra-args: BEDROCK_ARGS flags reached vllm serve" || fail "extra-args: flags missing"
fi

# --- Case 11b: BEDROCK_ARGS reaches TEI the same way, each flag its own arg ---
reset_state
add_disk sdf
write_env m1 tei "${DEV_DIR}/sdf" "--pooling=cls --max-batch-tokens=8192"
if run_ok "m1"; then
    grep -q -- "--pooling=cls --max-batch-tokens=8192" "${TEI_CALLS}" \
        && pass "extra-args-tei: BEDROCK_ARGS flags reached TEI" || fail "extra-args-tei: flags missing"
fi

# --- Case 12: env file never appears for this member ---
reset_state
_t0=$(date +%s)
BEDROCK_ENV_WAIT=3 run_fails "m1"
_t1=$(date +%s)
_elapsed=$((_t1 - _t0))
[ "${_elapsed}" -ge 3 ] \
    && pass "env-never-appears: waited the full ${_elapsed}s bound before giving up" \
    || fail "env-never-appears: returned after only ${_elapsed}s, expected >= 3s"

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all bedrock-serve cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
