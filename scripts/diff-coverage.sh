#!/usr/bin/env bash
set -euo pipefail

# Diff-based coverage checker for Go projects.
# Ensures new/changed lines meet a minimum test coverage threshold.
#
# Usage: scripts/diff-coverage.sh <coverprofile> [--base <ref>] [--threshold <pct>]
#
# Base ref auto-detection:
#   main branch   → HEAD~1
#   dev branch    → origin/main
#   other branch  → origin/dev

PROFILE=""
BASE_REF=""
THRESHOLD=60
QUIET="${QUIET:-}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --base)      BASE_REF="$2"; shift 2 ;;
        --threshold) THRESHOLD="$2"; shift 2 ;;
        -*)          echo "Unknown option: $1" >&2; exit 1 ;;
        *)           PROFILE="$1"; shift ;;
    esac
done

if [[ -z "$PROFILE" ]]; then
    echo "Usage: scripts/diff-coverage.sh <coverprofile> [--base <ref>] [--threshold <pct>]" >&2
    exit 1
fi

if [[ ! -f "$PROFILE" ]]; then
    echo "Error: coverprofile not found: $PROFILE" >&2
    exit 1
fi

if [[ ! -s "$PROFILE" ]]; then
    echo "Error: coverprofile is empty: $PROFILE" >&2
    exit 1
fi

# Auto-detect base ref from branch name
if [[ -z "$BASE_REF" ]]; then
    BRANCH="${GITHUB_REF_NAME:-$(git rev-parse --abbrev-ref HEAD)}"
    case "$BRANCH" in
        main) BASE_REF="HEAD~1" ;;
        dev)  BASE_REF="origin/main" ;;
        *)    BASE_REF="origin/dev" ;;
    esac
    [[ -z "$QUIET" ]] && echo "Base ref: $BASE_REF (branch: $BRANCH)"
fi

# Verify base ref exists
if ! git rev-parse --verify "$BASE_REF" &>/dev/null; then
    echo "Error: base ref '$BASE_REF' not reachable." >&2
    echo "Fetch it first: git fetch origin <branch>" >&2
    exit 1
fi

# Get Go module path to strip from coverprofile paths
MODULE_PATH=$(awk '/^module / {print $2; exit}' go.mod)/

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

# --- Step 1: Extract added line numbers from diff ---
# Only non-test .go files that were added or modified.
# -M enables rename detection so file splits/moves only count truly changed lines.
# -C enables copy detection for the same reason (file split = partial copy).
git diff "${BASE_REF}" HEAD --unified=0 -M -C --diff-filter=AM -- '*.go' ':!*_test.go' ':!spinifex/migrate/*' | awk '
    /^\+\+\+ / {
        file = substr($2, 3)
        next
    }
    /^@@ / {
        plus = $3
        sub(/^\+/, "", plus)
        split(plus, nums, ",")
        start = nums[1] + 0
        count = (nums[2] != "") ? nums[2] + 0 : 1
        if (count == 0) next
        for (i = 0; i < count; i++) {
            print file ":" (start + i)
        }
    }
' > "$TMPDIR/diff_lines"

DIFF_COUNT=$(wc -l < "$TMPDIR/diff_lines" | tr -d ' ')
if [[ "$DIFF_COUNT" -eq 0 ]]; then
    echo "No new/modified Go source lines — skipping diff coverage."
    exit 0
fi

MIN_LOC=100
if [[ "$DIFF_COUNT" -lt "$MIN_LOC" ]]; then
    echo "Only $DIFF_COUNT changed lines (< $MIN_LOC) — skipping diff coverage."
    exit 0
fi

# --- Step 2: Build coverage map from coverprofile ---
# Expand line ranges into file:line → covered (0 or 1)
awk -v module="$MODULE_PATH" '
    NR == 1 { next }
    {
        split($1, parts, ":")
        file = parts[1]
        sub(module, "", file)

        split(parts[2], rng, ",")
        split(rng[1], s, ".")
        split(rng[2], e, ".")

        start = s[1] + 0
        end   = e[1] + 0
        hit   = ($3 + 0 > 0) ? 1 : 0

        for (l = start; l <= end; l++) {
            key = file ":" l
            if (!(key in cov) || hit) cov[key] = hit
        }
    }
    END { for (k in cov) print k, cov[k] }
' "$PROFILE" > "$TMPDIR/cover_map"

# --- Step 3: Cross-reference diff lines with coverage map ---
# Lines in the coverage map are instrumentable code.
# Lines NOT in the map are non-instrumentable (comments, blank, declarations) — skipped.
awk -v threshold="$THRESHOLD" '
    NR == FNR {
        cov[$1] = $2
        next
    }
    {
        if ($1 in cov) {
            total++
            if (cov[$1]) {
                covered++
            }
        } else {
            skipped++
        }
    }
    END {
        if (total == 0) {
            printf "No instrumentable lines in diff (%d non-instrumentable skipped) — skipping.\n", skipped+0
            exit 0
        }

        min_instrumentable = 5
        if (total < min_instrumentable) {
            printf "Only %d instrumentable lines (< %d minimum) — skipping diff coverage.\n", total, min_instrumentable
            exit 0
        }

        uncovered = total - covered
        pct = (covered / total) * 100

        quiet = ENVIRON["QUIET"]

        if (!quiet) {
            printf "\n=== Diff Coverage ===\n"
            printf "Instrumentable lines:   %d\n", total
            printf "Covered:                %d\n", covered+0
            printf "Uncovered:              %d\n", uncovered
            printf "Non-instrumentable:     %d (skipped)\n", skipped+0
            printf "Diff coverage:          %.1f%%\n", pct
            printf "Threshold:              %d%%\n", threshold
        }

        if (pct + 0.05 < threshold) {
            printf "\nFAIL: diff coverage %.1f%% is below %d%% threshold\n", pct, threshold
            exit 1
        }
        printf "\nPASS: diff coverage %.1f%% meets %d%% threshold\n", pct, threshold
    }
' "$TMPDIR/cover_map" "$TMPDIR/diff_lines"
