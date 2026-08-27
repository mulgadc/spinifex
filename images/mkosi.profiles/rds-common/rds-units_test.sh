#!/bin/sh
set -eu

# The systemd half of the RDS images, replacing the OpenRC dependency tests.
# Each property here fails silently at boot rather than loudly: a unit that is
# not enabled, an engine that starts beside a failed bootstrap, or a drop-in
# whose EnvironmentFile is read before its Environment.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
PROFILES=$(CDPATH='' cd -- "${SCRIPT_DIR}/.." && pwd)
REPO_ROOT=$(CDPATH='' cd -- "${SCRIPT_DIR}/../../.." && pwd)
COMMON="${SCRIPT_DIR}"
UNITS="${COMMON}/mkosi.extra/etc/systemd/system"
CATALOG="${REPO_ROOT}/spinifex/utils/images.go"
AGENT_LAYOUTS="${REPO_ROOT}/cmd/rds-agent/engine.go"

FAILS=0
fail() { echo "FAIL: $*" >&2; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

# One entry of the catalog map, brace to brace. grep -A with a fixed window
# silently drops the tags once a field is added to the Images struct.
catalog_entry() {
    awk -v key="\"$1\": {" 'index($0, key) { inside = 1 } inside { print } inside && /^\t},$/ { exit }' "${CATALOG}"
}

# One entry of rds-agent's engineLayouts, same reason.
agent_layout() {
    awk -v key="$1: {" 'index($0, key) { inside = 1 } inside { print } inside && /^\t},$/ { exit }' "${AGENT_LAYOUTS}"
}

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

# Every unit that can outrun systemd's 90s default has to state its own bound,
# so the script is what decides the timeout. Unset, a volume attaching inside
# rds-datadir's designed tolerance is killed mid-mkfs on a customer's disk.
for u in rds-bootstrap-wait rds-datadir rds-init; do
    _bound=$(sed -n 's/^TimeoutStartSec=//p' "${UNITS}/${u}.service")
    case "${_bound}" in
        '' | *[!0-9]*) fail "${u}.service states no numeric TimeoutStartSec, so it inherits the 90s default" ;;
        *)
            if [ "${_bound}" -gt 120 ]; then
                pass "${u}.service bounds itself at ${_bound}s, above its script's own wait"
            else
                fail "${u}.service TimeoutStartSec=${_bound} is not above the 120s its script waits"
            fi
            ;;
    esac
done

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
    _image="$6"
    _layout_key="$7"
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

    # The engine name has to agree in four places or a launch resolves the wrong
    # image, refuses the right one, or stamps a volume with an engine that did
    # not write it.
    catalog_entry "${_image}" | grep -q "\"engine\": \"${_engine}\"" ||
        fail "${_profile}: the ${_image} catalog entry does not carry engine=${_engine}"
    [ -n "$(agent_layout "${_layout_key}")" ] ||
        fail "${_profile}: rds-agent has no engineLayouts entry keyed ${_layout_key}"
    grep -q "^ENGINE=\"${_engine}\"\$" "${_dir}/rds-init" ||
        fail "${_profile}: rds-init does not stamp the data volume '${_engine}'"

    # The series is what an EngineVersion request resolves an AMI by, so the
    # build assertion, the published tag and the control plane's own catalog
    # have to be the same number.
    _series=$(sed -n 's/^\(WANT_SERIES\|PG_VERSION\)=\(.*\)$/\2/p' "${_postinst}" | head -n 1)
    if [ -z "${_series}" ]; then
        fail "${_profile}: postinst asserts no engine series"
    else
        catalog_entry "${_image}" | grep -q "\"engine-version\": \"${_series}\"" ||
            fail "${_profile}: the ${_image} catalog entry does not publish engine-version=${_series}"
        _engine_go="${REPO_ROOT}/spinifex/handlers/rds/engine_${_engine}.go"
        grep -q "MajorVersion: *\"${_series}\"" "${_engine_go}" ||
            fail "${_profile}: ${_engine_go} does not carry MajorVersion ${_series}, so a valid EngineVersion resolves no AMI"
    fi

    # mkosi builds UEFI-only. A catalog entry saying bios launches the instance
    # with the wrong firmware and it never boots, with nothing failing earlier.
    catalog_entry "${_image}" | grep -q 'BootMode: *"uefi"' ||
        fail "${_profile}: the ${_image} catalog entry is not BootMode uefi"

    # rds-agent's layout table is the fifth place this image's paths are stated,
    # and the only one no build assertion reaches: a mismatch boots and serves
    # while every password rotate and parameter apply fails.
    agent_layout "${_layout_key}" | grep -q "service: *\"${_unit}\"" ||
        fail "${_profile}: rds-agent's ${_layout_key} layout does not drive ${_unit}"
    agent_layout "${_layout_key}" | grep -q "dataMount: *\"${_mount}\"" ||
        fail "${_profile}: rds-agent's ${_layout_key} layout does not agree on the ${_mount} mount point"
    agent_layout "${_layout_key}" | grep -q "osUser: *\"${_user}\"" ||
        fail "${_profile}: rds-agent's ${_layout_key} layout does not run as ${_user}"
}

check_engine rds-postgres postgres spinifex-postgresql.service /var/lib/postgresql postgres \
    spinifex-rds-postgres enginePostgres
check_engine rds-mariadb mariadb mariadb.service /var/lib/mysql mysql \
    spinifex-rds-mariadb engineMariaDB

# The two paths rds-agent shells out to that no postinst can assert, because
# each is stated once in the image and once in Go with nothing between them.
agent_layout enginePostgres | grep -q "binDir: *\"/usr/lib/postgresql/18/bin\"" ||
    fail "rds-agent's postgres binDir does not match RDS_PG_BIN in the rds-postgres drop-in"
grep -q 'RDS_PG_BIN=/usr/lib/postgresql/18/bin' \
    "${PROFILES}/rds-postgres/mkosi.extra/etc/systemd/system/rds-init.service.d/10-engine.conf" ||
    fail "the rds-postgres rds-init drop-in does not set RDS_PG_BIN to the path rds-agent uses"

_want_pid=$(sed -n 's/^WANT_PIDFILE=//p' "${PROFILES}/rds-mariadb/mkosi.postinst.chroot")
agent_layout engineMariaDB | grep -q "pidFile: *\"${_want_pid}\"" ||
    fail "rds-agent reads a pidfile the rds-mariadb postinst does not assert (${_want_pid}), so the health probe reports every healthy instance down"

if [ "${FAILS}" -eq 0 ]; then
    echo "rds-units: all tests passed"
    exit 0
fi
exit 1
