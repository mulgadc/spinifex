#!/bin/sh
# setup_ocibsd_test.sh — setup.sh installs the tarball's systemd/ and udev/
# files and never the ocibsd/ ones, which only the install role puts on an
# ocibsd node. A stub SUDO records every privileged command setup.sh runs.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SETUP="$SCRIPT_DIR/setup.sh"
BUILD="$(dirname -- "$SCRIPT_DIR")/build"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- setup.sh leaves the tarball's ocibsd/ alone"

# The tarball layout the release image and dev-install.sh assemble.
EXTRACT="$TMP/extract"
mkdir -p "$EXTRACT/systemd" "$EXTRACT/udev"
cp "$BUILD"/systemd/* "$EXTRACT/systemd/"
cp "$BUILD"/udev/* "$EXTRACT/udev/"
cp -r "$BUILD/ocibsd" "$EXTRACT/ocibsd"

cat > "$TMP/sudo" <<EOF
#!/bin/sh
echo "\$*" >> "$TMP/calls"
case "\$1" in tee) cat > /dev/null ;; esac
exit 0
EOF
chmod +x "$TMP/sudo"
: > "$TMP/calls"

OUT=$(ISO_BUILD=1 INSTALL_SPINIFEX_LIB_ONLY=1 bash -c '
    . "$1"; SUDO="$2"; EXTRACT_DIR="$3"
    install_systemd && install_udev' _ "$SETUP" "$TMP/sudo" "$EXTRACT" 2>&1) || fail "install_systemd/install_udev failed: $OUT"

if grep -q " /etc/systemd/system/spinifex-viperblock.service$" "$TMP/calls" && grep -q " /etc/udev/rules.d/99-spinifex-vfio.rules$" "$TMP/calls"; then
    pass "installs the systemd/ units and udev/ rules"
else
    fail "did not install the systemd/ and udev/ files: $(cat "$TMP/calls")"
fi

if grep -q "ocibsd" "$TMP/calls"; then
    fail "installed an ocibsd/ file: $(grep ocibsd "$TMP/calls")"
else
    pass "installs nothing from ocibsd/"
fi

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
