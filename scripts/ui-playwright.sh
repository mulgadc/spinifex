#!/usr/bin/env bash
# Runs the console's Playwright suite in the pinned image CI uses, so screenshots
# rasterise identically here and on the runner. `compare` runs it as CI does;
# `baselines` redraws e2e/__screenshots__/. The host's node_modules is not used.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ui_dir="${repo_root}/spinifex/runtime/roles/spinifexui/frontend"
cache="${UI_PLAYWRIGHT_CACHE:-${XDG_CACHE_HOME:-${HOME}/.cache}/spinifex-ui-playwright}"

die() { echo "ui-playwright: $*" >&2; exit 1; }

mode="${1:-}"
case "${mode}" in
    compare)   e2e_args=() ;;
    baselines) e2e_args=(--update-snapshots=all) ;;
    *) die "usage: $0 compare|baselines [playwright args...]" ;;
esac
shift
e2e_args+=("$@")

image="$(tr -d '[:space:]' < "${ui_dir}/playwright.image")"
[ -n "${image}" ] || die "${ui_dir}/playwright.image is empty"

runtime="${CONTAINER_RUNTIME:-}"
if [ -z "${runtime}" ]; then
    runtime="$(command -v podman || command -v docker || true)"
fi
[ -n "${runtime}" ] || die "neither podman nor docker is installed"

# Files the suite writes (dist, screenshots, test-results) must stay owned by
# the invoking user, which rootless podman and docker reach differently.
case "$(basename "${runtime}")" in
    podman) user_args=(--userns=keep-id) ;;
    *)      user_args=(--user "$(id -u):$(id -g)") ;;
esac

mkdir -p "${cache}/node_modules" "${cache}/pnpm-store" "${cache}/corepack"

# Runs in the container: check the image against .nvmrc and the lockfile's
# Playwright, then install, build dist for the CSP test, and run the suite.
# shellcheck disable=SC2016
inner='
set -eu
want_node="v$(cat .nvmrc)"
[ "$(node --version)" = "${want_node}" ] ||
    { echo "ui-playwright: image has node $(node --version), .nvmrc wants ${want_node}" >&2; exit 1; }
corepack pnpm install --frozen-lockfile --store-dir /cache/pnpm-store
pw="$(corepack pnpm exec playwright --version | awk "{print \$2}")"
case "${UI_PLAYWRIGHT_IMAGE}" in
    *":v${pw}-"*) ;;
    *) echo "ui-playwright: lockfile has Playwright ${pw}, image is ${UI_PLAYWRIGHT_IMAGE}" >&2; exit 1 ;;
esac
corepack pnpm build
exec corepack pnpm e2e "$@"
'

exec "${runtime}" run --rm --init --ipc=host "${user_args[@]}" \
    -e CI=1 \
    -e HOME=/tmp/home \
    -e COREPACK_HOME=/cache/corepack \
    -e COREPACK_ENABLE_DOWNLOAD_PROMPT=0 \
    -e UI_PLAYWRIGHT_IMAGE="${image}" \
    -v "${ui_dir}:/work" \
    -v "${cache}/node_modules:/work/node_modules" \
    -v "${cache}:/cache" \
    -w /work \
    "${image}" bash -c "${inner}" ui-playwright "${e2e_args[@]}"
