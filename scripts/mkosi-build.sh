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
#   scripts/mkosi-build.sh --image <name> [--shell] [-- <mkosi options>]
#   scripts/mkosi-build.sh --profile <name> [--profile <name>...] [--shell] [-- <mkosi options>]
#
#   scripts/mkosi-build.sh --image spinifex-eks-node-gpu
#   MKOSI_VERB=clean scripts/mkosi-build.sh
#
# --image is the interface to prefer: it names an output and expands to the
# right ordered profile list. --profile is the escape hatch for ad-hoc builds
# and puts the ordering rule below on the caller.
#
# --image also names the artefact: the build writes <image>.raw, so what a
# composition produced is legible from the output rather than inferred from
# which build ran last. Profile-mode builds keep the base config's ImageId.
#
# Builds are always forced; see the --force note further down for why mkosi's
# own skip logic is not a cache worth having here.
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

# Named image compositions. Profiles are composition units, not outputs, so an
# image is an ordered list of them.
#
# The ORDER IS LOAD-BEARING and is why this table exists rather than callers
# passing profiles by hand: mkosi runs each profile's postinst in the order the
# profiles are given (verified — it is argument order, not alphabetical), so a
# profile whose postinst uses a tool another profile installs must come after
# it. docker's `nvidia-ctk runtime configure` needs gpu-nvidia's toolkit, and
# getting that backwards is the kind of mistake that ships a working-looking
# image whose GPU wiring is simply absent.
image_profiles() {
    case "$1" in
        ubuntu-gpu-nvidia)     echo "gpu-nvidia docker" ;;
        spinifex-eks-node-gpu) echo "gpu-nvidia eks-common eks-agent" ;;
        spinifex-ecs-node-gpu) echo "gpu-nvidia ecs" ;;
        *)                     return 1 ;;
    esac
}

# Go binaries a profile ships, as "<go package>:<path in image>".
#
# These are built on the host rather than in the builder container, which
# carries no Go on purpose: go.mod is the single pin for the toolchain version
# (CI honours it via setup-go's go-version-file), and a second pin in the
# Dockerfile could disagree with it silently. GOWORK=off because the sub-repo
# builds standalone and the workspace lives in the parent monorepo, which is
# not present in every checkout.
profile_binaries() {
    case "$1" in
        eks-agent) echo "./cmd/ecr-credential-provider:usr/local/bin/ecr-credential-provider" ;;
        ecs)       echo "./cmd/ecs-agent:usr/local/bin/ecs-agent" ;;
        *)         echo "" ;;
    esac
}

# Compile a profile's binaries into images/staging/<profile>/, which the
# profile picks up via ExtraTrees=. Staging is rebuilt from scratch each run:
# a stale binary from a previous build silently shipping is the exact failure
# this whole containerised, pinned path exists to avoid.
stage_profile_binaries() {
    local profile="$1" spec pkg dest staging
    spec="$(profile_binaries "${profile}")"
    [[ -z "${spec}" ]] && return 0

    staging="${IMAGE_DIR}/staging/${profile}"
    rm -rf "${staging}"

    command -v go >/dev/null || {
        echo "mkosi-build: go not found — profile ${profile} ships Go binaries" >&2
        exit 1
    }

    for entry in ${spec}; do
        pkg="${entry%%:*}"
        dest="${entry#*:}"
        mkdir -p "${staging}/$(dirname "${dest}")"
        echo "[mkosi-build] building ${pkg} -> staging/${profile}/${dest}"
        # Flags match the pre-mkosi manifests: a static, FIPS-pinned binary.
        ( cd "${MKOSI_REPO_ROOT}" && CGO_ENABLED=0 GOFIPS140=v1.0.0 GOWORK=off \
            go build -ldflags "-s -w" -o "${staging}/${dest}" "${pkg}" )
        chmod 0755 "${staging}/${dest}"
    done
}

IMAGE=""
PROFILES=()
WANT_SHELL=0
MKOSI_ARGS=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --image)   IMAGE="$2"; shift 2 ;;
        --profile) PROFILES+=("$2"); shift 2 ;;
        --shell)   WANT_SHELL=1; shift ;;
        --)        shift; MKOSI_ARGS=("$@"); break ;;
        *)         echo "mkosi-build: unknown arg: $1" >&2; exit 2 ;;
    esac
done

if [[ -n "${IMAGE}" ]]; then
    if [[ "${#PROFILES[@]}" -gt 0 ]]; then
        echo "mkosi-build: pass --image or --profile, not both" >&2
        exit 2
    fi
    if ! expanded="$(image_profiles "${IMAGE}")"; then
        echo "mkosi-build: unknown image: ${IMAGE}" >&2
        echo "mkosi-build: known images: ubuntu-gpu-nvidia spinifex-eks-node-gpu spinifex-ecs-node-gpu" >&2
        exit 2
    fi
    read -r -a PROFILES <<< "${expanded}"
    echo "[mkosi-build] image ${IMAGE} = ${PROFILES[*]}"
fi

require_docker

if [[ ! -d "${IMAGE_DIR}" ]]; then
    echo "mkosi-build: no image dir at ${IMAGE_DIR} (set MKOSI_IMAGE_DIR)" >&2
    exit 1
fi

build_builder_image

# Before the container starts: the repo is mounted read-only, so anything a
# profile needs built has to exist by now.
for p in "${PROFILES[@]+"${PROFILES[@]}"}"; do
    stage_profile_binaries "${p}"
done

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
    --volume "${OUTPUT_DIR}:/work/output"
)

# The whole repo is mounted, not just images/, because profiles pull build
# inputs from outside the image tree via ExtraTrees= — the eks helpers live in
# scripts/images/eks-node/ and are shared with the Alpine build that still
# consumes them. Referencing them where they are keeps one copy: duplicating
# them under mkosi.extra/ would reintroduce exactly the drift Stage A deleted a
# hand-copied helper to remove.
#
# Read-only: every build input is either checked in or staged before the
# container starts, and the only thing that legitimately gets written is the
# output directory, mounted separately above. A build that tries to mutate the
# repo is a bug, so let it fail here rather than silently succeed.
#
# An image dir outside the repo (ad-hoc/testing) keeps the old standalone
# mount and simply has no repo to reference.
if [[ "${IMAGE_DIR}" == "${MKOSI_REPO_ROOT}"/* ]]; then
    DOCKER_ARGS+=(--volume "${MKOSI_REPO_ROOT}:/work/repo:ro")
    CONTAINER_IMAGE_DIR="/work/repo/${IMAGE_DIR#"${MKOSI_REPO_ROOT}"/}"
else
    DOCKER_ARGS+=(--volume "${IMAGE_DIR}:/work/images:ro")
    CONTAINER_IMAGE_DIR="/work/images"
fi
DOCKER_ARGS+=(--workdir "${CONTAINER_IMAGE_DIR}")
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
for p in "${PROFILES[@]+"${PROFILES[@]}"}"; do
    CMD+=(--profile "${p}")
done

# What the caller already asked for, so neither default below overrides an
# explicit choice.
CALLER_SET_IMAGE_ID=0
CALLER_SET_FORCE=0
for arg in "${MKOSI_ARGS[@]+"${MKOSI_ARGS[@]}"}"; do
    case "${arg}" in
        --image-id|--image-id=*) CALLER_SET_IMAGE_ID=1 ;;
        --force|-f)              CALLER_SET_FORCE=1 ;;
    esac
done

# Name the output after the image, not after the base config's ImageId. Every
# composition otherwise lands on the same spinifex.raw, so the artefact carries
# no record of which profiles produced it and the only thing distinguishing an
# EKS image from an ECS one is which build ran last. Publishing then picks a
# name by hand, which is how the wrong image gets shipped under the right one.
if [[ -n "${IMAGE}" && "${CALLER_SET_IMAGE_ID}" -eq 0 ]]; then
    CMD+=(--image-id "${IMAGE}")
fi

# Always rebuild. mkosi skips the build whenever the output path merely exists
# — it does not compare inputs — so its "cache" never notices an edited
# mkosi.conf or a restaged binary, and it exits 0 while doing nothing. That
# makes a stale image indistinguishable from a fresh one, and the next step
# imports and boot-tests last week's build. The cache worth keeping is the
# package download volume, which --force does not touch.
if [[ "${CALLER_SET_FORCE}" -eq 0 ]]; then
    CMD+=(--force)
fi

CMD+=("${MKOSI_ARGS[@]+"${MKOSI_ARGS[@]}"}" "${VERB}")

BUILD_STARTED_AT="$(date +%s)"

echo "[mkosi-build] ${CMD[*]}"
docker run "${DOCKER_ARGS[@]}" "${BUILDER_TAG}" "${CMD[@]}"

# Belt and braces on the above: prove this run actually wrote the image rather
# than trusting the exit status, which mkosi reports as 0 for a build it
# declined to do. Only checked when the output name is ours to predict.
if [[ "${VERB}" == "build" && "${CALLER_SET_IMAGE_ID}" -eq 0 ]]; then
    EXPECTED_RAW="${OUTPUT_DIR}/${IMAGE:-spinifex}.raw"
    if [[ ! -f "${EXPECTED_RAW}" ]]; then
        echo "mkosi-build: build reported success but ${EXPECTED_RAW} does not exist" >&2
        exit 1
    fi
    if [[ "$(stat -c %Y "${EXPECTED_RAW}")" -lt "${BUILD_STARTED_AT}" ]]; then
        echo "mkosi-build: ${EXPECTED_RAW} predates this run — the build was skipped, not performed" >&2
        exit 1
    fi
    echo "[mkosi-build] built ${EXPECTED_RAW}"
fi

echo "[mkosi-build] artefacts in ${OUTPUT_DIR}:"
ls -la "${OUTPUT_DIR}"
