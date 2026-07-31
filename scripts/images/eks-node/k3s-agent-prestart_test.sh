#!/bin/sh
# Self-contained POSIX test for k3s-agent-prestart.sh: the K3S_URL/K3S_TOKEN
# guard and the IMDSv2 provider-id resolution. No IMDS/root: curl is stubbed
# on PATH; ENV_FILE and DROPIN are redirected into a temp dir via the
# script's env knobs.
#
# Run: sh scripts/images/eks-node/k3s-agent-prestart_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${SCRIPT_DIR}/k3s-agent-prestart.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

# curl stub: branches on the requested IMDS path. Fails outright (simulating
# an unreachable IMDS) when CURL_FAIL=1; otherwise answers the token PUT and
# the instance-id/az GETs from CURL_TOK/CURL_IID/CURL_AZ (empty by default,
# so a case that seeds only CURL_TOK exercises the "token ok, metadata empty"
# branch).
cat > "${STUBBIN}/curl" <<'EOF'
#!/bin/sh
[ "${CURL_FAIL:-0}" = "1" ] && exit 7
case "$*" in
    *"/api/token") printf '%s' "${CURL_TOK:-}" ;;
    *"/meta-data/instance-id") printf '%s' "${CURL_IID:-}" ;;
    *"/meta-data/placement/availability-zone") printf '%s' "${CURL_AZ:-}" ;;
esac
exit 0
EOF
chmod +x "${STUBBIN}/curl"

FAILS=0
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

# reset_env <name>: point ENV_FILE/DROPIN at a fresh, per-case dir with valid
# K3S_URL/K3S_TOKEN seeded and IMDS stub state cleared; callers override only
# what their case needs.
reset_env() {
    CASE="${WORK}/$1"
    mkdir -p "${CASE}"
    ENV_FILE="${CASE}/agent.env"
    DROPIN="${CASE}/k3s-agent-extra-args.env"
    STDERR="${CASE}/stderr"
    CURL_FAIL=0
    unset CURL_TOK CURL_IID CURL_AZ
    export ENV_FILE DROPIN CURL_FAIL
    printf 'K3S_URL=https://nlb.invalid:443\nK3S_TOKEN=tok123\n' > "${ENV_FILE}"
}

# Run the helper with the stubbed curl on PATH, capturing its exit code
# (without tripping set -e) + stderr.
invoke() { rc=0; PATH="${STUBBIN}:${PATH}" sh "${SCRIPT}" >"${CASE}/stdout" 2>"${STDERR}" || rc=$?; }

# --- Case 1: K3S_URL/K3S_TOKEN missing -> nonzero exit, no dropin written ---
reset_env missing_creds
: > "${ENV_FILE}"
invoke
[ "${rc}" -ne 0 ] && pass "missing-creds: nonzero exit" || fail "missing-creds: expected nonzero exit"
grep -q 'K3S_URL and K3S_TOKEN must be set' "${STDERR}" \
    && pass "missing-creds: error message emitted" || fail "missing-creds: message missing: $(cat "${STDERR}")"
[ -f "${DROPIN}" ] && fail "missing-creds: dropin must not be written" || pass "missing-creds: no dropin written"

# --- Case 2: IMDS unreachable -> exit 0, empty K3S_AGENT_EXTRA_ARGS, warning ---
reset_env imds_unreachable
export CURL_FAIL=1
invoke
[ "${rc}" -eq 0 ] && pass "imds-unreachable: exit 0 (must not block agent start)" || fail "imds-unreachable: expected exit 0 (rc=${rc}): $(cat "${STDERR}")"
grep -q '^K3S_AGENT_EXTRA_ARGS=$' "${DROPIN}" \
    && pass "imds-unreachable: K3S_AGENT_EXTRA_ARGS empty" || fail "imds-unreachable: dropin wrong: $(cat "${DROPIN}" 2>/dev/null)"
grep -q 'IMDS token request failed' "${STDERR}" \
    && pass "imds-unreachable: warning emitted" || fail "imds-unreachable: warning missing: $(cat "${STDERR}")"
unset CURL_FAIL

# --- Case 3: token ok but instance-id/az empty -> exit 0, empty extra args, warns ---
reset_env imds_partial
export CURL_TOK=TOKENVAL
invoke
[ "${rc}" -eq 0 ] && pass "imds-partial: exit 0" || fail "imds-partial: expected exit 0 (rc=${rc})"
grep -q '^K3S_AGENT_EXTRA_ARGS=$' "${DROPIN}" \
    && pass "imds-partial: K3S_AGENT_EXTRA_ARGS empty" || fail "imds-partial: dropin wrong: $(cat "${DROPIN}" 2>/dev/null)"
grep -q 'IMDS instance-id/az empty' "${STDERR}" \
    && pass "imds-partial: warning emitted" || fail "imds-partial: warning missing: $(cat "${STDERR}")"
unset CURL_TOK

# --- Case 4: full IMDS success -> provider-id kubelet arg written ---
reset_env imds_ok
export CURL_TOK=TOKENVAL CURL_IID=i-abc123 CURL_AZ=au-mel-1a
invoke
[ "${rc}" -eq 0 ] && pass "imds-ok: exit 0" || fail "imds-ok: expected exit 0 (rc=${rc}): $(cat "${STDERR}")"
grep -q '^K3S_AGENT_EXTRA_ARGS=--kubelet-arg=provider-id=aws:///au-mel-1a/i-abc123$' "${DROPIN}" \
    && pass "imds-ok: provider-id kubelet arg written" || fail "imds-ok: dropin wrong: $(cat "${DROPIN}" 2>/dev/null)"
unset CURL_TOK CURL_IID CURL_AZ

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all k3s-agent-prestart cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
