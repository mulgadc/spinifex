#!/bin/sh
# Self-contained POSIX test for nats-js-watchdog.sh's restart-decision logic:
# the age gate, cooldown, escalation, and js-probe exit-2 handling. No real
# NATS or spx: systemctl, curl, and spx are all stubbed on PATH ahead of the
# real ones, driven entirely by env vars this test controls.
#
# Run: sh build/scripts/nats-js-watchdog_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${SCRIPT_DIR}/nats-js-watchdog.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

FAKE_BIN="${WORK}/bin"
mkdir -p "${FAKE_BIN}"

FAILS=0
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

# Fake systemctl: `show` reports a controllable ExecMainStartTimestampMonotonic,
# and `try-restart` just records that it was called rather than touching a
# real unit.
cat > "${FAKE_BIN}/systemctl" <<'EOF'
#!/bin/sh
case "$1" in
    show)
        echo "${FAKE_NATS_START_USEC:-0}"
        ;;
    try-restart)
        echo "try-restart $2" >> "${FAKE_RESTART_LOG:-/dev/null}"
        ;;
esac
EOF
chmod +x "${FAKE_BIN}/systemctl"

# Fake curl: js_healthy() only cares about the exit status.
cat > "${FAKE_BIN}/curl" <<'EOF'
#!/bin/sh
[ "${FAKE_HEALTHY:-1}" = "1" ]
EOF
chmod +x "${FAKE_BIN}/curl"

# Fake spx: js-probe's whole contract from the watchdog's side is its exit
# code (0 ok, 1 JetStream refused, 2 inconclusive).
cat > "${FAKE_BIN}/spx" <<'EOF'
#!/bin/sh
exit "${FAKE_SPX_EXIT:-0}"
EOF
chmod +x "${FAKE_BIN}/spx"

PATH="${FAKE_BIN}:${PATH}"
export PATH

# reset_env <name>: fresh per-case dirs and a default env that already clears
# the age, cooldown, and escalation gates (old enough NATS, no cooldown
# stamp, no restart history) and a healthy, single-sample probe, so a case
# only has to override what it is actually testing.
reset_env() {
    CASE="${WORK}/$1"
    mkdir -p "${CASE}/state"
    UPTIME_FILE="${CASE}/uptime"
    printf '700.00 0.00\n' > "${UPTIME_FILE}"
    STATE_DIR="${CASE}/state"
    FAKE_RESTART_LOG="${CASE}/restart.log"
    FAKE_NATS_START_USEC=1000000   # 1s: 700s uptime - 1s start = 699s age, clears the 600s default gate
    FAKE_HEALTHY=1
    FAKE_SPX_EXIT=0
    SAMPLES=1
    SAMPLE_GAP=0
    NATS_MIN_AGE=600
    RESTART_COOLDOWN=600
    RESTART_ESCALATE_AT=3
    SPX_BIN="${FAKE_BIN}/spx"
    STDOUT="${CASE}/stdout"
    STDERR="${CASE}/stderr"
    export UPTIME_FILE STATE_DIR FAKE_RESTART_LOG FAKE_NATS_START_USEC \
        FAKE_HEALTHY FAKE_SPX_EXIT SAMPLES SAMPLE_GAP NATS_MIN_AGE \
        RESTART_COOLDOWN RESTART_ESCALATE_AT SPX_BIN
}

# Run the watchdog, capturing its exit code (without tripping set -e) + streams.
invoke() { rc=0; sh "${SCRIPT}" >"${STDOUT}" 2>"${STDERR}" || rc=$?; }

restarted() { [ -s "${FAKE_RESTART_LOG}" ]; }

# --- Case 1: NATS younger than the age gate -> no restart ---
reset_env young
FAKE_NATS_START_USEC=690000000  # 690s: 700s uptime - 690s start = 10s age, well under 600s
export FAKE_NATS_START_USEC
invoke
[ "${rc}" -eq 0 ] && pass "young: exit 0" || fail "young: expected exit 0 (rc=${rc}): $(cat "${STDERR}")"
restarted && fail "young: must not restart while still in the recovery window" || pass "young: no restart"

# --- Case 2: cooldown stamp fresh -> no restart ---
reset_env cooldown
echo 650 > "${STATE_DIR}/nats-js-watchdog.last-restart"
invoke
[ "${rc}" -eq 0 ] && pass "cooldown: exit 0" || fail "cooldown: expected exit 0 (rc=${rc}): $(cat "${STDERR}")"
restarted && fail "cooldown: must not restart again inside the cooldown window" || pass "cooldown: no restart"

# --- Case 3: escalation threshold reached -> no restart, loud log ---
reset_env escalated
echo 3 > "${STATE_DIR}/nats-js-watchdog.restart-count"
invoke
[ "${rc}" -eq 0 ] && pass "escalated: exit 0" || fail "escalated: expected exit 0 (rc=${rc}): $(cat "${STDERR}")"
restarted && fail "escalated: must not restart past the escalation threshold" || pass "escalated: no restart"
grep -q 'giving up rather than looping' "${STDERR}" \
    && pass "escalated: loud log emitted" || fail "escalated: escalation log missing: $(cat "${STDERR}")"

# --- Case 4: js-probe exits 2 (inconclusive) -> no restart ---
reset_env inconclusive
FAKE_SPX_EXIT=2
export FAKE_SPX_EXIT
invoke
[ "${rc}" -eq 0 ] && pass "inconclusive: exit 0" || fail "inconclusive: expected exit 0 (rc=${rc}): $(cat "${STDERR}")"
restarted && fail "inconclusive: exit 2 must never be read as a JetStream failure" || pass "inconclusive: no restart"
grep -q 'inconclusive' "${STDERR}" \
    && pass "inconclusive: reason logged" || fail "inconclusive: log missing: $(cat "${STDERR}")"

# --- Case 5: genuine JetStream write failure -> restart happens ---
reset_env genuine_failure
FAKE_SPX_EXIT=1
export FAKE_SPX_EXIT
echo 2 > "${STATE_DIR}/nats-js-watchdog.restart-count"
invoke
[ "${rc}" -eq 0 ] && pass "genuine-failure: exit 0" || fail "genuine-failure: expected exit 0 (rc=${rc}): $(cat "${STDERR}")"
restarted && pass "genuine-failure: restart triggered" || fail "genuine-failure: expected a restart: $(cat "${STDERR}")"
[ "$(cat "${STATE_DIR}/nats-js-watchdog.restart-count" 2>/dev/null)" = "3" ] \
    && pass "genuine-failure: restart count incremented" || fail "genuine-failure: restart count not incremented"
[ -f "${STATE_DIR}/nats-js-watchdog.last-restart" ] \
    && pass "genuine-failure: cooldown stamp written" || fail "genuine-failure: cooldown stamp missing"

# --- Case 6: healthy NATS -> no restart, and prior restart history clears ---
reset_env healthy
echo 2 > "${STATE_DIR}/nats-js-watchdog.restart-count"
invoke
[ "${rc}" -eq 0 ] && pass "healthy: exit 0" || fail "healthy: expected exit 0 (rc=${rc}): $(cat "${STDERR}")"
restarted && fail "healthy: must not restart a healthy server" || pass "healthy: no restart"
[ -f "${STATE_DIR}/nats-js-watchdog.restart-count" ] \
    && fail "healthy: restart count must clear once JetStream is confirmed writable" \
    || pass "healthy: restart count cleared"

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all nats-js-watchdog cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
