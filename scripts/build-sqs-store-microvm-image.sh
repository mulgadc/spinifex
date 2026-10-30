#!/bin/bash
# build-sqs-store-microvm-image.sh — build the SQS store microVM kernel +
# initramfs: the dedicated nats-server, ext4 support, the store's guest steps
# and a clean power-down. The guest never formats a disk; build/microvm/
# sqs-store/sqs-store-lib.sh states its fw_cfg contract. The LB image
# (build-microvm-image.sh) is separate and unchanged.
#
# SQS_MICROVM_PROFILE=worker builds the shared SQS worker guest instead: the
# same network /init and kernel, build/microvm/sqs-worker's steps and
# $REPO_ROOT/bin/sqs-worker, with no nats-server.
#
# Inputs:
#   $REPO_ROOT/build/microvm/sqs-store/init.sh, inittab
#   $REPO_ROOT/build/microvm/sqs-store/sqs-store-{lib.sh,prepare,run,stop}
#   $REPO_ROOT/bin/nats-server  (make bin/nats-server, the go.mod-pinned version)
#
# Outputs:
#   $REPO_ROOT/build/microvm/sqs-store/vmlinuz
#   $REPO_ROOT/build/microvm/sqs-store/initramfs.cpio.gz
#
# Rootfs strategy (first match wins):
#   1. apk available on host → install directly into a temp chroot (Alpine CI/CD)
#   2. docker available      → run alpine container, export filesystem
#   3. podman available      → same via podman
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="$REPO_ROOT/build/microvm/sqs-store"
INIT_SH="$BUILD_DIR/init.sh"
INITTAB="$BUILD_DIR/inittab"
NATS_SERVER_BIN="$REPO_ROOT/bin/nats-server"

PROFILE="${SQS_MICROVM_PROFILE:-store}"
case "$PROFILE" in
    store) ;;
    worker)
        BUILD_DIR="$REPO_ROOT/build/microvm/sqs-worker"
        INITTAB="$BUILD_DIR/inittab"
        SQS_WORKER_BIN="$REPO_ROOT/bin/sqs-worker"
        ;;
    *) echo "ERROR: SQS_MICROVM_PROFILE must be store or worker, not $PROFILE" >&2; exit 1 ;;
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

echo "$TAG repo root: $REPO_ROOT"
echo "$TAG build dir: $BUILD_DIR"
mkdir -p "$BUILD_DIR"

# --- Verify non-negotiable tools ---
# fakeroot is mandatory: device nodes are written into the cpio archive inside a
# faked-root environment so no real device node is ever created on the host
# filesystem (which previously required sudo mknod and risked clobbering host
# /dev/null on cleanup).
for tool in cpio gzip find fakeroot; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "ERROR: required tool not found: $tool" >&2
        exit 1
    fi
done

if [ "$PROFILE" = worker ]; then
    if [ ! -x "$SQS_WORKER_BIN" ]; then
        echo "ERROR: $SQS_WORKER_BIN not found — run 'make bin/sqs-worker'" >&2
        exit 1
    fi
    if ! file -b "$SQS_WORKER_BIN" | grep -q "statically linked"; then
        echo "ERROR: $SQS_WORKER_BIN is not statically linked — it cannot run on the musl guest" >&2
        exit 1
    fi
else
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
fi

# --- Create chroot ---
CHROOT_DIR=$(mktemp -d)
# Guard: every destructive op below targets "$CHROOT_DIR/...". An empty or
# unexpected value would aim rm -rf at the host (e.g. /dev). Refuse anything that
# is not a freshly-created temp dir.
case "${CHROOT_DIR:?mktemp -d returned empty}" in
    /tmp/*|/var/tmp/*|"${TMPDIR:-/nonexistent}"/*) ;;
    *) echo "ERROR: refusing to use unexpected CHROOT_DIR: $CHROOT_DIR" >&2; exit 1 ;;
esac
# Outputs are written here first, beside $BUILD_DIR so publishing is a rename.
STAGE_DIR=$(mktemp -d "$BUILD_DIR/.stage.XXXXXX")
CONTAINER_TOOL=""
CONTAINER_CID=""
cleanup() {
    echo "$TAG cleaning up chroot: $CHROOT_DIR"
    rm -rf "$CHROOT_DIR"
    rm -rf "${STAGE_DIR:?}"
    if [ -n "$CONTAINER_CID" ] && [ -n "$CONTAINER_TOOL" ]; then
        echo "$TAG cleaning up container: $CONTAINER_CID"
        $CONTAINER_TOOL rm -f "$CONTAINER_CID" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

# --- Populate rootfs ---
# Strategy 1: native apk (Alpine hosts, CI)
if command -v apk >/dev/null 2>&1; then
    echo "$TAG rootfs: native apk"
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

# Strategy 2: docker -> BuildKit local export.
# Do NOT use `docker export` here. On Docker's overlay2 driver it leaks the
# container's overlay mount (the mount survives `docker rm`), orphaning layers
# under /var/lib/docker that only a daemon restart reclaims; on a busy CI host
# this grows without bound. `buildx --output type=local` writes the image
# filesystem straight to a directory and releases every mount, so nothing leaks.
elif command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    echo "$TAG rootfs: docker buildx --output type=local (alpine:${ALPINE_VERSION})"
    BUILD_CTX=$(mktemp -d)
    cat > "$BUILD_CTX/Dockerfile" <<DOCKERFILE
FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ${ALPINE_PACKAGES}
DOCKERFILE
    docker buildx build \
        --platform linux/amd64 \
        --output "type=local,dest=${CHROOT_DIR}" \
        "$BUILD_CTX"
    rm -rf "$BUILD_CTX"

# Strategy 3: podman -> run + export. Podman's store does not exhibit the docker
# overlay2 export-mount leak, and podman has no buildx.
elif command -v podman >/dev/null 2>&1; then
    echo "$TAG rootfs: podman export (alpine:${ALPINE_VERSION})"
    CONTAINER_TOOL=podman
    CONTAINER_CID=$(podman run -d \
        "alpine:${ALPINE_VERSION}" \
        sh -c "apk add --no-cache ${ALPINE_PACKAGES}")

    exit_code=$(podman wait "$CONTAINER_CID")
    if [ "$exit_code" != "0" ]; then
        echo "ERROR: package installation failed in container (exit $exit_code)" >&2
        exit 1
    fi

    podman export "$CONTAINER_CID" | tar -x -C "$CHROOT_DIR"

else
    echo "ERROR: no rootfs build tool available." >&2
    echo "       Install one of: apk (Alpine), docker, podman" >&2
    exit 1
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

if [ "$PROFILE" = worker ]; then
    echo "$TAG installing worker guest steps..."
    for step in sqs-worker-prepare sqs-worker-ready sqs-worker-stop; do
        if [ ! -f "$BUILD_DIR/$step" ]; then
            echo "ERROR: $BUILD_DIR/$step not found — cannot build initramfs" >&2
            exit 1
        fi
    done
    mkdir -p "$CHROOT_DIR/usr/local/lib/sqs-worker" "$CHROOT_DIR/usr/local/bin"
    install -m 0755 "$BUILD_DIR/sqs-worker-prepare" "$BUILD_DIR/sqs-worker-ready" "$BUILD_DIR/sqs-worker-stop" \
        "$CHROOT_DIR/usr/local/lib/sqs-worker/"
    install -m 0755 "$SQS_WORKER_BIN" "$CHROOT_DIR/usr/local/bin/sqs-worker"
    # The mountpoint ships empty; the worker proves its disk is mounted here.
    mkdir -p "$CHROOT_DIR/data"
else
# --- Place the store's guest steps ---
echo "$TAG installing store guest steps..."
mkdir -p "$CHROOT_DIR/usr/local/lib/sqs-store"
for step in sqs-store-lib.sh sqs-store-prepare sqs-store-run sqs-store-stop; do
    if [ ! -f "$BUILD_DIR/$step" ]; then
        echo "ERROR: $BUILD_DIR/$step not found — cannot build initramfs" >&2
        exit 1
    fi
done
install -m 0644 "$BUILD_DIR/sqs-store-lib.sh" "$CHROOT_DIR/usr/local/lib/sqs-store/"
install -m 0755 "$BUILD_DIR/sqs-store-prepare" "$BUILD_DIR/sqs-store-run" "$BUILD_DIR/sqs-store-stop" \
    "$CHROOT_DIR/usr/local/lib/sqs-store/"

# --- Copy nats-server binary ---
echo "$TAG copying nats-server binary..."
mkdir -p "$CHROOT_DIR/usr/local/bin"
install -m 0755 "$NATS_SERVER_BIN" "$CHROOT_DIR/usr/local/bin/nats-server"
fi

# --- Create required directories for init ---
mkdir -p \
    "$CHROOT_DIR/proc" \
    "$CHROOT_DIR/sys" \
    "$CHROOT_DIR/dev" \
    "$CHROOT_DIR/etc/ssl/certs"

# --- Locate kernel ---
KERNEL_IMG=$(find "$CHROOT_DIR/boot" -name "vmlinuz*" | sort | tail -1)
if [ -z "$KERNEL_IMG" ]; then
    echo "ERROR: no vmlinuz found in $CHROOT_DIR/boot" >&2
    exit 1
fi
echo "$TAG kernel image: $KERNEL_IMG"

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
    echo "$TAG fw_cfg: built-in (CONFIG_FW_CFG_SYSFS=y)"
else
    echo "$TAG fw_cfg: module found in lib/modules"
fi

# --- Assert ext4 userspace tools (data volume) ---
for fs_tool in mkfs.ext4 e2fsck; do
    if [ ! -x "$CHROOT_DIR/sbin/$fs_tool" ] && [ ! -x "$CHROOT_DIR/usr/sbin/$fs_tool" ]; then
        echo "ERROR: $fs_tool missing from image — guest cannot check or create an ext4 data volume" >&2
        exit 1
    fi
done
echo "$TAG e2fsprogs: mkfs.ext4 + e2fsck present"

# --- Strip kernel modules (Phase 2 keep-list approach) ---
# Only modules needed by a virtio-mmio microvm guest are kept:
#   virtio*.ko*    — virtio bus, net, blk, rng, console, and helpers
#   *fw_cfg*.ko*   — QEMU fw_cfg sysfs driver (delivers boot blobs)
#   *failover*.ko* — net_failover + failover (virtio_net deps for live migration)
#   $MICROVM_EXTRA_MODULES — ext4 by default, plus dependency closure
# All other loadable modules are removed. Drivers compiled into the kernel
# (CONFIG_VIRTIO_MMIO=y etc.) are unaffected — they live in vmlinuz, not here.

# module_path prints a module's kernel/ path from the stock modules.dep, by
# name or modules.alias; it fails when the module is not loadable.
module_path() {
    local kdir="$1" name="${2//-/_}" path alias_target
    path=$(awk -F: -v n="$name" '{ f = $1; sub(/.*\//, "", f); sub(/\.ko(\.gz|\.xz|\.zst)?$/, "", f);
        gsub(/-/, "_", f); if (f == n) { print $1; exit } }' "$kdir/modules.dep")
    if [ -z "$path" ] && [ -f "$kdir/modules.alias" ]; then
        alias_target=$(awk -v n="$2" '$1 == "alias" && $2 == n { print $3; exit }' "$kdir/modules.alias")
        [ -n "$alias_target" ] && [ "$alias_target" != "$2" ] && path=$(module_path "$kdir" "$alias_target")
    fi
    [ -n "$path" ] && printf '%s\n' "$path"
}

# module_builtin reports whether a module name is compiled into the kernel.
module_builtin() {
    local kdir="$1" name="${2//-/_}"
    [ -f "$kdir/modules.builtin" ] || return 1
    awk -v n="$name" '{ f = $0; sub(/.*\//, "", f); sub(/\.ko$/, "", f); gsub(/-/, "_", f);
        if (f == n) { found = 1; exit } } END { exit !found }' "$kdir/modules.builtin"
}

# image_kmod runs the image's own kmod (depmod, modprobe) through its musl
# loader, so module metadata matches the guest's modprobe without a host kmod.
image_kmod() {
    "$CHROOT_DIR/lib/ld-musl-x86_64.so.1" --library-path "$CHROOT_DIR/lib:$CHROOT_DIR/usr/lib" \
        --argv0 "$1" "$CHROOT_DIR/bin/kmod" "${@:2}"
}

echo "$TAG stripping kernel modules to virtio+fw_cfg+${MICROVM_EXTRA_MODULES} only..."
for kver_dir in "$CHROOT_DIR/lib/modules"/*/; do
    kver="$(basename "$kver_dir")"
    kernel_dir="$kver_dir/kernel"
    [ -d "$kernel_dir" ] || continue

    if [ ! -s "$kver_dir/modules.dep" ]; then
        echo "ERROR: stock ${kver_dir}modules.dep missing — cannot resolve module dependencies" >&2
        exit 1
    fi

    # Extra modules start the closure; the glob families are kept as found.
    keep_list=$(mktemp)
    : > "$keep_list"
    for name in $MICROVM_EXTRA_MODULES; do
        if ! path=$(module_path "$kver_dir" "$name"); then
            if module_builtin "$kver_dir" "$name"; then continue; fi
            echo "ERROR: module $name is neither loadable nor built into $kver" >&2
            exit 1
        fi
        echo "$path" >> "$keep_list"
    done

    # Close over modules.dep hard dependencies and modules.softdep pre:/post:
    # names until the set stops growing. A softdep must be loadable or built in.
    while :; do
        before=$(sort -u "$keep_list" | wc -l)
        for path in $(sort -u "$keep_list"); do
            awk -F': ' -v p="$path" '$1 == p { n = split($2, d, " "); for (i = 1; i <= n; i++) print d[i] }' \
                "$kver_dir/modules.dep" >> "$keep_list"
            mod=$(basename "$path"); mod="${mod%%.ko*}"; mod="${mod//-/_}"
            [ -f "$kver_dir/modules.softdep" ] || continue
            for soft in $(awk -v n="$mod" '{ m = $2; gsub(/-/, "_", m) }
                    $1 == "softdep" && m == n { for (i = 3; i <= NF; i++) if ($i != "pre:" && $i != "post:") print $i }' \
                    "$kver_dir/modules.softdep"); do
                if soft_path=$(module_path "$kver_dir" "$soft"); then
                    echo "$soft_path" >> "$keep_list"
                elif ! module_builtin "$kver_dir" "$soft"; then
                    echo "ERROR: softdep $soft of $mod is neither loadable nor built into $kver" >&2
                    exit 1
                fi
            done
        done
        after=$(sort -u "$keep_list" | wc -l)
        [ "$before" -eq "$after" ] && break
    done

    extra_names=$(sort -u "$keep_list" | while read -r p; do m=$(basename "$p"); m="${m%%.ko*}"; echo "${m//-/_}"; done)
    (cd "$kver_dir" && find kernel -name "virtio*.ko*" -o -name "*fw_cfg*.ko*" \
        -o -name "*failover*.ko*") >> "$keep_list"

    # Copy the keeper set out, preserving each kernel/ relative path.
    tmp_keep=$(mktemp -d)
    while read -r rel; do
        rel="${rel#kernel/}"
        mkdir -p "$tmp_keep/$(dirname "$rel")"
        cp "$kernel_dir/$rel" "$tmp_keep/$rel"
    done < <(sort -u "$keep_list")
    rm -f "$keep_list"

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

    # Independent closure gate: each extra-closure module's own .modinfo depends=
    # must name a kept module or a built-in, so a resolver bug fails here.
    kept_names=$(find "$kernel_dir" -name "*.ko" -exec basename {} .ko \; | tr '-' '_' | sort -u)
    while read -r ko; do
        ko_name=$(basename "$ko" .ko); ko_name="${ko_name//-/_}"
        grep -qx "$ko_name" <<< "$extra_names" || continue
        deps=$(tr '\0' '\n' < "$ko" | grep -a -m1 '^depends=' | cut -d= -f2 | tr ',' ' ' || true)
        for dep in $deps; do
            dep="${dep//-/_}"
            if ! grep -qx "$dep" <<< "$kept_names" && ! module_builtin "$kver_dir" "$dep"; then
                echo "ERROR: $(basename "$ko") depends on $dep, which is neither kept nor built in" >&2
                exit 1
            fi
        done
    done < <(find "$kernel_dir" -name "*.ko")
    for name in $MICROVM_EXTRA_MODULES; do
        if ! grep -qx "${name//-/_}" <<< "$kept_names" && ! module_builtin "$kver_dir" "$name"; then
            echo "ERROR: required module $name missing from the stripped module tree" >&2
            exit 1
        fi
    done
    echo "$TAG extra module closure: $(echo "$extra_names" | tr '\n' ' ')"
    echo "$TAG modules kept: $(echo "$kept_names" | tr '\n' ' ')"

    # Rebuild module metadata with the image's own kmod, the binary the guest's
    # modprobe runs, so no host depmod is needed. Stock indexes still name .ko.gz
    # files, so they are removed first and a failed rebuild cannot ship them.
    for f in dep dep.bin alias alias.bin softdep weakdep symbols symbols.bin \
        builtin.bin builtin.alias.bin devname; do
        rm -f "$kver_dir/modules.$f"
    done
    if ! image_kmod depmod -b "$CHROOT_DIR" "$kver"; then
        echo "ERROR: image depmod failed for $kver" >&2
        exit 1
    fi
    if [ ! -s "$kver_dir/modules.dep" ] || [ ! -s "$kver_dir/modules.dep.bin" ]; then
        echo "ERROR: ${kver_dir}modules.dep or modules.dep.bin missing or empty after depmod" >&2
        exit 1
    fi
    # Resolution gate: the image's modprobe must resolve every required module
    # to plain .ko files present in the stripped tree.
    for name in $MICROVM_EXTRA_MODULES qemu_fw_cfg virtio_net; do
        module_builtin "$kver_dir" "$name" && continue
        if ! plan=$(image_kmod modprobe -d "$CHROOT_DIR" -S "$kver" --show-depends "$name"); then
            echo "ERROR: image modprobe cannot resolve $name for $kver" >&2
            exit 1
        fi
        while read -r verb path _; do
            [ "$verb" = "insmod" ] || continue
            case "$path" in
                "$CHROOT_DIR/lib/modules/$kver/kernel/"*.ko) [ -f "$path" ] && continue ;;
            esac
            echo "ERROR: modprobe plan for $name names $path, not a kept plain .ko" >&2
            exit 1
        done <<< "$plan"
        echo "$TAG modprobe $name: $(awk '{ n = split($2, a, "/"); printf "%s ", a[n] }' <<< "$plan")"
    done
done

# --- Strip package-manager artifacts (not needed at runtime) ---
echo "$TAG stripping package manager artifacts..."
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
# nats-server in /usr/local/bin is left as built (-s -w), so the shipped bytes
# are the ones whose version was checked above.
echo "$TAG stripping debug symbols from binaries..."
find "$CHROOT_DIR/usr/sbin" "$CHROOT_DIR/usr/bin" \
     "$CHROOT_DIR/sbin" "$CHROOT_DIR/bin" \
    -type f 2>/dev/null | while read -r bin; do
    strip --strip-all "$bin" 2>/dev/null || true
done

# --- Copy vmlinuz out before stripping /boot from chroot ---
VMLINUZ_OUT="$STAGE_DIR/vmlinuz"
echo "$TAG copying kernel: $VMLINUZ_OUT"
cp "$KERNEL_IMG" "$VMLINUZ_OUT"

# Drop /boot from the chroot — vmlinuz, Alpine's stock initramfs-virt, System.map
# and kernel config all live here (~25 MiB). None are needed at runtime: the
# guest already has the kernel loaded by QEMU's -kernel, and Alpine's initramfs
# is unused because we replace it with our own /init. Leaving /boot in the cpio
# nearly doubles uncompressed initramfs size and forces a 512 MiB guest just to
# survive decompression.
rm -rf "${CHROOT_DIR:?}/boot"

# --- Build initramfs ---
INITRAMFS_OUT="$STAGE_DIR/initramfs.cpio.gz"
echo "$TAG building initramfs: $INITRAMFS_OUT"
# Create the static device nodes and pack the archive inside a single fakeroot
# session: mknod/cpio see faked device entries and write real char-device
# records into the cpio, while the host filesystem never gets an actual node.
fakeroot sh -c '
    cd "$1" || exit 1
    mknod -m 600 dev/console c 5 1
    mknod -m 666 dev/null    c 1 3
    find . | cpio --quiet -o -H newc | gzip -9 > "$2"
' _ "$CHROOT_DIR" "$INITRAMFS_OUT"

# --- Gate the staged pair, then publish it ---
# Nothing reaches $BUILD_DIR until every gate passes, so a failed build cannot
# leave a pair the grouped make target would take as up to date.
"$REPO_ROOT/scripts/lib/sqs-store-microvm-publish.sh" "$STAGE_DIR" "$BUILD_DIR" "$PROFILE"
echo ""
echo "$TAG done."
