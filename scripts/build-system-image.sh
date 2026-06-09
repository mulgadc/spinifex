#!/bin/bash
set -euo pipefail

# build-system-image.sh — Build a minimal system AMI from a manifest
#
# Supports Alpine Linux and Ubuntu 24.04+ as base distros. Creates a pre-baked
# image with custom packages, binaries, and setup scripts installed, ready for
# import as a Spinifex AMI.
#
# Customization runs entirely inside the libguestfs appliance (its own kernel +
# userspace), so the build touches NO host block device, NO host mount, and
# needs NO sudo. This deliberately replaces the previous qemu-nbd + `sudo mount`
# + chroot + /dev bind-mount flow, whose leaked binds could be traversed by a
# later `rm -rf` and wipe the host's /dev (it wedged CI runners). libguestfs
# isolates all of that in a throwaway VM.
#
# Requirements: virt-customize, guestfish, virt-resize (libguestfs-tools),
#               qemu-img, curl
# Usage: ./scripts/build-system-image.sh <manifest.conf> [--import]
#   --import  Also import the image as an AMI via spx admin images import

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# No libvirt on CI runners — drive qemu directly. Harmless when libvirt exists.
export LIBGUESTFS_BACKEND="${LIBGUESTFS_BACKEND:-direct}"

usage() {
    echo "Usage: $0 <manifest.conf> [--import] [--no-cache]"
    echo ""
    echo "  manifest.conf  Path to image manifest (see scripts/images/ for examples)"
    echo "  --import       Import the built image as an AMI"
    echo "  --no-cache     Always rebuild; ignore a recently-built cached raw"
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
NO_CACHE=false
for arg in "$@"; do
    case "$arg" in
        --import)   DO_IMPORT=true ;;
        --quiet)    QUIET=true ;;
        --no-cache) NO_CACHE=true ;;
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
        BOOT_MODE="bios"
        ;;
    ubuntu)
        if [[ -z "${UBUNTU_VERSION:-}" ]]; then
            echo "ERROR: Manifest missing required field: UBUNTU_VERSION"
            exit 1
        fi
        SOURCE_IMAGE="ubuntu-${UBUNTU_VERSION}-minimal-cloudimg-amd64.img"
        SOURCE_URL="https://cloud-images.ubuntu.com/minimal/releases/${UBUNTU_CODENAME:-noble}/release/${SOURCE_IMAGE}"
        OUTPUT_IMAGE="${BUILD_DIR}/${IMAGE_NAME}-ubuntu.qcow2"
        OUTPUT_RAW="${BUILD_DIR}/${IMAGE_NAME}-ubuntu.raw"
        DISTRO_VERSION="${UBUNTU_VERSION}"
        BOOT_MODE="uefi"
        ;;
    *)
        echo "ERROR: Unknown DISTRO: $DISTRO (supported: alpine, ubuntu)"
        exit 1
        ;;
esac

# import_ami registers the built raw image as a Spinifex AMI. Shared by the
# fresh-build and cached-build paths.
import_ami() {
    echo "Importing as AMI..."
    rm -f "$OUTPUT_IMAGE"
    local args=(--file "$OUTPUT_RAW" --distro "${DISTRO}" --version "${DISTRO_VERSION}" --arch x86_64 --boot-mode "${BOOT_MODE}")
    if [[ -n "${AMI_NAME:-}" ]]; then
        args+=(--ami-name "$AMI_NAME")
    fi
    if [[ -n "${SYSTEM_TAG:-}" ]]; then
        args+=(--tag "$SYSTEM_TAG")
    fi
    spx admin images import "${args[@]}"
}

# In quiet mode, redirect build output to /dev/null (import output still shown)
if [[ "$QUIET" == true ]]; then
    exec 3>&1         # save original stdout
    exec 1>/dev/null  # suppress build output
fi

echo "=== System Image Builder (libguestfs) ==="
echo "Image:   ${IMAGE_NAME} — ${IMAGE_DESCRIPTION:-}"
echo "Distro:  ${DISTRO} ${DISTRO_VERSION}"
echo "Size:    ${IMAGE_SIZE}"
echo "Build:   ${BUILD_DIR}"
echo ""

# Step 0: Check prerequisites
for tool in virt-customize qemu-img; do
    if ! command -v "$tool" &>/dev/null; then
        echo "ERROR: $tool not found. Install libguestfs-tools + qemu-utils."
        exit 1
    fi
done
if [[ "$DISTRO" == "alpine" ]] && ! command -v guestfish &>/dev/null; then
    echo "ERROR: guestfish not found. Install libguestfs-tools."
    exit 1
fi
if [[ "$DISTRO" == "ubuntu" ]] && ! command -v virt-resize &>/dev/null; then
    echo "ERROR: virt-resize not found. Install libguestfs-tools."
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

# Serialize the build with flock — concurrent builds on the same host (e.g. CI
# single-node + multi-node jobs) share BUILD_DIR + the cached raw image.
BUILD_LOCK="/tmp/build-system-image.lock"
echo "Acquiring build lock..."
exec 9>"$BUILD_LOCK"
flock 9
echo "Lock acquired"

# If the raw image was built recently (< 10 min), skip the entire build.
# This avoids duplicate work when concurrent CI jobs build the same image.
# --no-cache (e.g. the publish path) forces a fresh build so a stale raw from
# a prior/concurrent run on a persistent runner is never republished.
if [[ "$NO_CACHE" == false ]] && [[ -f "$OUTPUT_RAW" ]] && [[ $(( $(date +%s) - $(stat -c %Y "$OUTPUT_RAW") )) -lt 600 ]]; then
    echo "=== Skipping build — $OUTPUT_RAW is fresh (< 10 min old) ==="

    # Restore stdout if suppressed, then jump to import
    if [[ "$QUIET" == true ]]; then
        exec 1>&3 3>&-
    fi

    echo ""
    echo "=== Build complete (cached) ==="
    echo "  raw: $OUTPUT_RAW ($(du -h "$OUTPUT_RAW" | cut -f1))"

    if [[ "$DO_IMPORT" == true ]]; then
        import_ami
    fi
    exit 0
fi

mkdir -p "$BUILD_DIR"

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

# Step 3: Resize to provide room for packages. Both paths grow the root
# filesystem entirely inside the libguestfs appliance — no host loop/nbd device.
echo "Resizing image to ${IMAGE_SIZE}..."
if [[ "$DISTRO" == "alpine" ]]; then
    # Alpine cloud images have ext4 directly on the whole disk (no partition
    # table); grow the disk file then expand the fs from inside the appliance.
    qemu-img resize "$OUTPUT_IMAGE" "$IMAGE_SIZE"
    guestfish --rw -a "$OUTPUT_IMAGE" run \
        : e2fsck /dev/sda forceall:true \
        : resize2fs /dev/sda
else
    # Ubuntu images use a GPT partition table with the root fs on /dev/sda1;
    # virt-resize expands the partition + fs into a fresh IMAGE_SIZE disk.
    RESIZED="${OUTPUT_IMAGE}.resized"
    rm -f "$RESIZED"
    qemu-img create -f qcow2 "$RESIZED" "$IMAGE_SIZE"
    virt-resize --expand /dev/sda1 "$OUTPUT_IMAGE" "$RESIZED"
    mv -f "$RESIZED" "$OUTPUT_IMAGE"
fi

# Step 4: Assemble the virt-customize operation list. virt-customize runs the
# operations in the order they appear on the command line, so this mirrors the
# old chroot sequence: packages → binaries → files → setup → services → clean.
CUST=(virt-customize -a "$OUTPUT_IMAGE" --network)
if [[ "$DISTRO" == "ubuntu" ]]; then
    # DKMS driver bakes (GPU images) need real resources in the appliance.
    CUST+=(--memsize 6144 --smp 4)
else
    CUST+=(--memsize 2048)
fi

# Packages
if [[ "$DISTRO" == "alpine" ]] && [[ -n "${APK_PACKAGES:-}" ]]; then
    echo "Will install packages: ${APK_PACKAGES}"
    CUST+=(--run-command 'sed -i "s|^#\(.*community\)|\1|" /etc/apk/repositories 2>/dev/null || true')
    CUST+=(--run-command 'grep -q community /etc/apk/repositories || grep main /etc/apk/repositories | head -1 | sed "s|/main|/community|" >> /etc/apk/repositories')
    CUST+=(--run-command 'apk update')
    CUST+=(--run-command "apk add ${APK_PACKAGES}")
elif [[ "$DISTRO" == "ubuntu" ]] && [[ -n "${APT_PACKAGES:-}" ]]; then
    echo "Will install packages: ${APT_PACKAGES}"
    CUST+=(--run-command 'export DEBIAN_FRONTEND=noninteractive; apt-get update')
    CUST+=(--run-command "export DEBIAN_FRONTEND=noninteractive; apt-get install -y --no-install-recommends ${APT_PACKAGES}")
fi

# Binaries (mode 0755). --mkdir is idempotent (mkdir -p semantics).
if [[ -n "${INSTALL_BINARIES:-}" ]]; then
    IFS=' ' read -ra BINARY_PAIRS <<< "$INSTALL_BINARIES"
    for pair in "${BINARY_PAIRS[@]}"; do
        src="${pair%%:*}"
        dst="${pair#*:}"
        src_path="${PROJECT_DIR}/${src}"
        CUST+=(--mkdir "$(dirname "$dst")")
        CUST+=(--upload "${src_path}:${dst}")
        CUST+=(--chmod "0755:${dst}")
    done
fi

# Auxiliary files (systemd units, OpenRC initd scripts, cron entries, configs).
# Mode 0644; setup scripts may chmod role-specific files to 0755 afterwards.
if [[ -n "${INSTALL_FILES:-}" ]]; then
    IFS=' ' read -ra FILE_PAIRS <<< "$INSTALL_FILES"
    for pair in "${FILE_PAIRS[@]}"; do
        src="${pair%%:*}"
        dst="${pair#*:}"
        src_path="${PROJECT_DIR}/${src}"
        if [[ ! -f "$src_path" ]]; then
            echo "ERROR: INSTALL_FILES source not found: $src_path"
            exit 1
        fi
        CUST+=(--mkdir "$(dirname "$dst")")
        CUST+=(--upload "${src_path}:${dst}")
        CUST+=(--chmod "0644:${dst}")
    done
fi

# Custom setup script (uploaded + executed + removed by --run, inside the guest).
if [[ -n "${SETUP_SCRIPT:-}" ]]; then
    setup_path="${PROJECT_DIR}/${SETUP_SCRIPT}"
    if [[ ! -f "$setup_path" ]]; then
        echo "ERROR: Setup script not found: $setup_path"
        exit 1
    fi
    CUST+=(--run "$setup_path")
fi

# Enable services (after files + setup so referenced init scripts exist).
if [[ -n "${ENABLE_SERVICES:-}" ]]; then
    IFS=' ' read -ra SERVICES <<< "$ENABLE_SERVICES"
    for svc in "${SERVICES[@]}"; do
        if [[ "$DISTRO" == "alpine" ]]; then
            CUST+=(--run-command "rc-update add ${svc} default")
        else
            CUST+=(--run-command "systemctl enable ${svc}")
        fi
    done
fi

# Final cleanup of package caches + tmp.
if [[ "$DISTRO" == "alpine" ]]; then
    CUST+=(--run-command 'apk cache clean 2>/dev/null || true; rm -rf /var/cache/apk/* /tmp/* 2>/dev/null || true')
else
    CUST+=(--run-command 'export DEBIAN_FRONTEND=noninteractive; apt-get clean; rm -rf /var/lib/apt/lists/* /tmp/* 2>/dev/null || true')
fi

echo "Customizing image (libguestfs appliance)..."
"${CUST[@]}"

# Step 5: Convert to raw for import
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
    import_ami
else
    echo "To import as AMI, run:"
    echo "  spx admin images import \\"
    echo "    --file $OUTPUT_RAW \\"
    NAME_HINT=""
    if [[ -n "${AMI_NAME:-}" ]]; then
        NAME_HINT=" \\\n    --ami-name ${AMI_NAME}"
    fi
    if [[ -n "${SYSTEM_TAG:-}" ]]; then
        echo -e "    --distro ${DISTRO} --version ${DISTRO_VERSION} --arch x86_64 --boot-mode ${BOOT_MODE}${NAME_HINT} \\"
        echo "    --tag ${SYSTEM_TAG}"
    else
        echo -e "    --distro ${DISTRO} --version ${DISTRO_VERSION} --arch x86_64 --boot-mode ${BOOT_MODE}${NAME_HINT}"
    fi
fi
