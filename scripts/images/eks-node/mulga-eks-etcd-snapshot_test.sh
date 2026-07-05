#!/bin/sh
# Self-contained POSIX test for mulga-eks-etcd-snapshot.sh cadence + upload. No
# k3s/predastore/root: k3s, curl and logger are stubbed on PATH; the role file,
# ENVFILE, SNAPSHOT_DIR and K3S_BIN are redirected into a temp dir via the
# script's env knobs. The k3s stub fabricates the snapshot file the script then
# uploads; the curl stub records the request line so we can assert the key.
#
# Run: sh scripts/images/eks-node/mulga-eks-etcd-snapshot_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${SCRIPT_DIR}/mulga-eks-etcd-snapshot.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

# k3s stub: on `etcd-snapshot save --name <NAME>` fabricate the snapshot file the
# real k3s would drop under SNAPSHOT_DIR, and record that it was invoked.
cat > "${STUBBIN}/k3s" <<'EOF'
#!/bin/sh
echo "k3s $*" >> "${K3S_CALLS}"
name=
while [ $# -gt 0 ]; do
    case "$1" in --name) name="$2"; shift 2 ;; *) shift ;; esac
done
[ -n "${name}" ] && { mkdir -p "${SNAPSHOT_DIR}"; echo snap > "${SNAPSHOT_DIR}/${name}.zip"; }
:
EOF

# curl stub: record the full argument line (carries --upload-file + URL).
cat > "${STUBBIN}/curl" <<'EOF'
#!/bin/sh
echo "curl $*" >> "${CURL_CALLS}"
EOF

# logger stub: swallow.
cat > "${STUBBIN}/logger" <<'EOF'
#!/bin/sh
:
EOF

chmod +x "${STUBBIN}"/*
PATH="${STUBBIN}:${PATH}"
export PATH

ENVFILE="${WORK}/etcd-snapshot.env"
cat > "${ENVFILE}" <<'EOF'
EKS_ACCOUNT_ID=000000000001
EKS_CLUSTER_NAME=demo3
SPINIFEX_PREDASTORE_ENDPOINT=https://pred.invalid:8443
SPINIFEX_PREDASTORE_AKID=AKIDTEST
SPINIFEX_PREDASTORE_SECRET=SECRETTEST
EOF

FAILS=0
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

# Symlinks whose parent dir names the cadence — invoking them gives the script
# the real $0 it sniffs in production, exercising the crond-dir tier selection.
mkdir -p "${WORK}/15min" "${WORK}/daily"
ln -sf "${SCRIPT}" "${WORK}/15min/mulga-eks-etcd-snapshot"
ln -sf "${SCRIPT}" "${WORK}/daily/mulga-eks-etcd-snapshot"

# run <role> <invocation-path>: resets the capture files + knobs and runs the
# script via the given symlink so $0 (the cadence source) is authentic.
run() {
    _role=$1; _path=$2
    K3S_CALLS="${WORK}/k3s.calls"; CURL_CALLS="${WORK}/curl.calls"
    SNAPSHOT_DIR="${WORK}/snapshots"; ROLE_FILE="${WORK}/role"; K3S_BIN=k3s
    : > "${K3S_CALLS}"; : > "${CURL_CALLS}"; rm -rf "${SNAPSHOT_DIR}"
    printf '%s' "${_role}" > "${ROLE_FILE}"
    export K3S_CALLS CURL_CALLS SNAPSHOT_DIR ROLE_FILE K3S_BIN ENVFILE
    unset TIER 2>/dev/null || true
    sh "${_path}" </dev/null || fail "${_path}: non-zero exit"
}

# --- Case 1: agent role -> no snapshot, no upload ---
run agent "${WORK}/15min/mulga-eks-etcd-snapshot"
[ -s "${WORK}/k3s.calls" ] && fail "agent: must not snapshot" || pass "agent: no k3s call"
[ -s "${WORK}/curl.calls" ] && fail "agent: must not upload" || pass "agent: no upload"

# --- Case 2: server, 15min dir -> frequent tier upload key ---
run server "${WORK}/15min/mulga-eks-etcd-snapshot"
grep -q 'etcd-snapshot save --name etcd-frequent-' "${WORK}/k3s.calls" \
    && pass "frequent: k3s snapshot named for tier" || fail "frequent: k3s not called for tier: $(cat "${WORK}/k3s.calls")"
grep -q '/eks-backups-system/000000000001/demo3/etcd-frequent-.*\.snap' "${WORK}/curl.calls" \
    && pass "frequent: upload key carries tier + identity" || fail "frequent: upload key wrong: $(cat "${WORK}/curl.calls")"
ls "${WORK}/snapshots"/etcd-frequent-* >/dev/null 2>&1 \
    && fail "frequent: local snapshot must be pruned after upload" || pass "frequent: local snapshot pruned"

# --- Case 3: server, daily dir -> daily tier upload key ---
run server "${WORK}/daily/mulga-eks-etcd-snapshot"
grep -q '/eks-backups-system/000000000001/demo3/etcd-daily-.*\.snap' "${WORK}/curl.calls" \
    && pass "daily: cadence sniffed from periodic dir" || fail "daily: sniff wrong: $(cat "${WORK}/curl.calls")"

# --- Case 4: missing ENVFILE -> quiet no-op, no upload ---
ENVFILE_SAVE="${ENVFILE}"; ENVFILE="${WORK}/missing.env"
run server "${WORK}/15min/mulga-eks-etcd-snapshot"
[ -s "${WORK}/curl.calls" ] && fail "no-env: must not upload" || pass "no-env: no upload without creds"
ENVFILE="${ENVFILE_SAVE}"

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all mulga-eks-etcd-snapshot cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
