#!/bin/sh
set -eu

# The generated parameter files live on the data volume and reach the server
# through one baked include. Three properties make that work, and each fails
# silently rather than loudly if it breaks: the include must be read after
# every packaged drop-in, it must point at the mount rds-datadir uses, and it
# must be named so MariaDB reads it at all.
#
# The build-time half — that the directory it sits in is really the one my.cnf
# reads last — can only be checked against a built image, so the postinst owns
# it. This suite checks that the postinst still does.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
INCLUDE="zz-rds-include.cnf"
INCLUDE_DIR="etc/mysql/mariadb.conf.d"
INCLUDE_PATH="${SCRIPT_DIR}/mkosi.extra/${INCLUDE_DIR}/${INCLUDE}"
POSTINST="${SCRIPT_DIR}/mkosi.postinst.chroot"

FAILS=0
fail() { echo "FAIL: $*" >&2; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

# MariaDB reads only .cnf and .ini out of an include directory. Any other
# suffix would leave the customer's parameters silently unread.
case "${INCLUDE}" in
    *.cnf) pass "the include is named so MariaDB reads it" ;;
    *) fail "${INCLUDE} is not a name MariaDB reads out of an include directory" ;;
esac

if [ ! -f "${INCLUDE_PATH}" ]; then
    fail "${INCLUDE} is not delivered to /${INCLUDE_DIR}"
fi

# Debian's own drop-in is the file this has to outsort: MariaDB walks the
# include directory in byte order and takes the last occurrence of a setting,
# so a digit prefix would sort ahead of it and let a distribution default beat
# the platform.
packaged="50-server.cnf"
if [ "$(printf '%s\n%s\n' "${INCLUDE}" "${packaged}" | LC_ALL=C sort | tail -n 1)" = "${INCLUDE}" ]; then
    pass "the include is read after ${packaged}"
else
    fail "${packaged} is read after ${INCLUDE}, so packaged defaults win"
fi

# Ubuntu's my.cnf reads conf.d/ before mariadb.conf.d/, so which directory the
# include lands in decides whether it is read last at all. A check against a
# directory the server never reads is worse than no check, so the postinst
# derives the answer from my.cnf itself rather than asserting it here.
grep -q 'includedir.*\/etc\/mysql\/my\.cnf' "${POSTINST}" ||
    fail "the postinst does not derive which include directory my.cnf reads last"
grep -q 'LC_ALL=C sort | tail -n 1' "${POSTINST}" ||
    fail "the postinst does not assert the include is the last drop-in"

include_dir=$(sed -n 's/^!includedir[[:space:]]*//p' "${INCLUDE_PATH}")
if [ -z "${include_dir}" ]; then
    fail "${INCLUDE} declares no !includedir"
else
    # The generated files survive a VM replacement only by living on the data
    # volume, so the include has to sit under the mount rds-datadir provides.
    dropin="${SCRIPT_DIR}/mkosi.extra/etc/systemd/system/rds-datadir.service.d/10-engine.conf"
    data_mount=$(sed -n 's/.*RDS_DATA_MOUNT=\([^ ]*\).*/\1/p' "${dropin}")
    case "${include_dir}" in
        "${data_mount}"/*) pass "the include target is on the data volume at ${include_dir}" ;;
        *) fail "${include_dir} is not under the data mount ${data_mount:-<unset>}" ;;
    esac

    # An !includedir MariaDB cannot open is a fatal defaults-parsing error,
    # which would take the client down alongside the server on a boot with no
    # volume.
    grep -q "install -d .* ${include_dir}\$" "${POSTINST}" ||
        fail "the postinst does not bake ${include_dir} into the image"
fi

# A packaged file disabling TCP would leave the endpoint resolving and the
# health probe passing while nothing on the customer ENI could ever connect.
grep -q 'skip\[-_\]networking' "${POSTINST}" ||
    fail "the postinst does not assert that no configuration file disables TCP"

if [ "${FAILS}" -eq 0 ]; then
    echo "rds-mycnf: all tests passed"
    exit 0
fi
exit 1
