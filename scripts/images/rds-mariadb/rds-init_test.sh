#!/bin/sh
# Self-contained POSIX test for the MariaDB rds-init: the initialize path
# (mariadb-install-db, the master grant, the initial database), the idempotent
# attach path, the engine stamp, the data-volume guard, TLS install and the
# parameter include. No MariaDB, no root: mariadb-install-db, mariadbd,
# mariadb-admin and the mariadb client are stubbed on PATH alongside
# install/chown (the script sets file ownership), and every path it touches is
# redirected into a temp dir via its env knobs.
#
# Run: sh scripts/images/rds-mariadb/rds-init_test.sh
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
cp "${src}" "${dest}" || exit 1
[ -n "${mode}" ] && chmod "${mode}" "${dest}"
exit 0
EOF

# chown stub: file ownership is a no-op outside the guest.
cat > "${STUBBIN}/chown" <<'EOF'
#!/bin/sh
exit 0
EOF

# ls stub: delegates normally, but lets the datadir safety tests prove that a
# failed enumeration is not mistaken for an empty directory.
cat > "${STUBBIN}/ls" <<'EOF'
#!/bin/sh
[ "${LS_FAIL:-0}" = "1" ] && exit 74
exec "${REAL_LS}" "$@"
EOF

# mariadb-install-db stub: records the call and materialises what a real one
# leaves behind. The mysql system database is what rds-init keys "already
# initialised" off.
cat > "${STUBBIN}/mariadb-install-db" <<'EOF'
#!/bin/sh
echo "mariadb-install-db $*" >> "${INSTALLDB_CALLS}"
[ "${INSTALLDB_FAIL:-0}" = "1" ] && exit 1
datadir=""
for a in "$@"; do
    case "$a" in --datadir=*) datadir="${a#--datadir=}" ;; esac
done
mkdir -p "${datadir}/mysql"
echo "global_priv" > "${datadir}/mysql/global_priv.MAD"
echo "ibdata" > "${datadir}/ibdata1"
exit 0
EOF

# mariadbd stub: the private bootstrap server. Opens its socket and pidfile and
# serves until mariadb-admin shutdown removes the socket. MARIADBD_START_FAIL is
# a server that will not come up, e.g. a datadir it cannot open;
# MARIADBD_KILL_PARENT signals rds-init from the one window between the datadir
# and the master user.
cat > "${STUBBIN}/mariadbd" <<'EOF'
#!/bin/sh
echo "mariadbd $*" >> "${MARIADBD_CALLS}"
[ "${MARIADBD_START_FAIL:-0}" = "1" ] && exit 1
sock=""; pid=""
for a in "$@"; do
    case "$a" in
        --socket=*) sock="${a#--socket=}" ;;
        --pid-file=*) pid="${a#--pid-file=}" ;;
    esac
done
if [ "${MARIADBD_KILL_PARENT:-0}" = "1" ]; then
    kill -TERM "$(cat "${KILL_PID_FILE}")"
    exit 0
fi
: > "${sock}"
echo "$$" > "${pid}"
# The bound keeps a refused shutdown from leaving a process behind the run.
_left=200
while [ -e "${sock}" ] && [ "${_left}" -gt 0 ]; do
    sleep 0.05
    _left=$((_left - 1))
done
rm -f "${pid}"
exit 0
EOF

# mariadb-admin stub: ping answers only while the socket is open, and shutdown
# closes it. ADMIN_SHUTDOWN_FAIL is a server that will not let go of the datadir.
cat > "${STUBBIN}/mariadb-admin" <<'EOF'
#!/bin/sh
sock=""; action=""
for a in "$@"; do
    case "$a" in
        --socket=*) sock="${a#--socket=}" ;;
        ping|shutdown) action="$a" ;;
    esac
done
echo "mariadb-admin ${action} $*" >> "${ADMIN_CALLS}"
[ -e "${sock}" ] || exit 1
case "${action}" in
    ping) exit 0 ;;
    shutdown)
        [ "${ADMIN_SHUTDOWN_FAIL:-0}" = "1" ] && exit 1
        rm -f "${sock}"
        exit 0
        ;;
esac
exit 1
EOF

# mariadb client stub: records the SQL it was fed on stdin plus its argv, so the
# test can prove the password rode neither an argument nor the environment.
cat > "${STUBBIN}/mariadb" <<'EOF'
#!/bin/sh
_in="${WORKDIR}/client.stdin.$$"
cat > "${_in}"
{
    echo "--- mariadb $* [env-password=${RDS_MASTER_PASSWORD:-}]"
    cat "${_in}"
} >> "${CLIENT_CALLS}"
# --batch aborts at the first error and echoes the statement it failed on,
# password and all, which is what the redaction filter exists to catch. Only the
# session carrying the password fails, so the case reaches the statement it is
# about.
if [ "${CLIENT_FAIL:-0}" = "1" ] && grep -q 'IDENTIFIED BY' "${_in}"; then
    _stmt=$(grep -m 1 'IDENTIFIED BY' "${_in}")
    rm -f "${_in}"
    # printf rather than echo: a real client echoes the statement verbatim, and
    # this shell's echo would eat the very backslash the escaping put there.
    printf 'ERROR 1064 (42000) at line 6: You have an error in your SQL syntax near "%s"\n' \
        "${_stmt}" >&2
    exit 1
fi
rm -f "${_in}"
# CLIENT_STATUS_CORRUPT=absent: the client succeeds, but rds-init cannot create
# the status file it recovers the exit code through — the shape a read-only or
# full /run gives it.
if [ "${CLIENT_STATUS_CORRUPT:-}" = "absent" ]; then
    for a in "$@"; do
        case "$a" in
            --socket=*) mkdir -p "$(dirname "${a#--socket=}")/client.status" ;;
        esac
    done
fi
exit 0
EOF

# cat stub: delegates normally, but lets the read-back of the client's exit
# status return what a torn write or an I/O error would. rds-init's other uses of
# cat read a file or stdin and are untouched.
cat > "${STUBBIN}/cat" <<'EOF'
#!/bin/sh
case "${1:-}" in
    *client.status)
        case "${CLIENT_STATUS_CORRUPT:-}" in
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
DATADIR="${DATA_MOUNT}/data"
CONF_DIR="${DATA_MOUNT}/conf.d"
PARAM_FILE="${CONF_DIR}/10-rds-parameters.cnf"
SERVING_FILE="${CONF_DIR}/10-rds-parameters.serving"
PLATFORM_FILE="${CONF_DIR}/90-rds-init.cnf"
SENTINEL="${DATADIR}/rds-bootstrap-incomplete"
RECEIPT_DIR="${DATA_MOUNT}/.spinifex-rds/bootstrap"
RECEIPT="${RECEIPT_DIR}/receipt.env"
STAMP="${DATA_MOUNT}/.spinifex-rds/engine"
HANDOFF="${WORK}/run/spinifex-rds"
SECURE_FILE_DIR="${WORK}/mysql-files"
MOUNTS="${WORK}/mounts"
KILL_PID_FILE="${WORK}/rds-init.pid"
WORKDIR="${WORK}"
export KILL_PID_FILE WORKDIR

INSTALLDB_CALLS="${WORK}/install-db.calls"
MARIADBD_CALLS="${WORK}/mariadbd.calls"
ADMIN_CALLS="${WORK}/admin.calls"
CLIENT_CALLS="${WORK}/client.calls"
export INSTALLDB_CALLS MARIADBD_CALLS ADMIN_CALLS CLIENT_CALLS

# write_handoff <mode> <password> <dbname>: lay down the bootstrap.env rds-agent
# would have written. An empty password omits the field, as `attach` does.
# MASTER_USER overrides the master user name, for the reserved-name cases.
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
        # handoff intact and reach the checks that exist for it. An embedded
        # quote is closed, escaped and reopened, exactly as the agent does.
        [ -n "$2" ] && printf "RDS_MASTER_PASSWORD='%s'\n" "$(printf '%s' "$2" | sed "s/'/'\\\\''/g")"
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

# The resolved parameter group rds-agent hands over, in the engine-neutral
# `name = 'value'` rendering and with no group header — rds-init is what adds the
# one MariaDB needs. general_log is in it to prove the bootstrap server does not
# read it: the password is interpolated into a statement, and a customer's
# logging parameter must not be able to write that statement to a log.
write_parameters() {
    mkdir -p "${HANDOFF}"
    {
        echo "# Resolved parameter group, written by rds-agent."
        echo "max_connections = '100'"
        echo "general_log = 'on'"
        echo "character_set_server = 'utf8mb4'"
        echo "collation_server = 'utf8mb4_general_ci'"
    } > "${HANDOFF}/parameters.conf"
}

# reset_state clears everything the previous case left behind: a fresh data
# volume, an attached mount, and empty stub-call logs.
reset_state() {
    rm -rf "${WORK}/data" "${WORK}/run" "${WORK}/log" "${SECURE_FILE_DIR}"
    mkdir -p "${DATA_MOUNT}"
    printf '/dev/vdb %s ext4 rw,relatime 0 0\n' "${DATA_MOUNT}" > "${MOUNTS}"
    : > "${INSTALLDB_CALLS}"
    : > "${MARIADBD_CALLS}"
    : > "${ADMIN_CALLS}"
    : > "${CLIENT_CALLS}"
    unset INSTALLDB_FAIL CLIENT_FAIL LS_FAIL RDS_ALLOW_LOCAL_DATADIR MASTER_USER || true
    unset PENDING PAYLOAD_ID DB_ID RECEIPT_DIR_OVERRIDE CLIENT_STATUS_CORRUPT STAMP_OVERRIDE || true
    unset MARIADBD_START_FAIL MARIADBD_KILL_PARENT ADMIN_SHUTDOWN_FAIL SYNC_KILL_PARENT || true
}

# run_env: invoke a command with every path knob pointed into the temp dir. The
# probe interval is driven down so the socket wait costs milliseconds rather than
# a real second per case.
run_env() {
    env RDS_MARIADB_BIN="${STUBBIN}" \
        RDS_DATA_MOUNT="${DATA_MOUNT}" \
        RDS_HANDOFF_DIR="${HANDOFF}" \
        RDS_SOCKET_DIR="${WORK}/run/mysqld" \
        RDS_BOOTSTRAP_DIR="${WORK}/run/rds-init" \
        RDS_LOG_DIR="${WORK}/log" \
        RDS_SECURE_FILE_DIR="${SECURE_FILE_DIR}" \
        RDS_MOUNTS_FILE="${MOUNTS}" \
        RDS_RECEIPT_DIR="${RECEIPT_DIR_OVERRIDE:-${RECEIPT_DIR}}" \
        RDS_ENGINE_STAMP="${STAMP_OVERRIDE:-${STAMP}}" \
        RDS_ALLOW_LOCAL_DATADIR="${RDS_ALLOW_LOCAL_DATADIR:-0}" \
        RDS_BOOTSTRAP_POLL=0.05 \
        RDS_BOOTSTRAP_PROBES=200 \
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
    grep -q 'mariadb-install-db' "${INSTALLDB_CALLS}" \
        && pass "initialize: the datadir was created" || fail "initialize: no mariadb-install-db"
    grep -q -- '--skip-test-db' "${INSTALLDB_CALLS}" \
        && pass "initialize: no test database installed" || fail "initialize: no --skip-test-db"
    grep -q -- '--auth-root-authentication-method=socket' "${INSTALLDB_CALLS}" \
        && pass "initialize: root authenticates through unix_socket" \
        || fail "initialize: root was not given socket authentication"
    grep -q -- "--datadir=${DATADIR}" "${INSTALLDB_CALLS}" \
        && pass "initialize: the datadir is one level inside the mount" \
        || fail "initialize: wrong datadir"

    # The mount point itself is not the datadir: a failed bootstrap clears the
    # datadir, and the include directory, the stamp and the receipt outlive it.
    [ -d "${CONF_DIR}" ] && [ -d "${DATADIR}" ] \
        && pass "initialize: the include directory sits beside the datadir" \
        || fail "initialize: the include directory is not beside the datadir"

    grep -q -- '--skip-networking' "${MARIADBD_CALLS}" \
        && pass "initialize: bootstrap server is socket-only" \
        || fail "initialize: bootstrap server listened on TCP"
    grep -q -- "--socket=${WORK}/run/rds-init/" "${MARIADBD_CALLS}" \
        && pass "initialize: bootstrap socket is in the private directory" \
        || fail "initialize: bootstrap socket outside the private directory"
    grep -q 'shutdown' "${ADMIN_CALLS}" \
        && pass "initialize: bootstrap server stopped" || fail "initialize: bootstrap server left running"

    grep -q "CREATE USER IF NOT EXISTS 'mulgamaster'@'%'" "${CLIENT_CALLS}" \
        && pass "initialize: master user created for the customer ENI" \
        || fail "initialize: no CREATE USER"
    grep -q "IDENTIFIED BY 's3cr3t'" "${CLIENT_CALLS}" \
        && pass "initialize: master password applied" || fail "initialize: password not applied"
    grep -q 'env-password=s3cr3t' "${CLIENT_CALLS}" \
        && fail "initialize: the password was also handed over in the environment" \
        || pass "initialize: the password rode stdin only"
    grep -q 'CREATE DATABASE IF NOT EXISTS `appdb`' "${CLIENT_CALLS}" \
        && pass "initialize: initial database created" || fail "initialize: no CREATE DATABASE"
    # A database keeps the character set it was created under, and the private
    # server runs on --no-defaults rather than on the resolved group.
    grep -q 'CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci' "${CLIENT_CALLS}" \
        && pass "initialize: the database takes the group's character set" \
        || fail "initialize: the database was created under the compiled default"

    # Withheld deliberately: FILE is arbitrary host file access as the mysql user,
    # SUPER reaches SET GLOBAL and plugin loading, and SHUTDOWN stops the server
    # underneath the control plane's lifecycle.
    for privilege in SUPER FILE SHUTDOWN 'ALL PRIVILEGES'; do
        grep -q "${privilege}" "${CLIENT_CALLS}" \
            && fail "initialize: the master grant includes ${privilege}" \
            || pass "initialize: master holds no ${privilege}"
    done
    grep -q 'GRANT SELECT, INSERT, UPDATE, DELETE' "${CLIENT_CALLS}" \
        && pass "initialize: the grant is enumerated" || fail "initialize: no enumerated grant"
    grep -q "WITH GRANT OPTION" "${CLIENT_CALLS}" \
        && pass "initialize: master can delegate its own privileges" \
        || fail "initialize: master cannot grant"
    grep -q "DELETE FROM mysql.global_priv WHERE User = ''" "${CLIENT_CALLS}" \
        && pass "initialize: anonymous accounts removed" || fail "initialize: anonymous accounts left"
    grep -q 'DROP DATABASE IF EXISTS test' "${CLIENT_CALLS}" \
        && pass "initialize: the test database is dropped" || fail "initialize: test database left"

    # Every session carries the guard, so a statement added later cannot end up
    # outside it. `grep -c` exits non-zero on no match, which `set -e` would turn
    # into an aborted run in place of a reported failure.
    guard_count=$(grep -cF "SET GLOBAL general_log = 0;" "${CLIENT_CALLS}" || true)
    session_count=$(grep -c '^--- mariadb ' "${CLIENT_CALLS}" || true)
    [ "${guard_count}" = "${session_count}" ] \
        && pass "log-guard: all ${session_count} bootstrap sessions guarded" \
        || fail "log-guard: ${guard_count} of ${session_count} bootstrap sessions guarded"
    grep -qF "SET SESSION sql_mode = 'NO_ENGINE_SUBSTITUTION';" "${CLIENT_CALLS}" \
        && pass "log-guard: the escaping mode is pinned" \
        || fail "log-guard: the session mode the escaping assumes is not pinned"
    # The customer's group carries general_log = on. Nothing in the bootstrap
    # reads it, so their parameter cannot log the statement carrying the
    # password, and a static value they set cannot stop this server starting and
    # spend the one-shot password with it.
    for stub in "${MARIADBD_CALLS}" "${INSTALLDB_CALLS}" "${CLIENT_CALLS}"; do
        grep -q -- '--no-defaults' "${stub}" \
            && pass "log-guard: $(basename "${stub}" .calls) ignores the installed configuration" \
            || fail "log-guard: $(basename "${stub}" .calls) read the customer's parameters"
    done

    head -n 1 "${PARAM_FILE}" | grep -qx '\[mysqld\]' \
        && pass "initialize: the parameter file carries the group header" \
        || fail "initialize: a setting before any group is a fatal parse error"
    grep -q "max_connections = '100'" "${PARAM_FILE}" \
        && pass "initialize: resolved parameters installed" || fail "initialize: no parameter include"
    cmp -s "${PARAM_FILE}" "${SERVING_FILE}" \
        && pass "initialize: the serving copy is what the engine starts on" \
        || fail "initialize: the serving copy differs from the installed file"
    case "${SERVING_FILE}" in
        *.cnf) fail "initialize: the serving copy is read as a second settings file" ;;
        *) pass "initialize: the serving copy is not read by the include" ;;
    esac

    grep -q "^port = 6543" "${PLATFORM_FILE}" \
        && pass "initialize: delivered port applied" || fail "initialize: port not applied"
    grep -q "^datadir = ${DATADIR}" "${PLATFORM_FILE}" \
        && pass "initialize: datadir pinned by the platform" || fail "initialize: no datadir"
    grep -q "^bind_address = 0.0.0.0" "${PLATFORM_FILE}" \
        && pass "initialize: the engine listens for the customer ENI" \
        || fail "initialize: no bind_address"
    grep -q "^default_storage_engine = InnoDB" "${PLATFORM_FILE}" \
        && pass "initialize: tables land in the engine snapshots recover" \
        || fail "initialize: default_storage_engine not pinned"
    grep -q "^skip-log-bin" "${PLATFORM_FILE}" \
        && pass "initialize: binary logging off" || fail "initialize: binary logging left on"
    grep -q "^secure_file_priv = ${SECURE_FILE_DIR}" "${PLATFORM_FILE}" \
        && pass "initialize: secure_file_priv names a directory, not the empty string" \
        || fail "initialize: secure_file_priv unset or empty"
    [ -d "${SECURE_FILE_DIR}" ] \
        && pass "initialize: the secure_file_priv directory exists" \
        || fail "initialize: secure_file_priv points at nothing"
    grep -q "^ssl_cert = " "${PLATFORM_FILE}" && grep -q "^ssl_key = " "${PLATFORM_FILE}" \
        && pass "initialize: TLS offered" || fail "initialize: TLS not configured"
    grep -q "require_secure_transport" "${PLATFORM_FILE}" \
        && fail "initialize: TLS enforced before the enforcement phase" \
        || pass "initialize: TLS offered rather than enforced"
    # Removed outright in 11.8, so naming it is a startup failure rather than an
    # override — every instance would boot-loop on it.
    grep -q "innodb_buffer_pool_instances" "${PLATFORM_FILE}" \
        && fail "initialize: the platform file names a setting 11.8 removed" \
        || pass "initialize: no removed InnoDB settings"

    [ -e "${SENTINEL}" ] \
        && fail "initialize: incomplete-bootstrap sentinel left behind" \
        || pass "initialize: sentinel cleared once the master user exists"
fi

# --- Case 2: the master password does not outlive its use ---
grep -q '^RDS_MASTER_PASSWORD=' "${HANDOFF}/bootstrap.env" \
    && fail "consume: password still in the handoff" || pass "consume: password dropped from the handoff"
grep -q '^RDS_MASTER_USERNAME=mulgamaster' "${HANDOFF}/bootstrap.env" \
    && pass "consume: the rest of the config is kept" || fail "consume: non-secret fields lost"
[ -f "${HANDOFF}/server.key" ] \
    && fail "consume: serving key left in the handoff" || pass "consume: serving key moved out of the handoff"
[ -f "${WORK}/run/mysqld/tls/server.key" ] \
    && pass "tls: key installed on tmpfs, not the data volume" || fail "tls: key not installed"
# Anything on the data volume rides into every snapshot of it.
if grep -rq 's3cr3t' "${DATA_MOUNT}" 2>/dev/null; then
    fail "consume: the master password is on the data volume"
    grep -rl 's3cr3t' "${DATA_MOUNT}" 2>/dev/null
else
    pass "consume: no password anywhere on the data volume"
fi
grep -rq 'KEY' "${DATA_MOUNT}" 2>/dev/null \
    && fail "tls: the serving key reached the data volume" || pass "tls: data volume free of the key"
# A client option file holding the password would hand it to anything that runs
# the mariadb client as root, and would survive the boot.
grep -q '\.my\.cnf' "${SCRIPT}" \
    && fail "consume: rds-init writes a client option file" || pass "consume: no /root/.my.cnf written"

# --- Case 3: second boot (attach) is idempotent ---
: > "${INSTALLDB_CALLS}"; : > "${CLIENT_CALLS}"; : > "${MARIADBD_CALLS}"
write_handoff attach '' appdb
write_parameters
if run_ok "attach"; then
    [ -s "${INSTALLDB_CALLS}" ] \
        && fail "attach: re-initialised an existing datadir" || pass "attach: initialisation skipped"
    [ -s "${CLIENT_CALLS}" ] \
        && fail "attach: re-applied the master user" || pass "attach: no user/database work"
    [ -s "${MARIADBD_CALLS}" ] \
        && fail "attach: started a bootstrap server it had no work for" \
        || pass "attach: no bootstrap server"
    grep -q "^ssl_cert = " "${PLATFORM_FILE}" \
        && pass "attach: TLS survives a handoff without a new cert" || fail "attach: TLS turned off"
    cmp -s "${PARAM_FILE}" "${SERVING_FILE}" \
        && pass "attach: the serving copy tracks the boot" \
        || fail "attach: the serving copy is stale"
fi

# --- Case 3a: a handoff with no parameters keeps the installed ones ---
: > "${INSTALLDB_CALLS}"
rm -f "${HANDOFF}/parameters.conf"
write_handoff attach '' appdb
if run_ok "no-parameters"; then
    grep -q "max_connections = '100'" "${PARAM_FILE}" \
        && pass "no-parameters: the installed group is kept" \
        || fail "no-parameters: the instance was reset to engine defaults"
    cmp -s "${PARAM_FILE}" "${SERVING_FILE}" \
        && pass "no-parameters: the serving copy still matches" \
        || fail "no-parameters: the serving copy drifted"
fi

# --- Case 3b: a failing client must not echo the password to the console ---
# rds-init.initd sends this script's output to /dev/console, which the host
# captures off ttyS0, and the client under --batch echoes the statement it failed
# on. The redaction must not swallow the failure: the datadir is cleared.
reset_state
write_handoff initialize 's3cr3t' appdb
write_parameters
CLIENT_FAIL=1
export CLIENT_FAIL
run_fails "console-redaction"
grep -q 's3cr3t' "${WORK}/out" \
    && fail "console-redaction: the console capture carries the master password" \
    || pass "console-redaction: no password on the console"
grep -q '\[REDACTED\]' "${WORK}/out" \
    && pass "console-redaction: the redaction is marked" \
    || fail "console-redaction: the client's stderr never reached the console"
grep -q 'ERROR 1064' "${WORK}/out" \
    && pass "console-redaction: the diagnostic itself survives" \
    || fail "console-redaction: the redaction swallowed the diagnostic"
[ -e "${DATADIR}/mysql" ] \
    && fail "console-redaction: datadir kept after a failed master bootstrap" \
    || pass "console-redaction: the failure still propagates"
unset CLIENT_FAIL

# --- Case 3c: the client's status must cross the redaction pipeline or fail closed ---
# The status file is how the client's exit code leaves the subshell, POSIX sh
# having no pipefail. A torn write leaves it created but empty, and `return ""`
# is fatal in the guest's ash: rds-init would die past the datadir sweep and
# before any diagnostic.
for corruption in empty garbage; do
    reset_state
    write_handoff initialize 's3cr3t' appdb
    CLIENT_STATUS_CORRUPT="${corruption}"
    export CLIENT_STATUS_CORRUPT
    run_fails "client-status-${corruption}"
    grep -q 'exit status could not be read back' "${WORK}/out" \
        && pass "client-status-${corruption}: names the unreadable status rather than blaming the client" \
        || fail "client-status-${corruption}: no diagnostic naming the status file"
    grep -q 'Illegal number\|numeric argument required' "${WORK}/out" \
        && fail "client-status-${corruption}: an unvalidated status reached return" \
        || pass "client-status-${corruption}: nothing unvalidated reached return"
    [ -e "${DATADIR}/mysql" ] \
        && fail "client-status-${corruption}: datadir kept after an unrecoverable status" \
        || pass "client-status-${corruption}: datadir cleared"
    grep -q 'shutdown' "${ADMIN_CALLS}" \
        && pass "client-status-${corruption}: bootstrap server stopped" \
        || fail "client-status-${corruption}: bootstrap server left holding the datadir"
    unset CLIENT_STATUS_CORRUPT
done

# --- Case 3d: a status file that could never be written is not the client's fault ---
reset_state
write_handoff initialize 's3cr3t' appdb
CLIENT_STATUS_CORRUPT=absent
export CLIENT_STATUS_CORRUPT
run_fails "client-status-absent"
grep -q 'exit status was never recorded' "${WORK}/out" \
    && pass "client-status-absent: names the missing status file" \
    || fail "client-status-absent: no diagnostic distinguishing it from a client failure"
[ -e "${DATADIR}/mysql" ] \
    && fail "client-status-absent: datadir kept" || pass "client-status-absent: datadir cleared"
unset CLIENT_STATUS_CORRUPT

# --- Case 3e: a multi-line master password is refused before it is spent ---
reset_state
write_handoff initialize 'top
secret' appdb
run_fails "multiline-password"
[ -s "${INSTALLDB_CALLS}" ] \
    && fail "multiline-password: the datadir was created before the password was refused" \
    || pass "multiline-password: refused before the datadir was created"
grep -q 'spans more than one line' "${WORK}/out" \
    && pass "multiline-password: refusal names the reason" \
    || fail "multiline-password: no refusal message"

# --- Case 3f: a password holding a quote and a backslash is escaped, not mangled ---
# ValidateMasterUserPassword permits both, and the mariadb client has no
# equivalent of psql's :'password' quoting, so the literal is built here.
reset_state
write_handoff initialize "pa'ss\\word" appdb
write_parameters
if run_ok "password-escaping"; then
    grep -qF "IDENTIFIED BY 'pa''ss\\\\word'" "${CLIENT_CALLS}" \
        && pass "password-escaping: the quote is doubled and the backslash escaped" \
        || fail "password-escaping: the literal is not correctly escaped"
    grep -qF "IDENTIFIED BY 'pa'ss" "${CLIENT_CALLS}" \
        && fail "password-escaping: an unescaped quote closed the literal early" \
        || pass "password-escaping: the literal is not closed early"
fi

# The escaped form is what a failing statement echoes, and it is trivially
# unescaped by anyone reading the console, so it has to be redacted too.
reset_state
CLIENT_FAIL=1
export CLIENT_FAIL
write_handoff initialize "pa'ss\\word" appdb
run_fails "password-escaping-redaction"
grep -qF "pa''ss\\\\word" "${WORK}/out" \
    && fail "password-escaping-redaction: the escaped password reached the console" \
    || pass "password-escaping-redaction: the escaped form is redacted too"
grep -qF "pa'ss\\word" "${WORK}/out" \
    && fail "password-escaping-redaction: the raw password reached the console" \
    || pass "password-escaping-redaction: the raw form is redacted"
grep -q '\[REDACTED\]' "${WORK}/out" \
    && pass "password-escaping-redaction: the statement was echoed and caught" \
    || fail "password-escaping-redaction: nothing was redacted, so this proves nothing"
unset CLIENT_FAIL

# --- Case 4: an empty datadir in attach mode means the data volume is missing ---
reset_state
write_handoff attach '' appdb
run_fails "attach-empty"
[ -s "${INSTALLDB_CALLS}" ] \
    && fail "attach-empty: created an empty database over missing data" || pass "attach-empty: nothing created"

# --- Case 5: the datadir must be on the attached data volume ---
reset_state
: > "${MOUNTS}"
write_handoff initialize 's3cr3t' appdb
run_fails "no-volume"
[ -s "${INSTALLDB_CALLS}" ] \
    && fail "no-volume: initialised on the boot volume" || pass "no-volume: nothing created"

RDS_ALLOW_LOCAL_DATADIR=1
export RDS_ALLOW_LOCAL_DATADIR
if run_ok "local-datadir"; then
    grep -q 'mariadb-install-db' "${INSTALLDB_CALLS}" \
        && pass "local-datadir: explicit override initialises anyway" || fail "local-datadir: nothing created"
fi
unset RDS_ALLOW_LOCAL_DATADIR

# --- Case 6: a failed initialisation leaves nothing that looks initialised ---
reset_state
write_handoff initialize 's3cr3t' appdb
INSTALLDB_FAIL=1
export INSTALLDB_FAIL
run_fails "install-db-fail"
[ -e "${DATADIR}/mysql" ] \
    && fail "install-db-fail: half-written datadir kept" || pass "install-db-fail: datadir cleared"
unset INSTALLDB_FAIL

# --- Case 6a: a failed initialisation over a NON-empty datadir must not delete it ---
# A datadir whose system tables were lost over otherwise intact data takes the
# initialise path, and "directory not empty" is one way the install then fails.
reset_state
write_handoff initialize 's3cr3t' appdb
mkdir -p "${DATADIR}"
echo 'customer table data' > "${DATADIR}/appdb.ibd"
printf 'mariadb\n' > "${WORK}/stamp-seed"
mkdir -p "$(dirname "${STAMP}")"
cp "${WORK}/stamp-seed" "${STAMP}"
INSTALLDB_FAIL=1
export INSTALLDB_FAIL
run_fails "install-db-fail-nonempty"
[ -f "${DATADIR}/appdb.ibd" ] \
    && pass "install-db-fail-nonempty: pre-existing datadir preserved" \
    || fail "install-db-fail-nonempty: pre-existing datadir deleted"
grep -q 'refusing to clear' "${WORK}/out" \
    && pass "install-db-fail-nonempty: refusal explains why" \
    || fail "install-db-fail-nonempty: no refusal message"
unset INSTALLDB_FAIL

# --- Case 6b: a failed emptiness probe must abort before anything is created ---
reset_state
write_handoff initialize 's3cr3t' appdb
LS_FAIL=1
export LS_FAIL
run_fails "datadir-probe-fail"
unset LS_FAIL
[ -s "${INSTALLDB_CALLS}" ] \
    && fail "datadir-probe-fail: initialised after the probe failed" \
    || pass "datadir-probe-fail: nothing created"
grep -q 'could not inspect datadir' "${WORK}/out" \
    && pass "datadir-probe-fail: refusal explains why" || fail "datadir-probe-fail: no refusal message"

# --- Case 6c: the bootstrap server never starts ---
# The datadir already carries the system tables and the password is spent, so
# leaving it would attach on the next boot and serve a database whose master user
# was never created.
reset_state
write_handoff initialize 's3cr3t' appdb
MARIADBD_START_FAIL=1
export MARIADBD_START_FAIL
run_fails "bootstrap-nostart"
[ -e "${DATADIR}/mysql" ] \
    && fail "bootstrap-nostart: datadir kept after the server failed to start" \
    || pass "bootstrap-nostart: datadir cleared"
grep -q 'exited before it opened' "${WORK}/out" \
    && pass "bootstrap-nostart: refusal names the server" || fail "bootstrap-nostart: no refusal message"
unset MARIADBD_START_FAIL

# --- Case 6d: a bootstrap server that will not stop is a failure ---
# It holds the datadir, so the mariadb service cannot start for the life of the
# boot. The master user exists by then, so the datadir itself must survive.
reset_state
write_handoff initialize 's3cr3t' appdb
ADMIN_SHUTDOWN_FAIL=1
export ADMIN_SHUTDOWN_FAIL
run_fails "stop-fail"
grep -q 'did not stop' "${WORK}/out" \
    && pass "stop-fail: reported rather than swallowed" || fail "stop-fail: no failure message"
[ -e "${DATADIR}/mysql" ] \
    && pass "stop-fail: bootstrapped datadir kept" || fail "stop-fail: cleared a datadir with a master user"
[ -e "${SENTINEL}" ] \
    && fail "stop-fail: sentinel kept although the master user exists" \
    || pass "stop-fail: sentinel cleared"
[ -e "${RECEIPT}" ] \
    && pass "stop-fail: the completed bootstrap is still proven" \
    || fail "stop-fail: no receipt for a bootstrap that completed"
unset ADMIN_SHUTDOWN_FAIL

# --- Case 6e: a SIGTERM between the datadir and the master user clears it ---
# An EXIT trap does not run on a signal in a POSIX shell, so a shutdown, an ACPI
# powerdown from a stop arriving while the instance is still creating, or a
# host-side force-stop would otherwise leave a datadir with no master user.
reset_state
write_handoff initialize 's3cr3t' appdb
MARIADBD_KILL_PARENT=1
export MARIADBD_KILL_PARENT
run_signalled_fails "sigterm-midbootstrap"
[ -e "${DATADIR}/mysql" ] \
    && fail "sigterm-midbootstrap: datadir kept after a signal mid-bootstrap" \
    || pass "sigterm-midbootstrap: datadir cleared"
[ -s "${CLIENT_CALLS}" ] \
    && fail "sigterm-midbootstrap: the master user work ran after the signal" \
    || pass "sigterm-midbootstrap: stopped before the master user"
unset MARIADBD_KILL_PARENT

# --- Case 6f: a datadir carrying the sentinel is refused, not started ---
# No trap survives a crash or a SIGKILL, so the sentinel is the only record that
# the bootstrap did not finish. It says the master user is missing, not that the
# volume is empty, so the refusal must not clear anything.
reset_state
write_handoff initialize 's3cr3t' appdb
if run_ok "sentinel-setup"; then
    echo 'customer table data' > "${DATADIR}/appdb.ibd"
    : > "${SENTINEL}"
    write_handoff attach '' appdb
    run_fails "sentinel-refused"
    [ -f "${DATADIR}/appdb.ibd" ] \
        && pass "sentinel-refused: datadir preserved" || fail "sentinel-refused: datadir cleared"
    grep -q 'recover the data volume out of band' "${WORK}/out" \
        && pass "sentinel-refused: refusal points at out-of-band recovery" \
        || fail "sentinel-refused: no refusal message"
fi

# --- Case 7: a completed bootstrap leaves a receipt proving it ---
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
        "${DATADIR}"/*) fail "receipt: written inside the datadir the traps clear" ;;
        *) pass "receipt: written outside the datadir" ;;
    esac
fi

# --- Case 7a: a receipt that cannot be written is fatal ---
reset_state
PENDING=1 PAYLOAD_ID=bp-beta
export PENDING PAYLOAD_ID
write_handoff initialize 's3cr3t' appdb
: > "${WORK}/not-a-dir"
RECEIPT_DIR_OVERRIDE="${WORK}/not-a-dir/bootstrap"
export RECEIPT_DIR_OVERRIDE
run_fails "receipt-fail"
[ -e "${DATADIR}/mysql" ] \
    && fail "receipt-fail: datadir kept although its bootstrap cannot be proven" \
    || pass "receipt-fail: datadir cleared"
grep -q 'bootstrap receipt' "${WORK}/out" \
    && pass "receipt-fail: reported rather than swallowed" || fail "receipt-fail: no failure message"
unset RECEIPT_DIR_OVERRIDE

# --- Case 7b: a sweep between the receipt and the retired traps takes both ---
reset_state
PENDING=1 PAYLOAD_ID=bp-gamma
export PENDING PAYLOAD_ID
write_handoff initialize 's3cr3t' appdb
SYNC_KILL_PARENT=1
export SYNC_KILL_PARENT
run_signalled_fails "receipt-swept"
[ -e "${DATADIR}/mysql" ] \
    && fail "receipt-swept: datadir kept after a signal mid-bootstrap" \
    || pass "receipt-swept: datadir cleared"
[ -e "${RECEIPT}" ] \
    && fail "receipt-swept: receipt survived the datadir it vouches for" \
    || pass "receipt-swept: receipt cleared with the datadir"
unset SYNC_KILL_PARENT

# --- Case 7c: pending payload, initialised datadir, no receipt -> fail closed ---
reset_state
PENDING=1 PAYLOAD_ID=bp-delta DB_ID=db-delta
export PENDING PAYLOAD_ID DB_ID
write_handoff initialize 's3cr3t' appdb
if run_ok "pending-setup"; then
    echo 'customer table data' > "${DATADIR}/appdb.ibd"
    rm -f "${RECEIPT}"
    run_fails "pending-no-receipt"
    [ -f "${DATADIR}/appdb.ibd" ] \
        && pass "pending-no-receipt: datadir preserved" || fail "pending-no-receipt: datadir cleared"
    grep -q 'master user cannot be proven' "${WORK}/out" \
        && pass "pending-no-receipt: refusal names the real reason" \
        || fail "pending-no-receipt: no refusal message"

    # --- Case 7d: the same boot with a matching receipt attaches ---
    : > "${INSTALLDB_CALLS}"; : > "${CLIENT_CALLS}"
    mkdir -p "${RECEIPT_DIR}"
    printf 'RDS_RECEIPT_PAYLOAD_ID=bp-delta\nRDS_RECEIPT_DB_INSTANCE_IDENTIFIER=db-delta\n' > "${RECEIPT}"
    if run_ok "pending-with-receipt"; then
        [ -s "${INSTALLDB_CALLS}" ] \
            && fail "pending-with-receipt: re-initialised" || pass "pending-with-receipt: attached"
    fi

    # --- Case 7e: a receipt naming another instance is treated as absent ---
    # The receipt is on the data volume, so it rides along in every snapshot of
    # it and a restored volume carries the source instance's receipt.
    printf 'RDS_RECEIPT_PAYLOAD_ID=bp-delta\nRDS_RECEIPT_DB_INSTANCE_IDENTIFIER=db-somebody-else\n' > "${RECEIPT}"
    run_fails "pending-foreign-receipt"
    [ -f "${DATADIR}/appdb.ibd" ] \
        && pass "pending-foreign-receipt: datadir preserved" || fail "pending-foreign-receipt: datadir cleared"
fi
unset PENDING PAYLOAD_ID DB_ID

# --- Case 7f: no pending payload leaves the attach path unchanged ---
reset_state
write_handoff initialize 's3cr3t' appdb
if run_ok "legacy-setup"; then
    rm -rf "${RECEIPT_DIR}"
    : > "${INSTALLDB_CALLS}"
    write_handoff attach '' appdb
    if run_ok "legacy-attach"; then
        [ -s "${INSTALLDB_CALLS}" ] \
            && fail "legacy-attach: re-initialised" || pass "legacy-attach: attached without a receipt"
    fi
fi

# --- Case 8: no serving cert -> TLS off rather than a failed start ---
reset_state
write_handoff initialize 's3cr3t' ''
if run_ok "no-cert"; then
    grep -q '^ssl_cert' "${PLATFORM_FILE}" \
        && fail "no-cert: points at a cert that was never delivered" || pass "no-cert: no cert paths"
    grep -q 'CREATE DATABASE' "${CLIENT_CALLS}" \
        && fail "no-dbname: created a database without a DBName" || pass "no-dbname: no initial database"
fi

# --- Case 9: no handoff at all ---
reset_state
rm -rf "${HANDOFF}"
run_fails "no-handoff"

# --- Case 10: a master username the platform reserves ---
# The master user is created without SUPER, so one named after an account
# mariadb-install-db already made would either fail outright or rewrite the
# account rds-init and rds-agent reach the server through — on a datadir that
# bootstraps once. The set and the case-insensitive matching are the control
# plane's: a name it refuses must not reach the install here.
for reserved in root mysql rdsadmin public Root mariadb.sys; do
    reset_state
    MASTER_USER="${reserved}"
    write_handoff initialize 's3cr3t' appdb
    run_fails "reserved-master-${reserved}"
    [ -s "${INSTALLDB_CALLS}" ] \
        && fail "reserved-master-${reserved}: initialised before the name was refused" \
        || pass "reserved-master-${reserved}: refused before the datadir was created"
    unset MASTER_USER
done

# --- Case 10a: a master username that is not safe to interpolate ---
# It is built into CREATE USER by the shell, the client having no identifier
# quoting of its own.
for badname in 'my-user' 'my user' "o'brien" '1user' 'user;DROP' \
    'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; do
    reset_state
    MASTER_USER="${badname}"
    write_handoff initialize 's3cr3t' appdb
    run_fails "bad-master-${badname}"
    [ -s "${INSTALLDB_CALLS}" ] \
        && fail "bad-master-${badname}: initialised before the name was refused" \
        || pass "bad-master-${badname}: refused before the datadir was created"
    unset MASTER_USER
done

# --- Case 11: an initial database name the control plane would have refused ---
# MariaDB maps a database name onto a directory name and the client cannot quote
# an identifier for us, so this rule is the barrier rather than defence in depth.
for badname in 'my-db' 'my db' 'my/db' 'my.db' '1db' 'db`x' \
    'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; do
    reset_state
    write_handoff initialize 's3cr3t' "${badname}"
    run_fails "bad-dbname-${badname}"
    [ -s "${INSTALLDB_CALLS}" ] \
        && fail "bad-dbname-${badname}: initialised before the name was refused" \
        || pass "bad-dbname-${badname}: refused before the datadir was created"
done

# The rule is a rejection of malformed names, not of every name: the boundary
# length and an underscore have to keep working.
reset_state
write_handoff initialize 's3cr3t' \
    'a_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
write_parameters
if run_ok "dbname-at-the-limit"; then
    grep -q 'CREATE DATABASE' "${CLIENT_CALLS}" \
        && pass "dbname-at-the-limit: accepted" || fail "dbname-at-the-limit: no CREATE DATABASE"
fi

# --- Case 12: the data volume records which engine wrote it ---
reset_state
write_handoff initialize 's3cr3t' appdb
if run_ok "stamp-absent-empty"; then
    [ "$(cat "${STAMP}" 2>/dev/null)" = "mariadb" ] \
        && pass "stamp-absent-empty: fresh volume stamped mariadb" \
        || fail "stamp-absent-empty: no stamp written at initialisation"
    case "${STAMP}" in
        "${DATADIR}"/*) fail "stamp-absent-empty: written inside the datadir the traps clear" ;;
        *) pass "stamp-absent-empty: written outside the datadir" ;;
    esac

    # --- Case 12a: a matching stamp attaches ---
    : > "${INSTALLDB_CALLS}"
    write_handoff attach '' appdb
    if run_ok "stamp-match"; then
        [ -s "${INSTALLDB_CALLS}" ] \
            && fail "stamp-match: re-initialised" || pass "stamp-match: attached"
    fi

    # --- Case 12b: another engine's stamp is fatal and touches nothing ---
    # A PostgreSQL data volume mounts cleanly here and its datadir reads as
    # uninitialised, so without this rds-init would bootstrap beside the
    # customer's data and serve an empty database that passes every probe.
    echo 'customer table data' > "${DATADIR}/appdb.ibd"
    printf 'postgres\n' > "${STAMP}"
    : > "${INSTALLDB_CALLS}"
    run_fails "stamp-mismatch"
    [ -s "${INSTALLDB_CALLS}" ] \
        && fail "stamp-mismatch: initialised over another engine's volume" \
        || pass "stamp-mismatch: nothing created"
    [ -f "${DATADIR}/appdb.ibd" ] \
        && pass "stamp-mismatch: datadir preserved" || fail "stamp-mismatch: datadir cleared"
    [ "$(cat "${STAMP}")" = "postgres" ] \
        && pass "stamp-mismatch: the other engine's stamp is left as it stands" \
        || fail "stamp-mismatch: stamp overwritten"
    grep -q "holds a 'postgres' datadir" "${WORK}/out" \
        && pass "stamp-mismatch: refusal names both engines" \
        || fail "stamp-mismatch: no refusal naming the stamped engine"

    # --- Case 12c: a stamp that cannot be read is not an absent one ---
    rm -f "${STAMP}"
    mkdir "${STAMP}"
    run_fails "stamp-unreadable"
    grep -q 'could not be read' "${WORK}/out" \
        && pass "stamp-unreadable: refusal names the unreadable stamp" \
        || fail "stamp-unreadable: no refusal message"
    rmdir "${STAMP}"

    # --- Case 12d: an unstamped datadir with data in it is refused ---
    # PostgreSQL backfills here, because it had unstamped volumes in the field.
    # MariaDB never did, so a datadir with content and no stamp is by definition
    # not ours, and stamping it would rubber-stamp the volume the check exists
    # to refuse.
    rm -f "${STAMP}"
    : > "${INSTALLDB_CALLS}"
    run_fails "stamp-absent-nonempty"
    [ -s "${INSTALLDB_CALLS}" ] \
        && fail "stamp-absent-nonempty: initialised over an unidentified datadir" \
        || pass "stamp-absent-nonempty: nothing created"
    [ -f "${DATADIR}/appdb.ibd" ] \
        && pass "stamp-absent-nonempty: datadir preserved" \
        || fail "stamp-absent-nonempty: datadir cleared"
    [ -e "${STAMP}" ] \
        && fail "stamp-absent-nonempty: an unidentified volume was stamped ours" \
        || pass "stamp-absent-nonempty: no stamp written"
    grep -q 'no mariadb engine stamp' "${WORK}/out" \
        && pass "stamp-absent-nonempty: refusal names the missing stamp" \
        || fail "stamp-absent-nonempty: no refusal message"
fi

# --- Case 12e: a stamp that cannot be written is fatal, before initialisation ---
reset_state
write_handoff initialize 's3cr3t' appdb
: > "${WORK}/not-a-dir-stamp"
STAMP_OVERRIDE="${WORK}/not-a-dir-stamp/engine"
export STAMP_OVERRIDE
run_fails "stamp-unwritable"
[ -s "${INSTALLDB_CALLS}" ] \
    && fail "stamp-unwritable: initialised before the stamp was recorded" \
    || pass "stamp-unwritable: refused before the datadir was created"
grep -q 'engine stamp' "${WORK}/out" \
    && pass "stamp-unwritable: refusal names the stamp" || fail "stamp-unwritable: no refusal message"
unset STAMP_OVERRIDE

if [ "${FAILS}" -eq 0 ]; then
    echo "PASS: all rds-init cases"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
