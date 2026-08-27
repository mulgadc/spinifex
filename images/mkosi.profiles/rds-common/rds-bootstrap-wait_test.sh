#!/bin/sh
set -eu

# rds-bootstrap-wait blocks the boot on the agent's handoff. Two behaviours are
# load-bearing and neither is obvious from the unit file: it must return
# success on timeout (so rds-init is what fails, visibly, on the missing
# handoff) and it must mirror the agent's log to the console when it does,
# because that log is inside a guest nothing can reach.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SCRIPT="${SCRIPT_DIR}/rds-bootstrap-wait"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

FAILS=0
fail() { echo "FAIL: $*" >&2; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

# Stands in for journalctl, printing whatever the case put in JOURNAL_FIXTURE.
STUB="${WORK}/journalctl"
cat > "${STUB}" <<'EOF'
#!/bin/sh
[ -f "${JOURNAL_FIXTURE}" ] && cat "${JOURNAL_FIXTURE}"
exit 0
EOF
chmod 0755 "${STUB}"

run_wait() {
    RDS_HANDOFF_DIR="$1" RDS_HANDOFF_TIMEOUT="$2" RDS_JOURNALCTL="${STUB}" \
        JOURNAL_FIXTURE="${WORK}/journal" sh "${SCRIPT}" > "${WORK}/out" 2>&1
}

# --- the handoff is already there -----------------------------------------
HANDOFF="${WORK}/present"
mkdir -p "${HANDOFF}"
: > "${HANDOFF}/bootstrap.env"
: > "${WORK}/journal"
if run_wait "${HANDOFF}" 5; then
    grep -q 'bootstrap handoff present' "${WORK}/out" ||
        fail "a present handoff was not reported"
    pass "returns immediately when the handoff is already written"
else
    fail "a present handoff still failed the wait"
fi

# --- timeout with the agent having logged ---------------------------------
MISSING="${WORK}/missing"
mkdir -p "${MISSING}"
printf 'dial tcp 10.0.0.1:443: connect: no route to host\n' > "${WORK}/journal"
if run_wait "${MISSING}" 0; then
    pass "timeout exits 0, leaving rds-init to fail on the missing handoff"
else
    fail "timeout failed the unit; rds-init's specific failure would be masked"
fi
grep -q 'no bootstrap handoff' "${WORK}/out" ||
    fail "the timeout did not say the handoff was missing"
grep -q 'no route to host' "${WORK}/out" ||
    fail "the agent's own log was not mirrored to the console on timeout"

# --- timeout with a silent agent ------------------------------------------
: > "${WORK}/journal"
run_wait "${MISSING}" 0
grep -q 'logged nothing at all' "${WORK}/out" ||
    fail "a silent agent was not reported as silent"

if [ "${FAILS}" -eq 0 ]; then
    echo "rds-bootstrap-wait: all tests passed"
    exit 0
fi
exit 1
