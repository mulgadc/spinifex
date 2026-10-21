# shellcheck shell=bash
# distro.sh — host-OS capability probes. Each prints its answer, or names every
# candidate it tried on stderr and returns 1, also when it finds both of a pair.
# setup.sh and node-reset.sh carry inline copies; distro_test.sh keeps them equal.

# Reads unit files off disk, not through systemctl, so it answers in a chroot.
# DISTRO_ROOT prefixes every host path the probes read; tests set it.
distro_unit_exists() {
    local dir
    for dir in /etc/systemd/system /run/systemd/system /usr/lib/systemd/system /lib/systemd/system; do
        if [ -e "${DISTRO_ROOT:-}$dir/$1" ] || [ -L "${DISTRO_ROOT:-}$dir/$1" ]; then
            return 0
        fi
    done
    return 1
}

distro_pkg_manager() {
    local apt=false dnf=false
    command -v apt-get >/dev/null 2>&1 && apt=true
    command -v dnf >/dev/null 2>&1 && dnf=true
    if $apt && $dnf; then
        echo "distro.sh: package manager: found both apt-get and dnf; cannot tell which one manages this host" >&2
        return 1
    fi
    if $apt; then echo apt; return 0; fi
    if $dnf; then echo dnf; return 0; fi
    echo "distro.sh: package manager: found neither apt-get nor dnf on PATH" >&2
    return 1
}

# The OVS umbrella unit: openvswitch-switch.service on Debian, openvswitch.service on EL.
distro_ovs_unit() {
    local deb=false el=false
    distro_unit_exists openvswitch-switch.service && deb=true
    distro_unit_exists openvswitch.service && el=true
    if $deb && $el; then
        echo "distro.sh: OVS unit: found both openvswitch-switch.service and openvswitch.service; cannot tell which one owns OVS" >&2
        return 1
    fi
    if $deb; then echo openvswitch-switch.service; return 0; fi
    if $el; then echo openvswitch.service; return 0; fi
    echo "distro.sh: OVS unit: found neither openvswitch-switch.service nor openvswitch.service; is Open vSwitch installed?" >&2
    return 1
}

# "split": Debian's ovn-central plus separate NB and SB DB units.
# "lone-northd": EL's single ovn-northd.service, which starts both DB servers.
distro_ovn_central_layout() {
    local split=false lone=false unit missing=""
    for unit in ovn-central.service ovn-ovsdb-server-nb.service ovn-ovsdb-server-sb.service; do
        if distro_unit_exists "$unit"; then
            split=true
        else
            missing="${missing:+$missing, }$unit"
        fi
    done
    if distro_unit_exists ovn-northd.service && [ -f "${DISTRO_ROOT:-}/etc/sysconfig/ovn" ]; then
        lone=true
    fi
    if $split && $lone; then
        echo "distro.sh: OVN central layout: found both the split units (ovn-central.service) and a lone ovn-northd.service with /etc/sysconfig/ovn; cannot tell which one owns the OVN databases" >&2
        return 1
    fi
    if $split; then
        if [ -n "$missing" ]; then
            echo "distro.sh: OVN central layout: ovn-central is installed but $missing is missing" >&2
            return 1
        fi
        echo split
        return 0
    fi
    if $lone; then echo lone-northd; return 0; fi
    echo "distro.sh: OVN central layout: found neither ovn-central.service nor ovn-northd.service with /etc/sysconfig/ovn; is OVN central installed?" >&2
    return 1
}

# The trust-store update command and its local anchor directory, space-separated.
distro_ca_trust() {
    local deb=false el=false
    command -v update-ca-certificates >/dev/null 2>&1 && deb=true
    command -v update-ca-trust >/dev/null 2>&1 && el=true
    if $deb && $el; then
        echo "distro.sh: CA trust: found both update-ca-certificates and update-ca-trust; cannot tell which trust store TLS clients read" >&2
        return 1
    fi
    if $deb; then echo "update-ca-certificates /usr/local/share/ca-certificates"; return 0; fi
    if $el; then echo "update-ca-trust /etc/pki/ca-trust/source/anchors"; return 0; fi
    echo "distro.sh: CA trust: found neither update-ca-certificates nor update-ca-trust on PATH" >&2
    return 1
}

distro_ovn_logrotate() {
    local deb=false el=false
    [ -f "${DISTRO_ROOT:-}/etc/logrotate.d/ovn-common" ] && deb=true
    [ -f "${DISTRO_ROOT:-}/etc/logrotate.d/ovn" ] && el=true
    if $deb && $el; then
        echo "distro.sh: OVN logrotate: found both /etc/logrotate.d/ovn-common and /etc/logrotate.d/ovn; cannot tell which one the OVN packages own" >&2
        return 1
    fi
    if $deb; then echo /etc/logrotate.d/ovn-common; return 0; fi
    if $el; then echo /etc/logrotate.d/ovn; return 0; fi
    echo "distro.sh: OVN logrotate: found neither /etc/logrotate.d/ovn-common nor /etc/logrotate.d/ovn; is OVN installed?" >&2
    return 1
}

# The OVS and OVN units the given unit files depend on or order against that
# are not installed, one per line; prints nothing when all are. Fails only
# when a unit file cannot be read.
distro_missing_ovs_deps() {
    local file line dep missing=""
    for file in "$@"; do
        if [ ! -r "${DISTRO_ROOT:-}$file" ]; then
            echo "distro.sh: OVS dependencies: cannot read $file" >&2
            return 1
        fi
        while IFS= read -r line || [ -n "$line" ]; do
            case "$line" in
                After=* | Before=* | Wants=* | Requires=* | Requisite=* | BindsTo=* | PartOf=* | Upholds=*) ;;
                *) continue ;;
            esac
            for dep in ${line#*=}; do
                case "$dep" in
                    ovs* | ovn* | openvswitch*) ;;
                    *) continue ;;
                esac
                case " $missing " in
                    *" $dep "*) continue ;;
                esac
                distro_unit_exists "$dep" || missing="${missing:+$missing }$dep"
            done
        done <"${DISTRO_ROOT:-}$file"
    done
    for dep in $missing; do echo "$dep"; done
}
