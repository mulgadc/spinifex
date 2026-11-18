#!/bin/sh
# microvm-image_test.sh — microvm_pack_initramfs publishes only an archive that
# lists completely, removes a rejected one, and succeeds again on a retry.
#
# The real pack step runs against a small chroot; fakeroot and mknod are
# replaced so it needs no root, and cpio is wrapped to simulate a cut pack.
set -eu

SCRIPTS_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- microvm_pack_initramfs"

if ! command -v cpio >/dev/null 2>&1 || ! command -v bash >/dev/null 2>&1; then
    echo "  skip - cpio or bash not installed"
    exit 0
fi
REAL_CPIO=$(command -v cpio)

mkdir -p "$TMP/bin" "$TMP/chroot/dev" "$TMP/chroot/etc" "$TMP/stage"
printf '#!/bin/sh\nexec "$@"\n' > "$TMP/bin/fakeroot"
printf '#!/bin/sh\nexit 0\n' > "$TMP/bin/mknod"
chmod +x "$TMP/bin/fakeroot" "$TMP/bin/mknod"
# Large enough that a cut at 64 KiB loses whole entries and the trailer.
head -c 400000 /dev/urandom > "$TMP/chroot/etc/blob"
printf 'init\n' > "$TMP/chroot/init"

# pack <cpio mode>: runs the real step; mode "cut" truncates the archive stream
# and exits 0, mode "fail" writes it whole and exits 1, mode "ok" is untouched.
pack() {
    cat > "$TMP/bin/cpio" <<EOF
#!/bin/bash
case " \$* " in *" -o "*) ;; *) exec "$REAL_CPIO" "\$@" ;; esac
case "$1" in
    cut)  "$REAL_CPIO" "\$@" | head -c 65536; exit 0 ;;
    fail) "$REAL_CPIO" "\$@"; exit 1 ;;
    *)    exec "$REAL_CPIO" "\$@" ;;
esac
EOF
    chmod +x "$TMP/bin/cpio"
    PATH="$TMP/bin:$PATH" bash -c '
        set -euo pipefail
        . "$1/lib/microvm-image.sh"
        TAG="[test]" CHROOT_DIR="$2" STAGE_DIR="$3"
        microvm_pack_initramfs
    ' _ "$SCRIPTS_DIR" "$TMP/chroot" "$TMP/stage" > "$TMP/log" 2>&1
}

OUT="$TMP/stage/initramfs.cpio.gz"

for mode in cut fail; do
    printf 'stale\n' > "$OUT"
    if pack "$mode"; then
        fail "$mode: pack succeeded, want failure"
    elif [ -e "$OUT" ] || [ -e "$OUT.partial" ]; then
        fail "$mode: rejected archive left behind"
    else
        pass "$mode: pack fails and leaves no archive"
    fi
done

if ! pack ok; then
    fail "retry after failure: $(tr '\n' ' ' < "$TMP/log")"
elif [ -e "$OUT.partial" ]; then
    fail "retry after failure: partial file left behind"
elif ! zcat "$OUT" | "$REAL_CPIO" -t --quiet 2>/dev/null | grep -qx 'etc/blob'; then
    fail "retry after failure: archive does not list etc/blob"
else
    pass "retry after failure publishes a complete archive"
fi

[ "$fails" -eq 0 ] || exit 1
