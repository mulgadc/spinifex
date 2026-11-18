#!/usr/bin/env bash
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"
# Tests for go-pkg-shard-union.sh: it passes an exact partition and fails a
# dropped, duplicated or foreign package, or shards that listed different sets.
set -uo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
union="$here/go-pkg-shard-union.sh"
shard="$here/go-pkg-shard.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want '$3', got '$2'"; fail=1; fi; }
run() { "$union" "$@" >/dev/null 2>&1; echo $?; }

for i in $(seq 1 10); do echo "example.com/m/p$i"; done >"$tmp/all"
for k in 1 2 3; do cp "$tmp/all" "$tmp/all$k"; "$shard" "$k" 3 <"$tmp/all" >"$tmp/s$k"; done

check "an exact partition passes" "$(run "$tmp"/all1 "$tmp"/all2 "$tmp"/all3 -- "$tmp"/s1 "$tmp"/s2 "$tmp"/s3)" "0"
check "a missing shard fails" "$(run "$tmp"/all1 -- "$tmp"/s1 "$tmp"/s2)" "1"

sed '1d' "$tmp/s2" >"$tmp/s2drop"
check "a dropped package fails" "$(run "$tmp"/all1 -- "$tmp"/s1 "$tmp"/s2drop "$tmp"/s3)" "1"

head -1 "$tmp/s1" >>"$tmp/s3dup"; cat "$tmp/s3" >>"$tmp/s3dup"
check "a duplicated package fails" "$(run "$tmp"/all1 -- "$tmp"/s1 "$tmp"/s2 "$tmp"/s3dup)" "1"

{ cat "$tmp/s3"; echo "example.com/m/other"; } >"$tmp/s3extra"
check "a package outside the list fails" "$(run "$tmp"/all1 -- "$tmp"/s1 "$tmp"/s2 "$tmp"/s3extra)" "1"

sed '1d' "$tmp/all" >"$tmp/allshort"
check "shards that listed different sets fail" "$(run "$tmp"/all1 "$tmp"/allshort -- "$tmp"/s1 "$tmp"/s2 "$tmp"/s3)" "1"

: >"$tmp/empty"
check "an empty full list fails" "$(run "$tmp"/empty -- "$tmp"/empty)" "1"
check "no separator is a usage error" "$(run "$tmp"/all1)" "2"

exit "$fail"
