#!/bin/sh
set -eu

# The engine this preset bakes is stated in four places that have to agree: the
# stamp setup.sh writes, the manifest tag resolveEngineAMI selects on, the name
# rds-agent's layout table is keyed by, and the stamp rds-init puts on the data
# volume. A drift between them launches the wrong image, refuses the right one,
# or stamps a volume with an engine that did not write it.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd -- "${SCRIPT_DIR}/../../.." && pwd)
ENGINE="postgres"

stamped=$(sed -n "s|^printf '\\(.*\\)\\\\n' > /etc/spinifex-rds/engine\$|\\1|p" "${SCRIPT_DIR}/setup.sh")
if [ "${stamped}" != "${ENGINE}" ]; then
    echo "FAIL: setup.sh stamps engine '${stamped}', want '${ENGINE}'" >&2
    exit 1
fi

if ! grep -q "engine=${ENGINE} " "${SCRIPT_DIR}/manifest.conf"; then
    echo "FAIL: manifest.conf SYSTEM_TAG does not carry engine=${ENGINE}" >&2
    grep '^SYSTEM_TAG' "${SCRIPT_DIR}/manifest.conf" >&2
    exit 1
fi

if ! grep -q "enginePostgres = \"${ENGINE}\"" "${REPO_ROOT}/cmd/rds-agent/engine.go"; then
    echo "FAIL: rds-agent does not implement an engine named ${ENGINE}" >&2
    exit 1
fi

if ! grep -q "^ENGINE=\"${ENGINE}\"\$" "${SCRIPT_DIR}/rds-init"; then
    echo "FAIL: rds-init does not stamp the data volume '${ENGINE}'" >&2
    grep '^ENGINE=' "${SCRIPT_DIR}/rds-init" >&2
    exit 1
fi

echo "rds-engine-stamp: all tests passed"
