#!/bin/sh
# Self-contained POSIX test for k3s-prestart.sh: the restore-block guard,
# config.yaml derivation from its skeleton, konnectivity-agent + CoreDNS
# manifest staging, and the token-webhook kubeconfig wait. No k3s/root: every
# path the script touches is redirected into a temp dir via its env knobs.
#
# Run: sh scripts/images/eks-node/k3s-prestart_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${SCRIPT_DIR}/k3s-prestart.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

FAILS=0
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

# reset_env <name>: point every knob at a fresh, per-case dir; K3S_CONFIG and
# TOKEN_WEBHOOK_KUBECONFIG are pre-seeded present and WAIT_SECS=0, so a case
# that isn't specifically testing config derivation or the webhook wait skips
# both fast. Callers override only what their case needs.
reset_env() {
    CASE="${WORK}/$1"
    mkdir -p "${CASE}"
    BLOCK_MARKER="${CASE}/k3s-restore-block"
    K3S_CONFIG="${CASE}/config.yaml"
    K3S_CONFIG_SKEL="${CASE}/config.yaml.skel"
    FIRST_BOOT_ENVFILE="${CASE}/first-boot.env"
    KONN_AGENT_MANIFEST_SRC="${CASE}/konnectivity-agent.yaml.src"
    KONN_AGENT_MANIFEST_DST="${CASE}/manifests/konnectivity-agent.yaml"
    COREDNS_SRC="${CASE}/coredns-mulga.yaml.src"
    COREDNS_SKIP="${CASE}/manifests/coredns.yaml.skip"
    COREDNS_DST="${CASE}/manifests/coredns-mulga.yaml"
    TOKEN_WEBHOOK_KUBECONFIG="${CASE}/token-webhook.kubeconfig"
    WAIT_SECS=0
    STDERR="${CASE}/stderr"
    export BLOCK_MARKER K3S_CONFIG K3S_CONFIG_SKEL FIRST_BOOT_ENVFILE \
        KONN_AGENT_MANIFEST_SRC KONN_AGENT_MANIFEST_DST \
        COREDNS_SRC COREDNS_SKIP COREDNS_DST TOKEN_WEBHOOK_KUBECONFIG WAIT_SECS
    : > "${K3S_CONFIG}"
    printf 'server-ca\n' > "${TOKEN_WEBHOOK_KUBECONFIG}"
}

# Run the helper, capturing its exit code (without tripping set -e) + stderr.
invoke() { rc=0; sh "${SCRIPT}" >"${CASE}/stdout" 2>"${STDERR}" || rc=$?; }

# --- Case 1: restore-block marker present -> refuse to start, nonzero exit ---
reset_env restore_block
: > "${BLOCK_MARKER}"
invoke
[ "${rc}" -ne 0 ] && pass "restore-block: nonzero exit" || fail "restore-block: expected nonzero exit"
grep -q 'refusing to start k3s on an empty datastore' "${STDERR}" \
    && pass "restore-block: error message emitted" || fail "restore-block: message missing: $(cat "${STDERR}")"

# --- Case 2: config.yaml missing, .skel present -> derived, exit 0 ---
reset_env derive_config
rm -f "${K3S_CONFIG}"
printf 'cluster-init: true\n' > "${K3S_CONFIG_SKEL}"
invoke
[ "${rc}" -eq 0 ] && pass "derive: exit 0" || fail "derive: expected exit 0 (rc=${rc}): $(cat "${STDERR}")"
[ "$(cat "${K3S_CONFIG}" 2>/dev/null)" = "cluster-init: true" ] \
    && pass "derive: config.yaml copied from .skel" || fail "derive: config.yaml not derived"

# --- Case 3: config.yaml missing, no .skel -> error, nonzero exit ---
reset_env no_skel
rm -f "${K3S_CONFIG}" "${K3S_CONFIG_SKEL}"
invoke
[ "${rc}" -ne 0 ] && pass "no-skel: nonzero exit" || fail "no-skel: expected nonzero exit"
grep -q 'no .*config.yaml and no .*to derive from' "${STDERR}" \
    && pass "no-skel: error message emitted" || fail "no-skel: message missing: $(cat "${STDERR}")"

# --- Case 4: konnectivity-agent staging skipped when host empty -> warning, no dst ---
reset_env konn_empty_host
cat > "${KONN_AGENT_MANIFEST_SRC}" <<'EOF'
image: __KONN_AGENT_IMAGE__
host: __KONN_HOST__
EOF
: > "${FIRST_BOOT_ENVFILE}"
invoke
[ "${rc}" -eq 0 ] && pass "konn-empty-host: exit 0 (staging is best-effort)" || fail "konn-empty-host: expected exit 0 (rc=${rc})"
[ -f "${KONN_AGENT_MANIFEST_DST}" ] && fail "konn-empty-host: manifest must not be staged" || pass "konn-empty-host: manifest not staged"
grep -q 'EKS_KONNECTIVITY_HOST empty' "${STDERR}" \
    && pass "konn-empty-host: warning emitted" || fail "konn-empty-host: warning missing: $(cat "${STDERR}")"

# --- Case 4b: first-boot.env absent entirely -> warning, no dst, still exit 0 ---
# Distinct from case 4: the staging guard tests the manifest, not the env file,
# so awk is handed a missing path and exits nonzero. That must degrade to the
# same warning rather than abort the run and leave the control plane unstarted.
reset_env konn_no_envfile
cat > "${KONN_AGENT_MANIFEST_SRC}" <<'EOF'
image: __KONN_AGENT_IMAGE__
host: __KONN_HOST__
EOF
rm -f "${FIRST_BOOT_ENVFILE}"
invoke
[ "${rc}" -eq 0 ] && pass "konn-no-envfile: exit 0 (must not block k3s)" || fail "konn-no-envfile: expected exit 0 (rc=${rc}): $(cat "${STDERR}")"
[ -f "${KONN_AGENT_MANIFEST_DST}" ] && fail "konn-no-envfile: manifest must not be staged" || pass "konn-no-envfile: manifest not staged"
grep -q 'EKS_KONNECTIVITY_HOST empty' "${STDERR}" \
    && pass "konn-no-envfile: warning emitted" || fail "konn-no-envfile: warning missing: $(cat "${STDERR}")"

# --- Case 5: konnectivity-agent staged when host present -> tokens substituted ---
reset_env konn_staged
cat > "${KONN_AGENT_MANIFEST_SRC}" <<'EOF'
image: __KONN_AGENT_IMAGE__
host: __KONN_HOST__
EOF
printf 'EKS_KONNECTIVITY_HOST=nlb.invalid:443\n' > "${FIRST_BOOT_ENVFILE}"
invoke
[ "${rc}" -eq 0 ] && pass "konn-staged: exit 0" || fail "konn-staged: expected exit 0 (rc=${rc}): $(cat "${STDERR}")"
grep -q 'host: nlb.invalid:443' "${KONN_AGENT_MANIFEST_DST}" 2>/dev/null \
    && pass "konn-staged: host substituted" || fail "konn-staged: host not substituted"
grep -q 'image: registry.k8s.io/kas-network-proxy/proxy-agent:v0.30.3' "${KONN_AGENT_MANIFEST_DST}" 2>/dev/null \
    && pass "konn-staged: pinned agent image substituted" || fail "konn-staged: image not substituted"

# --- Case 6: CoreDNS staged when bundle present -> skip marker + dst copied ---
reset_env coredns
printf 'kind: DaemonSet\n' > "${COREDNS_SRC}"
invoke
[ "${rc}" -eq 0 ] && pass "coredns: exit 0" || fail "coredns: expected exit 0 (rc=${rc})"
[ -f "${COREDNS_SKIP}" ] && pass "coredns: bundled CoreDNS suppressed" || fail "coredns: skip marker missing"
[ "$(cat "${COREDNS_DST}" 2>/dev/null)" = "kind: DaemonSet" ] \
    && pass "coredns: DaemonSet staged" || fail "coredns: DaemonSet not staged"

# --- Case 7: token-webhook kubeconfig absent -> bounded wait, warns, exit 0 ---
reset_env webhook_wait
rm -f "${TOKEN_WEBHOOK_KUBECONFIG}"
invoke
[ "${rc}" -eq 0 ] && pass "webhook-wait: exit 0 (must not block k3s)" || fail "webhook-wait: expected exit 0 (rc=${rc})"
grep -q 'eks-token-webhook kubeconfig absent after' "${STDERR}" \
    && pass "webhook-wait: warning emitted" || fail "webhook-wait: warning missing: $(cat "${STDERR}")"

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all k3s-prestart cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
