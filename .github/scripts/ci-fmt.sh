# shellcheck shell=bash
# Source from a workflow `run:` block:
#   source "${GITHUB_WORKSPACE}/spinifex/.github/scripts/ci-fmt.sh"
#
# Provides colored banners + ::group::/::endgroup:: wrappers. ANSI escapes
# render in the GitHub Actions live log; the same lines also land in any
# artifact log (tee'd output) without breaking parsers because they only
# affect terminal display.
#
# Functions kept short and unindented on purpose — wide nested output wraps
# badly on narrow terminals.

if [[ -n "${GITHUB_ACTIONS:-}" ]] || [[ -t 1 ]]; then
  _R=$'\033[0m'
  _B=$'\033[1m'    # bold
  _D=$'\033[2m'    # dim
  _RED=$'\033[31m'
  _GRN=$'\033[32m'
  _YEL=$'\033[33m'
  _BLU=$'\033[34m'
  _CYA=$'\033[36m'
else
  _R="" _B="" _D="" _RED="" _GRN="" _YEL="" _BLU="" _CYA=""
fi

# Single-line rule using box-drawing chars — narrow enough that GitHub's
# timestamp prefix + this rule still fit on an 80-col terminal.
# banner LABEL — bold cyan title with surrounding rule. Used at the start
# of an inherently multi-line phase (test run, artifact pull) so the live
# log has an obvious visual marker.
banner() {
  printf '\n%s%s━━ %s ━━%s\n' "$_B" "$_CYA" "$*" "$_R"
}

# Status indicators. Single char prefix only — keeps width on narrow logs.
ok()   { printf '%s✓%s %s\n' "$_GRN" "$_R" "$*"; }
bad()  { printf '%s✗%s %s\n' "$_RED" "$_R" "$*"; }
warn() { printf '%s!%s %s\n' "$_YEL" "$_R" "$*"; }
note() { printf '%s·%s %s\n' "$_D"   "$_R" "$*"; }

# group_open LABEL / group_close — thin wrapper around GitHub's collapse
# markers. No extra body printed; banner() is the visible inside-group
# heading when callers want one.
group_open()  { echo "::group::$*"; }
group_close() { echo "::endgroup::"; }

# quiet_run LABEL LOG_FILE -- CMD ARGS …
# Run CMD redirected to LOG_FILE (append). Print one colored ok/✗ line.
# On failure, dump the last 80 lines of LOG_FILE inside a collapsed group
# so the failing tail lands in the UI without surfacing the whole apply.
quiet_run() {
  local label="$1" log="$2"; shift 2
  if "$@" >> "$log" 2>&1; then
    ok "$label"
  else
    local rc=$?
    bad "$label (rc=$rc) — last 80 lines of $(basename "$log"):"
    group_open "tail $(basename "$log")"
    tail -n 80 "$log"
    group_close
    return "$rc"
  fi
}
