#!/bin/sh
set -eu

# rds-datadir is shared by every engine preset and refuses to guess a layout, so
# each preset's service is the only statement of where its volume mounts and who
# owns it. A preset that stopped exporting them would fail closed at boot; a
# preset that exported another engine's would mount the customer's volume where
# its engine is not looking, which reads as an empty datadir.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
IMAGES=$(CDPATH='' cd -- "${SCRIPT_DIR}/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

FAILS=0
fail() { echo "FAIL: $*" >&2; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

# Sourced rather than executed: start_pre is what OpenRC runs, and the values it
# exports are what reach the mount script.
layout() {
    (
        # shellcheck disable=SC1090
        . "${IMAGES}/$1/rds-datadir.initd"
        # Read by the sourced start_pre, which shellcheck cannot see.
        export AGENT_ENV="$2"
        start_pre
        printf '%s %s\n' "${RDS_DATA_MOUNT:-}" "${RDS_ENGINE_USER:-}"
    )
}

check_preset() {
    _preset=$1
    _mount=$2
    _user=$3

    _got=$(layout "${_preset}" "${WORK}/absent.env")
    if [ "${_got}" = "${_mount} ${_user}" ]; then
        pass "${_preset}: exports ${_mount} owned by ${_user}"
    else
        fail "${_preset}: exports '${_got}', want '${_mount} ${_user}'"
    fi

    # The control plane can move the mount point without a rebuild, so a
    # delivered value has to win over the image's own default.
    printf "RDS_DATA_MOUNT=%s\n" "${WORK}/delivered" > "${WORK}/agent.env"
    _got=$(layout "${_preset}" "${WORK}/agent.env")
    if [ "${_got}" = "${WORK}/delivered ${_user}" ]; then
        pass "${_preset}: a delivered mount point wins"
    else
        fail "${_preset}: delivered mount point ignored, got '${_got}'"
    fi
}

check_preset rds-postgres /var/lib/postgresql postgres
check_preset rds-mariadb /var/lib/mysql mysql

if [ "${FAILS}" -eq 0 ]; then
    echo "rds-datadir.initd: all presets state their layout"
    exit 0
fi
echo "FAILED: ${FAILS} case(s)"
exit 1
