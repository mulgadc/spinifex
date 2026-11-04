#!/bin/sh
# build-lambda-runtime-microvm-image_test.sh — a build into a chosen output
# directory stages and publishes there, and leaves the default input directory
# untouched.
#
# The script runs in a fixture repo against the real library, with only the
# rootfs, kernel and packing steps replaced so no image is downloaded.
set -eu

SCRIPTS_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- build-lambda-runtime-microvm-image.sh output directory"

REPO="$TMP/repo"
IN="$REPO/build/microvm/lambda-runtime"
mkdir -p "$REPO/scripts/lib" "$IN" "$REPO/bin" "$TMP/bin"
cp "$SCRIPTS_DIR/build-lambda-runtime-microvm-image.sh" "$REPO/scripts/"
for f in init.sh inittab run-agent; do printf '%s\n' "$f" > "$IN/$f"; done
printf 'default image\n' > "$IN/vmlinuz"
printf '#!/bin/sh\n' > "$REPO/bin/agent-testlink"
chmod +x "$REPO/bin/agent-testlink"
printf '#!/bin/sh\necho "ELF 64-bit, statically linked"\n' > "$TMP/bin/file"
chmod +x "$TMP/bin/file"
BEFORE=$(cd "$IN" && ls -A | cksum)

# The real library, with the steps that need a rootfs or kernel replaced.
# Packing runs from inside the chroot as the real step does, and records where
# the stage is while it still exists.
cat > "$REPO/scripts/lib/microvm-image.sh" <<EOF
. "$SCRIPTS_DIR/lib/microvm-image.sh"
microvm_check_tools() { :; }
microvm_make_rootfs() { mkdir -p "\$CHROOT_DIR/etc"; }
microvm_empty_dev() { :; }
microvm_locate_kernel() { :; }
microvm_strip_modules() { :; }
microvm_strip_rootfs() { :; }
microvm_stage_kernel() { printf 'kernel\n' > "\$STAGE_DIR/vmlinuz"; }
microvm_pack_initramfs() {
    (cd "\$CHROOT_DIR" && printf 'initramfs\n' | gzip > "\$STAGE_DIR/initramfs.cpio.gz")
    dirname "\$STAGE_DIR" > "$TMP/stage-parent"
    ls -A "$IN" > "$TMP/input-during"
}
EOF

# The output is given relative to the repo root, as make passes it.
OUT="$REPO/build/microvm/lambda-testlink-runtime"
if ! (cd "$REPO" && PATH="$TMP/bin:$PATH" bash scripts/build-lambda-runtime-microvm-image.sh bin/agent-testlink build/microvm/lambda-testlink-runtime) > "$TMP/log" 2>&1; then
    fail "build into output dir: $(tr '\n' ' ' < "$TMP/log")"
else
    if [ "$(cat "$TMP/stage-parent")" = "$OUT" ]; then
        pass "stage created in the output directory"
    else
        fail "stage created in $(cat "$TMP/stage-parent"), want $OUT"
    fi
    if grep -q '^\.stage\.' "$TMP/input-during"; then
        fail "stage present in the default input directory during the build"
    else
        pass "no stage in the default input directory during the build"
    fi
    if [ "$(cd "$IN" && ls -A | cksum)" = "$BEFORE" ] && grep -qx 'default image' "$IN/vmlinuz"; then
        pass "default input directory unchanged"
    else
        fail "default input directory changed: $(cd "$IN" && ls -A | tr '\n' ' ')"
    fi
    if grep -qx kernel "$OUT/vmlinuz" && gzip -t "$OUT/initramfs.cpio.gz" && [ "$(ls -A "$OUT" | wc -l)" -eq 2 ]; then
        pass "output directory holds exactly the published pair"
    else
        fail "output directory: $(ls -A "$OUT" | tr '\n' ' ')"
    fi
fi

if [ "$fails" -ne 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
