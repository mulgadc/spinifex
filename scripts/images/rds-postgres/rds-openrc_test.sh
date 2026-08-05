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
assert_line "${WORK}/agent" "before rds-datadir rds-init postgresql"
assert_line "${WORK}/datadir" "after localmount rds-agent"
assert_line "${WORK}/datadir" "before rds-init postgresql"
assert_line "${WORK}/init" "need net"
assert_line "${WORK}/init" "after rds-agent rds-datadir"
assert_line "${WORK}/init" "before postgresql"

if grep -Fq "before rds-agent" "${WORK}/datadir"; then
    echo "FAIL: rds-datadir still starts before the bootstrap handoff" >&2
    exit 1
fi

echo "rds-openrc: dependency order tests passed"
