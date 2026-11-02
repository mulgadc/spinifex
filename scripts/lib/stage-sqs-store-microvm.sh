#!/bin/sh
# stage-sqs-store-microvm.sh <image-dir> <dist-root> [sqs-worker]: copy the
# built SQS microVM image into a release tree as <dist-root>/microvm-sqs-worker.
# Without a complete image it stages nothing and says so, as the LB image does.
set -eu
src=$1 name=${3:-sqs-worker}
case "$name" in
    sqs-worker) label="SQS worker" ;;
    *) echo "[distro] unknown SQS microVM image '$name'" >&2; exit 2 ;;
esac
dest=$2/microvm-$name
# An image staged by an earlier build must not ship with this one. Only the
# two image files are removed; anything else in the directory is left alone.
rm -f "$dest/vmlinuz" "$dest/initramfs.cpio.gz"
if [ -s "$src/vmlinuz" ] && [ -s "$src/initramfs.cpio.gz" ]; then
    echo "[distro] staging the $label microVM image into the tarball"
    mkdir -p "$dest"
    cp "$src/vmlinuz" "$src/initramfs.cpio.gz" "$dest/"
else
    [ ! -d "$dest" ] || rmdir "$dest" 2> /dev/null || true
    echo "[distro] WARNING: $label microVM image missing; the tarball will not include it"
    echo "[distro]          run 'make build-$name-microvm-image' first"
fi
