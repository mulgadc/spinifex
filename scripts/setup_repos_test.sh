#!/bin/sh
# setup_repos_test.sh — tests for the Oracle Linux repository trust setup in
# setup.sh: the NFV SIG key fingerprint check and the AppStream QEMU exclude.
#
# setup.sh is sourced with INSTALL_SPINIFEX_LIB_ONLY=1 so main never runs. Stub
# gpg, curl, rpm and dnf read and log fixture files, and the key path is a
# fixture path.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SETUP="$SCRIPT_DIR/setup.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- setup.sh NFV SIG key and AppStream exclude"

GOOD=35154228174901BEFA8E69A621465E289D2A76A7
BAD=0000000000000000000000000000000000000BAD

mkdir -p "$TMP/bin"
# A fixture key file holds the --with-colons listing gpg would print for it.
cat > "$TMP/bin/gpg" <<'EOF'
#!/bin/sh
for last; do :; done
cat "$last"
EOF
cat > "$TMP/bin/curl" <<EOF
#!/bin/sh
echo "curl" >> "$TMP/calls"
while [ \$# -gt 0 ]; do
    case "\$1" in -o) cp "$TMP/served" "\$2"; exit 0 ;; esac
    shift
done
exit 1
EOF
cat > "$TMP/bin/rpm" <<EOF
#!/bin/sh
echo "rpm \$*" >> "$TMP/calls"
EOF
cat > "$TMP/bin/dnf" <<EOF
#!/bin/sh
case "\$*" in
    "config-manager --dump "*) cat "$TMP/appstream" ;;
    "config-manager --save "*) echo "\$3" >> "$TMP/calls" ;;
    *) echo "unexpected dnf \$*" >&2; exit 2 ;;
esac
EOF
chmod +x "$TMP/bin/"*

key() { # key <file> <pub-fpr> [sub-fpr] [second-pub-fpr]
    {
        printf 'pub:-:4096:1:AAAA:1:::-:::scESC::::::23::0:\nfpr:::::::::%s:\n' "$2"
        if [ -n "${3:-}" ]; then printf 'sub:-:4096:1:BBBB:1::::::e::::::23:\nfpr:::::::::%s:\n' "$3"; fi
        if [ -n "${4:-}" ]; then printf 'pub:-:4096:1:CCCC:1:::-:::scESC::::::23::0:\nfpr:::::::::%s:\n' "$4"; fi
    } > "$1"
}

# run <function> — sets RC, OUT and CALLS, from $TMP as the working directory.
run() {
    rm -f "$TMP/calls"
    RC=0
    OUT=$(cd "$TMP/cwd" && PATH="$TMP/bin:$PATH" INSTALL_SPINIFEX_LIB_ONLY=1 \
        bash -c '. "$1"; SUDO=""; NFV_SIG_KEY_FILE="$2/installed.key"; "$3"' _ "$SETUP" "$TMP" "$1" 2>&1) || RC=$?
    CALLS=$(cat "$TMP/calls" 2>/dev/null || true)
}
mkdir -p "$TMP/cwd"

imports_once() { [ "$(printf '%s\n' "$CALLS" | grep -c '^rpm --import')" = 1 ]; }

rm -f "$TMP/installed.key"
key "$TMP/served" "$GOOD"
run install_nfv_sig_key
if [ "$RC" = 0 ] && imports_once && cmp -s "$TMP/served" "$TMP/installed.key"; then
    pass "the SIG's key is installed and imported once"
else
    fail "the SIG's key was not installed and imported once: rc=$RC calls='$CALLS': $OUT"
fi

refused() { # refused <desc>
    case "$RC:$OUT" in
        0:*) fail "$1 was accepted" ;;
        *"Refusing to trust it"*)
            if printf '%s\n' "$CALLS" | grep -q '^rpm' || [ -e "$TMP/installed.key" ]; then
                fail "$1 was refused, but only after it was installed or imported: $CALLS"
            else
                pass "$1 is refused before anything is trusted"
            fi ;;
        *) fail "$1 stopped without the refusal: $OUT" ;;
    esac
}

rm -f "$TMP/installed.key"
key "$TMP/served" "$BAD"
run install_nfv_sig_key
refused "a key with another fingerprint"
case "$OUT" in
    *"$BAD"*"$GOOD"*) pass "the refusal names both fingerprints" ;;
    *) fail "the refusal does not name both fingerprints: $OUT" ;;
esac

rm -f "$TMP/installed.key"
key "$TMP/served" "$GOOD" "" "$BAD"
run install_nfv_sig_key
refused "a file holding the SIG's key and a second key"

rm -f "$TMP/installed.key"
key "$TMP/served" "$BAD" "$GOOD"
run install_nfv_sig_key
refused "a key whose subkey, not primary key, has the SIG's fingerprint"

key "$TMP/installed.key" "$BAD"
key "$TMP/served" "$GOOD"
run install_nfv_sig_key
if [ "$RC" = 0 ] && printf '%s\n' "$CALLS" | grep -q '^curl' && imports_once && cmp -s "$TMP/served" "$TMP/installed.key"; then
    pass "a cached key with another fingerprint is downloaded again and replaced"
else
    fail "a cached key with another fingerprint was not replaced: rc=$RC calls='$CALLS': $OUT"
fi

key "$TMP/installed.key" "$GOOD"
run install_nfv_sig_key
if [ "$RC" = 0 ] && ! printf '%s\n' "$CALLS" | grep -q '^curl' && imports_once; then
    pass "a cached SIG key is imported without a download"
else
    fail "a cached SIG key was not reused: rc=$RC calls='$CALLS': $OUT"
fi

# excludes <desc> <dump excludepkgs line, or empty> <expected setopt>
excludes() {
    if [ -n "$2" ]; then printf '[ol10_appstream]\nexcludepkgs = %s\nenabled = 1\n' "$2" > "$TMP/appstream"
    else printf '[ol10_appstream]\nenabled = 1\n' > "$TMP/appstream"; fi
    run exclude_from_appstream
    if [ "$RC" = 0 ] && [ "$CALLS" = "--setopt=ol10_appstream.excludepkgs=$3" ]; then
        pass "$1"
    else
        fail "$1: rc=$RC calls='$CALLS': $OUT"
    fi
}

excludes "no exclude set gains the QEMU patterns" "" "qemu-kvm*,qemu-img*"
excludes "an operator's excludes are kept" "foo*, bar" "foo*,bar,qemu-kvm*,qemu-img*"
excludes "a rerun adds no duplicates" "foo*, bar, qemu-kvm*, qemu-img*" "foo*,bar,qemu-kvm*,qemu-img*"
: > "$TMP/cwd/qemu-kvm-x"
excludes "the patterns are never expanded against the working directory" "" "qemu-kvm*,qemu-img*"

[ "$fails" = 0 ] || { echo "$fails failure(s)"; exit 1; }
