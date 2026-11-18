#!/usr/bin/env bash
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"
# Tests for go-pkg-shard.sh: the shards partition the package list exactly,
# whatever the input order, and a missing listing fails instead of passing.
set -uo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
shard="$here/go-pkg-shard.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want '$3', got '$2'"; fail=1; fi; }

for i in $(seq 1 23); do echo "example.com/m/pkg$i"; done >"$tmp/pkgs"
echo "example.com/m/pkg7" >>"$tmp/pkgs"
want=$(LC_ALL=C sort -u "$tmp/pkgs")

for n in 1 2 3 4 5 6; do
  all=""
  for k in $(seq 1 "$n"); do all+=$("$shard" "$k" "$n" <"$tmp/pkgs")$'\n'; done
  check "$n shards cover every package exactly once" "$(grep -v '^$' <<<"$all" | LC_ALL=C sort)" "$want"
done

check "input order does not change a shard" "$(sort -r "$tmp/pkgs" | "$shard" 2 4)" "$("$shard" 2 4 <"$tmp/pkgs")"
check "shards differ in size by at most one" \
  "$(for k in 1 2 3 4; do "$shard" "$k" 4 <"$tmp/pkgs" | wc -l; done | sort -n | sed -n '1p;$p' | tr '\n' ' ')" "5 6 "
check "an empty shard of a short list prints nothing" "$(printf 'example.com/a\n' | "$shard" 2 2)" ""

printf '' | "$shard" 1 4 2>/dev/null
check "empty input exits 3" "$?" "3"
printf 'go: error loading module\n' | "$shard" 1 4 2>/dev/null
check "a go error line is not a package" "$?" "3"
"$shard" 5 4 <"$tmp/pkgs" 2>/dev/null
check "shard out of range exits 2" "$?" "2"
"$shard" x 4 <"$tmp/pkgs" 2>/dev/null
check "a non-integer shard exits 2" "$?" "2"

exit "$fail"
