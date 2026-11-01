#!/bin/sh
# sqs-store-microvm-publish_test.sh — a staged SQS microVM pair reaches
# the output directory only when every gate passes, and a failed build cannot
# leave a pair that make then takes as up to date.
#
# Artifacts are small fake archives listing the gated paths; no image is built.
set -eu

LIB_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
PUBLISH="$LIB_DIR/sqs-store-microvm-publish.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- lib/sqs-store-microvm-publish.sh"

for tool in cpio gzip make; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "  FAIL - required tool not found: $tool"
        exit 1
    fi
done

# stage <dir> [omit-path] — a fake staged pair; omit-path is left out of the archive.
stage() {
    dir=$1 omit=${2:-}
    root="$TMP/root.$$.$(date +%s%N)"
    for p in init etc/inittab usr/local/bin/nats-server usr/local/bin/sqs-worker data sbin/mkfs.ext4 sbin/e2fsck \
        usr/local/lib/sqs-worker/sqs-worker-lib.sh usr/local/lib/sqs-worker/sqs-worker-prepare \
        usr/local/lib/sqs-worker/sqs-worker-store usr/local/lib/sqs-worker/sqs-worker-ready \
        usr/local/lib/sqs-worker/sqs-worker-stop \
        dev/console dev/null lib/modules/6.12-virt/kernel/fs/ext4/ext4.ko \
        lib/modules/6.12-virt/kernel/drivers/acpi/tiny-power-button.ko; do
        [ "$p" = "$omit" ] && continue
        mkdir -p "$root/$(dirname "$p")"
        echo "$p" > "$root/$p"
    done
    # pad=N appends N files after the gated ones, as a real image's listing has.
    if [ "${pad:-0}" -gt 0 ]; then
        mkdir -p "$root/zz"
        (cd "$root/zz" && seq "$pad" | xargs touch)
    fi
    mkdir -p "$dir"
    (cd "$root" && find . -mindepth 1 | sed 's|^\./||' | sort | cpio --quiet -o -H newc) | gzip > "$dir/initramfs.cpio.gz"
    echo kernel > "$dir/vmlinuz"
    rm -rf "$root"
}

# fingerprint <dir> — content and mtime of both outputs, or "absent".
fingerprint() {
    for f in "$1/vmlinuz" "$1/initramfs.cpio.gz"; do
        if [ -e "$f" ]; then
            printf '%s %s ' "$(cksum < "$f")" "$(stat -c %Y "$f")"
        else
            printf 'absent '
        fi
    done
}

# --- A valid pair is published and leaves the stage ---
stage "$TMP/ok/stage"
if bash "$PUBLISH" "$TMP/ok/stage" "$TMP/ok/out" >/dev/null 2>&1 \
    && [ -s "$TMP/ok/out/vmlinuz" ] && [ -s "$TMP/ok/out/initramfs.cpio.gz" ] \
    && [ ! -e "$TMP/ok/stage/vmlinuz" ] && [ ! -e "$TMP/ok/stage/initramfs.cpio.gz" ]; then
    pass "valid pair is published"
else
    fail "valid pair is published"
fi

# --- A large listing with the modules early still passes the module gate ---
pad=20000 stage "$TMP/large/stage"
if bash "$PUBLISH" "$TMP/large/stage" "$TMP/large/out" >/dev/null 2>&1; then
    pass "large archive with early modules is published"
else
    fail "large archive with early modules is published"
fi

# --- Each failing gate keeps the previous valid pair exactly ---
for case in nats-server worker-store ext4 size; do
    d="$TMP/keep-$case"
    stage "$d/prev"
    bash "$PUBLISH" "$d/prev" "$d/out" >/dev/null 2>&1
    before=$(fingerprint "$d/out")
    max=50
    case $case in
        nats-server) stage "$d/stage" usr/local/bin/nats-server ;;
        worker-store) stage "$d/stage" usr/local/lib/sqs-worker/sqs-worker-store ;;
        ext4) stage "$d/stage" lib/modules/6.12-virt/kernel/fs/ext4/ext4.ko ;;
        size) stage "$d/stage"; max=0 ;;
    esac
    if MICROVM_ARTIFACT_MAX_MiB=$max bash "$PUBLISH" "$d/stage" "$d/out" >/dev/null 2>&1; then
        fail "$case gate failure is refused"
    elif [ "$(fingerprint "$d/out")" = "$before" ]; then
        pass "$case gate failure keeps the previous pair"
    else
        fail "$case gate failure keeps the previous pair"
    fi
done

# --- With no previous pair, a failed gate publishes nothing ---
stage "$TMP/none/stage" usr/local/bin/nats-server
if bash "$PUBLISH" "$TMP/none/stage" "$TMP/none/out" >/dev/null 2>&1; then
    fail "failed gate with no previous pair is refused"
elif [ "$(fingerprint "$TMP/none/out")" = "absent absent " ]; then
    pass "failed gate with no previous pair publishes nothing"
else
    fail "failed gate with no previous pair publishes nothing"
fi

# --- make cannot take a failed rebuild's pair as up to date ---
# The same grouped-target shape as the Makefile; the builder stages a pair
# missing nats-server once BAD exists, the way a failed real build would.
m="$TMP/make"
mkdir -p "$m/out"
cat > "$m/build.sh" <<EOF
#!/bin/sh
set -eu
rm -rf "$m/stage"
. "$m/build.sh.lib"
if [ -e "$m/BAD" ]; then
    stage "$m/stage" usr/local/bin/nats-server
else
    stage "$m/stage"
fi
bash "$PUBLISH" "$m/stage" "$m/out"
EOF
{
    echo "TMP=\"$TMP\""
    sed -n '/^stage() {/,/^}/p' "$0"
} > "$m/build.sh.lib"
printf 'out/vmlinuz out/initramfs.cpio.gz &: input\n\tsh build.sh\n' > "$m/Makefile"
echo v1 > "$m/input"
if (cd "$m" && make -s out/vmlinuz >/dev/null 2>&1) && [ -s "$m/out/vmlinuz" ]; then
    pass "make builds the first valid pair"
else
    fail "make builds the first valid pair"
    echo "  $fails failure(s)"; exit 1
fi
good=$(fingerprint "$m/out")
touch "$m/BAD"
# Age the published pair so the input is strictly newer, without a future mtime.
past="@$(( $(date +%s) - 10 ))"
touch -d "$past" "$m/out/vmlinuz" "$m/out/initramfs.cpio.gz"
touch "$m/input"
good=$(fingerprint "$m/out")
if (cd "$m" && make -s out/vmlinuz >/dev/null 2>&1); then
    fail "make reports the failed rebuild"
else
    pass "make reports the failed rebuild"
fi
if (cd "$m" && make -q out/vmlinuz out/initramfs.cpio.gz); then
    fail "after a failed rebuild make still sees the pair as stale"
else
    pass "after a failed rebuild make still sees the pair as stale"
fi
if [ "$(fingerprint "$m/out")" = "$good" ]; then
    pass "failed rebuild leaves the previous valid pair"
else
    fail "failed rebuild leaves the previous valid pair"
fi

# --- A publish stopped after the kernel lands leaves no mixed pair ---
# A stub mv lets the first rename through and fails the second: the point at
# which an interrupt would leave a new kernel beside an old initramfs.
i="$TMP/interrupt"
mkdir -p "$i/bin"
real_mv=$(command -v mv)
{
    echo '#!/bin/sh'
    echo "if [ -e '$i/moved' ]; then exit 1; fi"
    echo ": > '$i/moved'"
    echo "exec '$real_mv' \"\$@\""
} > "$i/bin/mv"
chmod +x "$i/bin/mv"
stage "$i/prev"
bash "$PUBLISH" "$i/prev" "$i/out" >/dev/null 2>&1
stage "$i/stage"
echo new-kernel > "$i/stage/vmlinuz"
if PATH="$i/bin:$PATH" bash "$PUBLISH" "$i/stage" "$i/out" >/dev/null 2>&1; then
    fail "interrupted publish is reported"
elif [ "$(cat "$i/out/vmlinuz")" = new-kernel ] && [ ! -e "$i/out/initramfs.cpio.gz" ]; then
    pass "interrupted publish leaves no initramfs beside the new kernel"
else
    fail "interrupted publish leaves no initramfs beside the new kernel"
fi
printf 'out/vmlinuz out/initramfs.cpio.gz &: input\n\ttrue\n' > "$i/Makefile"
touch -d "@$(( $(date +%s) - 10 ))" "$i/input"
if (cd "$i" && make -q out/vmlinuz out/initramfs.cpio.gz); then
    fail "make does not accept the interrupted publish"
else
    pass "make does not accept the interrupted publish"
fi

[ "$fails" -eq 0 ] || { echo "  $fails failure(s)"; exit 1; }
