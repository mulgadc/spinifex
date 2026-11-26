#!/bin/sh
# setup_selinux_test.sh — install_selinux_policy loads the tarball's module and
# relabels the installed paths only where SELinux is enabled, and never recurses
# into a directory that holds guest files. A stub SUDO records every command.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SETUP="$SCRIPT_DIR/setup.sh"
BUILD="$(dirname -- "$SCRIPT_DIR")/build"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

EXTRACT="$TMP/extract"
mkdir -p "$EXTRACT/selinux"
cp "$BUILD/selinux/spinifex.cil" "$EXTRACT/selinux/"

# test -e reports /etc/ipsec.secrets missing; everything else is only recorded.
cat > "$TMP/sudo" <<'EOF'
#!/bin/sh
echo "$*" >> "$CALLS"
[ "$1 $2 $3" = "test -e /etc/ipsec.secrets" ] && exit 1
exit 0
EOF
chmod +x "$TMP/sudo"

# run_case <name> <selinux on|off|absent> <extract dir> <pluto yes|no> [os id]
run_case() {
    name="$1" mode="$2" extract="$3" pluto="$4" os="${5:-ol}"
    bin="$TMP/$name/bin"
    CALLS="$TMP/$name/calls"
    mkdir -p "$bin"
    : > "$CALLS"
    case "$mode" in
        on) printf '#!/bin/sh\nexit 0\n' > "$bin/selinuxenabled" ;;
        off) printf '#!/bin/sh\nexit 1\n' > "$bin/selinuxenabled" ;;
    esac
    printf '#!/bin/sh\necho plugindir=/usr/lib64/nbdkit/plugins\n' > "$bin/nbdkit"
    chmod +x "$bin"/*
    PLUTO="$TMP/$name/pluto"
    if [ "$pluto" = yes ]; then printf '#!/bin/sh\n' > "$PLUTO" && chmod +x "$PLUTO"; fi
    export CALLS
    RC=0
    # PATH keeps only the stubs and the base tools, so a host selinuxenabled is never found.
    OUT=$(PATH="$bin:/usr/bin:/bin" SPINIFEX_PLUTO="$PLUTO" INSTALL_SPINIFEX_LIB_ONLY=1 bash -c '
        . "$1"; SUDO="$2"; EXTRACT_DIR="$3"; ID="$4"; install_selinux_policy' _ "$SETUP" "$TMP/sudo" "$extract" "$os" 2>&1) || RC=$?
}

# recursed <path> — true when an -RF restorecon call names exactly this path.
recursed() {
    grep '^restorecon -RF ' "$CALLS" | tr ' ' '\n' | grep -qx "$1"
}

echo "-- install_selinux_policy"

if [ -x /usr/bin/selinuxenabled ] || [ -x /bin/selinuxenabled ]; then
    echo "  skip - this host has selinuxenabled in /usr/bin, which the absent case cannot hide"
else
    run_case absent absent "$EXTRACT" yes
    if [ "$RC" = 0 ] && [ ! -s "$CALLS" ]; then
        pass "does nothing on a host without SELinux tools"
    else
        fail "ran commands without SELinux tools (rc $RC): $(cat "$CALLS") $OUT"
    fi
fi

run_case off off "$EXTRACT" yes
if [ "$RC" = 0 ] && [ ! -s "$CALLS" ]; then
    pass "does nothing when SELinux is disabled"
else
    fail "ran commands with SELinux disabled (rc $RC): $(cat "$CALLS") $OUT"
fi

run_case on on "$EXTRACT" yes
if [ "$RC" = 0 ] && grep -qx "semodule -X 300 -i $EXTRACT/selinux/spinifex.cil" "$CALLS"; then
    pass "installs the module at priority 300"
else
    fail "did not install the module (rc $RC): $(cat "$CALLS") $OUT"
fi
for path in /var/lib/spinifex/wait-for-nats.sh /var/lib/spinifex/nats-js-watchdog.sh /swapfile /etc/ipsec.secrets \
    /usr/local/bin/spx /run/spinifex /run/spinifex/nbd; do
    if grep '^restorecon -F -i ' "$CALLS" | tr ' ' '\n' | grep -qx "$path"; then
        pass "relabels $path"
    else
        fail "does not relabel $path: $(cat "$CALLS")"
    fi
done
if recursed /usr/local/share/spinifex && recursed /usr/lib64/nbdkit/plugins/nbdkit-viperblock-plugin.so; then
    pass "relabels the installed trees and the nbdkit plugin"
else
    fail "does not relabel the installed trees: $(cat "$CALLS")"
fi
for path in /run/spinifex /run/spinifex/nbd /var/lib/spinifex /var/lib/spinifex/uefi /var/log/spinifex; do
    if recursed "$path" || grep -q "^restorecon -RF .* $path/" "$CALLS"; then
        fail "recurses into $path, which can hold guest files"
    else
        pass "does not recurse into $path"
    fi
done
if grep -qx "install -m 0600 /dev/null /etc/ipsec.secrets" "$CALLS"; then
    pass "creates /etc/ipsec.secrets where libreswan is installed"
else
    fail "did not create /etc/ipsec.secrets: $(cat "$CALLS")"
fi
before=$(grep -n "install -m 0600 /dev/null /etc/ipsec.secrets" "$CALLS" | cut -d: -f1)
after=$(grep -n "^restorecon -F -i " "$CALLS" | cut -d: -f1)
if [ -n "$before" ] && [ -n "$after" ] && [ "$before" -lt "$after" ]; then
    pass "creates /etc/ipsec.secrets before it relabels it"
else
    fail "relabels before it creates /etc/ipsec.secrets: $(cat "$CALLS")"
fi

run_case nopluto on "$EXTRACT" no
if [ "$RC" = 0 ] && ! grep -q "^install .*/etc/ipsec.secrets" "$CALLS"; then
    pass "creates no /etc/ipsec.secrets without libreswan"
else
    fail "created /etc/ipsec.secrets without libreswan: $(cat "$CALLS")"
fi

run_case debian on "$EXTRACT" yes debian
if [ "$RC" = 0 ] && [ ! -s "$CALLS" ]; then
    pass "does nothing on Debian with SELinux enabled"
else
    fail "ran commands on Debian with SELinux enabled (rc $RC): $(cat "$CALLS") $OUT"
fi

# The systemd stage starts the NATS watchdog timer, which executes a script this
# stage labels, so main must run this stage first.
selinux_line=$(grep -n '^ *stage_enabled selinux *&& install_selinux_policy' "$SETUP" | cut -d: -f1)
systemd_line=$(grep -n '^ *stage_enabled systemd *&& install_systemd' "$SETUP" | cut -d: -f1)
if [ -n "$selinux_line" ] && [ -n "$systemd_line" ] && [ "$selinux_line" -lt "$systemd_line" ]; then
    pass "main labels the scripts before the systemd stage starts units"
else
    fail "main runs the systemd stage before the selinux stage (lines ${selinux_line:-none}, ${systemd_line:-none})"
fi

mkdir -p "$TMP/empty"
run_case nocil on "$TMP/empty" yes
if [ "$RC" != 0 ] && printf '%s' "$OUT" | grep -q "no selinux/spinifex.cil"; then
    pass "refuses when SELinux is enabled and the tarball has no module"
else
    fail "did not refuse a tarball without the module (rc $RC): $OUT"
fi

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
