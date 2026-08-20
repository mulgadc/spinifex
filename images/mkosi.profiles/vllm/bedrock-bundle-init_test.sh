#!/bin/sh
# Self-contained POSIX test for bedrock-bundle-init (mkosi.extra/usr/local/
# sbin/bedrock-bundle-init): the bounded wait for the first member env file,
# enumerating every *.env file present, and starting each one's
# bedrock-serve@<instance>.service. systemctl is stubbed on PATH so this runs
# with no real systemd. Not shipped into the image -- it sits beside the
# profile, not under mkosi.extra/.
#
# Run: sh images/mkosi.profiles/vllm/bedrock-bundle-init_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${BEDROCK_TEST_SCRIPT:-${SCRIPT_DIR}/mkosi.extra/usr/local/sbin/bedrock-bundle-init}"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

cat > "${STUBBIN}/systemctl" <<'EOF'
#!/bin/sh
echo "systemctl $*" >> "${SYSTEMCTL_CALLS}"
exit 0
EOF
chmod +x "${STUBBIN}"/*
PATH="${STUBBIN}:${PATH}"
export PATH

FAILS=0
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

ENV_DIR="${WORK}/bedrock-bundle"
SYSTEMCTL_CALLS="${WORK}/systemctl.calls"
export SYSTEMCTL_CALLS

reset_state() {
    rm -rf "${ENV_DIR}"
    mkdir -p "${ENV_DIR}"
    : > "${SYSTEMCTL_CALLS}"
}

run() {
    env BEDROCK_ENV_DIR="${ENV_DIR}" \
        BEDROCK_BUNDLE_ENV_WAIT="${BEDROCK_BUNDLE_ENV_WAIT:-2}" \
        sh "${SCRIPT}" </dev/null
}

run_ok() { run > "${WORK}/out" 2>&1 || { fail "$1: non-zero exit: $(cat "${WORK}/out")"; return 1; }; }
run_fails() { run > "${WORK}/out" 2>&1 && fail "$1: expected a non-zero exit" || pass "$1: refused"; }

# --- Case 1: a single member (bundle-of-one) starts exactly one instance ---
reset_state
: > "${ENV_DIR}/solo.env"
if run_ok "bundle-of-one"; then
    grep -q "systemctl start --no-block bedrock-serve@solo.service" "${SYSTEMCTL_CALLS}" \
        && pass "bundle-of-one: started the sole member's instance" || fail "bundle-of-one: instance not started"
    [ "$(grep -c '^systemctl start' "${SYSTEMCTL_CALLS}")" -eq 1 ] \
        && pass "bundle-of-one: started exactly one instance" || fail "bundle-of-one: started more than one instance"
fi

# --- Case 2: a real bundle starts one instance per member, sanitized names
# preserved verbatim from the filename (bedrock-serve's own INSTANCE arg
# matching userdata.go's memberEnvPath/sanitizeMemberInstanceName) ---
reset_state
: > "${ENV_DIR}/meta_llama3-2-1b-instruct-v1_0.env"
: > "${ENV_DIR}/nomic-embed-text-v1_5.env"
: > "${ENV_DIR}/bge-reranker-v2-m3.env"
if run_ok "three-member-bundle"; then
    for n in meta_llama3-2-1b-instruct-v1_0 nomic-embed-text-v1_5 bge-reranker-v2-m3; do
        grep -q "systemctl start --no-block bedrock-serve@${n}.service" "${SYSTEMCTL_CALLS}" \
            && pass "three-member-bundle: started ${n}" || fail "three-member-bundle: ${n} not started"
    done
    [ "$(grep -c '^systemctl start' "${SYSTEMCTL_CALLS}")" -eq 3 ] \
        && pass "three-member-bundle: started exactly three instances" || fail "three-member-bundle: wrong instance count"
fi

# --- Case 3: no env files ever appear ---
reset_state
_t0=$(date +%s)
BEDROCK_BUNDLE_ENV_WAIT=3 run_fails "no-env-files"
_t1=$(date +%s)
_elapsed=$((_t1 - _t0))
[ "${_elapsed}" -ge 3 ] \
    && pass "no-env-files: waited the full ${_elapsed}s bound before giving up" \
    || fail "no-env-files: returned after only ${_elapsed}s, expected >= 3s"
grep -q . "${SYSTEMCTL_CALLS}" \
    && fail "no-env-files: started something despite no handoff" || pass "no-env-files: nothing started"

# --- Case 4: an env file appears mid-wait ---
reset_state
( sleep 1; : > "${ENV_DIR}/late.env" ) &
BGPID=$!
if BEDROCK_BUNDLE_ENV_WAIT=5 run_ok "env-appears-midwait"; then
    grep -q "systemctl start --no-block bedrock-serve@late.service" "${SYSTEMCTL_CALLS}" \
        && pass "env-appears-midwait: retried until the handoff showed up, then started it" \
        || fail "env-appears-midwait: did not start the member once its env file appeared"
fi
wait "${BGPID}" 2>/dev/null || true

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all bedrock-bundle-init cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
