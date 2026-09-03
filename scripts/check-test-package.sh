#!/usr/bin/env bash
set -euo pipefail

# Requires newly added test files to use an external test package.
#
# `package foo_test` binds tests to the exported API, so internals can be
# refactored without rewriting tests, and it lets packages share test doubles
# (an in-package double is invisible to every other package). Coverage is
# unaffected — Go compiles internal and external test packages into a single
# binary per package, so attribution is identical either way.
#
# Only ADDED files are checked; existing in-package tests are left alone.
# A file that genuinely needs unexported access can either opt out with a
# `//go:build` free marker comment (see ALLOW_MARKER) or move the unexported
# hook into an export_test.go.
#
# Usage: scripts/check-test-package.sh [--base <ref>]
#
# Base ref auto-detection matches diff-coverage.sh:
#   main branch   → HEAD~1
#   dev branch    → origin/main
#   other branch  → origin/dev

BASE_REF=""
QUIET="${QUIET:-}"
ALLOW_MARKER="//test:in-package"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --base) BASE_REF="$2"; shift 2 ;;
        -*)     echo "Unknown option: $1" >&2; exit 1 ;;
        *)      echo "Unexpected argument: $1" >&2; exit 1 ;;
    esac
done

if [[ -z "$BASE_REF" ]]; then
    BRANCH="${GITHUB_REF_NAME:-$(git rev-parse --abbrev-ref HEAD)}"
    case "$BRANCH" in
        main) BASE_REF="HEAD~1" ;;
        dev)  BASE_REF="origin/main" ;;
        *)    BASE_REF="origin/dev" ;;
    esac
    [[ -z "$QUIET" ]] && echo "Base ref: $BASE_REF (branch: $BRANCH)"
fi

if ! git rev-parse --verify "$BASE_REF" &>/dev/null; then
    echo "Error: base ref '$BASE_REF' not reachable." >&2
    echo "Fetch it first: git fetch origin <branch>" >&2
    exit 1
fi

# e2e and integration suites share a package-level fixture built in
# main_test.go, so their files cannot move individually. export_test.go is the
# remedy this check recommends, and it is in-package by definition.
ADDED=$(git diff "$BASE_REF" HEAD --name-only --diff-filter=A -- '*_test.go' \
    ':!tests/e2e/*' ':!tests/integration/*' ':!*export_test.go' || true)

if [[ -z "$ADDED" ]]; then
    [[ -z "$QUIET" ]] && echo "No new test files — skipping test package check."
    exit 0
fi

VIOLATIONS=()
for f in $ADDED; do
    [[ -f "$f" ]] || continue
    if grep -qF "$ALLOW_MARKER" "$f"; then
        continue
    fi
    pkg=$(awk '/^package /{print $2; exit}' "$f")
    if [[ -n "$pkg" && "$pkg" != *_test ]]; then
        VIOLATIONS+=("$f (package $pkg)")
    fi
done

if [[ ${#VIOLATIONS[@]} -gt 0 ]]; then
    echo "ERROR: new test files must use an external test package (package <pkg>_test):" >&2
    for v in "${VIOLATIONS[@]}"; do
        echo "  $v" >&2
    done
    echo "" >&2
    echo "Move unexported hooks into export_test.go, or add the comment" >&2
    echo "'$ALLOW_MARKER' to the file with a reason if in-package access is required." >&2
    exit 1
fi

[[ -z "$QUIET" ]] && echo "Test package check OK ($(echo "$ADDED" | wc -w) new file(s))"
exit 0
