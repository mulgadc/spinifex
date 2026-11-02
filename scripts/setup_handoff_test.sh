#!/bin/sh
# setup_handoff_test.sh — tests that setup.sh runs the setup.sh shipped in the
# tarball it downloaded, before it changes anything on the host.
#
# setup.sh is sourced with INSTALL_SPINIFEX_LIB_ONLY=1 and main runs with the
# host-touching functions stubbed; the packaged setup.sh is a fixture that
# prints what it was given.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SETUP="$SCRIPT_DIR/setup.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- setup.sh hand-off to the packaged setup.sh"

mkdir -p "$TMP/extract"
cat > "$TMP/packaged-setup.sh" <<'EOF'
echo "PACKAGED args=[$*] tarball=${INSTALL_SPINIFEX_TARBALL:-} handoff=${SPINIFEX_HANDOFF_TMPDIR:-}"
EOF

# run <packaged: yes|no> [main args...] — runs main with every step that would
# touch the host stubbed to print its name, and sets OUT.
run() {
    packaged="$1"
    shift
    rm -f "$TMP/extract/setup.sh"
    [ "$packaged" = yes ] && cp "$TMP/packaged-setup.sh" "$TMP/extract/setup.sh"
    OUT=$(INSTALL_SPINIFEX_LIB_ONLY=1 TMPROOT="$TMP" bash -c '
        . "$1"; shift
        info() { :; }
        setup_sudo() { :; }
        resolve_firewall_mode() { :; }
        detect_os() { :; }
        detect_arch() { :; }
        download_spinifex() {
            SPINIFEX_TMPDIR="$TMPROOT/extract"
            TARBALL="$SPINIFEX_TMPDIR/spinifex.tar.gz"
            EXTRACT_DIR="$SPINIFEX_TMPDIR"
        }
        handle_upgrade() { echo HANDLE_UPGRADE; exit 0; }
        main "$@"
    ' _ "$SETUP" "$@" 2>&1) || true
    OUT=$(printf '%s\n' "$OUT" | sed '/^$/d')
}

run yes --channel dev --firewall off
want="PACKAGED args=[--channel dev --firewall off] tarball=$TMP/extract/spinifex.tar.gz handoff=$TMP/extract"
if [ "$OUT" = "$want" ]; then
    pass "a downloaded tarball's setup.sh runs with the same arguments, before services are stopped"
else
    fail "downloaded tarball: got [$OUT], want [$want]"
fi

run no --channel dev
if [ "$OUT" = HANDLE_UPGRADE ]; then
    pass "a tarball without setup.sh is installed by the running script"
else
    fail "no packaged setup.sh: got [$OUT]"
fi

run_local() {
    rm -f "$TMP/extract/setup.sh"
    cp "$TMP/packaged-setup.sh" "$TMP/extract/setup.sh"
    OUT=$(INSTALL_SPINIFEX_LIB_ONLY=1 INSTALL_SPINIFEX_TARBALL=/somewhere/spinifex.tar.gz bash -c '
        . "$1"
        info() { :; }
        EXTRACT_DIR="$2"; TARBALL="$2/spinifex.tar.gz"; SPINIFEX_TMPDIR="$2"
        hand_off_to_packaged_setup --channel dev
        echo STAYED
    ' _ "$SETUP" "$TMP/extract" 2>&1) || true
}
run_local
if [ "$OUT" = STAYED ]; then
    pass "a local tarball, including the one a hand-off passes on, is not handed off again"
else
    fail "local tarball: got [$OUT]"
fi

if [ "$fails" -gt 0 ]; then
    echo "setup_handoff_test.sh: $fails failure(s)"
    exit 1
fi
echo "setup_handoff_test.sh: ok"
