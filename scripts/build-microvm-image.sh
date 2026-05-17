#!/bin/bash
# build-microvm-image.sh — build a minimal Alpine-based microVM kernel + initramfs
# for use with QEMU microvm machine type and Spinifex lb-agent.
#
# Outputs:
#   $REPO_ROOT/build/microvm/vmlinuz
#   $REPO_ROOT/build/microvm/initramfs.cpio.gz
#
# Rootfs strategy (first match wins):
#   1. apk available on host → install directly into a temp chroot (Alpine CI/CD)
#   2. docker available      → run alpine container, export filesystem
#   3. podman available      → same via podman
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="$REPO_ROOT/build/microvm"
INIT_SH="$BUILD_DIR/init.sh"
INITTAB="$BUILD_DIR/inittab"

ALPINE_VERSION="${ALPINE_VERSION:-3.20}"
ALPINE_MIRROR="${ALPINE_MIRROR:-https://dl-cdn.alpinelinux.org/alpine}"
ALPINE_PACKAGES="busybox linux-virt haproxy iproute2 ca-certificates"

echo "[build-microvm-image] repo root: $REPO_ROOT"
echo "[build-microvm-image] build dir: $BUILD_DIR"
mkdir -p "$BUILD_DIR"

# --- Verify non-negotiable tools ---
for tool in cpio gzip find; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "ERROR: required tool not found: $tool" >&2
        exit 1
    fi
done

# --- Create chroot ---
CHROOT_DIR=$(mktemp -d)
cleanup() {
    echo "[build-microvm-image] cleaning up chroot: $CHROOT_DIR"
    rm -rf "$CHROOT_DIR"
}
trap cleanup EXIT

# --- Populate rootfs ---
# Strategy 1: native apk (Alpine hosts, CI)
if command -v apk >/dev/null 2>&1; then
    echo "[build-microvm-image] rootfs: native apk"
    mkdir -p \
        "$CHROOT_DIR/etc/apk" \
        "$CHROOT_DIR/lib/apk/db" \
        "$CHROOT_DIR/var/cache/apk"
    cat > "$CHROOT_DIR/etc/apk/repositories" <<EOF
${ALPINE_MIRROR}/v${ALPINE_VERSION}/main
${ALPINE_MIRROR}/v${ALPINE_VERSION}/community
EOF
    apk add \
        --root "$CHROOT_DIR" \
        --arch x86_64 \
        --no-cache \
        --allow-untrusted \
        --initdb \
        $ALPINE_PACKAGES

# Strategy 2/3: container runtime (dev machines)
else
    CONTAINER_TOOL=""
    if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
        CONTAINER_TOOL=docker
    elif command -v podman >/dev/null 2>&1; then
        CONTAINER_TOOL=podman
    else
        echo "ERROR: no rootfs build tool available." >&2
        echo "       Install one of: apk (Alpine), docker, podman" >&2
        exit 1
    fi

    echo "[build-microvm-image] rootfs: $CONTAINER_TOOL export (alpine:${ALPINE_VERSION})"
    cid=$($CONTAINER_TOOL run -d \
        "alpine:${ALPINE_VERSION}" \
        sh -c "apk add --no-cache ${ALPINE_PACKAGES}")

    exit_code=$($CONTAINER_TOOL wait "$cid")
    if [ "$exit_code" != "0" ]; then
        echo "ERROR: package installation failed in container (exit $exit_code)" >&2
        $CONTAINER_TOOL rm "$cid" >/dev/null 2>&1 || true
        exit 1
    fi

    $CONTAINER_TOOL export "$cid" | tar -x -C "$CHROOT_DIR"
    $CONTAINER_TOOL rm "$cid" >/dev/null 2>&1
fi

# --- Fix /dev device nodes ---
# tar extraction without root silently creates 0-byte regular files for device
# nodes (mknod requires CAP_MKNOD). The kernel opens /dev/console to wire
# init's stdio to the serial console; a regular file there causes all init
# output to be silently discarded. Recreate the two nodes needed before
# devtmpfs mounts (which provides the rest at runtime).
rm -rf "$CHROOT_DIR/dev"
mkdir "$CHROOT_DIR/dev"
sudo mknod -m 600 "$CHROOT_DIR/dev/console" c 5 1
sudo mknod -m 666 "$CHROOT_DIR/dev/null"    c 1 3

# --- Place init script ---
echo "[build-microvm-image] installing init.sh as /init..."
if [ ! -f "$INIT_SH" ]; then
    echo "ERROR: $INIT_SH not found — cannot build initramfs" >&2
    exit 1
fi
install -m 0755 "$INIT_SH" "$CHROOT_DIR/init"

# --- Place inittab ---
if [ -f "$INITTAB" ]; then
    echo "[build-microvm-image] installing inittab..."
    install -m 0644 "$INITTAB" "$CHROOT_DIR/etc/inittab"
fi

# --- Copy lb-agent binary ---
LB_AGENT_BIN="$REPO_ROOT/bin/lb-agent"
if [ -f "$LB_AGENT_BIN" ]; then
    echo "[build-microvm-image] copying lb-agent binary..."
    mkdir -p "$CHROOT_DIR/usr/local/bin"
    install -m 0755 "$LB_AGENT_BIN" "$CHROOT_DIR/usr/local/bin/lb-agent"
else
    echo "[build-microvm-image] WARNING: $LB_AGENT_BIN not found — lb-agent will be absent from initramfs" >&2
fi

# --- Create required directories for init ---
mkdir -p \
    "$CHROOT_DIR/proc" \
    "$CHROOT_DIR/sys" \
    "$CHROOT_DIR/dev" \
    "$CHROOT_DIR/etc/ssl/certs" \
    "$CHROOT_DIR/etc/conf.d"

# --- Locate kernel ---
KERNEL_IMG=$(find "$CHROOT_DIR/boot" -name "vmlinuz*" | sort | tail -1)
if [ -z "$KERNEL_IMG" ]; then
    echo "ERROR: no vmlinuz found in $CHROOT_DIR/boot" >&2
    exit 1
fi
echo "[build-microvm-image] kernel image: $KERNEL_IMG"

KERNEL_VER=$(basename "$KERNEL_IMG" | sed 's/vmlinuz-//')
KERNEL_CONFIG="$CHROOT_DIR/boot/config-${KERNEL_VER}"

# --- Assert fw_cfg module ---
if ! find "$CHROOT_DIR/lib/modules" \
        -name "qemu_fw_cfg.ko*" \
        -o -name "fw_cfg_sysfs.ko*" \
    | grep -q .; then
    if ! grep -q "CONFIG_FW_CFG_SYSFS=y" "$KERNEL_CONFIG" 2>/dev/null; then
        echo "ERROR: qemu_fw_cfg module missing from initramfs — fw_cfg reads will fail at boot" >&2
        exit 1
    fi
    echo "[build-microvm-image] fw_cfg: built-in (CONFIG_FW_CFG_SYSFS=y)"
else
    echo "[build-microvm-image] fw_cfg: module found in lib/modules"
fi

# --- Build initramfs ---
INITRAMFS_OUT="$BUILD_DIR/initramfs.cpio.gz"
echo "[build-microvm-image] building initramfs: $INITRAMFS_OUT"
(
    cd "$CHROOT_DIR"
    find . | cpio --quiet -o -H newc | gzip -9 > "$INITRAMFS_OUT"
)

# --- Copy vmlinuz ---
VMLINUZ_OUT="$BUILD_DIR/vmlinuz"
echo "[build-microvm-image] copying kernel: $VMLINUZ_OUT"
cp "$KERNEL_IMG" "$VMLINUZ_OUT"

# --- Log artifact sizes ---
echo ""
echo "[build-microvm-image] artifacts:"
ls -lh "$VMLINUZ_OUT" "$INITRAMFS_OUT"
echo ""
echo "[build-microvm-image] done."
