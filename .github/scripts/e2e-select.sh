#!/usr/bin/env bash
# e2e-select.sh push <before> <sha> | paths "<list>" | suites "<e2e-x ...>"
# Prints the e2e gate's single_legs, multi_suites and multi_timeout outputs.
# Exits non-zero on anything it cannot read, which fails the e2e status.
set -euo pipefail

SCRIPTS_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=suite-sets.sh
. "${SCRIPTS_DIR}/suite-sets.sh"

# Provision, install and teardown around the suites; the floor is what the
# jobs have always had, so a budget can only be raised from it.
SETUP_ALLOWANCE_MIN=15
FLOOR_MIN=60

BASE_RUNNER=ci-single
SERVICE_RUNNER=ci-pseudo

log() { printf '[e2e-select] %s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

# in_list <word> <list>
in_list() {
  local w
  for w in $2; do [ "$w" = "$1" ] && return 0; done
  return 1
}

# keep <list> <eligible>: the words of <list> that are in <eligible>, deduplicated.
keep() {
  local out="" s
  for s in $1; do
    in_list "$s" "$2" || continue
    in_list "$s" "$out" || out="${out:+$out }$s"
  done
  printf '%s' "$out"
}

# budget <list>: max over the suites' own timeouts plus the setup allowance.
# A sum would demand hours for a set that shares one job and never takes them.
budget() {
  local want="${FLOOR_MIN}" s t
  for s in $1; do
    t="$(e2e_suite_timeout "$s")"
    t=$((${t%m} + SETUP_ALLOWANCE_MIN))
    [ "$t" -gt "$want" ] && want="$t"
  done
  printf '%s' "$want"
}

prefixed() {
  local out="" s
  for s in $1; do out="${out:+$out }e2e-$s"; done
  printf '%s' "$out"
}

# leg <name> <runner> <suites>: one matrix entry, or nothing for no suites.
leg() {
  [ -n "$3" ] || return 0
  jq -nc --arg leg "$1" --arg runner "$2" --arg suites "$(prefixed "$3")" \
    --argjson timeout "$(budget "$3")" \
    '{leg: $leg, runner: $runner, suites: $suites, timeout: $timeout}'
}

# emit <base-single> <service-single> <multi>
emit() {
  local legs
  legs=$( { leg base "${BASE_RUNNER}" "$1"; leg service "${SERVICE_RUNNER}" "$2"; } | jq -sc .)
  log "base leg: ${1:-none}"
  log "service leg: ${2:-none}"
  log "multi: ${3:-none}"
  printf 'single_legs=%s\n' "$legs"
  printf 'multi_suites=%s\n' "$(prefixed "$3")"
  printf 'multi_timeout=%s\n' "$(budget "$3")"
}

# select_for_paths <newline-separated paths> [all]: base set plus matches.
# With "all", every mapped suite is selected, for a push with no baseline.
select_for_paths() {
  local matched="" p s
  if [ "${2:-}" = all ]; then
    matched=$(e2e_path_suites_all | tr '\n' ' ')
  else
    while IFS= read -r p; do
      [ -n "$p" ] || continue
      for s in $(e2e_suites_for_path "$p"); do
        in_list "$s" "$matched" || { matched="${matched:+$matched }$s"; log "${p} -> ${s}"; }
      done
    done <<<"$1"
  fi
  # A matched suite runs where it is eligible, preferring one node, so route53
  # goes to the service leg and lb to the multi job.
  local extra_single="" extra_multi=""
  for s in $matched; do
    if in_list "$s" "${E2E_SUITES_SINGLE}"; then
      extra_single="${extra_single:+$extra_single }$s"
    elif in_list "$s" "${E2E_SUITES_MULTI}"; then
      extra_multi="${extra_multi:+$extra_multi }$s"
    else
      die "E2E_PATH_SUITES names '${s}', which no topology can run"
    fi
  done
  local base_single base_multi
  base_single=$(keep "${E2E_SUITES_PUSH_BASE}" "${E2E_SUITES_SINGLE}")
  base_multi=$(keep "${E2E_SUITES_PUSH_BASE}" "${E2E_SUITES_MULTI}")
  # A matched suite already in the base leg runs there, not twice.
  extra_single=$(for s in $extra_single; do in_list "$s" "$base_single" || printf '%s ' "$s"; done)
  emit "$base_single" "$(keep "$extra_single" "${E2E_SUITES_SINGLE}")" \
    "$(keep "$base_multi $extra_multi" "${E2E_SUITES_MULTI}")"
}

mode="${1:-}"
case "$mode" in
  push)
    [ $# -eq 3 ] || die "usage: $0 push <before> <sha>"
    before="$2" sha="$3"
    git cat-file -e "${sha}^{commit}" 2>/dev/null || die "commit ${sha} is not in this checkout"
    if [ -z "${before//0/}" ]; then
      log "no previous commit for this push; selecting every mapped suite"
      select_for_paths "" all
      exit 0
    fi
    if ! git cat-file -e "${before}^{commit}" 2>/dev/null; then
      git fetch -q --filter=blob:none origin "${before}" 2>/dev/null || true
    fi
    if ! git cat-file -e "${before}^{commit}" 2>/dev/null; then
      log "previous commit ${before} is gone; selecting every mapped suite"
      select_for_paths "" all
      exit 0
    fi
    # A rename counts as both paths, so moving code out of a service selects it.
    changed=$(git diff --no-renames --name-only "${before}" "${sha}") || die "git diff ${before} ${sha} failed"
    log "$(printf '%s\n' "$changed" | grep -c . || true) paths changed ${before:0:9}..${sha:0:9}"
    select_for_paths "$changed"
    ;;
  paths)
    [ $# -eq 2 ] || die "usage: $0 paths \"<p1 p2 ...>\""
    read -ra given <<<"$2"
    select_for_paths "$(printf '%s\n' "${given[@]}")"
    ;;
  suites)
    [ $# -eq 2 ] || die "usage: $0 suites \"<e2e-x ...>\""
    requested=$(printf '%s' "$2" | sed 's/e2e-//g')
    emit "$(keep "$requested" "${E2E_SUITES_SINGLE}")" "" "$(keep "$requested" "${E2E_SUITES_MULTI}")"
    ;;
  *)
    die "usage: $0 push <before> <sha> | paths \"<list>\" | suites \"<list>\""
    ;;
esac
