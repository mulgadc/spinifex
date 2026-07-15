# shellcheck shell=bash
# Shared plumbing for the containerised build and publish steps. Both run the
# same pinned toolchain image, so building that image lives here once rather
# than drifting between the two callers.

MKOSI_REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILDER_TAG="${BUILDER_TAG:-spinifex-mkosi-builder}"
BUILDER_DOCKERFILE="${MKOSI_REPO_ROOT}/scripts/mkosi-builder.Dockerfile"

require_docker() {
    command -v docker >/dev/null || {
        echo "docker not found — it is the only host requirement" >&2
        exit 1
    }
}

# The builder account is matched to the invoking user so bind-mounted output is
# not written back as some other uid. Docker caches every layer after the first
# call, so re-running this per invocation costs a fraction of a second.
build_builder_image() {
    echo "[${0##*/}] building toolchain image ${BUILDER_TAG} (cached after first run)"
    docker build \
        --quiet \
        --file "${BUILDER_DOCKERFILE}" \
        --build-arg "BUILDER_UID=$(id -u)" \
        --build-arg "BUILDER_GID=$(id -g)" \
        --tag "${BUILDER_TAG}" \
        "${MKOSI_REPO_ROOT}/scripts" >/dev/null
}
