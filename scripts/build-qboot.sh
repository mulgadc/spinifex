#!/bin/bash
# build-qboot.sh — build qboot, the firmware the QEMU microvm machine boots
# system instances (ALB/NLB) with, from a pinned upstream commit.
#
# Outputs:
#   $REPO_ROOT/build/microvm/qboot.rom          firmware, shipped beside the microvm kernel
#   $REPO_ROOT/build/microvm/qboot.LICENSE      qboot's GPL-2.0 licence text
#   $REPO_ROOT/build/qboot/qboot-<commit>.tar.gz the matching source, published with releases
#
# The build runs in a digest-pinned Debian container, so its output is
# reproducible and is checked against a recorded hash. That hash is also the
# sha256 of Debian 13's /usr/share/qemu/qboot.rom, which is built from the same commit.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT_DIR="$REPO_ROOT/build/microvm"
SRC_DIR="$REPO_ROOT/build/qboot"

QBOOT_REPO="${QBOOT_REPO:-https://gitlab.com/qemu-project/qboot.git}"
QBOOT_COMMIT="8ca302e86d685fa05b16e2b208888243da319941"
QBOOT_SHA256="5c9c4b12c4438e3cf11d087263afe6519c8c05dd5896727b8b169b8f5dbf9877"
BUILD_IMAGE="debian:trixie@sha256:4c25275446ca371766b8b093c696782eabce06263fccacb8d5d813b3dc44b308"

log() { echo "[build-qboot] $*"; }
fail() { echo "ERROR: $*" >&2; exit 1; }

CONTAINER_TOOL=""
for tool in docker podman; do
    if command -v "$tool" >/dev/null 2>&1 && "$tool" info >/dev/null 2>&1; then
        CONTAINER_TOOL=$tool
        break
    fi
done
[ -n "$CONTAINER_TOOL" ] || fail "qboot builds in a container; install docker or podman"
command -v git >/dev/null 2>&1 || fail "required tool not found: git"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

log "fetching qboot $QBOOT_COMMIT from $QBOOT_REPO"
git init -q "$WORK/src"
git -C "$WORK/src" fetch -q --depth 1 "$QBOOT_REPO" "$QBOOT_COMMIT"
git -C "$WORK/src" checkout -q FETCH_HEAD
got_commit=$(git -C "$WORK/src" rev-parse HEAD)
[ "$got_commit" = "$QBOOT_COMMIT" ] || fail "fetched qboot $got_commit, not the pinned $QBOOT_COMMIT"

mkdir -p "$SRC_DIR"
src_tarball="$SRC_DIR/qboot-${QBOOT_COMMIT}.tar.gz"
git -C "$WORK/src" archive --format=tar.gz --prefix="qboot-${QBOOT_COMMIT}/" -o "$src_tarball" "$QBOOT_COMMIT"

log "building in $BUILD_IMAGE with $CONTAINER_TOOL"
# The firmware comes back on stdout, so nothing the container writes is left
# root-owned on the host.
"$CONTAINER_TOOL" run --rm --platform linux/amd64 -v "$WORK/src:/src:ro" \
    -e DEBIAN_FRONTEND=noninteractive "$BUILD_IMAGE" sh -euc '
        {
            apt-get update -qq
            apt-get install -y -qq --no-install-recommends gcc libc6-dev meson ninja-build binutils
            meson setup /tmp/build /src
            ninja -C /tmp/build
        } >/dev/null
        cat /tmp/build/bios.bin
    ' > "$WORK/qboot.rom"

got_sha=$(sha256sum "$WORK/qboot.rom" | cut -d' ' -f1)
if [ "$got_sha" != "$QBOOT_SHA256" ]; then
    fail "qboot.rom has sha256 $got_sha, not the recorded $QBOOT_SHA256.
       The pinned commit or build image changed; rebuild, boot an ALB on it, then update QBOOT_SHA256."
fi

mkdir -p "$OUT_DIR"
install -m 0644 "$WORK/qboot.rom" "$OUT_DIR/qboot.rom"
install -m 0644 "$WORK/src/LICENSE" "$OUT_DIR/qboot.LICENSE"
log "qboot.rom sha256 $got_sha"
log "source: $src_tarball"
