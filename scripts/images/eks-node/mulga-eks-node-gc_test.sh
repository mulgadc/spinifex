#!/bin/sh
# Self-contained POSIX test for mulga-eks-node-gc.sh. kubectl, eks-gateway-fetch
# and logger are stubbed on PATH: kubectl serves canned Node rows and records
# every delete, and eks-gateway-fetch serves a canned worker set or fails.
#
# Run: sh scripts/images/eks-node/mulga-eks-node-gc_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
AGENT="${SCRIPT_DIR}/mulga-eks-node-gc.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

cat > "${STUBBIN}/kubectl" <<'EOF'
#!/bin/sh
case "$1 $2" in
    "get nodes")
        [ "${NODES_RC:-0}" -eq 0 ] || exit "${NODES_RC}"
        printf '%s\n' "${NODES_BODY:-}" ;;
    "delete node") echo "$3" >> "${DELETED}" ;;
esac
EOF

cat > "${STUBBIN}/eks-gateway-fetch" <<'EOF'
#!/bin/sh
echo "$*" >> "${FETCHED}"
[ "${FETCH_RC:-0}" -eq 0 ] || exit "${FETCH_RC}"
printf '%s' "${LIVE_BODY:-}"
EOF

cat > "${STUBBIN}/logger" <<'EOF'
#!/bin/sh
:
EOF

chmod +x "${STUBBIN}"/*
PATH="${STUBBIN}:${PATH}"
export PATH

ENVFILE="${WORK}/first-boot.env"
cat > "${ENVFILE}" <<'EOF'
EKS_GATEWAY_URL=https://gw.invalid:9999
EKS_ACCOUNT_ID=000000000001
EKS_CLUSTER_NAME=demo3
EOF
CONSOLE="${WORK}/console"
DELETED="${WORK}/deleted"
FETCHED="${WORK}/fetched"
export ENVFILE CONSOLE DELETED FETCHED

PASS=0
FAIL=0
check() {
    if [ "$2" = "$3" ]; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        printf 'FAIL: %s\n  want: %s\n  got:  %s\n' "$1" "$2" "$3"
    fi
}

run_agent() {
    : > "${DELETED}"
    : > "${FETCHED}"
    sh "${AGENT}"
}

deleted() { tr '\n' ' ' < "${DELETED}" | sed 's/ $//'; }

# Every way a Node can look: only a NotReady nodegroup worker whose instance the
# cluster's nodegroups no longer own is deleted.
NODES_BODY='live|aws:///z1/i-0aaa|ng1|True
gone|aws:///z1/i-0bbb|ng1|Unknown
gone-never-reported|aws:///z1/i-0ddd|ng2|
owned-notready|aws:///z1/i-0ccc|ng1|False
gone-but-ready|aws:///z1/i-0eee|ng1|True
self-managed|aws:///z1/i-0fff||Unknown
k3s-provider|k3s://k3s-node|ng1|Unknown'
LIVE_BODY='i-0aaa
i-0ccc
'
export NODES_BODY LIVE_BODY

run_agent
check "deletes only NotReady nodegroup Nodes whose instance is gone" "gone gone-never-reported" "$(deleted)"
check "fetches the workers resource" "-resource workers" "$(cat "${FETCHED}")"

# A cluster with no nodegroups left: every NotReady nodegroup Node goes.
LIVE_BODY='' run_agent
check "empty worker set deletes every NotReady nodegroup Node" "gone gone-never-reported owned-notready" "$(deleted)"

# A failed fetch is not an empty set: nothing is deleted.
FETCH_RC=1 run_agent
check "failed fetch deletes nothing" "" "$(deleted)"

# Without a Node listing the workers are not fetched, so a set fetched before a
# Node registered can never be compared against it.
NODES_RC=1 run_agent
check "failed node listing deletes nothing" "" "$(deleted)"
check "failed node listing does not fetch" "" "$(cat "${FETCHED}")"

echo "---"
echo "PASS=${PASS} FAIL=${FAIL}"
[ "${FAIL}" -eq 0 ]
