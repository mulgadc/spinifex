#!/bin/sh
# Self-contained POSIX test for eks-node-role.sh role selection. No bats/root
# needed: rc-update / rc-service are stubbed on PATH and record their calls; the
# script's file paths are redirected into a temp dir via its EKS_NODE_* env knobs.
#
# Run: sh scripts/images/eks-node/eks-node-role_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SELECTOR="${SCRIPT_DIR}/eks-node-role.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

# Stub rc-update / rc-service: append "cmd args" to ${CALLS} when invoked.
STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"
for tool in rc-update rc-service; do
    cat > "${STUBBIN}/${tool}" <<EOF
#!/bin/sh
echo "${tool} \$*" >> "\${CALLS}"
exit 0
EOF
    chmod +x "${STUBBIN}/${tool}"
done

PASS=0
FAIL=0
check() { # check <desc> <expected> <actual>
    if [ "$2" = "$3" ]; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        echo "FAIL: $1"
        echo "  expected: [$2]"
        echo "  actual:   [$3]"
    fi
}

# run_case <name>: resets a per-case dir, exports the knobs; caller seeds env
# files then invokes the selector. Sets CASE (dir), CALLS (log), ROLE_FILE.
run_case() {
    CASE="${WORK}/$1"
    mkdir -p "${CASE}"
    CALLS="${CASE}/calls"
    : > "${CALLS}"
    ROLE_FILE="${CASE}/role"
    export CALLS
    export EKS_NODE_ROLE_FILE="${ROLE_FILE}"
    export EKS_NODE_ENVFILE="${CASE}/first-boot.env"
    export EKS_NODE_AGENT_ENVFILE="${CASE}/agent.env"
}

# Run the selector, capturing its exit code in ${rc} without tripping set -e.
invoke() { rc=0; PATH="${STUBBIN}:${PATH}" sh "${SELECTOR}" >/dev/null 2>&1 || rc=$?; }

# --- Case 1: explicit server role ---
run_case server
printf 'SPINIFEX_K3S_ROLE=server\nEKS_CLUSTER_NAME=alpha\n' > "${EKS_NODE_ENVFILE}"
invoke
check "server: exit 0" 0 "${rc}"
check "server: role file" "server" "$(cat "${ROLE_FILE}")"
check "server: enables webhook" "yes" "$(grep -q 'rc-update add eks-token-webhook default' "${CALLS}" && echo yes || echo no)"
check "server: enables k3s" "yes" "$(grep -q 'rc-update add k3s default' "${CALLS}" && echo yes || echo no)"
check "server: enables k3s-first-boot" "yes" "$(grep -q 'rc-update add k3s-first-boot default' "${CALLS}" && echo yes || echo no)"
check "server: starts k3s" "yes" "$(grep -q 'rc-service k3s start' "${CALLS}" && echo yes || echo no)"
check "server: enables konnectivity-server" "yes" "$(grep -q 'rc-update add konnectivity-server default' "${CALLS}" && echo yes || echo no)"
check "server: no k3s-agent" "no" "$(grep -q 'k3s-agent' "${CALLS}" && echo yes || echo no)"
check "server: self-disables" "yes" "$(grep -q 'rc-update del eks-node-role default' "${CALLS}" && echo yes || echo no)"

# --- Case 1b: explicit server-join role ---
run_case server_join
printf 'SPINIFEX_K3S_ROLE=server-join\nEKS_CLUSTER_NAME=alpha\n' > "${EKS_NODE_ENVFILE}"
invoke
check "server-join: exit 0" 0 "${rc}"
check "server-join: role file" "server-join" "$(cat "${ROLE_FILE}")"
check "server-join: enables webhook" "yes" "$(grep -q 'rc-update add eks-token-webhook default' "${CALLS}" && echo yes || echo no)"
check "server-join: enables k3s" "yes" "$(grep -q 'rc-update add k3s default' "${CALLS}" && echo yes || echo no)"
check "server-join: enables state-report" "yes" "$(grep -q 'rc-update add mulga-eks-state-report default' "${CALLS}" && echo yes || echo no)"
check "server-join: enables konnectivity-server" "yes" "$(grep -q 'rc-update add konnectivity-server default' "${CALLS}" && echo yes || echo no)"
# Join servers must NOT re-publish bootstrap; the first server already did.
check "server-join: no k3s-first-boot" "no" "$(grep -q 'k3s-first-boot' "${CALLS}" && echo yes || echo no)"
check "server-join: no k3s-agent" "no" "$(grep -q 'k3s-agent' "${CALLS}" && echo yes || echo no)"
check "server-join: self-disables" "yes" "$(grep -q 'rc-update del eks-node-role default' "${CALLS}" && echo yes || echo no)"

# --- Case 2: explicit agent role ---
run_case agent_explicit
printf 'SPINIFEX_K3S_ROLE=agent\n' > "${EKS_NODE_ENVFILE}"
invoke
check "agent: exit 0" 0 "${rc}"
check "agent: role file" "agent" "$(cat "${ROLE_FILE}")"
check "agent: enables k3s-agent" "yes" "$(grep -q 'rc-update add k3s-agent default' "${CALLS}" && echo yes || echo no)"
check "agent: no server services" "no" "$(grep -qE 'eks-token-webhook|k3s-first-boot|add k3s default' "${CALLS}" && echo yes || echo no)"

# --- Case 3: agent inferred from agent.env (no explicit role) ---
run_case agent_inferred
printf 'K3S_URL=https://nlb:443\nK3S_TOKEN=abc\n' > "${EKS_NODE_AGENT_ENVFILE}"
invoke
check "infer: exit 0" 0 "${rc}"
check "infer: role file" "agent" "$(cat "${ROLE_FILE}")"
check "infer: enables k3s-agent" "yes" "$(grep -q 'rc-update add k3s-agent default' "${CALLS}" && echo yes || echo no)"

# --- Case 4: missing role + no env files → fail, no role file, no services ---
run_case missing
invoke
check "missing: nonzero exit" "1" "${rc}"
check "missing: no role file" "absent" "$([ -f "${ROLE_FILE}" ] && echo present || echo absent)"
check "missing: no rc calls" "" "$(cat "${CALLS}")"

# --- Case 5: already resolved (role file present) → no-op, exit 0 ---
run_case resolved
printf 'SPINIFEX_K3S_ROLE=server\n' > "${EKS_NODE_ENVFILE}"
printf 'server\n' > "${ROLE_FILE}"
invoke
check "resolved: exit 0" 0 "${rc}"
check "resolved: no rc calls" "" "$(cat "${CALLS}")"

echo "---"
echo "PASS=${PASS} FAIL=${FAIL}"
[ "${FAIL}" -eq 0 ]
