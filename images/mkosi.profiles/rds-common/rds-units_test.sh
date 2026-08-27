#!/bin/sh
set -eu

# The systemd half of the RDS images, replacing the OpenRC dependency tests.
# Each property here fails silently at boot rather than loudly: a unit that is
# not enabled, an engine that starts beside a failed bootstrap, or a drop-in
# whose EnvironmentFile is read before its Environment.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
PROFILES=$(CDPATH='' cd -- "${SCRIPT_DIR}/.." && pwd)
COMMON="${SCRIPT_DIR}"
UNITS="${COMMON}/mkosi.extra/etc/systemd/system"

FAILS=0
fail() { echo "FAIL: $*" >&2; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

# The engine-agnostic units, and the preset that decides they start at all.
COMMON_UNITS="rds-agent rds-bootstrap-wait rds-datadir rds-init"
PRESET="${COMMON}/mkosi.extra/usr/lib/systemd/system-preset/60-mulga-rds.preset"

for u in ${COMMON_UNITS}; do
    if [ -f "${UNITS}/${u}.service" ]; then
        pass "${u}.service ships in rds-common"
    else
        fail "${u}.service is missing from rds-common"
        continue
    fi
    # mkosi runs preset-all after every postinst, so a unit not named here is
    # left to fallthrough rather than to a stated policy.
    grep -Fxq "enable ${u}.service" "${PRESET}" ||
        fail "${u}.service is not enabled by 60-mulga-rds.preset"
done

# rds-init is what fails closed. Nothing may order the mount or the bootstrap
# ahead of the handoff the agent fetches.
grep -q '^Before=.*rds-datadir\.service' "${UNITS}/rds-agent.service" ||
    fail "rds-agent.service does not order itself before rds-datadir"
grep -q '^After=.*rds-bootstrap-wait\.service' "${UNITS}/rds-datadir.service" ||
    fail "rds-datadir.service does not wait for the bootstrap handoff"
grep -q '^After=.*rds-datadir\.service' "${UNITS}/rds-init.service" ||
    fail "rds-init.service does not run after the data volume is mounted"

# A Requires= on the agent would skip these oneshots on a failed fetch instead
# of failing them visibly, which is what the control plane reads.
for u in rds-datadir rds-init; do
    if grep -q '^Requires=.*rds-agent' "${UNITS}/${u}.service"; then
        fail "${u}.service Requires= rds-agent, so a failed fetch would skip it rather than fail it"
    fi
done

# --- per engine ------------------------------------------------------------

check_engine() {
    _profile="$1"
    _engine="$2"
    _unit="$3"
    _mount="$4"
    _user="$5"
    _dir="${PROFILES}/${_profile}"
    _extra="${_dir}/mkosi.extra/etc/systemd/system"

    # The engine must depend on the bootstrap, not merely follow it: both
    # packaged servers start on a warning when the datadir is empty.
    _dropin="${_extra}/${_unit}.d/10-rds.conf"
    [ -f "${_dropin}" ] || _dropin="${_extra}/${_unit}"
    if [ ! -f "${_dropin}" ]; then
        fail "${_profile}: no unit or drop-in declaring ${_unit}'s dependency on rds-init"
        return
    fi
    if grep -q '^Requires=rds-init\.service$' "${_dropin}" &&
        grep -q '^After=.*rds-init\.service' "${_dropin}"; then
        pass "${_profile}: ${_unit} requires and follows rds-init"
    else
        fail "${_profile}: ${_unit} does not Requires=/After= rds-init.service"
    fi

    # The engine unit has to be enabled by a preset for the same reason the
    # common four are.
    if ! grep -rqFx "enable ${_unit}" "${_dir}/mkosi.extra/usr/lib/systemd/system-preset/"; then
        fail "${_profile}: no preset enables ${_unit}"
    fi

    # U1: systemd applies Environment= and EnvironmentFile= in declaration
    # order and the later assignment wins. Inverted, the image's own default
    # beats a control-plane-delivered one and only a launch that overrides the
    # mount point would ever notice.
    for u in rds-datadir rds-init; do
        _conf="${_extra}/${u}.service.d/10-engine.conf"
        if [ ! -f "${_conf}" ]; then
            fail "${_profile}: no ${u} drop-in supplying this engine's layout"
            continue
        fi
        _last_env=$(grep -n '^Environment=' "${_conf}" | tail -n 1 | cut -d: -f1)
        _first_file=$(grep -n '^EnvironmentFile=' "${_conf}" | head -n 1 | cut -d: -f1)
        if [ -z "${_last_env}" ] || [ -z "${_first_file}" ]; then
            fail "${_profile}: ${u} drop-in is missing Environment= or EnvironmentFile="
        elif [ "${_first_file}" -gt "${_last_env}" ]; then
            pass "${_profile}: ${u} reads agent.env after its image defaults"
        else
            fail "${_profile}: ${u} drop-in reads agent.env BEFORE its Environment= defaults, so a delivered value is discarded"
        fi
    done

    # The mount point rds-datadir is told about has to be the one this engine's
    # rds-init resolves its datadir under, or the volume lands where the engine
    # is not looking and reads as an empty datadir.
    _datadir_conf="${_extra}/rds-datadir.service.d/10-engine.conf"
    grep -q "RDS_DATA_MOUNT=${_mount}\b" "${_datadir_conf}" ||
        fail "${_profile}: rds-datadir drop-in does not mount at ${_mount}"
    grep -q "RDS_ENGINE_USER=${_user}\b" "${_datadir_conf}" ||
        fail "${_profile}: rds-datadir drop-in does not own the mount as ${_user}"
    grep -q "RDS_DATA_MOUNT=${_mount}\b" "${_extra}/rds-init.service.d/10-engine.conf" ||
        fail "${_profile}: rds-init drop-in does not agree on the ${_mount} mount point"

    # rds-init must be ordered ahead of the engine from the RDS side too, so
    # the ordering survives a packaged unit this profile does not own.
    grep -q "^Before=${_unit}\$" "${_extra}/rds-init.service.d/10-engine.conf" ||
        fail "${_profile}: rds-init drop-in does not order itself before ${_unit}"

    # The stamp rds-agent builds its engine implementation from. Read-only so
    # nothing in the guest can retarget the agent at another engine.
    _postinst="${_dir}/mkosi.postinst.chroot"
    grep -q "printf '${_engine}\\\\n' > /etc/spinifex-rds/engine" "${_postinst}" ||
        fail "${_profile}: postinst does not stamp the engine as ${_engine}"
    grep -q '^chmod 0444 /etc/spinifex-rds/engine$' "${_postinst}" ||
        fail "${_profile}: the engine stamp is not made read-only"
}

check_engine rds-postgres postgres spinifex-postgresql.service /var/lib/postgresql postgres
check_engine rds-mariadb mariadb mariadb.service /var/lib/mysql mysql

if [ "${FAILS}" -eq 0 ]; then
    echo "rds-units: all tests passed"
    exit 0
fi
exit 1
