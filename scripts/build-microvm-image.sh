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
# haproxy: ALB (L7) data plane. nginx + nginx-mod-stream: NLB (L4) data plane —
# the `stream` module (a separate Alpine package) load-balances TCP/UDP/TLS,
# which haproxy cannot do for UDP.
ALPINE_PACKAGES="busybox linux-virt haproxy nginx nginx-mod-stream iproute2 ca-certificates"

echo "[build-microvm-image] repo root: $REPO_ROOT"
echo "[build-microvm-image] build dir: $BUILD_DIR"
mkdir -p "$BUILD_DIR"

# --- Verify non-negotiable tools ---
# fakeroot is mandatory: device nodes are written into the cpio archive inside a
# faked-root environment so no real device node is ever created on the host
# filesystem (which previously required sudo mknod and risked clobbering host
# /dev/null on cleanup).
for tool in cpio gzip find fakeroot depmod; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "ERROR: required tool not found: $tool" >&2
        exit 1
    fi
done

# --- Create chroot ---
CHROOT_DIR=$(mktemp -d)
# Guard: every destructive op below targets "$CHROOT_DIR/...". An empty or
# unexpected value would aim rm -rf at the host (e.g. /dev). Refuse anything that
# is not a freshly-created temp dir.
case "${CHROOT_DIR:?mktemp -d returned empty}" in
    /tmp/*|/var/tmp/*|"${TMPDIR:-/nonexistent}"/*) ;;
    *) echo "ERROR: refusing to use unexpected CHROOT_DIR: $CHROOT_DIR" >&2; exit 1 ;;
esac
CONTAINER_TOOL=""
CONTAINER_CID=""
cleanup() {
    echo "[build-microvm-image] cleaning up chroot: $CHROOT_DIR"
    rm -rf "$CHROOT_DIR"
    if [ -n "$CONTAINER_CID" ] && [ -n "$CONTAINER_TOOL" ]; then
        echo "[build-microvm-image] cleaning up container: $CONTAINER_CID"
        $CONTAINER_TOOL rm -f "$CONTAINER_CID" >/dev/null 2>&1 || true
    fi
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
    CONTAINER_CID=$($CONTAINER_TOOL run -d \
        "alpine:${ALPINE_VERSION}" \
        sh -c "apk add --no-cache ${ALPINE_PACKAGES}")

    exit_code=$($CONTAINER_TOOL wait "$CONTAINER_CID")
    if [ "$exit_code" != "0" ]; then
        echo "ERROR: package installation failed in container (exit $exit_code)" >&2
        exit 1
    fi

    $CONTAINER_TOOL export "$CONTAINER_CID" | tar -x -C "$CHROOT_DIR"
fi

# --- /dev: empty mountpoint only ---
# tar extraction without root silently creates 0-byte regular files for device
# nodes. Drop them and leave an empty /dev mountpoint. The two static nodes the
# kernel needs (/dev/console to wire init's stdio, /dev/null) are written into
# the cpio archive under fakeroot at pack time — NOT created on the host. init
# mounts devtmpfs over /dev as its first action for everything else at runtime.
rm -rf "${CHROOT_DIR:?}/dev"
mkdir "$CHROOT_DIR/dev"

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

# --- Assert nginx + stream module (NLB L4 data plane) ---
# The lb-agent runs `nginx -c` with a config that `load_module`s the stream
# module; if either the binary or the .so is missing the agent never enters
# nginx mode, never probes, and NLB targets stay at 0/N healthy. Fail the build
# loudly here rather than discover it as an opaque e2e health timeout.
if [ ! -x "$CHROOT_DIR/usr/sbin/nginx" ] && [ ! -x "$CHROOT_DIR/usr/bin/nginx" ]; then
    echo "ERROR: nginx binary missing from image — NLB data plane will not start" >&2
    exit 1
fi
if ! find "$CHROOT_DIR/usr/lib/nginx/modules" -name "ngx_stream_module.so" 2>/dev/null | grep -q .; then
    echo "ERROR: ngx_stream_module.so missing from image — NLB stream config fails to load" >&2
    exit 1
fi
echo "[build-microvm-image] nginx: binary + stream module present"

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

    # Decompress kept modules to plain .ko. Alpine ships .ko.gz, but a host
    # kmod that lacks gzip support (e.g. Debian trixie's kmod 34) silently
    # produces an EMPTY modules.dep from compressed modules — the guest's
    # modprobe then no-ops and qemu_fw_cfg never loads. Plain .ko sidesteps the
    # host-kmod dependency entirely and the guest needs no decompressor either.
    find "$kernel_dir" -name "*.ko.gz" -exec gunzip -f {} +

    # Regenerate module dependency map from the surviving modules. A usable
    # modules.dep is mandatory: the guest init's load_mod() tries modprobe
    # first, and busybox modprobe silently no-ops without it. Fail loudly here
    # rather than ship an image that depends on init's insmod fallback alone.
    if ! depmod -b "$CHROOT_DIR" "$kver"; then
        echo "ERROR: depmod failed for $kver" >&2
        exit 1
    fi
    if [ ! -s "$kver_dir/modules.dep" ]; then
        echo "ERROR: ${kver_dir}modules.dep missing or empty after depmod" >&2
        exit 1
    fi
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
# Create the static device nodes and pack the archive inside a single fakeroot
# session: mknod/cpio see faked device entries and write real char-device
# records into the cpio, while the host filesystem never gets an actual node.
fakeroot sh -c '
    cd "$1" || exit 1
    mknod -m 600 dev/console c 5 1
    mknod -m 666 dev/null    c 1 3
    find . | cpio --quiet -o -H newc | gzip -9 > "$2"
' _ "$CHROOT_DIR" "$INITRAMFS_OUT"

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
