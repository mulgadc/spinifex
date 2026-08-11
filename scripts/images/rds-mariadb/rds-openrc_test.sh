#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

capture_dependencies() {
    _script=$1
    (
        need() { echo "need $*"; }
        after() { echo "after $*"; }
        before() { echo "before $*"; }
        # shellcheck disable=SC1090
        . "${SCRIPT_DIR}/${_script}"
        depend
    )
}

assert_line() {
    _file=$1
    _line=$2
    grep -Fxq "${_line}" "${_file}" || {
        echo "FAIL: ${_file} missing dependency: ${_line}" >&2
        cat "${_file}" >&2
        exit 1
    }
}

capture_dependencies rds-agent.initd > "${WORK}/agent"
capture_dependencies rds-datadir.initd > "${WORK}/datadir"
capture_dependencies rds-init.initd > "${WORK}/init"

assert_line "${WORK}/agent" "need net"
assert_line "${WORK}/agent" "after localmount"
assert_line "${WORK}/agent" "before rds-datadir rds-init mariadb"
assert_line "${WORK}/datadir" "after localmount rds-agent"
assert_line "${WORK}/datadir" "before rds-init mariadb"
assert_line "${WORK}/init" "need net"
assert_line "${WORK}/init" "after rds-agent rds-datadir"
assert_line "${WORK}/init" "before mariadb"

if grep -Fq "before rds-agent" "${WORK}/datadir"; then
    echo "FAIL: rds-datadir still starts before the bootstrap handoff" >&2
    exit 1
fi

# Ordering alone is not enough for this engine: Alpine's mariadb service only
# warns about an empty datadir and starts anyway, so a failed rds-init would be
# answered by launching mysqld_safe against an unbootstrapped volume. OpenRC
# reads the dependency out of the service's conf.d file.
grep -Fq 'rc_need="rds-init"' "${SCRIPT_DIR}/mariadb.confd" || {
    echo "FAIL: /etc/conf.d/mariadb does not make the engine need rds-init" >&2
    exit 1
}
grep -Fq "mariadb.confd:/etc/conf.d/mariadb" "${SCRIPT_DIR}/manifest.conf" || {
    echo "FAIL: manifest.conf does not install mariadb.confd" >&2
    exit 1
}
for svc in rds-init mariadb; do
    grep -qE "^ENABLE_SERVICES=.*[\" ]${svc}[\" ]" "${SCRIPT_DIR}/manifest.conf" || {
        echo "FAIL: manifest.conf does not enable ${svc}" >&2
        grep '^ENABLE_SERVICES' "${SCRIPT_DIR}/manifest.conf" >&2
        exit 1
    }
done

echo "rds-openrc: dependency order tests passed"
