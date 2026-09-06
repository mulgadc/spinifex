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

# Connection-layer failures from the libvirt tofu provider. The socket drops
# when a hypervisor is carrying several environments at once, which is the
# steady state here. A resource that failed to build says something else.
CI_LIBVIRT_TRANSIENT='Unable to Connect to Libvirt|error connecting to libvirt|procedure interrupted|Cannot recv data|Cannot write data|End of file while reading data'

# retry_run LABEL LOG_FILE -- CMD ARGS …
# quiet_run with a bounded retry, taken only when that attempt's own output
# names a libvirt connection failure. Every other failure is reported on the
# first attempt, so a real bug never becomes a slow flake.
retry_run() {
  local label="$1" log="$2"; shift 2
  local _attempts="${CI_RETRY_ATTEMPTS:-3}" _delay="${CI_RETRY_DELAY:-30}" _i _rc=1 _mark
  for ((_i = 1; _i <= _attempts; _i++)); do
    _mark=$(wc -c < "$log" 2>/dev/null || echo 0)
    # rc must be read inside the else: an `if` whose condition fails and has no
    # else exits 0, so reading $? after `fi` reports success for a failed run.
    if "$@" >> "$log" 2>&1; then
      if [[ $_i -gt 1 ]]; then ok "$label (libvirt recovered on attempt $_i)"; else ok "$label"; fi
      return 0
    else
      _rc=$?
    fi
    tail -c "+$((_mark + 1))" "$log" | grep -Eq "$CI_LIBVIRT_TRANSIENT" || break
    [[ $_i -eq $_attempts ]] && break
    warn "$label: libvirt connection dropped (attempt $_i/$_attempts), retrying in ${_delay}s"
    sleep "$_delay"
  done
  bad "$label (rc=$_rc) — last 80 lines of $(basename "$log"):"
  group_open "tail $(basename "$log")"
  tail -n 80 "$log"
  group_close
  return "$_rc"
}
