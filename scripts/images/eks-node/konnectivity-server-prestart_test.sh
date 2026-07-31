#!/bin/sh
# Self-contained POSIX test for konnectivity-server-prestart.sh: server-count
# derivation, the bounded CA/kubeconfig wait, the SANS guard, and the cert
# mint + KONN_SERVER_COUNT drop-in. No konnectivity/k3s/root: eks-konnectivity-
# cert is stubbed via the script's CERT_BIN knob; every other path is
# redirected into a temp dir via its env knobs.
#
# Run: sh scripts/images/eks-node/konnectivity-server-prestart_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${SCRIPT_DIR}/konnectivity-server-prestart.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

# eks-konnectivity-cert stub: record the invocation; fail when CERT_FAIL=1 so
# the harness can exercise the mint-failure path.
cat > "${STUBBIN}/eks-konnectivity-cert" <<'EOF'
#!/bin/sh
echo "cert $*" >> "${CERT_CALLS}"
[ "${CERT_FAIL:-0}" = "1" ] && exit 1
exit 0
EOF
chmod +x "${STUBBIN}/eks-konnectivity-cert"

FAILS=0
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

# reset_env <name>: point every knob at a fresh, per-case dir. CA/key/
# kubeconfig are pre-seeded readable and WAIT_SECS=0, so a case that isn't
# specifically testing the wait loop skips it fast.
reset_env() {
    CASE="${WORK}/$1"
    mkdir -p "${CASE}"
    KONN_DIR="${CASE}/konnectivity"
    KONN_RUNDIR="${CASE}/run-konnectivity"
    ENVFILE="${CASE}/first-boot.env"
    K3S_CA="${CASE}/server-ca.crt"
    K3S_CA_KEY="${CASE}/server-ca.key"
    KUBECONFIG_FILE="${CASE}/k3s.yaml"
    DROPIN="${CASE}/konnectivity-server-args.env"
    WAIT_SECS=0
    CERT_BIN="${STUBBIN}/eks-konnectivity-cert"
    CERT_CALLS="${CASE}/cert.calls"
    STDERR="${CASE}/stderr"
    : > "${CERT_CALLS}"
    export KONN_DIR KONN_RUNDIR ENVFILE K3S_CA K3S_CA_KEY KUBECONFIG_FILE \
        DROPIN WAIT_SECS CERT_BIN CERT_CALLS
    printf 'cert\n' > "${K3S_CA}"
    printf 'key\n' > "${K3S_CA_KEY}"
    printf 'kubeconfig\n' > "${KUBECONFIG_FILE}"
    printf 'EKS_KONNECTIVITY_SANS=nlb.invalid,10.0.0.1\n' > "${ENVFILE}"
}

# Run the helper, capturing its exit code (without tripping set -e) + stderr.
invoke() { rc=0; sh "${SCRIPT}" >"${CASE}/stdout" 2>"${STDERR}" || rc=$?; }

# --- Case 1: default server count (no EKS_KONNECTIVITY_SERVER_COUNT) -> 1 ---
reset_env default_count
invoke
[ "${rc}" -eq 0 ] && pass "default-count: exit 0" || fail "default-count: expected exit 0 (rc=${rc}): $(cat "${STDERR}")"
grep -q '^KONN_SERVER_COUNT=1$' "${DROPIN}" \
    && pass "default-count: KONN_SERVER_COUNT=1" || fail "default-count: dropin wrong: $(cat "${DROPIN}" 2>/dev/null)"

# --- Case 2: EKS_KONNECTIVITY_SERVER_COUNT honoured ---
reset_env server_count
printf 'EKS_KONNECTIVITY_SANS=nlb.invalid\nEKS_KONNECTIVITY_SERVER_COUNT=3\n' > "${ENVFILE}"
invoke
[ "${rc}" -eq 0 ] && pass "server-count: exit 0" || fail "server-count: expected exit 0 (rc=${rc}): $(cat "${STDERR}")"
grep -q '^KONN_SERVER_COUNT=3$' "${DROPIN}" \
    && pass "server-count: KONN_SERVER_COUNT=3 honoured" || fail "server-count: dropin wrong: $(cat "${DROPIN}" 2>/dev/null)"

# --- Case 3: SANS empty -> nonzero exit, no cert mint attempted ---
reset_env empty_sans
: > "${ENVFILE}"
invoke
[ "${rc}" -ne 0 ] && pass "empty-sans: nonzero exit" || fail "empty-sans: expected nonzero exit"
grep -q 'EKS_KONNECTIVITY_SANS empty' "${STDERR}" \
    && pass "empty-sans: error message emitted" || fail "empty-sans: message missing: $(cat "${STDERR}")"
[ -s "${CERT_CALLS}" ] && fail "empty-sans: cert mint must not be attempted" || pass "empty-sans: no cert mint attempted"

# --- Case 4: CA/key/kubeconfig missing -> bounded wait, nonzero exit ---
reset_env missing_ca
rm -f "${K3S_CA}"
invoke
[ "${rc}" -ne 0 ] && pass "missing-ca: nonzero exit" || fail "missing-ca: expected nonzero exit"
grep -q 'k3s server CA / admin kubeconfig not present after' "${STDERR}" \
    && pass "missing-ca: error message emitted" || fail "missing-ca: message missing: $(cat "${STDERR}")"

# --- Case 5: cert mint fails -> nonzero exit, no dropin written ---
reset_env cert_fail
export CERT_FAIL=1
invoke
[ "${rc}" -ne 0 ] && pass "cert-fail: nonzero exit" || fail "cert-fail: expected nonzero exit"
[ -f "${DROPIN}" ] && fail "cert-fail: dropin must not be written on mint failure" || pass "cert-fail: no dropin written"
unset CERT_FAIL

# --- Case 6: successful mint invokes the cert binary with the right SANS ---
reset_env cert_ok
invoke
grep -q -- '-sans nlb.invalid,10.0.0.1' "${CERT_CALLS}" \
    && pass "cert-ok: cert minted with SANS from first-boot.env" || fail "cert-ok: cert invocation wrong: $(cat "${CERT_CALLS}")"

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all konnectivity-server-prestart cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
