#!/bin/sh
# Self-contained POSIX test for eks-node-role.sh role selection. No bats/root
# needed: rc-update/rc-service (openrc) and systemctl (systemd) are stubbed on
# PATH and record their calls; the script's file paths are redirected into a
# temp dir via its EKS_NODE_* env knobs. Every role case runs once per init so
# a regression in either dispatch branch is caught.
#
# Run: sh scripts/images/eks-node/eks-node-role_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SELECTOR="${SCRIPT_DIR}/eks-node-role.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

# Stub rc-update/rc-service in one bin dir (openrc) and systemctl in another
# (systemd): append "cmd args" to ${CALLS} when invoked. Kept as separate
# dirs so each init's PATH only ever sees its own tools, same as a real host.
STUB_OPENRC="${WORK}/bin-openrc"
mkdir -p "${STUB_OPENRC}"
for tool in rc-update rc-service; do
    cat > "${STUB_OPENRC}/${tool}" <<EOF
#!/bin/sh
echo "${tool} \$*" >> "\${CALLS}"
exit 0
EOF
    chmod +x "${STUB_OPENRC}/${tool}"
done

STUB_SYSTEMD="${WORK}/bin-systemd"
mkdir -p "${STUB_SYSTEMD}"
cat > "${STUB_SYSTEMD}/systemctl" <<'EOF'
#!/bin/sh
echo "systemctl $*" >> "${CALLS}"
exit 0
EOF
chmod +x "${STUB_SYSTEMD}/systemctl"

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

# Build the exact command line the selector should emit for a given init +
# operation, so assertions read the same regardless of branch.
enable_cmd() { # <init> <svc>
    case "$1" in
        openrc) echo "rc-update add $2 default" ;;
        systemd) echo "systemctl enable $2.service" ;;
    esac
}
start_cmd() { # <init> <svc>
    case "$1" in
        openrc) echo "rc-service $2 start" ;;
        # --no-block is asserted, not incidental: without it the selector
        # deadlocks against the units it is ordered Before=.
        systemd) echo "systemctl start --no-block $2.service" ;;
    esac
}
disable_cmd() { # <init> <svc>
    case "$1" in
        openrc) echo "rc-update del $2 default" ;;
        systemd) echo "systemctl disable $2.service" ;;
    esac
}

# present <file> <literal>: "yes"/"no" whether <literal> occurs verbatim.
present() { grep -qF "$2" "$1" && echo yes || echo no; }
# absent_all <file> <literal...>: "yes" only if none of the literals occur.
absent_all() {
    f="$1"
    shift
    for s in "$@"; do
        if grep -qF "$s" "${f}"; then
            echo no
            return
        fi
    done
    echo yes
}

# run_case <init> <name>: resets a per-case dir, exports the path knobs, and
# selects the stub bin + EKS_NODE_INIT for the requested branch. The openrc
# branch deliberately leaves EKS_NODE_INIT unset and relies on autodetection
# via the stubbed rc-update on PATH — that's what proves detection still
# prefers rc-update, not just that the override works. The systemd branch has
# no real systemctl on most test hosts, so it must force the override.
run_case() {
    INIT="$1"
    CASE="${WORK}/${1}_$2"
    mkdir -p "${CASE}"
    CALLS="${CASE}/calls"
    : > "${CALLS}"
    ROLE_FILE="${CASE}/role"
    export CALLS
    export EKS_NODE_ROLE_FILE="${ROLE_FILE}"
    export EKS_NODE_ENVFILE="${CASE}/first-boot.env"
    export EKS_NODE_AGENT_ENVFILE="${CASE}/agent.env"
    if [ "${INIT}" = "openrc" ]; then
        STUBBIN="${STUB_OPENRC}"
        unset EKS_NODE_INIT || true
    else
        STUBBIN="${STUB_SYSTEMD}"
        export EKS_NODE_INIT=systemd
    fi
}

# Run the selector, capturing its exit code in ${rc} without tripping set -e.
invoke() { rc=0; PATH="${STUBBIN}:${PATH}" sh "${SELECTOR}" >/dev/null 2>&1 || rc=$?; }

for INIT in openrc systemd; do

    # --- server ---
    run_case "${INIT}" server
    printf 'SPINIFEX_K3S_ROLE=server\nEKS_CLUSTER_NAME=alpha\n' > "${EKS_NODE_ENVFILE}"
    invoke
    check "server(${INIT}): exit 0" 0 "${rc}"
    check "server(${INIT}): role file" "server" "$(cat "${ROLE_FILE}")"
    check "server(${INIT}): enables webhook" "yes" "$(present "${CALLS}" "$(enable_cmd "${INIT}" eks-token-webhook)")"
    check "server(${INIT}): enables k3s" "yes" "$(present "${CALLS}" "$(enable_cmd "${INIT}" k3s)")"
    check "server(${INIT}): enables k3s-first-boot" "yes" "$(present "${CALLS}" "$(enable_cmd "${INIT}" k3s-first-boot)")"
    check "server(${INIT}): starts k3s" "yes" "$(present "${CALLS}" "$(start_cmd "${INIT}" k3s)")"
    check "server(${INIT}): enables konnectivity-server" "yes" "$(present "${CALLS}" "$(enable_cmd "${INIT}" konnectivity-server)")"
    check "server(${INIT}): enables k3s-recovery" "yes" "$(present "${CALLS}" "$(enable_cmd "${INIT}" mulga-eks-k3s-recovery)")"
    # Recovery is enabled (so it also runs pre-k3s on later boots) AND started
    # inline now — a restore-snapshot DR seed's directive must apply on this
    # very first boot, before k3s starts.
    check "server(${INIT}): starts k3s-recovery" "yes" "$(present "${CALLS}" "$(start_cmd "${INIT}" mulga-eks-k3s-recovery)")"
    check "server(${INIT}): starts k3s-recovery before k3s" "yes" "$(
        awk -v r="$(start_cmd "${INIT}" mulga-eks-k3s-recovery)" -v k="$(start_cmd "${INIT}" k3s)" '
            index($0, r) == 1 { rn = NR }
            index($0, k) == 1 { kn = NR }
            END { print (rn > 0 && kn > 0 && rn < kn) ? "yes" : "no" }
        ' "${CALLS}"
    )"
    check "server(${INIT}): no k3s-agent" "no" "$(present "${CALLS}" "$(enable_cmd "${INIT}" k3s-agent)")"
    check "server(${INIT}): self-disables" "yes" "$(present "${CALLS}" "$(disable_cmd "${INIT}" eks-node-role)")"

    # --- server-join ---
    run_case "${INIT}" server_join
    printf 'SPINIFEX_K3S_ROLE=server-join\nEKS_CLUSTER_NAME=alpha\n' > "${EKS_NODE_ENVFILE}"
    invoke
    check "server-join(${INIT}): exit 0" 0 "${rc}"
    check "server-join(${INIT}): role file" "server-join" "$(cat "${ROLE_FILE}")"
    check "server-join(${INIT}): enables webhook" "yes" "$(present "${CALLS}" "$(enable_cmd "${INIT}" eks-token-webhook)")"
    check "server-join(${INIT}): enables k3s" "yes" "$(present "${CALLS}" "$(enable_cmd "${INIT}" k3s)")"
    check "server-join(${INIT}): enables state-report" "yes" "$(present "${CALLS}" "$(enable_cmd "${INIT}" mulga-eks-state-report)")"
    check "server-join(${INIT}): enables konnectivity-server" "yes" "$(present "${CALLS}" "$(enable_cmd "${INIT}" konnectivity-server)")"
    check "server-join(${INIT}): enables k3s-recovery" "yes" "$(present "${CALLS}" "$(enable_cmd "${INIT}" mulga-eks-k3s-recovery)")"
    check "server-join(${INIT}): starts k3s-recovery" "yes" "$(present "${CALLS}" "$(start_cmd "${INIT}" mulga-eks-k3s-recovery)")"
    # Join servers must NOT re-publish bootstrap: the first server already
    # published the cluster-identical artifacts, and a join re-publish would
    # only race the bootstrap bus. This is deliberate, not an oversight.
    check "server-join(${INIT}): no k3s-first-boot" "yes" "$(absent_all "${CALLS}" "k3s-first-boot")"
    check "server-join(${INIT}): no k3s-agent" "yes" "$(absent_all "${CALLS}" "$(enable_cmd "${INIT}" k3s-agent)")"
    check "server-join(${INIT}): self-disables" "yes" "$(present "${CALLS}" "$(disable_cmd "${INIT}" eks-node-role)")"

    # --- agent (explicit role) ---
    run_case "${INIT}" agent_explicit
    printf 'SPINIFEX_K3S_ROLE=agent\n' > "${EKS_NODE_ENVFILE}"
    invoke
    check "agent(${INIT}): exit 0" 0 "${rc}"
    check "agent(${INIT}): role file" "agent" "$(cat "${ROLE_FILE}")"
    check "agent(${INIT}): enables k3s-agent" "yes" "$(present "${CALLS}" "$(enable_cmd "${INIT}" k3s-agent)")"
    check "agent(${INIT}): no server services" "yes" "$(absent_all "${CALLS}" "eks-token-webhook" "k3s-first-boot" "$(enable_cmd "${INIT}" k3s)")"

    # --- agent (inferred from agent.env, no explicit role) ---
    run_case "${INIT}" agent_inferred
    printf 'K3S_URL=https://nlb:443\nK3S_TOKEN=abc\n' > "${EKS_NODE_AGENT_ENVFILE}"
    invoke
    check "infer(${INIT}): exit 0" 0 "${rc}"
    check "infer(${INIT}): role file" "agent" "$(cat "${ROLE_FILE}")"
    check "infer(${INIT}): enables k3s-agent" "yes" "$(present "${CALLS}" "$(enable_cmd "${INIT}" k3s-agent)")"

done

# --- autodetection prefers rc-update over systemctl when both are present ---
# The test suite has always stubbed rc-update/rc-service on PATH; if detection
# ever flipped to prefer systemctl, those openrc-branch cases above would
# start silently exercising the wrong dispatch path without this failing.
run_case openrc autodetect_prefers_openrc
BOTHBIN="${WORK}/bin-both"
mkdir -p "${BOTHBIN}"
cp "${STUB_OPENRC}/rc-update" "${STUB_OPENRC}/rc-service" "${BOTHBIN}/"
cp "${STUB_SYSTEMD}/systemctl" "${BOTHBIN}/"
STUBBIN="${BOTHBIN}"
printf 'SPINIFEX_K3S_ROLE=agent\n' > "${EKS_NODE_ENVFILE}"
invoke
check "autodetect: exit 0" 0 "${rc}"
check "autodetect: uses rc-update" "yes" "$(present "${CALLS}" "rc-update add k3s-agent default")"
check "autodetect: does not use systemctl" "yes" "$(absent_all "${CALLS}" "systemctl")"

# --- missing role + no env files → fail, no role file, no service calls ---
run_case openrc missing
invoke
check "missing: nonzero exit" "1" "${rc}"
check "missing: no role file" "absent" "$([ -f "${ROLE_FILE}" ] && echo present || echo absent)"
check "missing: no rc calls" "" "$(cat "${CALLS}")"

# --- already resolved (role file present) → no-op, exit 0 ---
run_case openrc resolved
printf 'SPINIFEX_K3S_ROLE=server\n' > "${EKS_NODE_ENVFILE}"
printf 'server\n' > "${ROLE_FILE}"
invoke
check "resolved: exit 0" 0 "${rc}"
check "resolved: no rc calls" "" "$(cat "${CALLS}")"

echo "---"
echo "PASS=${PASS} FAIL=${FAIL}"
[ "${FAIL}" -eq 0 ]
