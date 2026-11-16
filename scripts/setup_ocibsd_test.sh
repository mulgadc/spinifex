#!/bin/sh
# setup_ocibsd_test.sh — setup.sh's systemd and udev stages never install the
# tarball's ocibsd/ files; install_ocibsd stages them on an OCI instance, and
# reinstalls them on upgrade only where the node's config already runs ocibsd.
# A stub SUDO records every privileged command and runs the file ones under a
# scratch root, so what each case leaves on disk is checked as well.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SETUP="$SCRIPT_DIR/setup.sh"
BUILD="$(dirname -- "$SCRIPT_DIR")/build"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

# The tarball layout the release image and dev-install.sh assemble.
EXTRACT="$TMP/extract"
mkdir -p "$EXTRACT/systemd" "$EXTRACT/udev"
cp "$BUILD"/systemd/* "$EXTRACT/systemd/"
cp "$BUILD"/udev/* "$EXTRACT/udev/"
cp -r "$BUILD/ocibsd" "$EXTRACT/ocibsd"

# install, ln, rm and mkdir run with /etc and /usr/local/share/spinifex moved
# under $ROOT; anything else is only recorded.
cat > "$TMP/sudo" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$CALLS"
case "$1" in
    install | ln | rm | mkdir) ;;
    tee) cat > /dev/null; exit 0 ;;
    *) exit 0 ;;
esac
args=()
for a in "$@"; do
    case "$a" in
        /etc/* | /usr/local/share/spinifex/*) a="$ROOT$a" ;;
    esac
    args+=("$a")
done
exec "${args[@]}"
EOF
chmod +x "$TMP/sudo"

# run_setup <case> <oci_instance yes|no> <config body or -> <functions...>
# runs the named setup.sh functions in a fresh root for one case.
run_setup() {
    name="$1" vm="$2" body="$3"
    shift 3
    ROOT="$TMP/$name/root"
    CALLS="$TMP/$name/calls"
    mkdir -p "$ROOT/etc/systemd/system" "$ROOT/etc/spinifex" "$ROOT/usr/local/share/spinifex"
    : > "$CALLS"
    [ "$body" = - ] || printf '%s\n' "$body" > "$ROOT/etc/spinifex/spinifex.toml"
    export ROOT CALLS
    OUT=$(INSTALL_SPINIFEX_LIB_ONLY=1 bash -c '
        . "$1"; SUDO="$2"; EXTRACT_DIR="$3"; OCI_INSTANCE="$4"; SPINIFEX_TOML="$ROOT/etc/spinifex/spinifex.toml"
        shift 4
        for f in "$@"; do $f; done' _ "$SETUP" "$TMP/sudo" "$EXTRACT" "$vm" "$@" 2>&1) \
        || fail "$name: $* failed: $OUT"
}

echo "-- the systemd and udev stages leave the tarball's ocibsd/ alone"
run_setup units yes - install_systemd install_udev
if grep -q " /etc/systemd/system/spinifex-viperblock.service$" "$CALLS" && grep -q " /etc/udev/rules.d/99-spinifex-vfio.rules$" "$CALLS"; then
    pass "installs the systemd/ units and udev/ rules"
else
    fail "did not install the systemd/ and udev/ files: $(cat "$CALLS")"
fi
if grep -q "ocibsd" "$CALLS"; then
    fail "installed an ocibsd/ file: $(grep ocibsd "$CALLS")"
else
    pass "installs nothing from ocibsd/"
fi

STAGED="usr/local/share/spinifex/ocibsd"
FILES="spinifex-ocibsd.service 99-spinifex-ocibsd.rules spinifex-daemon.service.d/ocibsd.conf"
installed_count() {
    n=0
    for p in etc/systemd/system/spinifex-ocibsd.service etc/udev/rules.d/99-spinifex-ocibsd.rules \
        etc/systemd/system/spinifex-daemon.service.d/ocibsd.conf; do
        [ ! -e "$ROOT/$p" ] || n=$((n + 1))
    done
    echo "$n"
}
staged_ok() {
    for f in $FILES; do
        cmp -s "$EXTRACT/ocibsd/$f" "$ROOT/$STAGED/$f" || return 1
    done
}

echo "-- a fresh install on an OCI instance stages ocibsd/ and installs none of it"
run_setup fresh yes - install_systemd install_udev install_ocibsd
if staged_ok; then pass "the three files are staged"; else fail "the stage does not match the tarball: $(ls -R "$ROOT/$STAGED" 2>&1)"; fi
[ "$(installed_count)" = 0 ] && pass "nothing is installed" || fail "installed $(installed_count) ocibsd file(s) on a node init has not resolved"
[ -L "$ROOT/etc/systemd/system/spinifex-viperblock.service" ] && fail "masked viperblock" || pass "viperblock is not masked"

echo "-- an upgrade of an ocibsd node replaces the installed files with the new stage"
OCIBSD_TOML='[nodes.node1.ebs]
provider = "ocibsd"

[nodes.node1.ebs.oci]
auth = "instance_principal"'
ROOT="$TMP/upgrade/root"
mkdir -p "$ROOT/$STAGED/spinifex-daemon.service.d" "$ROOT/etc/systemd/system/spinifex-daemon.service.d" "$ROOT/etc/udev/rules.d"
for f in $FILES; do echo "# the previous release" > "$ROOT/$STAGED/$f"; done
echo "# the previous release" > "$ROOT/etc/systemd/system/spinifex-ocibsd.service"
echo "# the previous release" > "$ROOT/etc/systemd/system/spinifex-daemon.service.d/ocibsd.conf"
echo "# the previous release" > "$ROOT/etc/udev/rules.d/99-spinifex-ocibsd.rules"
ln -s /dev/null "$ROOT/etc/systemd/system/spinifex-viperblock.service"
run_setup upgrade yes "$OCIBSD_TOML" install_systemd install_udev install_ocibsd
if staged_ok; then pass "the stage holds the new files"; else fail "the stage kept the previous release"; fi
if cmp -s "$EXTRACT/ocibsd/spinifex-ocibsd.service" "$ROOT/etc/systemd/system/spinifex-ocibsd.service" \
    && cmp -s "$EXTRACT/ocibsd/spinifex-daemon.service.d/ocibsd.conf" "$ROOT/etc/systemd/system/spinifex-daemon.service.d/ocibsd.conf" \
    && cmp -s "$EXTRACT/ocibsd/99-spinifex-ocibsd.rules" "$ROOT/etc/udev/rules.d/99-spinifex-ocibsd.rules"; then
    pass "the unit, drop-in and udev rule are the new ones"
else
    fail "an installed ocibsd file is not the new one"
fi
if [ "$(readlink "$ROOT/etc/systemd/system/spinifex-viperblock.service")" = /dev/null ]; then
    pass "viperblock is masked again after the unit reinstall"
else
    fail "the reinstalled viperblock unit is not masked"
fi
grep -q '^systemctl daemon-reload$' "$CALLS" && grep -q '^udevadm control --reload-rules$' "$CALLS" \
    && pass "systemd and udev are reloaded" || fail "no reload: $(cat "$CALLS")"

echo "-- an ocibsd node is refreshed even when the metadata service does not answer"
run_setup upgrade-no-imds no "$OCIBSD_TOML" install_ocibsd
[ "$(installed_count)" = 3 ] && pass "the three files are installed" || fail "installed $(installed_count) of 3"

echo "-- an upgrade of a viperblock node on an OCI instance installs none of them"
run_setup viperblock yes '[nodes.node1.ebs]
provider = "viperblockd"' install_systemd install_udev install_ocibsd
[ "$(installed_count)" = 0 ] && pass "nothing is installed" || fail "installed $(installed_count) ocibsd file(s) on a viperblock node"
[ -f "$ROOT/etc/systemd/system/spinifex-viperblock.service" ] && [ ! -L "$ROOT/etc/systemd/system/spinifex-viperblock.service" ] \
    && pass "the viperblock unit is installed and not masked" || fail "the viperblock unit was masked"

echo "-- off OCI nothing is staged"
run_setup off-oci no '[nodes.node1.daemon]' install_ocibsd
[ ! -e "$ROOT/$STAGED" ] && pass "no stage" || fail "staged ocibsd/ off OCI"

echo "-- the initiator is installed on an OCI instance only"
run_setup initiator-apt yes - "install_iscsi_initiator apt"
grep -q 'apt-get install -y -qq open-iscsi' "$CALLS" && grep -q '^systemctl enable --now iscsid.socket$' "$CALLS" \
    && pass "apt installs open-iscsi and enables iscsid.socket" || fail "apt: $(cat "$CALLS")"
run_setup initiator-dnf yes - "install_iscsi_initiator dnf"
grep -q 'dnf install -y -q iscsi-initiator-utils' "$CALLS" && pass "dnf installs iscsi-initiator-utils" || fail "dnf: $(cat "$CALLS")"
run_setup initiator-off no - "install_iscsi_initiator apt"
[ -s "$CALLS" ] && fail "installed an initiator off OCI: $(cat "$CALLS")" || pass "no initiator off OCI"

echo "-- detection takes any shape the metadata service answers, bare metal included"
mkdir -p "$TMP/bin"
printf '#!/bin/sh\n[ -n "$SHAPE" ] || exit 22\nprintf %%s "$SHAPE"\n' > "$TMP/bin/curl"
chmod +x "$TMP/bin/curl"
detect() {
    SHAPE="$1" PATH="$TMP/bin:$PATH" INSTALL_SPINIFEX_LIB_ONLY=1 bash -c '
        . "$1"; unset OCI_INSTANCE
        if is_oci_instance; then echo yes; else echo no; fi' _ "$SETUP" 2>/dev/null
}
for shape in VM.Standard.E6.Flex BM.Standard.E2.64; do
    [ "$(detect "$shape")" = yes ] && pass "$shape is an OCI instance" || fail "$shape was not detected"
done
[ "$(detect "")" = no ] && pass "no answer is not an OCI instance" || fail "detected an OCI instance with no metadata answer"

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
