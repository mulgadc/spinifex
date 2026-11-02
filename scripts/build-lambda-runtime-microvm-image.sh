#!/bin/bash
# build-lambda-runtime-microvm-image.sh — build the Lambda runtime microVM kernel
# + initramfs: a guest running the Lambda guest agent, with virtio-net for its
# host control connection. It is not shipped.
#
# Inputs:
#   $REPO_ROOT/build/microvm/lambda-runtime/{init.sh,inittab,run-agent}
#   $REPO_ROOT/bin/lambda-agent  (make bin/lambda-agent)
#
# Outputs:
#   $REPO_ROOT/build/microvm/lambda-runtime/vmlinuz
#   $REPO_ROOT/build/microvm/lambda-runtime/initramfs.cpio.gz
#
# Rootfs strategy and module stripping: scripts/lib/microvm-image.sh.
# shellcheck disable=SC2034 # variables are consumed by the sourced library
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="$REPO_ROOT/build/microvm/lambda-runtime"
INIT_SH="$BUILD_DIR/init.sh"
INITTAB="$BUILD_DIR/inittab"
RUN_AGENT="$BUILD_DIR/run-agent"
LAMBDA_AGENT_BIN="$REPO_ROOT/bin/lambda-agent"
TAG="[build-lambda-runtime-microvm-image]"

ALPINE_VERSION="${ALPINE_VERSION:-3.24}"
ALPINE_MIRROR="${ALPINE_MIRROR:-https://dl-cdn.alpinelinux.org/alpine}"
# kmod: the image's own depmod/modprobe rebuild module metadata. busybox ships ip.
ALPINE_PACKAGES="busybox linux-virt kmod ca-certificates"
# tiny_power_button turns the ACPI power button into a signal busybox init
# handles as poweroff.
MICROVM_EXTRA_MODULES="${MICROVM_EXTRA_MODULES:-tiny_power_button}"
# virtio_net needs failover and net_failover, which the virtio* glob misses.
MICROVM_EXTRA_KEEP_GLOBS=("*failover*.ko*")
MICROVM_GATE_MODULES="qemu_fw_cfg virtio_net"

# shellcheck source=lib/microvm-image.sh
source "$REPO_ROOT/scripts/lib/microvm-image.sh"

echo "$TAG repo root: $REPO_ROOT"
echo "$TAG build dir: $BUILD_DIR"

microvm_check_tools

for f in "$INIT_SH" "$INITTAB" "$RUN_AGENT"; do
    if [ ! -f "$f" ]; then
        echo "ERROR: $f not found — cannot build initramfs" >&2
        exit 1
    fi
done
if [ ! -x "$LAMBDA_AGENT_BIN" ]; then
    echo "ERROR: $LAMBDA_AGENT_BIN not found — run 'make bin/lambda-agent'" >&2
    exit 1
fi
if ! file -b "$LAMBDA_AGENT_BIN" | grep -q "statically linked"; then
    echo "ERROR: $LAMBDA_AGENT_BIN is not statically linked — it cannot run on the musl guest" >&2
    exit 1
fi

microvm_init_dirs
microvm_make_rootfs
microvm_empty_dev

echo "$TAG installing init, inittab, run-agent and lambda-agent..."
install -m 0755 "$INIT_SH" "$CHROOT_DIR/init"
install -m 0644 "$INITTAB" "$CHROOT_DIR/etc/inittab"
mkdir -p "$CHROOT_DIR/usr/local/lib/lambda-runtime" "$CHROOT_DIR/usr/local/bin"
install -m 0755 "$RUN_AGENT" "$CHROOT_DIR/usr/local/lib/lambda-runtime/run-agent"
install -m 0755 "$LAMBDA_AGENT_BIN" "$CHROOT_DIR/usr/local/bin/lambda-agent"
# Function-code mountpoint and scratch space ship empty.
mkdir -p "$CHROOT_DIR/var/task" "$CHROOT_DIR/tmp" \
    "$CHROOT_DIR/proc" "$CHROOT_DIR/sys" "$CHROOT_DIR/dev" "$CHROOT_DIR/etc/ssl/certs"
# The bootstrap runs unprivileged, so / must be traversable (mktemp -d makes it
# 0700) and nothing it runs from may be group-writable.
chmod 0755 "$CHROOT_DIR" "$CHROOT_DIR/var/task" "$CHROOT_DIR/usr/local/lib/lambda-runtime"

microvm_locate_kernel
microvm_strip_modules
microvm_strip_rootfs
microvm_stage_kernel
microvm_pack_initramfs

# Nothing reaches $BUILD_DIR until the staged pair passes these checks.
for out in vmlinuz initramfs.cpio.gz; do
    if [ ! -s "$STAGE_DIR/$out" ]; then
        echo "ERROR: staged $out is missing or empty" >&2
        exit 1
    fi
done
if ! gzip -t "$STAGE_DIR/initramfs.cpio.gz"; then
    echo "ERROR: staged initramfs.cpio.gz is not valid gzip" >&2
    exit 1
fi
mv -f "$STAGE_DIR/vmlinuz" "$BUILD_DIR/vmlinuz"
mv -f "$STAGE_DIR/initramfs.cpio.gz" "$BUILD_DIR/initramfs.cpio.gz"
echo ""
echo "$TAG done."
