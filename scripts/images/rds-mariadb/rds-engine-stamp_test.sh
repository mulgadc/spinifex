#!/bin/sh
set -eu

# The engine this preset bakes, and the series it pins, are stated in places that
# have to agree: the stamp setup.sh writes, the manifest tags resolveEngineAMI
# selects on, the apk pin the packages are installed from, and the engine value
# the control plane launches instances with. A drift between them launches the
# wrong image, refuses the right one, or reports a version the VM is not running.
#
# This preset carries no bootstrap yet, so the datadir stamp and the agent's
# layout table are checked by the postgres preset's copy of this test only.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd -- "${SCRIPT_DIR}/../../.." && pwd)
ENGINE="mariadb"
ENGINE_GO="${REPO_ROOT}/spinifex/handlers/rds/engine_mariadb.go"

FAILS=0
fail() { echo "FAIL: $*" >&2; FAILS=$((FAILS + 1)); }

stamped=$(sed -n "s|^printf '\\(.*\\)\\\\n' > /etc/spinifex-rds/engine\$|\\1|p" "${SCRIPT_DIR}/setup.sh")
if [ "${stamped}" != "${ENGINE}" ]; then
    fail "setup.sh stamps engine '${stamped}', want '${ENGINE}'"
fi

if ! grep -q "engine=${ENGINE} " "${SCRIPT_DIR}/manifest.conf"; then
    fail "manifest.conf SYSTEM_TAG does not carry engine=${ENGINE}"
    grep '^SYSTEM_TAG' "${SCRIPT_DIR}/manifest.conf" >&2
fi

if ! grep -q "Name: *\"${ENGINE}\"" "${ENGINE_GO}"; then
    fail "the control plane has no engine named ${ENGINE}"
fi

# The tag is what an instance's recorded EngineVersion resolves an AMI by, so it
# has to be the same series the control plane pins and the packages install.
version=$(sed -n 's/^[[:space:]]*MajorVersion: *"\([^"]*\)".*/\1/p' "${ENGINE_GO}")
if [ -z "${version}" ]; then
    fail "could not read MajorVersion from ${ENGINE_GO}"
else
    if ! grep -q "engine-version=${version} " "${SCRIPT_DIR}/manifest.conf"; then
        fail "manifest.conf SYSTEM_TAG does not carry engine-version=${version}"
        grep '^SYSTEM_TAG' "${SCRIPT_DIR}/manifest.conf" >&2
    fi
    # Alpine's package name carries no version, so this fuzzy pin is the only
    # thing standing between a rebuild and a silent series bump.
    if ! grep -q "APK_PACKAGES=.*${ENGINE}=~${version} " "${SCRIPT_DIR}/manifest.conf"; then
        fail "manifest.conf does not pin the ${ENGINE} packages to =~${version}"
        grep '^APK_PACKAGES' "${SCRIPT_DIR}/manifest.conf" >&2
    fi
fi

if [ "${FAILS}" -eq 0 ]; then
    echo "rds-engine-stamp: all tests passed"
    exit 0
fi
exit 1
