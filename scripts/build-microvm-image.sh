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

# --- Strip kernel modules (Phase 2 keep-list approach) ---
# Only modules needed by a virtio-mmio microvm guest are kept:
#   virtio*.ko*    — virtio bus, net, blk, rng, console, and helpers
#   *fw_cfg*.ko*   — QEMU fw_cfg sysfs driver (delivers boot blobs)
#   *failover*.ko* — net_failover + failover (virtio_net deps for live migration)
# All other loadable modules are removed. Drivers compiled into the kernel
# (CONFIG_VIRTIO_MMIO=y etc.) are unaffected — they live in vmlinuz, not here.
echo "[build-microvm-image] stripping kernel modules to virtio+fw_cfg only..."
for kver_dir in "$CHROOT_DIR/lib/modules"/*/; do
    kver="$(basename "$kver_dir")"
    kernel_dir="$kver_dir/kernel"
    [ -d "$kernel_dir" ] || continue

    # Collect keeper modules and their relative paths from the kernel/ tree.
    tmp_keep=$(mktemp -d)
    find "$kernel_dir" -name "virtio*.ko.gz" -o -name "virtio*.ko" \
                       -o -name "*fw_cfg*.ko.gz" -o -name "*fw_cfg*.ko" \
                       -o -name "*failover*.ko.gz" -o -name "*failover*.ko" \
        2>/dev/null | while read -r mod; do
        rel="${mod#"$kernel_dir/"}"
        dest_dir="$tmp_keep/$(dirname "$rel")"
        mkdir -p "$dest_dir"
        cp "$mod" "$dest_dir/"
    done

    # Replace kernel module tree with the keeper set.
    rm -rf "$kernel_dir"
    mkdir -p "$kernel_dir"
    # Restore each kept file into the same relative path.
    (cd "$tmp_keep" && find . -name "*.ko*" | while read -r f; do
        dest_dir="$kernel_dir/$(dirname "${f#./}")"
        mkdir -p "$dest_dir"
        cp "$f" "$dest_dir/"
    done)
    rm -rf "$tmp_keep"

    # Regenerate module dependency map from the surviving modules.
    depmod -b "$CHROOT_DIR" "$kver" 2>/dev/null || true
done

# --- Strip package-manager artifacts (not needed at runtime) ---
echo "[build-microvm-image] stripping package manager artifacts..."
rm -rf \
    "$CHROOT_DIR/lib/apk" \
    "$CHROOT_DIR/var/cache/apk" \
    "$CHROOT_DIR/etc/apk" \
    "$CHROOT_DIR/usr/share/man" \
    "$CHROOT_DIR/usr/share/doc" \
    "$CHROOT_DIR/usr/share/apk" \
    "$CHROOT_DIR/usr/include" \
    "$CHROOT_DIR/usr/lib/pkgconfig"

# --- Strip debug symbols from binaries ---
echo "[build-microvm-image] stripping debug symbols from binaries..."
find "$CHROOT_DIR/usr/sbin" "$CHROOT_DIR/usr/bin" \
     "$CHROOT_DIR/sbin" "$CHROOT_DIR/bin" \
     "$CHROOT_DIR/usr/local/bin" \
    -type f 2>/dev/null | while read -r bin; do
    strip --strip-all "$bin" 2>/dev/null || true
done

# --- Copy vmlinuz out before stripping /boot from chroot ---
VMLINUZ_OUT="$BUILD_DIR/vmlinuz"
echo "[build-microvm-image] copying kernel: $VMLINUZ_OUT"
cp "$KERNEL_IMG" "$VMLINUZ_OUT"

# Drop /boot from the chroot — vmlinuz, Alpine's stock initramfs-virt, System.map
# and kernel config all live here (~25 MiB). None are needed at runtime: the
# guest already has the kernel loaded by QEMU's -kernel, and Alpine's initramfs
# is unused because we replace it with our own /init. Leaving /boot in the cpio
# nearly doubles uncompressed initramfs size and forces a 512 MiB guest just to
# survive decompression.
rm -rf "$CHROOT_DIR/boot"

# --- Build initramfs ---
INITRAMFS_OUT="$BUILD_DIR/initramfs.cpio.gz"
echo "[build-microvm-image] building initramfs: $INITRAMFS_OUT"
(
    cd "$CHROOT_DIR"
    find . | cpio --quiet -o -H newc | gzip -9 > "$INITRAMFS_OUT"
)

# --- Log artifact sizes ---
echo ""
echo "[build-microvm-image] artifacts:"
ls -lh "$VMLINUZ_OUT" "$INITRAMFS_OUT"
echo ""

# --- Phase 2 artifact size gate (50 MiB total) ---
ARTIFACT_MAX_MiB="${MICROVM_ARTIFACT_MAX_MiB:-50}"
total_bytes=$(( $(stat -c %s "$VMLINUZ_OUT") + $(stat -c %s "$INITRAMFS_OUT") ))
max_bytes=$(( ARTIFACT_MAX_MiB * 1024 * 1024 ))
total_mib=$(( (total_bytes + 1048575) / 1048576 ))
if [ "$total_bytes" -gt "$max_bytes" ]; then
    echo "ERROR: artifact size ${total_mib} MiB exceeds ${ARTIFACT_MAX_MiB} MiB gate" >&2
    echo "       Set MICROVM_ARTIFACT_MAX_MiB=N to override during development." >&2
    exit 1
fi
echo "[build-microvm-image] artifact size gate: PASS (${total_mib} MiB / ${ARTIFACT_MAX_MiB} MiB)"
echo ""
echo "[build-microvm-image] done."
