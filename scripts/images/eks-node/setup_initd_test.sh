#!/bin/sh
# Every /etc/init.d script that INSTALL_FILES ships at 0644 must be chmod'd
# 0755 by setup.sh, or OpenRC refuses to enable it ("is not executable") and
# eks-node-role, running under set -eu, aborts before k3s starts.
#
# Run: sh scripts/images/eks-node/setup_initd_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
MANIFEST="${SCRIPT_DIR}/manifest.conf"
SETUP="${SETUP_SH:-${SCRIPT_DIR}/setup.sh}"

fail=0
for dst in $(grep -o ':/etc/init\.d/[A-Za-z0-9._-]*' "${MANIFEST}" | cut -d: -f2); do
    if ! grep -E '^[[:space:]]*(chmod 0755 )?.*'"${dst}"'([[:space:]\\]|$)' "${SETUP}" \
        | grep -q . ; then
        echo "FAIL: ${dst} is installed 0644 and never chmod'd 0755 in setup.sh"
        fail=1
    fi
done

if [ "${fail}" -ne 0 ]; then
    exit 1
fi
echo "PASS: every shipped init script is chmod'd 0755"
