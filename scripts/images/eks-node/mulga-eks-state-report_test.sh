#!/bin/sh
# Self-contained POSIX test for mulga-eks-state-report.sh diagnosis + payload. No
# bats/root/cluster: kubectl, df, logger and eks-gateway-publish are stubbed on
# PATH and fed canned fixtures; the script's ENVFILE and CONSOLE are redirected
# into a temp dir via its env knobs. The stubbed eks-gateway-publish captures the
# published payload so we can assert on it.
#
# Run: sh scripts/images/eks-node/mulga-eks-state-report_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
AGENT="${SCRIPT_DIR}/mulga-eks-state-report.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

# kubectl stub: branch on the requested raw path / verb. /healthz/etcd is matched
# before /healthz because the latter is a substring of the former.
cat > "${STUBBIN}/kubectl" <<'EOF'
#!/bin/sh
case "$*" in
    *"--raw=/healthz/etcd"*)   printf '%s\n' "${ETCD_BODY:-ok}" ;;
    *"--raw=/readyz?verbose"*) printf '%s\n' "${READYZ_BODY:-}" ;;
    *"--raw=/healthz"*)        printf '%s\n' "${HEALTHZ_BODY:-ok}" ;;
    *"get nodes"*" -L "*)      printf '%s\n' "${NODES_LABELED_BODY:-}" ;;
    *"get nodes"*)             printf '%s\n' "${NODES_BODY:-}" ;;
    *) : ;;
esac
EOF

# df stub: 2-line -Pk output with a configurable Available (column 4).
cat > "${STUBBIN}/df" <<'EOF'
#!/bin/sh
echo "Filesystem 1024-blocks Used Available Capacity Mounted"
echo "/dev/root 1000000 1000 ${DF_AVAIL_KB:-9000000} 1% /"
EOF

# eks-gateway-publish stub: record the payload (stdin) to ${PAYLOAD_OUT}.
cat > "${STUBBIN}/eks-gateway-publish" <<'EOF'
#!/bin/sh
cat > "${PAYLOAD_OUT}"
EOF

# logger stub: swallow.
cat > "${STUBBIN}/logger" <<'EOF'
#!/bin/sh
:
EOF

chmod +x "${STUBBIN}"/*
PATH="${STUBBIN}:${PATH}"
export PATH

# ENVFILE the agent sources for gateway creds + cluster identity.
ENVFILE="${WORK}/first-boot.env"
cat > "${ENVFILE}" <<'EOF'
EKS_GATEWAY_URL=https://gw.invalid:9999
EKS_ACCOUNT_ID=000000000001
EKS_CLUSTER_NAME=demo3
EOF

FAILS=0
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

# run_case <name>; caller pre-exports the fixture vars + PAYLOAD_OUT + CONSOLE.
run_agent() {
    PAYLOAD_OUT="${WORK}/payload.json"
    CONSOLE="${WORK}/console.out"
    : > "${CONSOLE}"
    export PAYLOAD_OUT CONSOLE ENVFILE ETCD_DB_DIR
    unset STATE_REPORT_INTERVAL
    sh "${AGENT}"
}

# --- Case 1: healthy apiserver, 3 Ready nodes -> no reason, quiet console ---
# NODES_LABELED_BODY carries the -L eks.amazonaws.com/nodegroup breakdown: a
# CP node ("<none>", excluded), 2 Ready ng-a workers, 1 Ready ng-b worker, and
# a NotReady ng-b worker that must not count — proving the per-nodegroup tally
# scopes to THAT nodegroup's own Ready nodes, not the cluster-wide total.
HEALTHZ_BODY=ok
NODES_BODY=$(printf 'ip-a Ready s\nip-b Ready s\nip-c Ready s\n')
NODES_LABELED_BODY=$(printf 'ip-cp Ready s <none>\nip-a Ready s ng-a\nip-b Ready s ng-a\nip-c Ready s ng-b\nip-d NotReady s ng-b\n')
ETCD_DB_DIR="${WORK}"
export HEALTHZ_BODY NODES_BODY NODES_LABELED_BODY
run_agent
P=$(cat "${WORK}/payload.json")
case "${P}" in *'"healthz":"ok"'*) pass "healthy: healthz ok" ;; *) fail "healthy: healthz not ok: ${P}" ;; esac
case "${P}" in *'"node_count":3'*) pass "healthy: node_count 3" ;; *) fail "healthy: node_count wrong: ${P}" ;; esac
case "${P}" in *'"reason"'*) fail "healthy: reason must be absent: ${P}" ;; *) pass "healthy: no reason field" ;; esac
case "${P}" in *'"nodegroup_ready":{'*) pass "healthy: nodegroup_ready object present" ;; *) fail "healthy: nodegroup_ready missing: ${P}" ;; esac
case "${P}" in *'"ng-a":2'*) pass "healthy: ng-a Ready count scoped to its own 2 workers" ;; *) fail "healthy: ng-a count wrong: ${P}" ;; esac
case "${P}" in *'"ng-b":1'*) pass "healthy: ng-b Ready count excludes its NotReady worker" ;; *) fail "healthy: ng-b count wrong: ${P}" ;; esac
[ -s "${WORK}/console.out" ] && fail "healthy: console must be quiet" || pass "healthy: console quiet"

# --- Case 2: unhealthy, etcd subcheck failing, etcd unreachable, disk ok ---
HEALTHZ_BODY=fail
READYZ_BODY=$(printf '[+]ping ok\n[+]log ok\n[-]etcd failed: reason withheld\n[-]poststarthook/start-service-ip-repair-controllers failed\n')
ETCD_BODY=error
DF_AVAIL_KB=9000000
export HEALTHZ_BODY READYZ_BODY ETCD_BODY DF_AVAIL_KB
run_agent
P=$(cat "${WORK}/payload.json")
case "${P}" in *'"healthz":"fail"'*) pass "unhealthy: healthz fail" ;; *) fail "unhealthy: healthz wrong: ${P}" ;; esac
case "${P}" in *'"nodegroup_ready":{}'*) pass "unhealthy: nodegroup_ready empty (no kubectl node query on a failing apiserver)" ;; *) fail "unhealthy: nodegroup_ready wrong: ${P}" ;; esac
case "${P}" in *'"reason":"readyz:[etcd poststarthook/start-service-ip-repair-controllers]; etcd:unreachable; disk:ok"'*) pass "unhealthy: reason names failing subchecks + etcd + disk" ;; *) fail "unhealthy: reason wrong: ${P}" ;; esac
case "${P}" in *'"reason":"'*'"'*'"'*) : ;; esac
C=$(cat "${WORK}/console.out")
case "${C}" in *'mulga-eks CP unhealthy'*) pass "unhealthy: console banner emitted" ;; *) fail "unhealthy: console banner missing: ${C}" ;; esac
case "${C}" in *'[-]etcd failed'*) pass "unhealthy: console shows failing readyz lines" ;; *) fail "unhealthy: console missing readyz detail: ${C}" ;; esac

# --- Case 3: unhealthy, etcd reachable but disk low -> disk:low ---
HEALTHZ_BODY=fail
READYZ_BODY=$(printf '[+]ping ok\n[-]etcd failed\n')
ETCD_BODY=ok
DF_AVAIL_KB=1024
export HEALTHZ_BODY READYZ_BODY ETCD_BODY DF_AVAIL_KB
run_agent
P=$(cat "${WORK}/payload.json")
case "${P}" in *'etcd:ok; disk:low:1024k'*) pass "disk-low: reason flags low etcd disk" ;; *) fail "disk-low: reason wrong: ${P}" ;; esac

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all mulga-eks-state-report cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
