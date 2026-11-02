#!/bin/bash
# sqs-store-microvm-publish.sh STAGE_DIR OUT_DIR [PROFILE] — gate a staged SQS
# store (default) or worker microVM kernel + initramfs and move them into
# OUT_DIR only if every gate passes.
#
# A failed gate leaves OUT_DIR as it was: a previous valid pair stays, and an
# older pair stays older than the build inputs, so make rebuilds next time.
set -euo pipefail

STAGE_DIR="${1:?usage: sqs-store-microvm-publish.sh STAGE_DIR OUT_DIR}"
OUT_DIR="${2:?usage: sqs-store-microvm-publish.sh STAGE_DIR OUT_DIR}"
VMLINUZ="$STAGE_DIR/vmlinuz"
INITRAMFS="$STAGE_DIR/initramfs.cpio.gz"
PROFILE="${3:-worker}"
case "$PROFILE" in
    worker)
        payload="usr/local/bin/nats-server usr/local/bin/sqs-worker usr/local/lib/sqs-worker/sqs-worker-lib.sh usr/local/lib/sqs-worker/sqs-worker-prepare usr/local/lib/sqs-worker/sqs-worker-store usr/local/lib/sqs-worker/sqs-worker-ready usr/local/lib/sqs-worker/sqs-worker-stop data" ;;
    *) echo "ERROR: profile must be worker, not $PROFILE" >&2; exit 1 ;;
esac
tag="[build-sqs-$PROFILE-microvm-image]"

for f in "$VMLINUZ" "$INITRAMFS"; do
    if [ ! -s "$f" ]; then
        echo "ERROR: staged artifact $f missing or empty" >&2
        exit 1
    fi
done

# --- Content gate: read the packed archive, not the chroot ---
if ! archive_list=$(zcat "$INITRAMFS" | cpio --quiet -t); then
    echo "ERROR: $INITRAMFS is not a readable gzip cpio archive" >&2
    exit 1
fi
# shellcheck disable=SC2086 # payload is a word list on purpose
for want in init etc/inittab dev/console dev/null $payload; do
    if ! grep -qx "$want" <<< "$archive_list"; then
        echo "ERROR: $want missing from $INITRAMFS" >&2
        exit 1
    fi
done
for fs_tool in mkfs.ext4 e2fsck; do
    if ! grep -qxE "(usr/)?sbin/$fs_tool" <<< "$archive_list"; then
        echo "ERROR: $fs_tool missing from $INITRAMFS" >&2
        exit 1
    fi
done
# Module files may spell a name with dashes (tiny-power-button.ko). Normalise
# once: piping into grep -q under pipefail fails when grep exits on a match.
module_list=${archive_list//-/_}
for want_ko in ext4 tiny_power_button; do
    if ! grep -q "/$want_ko\.ko\$" <<< "$module_list"; then
        echo "ERROR: $want_ko missing from $INITRAMFS" >&2
        exit 1
    fi
done
if grep -qE '(^|/)(lb-agent|haproxy|nginx)$' <<< "$archive_list"; then
    echo "ERROR: LB data-plane binaries found in the SQS image" >&2
    exit 1
fi
echo "$tag content gate: PASS"

echo ""
echo "$tag staged artifacts:"
ls -lh "$VMLINUZ" "$INITRAMFS"
echo ""

# --- Artifact size gate (50 MiB total) ---
ARTIFACT_MAX_MiB="${MICROVM_ARTIFACT_MAX_MiB:-50}"
total_bytes=$(( $(stat -c %s "$VMLINUZ") + $(stat -c %s "$INITRAMFS") ))
max_bytes=$(( ARTIFACT_MAX_MiB * 1024 * 1024 ))
total_mib=$(( (total_bytes + 1048575) / 1048576 ))
if [ "$total_bytes" -gt "$max_bytes" ]; then
    echo "ERROR: artifact size ${total_mib} MiB exceeds ${ARTIFACT_MAX_MiB} MiB gate" >&2
    echo "       Set MICROVM_ARTIFACT_MAX_MiB=N to override during development." >&2
    exit 1
fi
echo "$tag artifact size gate: PASS (${total_mib} MiB / ${ARTIFACT_MAX_MiB} MiB)"

# --- Guest-RAM initramfs gate (uncompressed) ---
# The kernel unpacks the whole initramfs into RAM before init, so the
# UNCOMPRESSED size — not the gzip artifact — sets the guest memory floor.
UNCOMPRESSED_MAX_MiB="${MICROVM_INITRAMFS_UNCOMPRESSED_MAX_MiB:-96}"
uncompressed_bytes=$(zcat "$INITRAMFS" | wc -c)
uncompressed_mib=$(( (uncompressed_bytes + 1048575) / 1048576 ))
umax_bytes=$(( UNCOMPRESSED_MAX_MiB * 1024 * 1024 ))
if [ "$uncompressed_bytes" -gt "$umax_bytes" ]; then
    echo "ERROR: uncompressed initramfs ${uncompressed_mib} MiB exceeds ${UNCOMPRESSED_MAX_MiB} MiB guest-RAM gate" >&2
    echo "       The kernel unpacks this into guest RAM before init; a larger" >&2
    echo "       footprint panics before userspace. Slim the initramfs, or raise the" >&2
    echo "       instance memory AND MICROVM_INITRAMFS_UNCOMPRESSED_MAX_MiB together." >&2
    exit 1
fi
echo "$tag initramfs guest-RAM gate: PASS (${uncompressed_mib} MiB / ${UNCOMPRESSED_MAX_MiB} MiB uncompressed)"

# --- Publish ---
# Two renames are not atomic as a pair, so the old initramfs goes first and the
# new one lands last. Stopped in between, the initramfs is absent: no mixed pair
# exists and the grouped make target is missing, so make rebuilds.
mkdir -p "$OUT_DIR"
rm -f "$OUT_DIR/initramfs.cpio.gz"
mv -f "$VMLINUZ" "$OUT_DIR/vmlinuz"
mv -f "$INITRAMFS" "$OUT_DIR/initramfs.cpio.gz"
echo "$tag published: $OUT_DIR/vmlinuz $OUT_DIR/initramfs.cpio.gz"
