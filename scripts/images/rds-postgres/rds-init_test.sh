#!/bin/sh
# Self-contained POSIX test for rds-init: the initialize path (initdb, master
# password, initial database), the idempotent attach path, the data-volume
# guard, TLS install and the parameter include. No PostgreSQL, no root: initdb,
# pg_ctl and psql are stubbed on PATH alongside install/chown/su (the script
# drops privileges and sets file ownership), and every path it touches is
# redirected into a temp dir via its env knobs.
#
# Run: sh scripts/images/rds-postgres/rds-init_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${SCRIPT_DIR}/rds-init"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

REAL_LS=$(command -v ls)
REAL_CAT=$(command -v cat)
export REAL_LS REAL_CAT

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

# install stub: honours -d and -m, drops -o/-g (the harness is not root).
cat > "${STUBBIN}/install" <<'EOF'
#!/bin/sh
mode=""; dirmode=0; args=""
while [ $# -gt 0 ]; do
    case "$1" in
        -d) dirmode=1; shift ;;
        -m) mode="$2"; shift 2 ;;
        -o|-g) shift 2 ;;
        *) args="${args} $1"; shift ;;
    esac
done
# shellcheck disable=SC2086
set -- ${args}
if [ "${dirmode}" = 1 ]; then
    mkdir -p "$@" || exit 1
    [ -n "${mode}" ] && chmod "${mode}" "$@"
    exit 0
fi
dest=""
for a in "$@"; do dest="${a}"; done
src=""
for a in "$@"; do [ "${a}" = "${dest}" ] || src="${a}"; done
cp "${src}" "${dest}"
[ -n "${mode}" ] && chmod "${mode}" "${dest}"
exit 0
EOF

# chown stub: file ownership is a no-op outside the guest.
cat > "${STUBBIN}/chown" <<'EOF'
#!/bin/sh
exit 0
EOF

# ls stub: delegates normally, but lets the datadir safety test prove that a
# failed enumeration is not mistaken for an empty directory.
cat > "${STUBBIN}/ls" <<'EOF'
#!/bin/sh
[ "${LS_FAIL:-0}" = "1" ] && exit 74
exec "${REAL_LS}" "$@"
EOF

# su stub: `su postgres -c "cmd"` runs cmd in the harness user's shell.
cat > "${STUBBIN}/su" <<'EOF'
#!/bin/sh
while [ $# -gt 0 ]; do
    case "$1" in
        -c) exec sh -c "$2" ;;
        *) shift ;;
    esac
done
exit 0
EOF

# initdb stub: records the call and materialises the files a real initdb leaves
# behind (PG_VERSION is what rds-init keys "already initialised" off).
cat > "${STUBBIN}/initdb" <<'EOF'
#!/bin/sh
echo "initdb $*" >> "${INITDB_CALLS}"
[ "${INITDB_FAIL:-0}" = "1" ] && exit 1
for a in "$@"; do
    case "$a" in --pgdata=*) pgdata="${a#--pgdata=}" ;; esac
done
mkdir -p "${pgdata}"
echo 18 > "${pgdata}/PG_VERSION"
echo "# stock postgresql.conf" > "${pgdata}/postgresql.conf"
if [ "${PG_HBA_AS_DIR:-0}" = "1" ]; then
    mkdir "${pgdata}/pg_hba.conf"
else
    echo "local all all peer" > "${pgdata}/pg_hba.conf"
fi
exit 0
EOF

# pg_ctl stub: records start/stop of the bootstrap server. PGCTL_START_FAIL
# simulates a cluster that will not come up, e.g. a parameter the guest cannot
# allocate; PGCTL_STOP_FAIL a postmaster that will not let go of the datadir.
# PGCTL_KILL_PARENT signals rds-init from the start call, which is the window
# between initdb and the master role.
cat > "${STUBBIN}/pg_ctl" <<'EOF'
#!/bin/sh
echo "pg_ctl $*" >> "${PGCTL_CALLS}"
for a in "$@"; do
    [ "$a" = "start" ] && [ "${PGCTL_START_FAIL:-0}" = "1" ] && exit 1
    [ "$a" = "stop" ] && [ "${PGCTL_STOP_FAIL:-0}" = "1" ] && exit 1
    if [ "$a" = "start" ] && [ "${PGCTL_KILL_PARENT:-0}" = "1" ]; then
        kill -TERM "$(cat "${KILL_PID_FILE}")"
        exit 0
    fi
done
exit 0
EOF

# psql stub: records the SQL it was fed plus the credentials it was handed
# through the environment (never through argv).
cat > "${STUBBIN}/psql" <<'EOF'
#!/bin/sh
{
    echo "--- psql $* [master=${RDS_MASTER_USERNAME:-} password=${RDS_MASTER_PASSWORD:-} db=${RDS_DB_NAME:-} grp=${RDS_GROUP_ROLE:-}]"
    cat
} >> "${PSQL_CALLS}"
# ON_ERROR_STOP makes a real psql exit non-zero on any SQL failure; the stub
# reproduces that, and with it the echo of the failing statement — password and
# all — that the redaction filter exists to catch.
if [ "${PSQL_FAIL:-0}" = "1" ]; then
    echo "psql:<stdin>:7: ERROR: syntax error at or near \"${RDS_MASTER_PASSWORD:-}\"" >&2
    echo "psql:<stdin>:7: STATEMENT: ALTER ROLE \"m\" WITH LOGIN PASSWORD '${RDS_MASTER_PASSWORD:-}';" >&2
    exit 3
fi
# PSQL_STATUS_CORRUPT=absent: psql succeeds, but rds-init cannot create the
# status file it recovers psql's exit code through — the shape a read-only or
# full /run gives it. -h is the bootstrap directory the status file lives in.
if [ "${PSQL_STATUS_CORRUPT:-}" = "absent" ]; then
    while [ $# -gt 0 ]; do
        [ "$1" = "-h" ] && mkdir -p "$2/psql.status" && break
        shift
    done
fi
exit 0
EOF

# cat stub: delegates normally, but lets the read-back of psql's exit status
# return what a torn write or an I/O error would. rds-init's other use of cat
# takes no argument (it reads the bootstrap script from stdin) and is untouched.
cat > "${STUBBIN}/cat" <<'EOF'
#!/bin/sh
case "${1:-}" in
    *psql.status)
        case "${PSQL_STATUS_CORRUPT:-}" in
            empty) exit 0 ;;
            garbage) echo "not-a-number"; exit 0 ;;
        esac
        ;;
esac
exec "${REAL_CAT}" "$@"
EOF

# sync stub: a no-op, except that SYNC_KILL_PARENT signals rds-init from the
# directory sync that follows the receipt rename. That is the one window where
# an installed receipt and an armed sweep coexist. Matched on the receipt
# directory by name: the engine stamp syncs its directory too, long before the
# traps are armed, and killing there would leave the window untested.
cat > "${STUBBIN}/sync" <<'EOF'
#!/bin/sh
if [ "${SYNC_KILL_PARENT:-0}" = "1" ]; then
    case "${1:-}" in
        */bootstrap) kill -TERM "$(cat "${KILL_PID_FILE}")" ;;
    esac
fi
exit 0
EOF

chmod +x "${STUBBIN}"/*
PATH="${STUBBIN}:${PATH}"
export PATH

FAILS=0
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

DATA_MOUNT="${WORK}/data"
PGDATA="${DATA_MOUNT}/18/data"
SENTINEL="${PGDATA}/rds-bootstrap-incomplete"
RECEIPT_DIR="${DATA_MOUNT}/.spinifex-rds/bootstrap"
RECEIPT="${RECEIPT_DIR}/receipt.env"
STAMP="${DATA_MOUNT}/.spinifex-rds/engine"
HANDOFF="${WORK}/run/spinifex-rds"
MOUNTS="${WORK}/mounts"
KILL_PID_FILE="${WORK}/rds-init.pid"
export KILL_PID_FILE

INITDB_CALLS="${WORK}/initdb.calls"
PGCTL_CALLS="${WORK}/pg_ctl.calls"
PSQL_CALLS="${WORK}/psql.calls"
export INITDB_CALLS PGCTL_CALLS PSQL_CALLS

# write_handoff <mode> <password> <dbname>: lay down the bootstrap.env rds-agent
# would have written. An empty password omits the field, as `attach` does.
# MASTER_USER overrides the master role name, for the reserved-name cases.
# PENDING/PAYLOAD_ID carry the control plane's "this payload is still staged"
# assertion, which an agent predating the receipt protocol omits entirely.
write_handoff() {
    mkdir -p "${HANDOFF}"
    {
        echo "RDS_MODE=$1"
        echo "RDS_DB_INSTANCE_IDENTIFIER=${DB_ID:-db1}"
        echo "RDS_MASTER_USERNAME=${MASTER_USER:-mulgamaster}"
        # Single-quoted as rds-agent's shellQuote writes it, which is what lets a
        # password carrying shell metacharacters — or a newline — survive the
        # handoff intact and reach the checks that exist for it.
        [ -n "$2" ] && echo "RDS_MASTER_PASSWORD='$2'"
        # Quoted as rds-agent's shellQuote writes it, so a name carrying a space
        # or a shell metacharacter survives the handoff and reaches the check
        # that exists for it rather than being mangled on the way.
        [ -n "$3" ] && echo "RDS_DB_NAME='$3'"
        [ -n "${PENDING:-}" ] && echo "RDS_BOOTSTRAP_PENDING=${PENDING}"
        [ -n "${PAYLOAD_ID:-}" ] && echo "RDS_PAYLOAD_ID=${PAYLOAD_ID}"
        # Last, and unconditional: an optional line failing its test would make
        # the whole group's status non-zero and `set -e` would end the run here.
        echo "RDS_PORT=6543"
        echo "RDS_VM_GENERATION=1"
    } > "${HANDOFF}/bootstrap.env"
}

write_tls() {
    mkdir -p "${HANDOFF}"
    echo "CERT" > "${HANDOFF}/server.crt"
    echo "KEY" > "${HANDOFF}/server.key"
}

# write_parameters [setting]: the resolved parameter group rds-agent hands over.
write_parameters() {
    mkdir -p "${HANDOFF}"
    echo "${1:-shared_buffers = 128MB}" > "${HANDOFF}/parameters.conf"
}

# reset_state clears everything the previous case left behind: a fresh datadir,
# an attached data volume, and empty stub-call logs.
reset_state() {
    rm -rf "${WORK}/data" "${WORK}/run" "${WORK}/log"
    mkdir -p "${DATA_MOUNT}"
    printf '/dev/vdb %s ext4 rw,relatime 0 0\n' "${DATA_MOUNT}" > "${MOUNTS}"
    : > "${INITDB_CALLS}"
    : > "${PGCTL_CALLS}"
    : > "${PSQL_CALLS}"
    unset INITDB_FAIL PG_HBA_AS_DIR PSQL_FAIL LS_FAIL RDS_ALLOW_LOCAL_DATADIR MASTER_USER || true
    unset PENDING PAYLOAD_ID DB_ID RECEIPT_DIR_OVERRIDE PSQL_STATUS_CORRUPT STAMP_OVERRIDE || true
}

# run_env: invoke a command with every path knob pointed into the temp dir.
run_env() {
    env RDS_PG_BIN="${STUBBIN}" \
        RDS_DATA_MOUNT="${DATA_MOUNT}" \
        RDS_HANDOFF_DIR="${HANDOFF}" \
        RDS_SOCKET_DIR="${WORK}/run/postgresql" \
        RDS_BOOTSTRAP_DIR="${WORK}/run/rds-init" \
        RDS_LOG_DIR="${WORK}/log" \
        RDS_MOUNTS_FILE="${MOUNTS}" \
        RDS_RECEIPT_DIR="${RECEIPT_DIR_OVERRIDE:-${RECEIPT_DIR}}" \
        RDS_ENGINE_STAMP="${STAMP_OVERRIDE:-${STAMP}}" \
        RDS_ALLOW_LOCAL_DATADIR="${RDS_ALLOW_LOCAL_DATADIR:-0}" \
        INITDB_FAIL="${INITDB_FAIL:-0}" \
        PG_HBA_AS_DIR="${PG_HBA_AS_DIR:-0}" \
        PSQL_FAIL="${PSQL_FAIL:-0}" \
        INITDB_CALLS="${INITDB_CALLS}" PGCTL_CALLS="${PGCTL_CALLS}" PSQL_CALLS="${PSQL_CALLS}" \
        "$@" </dev/null
}

run() { run_env sh "${SCRIPT}"; }

run_ok() { run > "${WORK}/out" 2>&1 || { fail "$1: non-zero exit: $(cat "${WORK}/out")"; return 1; }; }
run_fails() { run > "${WORK}/out" 2>&1 && fail "$1: expected a non-zero exit" || pass "$1: refused"; }

# run_signalled: run rds-init in the background with its PID published for the
# stub that signals it. The wrapper writes the PID before exec'ing the script,
# so the stub cannot read the file before it exists.
run_signalled() {
    run_env sh -c 'echo $$ > "$1"; shift; exec sh "$@"' _ "${KILL_PID_FILE}" "${SCRIPT}" \
        > "${WORK}/out" 2>&1 &
    _bg=$!
    wait "${_bg}"
}

run_signalled_fails() {
    run_signalled && fail "$1: expected a non-zero exit" || pass "$1: refused"
}

# --- Case 1: initialize on a fresh data volume ---
reset_state
write_handoff initialize 's3cr3t' appdb
write_tls
write_parameters
if run_ok "initialize"; then
    grep -q 'initdb' "${INITDB_CALLS}" && pass "initialize: initdb ran" || fail "initialize: no initdb"
    grep -q -- '--data-checksums' "${INITDB_CALLS}" \
        && pass "initialize: data checksums on" || fail "initialize: no --data-checksums"
    grep -q 'host all all 0.0.0.0/0 scram-sha-256' "${PGDATA}/pg_hba.conf" \
        && pass "initialize: remote scram hba rule appended" || fail "initialize: no hba rule"
    grep -q "^include_dir = 'conf.d'" "${PGDATA}/postgresql.conf" \
        && pass "initialize: include_dir hooked" || fail "initialize: no include_dir"
    grep -q '^port = 6543' "${PGDATA}/conf.d/90-rds-init.conf" \
        && pass "initialize: delivered port applied" || fail "initialize: port not applied"
    grep -q '^ssl = on' "${PGDATA}/conf.d/90-rds-init.conf" \
        && pass "initialize: ssl enabled" || fail "initialize: ssl not enabled"
    grep -q 'shared_buffers = 128MB' "${PGDATA}/conf.d/10-rds-parameters.conf" \
        && pass "initialize: resolved parameters installed" || fail "initialize: no parameter include"
    grep -q 'ALTER ROLE :"master" WITH LOGIN NOSUPERUSER CREATEDB CREATEROLE PASSWORD' "${PSQL_CALLS}" \
        && pass "initialize: master password applied" || fail "initialize: no ALTER ROLE"
    grep -q 'password=s3cr3t' "${PSQL_CALLS}" \
        && pass "initialize: password passed through the environment" || fail "initialize: password not in psql env"
    grep -q 'CREATE DATABASE' "${PSQL_CALLS}" \
        && pass "initialize: initial database created" || fail "initialize: no CREATE DATABASE"
    grep -q 'CREATE EXTENSION' "${PSQL_CALLS}" \
        && fail "initialize: a tenant instance installed a platform extension" \
        || pass "initialize: tenant instance installs no platform extension"
    grep -q 'GRANT CREATE ON DATABASE' "${PSQL_CALLS}" \
        && fail "initialize: a tenant instance was granted database CREATE" \
        || pass "initialize: tenant instance gets no platform database grant"
    grep -q 'createrole_self_grant' "${PSQL_CALLS}" \
        && fail "initialize: a tenant master got createrole_self_grant" \
        || pass "initialize: tenant master gets no createrole_self_grant"

    # The master role is administrative but not a PostgreSQL superuser: a
    # superuser reaches outside the database (COPY FROM PROGRAM, pg_read_file,
    # untrusted languages), so master credentials would be a shell in the DB VM.
    grep -qE '(^|[^O])SUPERUSER' "${PSQL_CALLS}" \
        && fail "initialize: bootstrap SQL still grants SUPERUSER" \
        || pass "initialize: master role is not a superuser"
    grep -q 'GRANT :"grp" TO :"master" WITH ADMIN OPTION' "${PSQL_CALLS}" \
        && pass "initialize: master holds the administrative group role" \
        || fail "initialize: master never granted the group role"
    grep -q 'grp=rds_superuser' "${PSQL_CALLS}" \
        && pass "initialize: the group role is named as on AWS" || fail "initialize: group role name not delivered"
    grep -q 'GRANT pg_monitor, pg_signal_backend, pg_checkpoint TO :"grp"' "${PSQL_CALLS}" \
        && pass "initialize: monitoring/signal/checkpoint granted" || fail "initialize: group role has no privileges"

    # The three predefined roles that hand back exactly what SUPERUSER gave away.
    grep -qE 'pg_(read|write)_server_files|pg_execute_server_program' "${PSQL_CALLS}" \
        && fail "initialize: granted a role that restores file or program access" \
        || pass "initialize: no server-file or program-execution roles granted"
    grep -q "listen_addresses=''" "${PGCTL_CALLS}" \
        && pass "initialize: bootstrap server is socket-only" || fail "initialize: bootstrap server listened on TCP"
    grep -q 'stop' "${PGCTL_CALLS}" \
        && pass "initialize: bootstrap server stopped" || fail "initialize: bootstrap server left running"
    [ -e "${SENTINEL}" ] \
        && fail "initialize: incomplete-bootstrap sentinel left behind" \
        || pass "initialize: sentinel cleared once the master role exists"
    [ -e "${WORK}/run/rds-init/pg_hba.conf" ] \
        && fail "initialize: the trust-auth pg_hba outlived the bootstrap window" \
        || pass "initialize: trust-auth pg_hba removed"
fi

# --- Case 2: the master password does not outlive its use ---
grep -q '^RDS_MASTER_PASSWORD=' "${HANDOFF}/bootstrap.env" \
    && fail "consume: password still in the handoff" || pass "consume: password dropped from the handoff"
grep -q '^RDS_MASTER_USERNAME=mulgamaster' "${HANDOFF}/bootstrap.env" \
    && pass "consume: the rest of the config is kept" || fail "consume: non-secret fields lost"
[ -f "${HANDOFF}/server.key" ] \
    && fail "consume: serving key left in the handoff" || pass "consume: serving key moved out of the handoff"
[ -f "${WORK}/run/postgresql/tls/server.key" ] \
    && pass "tls: key installed on tmpfs, not the data volume" || fail "tls: key not installed"
[ -f "${PGDATA}/server.key" ] \
    && fail "tls: key written into the datadir (would land in every snapshot)" || pass "tls: datadir free of the key"

# --- Case 3: second boot (attach) is idempotent ---
: > "${INITDB_CALLS}"; : > "${PSQL_CALLS}"; : > "${PGCTL_CALLS}"
write_handoff attach '' appdb
write_parameters
if run_ok "attach"; then
    [ -s "${INITDB_CALLS}" ] \
        && fail "attach: initdb re-ran on an initialised datadir" || pass "attach: initdb skipped"
    [ -s "${PSQL_CALLS}" ] \
        && fail "attach: re-applied the master role" || pass "attach: no role/database work"
    grep -q '^ssl = on' "${PGDATA}/conf.d/90-rds-init.conf" \
        && pass "attach: TLS survives a handoff without a new cert" || fail "attach: TLS turned off"
fi

# --- Case 3b: the Ochre appliance master installs pgvector ---
# No DBName, as the appliance is launched: create_database is skipped and the
# gated extension install is the only platform-specific bootstrap step.
reset_state
MASTER_USER=ochre_vector_admin
write_handoff initialize 's3cr3t' ''
write_parameters
if run_ok "ochre-appliance"; then
    grep -q 'CREATE EXTENSION IF NOT EXISTS vector SCHEMA extensions' "${PSQL_CALLS}" \
        && pass "ochre-appliance: pgvector installed into the extensions schema" \
        || fail "ochre-appliance: pgvector not installed into the extensions schema"
    grep -q 'GRANT USAGE ON SCHEMA extensions TO PUBLIC' "${PSQL_CALLS}" \
        && pass "ochre-appliance: extensions schema readable by account roles" \
        || fail "ochre-appliance: extensions schema not granted to PUBLIC"
    grep -q 'GRANT CREATE ON DATABASE postgres TO "ochre_vector_admin"' "${PSQL_CALLS}" \
        && pass "ochre-appliance: master granted CREATE on the database" \
        || fail "ochre-appliance: master not granted CREATE on the database"
    grep -q 'createrole_self_grant' "${PSQL_CALLS}" \
        && pass "ochre-appliance: master can set into the roles it creates" \
        || fail "ochre-appliance: master missing createrole_self_grant"
fi
unset MASTER_USER

# --- Case 3a: a parameter group cannot log the master password ---
# The customer's parameters are installed before the master role is applied and
# the bootstrap postmaster does not override config_file, so log_statement = all
# is live for the ALTER ROLE carrying the password. The guard is session-local
# and SUSET, so it holds whatever the parameter group says.
reset_state
write_handoff initialize 's3cr3t' appdb
write_parameters "log_statement = all"
if run_ok "log-guard"; then
    grep -q '^log_statement = all' "${PGDATA}/conf.d/10-rds-parameters.conf" \
        && pass "log-guard: the customer's logging parameter is installed" \
        || fail "log-guard: the parameter group was not installed, so this is not the reported case"
    for setting in "SET log_statement = 'none';" \
        "SET log_min_duration_statement = -1;" \
        "SET log_min_error_statement = 'panic';"; do
        grep -qF "${setting}" "${PSQL_CALLS}" \
            && pass "log-guard: ${setting}" || fail "log-guard: psql never received ${setting}"
    done
    # Every bootstrap session carries the guard, so a statement added to the
    # bootstrap later cannot end up outside it.
    # `grep -c` exits non-zero on no match, which `set -e` would turn into an
    # aborted run in place of a reported failure.
    guard_count=$(grep -cF "SET log_statement = 'none';" "${PSQL_CALLS}" || true)
    psql_count=$(grep -c '^--- psql ' "${PSQL_CALLS}" || true)
    [ "${guard_count}" = "${psql_count}" ] \
        && pass "log-guard: all ${psql_count} bootstrap sessions guarded" \
        || fail "log-guard: ${guard_count} of ${psql_count} bootstrap sessions guarded"
    guard_line=$(grep -nF "SET log_statement = 'none';" "${PSQL_CALLS}" | head -n 1 | cut -d: -f1)
    alter_line=$(grep -n 'ALTER ROLE' "${PSQL_CALLS}" | head -n 1 | cut -d: -f1)
    { [ -n "${alter_line}" ] && [ "${guard_line}" -lt "${alter_line}" ]; } \
        && pass "log-guard: the guard precedes the ALTER ROLE" \
        || fail "log-guard: the guard does not precede the ALTER ROLE"
fi

# --- Case 3b: a failing psql must not echo the password to the console ---
# rds-init.initd sends this script's output to /dev/console, which the host
# captures off ttyS0, and psql under ON_ERROR_STOP echoes the statement it
# failed on. The redaction must not swallow the failure: the datadir is cleared
# exactly as in case 6d.
reset_state
write_handoff initialize 's3cr3t' appdb
write_parameters
PSQL_FAIL=1
export PSQL_FAIL
run_fails "console-redaction"
grep -q 's3cr3t' "${WORK}/out" \
    && fail "console-redaction: the console capture carries the master password" \
    || pass "console-redaction: no password on the console"
grep -q '\[REDACTED\]' "${WORK}/out" \
    && pass "console-redaction: the redaction is marked" \
    || fail "console-redaction: psql's stderr never reached the console"
grep -q 'ERROR: syntax error' "${WORK}/out" \
    && pass "console-redaction: the diagnostic itself survives" \
    || fail "console-redaction: the redaction swallowed the diagnostic"
[ -e "${PGDATA}/PG_VERSION" ] \
    && fail "console-redaction: datadir kept after a failed master bootstrap" \
    || pass "console-redaction: the failure still propagates"
unset PSQL_FAIL

# --- Case 3c: psql's status must cross the redaction pipeline or fail closed ---
# The status file is how psql's exit code leaves the subshell, POSIX sh having
# no pipefail. A torn write leaves it created but empty, and `return ""` is
# fatal in the guest's ash: rds-init would die past the datadir sweep and before
# any diagnostic, leaving a trust-authenticated postmaster on a deleted datadir.
for corruption in empty garbage; do
    reset_state
    write_handoff initialize 's3cr3t' appdb
    PSQL_STATUS_CORRUPT="${corruption}"
    export PSQL_STATUS_CORRUPT
    run_fails "psql-status-${corruption}"
    grep -q 'exit status could not be read back' "${WORK}/out" \
        && pass "psql-status-${corruption}: names the unreadable status rather than blaming psql" \
        || fail "psql-status-${corruption}: no diagnostic naming the status file"
    grep -q 'Illegal number\|numeric argument required' "${WORK}/out" \
        && fail "psql-status-${corruption}: an unvalidated status reached return" \
        || pass "psql-status-${corruption}: nothing unvalidated reached return"
    [ -e "${PGDATA}/PG_VERSION" ] \
        && fail "psql-status-${corruption}: datadir kept after an unrecoverable status" \
        || pass "psql-status-${corruption}: datadir cleared"
    grep -q 'stop' "${PGCTL_CALLS}" \
        && pass "psql-status-${corruption}: bootstrap server stopped" \
        || fail "psql-status-${corruption}: bootstrap server left holding the datadir"
    unset PSQL_STATUS_CORRUPT
done

# --- Case 3d: a status file that could never be written is not psql's fault ---
# The write can fail outright — a read-only or full /run — while psql itself
# succeeded. Failing closed is right, but the operator must not be sent to a
# PostgreSQL that did exactly what it was asked.
reset_state
write_handoff initialize 's3cr3t' appdb
PSQL_STATUS_CORRUPT=absent
export PSQL_STATUS_CORRUPT
run_fails "psql-status-absent"
grep -q 'exit status was never recorded' "${WORK}/out" \
    && pass "psql-status-absent: names the missing status file" \
    || fail "psql-status-absent: no diagnostic distinguishing it from a psql failure"
[ -e "${PGDATA}/PG_VERSION" ] \
    && fail "psql-status-absent: datadir kept" || pass "psql-status-absent: datadir cleared"
unset PSQL_STATUS_CORRUPT

# --- Case 3e: a multi-line master password is refused before it is spent ---
# redact_stream is line-oriented, so a password spanning a line boundary matches
# neither half and reaches /dev/console in the clear. The control plane rejects
# one; this is the guest asserting that rather than trusting it.
reset_state
write_handoff initialize 'top
secret' appdb
run_fails "multiline-password"
[ -s "${INITDB_CALLS}" ] \
    && fail "multiline-password: initdb ran before the password was refused" \
    || pass "multiline-password: refused before initdb spent it"
grep -q 'spans more than one line' "${WORK}/out" \
    && pass "multiline-password: refusal names the reason" \
    || fail "multiline-password: no refusal message"

# --- Case 4: an empty datadir in attach mode means the data volume is missing ---
reset_state
write_handoff attach '' appdb
run_fails "attach-empty"
[ -s "${INITDB_CALLS}" ] \
    && fail "attach-empty: initialised an empty cluster over missing data" || pass "attach-empty: no initdb"

# --- Case 5: the datadir must be on the attached data volume ---
reset_state
: > "${MOUNTS}"
write_handoff initialize 's3cr3t' appdb
run_fails "no-volume"
[ -s "${INITDB_CALLS}" ] \
    && fail "no-volume: initialised on the boot volume" || pass "no-volume: no initdb"

RDS_ALLOW_LOCAL_DATADIR=1
export RDS_ALLOW_LOCAL_DATADIR
if run_ok "local-datadir"; then
    grep -q 'initdb' "${INITDB_CALLS}" \
        && pass "local-datadir: explicit override initialises anyway" || fail "local-datadir: no initdb"
fi
unset RDS_ALLOW_LOCAL_DATADIR

# --- Case 6: a failed initdb leaves nothing that looks initialised ---
reset_state
write_handoff initialize 's3cr3t' appdb
INITDB_FAIL=1
export INITDB_FAIL
run_fails "initdb-fail"
[ -e "${PGDATA}/PG_VERSION" ] \
    && fail "initdb-fail: half-written datadir kept" || pass "initdb-fail: datadir cleared"
unset INITDB_FAIL

# --- Case 6a: a post-initdb failure is covered by the cleanup trap ---
reset_state
write_handoff initialize 's3cr3t' appdb
PG_HBA_AS_DIR=1
export PG_HBA_AS_DIR
run_fails "post-initdb-fail"
[ -e "${PGDATA}/PG_VERSION" ] \
    && fail "post-initdb-fail: datadir kept before the former late trap point" \
    || pass "post-initdb-fail: datadir cleared"
unset PG_HBA_AS_DIR

# --- Case 6b: a failed emptiness probe must abort before initdb ---
# No output from ls can mean either "empty" or "could not enumerate". Only a
# successful probe may authorize cleanup of a datadir this invocation creates.
reset_state
write_handoff initialize 's3cr3t' appdb
mkdir -p "${PGDATA}"
: > "${PGDATA}/PG_VERSION"
echo 'customer table data' > "${PGDATA}/base-relation"
LS_FAIL=1
export LS_FAIL
run_fails "datadir-probe-fail"
unset LS_FAIL
[ -s "${INITDB_CALLS}" ] \
    && fail "datadir-probe-fail: initdb ran after the probe failed" \
    || pass "datadir-probe-fail: initdb not run"
[ -f "${PGDATA}/base-relation" ] \
    && pass "datadir-probe-fail: pre-existing datadir preserved" \
    || fail "datadir-probe-fail: pre-existing datadir deleted"
grep -q 'could not inspect datadir' "${WORK}/out" \
    && pass "datadir-probe-fail: refusal explains why" || fail "datadir-probe-fail: no refusal message"

# --- Case 6c: a failed initdb over a NON-empty datadir must not delete it ---
# A zero-length PG_VERSION over an otherwise intact datadir takes the
# initialise path, and "directory not empty" is one way initdb then fails.
# Clearing there would destroy customer data in response to the one signal
# that it is still present.
reset_state
write_handoff initialize 's3cr3t' appdb
mkdir -p "${PGDATA}"
: > "${PGDATA}/PG_VERSION"
echo 'customer table data' > "${PGDATA}/base-relation"
INITDB_FAIL=1
export INITDB_FAIL
run_fails "initdb-fail-nonempty"
[ -f "${PGDATA}/base-relation" ] \
    && pass "initdb-fail-nonempty: pre-existing datadir preserved" \
    || fail "initdb-fail-nonempty: pre-existing datadir deleted"
grep -q 'refusing to clear' "${WORK}/out" \
    && pass "initdb-fail-nonempty: refusal explains why" || fail "initdb-fail-nonempty: no refusal message"
unset INITDB_FAIL

# --- Case 6d: a failed master bootstrap leaves nothing bootable either ---
# postgresql is in the default runlevel independently of this oneshot, so a
# datadir kept here would start an engine whose master role has no password —
# and the password is one-shot, so the next fetch is `attach` and cannot repair
# it. The datadir must go the same way a failed initdb's does.
reset_state
write_handoff initialize 's3cr3t' appdb
PSQL_FAIL=1
export PSQL_FAIL
run_fails "bootstrap-fail"
[ -e "${PGDATA}/PG_VERSION" ] \
    && fail "bootstrap-fail: datadir kept after a failed master bootstrap" \
    || pass "bootstrap-fail: datadir cleared"
grep -q 'stop' "${PGCTL_CALLS}" \
    && pass "bootstrap-fail: bootstrap server stopped" || fail "bootstrap-fail: bootstrap server left running"
unset PSQL_FAIL

# --- Case 6e: the bootstrap server never starts ---
# `set -e` aborts the moment pg_ctl start fails, so nothing inside
# bootstrap_master runs. The datadir already carries PG_VERSION and a pg_hba
# accepting scram, and the password is spent, so leaving it would attach on the
# next boot and serve a database whose master role has no password.
reset_state
write_handoff initialize 's3cr3t' appdb
PGCTL_START_FAIL=1
export PGCTL_START_FAIL
run_fails "bootstrap-nostart"
[ -e "${PGDATA}/PG_VERSION" ] \
    && fail "bootstrap-nostart: datadir kept after the cluster failed to start" \
    || pass "bootstrap-nostart: datadir cleared"
unset PGCTL_START_FAIL

# --- Case 6f: a pre-existing datadir is never swept by the trap ---
# The clear is scoped to a datadir this invocation created. A torn write that
# loses PG_VERSION over intact data takes the initialise branch, and the sweep
# must not answer that by destroying the customer's data.
reset_state
write_handoff initialize 's3cr3t' appdb
mkdir -p "${PGDATA}"
echo "customer data" > "${PGDATA}/base_survivor"
INITDB_FAIL=1
export INITDB_FAIL
run_fails "preexisting-nosweep"
[ -e "${PGDATA}/base_survivor" ] \
    && pass "preexisting-nosweep: pre-existing datadir untouched" \
    || fail "preexisting-nosweep: swept a datadir it did not create"
unset INITDB_FAIL

# --- Case 6g: a SIGTERM between initdb and the master role clears the datadir ---
# An EXIT trap does not run on a signal in a POSIX shell, so a shutdown, an ACPI
# powerdown from a stop arriving while the instance is still creating, or a
# host-side force-stop would otherwise leave a datadir with no master role.
reset_state
write_handoff initialize 's3cr3t' appdb
PGCTL_KILL_PARENT=1
export PGCTL_KILL_PARENT
run_signalled_fails "sigterm-midbootstrap"
[ -e "${PGDATA}/PG_VERSION" ] \
    && fail "sigterm-midbootstrap: datadir kept after a signal mid-bootstrap" \
    || pass "sigterm-midbootstrap: datadir cleared"
[ -s "${PSQL_CALLS}" ] \
    && fail "sigterm-midbootstrap: the master role work ran after the signal" \
    || pass "sigterm-midbootstrap: stopped before the master role"
unset PGCTL_KILL_PARENT

# --- Case 6h: a datadir carrying the sentinel is refused, not started ---
# No trap survives a crash or a SIGKILL, so the sentinel is the only record that
# the bootstrap did not finish. It says the master role is missing, not that the
# volume is empty, so the refusal must not clear anything.
reset_state
write_handoff initialize 's3cr3t' appdb
if run_ok "sentinel-setup"; then
    echo 'customer table data' > "${PGDATA}/base-relation"
    : > "${SENTINEL}"
    write_handoff attach '' appdb
    run_fails "sentinel-refused"
    [ -f "${PGDATA}/base-relation" ] \
        && pass "sentinel-refused: datadir preserved" || fail "sentinel-refused: datadir cleared"
    grep -q 'recover the data volume out of band' "${WORK}/out" \
        && pass "sentinel-refused: refusal points at out-of-band recovery" \
        || fail "sentinel-refused: no refusal message"
fi

# --- Case 6i: a bootstrap server that will not stop is a failure ---
# It holds the datadir, so the postgresql service cannot start for the life of
# the boot. The master role exists by then, so the datadir itself must survive.
reset_state
write_handoff initialize 's3cr3t' appdb
PGCTL_STOP_FAIL=1
export PGCTL_STOP_FAIL
run_fails "stop-fail"
grep -q 'did not stop' "${WORK}/out" \
    && pass "stop-fail: reported rather than swallowed" || fail "stop-fail: no failure message"
[ -e "${PGDATA}/PG_VERSION" ] \
    && pass "stop-fail: bootstrapped datadir kept" || fail "stop-fail: cleared a datadir with a master role"
[ -e "${SENTINEL}" ] \
    && fail "stop-fail: sentinel kept although the master role exists" \
    || pass "stop-fail: sentinel cleared"
unset PGCTL_STOP_FAIL

# --- Case 6j: a completed bootstrap leaves a receipt proving it ---
# The receipt is what lets rds-agent tell the control plane the staged password
# was durably applied, so the ciphertext can be destroyed. It lives outside
# PGDATA, which the failure traps rm -rf.
reset_state
PENDING=1 PAYLOAD_ID=bp-alpha DB_ID=db-alpha
export PENDING PAYLOAD_ID DB_ID
write_handoff initialize 's3cr3t' appdb
if run_ok "receipt"; then
    [ -f "${RECEIPT}" ] \
        && pass "receipt: written on the initialize path" || fail "receipt: no receipt written"
    grep -q '^RDS_RECEIPT_PAYLOAD_ID=bp-alpha$' "${RECEIPT}" \
        && pass "receipt: names the payload it applied" || fail "receipt: wrong or missing payload id"
    grep -q '^RDS_RECEIPT_DB_INSTANCE_IDENTIFIER=db-alpha$' "${RECEIPT}" \
        && pass "receipt: names its DB instance" || fail "receipt: wrong or missing DB instance"
    grep -q 'PASSWORD\|s3cr3t' "${RECEIPT}" \
        && fail "receipt: carries a secret" || pass "receipt: carries no secrets"
    case "${RECEIPT}" in
        "${PGDATA}"/*) fail "receipt: written inside the datadir the traps clear" ;;
        *) pass "receipt: written outside PGDATA" ;;
    esac
fi

# --- Case 6k: a receipt that cannot be written is fatal ---
# A successful bootstrap whose receipt never landed would bring up a healthy
# engine while the agent blocked forever on a receipt that never appears —
# a working database that can never take a password change or parameter reload.
reset_state
PENDING=1 PAYLOAD_ID=bp-beta
export PENDING PAYLOAD_ID
write_handoff initialize 's3cr3t' appdb
: > "${WORK}/not-a-dir"
RECEIPT_DIR_OVERRIDE="${WORK}/not-a-dir/bootstrap"
export RECEIPT_DIR_OVERRIDE
run_fails "receipt-fail"
[ -e "${PGDATA}/PG_VERSION" ] \
    && fail "receipt-fail: datadir kept although its bootstrap cannot be proven" \
    || pass "receipt-fail: datadir cleared"
grep -q 'bootstrap receipt' "${WORK}/out" \
    && pass "receipt-fail: reported rather than swallowed" || fail "receipt-fail: no failure message"
unset RECEIPT_DIR_OVERRIDE

# --- Case 6k2: a sweep between the receipt and the retired traps takes both ---
# The receipt is installed while the traps are still armed. A receipt left behind
# by a sweep would be read by rds-agent, acknowledged, and the control plane would
# destroy the only copy of the password this datadir can be initialised with.
reset_state
PENDING=1 PAYLOAD_ID=bp-gamma
export PENDING PAYLOAD_ID
write_handoff initialize 's3cr3t' appdb
SYNC_KILL_PARENT=1
export SYNC_KILL_PARENT
run_signalled_fails "receipt-swept"
[ -e "${PGDATA}/PG_VERSION" ] \
    && fail "receipt-swept: datadir kept after a signal mid-bootstrap" \
    || pass "receipt-swept: datadir cleared"
[ -e "${RECEIPT}" ] \
    && fail "receipt-swept: receipt survived the datadir it vouches for" \
    || pass "receipt-swept: receipt cleared with the datadir"
unset SYNC_KILL_PARENT

# --- Case 6l: pending payload, initialised datadir, no receipt -> fail closed ---
# The datadir alone cannot separate a legacy instance that bootstrapped before
# receipts existed from one that applied the master role and crashed before the
# receipt was durable. Only the control plane's pending flag can.
reset_state
PENDING=1 PAYLOAD_ID=bp-gamma DB_ID=db-gamma
export PENDING PAYLOAD_ID DB_ID
write_handoff initialize 's3cr3t' appdb
if run_ok "pending-setup"; then
    echo 'customer table data' > "${PGDATA}/base-relation"
    rm -f "${RECEIPT}"
    run_fails "pending-no-receipt"
    [ -f "${PGDATA}/base-relation" ] \
        && pass "pending-no-receipt: datadir preserved" || fail "pending-no-receipt: datadir cleared"
    grep -q 'master role cannot be proven' "${WORK}/out" \
        && pass "pending-no-receipt: refusal names the real reason" \
        || fail "pending-no-receipt: no refusal message"
    grep -q 'password is spent' "${WORK}/out" \
        && fail "pending-no-receipt: claims the password is spent, which it is not" \
        || pass "pending-no-receipt: does not claim a spent password"

    # --- Case 6m: the same boot with a matching receipt attaches ---
    : > "${INITDB_CALLS}"; : > "${PSQL_CALLS}"
    mkdir -p "${RECEIPT_DIR}"
    printf 'RDS_RECEIPT_PAYLOAD_ID=bp-gamma\nRDS_RECEIPT_DB_INSTANCE_IDENTIFIER=db-gamma\n' > "${RECEIPT}"
    if run_ok "pending-with-receipt"; then
        [ -s "${INITDB_CALLS}" ] \
            && fail "pending-with-receipt: re-ran initdb" || pass "pending-with-receipt: attached"
    fi

    # --- Case 6n: a receipt naming another instance is treated as absent ---
    # The receipt is on the data volume, so it rides along in every snapshot of
    # it and a restored volume carries the source instance's receipt.
    printf 'RDS_RECEIPT_PAYLOAD_ID=bp-gamma\nRDS_RECEIPT_DB_INSTANCE_IDENTIFIER=db-somebody-else\n' > "${RECEIPT}"
    run_fails "pending-foreign-receipt"
    [ -f "${PGDATA}/base-relation" ] \
        && pass "pending-foreign-receipt: datadir preserved" || fail "pending-foreign-receipt: datadir cleared"
fi
unset PENDING PAYLOAD_ID DB_ID

# --- Case 6o: no pending payload leaves the attach path unchanged ---
# An agent predating the receipt protocol sends neither flag, and a legacy
# instance carries no receipt. Neither may be turned into a refusal.
reset_state
write_handoff initialize 's3cr3t' appdb
if run_ok "legacy-setup"; then
    rm -rf "${RECEIPT_DIR}"
    : > "${INITDB_CALLS}"
    write_handoff attach '' appdb
    if run_ok "legacy-attach"; then
        [ -s "${INITDB_CALLS}" ] \
            && fail "legacy-attach: re-ran initdb" || pass "legacy-attach: attached without a receipt"
    fi
fi

# --- Case 7: no serving cert -> TLS off rather than a failed start ---
reset_state
write_handoff initialize 's3cr3t' ''
if run_ok "no-cert"; then
    grep -q '^ssl = off' "${PGDATA}/conf.d/90-rds-init.conf" \
        && pass "no-cert: ssl off" || fail "no-cert: ssl not off"
    grep -q 'ssl_cert_file' "${PGDATA}/conf.d/90-rds-init.conf" \
        && fail "no-cert: points at a cert that was never delivered" || pass "no-cert: no cert paths"
    grep -q 'CREATE DATABASE' "${PSQL_CALLS}" \
        && fail "no-dbname: created a database without a DBName" || pass "no-dbname: no initial database"
fi

# --- Case 8: no handoff at all ---
reset_state
rm -rf "${HANDOFF}"
run_fails "no-handoff"

# --- Case 9: a master username the platform reserves ---
# The master role is created NOSUPERUSER, so one named after the bootstrap
# superuser would leave the cluster without a superuser at all and strip
# rds-agent of the privileged SQL it needs — on a datadir that bootstraps once.
# The set and the case-insensitive matching are the control plane's: a name it
# refuses must not reach initdb here and spend the one-shot password.
for reserved in postgres rds_superuser rdsadmin pg_toast_owner PostGres; do
    reset_state
    MASTER_USER="${reserved}"
    write_handoff initialize 's3cr3t' appdb
    run_fails "reserved-master-${reserved}"
    [ -s "${INITDB_CALLS}" ] \
        && fail "reserved-master-${reserved}: initdb ran before the name was refused" \
        || pass "reserved-master-${reserved}: refused before initdb"
    unset MASTER_USER
done

# --- Case 10: an initial database name the control plane would have refused ---
# The name is interpolated into a CREATE DATABASE, and a failure there is a
# failure after initdb: the datadir is cleared and the one-shot password is
# gone. Refusing before initdb is what keeps the create retryable.
for badname in 'my-db' 'my db' 'my/db' 'my.db' '1db' \
    'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; do
    reset_state
    write_handoff initialize 's3cr3t' "${badname}"
    run_fails "bad-dbname-${badname}"
    [ -s "${INITDB_CALLS}" ] \
        && fail "bad-dbname-${badname}: initdb ran before the name was refused" \
        || pass "bad-dbname-${badname}: refused before initdb"
done

# The rule is a rejection of malformed names, not of every name: the boundary
# length and an underscore have to keep working.
reset_state
write_handoff initialize 's3cr3t' \
    'a_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
write_parameters
if run_ok "dbname-at-the-limit"; then
    grep -q 'CREATE DATABASE' "${PSQL_CALLS}" \
        && pass "dbname-at-the-limit: accepted" || fail "dbname-at-the-limit: no CREATE DATABASE"
fi

# --- Case 11: the data volume records which engine wrote it ---
# Another engine's datadir mounts cleanly and reads as uninitialised here, so
# without the stamp rds-init would initdb beside the customer's data and serve
# an empty database that passes every health probe while the real data sits
# unreferenced — until the first automated snapshot captures the wrong one.
reset_state
write_handoff initialize 's3cr3t' appdb
if run_ok "stamp-absent-empty"; then
    [ "$(cat "${STAMP}" 2>/dev/null)" = "postgres" ] \
        && pass "stamp-absent-empty: fresh volume stamped postgres" \
        || fail "stamp-absent-empty: no stamp written at initialisation"
    case "${STAMP}" in
        "${PGDATA}"/*) fail "stamp-absent-empty: written inside the datadir the traps clear" ;;
        *) pass "stamp-absent-empty: written outside PGDATA" ;;
    esac

    # --- Case 11a: a matching stamp attaches ---
    : > "${INITDB_CALLS}"
    write_handoff attach '' appdb
    if run_ok "stamp-match"; then
        [ -s "${INITDB_CALLS}" ] \
            && fail "stamp-match: re-ran initdb" || pass "stamp-match: attached"
    fi

    # --- Case 11b: a disagreeing stamp is fatal and touches nothing ---
    echo 'customer table data' > "${PGDATA}/base-relation"
    printf 'mariadb\n' > "${STAMP}"
    : > "${INITDB_CALLS}"
    run_fails "stamp-mismatch"
    [ -s "${INITDB_CALLS}" ] \
        && fail "stamp-mismatch: initdb ran on another engine's volume" \
        || pass "stamp-mismatch: no initdb"
    [ -f "${PGDATA}/base-relation" ] \
        && pass "stamp-mismatch: datadir preserved" || fail "stamp-mismatch: datadir cleared"
    [ "$(cat "${STAMP}")" = "mariadb" ] \
        && pass "stamp-mismatch: the other engine's stamp is left as it stands" \
        || fail "stamp-mismatch: stamp overwritten"
    grep -q "holds a 'mariadb' datadir" "${WORK}/out" \
        && pass "stamp-mismatch: refusal names both engines" \
        || fail "stamp-mismatch: no refusal naming the stamped engine"

    # --- Case 11c: a stamp that cannot be read is not an absent one ---
    # Reading it as absent would backfill our own engine over a volume whose
    # engine was never established, turning the check into a rubber stamp.
    rm -f "${STAMP}"
    mkdir "${STAMP}"
    run_fails "stamp-unreadable"
    grep -q 'could not be read' "${WORK}/out" \
        && pass "stamp-unreadable: refusal names the unreadable stamp" \
        || fail "stamp-unreadable: no refusal message"
    rmdir "${STAMP}"

    # --- Case 11c2: a zero-length stamp does not read as a match ---
    : > "${STAMP}"
    run_fails "stamp-empty"
    [ -f "${PGDATA}/base-relation" ] \
        && pass "stamp-empty: datadir preserved" || fail "stamp-empty: datadir cleared"

    # --- Case 11d: an unstamped datadir with data in it is backfilled ---
    # PostgreSQL volumes predate the stamp, so the check becomes total over time
    # rather than refusing every instance created before it existed.
    rm -f "${STAMP}"
    : > "${INITDB_CALLS}"
    if run_ok "stamp-absent-nonempty"; then
        [ "$(cat "${STAMP}" 2>/dev/null)" = "postgres" ] \
            && pass "stamp-absent-nonempty: existing datadir gains a stamp" \
            || fail "stamp-absent-nonempty: stamp not backfilled"
        [ -s "${INITDB_CALLS}" ] \
            && fail "stamp-absent-nonempty: re-ran initdb" || pass "stamp-absent-nonempty: attached"
        [ -f "${PGDATA}/base-relation" ] \
            && pass "stamp-absent-nonempty: datadir preserved" \
            || fail "stamp-absent-nonempty: datadir cleared"
    fi
fi

# --- Case 11e: a stamp that cannot be written is fatal, before initdb ---
# The refusal is before the one-shot password is spent, so the create is still
# retryable; continuing would leave a volume the next boot cannot identify.
reset_state
write_handoff initialize 's3cr3t' appdb
: > "${WORK}/not-a-dir-stamp"
STAMP_OVERRIDE="${WORK}/not-a-dir-stamp/engine"
export STAMP_OVERRIDE
run_fails "stamp-unwritable"
[ -s "${INITDB_CALLS}" ] \
    && fail "stamp-unwritable: initdb ran before the stamp was recorded" \
    || pass "stamp-unwritable: refused before initdb"
grep -q 'engine stamp' "${WORK}/out" \
    && pass "stamp-unwritable: refusal names the stamp" || fail "stamp-unwritable: no refusal message"
unset STAMP_OVERRIDE

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all rds-init cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
