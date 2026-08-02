#!/usr/bin/env bash
# Resolves setup.sh's runtime apt list against a suite's real indices with
# apt-get install -s. Catches a package that has been renamed, dropped, or made
# ambiguously virtual before it reaches a user's machine.
set -euo pipefail

IMAGE="${1:?usage: lint-apt-packages.sh <docker image>, e.g. ubuntu:26.04}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Source setup.sh for the list rather than restating it here, so the lint cannot
# drift from what the installer actually asks apt for.
INSTALL_SPINIFEX_LIB_ONLY=1
export INSTALL_SPINIFEX_LIB_ONLY
# shellcheck source=/dev/null
. "${SCRIPT_DIR}/setup.sh"

if [ -z "${APT_RUNTIME_PACKAGES:-}" ]; then
    echo "setup.sh did not define APT_RUNTIME_PACKAGES" >&2
    exit 1
fi

# Both arch-specific qemu packages exist in the amd64 archive, so linting them
# together covers the aarch64 install path from an amd64 runner.
PACKAGES="qemu-system-x86 qemu-system-arm ${APT_RUNTIME_PACKAGES}"

echo "Resolving ${IMAGE}..."
# shellcheck disable=SC2086
docker run --rm "${IMAGE}" bash -c \
    "apt-get update -qq >/dev/null && apt-get install -s -y -qq ${PACKAGES//$'\n'/ } >/dev/null"

echo "OK: every package resolves on ${IMAGE}"
