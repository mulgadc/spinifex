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
export REAL_LS

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
    mkdir -p "$@"
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
# reproduces that so the master-bootstrap failure path is exercisable.
[ "${PSQL_FAIL:-0}" = "1" ] && exit 3
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
write_handoff() {
    mkdir -p "${HANDOFF}"
    {
        echo "RDS_MODE=$1"
        echo "RDS_MASTER_USERNAME=${MASTER_USER:-mulgamaster}"
        [ -n "$2" ] && echo "RDS_MASTER_PASSWORD=$2"
        [ -n "$3" ] && echo "RDS_DB_NAME=$3"
        echo "RDS_PORT=6543"
    } > "${HANDOFF}/bootstrap.env"
}

write_tls() {
    mkdir -p "${HANDOFF}"
    echo "CERT" > "${HANDOFF}/server.crt"
    echo "KEY" > "${HANDOFF}/server.key"
}

write_parameters() {
    mkdir -p "${HANDOFF}"
    echo "shared_buffers = 128MB" > "${HANDOFF}/parameters.conf"
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

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all rds-init cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
