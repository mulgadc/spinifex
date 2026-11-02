#!/bin/sh
# setup_firewall_test.sh — tests for how setup.sh picks the firewall mode and
# masks firewalld.
#
# setup.sh is sourced with INSTALL_SPINIFEX_LIB_ONLY=1 so main never runs. A stub
# systemctl keeps firewalld's state in fixture files, and the mode file is a
# fixture path.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SETUP="$SCRIPT_DIR/setup.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- setup.sh firewall mode and firewalld"

mkdir -p "$TMP/bin"
cat > "$TMP/bin/systemctl" <<EOF
#!/bin/sh
state="$TMP/state"
[ "\$1" = "--quiet" ] && shift
case "\$1 \$2" in
    "is-active --quiet") [ -e "\$state/active" ] ;;
    "is-enabled firewalld.service") [ -e "\$state/enabled" ] || { echo not-found; exit 4; }; cat "\$state/enabled" ;;
    "mask --now")
        echo "mask" >> "\$state/calls"
        [ -e "\$state/fail-mask" ] && exit 1
        echo masked > "\$state/enabled"; rm -f "\$state/active" ;;
    *) echo "unexpected systemctl \$*" >&2; exit 2 ;;
esac
EOF
chmod +x "$TMP/bin/systemctl"

# host <firewalld: none|inactive|active|masked> [recorded mode] — sets up the
# fixture host. The mode file is absent when no mode is given.
host() {
    rm -rf "$TMP/state" "$TMP/mode"
    mkdir -p "$TMP/state"
    case "$1" in
        inactive) echo disabled > "$TMP/state/enabled" ;;
        active) echo enabled > "$TMP/state/enabled"; : > "$TMP/state/active" ;;
        masked) echo masked > "$TMP/state/enabled" ;;
    esac
    if [ -n "${2:-}" ]; then printf '%s\n' "$2" > "$TMP/mode"; fi
}

# run <function...> [--firewall value via FW] — sets RC and OUT, and MODE to
# the mode setup.sh resolved.
run() {
    RC=0
    OUT=$(PATH="$TMP/bin:$PATH" SPINIFEX_FIREWALL_MODE_FILE="$TMP/mode" INSTALL_SPINIFEX_FIREWALL="${FW:-}" \
        INSTALL_SPINIFEX_LIB_ONLY=1 bash -c '. "$1"; shift; for f in "$@"; do "$f"; done; echo "MODE=$INSTALL_SPINIFEX_FIREWALL"' \
        _ "$SETUP" "$@" 2>&1) || RC=$?
    MODE=$(printf '%s\n' "$OUT" | sed -n 's/^MODE=//p')
    unset FW
}

resolves() { # resolves <desc> <mode>
    if [ "$RC" = 0 ] && [ "$MODE" = "$2" ]; then pass "$1 resolves to $2"; else fail "$1: rc=$RC mode='$MODE': $OUT"; fi
}

host none
run resolve_firewall_mode
resolves "no firewalld, no recorded mode" off

host inactive
run resolve_firewall_mode
resolves "firewalld installed but inactive" off

host active
run resolve_firewall_mode
resolves "firewalld active" on

host masked on
run resolve_firewall_mode
resolves "a rerun after an install with --firewall=on" on

host active on
run resolve_firewall_mode
resolves "a retry after the mode file was written but before firewalld was masked" on

host active off
run resolve_firewall_mode
resolves "a recorded 'off' with firewalld active" off

host masked on
FW=off run resolve_firewall_mode
resolves "an explicit --firewall=off over a recorded 'on'" off

host none on
FW=off run resolve_firewall_mode
resolves "an explicit --firewall=off" off

host none
FW=maybe run resolve_firewall_mode
case "$RC:$OUT" in
    0:*) fail "accepts --firewall=maybe" ;;
    *"--firewall must be"*) pass "rejects --firewall=maybe" ;;
    *) fail "rejects --firewall=maybe without saying why: $OUT" ;;
esac

host none garbage
run resolve_firewall_mode
case "$RC:$OUT" in
    0:*) fail "accepts a mode file holding 'garbage'" ;;
    *"holds 'garbage'"*) pass "rejects a mode file holding 'garbage', naming it" ;;
    *) fail "rejects a bad mode file without naming its content: $OUT" ;;
esac

# The record sits under /etc/spinifex, which only root and the spinifex group
# can enter, so an installer run by any other sudoer must read it through sudo.
cat > "$TMP/bin/sudo" <<EOF
#!/bin/sh
echo "\$*" >> "$TMP/sudo-calls"
exec "\$@"
EOF
chmod +x "$TMP/bin/sudo"
host masked on
rm -f "$TMP/sudo-calls"
RC=0
OUT=$(PATH="$TMP/bin:$PATH" SUDO=sudo SPINIFEX_FIREWALL_MODE_FILE="$TMP/mode" INSTALL_SPINIFEX_FIREWALL="" \
    INSTALL_SPINIFEX_LIB_ONLY=1 bash -c '. "$1"; resolve_firewall_mode; echo "MODE=$INSTALL_SPINIFEX_FIREWALL"' \
    _ "$SETUP" 2>&1) || RC=$?
MODE=$(printf '%s\n' "$OUT" | sed -n 's/^MODE=//p')
if [ "$RC" = 0 ] && [ "$MODE" = on ] && grep -qx "cat $TMP/mode" "$TMP/sudo-calls" 2>/dev/null; then
    pass "a non-root rerun reads the recorded mode through sudo"
else
    fail "a non-root rerun did not read the recorded mode through sudo: rc=$RC mode='$MODE' calls='$(cat "$TMP/sudo-calls" 2>/dev/null)': $OUT"
fi

# A record that exists but cannot be read must stop the install, never fall
# through to the firewalld check, which reads a masked firewalld as "off".
if [ "$(id -u)" != 0 ]; then
    host masked on
    chmod 000 "$TMP/mode"
    run resolve_firewall_mode
    chmod 644 "$TMP/mode"
    case "$RC:$OUT" in
        0:*) fail "an unreadable mode file resolved to '$MODE'" ;;
        *"Cannot read the recorded firewall mode"*) pass "an unreadable mode file stops the install" ;;
        *) fail "an unreadable mode file stopped without saying why: $OUT" ;;
    esac
fi

masks() { # masks <desc> <expect: masked|untouched>
    calls=$(cat "$TMP/state/calls" 2>/dev/null || true)
    if [ "$RC" != 0 ]; then
        fail "$1: rc=$RC: $OUT"
    elif [ "$2" = masked ] && [ -n "$calls" ] && [ "$(cat "$TMP/state/enabled")" = masked ] && [ ! -e "$TMP/state/active" ]; then
        pass "$1: masked and stopped"
    elif [ "$2" = untouched ] && [ -z "$calls" ]; then
        pass "$1: left alone"
    else
        fail "$1: expected $2, calls='$calls': $OUT"
    fi
}

host active
run resolve_firewall_mode mask_firewalld
masks "firewalld active" masked
case "$OUT" in
    *"Restate any other restriction"*) pass "masking firewalld says its rules were not migrated" ;;
    *) fail "masking firewalld printed no notice: $OUT" ;;
esac

host inactive
run mask_firewalld
masks "firewalld installed but inactive" masked

host none
run mask_firewalld
masks "no firewalld" untouched

host masked
run mask_firewalld
masks "firewalld already masked" untouched

host active
echo masked > "$TMP/state/enabled"
run mask_firewalld
masks "firewalld masked but still running" masked

host active
: > "$TMP/state/fail-mask"
run mask_firewalld
case "$RC:$OUT" in
    0:*) fail "succeeds when firewalld cannot be masked" ;;
    *"Cannot stop and mask firewalld"*) pass "fails by name when firewalld cannot be masked" ;;
    *) fail "fails without naming firewalld: $OUT" ;;
esac

host active
FW=off run mask_firewalld
case "$OUT" in
    *"this host now has no firewall"*) pass "masking firewalld under --firewall=off says the host has no firewall" ;;
    *) fail "masking under --firewall=off did not say the host has no firewall: $OUT" ;;
esac

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
