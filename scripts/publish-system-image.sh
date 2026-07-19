#!/bin/bash
set -euo pipefail

# publish-system-image.sh — Publish a built system AMI to Cloudflare R2
#
# Converts the raw image produced by build-system-image.sh into a compressed
# qcow2, writes a .sha256 sidecar (coreutils format, matched by the spx catalog
# checksum verifier), and uploads both to the R2 bucket that backs
# iso.mulgadc.com/system-ami/. The matching catalog entry in
# spinifex/spinifex/utils/images.go then lets operators run:
#
#   spx admin images import --name <catalog-key>
#
# Object name is <AMI_NAME or IMAGE_NAME>-<arch>.qcow2 — this MUST match the
# basename of the catalog entry's URL.
#
# Requirements: qemu-img, sha256sum, aws (CLI; R2 speaks the S3 API)
# Usage: ./scripts/publish-system-image.sh <manifest.conf> [--build] [--dry-run]
#   --build    Run build-system-image.sh first (otherwise reuse the cached raw)
#   --dry-run  Print the convert/checksum/upload steps without uploading
#
# Env (required unless --dry-run):
#   R2_ENDPOINT            https://<account>.r2.cloudflarestorage.com
#   R2_BUCKET              defaults to "spinifex-iso" (backs iso.mulgadc.com)
#   AWS_ACCESS_KEY_ID      R2 token access key
#   AWS_SECRET_ACCESS_KEY  R2 token secret

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

usage() {
    echo "Usage: $0 <manifest.conf> [--build] [--dry-run]"
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

DO_BUILD=false
DRY_RUN=false
for arg in "$@"; do
    case "$arg" in
        --build)   DO_BUILD=true ;;
        --dry-run) DRY_RUN=true ;;
        *) echo "ERROR: unknown flag: $arg"; usage ;;
    esac
done

# shellcheck source=/dev/null
source "$MANIFEST"

DISTRO="${DISTRO:-alpine}"
ARCH="x86_64" # build-system-image.sh builds x86_64 only today
R2_BUCKET="${R2_BUCKET:-spinifex-iso}" # iso.mulgadc.com is served from this bucket

if [[ -z "${IMAGE_NAME:-}" ]]; then
    echo "ERROR: Manifest missing required field: IMAGE_NAME"
    exit 1
fi

# Mirror build-system-image.sh's derived paths so we reuse its cached raw.
# Honor SYSTEM_IMAGE_BUILD_DIR exactly as the build script does — the workflow
# points it off tmpfs so the 16G Ubuntu GPU raw fits, and the publish step must
# look where the build actually wrote, not a hardcoded /tmp.
BUILD_DIR="${SYSTEM_IMAGE_BUILD_DIR:-/tmp}/${IMAGE_NAME}-image-build"
RAW_IMAGE="${BUILD_DIR}/${IMAGE_NAME}-${DISTRO}.raw"

OBJECT_BASE="${AMI_NAME:-$IMAGE_NAME}-${ARCH}"
QCOW2_OUT="${BUILD_DIR}/${OBJECT_BASE}.qcow2"
SHA_OUT="${QCOW2_OUT}.sha256"
OBJECT_QCOW2="${OBJECT_BASE}.qcow2"
OBJECT_SHA="${OBJECT_BASE}.qcow2.sha256"

for tool in qemu-img sha256sum; do
    if ! command -v "$tool" &>/dev/null; then
        echo "ERROR: $tool not found"
        exit 1
    fi
done
if [[ "$DRY_RUN" == false ]] && ! command -v aws &>/dev/null; then
    echo "ERROR: aws CLI not found (required for R2 upload; use --dry-run to skip)"
    exit 1
fi

if [[ "$DO_BUILD" == true ]]; then
    echo "=== Building ${IMAGE_NAME} ==="
    # --no-cache: publishing must never reuse a stale raw left by a prior or
    # concurrent build on a persistent runner.
    "${SCRIPT_DIR}/build-system-image.sh" "$MANIFEST" --no-cache
fi

if [[ ! -f "$RAW_IMAGE" ]]; then
    echo "ERROR: Raw image not found: $RAW_IMAGE"
    echo "       Run with --build, or 'make build-system-image IMAGE=...' first."
    exit 1
fi

if [[ "$DRY_RUN" == false ]]; then
    for var in R2_ENDPOINT AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY; do
        if [[ -z "${!var:-}" ]]; then
            echo "ERROR: $var must be set (or use --dry-run)"
            exit 1
        fi
    done
fi

echo "=== Publish System Image ==="
echo "  Manifest: ${MANIFEST}"
echo "  Raw:      ${RAW_IMAGE}"
echo "  Object:   s3://${R2_BUCKET}/system-ami/${OBJECT_QCOW2}"
echo ""

run() {
    echo "+ $*"
    if [[ "$DRY_RUN" == false ]]; then
        "$@"
    fi
}

# Compressed qcow2 keeps the transfer small (consistent with the GPU images).
run qemu-img convert -c -f raw -O qcow2 "$RAW_IMAGE" "$QCOW2_OUT"

# Sidecar in coreutils format: "<hex>  <basename>". The spx catalog verifier
# matches the entry by the downloaded file's basename, so write the basename
# the catalog URL resolves to, not the local build path.
if [[ "$DRY_RUN" == false ]]; then
    ( cd "$(dirname "$QCOW2_OUT")" && sha256sum "$(basename "$QCOW2_OUT")" > "$(basename "$SHA_OUT")" )
    echo "  sha256: $(cut -d' ' -f1 "$SHA_OUT")"
else
    echo "+ sha256sum ${OBJECT_QCOW2} > ${OBJECT_SHA}"
fi

S3_PREFIX="s3://${R2_BUCKET}/system-ami"
run aws s3 cp --endpoint-url "${R2_ENDPOINT:-<R2_ENDPOINT>}" "$QCOW2_OUT" "${S3_PREFIX}/${OBJECT_QCOW2}"
run aws s3 cp --endpoint-url "${R2_ENDPOINT:-<R2_ENDPOINT>}" "$SHA_OUT" "${S3_PREFIX}/${OBJECT_SHA}"

echo ""
if [[ "$DRY_RUN" == true ]]; then
    echo "=== Dry run complete (no upload) ==="
else
    echo "=== Published ==="
    echo "  https://iso.mulgadc.com/system-ami/${OBJECT_QCOW2}"
    echo "  https://iso.mulgadc.com/system-ami/${OBJECT_SHA}"
fi
