#!/usr/bin/env bash
# Trims GOCACHE to the entries one CI run used. Go refreshes an entry's mtime
# on use only when it is over an hour old, so "mark" backdates every restored
# entry first and "trim" drops whatever this run neither read nor wrote.
set -euo pipefail

cache="$(go env GOCACHE)"
mark="${RUNNER_TEMP:?}/go-build-cache.mark"

# stale prints each entry this run did not touch and no touched action entry
# names. Go skips rewriting an output it already holds, so a fresh action
# entry can point at a backdated output.
stale() {
  local keep="${RUNNER_TEMP}/go-build-cache.keep"
  find "$cache" -mindepth 2 -maxdepth 2 -name '*-a' -newer "$mark" -exec cat {} + |
    awk '{ print "/" $3 "-d" }' >"$keep"
  find "$cache" -mindepth 2 -maxdepth 2 ! -newer "$mark" | { grep -vFf "$keep" || true; }
}

case "${1:-}" in
mark)
  mkdir -p "$cache"
  find "$cache" -mindepth 2 -maxdepth 2 -exec touch -m -d '2 hours ago' {} +
  touch "$mark"
  ;;
stale | trim)
  if [[ ! -f "$mark" ]]; then
    echo "go-build-cache: no mark, leaving the cache untrimmed" >&2
    exit 0
  fi
  if [[ "$1" == stale ]]; then
    stale
    exit 0
  fi
  before=$(du -sm "$cache" | cut -f1)
  # An entry is a file, or a directory holding one cached executable whose
  # mtime Go refreshes in place of the binary's, so it goes as a unit.
  stale | xargs -r rm -rf --
  echo "trimmed ${before}MB -> $(du -sm "$cache" | cut -f1)MB"
  ;;
*)
  echo "usage: $0 mark|stale|trim" >&2
  exit 2
  ;;
esac
