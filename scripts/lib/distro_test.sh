#!/bin/sh
# distro_test.sh — tests for the host-OS probes and the inline copies of them.
#
# Every probe runs against a fixture tree (DISTRO_ROOT) and a PATH holding only
# stub commands, so the result never depends on the distro running the test.
set -eu

LIB_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SCRIPTS_DIR=$(dirname -- "$LIB_DIR")
DISTRO="$LIB_DIR/distro.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- lib/distro.sh"

# fixture <name> — a fresh empty host root and stub bin dir.
fixture() {
    ROOT="$TMP/$1/root"
    BIN="$TMP/$1/bin"
    mkdir -p "$ROOT" "$BIN"
}
unit() { mkdir -p "$ROOT/usr/lib/systemd/system" && : > "$ROOT/usr/lib/systemd/system/$1"; }
file() { mkdir -p "$ROOT$(dirname "$1")" && : > "$ROOT$1"; }
cmd() { printf '#!/bin/sh\nexit 0\n' > "$BIN/$1" && chmod +x "$BIN/$1"; }

# probe <fn> [arg...] — runs one probe in bash against the current fixture,
# with only the stub dir on PATH. Sets OUT, ERR and RC.
probe() {
    RC=0
    OUT=$(DISTRO_ROOT="$ROOT" PATH="$BIN" /bin/bash -c '. "$1"; shift; "$@"' _ "$DISTRO" "$@" 2>"$TMP/err") || RC=$?
    ERR=$(cat "$TMP/err")
}

# expect_ok <desc> <want>; expect_fail <desc> <word>... — every word must appear
# in the error, which is how "names every candidate" is checked.
expect_ok() {
    if [ "$RC" = 0 ] && [ "$OUT" = "$2" ]; then pass "$1"; else fail "$1 (rc=$RC out='$OUT' err='$ERR')"; fi
}
expect_fail() {
    desc=$1; shift
    if [ "$RC" = 0 ]; then fail "$desc (succeeded with '$OUT')"; return; fi
    for word in "$@"; do
        case "$ERR" in
            *"$word"*) ;;
            *) fail "$desc (error does not name $word: '$ERR')"; return ;;
        esac
    done
    pass "$desc"
}

# --------------------------------------------------------------------------
# Package manager.
# --------------------------------------------------------------------------
fixture pm-apt; cmd apt-get; probe distro_pkg_manager
expect_ok "package manager: apt-get alone is apt" apt
fixture pm-dnf; cmd dnf; probe distro_pkg_manager
expect_ok "package manager: dnf alone is dnf" dnf
fixture pm-both; cmd apt-get; cmd dnf; probe distro_pkg_manager
expect_fail "package manager: both fails naming both" apt-get dnf
fixture pm-none; probe distro_pkg_manager
expect_fail "package manager: neither fails naming both" apt-get dnf

# --------------------------------------------------------------------------
# OVS umbrella unit. Unit files are found under any systemd unit directory, and
# a masked unit (a symlink to /dev/null) still counts as installed.
# --------------------------------------------------------------------------
fixture ovs-deb; unit openvswitch-switch.service; probe distro_ovs_unit
expect_ok "OVS unit: Debian name" openvswitch-switch.service
fixture ovs-el; unit openvswitch.service; probe distro_ovs_unit
expect_ok "OVS unit: EL name" openvswitch.service
fixture ovs-masked; mkdir -p "$ROOT/etc/systemd/system"
ln -s /dev/null "$ROOT/etc/systemd/system/openvswitch.service"; probe distro_ovs_unit
expect_ok "OVS unit: a masked unit in /etc counts" openvswitch.service
fixture ovs-lib; mkdir -p "$ROOT/lib/systemd/system"
: > "$ROOT/lib/systemd/system/openvswitch-switch.service"; probe distro_ovs_unit
expect_ok "OVS unit: found under /lib on an unmerged-/usr host" openvswitch-switch.service
fixture ovs-both; unit openvswitch-switch.service; unit openvswitch.service; probe distro_ovs_unit
expect_fail "OVS unit: both fails naming both" openvswitch-switch.service openvswitch.service
fixture ovs-none; probe distro_ovs_unit
expect_fail "OVS unit: neither fails naming both" openvswitch-switch.service openvswitch.service

# --------------------------------------------------------------------------
# OVN central layout. Both distros ship ovn-northd.service, so the unit alone
# must not decide it: Debian's split layout carries one too.
# --------------------------------------------------------------------------
split_units() { unit ovn-central.service; unit ovn-ovsdb-server-nb.service; unit ovn-ovsdb-server-sb.service; unit ovn-northd.service; }

fixture ovn-split; split_units; probe distro_ovn_central_layout
expect_ok "OVN layout: Debian split units" split
fixture ovn-lone; unit ovn-northd.service; file /etc/sysconfig/ovn; probe distro_ovn_central_layout
expect_ok "OVN layout: EL lone ovn-northd" lone-northd
fixture ovn-both; split_units; file /etc/sysconfig/ovn; probe distro_ovn_central_layout
expect_fail "OVN layout: both fails naming both" ovn-central.service ovn-northd.service /etc/sysconfig/ovn
fixture ovn-partial; unit ovn-central.service; unit ovn-ovsdb-server-nb.service; probe distro_ovn_central_layout
expect_fail "OVN layout: a partial split install names the missing unit" ovn-ovsdb-server-sb.service
fixture ovn-northd-only; unit ovn-northd.service; probe distro_ovn_central_layout
expect_fail "OVN layout: ovn-northd without /etc/sysconfig/ovn is not EL" ovn-central.service ovn-northd.service /etc/sysconfig/ovn
fixture ovn-none; probe distro_ovn_central_layout
expect_fail "OVN layout: neither fails naming both" ovn-central.service ovn-northd.service

# --------------------------------------------------------------------------
# CA trust.
# --------------------------------------------------------------------------
fixture ca-deb; cmd update-ca-certificates; probe distro_ca_trust
expect_ok "CA trust: Debian command and anchor dir" "update-ca-certificates /usr/local/share/ca-certificates"
fixture ca-el; cmd update-ca-trust; probe distro_ca_trust
expect_ok "CA trust: EL command and anchor dir" "update-ca-trust /etc/pki/ca-trust/source/anchors"
fixture ca-both; cmd update-ca-certificates; cmd update-ca-trust; probe distro_ca_trust
expect_fail "CA trust: both fails naming both" update-ca-certificates update-ca-trust
fixture ca-none; probe distro_ca_trust
expect_fail "CA trust: neither fails naming both" update-ca-certificates update-ca-trust

# --------------------------------------------------------------------------
# chrony config file.
# --------------------------------------------------------------------------
fixture chrony-deb; file /etc/chrony/chrony.conf; probe distro_chrony_conf
expect_ok "chrony config: Debian file" /etc/chrony/chrony.conf
fixture chrony-el; file /etc/chrony.conf; probe distro_chrony_conf
expect_ok "chrony config: EL file" /etc/chrony.conf
fixture chrony-both; file /etc/chrony/chrony.conf; file /etc/chrony.conf; probe distro_chrony_conf
expect_fail "chrony config: both fails naming both" /etc/chrony/chrony.conf /etc/chrony.conf
fixture chrony-none; probe distro_chrony_conf
expect_fail "chrony config: neither fails naming both" /etc/chrony/chrony.conf /etc/chrony.conf

# --------------------------------------------------------------------------
# OVN logrotate file.
# --------------------------------------------------------------------------
fixture lr-deb; file /etc/logrotate.d/ovn-common; probe distro_ovn_logrotate
expect_ok "OVN logrotate: Debian file" /etc/logrotate.d/ovn-common
fixture lr-el; file /etc/logrotate.d/ovn; probe distro_ovn_logrotate
expect_ok "OVN logrotate: EL file" /etc/logrotate.d/ovn
fixture lr-both; file /etc/logrotate.d/ovn-common; file /etc/logrotate.d/ovn; probe distro_ovn_logrotate
expect_fail "OVN logrotate: both fails naming both" /etc/logrotate.d/ovn-common /etc/logrotate.d/ovn
fixture lr-none; probe distro_ovn_logrotate
expect_fail "OVN logrotate: neither fails naming both" /etc/logrotate.d/ovn-common /etc/logrotate.d/ovn

# --------------------------------------------------------------------------
# OVS logrotate file.
# --------------------------------------------------------------------------
fixture ovslr-deb; file /etc/logrotate.d/openvswitch-switch; probe distro_ovs_logrotate
expect_ok "OVS logrotate: Debian file" /etc/logrotate.d/openvswitch-switch
fixture ovslr-el; file /etc/logrotate.d/openvswitch; probe distro_ovs_logrotate
expect_ok "OVS logrotate: EL file" /etc/logrotate.d/openvswitch
fixture ovslr-both; file /etc/logrotate.d/openvswitch-switch; file /etc/logrotate.d/openvswitch; probe distro_ovs_logrotate
expect_fail "OVS logrotate: both fails naming both" /etc/logrotate.d/openvswitch-switch /etc/logrotate.d/openvswitch
fixture ovslr-none; probe distro_ovs_logrotate
expect_fail "OVS logrotate: neither fails naming both" /etc/logrotate.d/openvswitch-switch /etc/logrotate.d/openvswitch

# --------------------------------------------------------------------------
# OVS and OVN units the shipped Spinifex units name. Run against the real unit
# files, so a unit that names a Debian-only OVS unit fails on the EL fixture.
# --------------------------------------------------------------------------
UNITS_SRC="$(dirname -- "$SCRIPTS_DIR")/build/systemd"
spinifex_units() {
    mkdir -p "$ROOT/etc/systemd/system"
    cp "$UNITS_SRC"/spinifex.target "$UNITS_SRC"/spinifex-*.service "$ROOT/etc/systemd/system/"
}
deps_probe() {
    set --
    for f in "$ROOT"/etc/systemd/system/spinifex*; do set -- "$@" "${f#"$ROOT"}"; done
    probe distro_missing_ovs_deps "$@"
}
el_ovs_units() {
    for u in openvswitch.service ovsdb-server.service ovs-vswitchd.service ovn-controller.service ovn-northd.service openvswitch-ipsec.service; do unit "$u"; done
}

fixture deps-el; spinifex_units; el_ovs_units; deps_probe
expect_ok "OVS dependencies: every one the shipped units name is installed on EL" ""
fixture deps-no-ipsec; spinifex_units; el_ovs_units; rm "$ROOT/usr/lib/systemd/system/openvswitch-ipsec.service"; deps_probe
expect_ok "OVS dependencies: a missing unit is printed once" openvswitch-ipsec.service
fixture deps-debian-only; el_ovs_units
mkdir -p "$ROOT/etc/systemd/system"
printf '[Unit]\n# After=ovs-old.service\nAfter=openvswitch-switch.service ovn-controller.service\nWants=openvswitch-switch.service\n' >"$ROOT/etc/systemd/system/spinifex-x.service"
printf '[Unit]\nRequires=ovn-central.service' >"$ROOT/etc/systemd/system/spinifex-y.service"
deps_probe
expect_ok "OVS dependencies: Debian-only names are missing on EL, comments are not read" "openvswitch-switch.service
ovn-central.service"
fixture deps-unreadable; probe distro_missing_ovs_deps /etc/systemd/system/spinifex.target
expect_fail "OVS dependencies: an unreadable unit file fails naming it" "cannot read /etc/systemd/system/spinifex.target"

# --------------------------------------------------------------------------
# Inline copies. setup.sh and node-reset.sh cannot source the library, so they
# carry copies between markers; each copied function must match the library's
# byte for byte, or a fix to one silently misses the other.
# --------------------------------------------------------------------------
# fn_body <file> <name> — the function from "name() {" to the first "}" line.
fn_body() {
    awk -v name="$2" '$0 == name "() {" { on = 1 } on { print } on && $0 == "}" { exit }' "$1"
}

for copy in setup.sh node-reset.sh; do
    path="$SCRIPTS_DIR/$copy"
    block=$(sed -n '/^# --- BEGIN distro.sh probes ---$/,/^# --- END distro.sh probes ---$/p' "$path")
    if [ -z "$block" ]; then
        fail "$copy: no distro.sh probe block"
        continue
    fi
    names=$(printf '%s\n' "$block" | sed -n 's/^\([a-z_][a-z0-9_]*\)() {$/\1/p')
    if [ -z "$names" ]; then
        fail "$copy: the distro.sh probe block holds no functions"
        continue
    fi
    for name in $names; do
        want=$(fn_body "$DISTRO" "$name")
        got=$(fn_body "$path" "$name")
        if [ -z "$want" ]; then
            fail "$copy: $name is not in lib/distro.sh"
        elif [ "$got" = "$want" ]; then
            pass "$copy: $name matches lib/distro.sh"
        else
            fail "$copy: $name differs from lib/distro.sh"
        fi
    done

    # A copied probe calling a probe helper the block lacks fails only when run.
    for called in $(printf '%s\n' "$block" | grep -v '^ *#' | grep -o 'distro_[a-z_]*' | sort -u); do
        case " $(echo $names) " in
            *" $called "*) ;;
            *) fail "$copy: copies a probe that calls $called without copying it" ;;
        esac
    done
done

# --------------------------------------------------------------------------
# A consumer with no library beside it and none installed must stop naming
# both paths, not run on with the probes undefined. Skipped on a host where
# setup.sh has installed the library, since the fallback then finds it.
# --------------------------------------------------------------------------
if [ -e /usr/local/share/spinifex/lib/distro.sh ]; then
    echo "  skip - consumers without distro.sh: /usr/local/share/spinifex/lib/distro.sh is installed here"
else
    # uninstall-spx.sh sources the library after its --help, so it is run into
    # its multi-node refusal, which exits 1 without touching the host either way.
    mkdir -p "$TMP/alone/bin" "$TMP/alone/etc"
    printf '#!/bin/sh\nexec "$@"\n' > "$TMP/alone/bin/sudo"
    chmod +x "$TMP/alone/bin/sudo"
    printf '[nodes.node1]\n[nodes.node2]\n' > "$TMP/alone/etc/spinifex.toml"
    for consumer in "setup-ovn.sh --help" "uninstall-spx.sh --dry-run"; do
        script=${consumer%% *}
        cp "$SCRIPTS_DIR/$script" "$TMP/alone/"
        PATH="$TMP/alone/bin:$PATH" SPX_ETC_DIR="$TMP/alone/etc" "$TMP/alone/$script" "${consumer#* }" >"$TMP/out" 2>&1 && rc=0 || rc=$?
        if [ "$rc" != 0 ] && grep -q "distro.sh not found at $TMP/alone/lib/distro.sh or /usr/local/share/spinifex/lib/distro.sh" "$TMP/out"; then
            pass "$script: stops naming both paths when distro.sh is missing"
        else
            fail "$script: without distro.sh exited $rc: $(head -3 "$TMP/out")"
        fi
    done
fi

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
