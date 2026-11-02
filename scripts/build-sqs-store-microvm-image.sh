#!/bin/bash
# build-sqs-store-microvm-image.sh — build the SQS microVM kernel + initramfs:
# one guest running the dedicated SQS nats-server and the SQS worker as separate
# processes, ext4 support, its guest steps and a clean power-down. The guest
# never formats a disk. The LB image (build-microvm-image.sh) is separate.
#
# Inputs:
#   $REPO_ROOT/build/microvm/sqs-worker/init.sh, inittab
#   $REPO_ROOT/build/microvm/sqs-worker/sqs-worker-{lib.sh,prepare,ready,stop}
#   $REPO_ROOT/bin/nats-server  (make bin/nats-server, the go.mod-pinned version)
#   $REPO_ROOT/bin/sqs-worker
#
# Outputs:
#   $REPO_ROOT/build/microvm/sqs-worker/vmlinuz
#   $REPO_ROOT/build/microvm/sqs-worker/initramfs.cpio.gz
#
# Rootfs strategy (first match wins):
#   1. apk available on host → install directly into a temp chroot (Alpine CI/CD)
#   2. docker available      → run alpine container, export filesystem
#   3. podman available      → same via podman
# shellcheck disable=SC2034 # variables are consumed by the sourced library
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="$REPO_ROOT/build/microvm/sqs-worker"
INIT_SH="$BUILD_DIR/init.sh"
INITTAB="$BUILD_DIR/inittab"
NATS_SERVER_BIN="$REPO_ROOT/bin/nats-server"
SQS_WORKER_BIN="$REPO_ROOT/bin/sqs-worker"

PROFILE="${SQS_MICROVM_PROFILE:-worker}"
case "$PROFILE" in
    worker) ;;
    *) echo "ERROR: SQS_MICROVM_PROFILE must be worker, not $PROFILE" >&2; exit 1 ;;
esac
TAG="[build-sqs-$PROFILE-microvm-image]"

ALPINE_VERSION="${ALPINE_VERSION:-3.24}"
ALPINE_MIRROR="${ALPINE_MIRROR:-https://dl-cdn.alpinelinux.org/alpine}"
# e2fsprogs: mkfs.ext4/e2fsck for a guest that mounts an ext4 data volume.
# kmod: the image's own depmod/modprobe rebuild module metadata below.
ALPINE_PACKAGES="busybox linux-virt iproute2 ca-certificates e2fsprogs kmod"
# Modules kept by name on top of the virtio/fw_cfg/failover globs, each with its
# full depends= and softdep closure. tiny_power_button turns the ACPI power
# button (QMP system_powerdown) into a signal busybox init handles as poweroff.
MICROVM_EXTRA_MODULES="${MICROVM_EXTRA_MODULES:-ext4 tiny_power_button}"

# Module file globs kept on top of virtio*/*fw_cfg*: virtio_net deps for live migration.
MICROVM_EXTRA_KEEP_GLOBS=("*failover*.ko*")
# Modules the image modprobe must resolve, beyond the extras.
MICROVM_GATE_MODULES="qemu_fw_cfg virtio_net"

# shellcheck source=lib/microvm-image.sh
source "$REPO_ROOT/scripts/lib/microvm-image.sh"

echo "$TAG repo root: $REPO_ROOT"
echo "$TAG build dir: $BUILD_DIR"
mkdir -p "$BUILD_DIR"

microvm_check_tools

if [ ! -x "$SQS_WORKER_BIN" ]; then
    echo "ERROR: $SQS_WORKER_BIN not found — run 'make bin/sqs-worker'" >&2
    exit 1
fi
if ! file -b "$SQS_WORKER_BIN" | grep -q "statically linked"; then
    echo "ERROR: $SQS_WORKER_BIN is not statically linked — it cannot run on the musl guest" >&2
    exit 1
fi
# --- Verify the dedicated nats-server is the go.mod-pinned build ---
# A stale or host-installed binary would ship an unreviewed server version.
NATS_SERVER_PIN=$(awk '$1 == "github.com/nats-io/nats-server/v2" { print $2; exit }' "$REPO_ROOT/go.mod")
if [ -z "$NATS_SERVER_PIN" ]; then
    echo "ERROR: no github.com/nats-io/nats-server/v2 requirement in $REPO_ROOT/go.mod" >&2
    exit 1
fi
if [ ! -x "$NATS_SERVER_BIN" ]; then
    echo "ERROR: $NATS_SERVER_BIN not found — run 'make bin/nats-server'" >&2
    exit 1
fi
if ! file -b "$NATS_SERVER_BIN" | grep -q "statically linked"; then
    echo "ERROR: $NATS_SERVER_BIN is not statically linked — it cannot run on the musl guest" >&2
    exit 1
fi
nats_server_version=$("$NATS_SERVER_BIN" --version 2>&1 | awk '{ print $NF; exit }')
if [ "$nats_server_version" != "$NATS_SERVER_PIN" ]; then
    echo "ERROR: $NATS_SERVER_BIN reports $nats_server_version, go.mod pins $NATS_SERVER_PIN" >&2
    exit 1
fi
echo "$TAG nats-server: $nats_server_version (matches go.mod)"

microvm_init_dirs

microvm_make_rootfs

microvm_empty_dev

# --- Place init script ---
echo "$TAG installing init.sh as /init..."
if [ ! -f "$INIT_SH" ]; then
    echo "ERROR: $INIT_SH not found — cannot build initramfs" >&2
    exit 1
fi
install -m 0755 "$INIT_SH" "$CHROOT_DIR/init"

# --- Place inittab ---
# Required: without it busybox init runs its default table, not the power-down.
if [ ! -f "$INITTAB" ]; then
    echo "ERROR: $INITTAB not found — cannot build initramfs" >&2
    exit 1
fi
echo "$TAG installing inittab..."
install -m 0644 "$INITTAB" "$CHROOT_DIR/etc/inittab"

echo "$TAG installing worker guest steps..."
for step in sqs-worker-lib.sh sqs-worker-prepare sqs-worker-store sqs-worker-ready sqs-worker-stop; do
    if [ ! -f "$BUILD_DIR/$step" ]; then
        echo "ERROR: $BUILD_DIR/$step not found — cannot build initramfs" >&2
        exit 1
    fi
done
mkdir -p "$CHROOT_DIR/usr/local/lib/sqs-worker" "$CHROOT_DIR/usr/local/bin"
install -m 0755 "$BUILD_DIR/sqs-worker-lib.sh" "$BUILD_DIR/sqs-worker-prepare" "$BUILD_DIR/sqs-worker-store" "$BUILD_DIR/sqs-worker-ready" "$BUILD_DIR/sqs-worker-stop" \
    "$CHROOT_DIR/usr/local/lib/sqs-worker/"
install -m 0755 "$SQS_WORKER_BIN" "$CHROOT_DIR/usr/local/bin/sqs-worker"
# The mountpoint ships empty; the worker proves its disk is mounted here.
mkdir -p "$CHROOT_DIR/data"
# --- Copy nats-server binary ---
echo "$TAG copying nats-server binary..."
mkdir -p "$CHROOT_DIR/usr/local/bin"
install -m 0755 "$NATS_SERVER_BIN" "$CHROOT_DIR/usr/local/bin/nats-server"

# --- Create required directories for init ---
mkdir -p \
    "$CHROOT_DIR/proc" \
    "$CHROOT_DIR/sys" \
    "$CHROOT_DIR/dev" \
    "$CHROOT_DIR/etc/ssl/certs"


microvm_locate_kernel

# --- Assert ext4 userspace tools (data volume) ---
for fs_tool in mkfs.ext4 e2fsck; do
    if [ ! -x "$CHROOT_DIR/sbin/$fs_tool" ] && [ ! -x "$CHROOT_DIR/usr/sbin/$fs_tool" ]; then
        echo "ERROR: $fs_tool missing from image — guest cannot check or create an ext4 data volume" >&2
        exit 1
    fi
done
echo "$TAG e2fsprogs: mkfs.ext4 + e2fsck present"

microvm_strip_modules
microvm_strip_rootfs
microvm_stage_kernel
microvm_pack_initramfs

# --- Gate the staged pair, then publish it ---
# Nothing reaches $BUILD_DIR until every gate passes, so a failed build cannot
# leave a pair the grouped make target would take as up to date.
"$REPO_ROOT/scripts/lib/sqs-store-microvm-publish.sh" "$STAGE_DIR" "$BUILD_DIR" "$PROFILE"
echo ""
echo "$TAG done."
