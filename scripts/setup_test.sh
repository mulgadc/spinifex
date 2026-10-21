#!/bin/sh
# setup_test.sh — tests for setup.sh's supported-OS gate.
#
# setup.sh is sourced with INSTALL_SPINIFEX_LIB_ONLY=1 so main never runs, and
# detect_os reads a fixture os-release, a fixture cpuinfo and a stub uname.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SETUP="$SCRIPT_DIR/setup.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- setup.sh OS gate"

V3_FLAGS="fpu cx16 lahf_lm popcnt sse4_1 sse4_2 ssse3 avx avx2 bmi1 bmi2 f16c fma abm movbe xsave"
V2_FLAGS="fpu cx16 lahf_lm popcnt sse4_1 sse4_2 ssse3"
UEK_R8="6.12.0-207.111.5.4.el10uek.x86_64"

# gate <os-release body> [machine] [kernel] [cpu flags] — runs detect_os and
# sets RC and OUT (stdout and stderr together).
gate() {
    printf '%s\n' "$1" > "$TMP/os-release"
    printf 'processor\t: 0\nflags\t\t: %s\n' "${4:-$V3_FLAGS}" > "$TMP/cpuinfo"
    mkdir -p "$TMP/bin"
    cat > "$TMP/bin/uname" <<EOF
#!/bin/sh
case "\$1" in
    -m) echo "${2:-x86_64}" ;;
    -r) echo "${3:-$UEK_R8}" ;;
    *) exit 1 ;;
esac
EOF
    chmod +x "$TMP/bin/uname"
    RC=0
    OUT=$(PATH="$TMP/bin:$PATH" SPINIFEX_OS_RELEASE="$TMP/os-release" SPINIFEX_CPUINFO="$TMP/cpuinfo" \
        INSTALL_SPINIFEX_LIB_ONLY=1 bash -c '. "$1"; detect_os' _ "$SETUP" 2>&1) || RC=$?
}
accepts() {
    if [ "$RC" = 0 ]; then pass "accepts $1"; else fail "rejects $1: $OUT"; fi
}
# rejects <desc> <word> — the refusal must say why, not just exit.
rejects() {
    if [ "$RC" = 0 ]; then
        fail "accepts $1"
    else
        case "$OUT" in
            *"$2"*) pass "rejects $1" ;;
            *) fail "rejects $1 without naming '$2': $OUT" ;;
        esac
    fi
}

OL='ID="ol"
NAME="Oracle Linux Server"
PRETTY_NAME="Oracle Linux Server 10.2"'

gate 'ID=debian
VERSION_ID="13"
PRETTY_NAME="Debian GNU/Linux 13 (trixie)"'
accepts "Debian 13"

# Debian testing and sid carry no VERSION_ID. The gate has always let them
# through, and Debian behaviour is not this gate's to change.
gate 'ID=debian
PRETTY_NAME="Debian GNU/Linux forky/sid"'
accepts "Debian testing with no VERSION_ID"

gate 'ID=debian
VERSION_ID="12"'
rejects "Debian 12" "Minimum: Debian 13"

gate 'ID=ubuntu
VERSION_ID="26.04"
PRETTY_NAME="Ubuntu 26.04 LTS"'
accepts "Ubuntu 26.04"

gate 'ID=ubuntu
VERSION_ID="22.04"'
rejects "Ubuntu 22.04" "Minimum: Ubuntu 24.04"

gate "$OL
VERSION_ID=\"10.2\""
accepts "Oracle Linux 10.2 on UEK R8, x86_64, x86-64-v3"

gate "$OL
VERSION_ID=\"10.4\""
accepts "Oracle Linux 10.4"

gate "$OL
VERSION_ID=\"10.1\""
rejects "Oracle Linux 10.1" "Oracle Linux 10.2 or a later 10.x"

gate "$OL
VERSION_ID=\"10\""
rejects "Oracle Linux with no minor version" "Oracle Linux 10.2 or a later 10.x"

gate "$OL
VERSION_ID=\"11.0\""
rejects "Oracle Linux 11" "Oracle Linux 10.2 or a later 10.x"

gate "$OL
VERSION_ID=\"9.6\"" x86_64 "5.15.0-310.184.5.2.el9uek.x86_64"
rejects "Oracle Linux 9" "Oracle Linux 10.2 or a later 10.x"

gate "$OL" x86_64
rejects "Oracle Linux with no VERSION_ID" "Oracle Linux 10.2 or a later 10.x"

gate "$OL
VERSION_ID=\"10.2\"" aarch64 "6.12.0-207.111.5.4.el10uek.aarch64"
rejects "Oracle Linux on aarch64" "x86_64 only"

gate "$OL
VERSION_ID=\"10.2\"" x86_64 "6.12.0-55.9.1.el10_0.x86_64"
rejects "Oracle Linux on the Red Hat compatible kernel" "is not UEK R8"

gate "$OL
VERSION_ID=\"10.2\"" x86_64 "5.15.0-310.184.5.2.el9uek.x86_64"
rejects "Oracle Linux on a UEK older than R8" "is not UEK R8"

gate "$OL
VERSION_ID=\"10.2\"" x86_64 "$UEK_R8" "$V2_FLAGS"
rejects "Oracle Linux on an x86-64-v2 CPU" "missing: avx avx2 bmi1 bmi2 f16c fma abm movbe xsave"

gate 'ID="rocky"
VERSION_ID="10.0"'
rejects "Rocky Linux 10" "Unsupported OS: rocky"

gate 'this is not an os-release file'
rejects "a malformed os-release" "Unsupported OS"

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
