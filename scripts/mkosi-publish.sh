#!/usr/bin/env bash
# mkosi-publish.sh — publish a built system image to Cloudflare R2, from inside
# the same pinned toolchain container as the build.
#
# Converts mkosi's raw output to a compressed qcow2, writes the .sha256 sidecar
# in the coreutils format the spx catalog verifier matches, and uploads both to
# the bucket behind iso.mulgadc.com/system-ami/. The object name must match the
# basename of the catalog entry's URL in spinifex/utils/images.go.
#
# Unlike the build, this needs no relaxed confinement: qemu-img and aws create
# no namespaces, so the container keeps Docker's default seccomp and AppArmor
# profiles.
#
# Usage:
#   scripts/mkosi-publish.sh --name <object-base> [--image <raw>] [--dry-run]
#
#   scripts/mkosi-publish.sh --name eks-node-x86_64 --dry-run
#
# Env (required unless --dry-run):
#   R2_ENDPOINT            https://<account>.r2.cloudflarestorage.com
#   R2_BUCKET              defaults to spinifex-iso (backs iso.mulgadc.com)
#   AWS_ACCESS_KEY_ID      R2 token access key
#   AWS_SECRET_ACCESS_KEY  R2 token secret
#
# Env (optional):
#   MKOSI_OUTPUT_DIR  where the build left its artefacts (default: images/output)
set -euo pipefail

# shellcheck source=scripts/mkosi-common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/mkosi-common.sh"

OUTPUT_DIR="${MKOSI_OUTPUT_DIR:-${MKOSI_REPO_ROOT}/images/output}"
RAW_NAME="spinifex.raw"
OBJECT_BASE=""
DRY_RUN=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --name)    OBJECT_BASE="$2"; shift 2 ;;
        --image)   RAW_NAME="$2"; shift 2 ;;
        --dry-run) DRY_RUN=1; shift ;;
        *) echo "mkosi-publish: unknown arg: $1" >&2; exit 2 ;;
    esac
done

[[ -n "${OBJECT_BASE}" ]] || { echo "mkosi-publish: --name is required" >&2; exit 2; }
[[ -f "${OUTPUT_DIR}/${RAW_NAME}" ]] || {
    echo "mkosi-publish: no raw image at ${OUTPUT_DIR}/${RAW_NAME} — run mkosi-build.sh first" >&2
    exit 1
}

R2_BUCKET="${R2_BUCKET:-spinifex-iso}"

# Fail before the (slow) convert rather than after it, so a missing credential
# is not discovered minutes into the run.
if [[ "${DRY_RUN}" -eq 0 ]]; then
    for var in R2_ENDPOINT AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY; do
        [[ -n "${!var:-}" ]] || { echo "mkosi-publish: ${var} must be set (or use --dry-run)" >&2; exit 1; }
    done
fi

require_docker
build_builder_image

DOCKER_ARGS=(
    --rm
    --volume "${OUTPUT_DIR}:/work/output"
    --workdir /work/output
    --env "R2_BUCKET=${R2_BUCKET}"
    --env "R2_ENDPOINT=${R2_ENDPOINT:-}"
    --env "AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID:-}"
    --env "AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY:-}"
    --env "OBJECT_BASE=${OBJECT_BASE}"
    --env "RAW_NAME=${RAW_NAME}"
    --env "DRY_RUN=${DRY_RUN}"
)

echo "[mkosi-publish] raw:    ${OUTPUT_DIR}/${RAW_NAME}"
echo "[mkosi-publish] object: s3://${R2_BUCKET}/system-ami/${OBJECT_BASE}.qcow2"

docker run "${DOCKER_ARGS[@]}" "${BUILDER_TAG}" bash -euo pipefail -c '
qcow2="${OBJECT_BASE}.qcow2"
sha="${qcow2}.sha256"
s3="s3://${R2_BUCKET}/system-ami"

# Compressed qcow2 keeps the transfer small, consistent with the GPU images.
echo "+ qemu-img convert -c -f raw -O qcow2 ${RAW_NAME} ${qcow2}"
qemu-img convert -c -f raw -O qcow2 "${RAW_NAME}" "${qcow2}"

# Sidecar in coreutils format ("<hex>  <basename>"): the catalog verifier
# matches on the downloaded file basename, so the name recorded here must be
# the one the catalog URL resolves to, not a local path.
sha256sum "${qcow2}" > "${sha}"
echo "  sha256: $(cut -d" " -f1 "${sha}")"

if [ "${DRY_RUN}" = "1" ]; then
    echo "+ aws s3 cp ${qcow2} ${s3}/${qcow2}   (skipped: --dry-run)"
    echo "+ aws s3 cp ${sha} ${s3}/${sha}   (skipped: --dry-run)"
    exit 0
fi

# The object path carries no version and the catalog resolves newest-wins, so
# an upload overwrites the live image with no way back. Copy the current object
# aside first: without this, a bad build is unrecoverable rather than a restore.
if aws s3 ls --endpoint-url "${R2_ENDPOINT}" "${s3}/${qcow2}" >/dev/null 2>&1; then
    stamp="$(date -u +%Y%m%dT%H%M%SZ)"
    echo "+ backing up current object to ${s3}/archive/${OBJECT_BASE}-${stamp}.qcow2"
    aws s3 cp --endpoint-url "${R2_ENDPOINT}" \
        "${s3}/${qcow2}" "${s3}/archive/${OBJECT_BASE}-${stamp}.qcow2"
    aws s3 cp --endpoint-url "${R2_ENDPOINT}" \
        "${s3}/${sha}" "${s3}/archive/${OBJECT_BASE}-${stamp}.qcow2.sha256" || true
fi

# Upload the image before its sidecar: the reverse advertises a checksum for
# bytes that are not there yet, so a concurrent import fails verification.
aws s3 cp --endpoint-url "${R2_ENDPOINT}" "${qcow2}" "${s3}/${qcow2}"
aws s3 cp --endpoint-url "${R2_ENDPOINT}" "${sha}" "${s3}/${sha}"

echo "=== Published ==="
echo "  https://iso.mulgadc.com/system-ami/${qcow2}"
echo "  https://iso.mulgadc.com/system-ami/${sha}"
'
