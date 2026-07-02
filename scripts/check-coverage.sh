#!/usr/bin/env bash
set -euo pipefail

# Check total coverage meets minimum threshold.
# Usage: scripts/check-coverage.sh <coverprofile> [quiet]

PROFILE="${1:?Usage: check-coverage.sh <coverprofile> [quiet]}"
MIN=70.0
QUIET="${2:-}"

if [[ ! -s "$PROFILE" ]]; then
    echo "ERROR: No coverage data generated — tests may have failed to compile"
    exit 1
fi

# Filter out packages excluded from coverage requirements (e.g. migrations)
FILTERED=$(mktemp)
trap 'rm -f "$FILTERED"' EXIT
awk 'NR==1 || !/\/migrate\//' "$PROFILE" > "$FILTERED"

TOTAL=$(go tool cover -func="$FILTERED" | tail -1 | awk '{print $NF}' | tr -d '%')

if [[ -z "$TOTAL" ]]; then
    echo "ERROR: No coverage data generated — tests may have failed to compile"
    exit 1
fi

if [[ -z "$QUIET" ]]; then
    echo ""
    echo "=== Total Coverage ==="
    go tool cover -func="$PROFILE" | tail -1
fi

PASS=$(awk "BEGIN {print ($TOTAL >= $MIN) ? 1 : 0}")
if [[ "$PASS" != "1" ]]; then
    echo "ERROR: Total coverage ${TOTAL}% is below minimum ${MIN}%"
    exit 1
fi

[[ -z "$QUIET" ]] && echo "Coverage ${TOTAL}% meets minimum ${MIN}% threshold"
exit 0
