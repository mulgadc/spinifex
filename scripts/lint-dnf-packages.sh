#!/usr/bin/env bash
# Runs setup.sh's Oracle Linux repository setup in a container, then resolves
# its runtime dnf list against the real repositories with dnf install --assumeno.
# Catches a renamed or dropped package, a SIG dependency the includepkgs
# allowlist misses, a changed SIG key, and QEMU or OVS resolving from the wrong
# repository.
set -euo pipefail

IMAGE="${1:-oraclelinux:10}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "Resolving ${IMAGE}..."
# The repository setup and the list both come from setup.sh itself, so the lint
# cannot drift from what the installer does.
docker run --rm -v "${SCRIPT_DIR}/setup.sh:/setup.sh:ro" "${IMAGE}" bash -c '
    set -euo pipefail
    INSTALL_SPINIFEX_LIB_ONLY=1 . /setup.sh
    SUDO=""
    ensure_ol_repos

    # --assumeno always exits 1, so the transaction is judged from its output.
    # shellcheck disable=SC2086
    out=$(dnf install --assumeno $OL_RUNTIME_PACKAGES 2>&1) || true
    if ! grep -q "^Operation aborted" <<< "$out" || grep -qE "^(Error|Problem)|No match for argument" <<< "$out"; then
        echo "$out"
        echo "FAIL: the Oracle Linux package list does not resolve" >&2
        exit 1
    fi

    # repo_of <package> — the repository dnf would install the package from.
    repo_of() { awk -v p="$1" "\$1 == p { print \$4; exit }" <<< "$out"; }
    for want in "qemu-kvm $OL_KVM_UTILS_REPO" "qemu-img $OL_KVM_UTILS_REPO" \
        "openvswitch3.5 $NFV_SIG_REPO_ID" "ovn25.03-central $NFV_SIG_REPO_ID"; do
        pkg=${want% *} repo=${want#* }
        got=$(repo_of "$pkg")
        if [ "$got" != "$repo" ]; then
            echo "$out"
            echo "FAIL: $pkg resolves from ${got:-nowhere}, not $repo" >&2
            exit 1
        fi
    done
'

echo "OK: every package resolves on ${IMAGE}, QEMU from KVM Utilities and OVS/OVN from the NFV SIG"
