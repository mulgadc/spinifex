#!/bin/sh
# stage-sqs-store-microvm.sh <image-dir> <dist-root>: copy a built SQS store
# microVM image into a release tree as <dist-root>/microvm-sqs-store. Without a
# complete image it stages nothing and says so, as the load balancer image does.
set -eu
src=$1 dest=$2/microvm-sqs-store
# An image staged by an earlier build must not ship with this one. Only the
# two image files are removed; anything else in the directory is left alone.
rm -f "$dest/vmlinuz" "$dest/initramfs.cpio.gz"
if [ -s "$src/vmlinuz" ] && [ -s "$src/initramfs.cpio.gz" ]; then
    echo "[distro] staging the SQS store microVM image into the tarball"
    mkdir -p "$dest"
    cp "$src/vmlinuz" "$src/initramfs.cpio.gz" "$dest/"
else
    [ ! -d "$dest" ] || rmdir "$dest" 2> /dev/null || true
    echo "[distro] WARNING: SQS store microVM image missing; the tarball will not include it"
    echo "[distro]          run 'make build-sqs-store-microvm-image' first"
fi
