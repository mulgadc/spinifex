#!/bin/sh
# Test for the etcd peer-prune step (3.5) of k3s-first-boot.sh: kubectl and the helpers
# are stubbed on PATH (jq required) and state paths rewritten into a temp dir.
# Run: sh scripts/images/eks-node/k3s-first-boot_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SRC="${SCRIPT_DIR}/k3s-first-boot.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); echo "ok   - $1"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL - $1"; }

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

# kubectl stub: readiness and JWKS succeed; "get nodes" returns the fixture;
# "delete node" is recorded so the test can see which Node the script removed.
cat > "${STUBBIN}/kubectl" <<'EOF'
#!/bin/sh
case "$*" in
    *"--raw=/readyz"*)         echo ok ;;
    *"--raw=/openid/v1/jwks"*) echo '{"keys":[]}' ;;
    *"get nodes"*)             printf '%s' "${NODES_BODY:-}" ;;
    *"delete node"*)           echo "$*" >> "${DELETE_LOG}" ;;
    *) : ;;
esac
EOF

# eks-gateway-publish stub: swallow the envelope.
cat > "${STUBBIN}/eks-gateway-publish" <<'EOF'
#!/bin/sh
cat > /dev/null
EOF

# eks-etcd-member stub: record argv; exit status is configurable so the
# best-effort contract (a helper failure never blocks first boot) can be tested.
cat > "${STUBBIN}/eks-etcd-member" <<'EOF'
#!/bin/sh
echo "$*" >> "${MEMBER_LOG}"
exit "${MEMBER_RC:-0}"
EOF

chmod +x "${STUBBIN}"/*
PATH="${STUBBIN}:${PATH}"
export PATH

command -v jq >/dev/null 2>&1 || { echo "SKIP - jq not installed"; exit 0; }

# Rewrite the script's absolute state paths into the sandbox.
mkdir -p "${WORK}/state" "${WORK}/etc" "${WORK}/k3s/server" "${WORK}/rancher"
sed -e "s|/var/lib/spinifex-eks|${WORK}/state|g" \
    -e "s|/etc/spinifex-eks|${WORK}/etc|g" \
    -e "s|/var/lib/rancher/k3s/server|${WORK}/k3s/server|g" \
    -e "s|/etc/rancher/k3s|${WORK}/rancher|g" \
    "${SRC}" > "${WORK}/first-boot.sh"

cat > "${WORK}/rancher/k3s.yaml" <<'EOF'
apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: Q0FEQVRB
    server: https://127.0.0.1:6443
EOF
echo node-token > "${WORK}/k3s/server/node-token"

# run_case <prune-ip-or-empty>: fresh sentinel + env, then run the script copy.
run_case() {
    prune_ip=$1
    : > "${WORK}/state/first-boot.pending"
    {
        echo "EKS_GATEWAY_URL=https://gw.invalid:9999"
        echo "EKS_ACCOUNT_ID=000000000001"
        echo "EKS_CLUSTER_NAME=demo"
        echo "EKS_NLB_ENDPOINT=https://demo.invalid"
        [ -z "${prune_ip}" ] || echo "EKS_ETCD_PRUNE_PEER_IP=${prune_ip}"
    } > "${WORK}/etc/first-boot.env"
    : > "${WORK}/delete.log"
    : > "${WORK}/member.log"
    DELETE_LOG="${WORK}/delete.log" MEMBER_LOG="${WORK}/member.log" \
        EKS_NODE_INIT=none sh "${WORK}/first-boot.sh" > "${WORK}/out.log" 2>&1
}

# --- Case 1: dead peer has no Node (all-CPs-without-agent steady state) -> the
# --- explicit member-remove helper is called with the peer IP.
NODES_BODY="" run_case 10.0.0.12 && rc=0 || rc=$?
[ "${rc}" -eq 0 ] && pass "no-node: first boot exits 0" || fail "no-node: exit ${rc}"
[ ! -s "${WORK}/delete.log" ] && pass "no-node: no Node delete attempted" || fail "no-node: unexpected delete: $(cat "${WORK}/delete.log")"
grep -q -- 'remove --peer-ip 10.0.0.12' "${WORK}/member.log" \
    && pass "no-node: eks-etcd-member remove --peer-ip 10.0.0.12 called" \
    || fail "no-node: member-remove helper not called with the peer IP (member.log: $(cat "${WORK}/member.log"))"

# --- Case 2: dead peer still has a Node (mixed fleet) -> the Node is deleted so
# --- k3s drops the member itself.
NODES_BODY="ip-10-0-0-12 10.0.0.12" run_case 10.0.0.12 && rc=0 || rc=$?
[ "${rc}" -eq 0 ] && pass "node-present: first boot exits 0" || fail "node-present: exit ${rc}"
grep -q 'delete node ip-10-0-0-12' "${WORK}/delete.log" \
    && pass "node-present: dead Node deleted" \
    || fail "node-present: dead Node not deleted (delete.log: $(cat "${WORK}/delete.log"))"

# --- Case 3: not a replacement (no prune IP) -> neither path runs.
NODES_BODY="" run_case "" && rc=0 || rc=$?
[ "${rc}" -eq 0 ] && pass "no-prune: first boot exits 0" || fail "no-prune: exit ${rc}"
{ [ ! -s "${WORK}/delete.log" ] && [ ! -s "${WORK}/member.log" ]; } \
    && pass "no-prune: neither delete nor member-remove ran" \
    || fail "no-prune: unexpected prune activity"

# --- Case 4: the helper fails -> best effort, first boot still completes (the
# --- sentinel is consumed) and the helper was actually attempted.
NODES_BODY="" MEMBER_RC=1 run_case 10.0.0.12 && rc=0 || rc=$?
[ "${rc}" -eq 0 ] && pass "helper-fails: first boot still exits 0" || fail "helper-fails: exit ${rc}"
[ ! -e "${WORK}/state/first-boot.pending" ] && pass "helper-fails: sentinel consumed" || fail "helper-fails: sentinel left behind"
[ -s "${WORK}/member.log" ] && pass "helper-fails: helper was attempted" || fail "helper-fails: helper never attempted"
grep -Eq 'prune.*fail.*10\.0\.0\.12' "${WORK}/out.log" \
    && pass "helper-fails: prune failure logged with the peer IP" \
    || fail "helper-fails: no prune-failure line naming 10.0.0.12 (out.log: $(cat "${WORK}/out.log"))"

echo
echo "passed=${PASS} failed=${FAIL}"
[ "${FAIL}" -eq 0 ]
