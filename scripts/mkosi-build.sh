#!/usr/bin/env bash
# mkosi-build.sh — run an mkosi system-image build inside the pinned builder
# container.
#
# The runner needs only Docker: the toolchain is baked into the image (see
# scripts/mkosi-builder.Dockerfile), so nothing is installed on the host and
# the build cannot drift between runners. The same command runs identically on
# a developer's box.
#
# Usage:
#   scripts/mkosi-build.sh [--profile <name>] [--shell] [-- <mkosi options>]
#
#   scripts/mkosi-build.sh --profile eks-node -- --force
#   MKOSI_VERB=clean scripts/mkosi-build.sh
#
# Args after `--` are mkosi OPTIONS only; the verb comes from MKOSI_VERB.
#
# Env:
#   MKOSI_IMAGE_DIR   directory holding mkosi.conf (default: images/)
#   MKOSI_OUTPUT_DIR  where artefacts land (default: <image dir>/output)
#   MKOSI_VERB        mkosi verb to run (default: build)
#   BUILDER_TAG       builder image tag (default: spinifex-mkosi-builder)
set -euo pipefail

# shellcheck source=scripts/mkosi-common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/mkosi-common.sh"

IMAGE_DIR="${MKOSI_IMAGE_DIR:-${MKOSI_REPO_ROOT}/images}"
OUTPUT_DIR="${MKOSI_OUTPUT_DIR:-${IMAGE_DIR}/output}"

PROFILE=""
WANT_SHELL=0
MKOSI_ARGS=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --profile) PROFILE="$2"; shift 2 ;;
        --shell)   WANT_SHELL=1; shift ;;
        --)        shift; MKOSI_ARGS=("$@"); break ;;
        *)         echo "mkosi-build: unknown arg: $1" >&2; exit 2 ;;
    esac
done

require_docker

if [[ ! -d "${IMAGE_DIR}" ]]; then
    echo "mkosi-build: no image dir at ${IMAGE_DIR} (set MKOSI_IMAGE_DIR)" >&2
    exit 1
fi

build_builder_image

mkdir -p "${OUTPUT_DIR}"

# mkosi builds in a user namespace, and two of Docker's default confinements
# block that. Neither is a privilege grant: capabilities stay dropped and no
# devices are exposed, unlike --privileged.
#
#   seccomp=unconfined   the default profile masks CLONE_NEWUSER out of clone,
#                        so the namespace cannot be created at all.
#   apparmor=unconfined  the docker-default profile separately blocks unsharing
#                        the MOUNT namespace. This one is easy to miss: with
#                        seccomp alone `unshare -U` succeeds and only
#                        `unshare -U -m` fails.
DOCKER_ARGS=(
    --rm
    --security-opt seccomp=unconfined
    --security-opt apparmor=unconfined
    --volume "${IMAGE_DIR}:/work/images"
    --volume "${OUTPUT_DIR}:/work/output"
    --workdir /work/images
)
[[ -t 0 ]] && DOCKER_ARGS+=(--interactive --tty)

# Persist the package cache across runs so a rebuild does not re-download the
# whole target distribution every time. Only the downloaded packages live here:
# see the workspace note below for why the build area deliberately does not.
CACHE_VOL="${BUILDER_TAG}-cache"
docker volume create "${CACHE_VOL}" >/dev/null
DOCKER_ARGS+=(--volume "${CACHE_VOL}:/home/builder/.cache")

# The workspace must sit on the same mount as the output directory. mkosi
# assembles the image in the workspace and moves the result to the output, and
# a rename across mounts fails EXDEV — Docker's named volume and the output
# bind mount are separate mounts even on one filesystem. mkosi then falls back
# to copying, which needs the finished image to exist twice at once. That is
# invisible on a small image and fatal on a real one: a ~4G GPU image needs ~8G
# to land. Keeping both on one mount makes the move a rename again.
WORKSPACE_DIR="${OUTPUT_DIR}/.mkosi-workspace"
mkdir -p "${WORKSPACE_DIR}"

if [[ "${WANT_SHELL}" -eq 1 ]]; then
    exec docker run "${DOCKER_ARGS[@]}" "${BUILDER_TAG}" bash
fi

# mkosi takes its options BEFORE the verb and silently discards any that follow
# it — `mkosi build --force` skips the rebuild, prints "Use --force to rebuild"
# and still exits 0. So the verb is always appended last, and a verb passed in
# via `--` is rejected rather than positioned wrong: passing one would push the
# real options after it and no-op them exactly the same silent way.
VERB="${MKOSI_VERB:-build}"
for arg in "${MKOSI_ARGS[@]+"${MKOSI_ARGS[@]}"}"; do
    case "${arg}" in
        build|clean|shell|boot|vm|qemu|sandbox|serve|burn|dependencies)
            echo "mkosi-build: pass the verb as MKOSI_VERB=${arg}, not after --" >&2
            echo "mkosi-build: (mkosi ignores options that follow a verb, silently)" >&2
            exit 2
            ;;
    esac
done

CMD=(mkosi --output-dir /work/output --workspace-dir /work/output/.mkosi-workspace)
[[ -n "${PROFILE}" ]] && CMD+=(--profile "${PROFILE}")
CMD+=("${MKOSI_ARGS[@]+"${MKOSI_ARGS[@]}"}" "${VERB}")

echo "[mkosi-build] ${CMD[*]}"
docker run "${DOCKER_ARGS[@]}" "${BUILDER_TAG}" "${CMD[@]}"

echo "[mkosi-build] artefacts in ${OUTPUT_DIR}:"
ls -la "${OUTPUT_DIR}"
