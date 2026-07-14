#!/bin/sh
# Self-contained POSIX test for mulga-eks-k3s-recovery.sh: role guard, directive
# fetch, epoch-guarded one-shot, and the cluster-reset / wipe-rejoin / snapshot
# actions. No k3s/IMDS/gateway/root: eks-gateway-fetch, curl, k3s and logger are
# stubbed on PATH; every path the script touches is redirected into a temp dir
# via its env knobs.
#
# Run: sh scripts/images/eks-node/mulga-eks-k3s-recovery_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${SCRIPT_DIR}/mulga-eks-k3s-recovery.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

# eks-gateway-fetch stub: emit the directive line from DIRECTIVE_LINE (a real-tab
# epoch<TAB>action<TAB>snapshot string the harness sets per case).
cat > "${STUBBIN}/eks-gateway-fetch" <<'EOF'
#!/bin/sh
printf '%s\n' "${DIRECTIVE_LINE}"
EOF

# curl stub: answers the IMDS token PUT + instance-id GET, and materialises a
# snapshot file for any `-o <path>` download. Records every call. A snapshot
# download (an `-o` arg) fails with a non-zero exit when CURL_SNAP_FAIL=1, so the
# harness can exercise the fetch-failure path.
cat > "${STUBBIN}/curl" <<'EOF'
#!/bin/sh
echo "curl $*" >> "${CURL_CALLS}"
for a in "$@"; do
    case "$a" in
        */api/token) echo "TOKENVAL"; exit 0 ;;
        */meta-data/instance-id) echo "${STUB_IID:-i-abc123}"; exit 0 ;;
    esac
done
prev=""
for a in "$@"; do
    if [ "$prev" = "-o" ]; then
        [ "${CURL_SNAP_FAIL:-0}" = "1" ] && { echo "curl: (22) 404" >&2; exit 22; }
        echo snapdata > "$a"
    fi
    prev="$a"
done
exit 0
EOF

# k3s stub: record the invocation (asserts the cluster-reset command + flags).
cat > "${STUBBIN}/k3s" <<'EOF'
#!/bin/sh
echo "k3s $*" >> "${K3S_CALLS}"
exit 0
EOF

cat > "${STUBBIN}/logger" <<'EOF'
#!/bin/sh
:
EOF

chmod +x "${STUBBIN}"/*
PATH="${STUBBIN}:${PATH}"
export PATH

# first-boot.env: gateway creds + cluster identity the fetch + snapshot path need.
ENVFILE="${WORK}/first-boot.env"
cat > "${ENVFILE}" <<'EOF'
EKS_GATEWAY_URL=https://gw.invalid:9999
EKS_ACCOUNT_ID=000000000001
EKS_CLUSTER_NAME=demo3
EOF

# etcd-snapshot.env: predastore creds for the snapshot-restore path.
SNAPSHOT_ENVFILE="${WORK}/etcd-snapshot.env"
cat > "${SNAPSHOT_ENVFILE}" <<'EOF'
SPINIFEX_PREDASTORE_ENDPOINT=https://pred.invalid:8443
SPINIFEX_PREDASTORE_AKID=AKIDTEST
SPINIFEX_PREDASTORE_SECRET=SECRETTEST
EOF

FAILS=0
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

# run <role> <directive-line>: reset capture files + state, then run the script
# with every path knob pointed into the temp dir.
_setup_run() {
    _role=$1
    DIRECTIVE_LINE=$2
    K3S_CALLS="${WORK}/k3s.calls"
    CURL_CALLS="${WORK}/curl.calls"
    ROLE_FILE="${WORK}/role"
    EPOCH_FILE="${WORK}/recovery.epoch"
    ETCD_DIR="${WORK}/etcd"
    SNAPSHOT_DIR="${WORK}/snapshots"
    BLOCK_MARKER="${WORK}/run/k3s-restore-block"
    : > "${K3S_CALLS}"
    : > "${CURL_CALLS}"
    rm -f "${BLOCK_MARKER}"
    printf '%s' "${_role}" > "${ROLE_FILE}"
    export DIRECTIVE_LINE K3S_CALLS CURL_CALLS ROLE_FILE EPOCH_FILE ETCD_DIR SNAPSHOT_DIR BLOCK_MARKER
    export ENVFILE SNAPSHOT_ENVFILE
    export K3S_BIN=k3s FETCH_BIN=eks-gateway-fetch
}

# run <role> <directive-line>: expects a zero exit (the steady-state path).
run() {
    _setup_run "$1" "$2"
    sh "${SCRIPT}" </dev/null || fail "role=${1}: non-zero exit"
}

# run_abort <role> <directive-line>: expects a NON-zero exit (the required-snapshot
# fetch-failure abort). CURL_SNAP_FAIL must be set by the caller.
run_abort() {
    _setup_run "$1" "$2"
    if sh "${SCRIPT}" </dev/null; then
        fail "role=${1}: expected non-zero exit (required-snapshot abort)"
    fi
}

TAB=$(printf '\t')

# --- Case 1: agent role -> no fetch, no k3s ---
rm -f "${WORK}/recovery.epoch"; rm -rf "${WORK}/etcd"
run agent "5${TAB}cluster-reset${TAB}"
[ -s "${WORK}/k3s.calls" ] && fail "agent: must not reset" || pass "agent: no k3s call"
[ -f "${WORK}/recovery.epoch" ] && fail "agent: must not record epoch" || pass "agent: no epoch recorded"

# --- Case 2: action=none -> no-op, no epoch recorded ---
rm -f "${WORK}/recovery.epoch"
run server "0${TAB}none${TAB}"
[ -s "${WORK}/k3s.calls" ] && fail "none: must not reset" || pass "none: no k3s call"
[ -f "${WORK}/recovery.epoch" ] && fail "none: must not record epoch" || pass "none: no epoch recorded"

# --- Case 3: cluster-reset, fresh epoch -> k3s --cluster-reset + epoch recorded ---
rm -f "${WORK}/recovery.epoch"
run server "2${TAB}cluster-reset${TAB}"
grep -q 'server --config .* --cluster-reset' "${WORK}/k3s.calls" \
    && pass "cluster-reset: k3s server --cluster-reset invoked" \
    || fail "cluster-reset: k3s not reset: $(cat "${WORK}/k3s.calls")"
grep -q 'cluster-reset-restore-path' "${WORK}/k3s.calls" \
    && fail "cluster-reset: no snapshot -> must not pass restore-path" \
    || pass "cluster-reset: local-data reset (no restore-path)"
[ "$(cat "${WORK}/recovery.epoch" 2>/dev/null)" = 2 ] \
    && pass "cluster-reset: applied epoch recorded" \
    || fail "cluster-reset: epoch not recorded ($(cat "${WORK}/recovery.epoch" 2>/dev/null))"

# --- Case 4: epoch guard -> same epoch again is a no-op ---
# recovery.epoch is 2 from case 3; re-issue epoch 2, must not re-run.
run server "2${TAB}cluster-reset${TAB}"
[ -s "${WORK}/k3s.calls" ] && fail "guard: epoch<=applied must not re-run" || pass "guard: stale epoch skipped"

# --- Case 5: wipe-rejoin -> etcd dir removed, epoch recorded ---
rm -f "${WORK}/recovery.epoch"
mkdir -p "${WORK}/etcd/member"; echo x > "${WORK}/etcd/member/data"
run server "3${TAB}wipe-rejoin${TAB}"
[ -d "${WORK}/etcd" ] && fail "wipe-rejoin: etcd dir must be removed" || pass "wipe-rejoin: etcd datastore wiped"
[ "$(cat "${WORK}/recovery.epoch" 2>/dev/null)" = 3 ] \
    && pass "wipe-rejoin: applied epoch recorded" || fail "wipe-rejoin: epoch not recorded"

# --- Case 6: cluster-reset with snapshot -> fetch + restore-path ---
rm -f "${WORK}/recovery.epoch"
run server "4${TAB}cluster-reset${TAB}etcd-daily-20260706T000000Z.snap"
grep -q 'cluster-reset-restore-path=.*etcd-daily-20260706T000000Z.snap' "${WORK}/k3s.calls" \
    && pass "snapshot: restore-path passed to k3s" \
    || fail "snapshot: restore-path missing: $(cat "${WORK}/k3s.calls")"
grep -q 'aws-sigv4' "${WORK}/curl.calls" \
    && pass "snapshot: SigV4 download issued" || fail "snapshot: no download: $(cat "${WORK}/curl.calls")"
[ -f "${WORK}/snapshots/etcd-daily-20260706T000000Z.snap" ] \
    && pass "snapshot: fetched into snapshot dir" || fail "snapshot: file not materialised"

# --- Case 7: REQUIRED snapshot, fetch OK -> restore-path + no block marker ---
rm -f "${WORK}/recovery.epoch"
CURL_SNAP_FAIL=0; export CURL_SNAP_FAIL
run server "5${TAB}cluster-reset${TAB}etcd-frequent-20260709T010000Z.snap${TAB}1"
grep -q 'cluster-reset-restore-path=.*etcd-frequent-20260709T010000Z.snap' "${WORK}/k3s.calls" \
    && pass "required-ok: restore-path passed" || fail "required-ok: no restore-path: $(cat "${WORK}/k3s.calls")"
[ -f "${WORK}/run/k3s-restore-block" ] && fail "required-ok: must not block k3s on success" || pass "required-ok: no block marker"

# --- Case 8: REQUIRED snapshot, fetch FAILS -> abort: block marker, no k3s, no epoch ---
rm -f "${WORK}/recovery.epoch"
CURL_SNAP_FAIL=1; export CURL_SNAP_FAIL
run_abort server "6${TAB}cluster-reset${TAB}etcd-frequent-20260709T010000Z.snap${TAB}1"
[ -f "${WORK}/run/k3s-restore-block" ] \
    && pass "required-fail: block marker dropped to stop k3s" || fail "required-fail: no block marker"
[ -s "${WORK}/k3s.calls" ] && fail "required-fail: must NOT cluster-reset into empty datastore" || pass "required-fail: k3s not reset"
[ -f "${WORK}/recovery.epoch" ] && fail "required-fail: epoch must not record (retry next boot)" || pass "required-fail: epoch not recorded"

# --- Case 9: NON-required snapshot, fetch FAILS -> local reset, no block, epoch recorded ---
rm -f "${WORK}/recovery.epoch"
CURL_SNAP_FAIL=1; export CURL_SNAP_FAIL
run server "7${TAB}cluster-reset${TAB}etcd-frequent-20260709T010000Z.snap${TAB}0"
grep -q 'server --config .* --cluster-reset' "${WORK}/k3s.calls" \
    && pass "optional-fail: falls back to local cluster-reset" || fail "optional-fail: no reset: $(cat "${WORK}/k3s.calls")"
grep -q 'cluster-reset-restore-path' "${WORK}/k3s.calls" \
    && fail "optional-fail: must not pass a restore-path for a failed fetch" || pass "optional-fail: local-data reset only"
[ -f "${WORK}/run/k3s-restore-block" ] && fail "optional-fail: must not block k3s (HA-reform semantics)" || pass "optional-fail: no block marker"
[ "$(cat "${WORK}/recovery.epoch" 2>/dev/null)" = 7 ] \
    && pass "optional-fail: epoch recorded" || fail "optional-fail: epoch not recorded"
CURL_SNAP_FAIL=0; export CURL_SNAP_FAIL

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all mulga-eks-k3s-recovery cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
