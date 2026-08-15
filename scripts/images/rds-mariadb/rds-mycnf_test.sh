#!/bin/sh
set -eu

# The generated parameter files live on the data volume and reach the server
# through one baked include. Three properties make that work, and each fails
# silently rather than loudly if it breaks: the include must be read after every
# packaged drop-in, it must point at the mount rds-datadir uses, and it must be
# named so MariaDB reads it at all.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
INCLUDE="zz-rds-include.cnf"

FAILS=0
fail() { echo "FAIL: $*" >&2; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

# MariaDB reads only .cnf and .ini out of an include directory. Any other suffix
# would leave the customer's parameters silently unread.
case "${INCLUDE}" in
    *.cnf) pass "the include is named so MariaDB reads it" ;;
    *) fail "${INCLUDE} is not a name MariaDB reads out of an include directory" ;;
esac

if [ ! -f "${SCRIPT_DIR}/${INCLUDE}" ]; then
    fail "${INCLUDE} is missing from the preset"
fi
if ! grep -q "${INCLUDE}:/etc/my.cnf.d/${INCLUDE}" "${SCRIPT_DIR}/manifest.conf"; then
    fail "manifest.conf does not install ${INCLUDE} into /etc/my.cnf.d"
fi

# Alpine's own drop-in is the file this has to outsort: MariaDB walks the include
# directory in byte order and takes the last occurrence of a setting, so a digit
# prefix would sort ahead of it and let a distribution default beat the platform.
packaged="mariadb-server.cnf"
if [ "$(printf '%s\n%s\n' "${INCLUDE}" "${packaged}" | LC_ALL=C sort | tail -n 1)" = "${INCLUDE}" ]; then
    pass "the include is read after ${packaged}"
else
    fail "${packaged} is read after ${INCLUDE}, so packaged defaults win"
fi

# setup.sh asserts the same property against the real directory at build time.
grep -q 'LC_ALL=C sort | tail -n 1' "${SCRIPT_DIR}/setup.sh" ||
    fail "setup.sh does not assert the include is the last drop-in"

include_dir=$(sed -n 's/^!includedir[[:space:]]*//p' "${SCRIPT_DIR}/${INCLUDE}")
if [ -z "${include_dir}" ]; then
    fail "${INCLUDE} declares no !includedir"
else
    # The generated files survive a VM replacement only by living on the data
    # volume, so the include has to sit under the mount rds-datadir provides.
    data_mount=$(sed -n 's/^[[:space:]]*RDS_DATA_MOUNT="\([^"]*\)".*/\1/p' "${SCRIPT_DIR}/rds-datadir.initd")
    case "${include_dir}" in
        "${data_mount}"/*) pass "the include target is on the data volume at ${include_dir}" ;;
        *) fail "${include_dir} is not under the data mount ${data_mount:-<unset>}" ;;
    esac

    # An !includedir MariaDB cannot open is a fatal defaults-parsing error, which
    # would take the client down alongside the server on a boot with no volume.
    grep -q "install -d .* ${include_dir}\$" "${SCRIPT_DIR}/setup.sh" ||
        fail "setup.sh does not bake ${include_dir} into the image"
fi

# The packaged drop-in disables TCP. Left in place, the endpoint resolves and the
# health probe passes while nothing on the customer ENI can ever connect.
grep -q 's/\^skip-networking\$/' "${SCRIPT_DIR}/setup.sh" ||
    fail "setup.sh does not disable the packaged skip-networking"
grep -q "skip\[-_\]networking" "${SCRIPT_DIR}/setup.sh" ||
    fail "setup.sh does not assert that no configuration file disables TCP"

if [ "${FAILS}" -eq 0 ]; then
    echo "rds-mycnf: all tests passed"
    exit 0
fi
exit 1
