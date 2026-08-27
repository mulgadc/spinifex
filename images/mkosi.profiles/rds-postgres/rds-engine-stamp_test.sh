#!/bin/sh
set -eu

# The engine this profile bakes is stated in four places that have to agree:
# the stamp the postinst writes, the engine tag resolveEngineAMI selects on in
# the image catalog, the name rds-agent's layout table is keyed by, and the
# stamp rds-init puts on the data volume. A drift between them launches the
# wrong image, refuses the right one, or stamps a volume with an engine that
# did not write it.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd -- "${SCRIPT_DIR}/../../.." && pwd)
ENGINE="postgres"
IMAGE="spinifex-rds-postgres"

stamped=$(sed -n "s|^printf '\\(.*\\)\\\\n' > /etc/spinifex-rds/engine$|\\1|p" "${SCRIPT_DIR}/mkosi.postinst.chroot")
if [ "${stamped}" != "${ENGINE}" ]; then
    echo "FAIL: the postinst stamps engine '${stamped}', want '${ENGINE}'" >&2
    exit 1
fi

# The catalog entry is what the import applies as AMI tags, so it is where the
# engine tag now lives — the manifest that used to carry SYSTEM_TAG is gone.
if ! grep -A 20 "\"${IMAGE}\": {" "${REPO_ROOT}/spinifex/utils/images.go" |
    grep -q "\"engine\": \"${ENGINE}\""; then
    echo "FAIL: the ${IMAGE} catalog entry does not carry engine=${ENGINE}" >&2
    exit 1
fi

if ! grep -qE "engine[A-Za-z]+ += \"${ENGINE}\"" "${REPO_ROOT}/cmd/rds-agent/engine.go"; then
    echo "FAIL: rds-agent does not implement an engine named ${ENGINE}" >&2
    exit 1
fi

if ! grep -q "^ENGINE=\"${ENGINE}\"$" "${SCRIPT_DIR}/rds-init"; then
    echo "FAIL: rds-init does not stamp the data volume '${ENGINE}'" >&2
    grep '^ENGINE=' "${SCRIPT_DIR}/rds-init" >&2
    exit 1
fi

echo "rds-engine-stamp: all tests passed"
