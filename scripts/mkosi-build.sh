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
        ubuntu-gpu-amd)        echo "gpu-amd docker" ;;
        # docker is deliberately absent: the serving VM runs vLLM/TEI as
        # systemd services and never touches a Docker daemon. vllm comes
        # second because it stacks onto the GPU driver gpu-nvidia installs.
        # The profile now bakes both co-served-bundle engines (vLLM for TEXT,
        # TEI for EMBEDDING/RERANK); TEI's own binary and CUDA runtime
        # libraries are staged from HuggingFace's published CUDA image by
        # stage_tei_binary() below, not built in this chroot.
        ubuntu-vllm-serving)   echo "gpu-nvidia vllm" ;;
        spinifex-eks-node-gpu) echo "gpu-nvidia eks-common eks-agent" ;;
        spinifex-ecs-node-gpu) echo "gpu-nvidia ecs" ;;
        # The CPU variant is the GPU one minus gpu-nvidia. ecs pulls nothing
        # from that profile: it adds no repositories, needs no network at build
        # time, and every package it installs is in main/universe, which the
        # base already enables.
        spinifex-ecs-node)     echo "ecs" ;;
        # One image for both EKS roles: eks-server and eks-agent are both
        # present and the first-boot selector starts whichever the launch asked
        # for. eks-common comes first because it installs the k3s binary the
        # other two build on.
        spinifex-eks-node)     echo "eks-common eks-server eks-agent" ;;
        # rds-common first: each engine profile's postinst asserts against the
        # /etc/spinifex-rds it creates and the agent it stages.
        spinifex-rds-postgres) echo "rds-common rds-postgres" ;;
        spinifex-rds-mariadb)  echo "rds-common rds-mariadb" ;;
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
        eks-agent)
            echo "./cmd/ecr-credential-provider:usr/local/bin/ecr-credential-provider"
            ;;
        eks-server)
            # konnectivity-server is NOT here — it is not an in-repo ./cmd/...
            # package, so it gets its own clone-and-build step in
            # stage_profile_binaries() below.
            echo "./cmd/eks-token-webhook:usr/local/bin/eks-token-webhook \
./cmd/eks-gateway-publish:usr/local/bin/eks-gateway-publish \
./cmd/eks-gateway-fetch:usr/local/bin/eks-gateway-fetch \
./cmd/eks-webhook-cert:usr/local/bin/eks-webhook-cert \
./cmd/eks-konnectivity-cert:usr/local/bin/eks-konnectivity-cert"
            ;;
        ecs) echo "./cmd/ecs-agent:usr/local/bin/ecs-agent" ;;
        rds-common) echo "./cmd/rds-agent:usr/local/bin/rds-agent" ;;
        *)   echo "" ;;
    esac
}

# apiserver-network-proxy version powering konnectivity-server. Pinned as
# a named constant, not resolved at build time: its GitHub releases ship only
# container images (no downloadable binary), and its own go.mod carries local
# replace directives, which `go install pkg@version` cannot resolve for a
# module outside its own checkout — a plain clone-and-build is the only route.
readonly KONNECTIVITY_SERVER_VERSION="v0.30.3"

# Clone and build konnectivity-server (apiserver-network-proxy's ./cmd/server)
# into the given staging dir, on the host, alongside stage_profile_binaries()'s
# in-repo binaries below. Kept as its own function because it is not an
# in-repo ./cmd/... package, so profile_binaries() cannot express it.
stage_konnectivity_server() {
    local staging="$1" clone_dir

    command -v git >/dev/null || {
        echo "mkosi-build: git not found — eks-server needs it to build konnectivity-server" >&2
        exit 1
    }
    command -v go >/dev/null || {
        echo "mkosi-build: go not found — eks-server needs it to build konnectivity-server" >&2
        exit 1
    }

    clone_dir="$(mktemp -d)"
    echo "[mkosi-build] cloning apiserver-network-proxy ${KONNECTIVITY_SERVER_VERSION}"
    git clone --quiet --depth 1 --branch "${KONNECTIVITY_SERVER_VERSION}" \
        https://github.com/kubernetes-sigs/apiserver-network-proxy "${clone_dir}"

    mkdir -p "${staging}/usr/local/bin"
    echo "[mkosi-build] building konnectivity-server -> staging/eks-server/usr/local/bin/konnectivity-server"
    # Same flags as the in-repo binaries below: a static, FIPS-pinned, stripped
    # binary. Built from within its own clone, so no GOWORK=off is needed —
    # nothing in a bare /tmp checkout can discover the monorepo's go.work.
    ( cd "${clone_dir}" && CGO_ENABLED=0 GOFIPS140=v1.0.0 \
        go build -ldflags "-s -w" -o "${staging}/usr/local/bin/konnectivity-server" ./cmd/server )
    chmod 0755 "${staging}/usr/local/bin/konnectivity-server"

    rm -rf "${clone_dir}"
}

# Compile a profile's binaries into images/staging/<profile>/, which the
# profile picks up via ExtraTrees=. Staging is rebuilt from scratch each run:
# a stale binary from a previous build silently shipping is the exact failure
# this whole containerised, pinned path exists to avoid.
stage_profile_binaries() {
    local profile="$1" spec pkg dest staging
    spec="$(profile_binaries "${profile}")"
    # eks-server always has in-repo binaries too (profile_binaries() above), so
    # this never leaves konnectivity-server staged alone with spec empty and
    # the rm -rf below wiping it out.
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

    if [[ "${profile}" == "eks-server" ]]; then
        stage_konnectivity_server "${staging}"
    fi
}

# TEI CUDA image tag the vllm profile extracts its embedding/rerank engine
# binary and CUDA runtime libraries from. Ampere-86 matches the RTX A1000
# (GA107, compute capability 8.6) this AMI runs on today; bumping this is a
# deliberate edit, done alongside checking TEI's own release notes for
# whatever GPU this image targets next.
readonly TEI_IMAGE="ghcr.io/huggingface/text-embeddings-inference:86-1.9"

# Extract text-embeddings-router and the CUDA runtime shared libraries its
# cuda feature links against (nvidia/cuda:*-runtime base under the hood, not
# just the driver) from HuggingFace's own published CUDA image, on the host.
# No cargo/nvcc build in the mkosi chroot: compiling TEI's candle-cuda
# kernels from source needs the CUDA toolkit as a build dependency, which is
# exactly what this image otherwise deliberately does not carry (see the
# vllm profile's own FlashInfer-disable note for why). `docker create` +
# `docker cp` only, never `docker run` — no GPU, no privileged flags, the
# same "extract a prebuilt artifact, stage it, ship the static result"
# pattern stage_konnectivity_server above already uses for a non-Go binary.
stage_tei_binary() {
    local profile="$1" staging container_id
    [[ "${profile}" == "vllm" ]] || return 0

    staging="${IMAGE_DIR}/staging/vllm/opt/tei"
    rm -rf "${staging}"
    mkdir -p "${staging}/bin" "${staging}/cuda-lib64"

    echo "[mkosi-build] pulling ${TEI_IMAGE} to extract the TEI binary"
    docker pull "${TEI_IMAGE}"
    container_id="$(docker create "${TEI_IMAGE}")"

    echo "[mkosi-build] extracting text-embeddings-router -> staging/vllm/opt/tei/bin/"
    docker cp "${container_id}:/usr/local/bin/text-embeddings-router" "${staging}/bin/text-embeddings-router"
    echo "[mkosi-build] extracting CUDA runtime libraries -> staging/vllm/opt/tei/cuda-lib64/"
    docker cp "${container_id}:/usr/local/cuda/lib64/." "${staging}/cuda-lib64/"

    docker rm -f "${container_id}" >/dev/null 2>&1 || true

    [[ -s "${staging}/bin/text-embeddings-router" ]] || {
        echo "mkosi-build: text-embeddings-router extraction produced an empty file" >&2
        exit 1
    }
    chmod 0755 "${staging}/bin/text-embeddings-router"
    echo "[mkosi-build] staged text-embeddings-router + CUDA runtime libs from ${TEI_IMAGE}"
}

# Reject an unescaped $VAR/${VAR} in any Exec*= line of a profile's units.
#
# systemd expands those from the UNIT's environment before the command runs and
# substitutes an empty string for a name it does not know, so a unit that wraps
# a shell to source a runtime-written env file must write $$ to defer expansion
# to that shell. A single $ is not a syntax error anywhere — it silently drops
# the argument's value, and the service dies on flag parsing with no useful log.
# This bites exactly on the OpenRC->systemd port, where the same ${VAR} text was
# correct because the shell was the only thing reading it.
#
# Runs on the host before the container starts, alongside the binary staging
# above, so a bad unit fails the build in seconds instead of at first boot.
lint_unit_expansions() {
    local profile="$1" extra
    extra="${IMAGE_DIR}/mkosi.profiles/${profile}/mkosi.extra"
    [[ -d "${extra}" ]] || return 0

    # Line continuations are joined first: the offending reference is typically
    # mid-way through a wrapped multi-line ExecStart, not on the Exec*= line.
    local bad
    bad="$(find "${extra}" -type f \( -name '*.service' -o -name '*.socket' \
        -o -name '*.timer' -o -name '*.mount' \) -print0 |
        xargs -0 --no-run-if-empty awk '
            FNR == 1 { pending = "" }
            { line = pending $0 }
            line ~ /\\$/ { pending = substr(line, 1, length(line) - 1); next }
            { pending = "" }
            line !~ /^[[:space:]]*Exec(Start|Stop|Reload|Condition)/ { next }
            # Strip $$ first so an escaped reference cannot match as a bare one.
            { probe = line; gsub(/\$\$/, "", probe) }
            probe ~ /\$\{?[A-Za-z_]/ { print FILENAME ": " line }
        ')"

    if [[ -n "${bad}" ]]; then
        echo "mkosi-build: unescaped systemd variable reference in Exec* line(s):" >&2
        echo "${bad}" >&2
        echo "mkosi-build: systemd expands these to empty; write \$\$ to defer to the shell" >&2
        exit 1
    fi
}

IMAGE=""
PROFILES=()
WANT_SHELL=0
MKOSI_ARGS=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        # Query only: exits 0 if the name is a known composition, without
        # touching Docker. The Makefile uses it to decide whether an IMAGE=
        # with no manifest is an mkosi image or a typo.
        --known-image) image_profiles "$2" >/dev/null; exit $? ;;
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
        echo "mkosi-build: known images: ubuntu-gpu-nvidia ubuntu-gpu-amd ubuntu-vllm-serving spinifex-eks-node-gpu spinifex-ecs-node-gpu spinifex-ecs-node spinifex-eks-node spinifex-rds-postgres spinifex-rds-mariadb" >&2
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
    lint_unit_expansions "${p}"
    stage_profile_binaries "${p}"
    stage_tei_binary "${p}"
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

    # mkosi only emits the raw. A qcow2 next to it came from a previous run's
    # hand conversion, and nothing about its name says which build it belongs
    # to — so it survives a rebuild and silently ships the old image to a node,
    # while the raw beside it carries the new one. Same failure the freshness
    # check above exists to prevent, one file over.
    #
    # Refresh rather than delete: the qcow2 is what gets copied to a node and
    # imported, so removing it just pushes the manual conversion (and this
    # trap) back onto the caller. Conversion is a couple of seconds.
    EXPECTED_QCOW2="${OUTPUT_DIR}/${IMAGE:-spinifex}.qcow2"
    if [[ -f "${EXPECTED_QCOW2}" && "$(stat -c %Y "${EXPECTED_QCOW2}")" -lt "${BUILD_STARTED_AT}" ]]; then
        if command -v qemu-img >/dev/null 2>&1; then
            echo "[mkosi-build] refreshing stale ${EXPECTED_QCOW2}"
            # Convert to a temp name and rename, so an interrupted run leaves
            # the old qcow2 rather than a half-written one that looks current.
            qemu-img convert -f raw -O qcow2 "${EXPECTED_RAW}" "${EXPECTED_QCOW2}.tmp"
            mv "${EXPECTED_QCOW2}.tmp" "${EXPECTED_QCOW2}"
        else
            echo "[mkosi-build] removing stale ${EXPECTED_QCOW2} (no qemu-img to refresh it)" >&2
            rm -f "${EXPECTED_QCOW2}"
        fi
    fi
fi

echo "[mkosi-build] artefacts in ${OUTPUT_DIR}:"
ls -la "${OUTPUT_DIR}"
