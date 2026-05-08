#!/bin/bash
set -euo pipefail

# build-system-image.sh — Build a minimal system AMI from a manifest
#
# Supports Alpine Linux and Ubuntu 24.04 as base distros.
# Creates a pre-baked image with custom packages, binaries, and setup scripts
# installed, ready for import as a Spinifex AMI.
#
# Requirements: qemu-nbd, qemu-img, sudo (for mount/chroot), curl
# Usage: ./scripts/build-system-image.sh <manifest.conf> [--import]
#   --import  Also import the image as an AMI via spx admin images import

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

usage() {
    echo "Usage: $0 <manifest.conf> [--import]"
    echo ""
    echo "  manifest.conf  Path to image manifest (see scripts/images/ for examples)"
    echo "  --import       Import the built image as an AMI"
    exit 1
}

if [[ $# -lt 1 || "$1" == "-h" || "$1" == "--help" ]]; then
    usage
fi

MANIFEST="$1"
shift

if [[ ! -f "$MANIFEST" ]]; then
    echo "ERROR: Manifest not found: $MANIFEST"
    exit 1
fi

DO_IMPORT=false
QUIET=false
for arg in "$@"; do
    case "$arg" in
        --import) DO_IMPORT=true ;;
        --quiet)  QUIET=true ;;
    esac
done

# Source the manifest
# shellcheck source=/dev/null
source "$MANIFEST"

# Default distro is alpine for backward compatibility
DISTRO="${DISTRO:-alpine}"

# Validate required manifest fields
for field in IMAGE_NAME IMAGE_SIZE; do
    if [[ -z "${!field:-}" ]]; then
        echo "ERROR: Manifest missing required field: $field"
        exit 1
    fi
done

# Derived paths
BUILD_DIR="/tmp/${IMAGE_NAME}-image-build"
NBD_DEV="/dev/nbd0"
MOUNT_DIR="${BUILD_DIR}/mnt"

case "$DISTRO" in
    alpine)
        if [[ -z "${ALPINE_VERSION:-}" ]]; then
            echo "ERROR: Manifest missing required field: ALPINE_VERSION"
            exit 1
        fi
        SOURCE_IMAGE="generic_alpine-${ALPINE_VERSION}-x86_64-bios-cloudinit-r0.qcow2"
        SOURCE_URL="https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION%.*}/releases/cloud/${SOURCE_IMAGE}"
        OUTPUT_IMAGE="${BUILD_DIR}/${IMAGE_NAME}-alpine.qcow2"
        OUTPUT_RAW="${BUILD_DIR}/${IMAGE_NAME}-alpine.raw"
        DISTRO_VERSION="${ALPINE_VERSION}"
        ROOT_PART="${NBD_DEV}"
        ;;
    ubuntu)
        if [[ -z "${UBUNTU_VERSION:-}" ]]; then
            echo "ERROR: Manifest missing required field: UBUNTU_VERSION"
            exit 1
        fi
        SOURCE_IMAGE="ubuntu-${UBUNTU_VERSION}-minimal-cloudimg-amd64.img"
        SOURCE_URL="https://cloud-images.ubuntu.com/minimal/releases/noble/release/${SOURCE_IMAGE}"
        OUTPUT_IMAGE="${BUILD_DIR}/${IMAGE_NAME}-ubuntu.qcow2"
        OUTPUT_RAW="${BUILD_DIR}/${IMAGE_NAME}-ubuntu.raw"
        DISTRO_VERSION="${UBUNTU_VERSION}"
        ROOT_PART="${NBD_DEV}p1"
        ;;
    *)
        echo "ERROR: Unknown DISTRO: $DISTRO (supported: alpine, ubuntu)"
        exit 1
        ;;
esac

cleanup() {
    echo "Cleaning up..."
    # Ubuntu chroot has bind mounts that must be released first
    sudo umount "${MOUNT_DIR}/dev/pts" 2>/dev/null || true
    sudo umount "${MOUNT_DIR}/dev"     2>/dev/null || true
    sudo umount "${MOUNT_DIR}/sys"     2>/dev/null || true
    sudo umount "${MOUNT_DIR}/proc"    2>/dev/null || true
    sudo umount "${MOUNT_DIR}"         2>/dev/null || true
    sudo qemu-nbd --disconnect "${NBD_DEV}" 2>/dev/null || true
    exec 9>&- 2>/dev/null || true  # release nbd lock if held
    echo "Done."
}
trap cleanup EXIT

# In quiet mode, redirect build output to /dev/null (import output still shown)
if [[ "$QUIET" == true ]]; then
    exec 3>&1         # save original stdout
    exec 1>/dev/null  # suppress build output
fi

echo "=== System Image Builder ==="
echo "Image:   ${IMAGE_NAME} — ${IMAGE_DESCRIPTION:-}"
echo "Distro:  ${DISTRO} ${DISTRO_VERSION}"
echo "Size:    ${IMAGE_SIZE}"
echo "Build:   ${BUILD_DIR}"
echo ""

# Step 0: Check prerequisites
if ! command -v qemu-nbd &>/dev/null; then
    echo "ERROR: qemu-nbd not found. Install qemu-utils."
    exit 1
fi

if ! command -v qemu-img &>/dev/null; then
    echo "ERROR: qemu-img not found. Install qemu-utils."
    exit 1
fi

if [[ "$DISTRO" == "ubuntu" ]] && ! command -v parted &>/dev/null; then
    echo "ERROR: parted not found. Install parted."
    exit 1
fi

# Build binaries if BUILD_COMMANDS is set
if [[ -n "${BUILD_COMMANDS:-}" ]]; then
    echo "Building binaries: ${BUILD_COMMANDS}"
    if ! (cd "$PROJECT_DIR" && eval "$BUILD_COMMANDS"); then
        echo "ERROR: BUILD_COMMANDS failed: ${BUILD_COMMANDS}"
        exit 1
    fi
fi

# Verify binaries exist; Alpine also requires static linking (uses musl)
if [[ -n "${INSTALL_BINARIES:-}" ]]; then
    IFS=' ' read -ra BINARY_PAIRS <<< "$INSTALL_BINARIES"
    for pair in "${BINARY_PAIRS[@]}"; do
        src="${pair%%:*}"
        src_path="${PROJECT_DIR}/${src}"
        if [[ ! -f "$src_path" ]]; then
            echo "ERROR: Binary not found: $src_path"
            exit 1
        fi
        if [[ "$DISTRO" == "alpine" ]] && ! file "$src_path" | grep -q "statically linked"; then
            echo "ERROR: $src_path is not statically linked (Alpine uses musl — dynamic glibc binaries will fail)"
            echo "  Rebuild with: CGO_ENABLED=0 go build ..."
            exit 1
        fi
    done
fi

# Serialize the entire image build with flock — concurrent builds on the same
# host (e.g. CI single-node + multi-node jobs) share /dev/nbd0 and BUILD_DIR.
NBD_LOCK="/tmp/build-system-image.lock"
echo "Acquiring build lock..."
exec 9>"$NBD_LOCK"
flock 9
echo "Lock acquired"

# If the raw image was built recently (< 10 min), skip the entire build.
# This avoids duplicate work when concurrent CI jobs build the same image.
if [[ -f "$OUTPUT_RAW" ]] && [[ $(( $(date +%s) - $(stat -c %Y "$OUTPUT_RAW") )) -lt 600 ]]; then
    echo "=== Skipping build — $OUTPUT_RAW is fresh (< 10 min old) ==="

    # Restore stdout if suppressed, then jump to import
    if [[ "$QUIET" == true ]]; then
        exec 1>&3 3>&-
    fi

    echo ""
    echo "=== Build complete (cached) ==="
    echo "  raw: $OUTPUT_RAW ($(du -h "$OUTPUT_RAW" | cut -f1))"

    if [[ "$DO_IMPORT" == true ]]; then
        echo "Importing as AMI..."
        IMPORT_ARGS=(--file "$OUTPUT_RAW" --distro "${DISTRO}" --version "${DISTRO_VERSION}" --arch x86_64)
        if [[ -n "${SYSTEM_TAG:-}" ]]; then
            IMPORT_ARGS+=(--tag "$SYSTEM_TAG")
        fi
        (cd "$PROJECT_DIR" && ./bin/spx admin images import "${IMPORT_ARGS[@]}")
    fi
    exit 0
fi

mkdir -p "$BUILD_DIR" "$MOUNT_DIR"

# Step 1: Download base cloud image
if [[ -f "${BUILD_DIR}/${SOURCE_IMAGE}" ]]; then
    echo "Base image already downloaded."
else
    echo "Downloading ${DISTRO} ${DISTRO_VERSION} cloud image..."
    if ! curl --fail -L -o "${BUILD_DIR}/${SOURCE_IMAGE}" "$SOURCE_URL"; then
        rm -f "${BUILD_DIR}/${SOURCE_IMAGE}"
        echo "ERROR: Failed to download image from $SOURCE_URL"
        exit 1
    fi
    # Verify the download is a valid qcow2/img
    if ! qemu-img info "${BUILD_DIR}/${SOURCE_IMAGE}" &>/dev/null; then
        rm -f "${BUILD_DIR}/${SOURCE_IMAGE}"
        echo "ERROR: Downloaded file is not a valid disk image"
        exit 1
    fi
fi

# Step 2: Copy image for customization
rm -f "$OUTPUT_IMAGE"
echo "Copying image for customization..."
cp "${BUILD_DIR}/${SOURCE_IMAGE}" "$OUTPUT_IMAGE"

# Resize the image to provide room for packages
qemu-img resize "$OUTPUT_IMAGE" "$IMAGE_SIZE"

# Step 3: Connect via qemu-nbd
echo "Connecting image via qemu-nbd..."
sudo modprobe nbd max_part=4 2>/dev/null || true
if [[ ! -e "${NBD_DEV}" ]]; then
    echo "ERROR: ${NBD_DEV} does not exist. Is the nbd kernel module loaded? Try: sudo modprobe nbd"
    exit 1
fi
sudo qemu-nbd --disconnect "${NBD_DEV}" 2>/dev/null || true
sudo qemu-nbd --connect="${NBD_DEV}" "$OUTPUT_IMAGE"

# Wait for the nbd device to be ready
for i in $(seq 1 10); do
    if sudo blockdev --getsize64 "${NBD_DEV}" &>/dev/null; then
        break
    fi
    sleep 1
done
sleep 1

# Ubuntu images have a partition table; wait for partition devices then resize
# the partition to fill the expanded image before resizing the filesystem.
if [[ "$DISTRO" == "ubuntu" ]]; then
    for i in $(seq 1 10); do
        if [[ -e "${ROOT_PART}" ]]; then break; fi
        sleep 1
    done
    echo "Resizing partition..."
    # After qemu-img resize the GPT backup header sits at the old end of disk.
    # Move it to the new end before parted tries to resize, otherwise parted
    # refuses with "Unable to satisfy all constraints on the partition."
    sudo sgdisk --move-second-header "${NBD_DEV}" 2>/dev/null || true
    sudo parted --script "${NBD_DEV}" resizepart 1 100%
fi

# Alpine cloud images have ext4 directly on the block device (no partition table).
echo "Checking filesystem..."
sudo e2fsck -f -y "${ROOT_PART}" || {
    ec=$?
    if [[ $ec -gt 1 ]]; then
        echo "ERROR: e2fsck failed with exit code $ec on ${ROOT_PART}"
        exit 1
    fi
}

echo "Resizing filesystem..."
if ! sudo resize2fs "${ROOT_PART}"; then
    echo "ERROR: resize2fs failed on ${ROOT_PART}"
    exit 1
fi

# Step 4: Mount and customize
echo "Mounting root filesystem..."
sudo mount "${ROOT_PART}" "$MOUNT_DIR"

# Set up resolv.conf for DNS inside chroot.
# Ubuntu cloud images symlink /etc/resolv.conf → /run/systemd/resolve/stub-resolv.conf
# which doesn't exist yet. Remove the symlink and write a real file.
sudo rm -f "${MOUNT_DIR}/etc/resolv.conf"
sudo cp /etc/resolv.conf "${MOUNT_DIR}/etc/resolv.conf"

# Ubuntu chroot requires /proc /sys /dev bind mounts for systemd and DKMS
if [[ "$DISTRO" == "ubuntu" ]]; then
    sudo mount --bind /proc       "${MOUNT_DIR}/proc"
    sudo mount --bind /sys        "${MOUNT_DIR}/sys"
    sudo mount --bind /dev        "${MOUNT_DIR}/dev"
    sudo mount --bind /dev/pts    "${MOUNT_DIR}/dev/pts"
fi

# Install packages
if [[ "$DISTRO" == "alpine" ]] && [[ -n "${APK_PACKAGES:-}" ]]; then
    echo "Installing packages: ${APK_PACKAGES}..."
    sudo chroot "$MOUNT_DIR" /bin/sh -c "
set -e
# Enable community repo
sed -i 's|^#\(.*community\)|\1|' /etc/apk/repositories 2>/dev/null || true

# Ensure community repo is present
if ! grep -q community /etc/apk/repositories; then
    MIRROR=\$(grep main /etc/apk/repositories | head -1 | sed 's|/main|/community|')
    echo \"\$MIRROR\" >> /etc/apk/repositories
fi

apk update
apk add ${APK_PACKAGES}
"
elif [[ "$DISTRO" == "ubuntu" ]] && [[ -n "${APT_PACKAGES:-}" ]]; then
    echo "Installing packages: ${APT_PACKAGES}..."
    sudo chroot "$MOUNT_DIR" /bin/bash -c "
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends ${APT_PACKAGES}
"
fi

# Enable services
if [[ -n "${ENABLE_SERVICES:-}" ]]; then
    echo "Enabling services: ${ENABLE_SERVICES}..."
    IFS=' ' read -ra SERVICES <<< "$ENABLE_SERVICES"
    for svc in "${SERVICES[@]}"; do
        if [[ "$DISTRO" == "alpine" ]]; then
            if ! sudo chroot "$MOUNT_DIR" /bin/sh -c "rc-update add ${svc} default"; then
                echo "ERROR: Failed to enable service '${svc}' — does it exist in the image?"
                exit 1
            fi
        else
            if ! sudo chroot "$MOUNT_DIR" /bin/bash -c "systemctl enable ${svc}"; then
                echo "ERROR: Failed to enable service '${svc}'"
                exit 1
            fi
        fi
    done
fi

# Step 5: Copy binaries into the image (before setup script, which may reference them)
if [[ -n "${INSTALL_BINARIES:-}" ]]; then
    echo "Installing binaries..."
    IFS=' ' read -ra BINARY_PAIRS <<< "$INSTALL_BINARIES"
    for pair in "${BINARY_PAIRS[@]}"; do
        src="${pair%%:*}"
        dst="${pair#*:}"
        src_path="${PROJECT_DIR}/${src}"
        echo "  ${src} -> ${dst}"
        sudo cp "$src_path" "${MOUNT_DIR}${dst}"
        sudo chmod 755 "${MOUNT_DIR}${dst}"
    done
fi

# Run custom setup script inside chroot (after binaries are installed)
if [[ -n "${SETUP_SCRIPT:-}" ]]; then
    setup_path="${PROJECT_DIR}/${SETUP_SCRIPT}"
    if [[ ! -f "$setup_path" ]]; then
        echo "ERROR: Setup script not found: $setup_path"
        exit 1
    fi
    echo "Running setup script: ${SETUP_SCRIPT}..."
    sudo cp "$setup_path" "${MOUNT_DIR}/tmp/setup.sh"
    sudo chmod 755 "${MOUNT_DIR}/tmp/setup.sh"
    if [[ "$DISTRO" == "ubuntu" ]]; then
        sudo chroot "$MOUNT_DIR" /bin/bash /tmp/setup.sh
    else
        sudo chroot "$MOUNT_DIR" /tmp/setup.sh
    fi
    sudo rm -f "${MOUNT_DIR}/tmp/setup.sh"
fi

# Step 6: Clean up and unmount
echo "Cleaning up image..."
if [[ "$DISTRO" == "alpine" ]]; then
    sudo chroot "$MOUNT_DIR" /bin/sh -c '
apk cache clean 2>/dev/null || true
rm -rf /var/cache/apk/* /tmp/*
'
else
    sudo chroot "$MOUNT_DIR" /bin/bash -c '
export DEBIAN_FRONTEND=noninteractive
apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/*
'
fi

# Restore the systemd-resolved symlink (Ubuntu default); cloud-init sets DNS on boot.
sudo rm -f "${MOUNT_DIR}/etc/resolv.conf"
sudo ln -sf /run/systemd/resolve/stub-resolv.conf "${MOUNT_DIR}/etc/resolv.conf"

echo "Unmounting..."
if [[ "$DISTRO" == "ubuntu" ]]; then
    sudo umount "${MOUNT_DIR}/dev/pts"
    sudo umount "${MOUNT_DIR}/dev"
    sudo umount "${MOUNT_DIR}/sys"
    sudo umount "${MOUNT_DIR}/proc"
fi
sudo umount "$MOUNT_DIR"
sudo qemu-nbd --disconnect "${NBD_DEV}"

# Step 7: Convert to raw for import
echo "Converting to raw format..."
qemu-img convert -f qcow2 -O raw "$OUTPUT_IMAGE" "$OUTPUT_RAW"

# Restore stdout if suppressed
if [[ "$QUIET" == true ]]; then
    exec 1>&3 3>&-
fi

echo ""
echo "=== Build complete ==="
echo "  Image: ${IMAGE_NAME}"
echo "  qcow2: $OUTPUT_IMAGE ($(du -h "$OUTPUT_IMAGE" | cut -f1))"
echo "  raw:   $OUTPUT_RAW ($(du -h "$OUTPUT_RAW" | cut -f1))"
echo ""

if [[ "$DO_IMPORT" == true ]]; then
    echo "Importing as AMI..."
    IMPORT_ARGS=(--file "$OUTPUT_RAW" --distro "${DISTRO}" --version "${DISTRO_VERSION}" --arch x86_64)
    if [[ -n "${SYSTEM_TAG:-}" ]]; then
        IMPORT_ARGS+=(--tag "$SYSTEM_TAG")
    fi
    (cd "$PROJECT_DIR" && sudo -u spinifex-storage ./bin/spx admin images import "${IMPORT_ARGS[@]}")
else
    echo "To import as AMI, run:"
    echo "  cd $PROJECT_DIR && ./bin/spx admin images import \\"
    echo "    --file $OUTPUT_RAW \\"
    if [[ -n "${SYSTEM_TAG:-}" ]]; then
        echo "    --distro ${DISTRO} --version ${DISTRO_VERSION} --arch x86_64 \\"
        echo "    --tag ${SYSTEM_TAG}"
    else
        echo "    --distro ${DISTRO} --version ${DISTRO_VERSION} --arch x86_64"
    fi
fi
