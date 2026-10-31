#!/bin/sh
# setup_sqs_store_image_test.sh — tests for shipping the SQS store and worker
# microVM images: staging them into a release tree and installing with setup.sh.
#
# setup.sh is sourced with INSTALL_SPINIFEX_LIB_ONLY=1 so main never runs, and
# installs into a temporary SPINIFEX_MICROVM_DIR with SUDO cleared.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SETUP="$SCRIPT_DIR/setup.sh"
STAGE="$SCRIPT_DIR/lib/stage-sqs-store-microvm.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- SQS store and worker microVM image release and install"

# image <dir> <tag>: a fake built image whose bytes name its tag.
image() {
    mkdir -p "$1"
    printf 'kernel-%s\n' "$2" > "$1/vmlinuz"
    printf 'initramfs-%s\n' "$2" > "$1/initramfs.cpio.gz"
}

# release <dir>: a release tree with the load balancer image, as distro-amd64 builds it.
release() {
    rm -rf "$1"
    mkdir -p "$1/microvm"
    printf 'lb-kernel\n' > "$1/microvm/vmlinuz"
    printf 'lb-initramfs\n' > "$1/microvm/initramfs.cpio.gz"
    printf 'qboot\n' > "$1/microvm/qboot.rom"
}

# install_image <extract-dir> <microvm-dir> [path-prefix]: runs
# install_sqs_store_image and sets RC and OUT.
install_image() {
    RC=0
    OUT=$(PATH="${3:-}$PATH" EXTRACT_DIR="$1" SPINIFEX_MICROVM_DIR="$2" INSTALL_SPINIFEX_LIB_ONLY=1 \
        bash -c '. "$1"; SUDO=""; install_sqs_store_image' _ "$SETUP" 2>&1) || RC=$?
}

same() { cmp -s "$1" "$2"; }

# --- staging ---
image "$TMP/built" v1
release "$TMP/rel"
sh "$STAGE" "$TMP/built" "$TMP/rel" > "$TMP/stage.out"
if same "$TMP/built/vmlinuz" "$TMP/rel/microvm-sqs-store/vmlinuz" \
    && same "$TMP/built/initramfs.cpio.gz" "$TMP/rel/microvm-sqs-store/initramfs.cpio.gz"; then
    pass "stages the built image beside the load balancer's"
else
    fail "staged image differs from the build"
fi
if [ ! -e "$TMP/rel/microvm/sqs-store" ] && [ "$(ls "$TMP/rel/microvm" | wc -l)" -eq 3 ]; then
    pass "leaves the load balancer's microvm directory as it was"
else
    fail "load balancer directory changed: $(ls "$TMP/rel/microvm")"
fi

release "$TMP/rel-none"
mkdir -p "$TMP/half"; printf 'k\n' > "$TMP/half/vmlinuz"
OUT=$(sh "$STAGE" "$TMP/half" "$TMP/rel-none")
if [ ! -e "$TMP/rel-none/microvm-sqs-store" ] && echo "$OUT" | grep -q 'WARNING: SQS store microVM image missing'; then
    pass "stages nothing from an incomplete build, and says so"
else
    fail "incomplete build staged or unannounced: $OUT"
fi

# --- an older installer meets a release that carries the store image ---
# It copies microvm/* without -r; the sibling directory must not reach it.
mkdir -p "$TMP/old-dest"
if cp "$TMP/rel/microvm/"* "$TMP/old-dest/" 2> "$TMP/old.err" && [ ! -e "$TMP/old-dest/sqs-store" ]; then
    pass "an older installer's copy still succeeds and ignores the store image"
else
    fail "older installer copy failed: $(cat "$TMP/old.err")"
fi
mkdir -p "$TMP/nested/microvm/sqs-store" "$TMP/nested-dest"
printf 'x\n' > "$TMP/nested/microvm/vmlinuz"
if cp "$TMP/nested/microvm/"* "$TMP/nested-dest/" 2> /dev/null; then
    fail "control: a nested directory did not break the older copy, so the test proves nothing"
else
    pass "control: a nested directory would have broken the older copy"
fi

# --- install ---
install_image "$TMP/rel" "$TMP/mv"
if [ "$RC" = 0 ] && same "$TMP/built/vmlinuz" "$TMP/mv/sqs-store/vmlinuz" \
    && same "$TMP/built/initramfs.cpio.gz" "$TMP/mv/sqs-store/initramfs.cpio.gz"; then
    pass "installs the staged image where the daemon looks"
else
    fail "install rc=$RC: $OUT"
fi
mode=$(stat -c '%a' "$TMP/mv/sqs-store/initramfs.cpio.gz" 2> /dev/null || echo none)
if [ "$mode" = 644 ]; then pass "installs the image 0644"; else fail "initramfs mode $mode"; fi

image "$TMP/built2" v2
release "$TMP/rel2"
sh "$STAGE" "$TMP/built2" "$TMP/rel2" > /dev/null
install_image "$TMP/rel2" "$TMP/mv"
if [ "$RC" = 0 ] && same "$TMP/built2/vmlinuz" "$TMP/mv/sqs-store/vmlinuz" \
    && same "$TMP/built2/initramfs.cpio.gz" "$TMP/mv/sqs-store/initramfs.cpio.gz"; then
    pass "an upgrade replaces both files"
else
    fail "upgrade rc=$RC: $OUT"
fi

# A release without the store image: nothing installed, and a clear warning.
install_image "$TMP/rel-none" "$TMP/mv-none"
if [ "$RC" = 0 ] && [ ! -e "$TMP/mv-none/sqs-store" ] && echo "$OUT" | grep -q 'no SQS store microVM image; SQS store launches will be refused'; then
    pass "a release without the image installs nothing and warns"
else
    fail "release without image: rc=$RC exists=$([ -e "$TMP/mv-none/sqs-store" ] && echo yes || echo no) $OUT"
fi

# installed <dir>: a store image from an earlier install, plus an unrelated file.
installed() {
    image "$1/sqs-store" old
    printf 'keep\n' > "$1/sqs-store/operator-note"
}
gone() { [ ! -e "$1/sqs-store/vmlinuz" ] && [ ! -e "$1/sqs-store/initramfs.cpio.gz" ]; }
kept() { [ "$(cat "$1/sqs-store/operator-note" 2> /dev/null)" = keep ]; }

# Upgrading to a release without the image must not leave the old one usable.
installed "$TMP/mv-up"
install_image "$TMP/rel-none" "$TMP/mv-up"
if [ "$RC" = 0 ] && gone "$TMP/mv-up" && kept "$TMP/mv-up" \
    && echo "$OUT" | grep -q 'Removed the SQS store microVM image left by an earlier install' \
    && echo "$OUT" | grep -q 'SQS store launches will be refused'; then
    pass "a release without the image removes the earlier one, keeps other files, and warns"
else
    fail "release without image over an installed one: rc=$RC $OUT"
fi

# An incomplete store image refuses and removes the earlier pair, because the
# binaries this release installed before it would otherwise boot the old one.
installed "$TMP/mv-bad"
cp -r "$TMP/rel2" "$TMP/rel-bad"
rm "$TMP/rel-bad/microvm-sqs-store/initramfs.cpio.gz"
install_image "$TMP/rel-bad" "$TMP/mv-bad"
if [ "$RC" != 0 ] && echo "$OUT" | grep -q 'incomplete: initramfs.cpio.gz is missing' \
    && gone "$TMP/mv-bad" && kept "$TMP/mv-bad"; then
    pass "an incomplete image is refused and the earlier pair is removed"
else
    fail "incomplete image: rc=$RC $OUT"
fi

# Restaging from a missing or incomplete build drops an image staged earlier.
for case in missing incomplete; do
    image "$TMP/built-$case" new
    release "$TMP/rel-$case"
    sh "$STAGE" "$TMP/built" "$TMP/rel-$case" > /dev/null
    if [ "$case" = missing ]; then rm -r "$TMP/built-$case"; else rm "$TMP/built-$case/initramfs.cpio.gz"; fi
    OUT=$(sh "$STAGE" "$TMP/built-$case" "$TMP/rel-$case")
    if [ ! -e "$TMP/rel-$case/microvm-sqs-store" ] && echo "$OUT" | grep -q 'tarball will not include it'; then
        pass "restaging from a $case build ships no earlier image"
    else
        fail "restaging from a $case build left: $(ls -R "$TMP/rel-$case/microvm-sqs-store" 2> /dev/null)"
    fi
done
mkdir -p "$TMP/rel-other/microvm-sqs-store"
image "$TMP/rel-other/microvm-sqs-store" old
printf 'keep\n' > "$TMP/rel-other/microvm-sqs-store/note"
sh "$STAGE" "$TMP/no-such-build" "$TMP/rel-other" > /dev/null
if [ ! -e "$TMP/rel-other/microvm-sqs-store/vmlinuz" ] && [ "$(cat "$TMP/rel-other/microvm-sqs-store/note")" = keep ]; then
    pass "restaging removes only the image files"
else
    fail "restaging touched more than the image files"
fi

# An install that stops at the initramfs leaves no pair the daemon would boot.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/install" << 'EOF'
#!/bin/sh
for a in "$@"; do last=$a; done
case "$last" in */initramfs.cpio.gz) echo "install: stopped" >&2; exit 1 ;; esac
exec /usr/bin/install "$@"
EOF
chmod +x "$TMP/bin/install"
install_image "$TMP/rel" "$TMP/mv" "$TMP/bin:"
if [ "$RC" != 0 ] && same "$TMP/built/vmlinuz" "$TMP/mv/sqs-store/vmlinuz" && [ ! -e "$TMP/mv/sqs-store/initramfs.cpio.gz" ]; then
    pass "an interrupted install leaves the initramfs missing, not mismatched"
else
    fail "interrupted install: rc=$RC initramfs=$([ -e "$TMP/mv/sqs-store/initramfs.cpio.gz" ] && echo present || echo missing)"
fi

# --- SQS worker image: same staging and install path, its own directory ---
install_worker() {
    RC=0
    OUT=$(EXTRACT_DIR="$1" SPINIFEX_MICROVM_DIR="$2" INSTALL_SPINIFEX_LIB_ONLY=1 \
        bash -c '. "$1"; SUDO=""; install_sqs_worker_image' _ "$SETUP" 2>&1) || RC=$?
}
image "$TMP/wbuilt" w1
release "$TMP/wrel"
sh "$STAGE" "$TMP/built" "$TMP/wrel" > /dev/null
sh "$STAGE" "$TMP/wbuilt" "$TMP/wrel" sqs-worker > /dev/null
if same "$TMP/wbuilt/vmlinuz" "$TMP/wrel/microvm-sqs-worker/vmlinuz" \
    && same "$TMP/wbuilt/initramfs.cpio.gz" "$TMP/wrel/microvm-sqs-worker/initramfs.cpio.gz" \
    && same "$TMP/built/vmlinuz" "$TMP/wrel/microvm-sqs-store/vmlinuz"; then
    pass "the worker image stages beside the store image"
else
    fail "worker staging: $(ls -R "$TMP/wrel")"
fi
install_worker "$TMP/wrel" "$TMP/wmv"
if [ "$RC" = 0 ] && same "$TMP/wbuilt/initramfs.cpio.gz" "$TMP/wmv/sqs-worker/initramfs.cpio.gz" \
    && [ ! -e "$TMP/wmv/sqs-store" ]; then
    pass "the worker image installs into its own directory only"
else
    fail "worker install: rc=$RC $OUT"
fi
release "$TMP/wrel-none"
OUT=$(sh "$STAGE" "$TMP/no-such-build" "$TMP/wrel-none" sqs-worker)
if [ ! -e "$TMP/wrel-none/microvm-sqs-worker" ] && echo "$OUT" | grep -q 'SQS worker microVM image missing'; then
    pass "a missing worker build stages nothing and says so"
else
    fail "missing worker build: $OUT"
fi
install_worker "$TMP/wrel-none" "$TMP/wmv"
if [ "$RC" = 0 ] && [ ! -e "$TMP/wmv/sqs-worker/vmlinuz" ] && [ ! -e "$TMP/wmv/sqs-worker/initramfs.cpio.gz" ] \
    && echo "$OUT" | grep -q 'no SQS worker microVM image; SQS worker launches will be refused'; then
    pass "a release without the worker image removes the earlier one and warns"
else
    fail "release without worker image: rc=$RC $OUT"
fi
cp -r "$TMP/wrel" "$TMP/wrel-bad"
rm "$TMP/wrel-bad/microvm-sqs-worker/vmlinuz"
install_worker "$TMP/wrel" "$TMP/wmv"
install_worker "$TMP/wrel-bad" "$TMP/wmv"
if [ "$RC" != 0 ] && echo "$OUT" | grep -q 'SQS worker microVM image in this release is incomplete: vmlinuz is missing' \
    && [ ! -e "$TMP/wmv/sqs-worker/initramfs.cpio.gz" ]; then
    pass "an incomplete worker image is refused and the earlier pair is removed"
else
    fail "incomplete worker image: rc=$RC $OUT"
fi
if OUT=$(sh "$STAGE" "$TMP/wbuilt" "$TMP/wrel" lb 2>&1); then
    fail "an unknown image name was staged"
else
    pass "an unknown image name is refused"
fi

[ "$fails" = 0 ] || { echo "FAIL: $fails"; exit 1; }
echo "  setup_sqs_store_image_test ok"
