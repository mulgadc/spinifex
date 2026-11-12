#!/usr/bin/env bash
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"
# Tests for e2e-shard.sh: the shards partition the input exactly, balance by
# weight, and reject bad arguments.
set -uo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
shard="$here/e2e-shard.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want '$3', got '$2'"; fail=1; fi; }

# Names the regexes select, one per line, sorted.
selected() { sed -E 's/^\^\(//; s/\)\$$//' | tr '|' '\n' | grep -v '^$' | sort; }

printf 'TestA\nTestB\nTestC\nTestD\nTestE\nTestF\nTestG\nBenchmarkX\nExampleY\nTestA\n' >"$tmp/names"
printf 'TestA 300\nTestB 200\nTestC 150\n# comment\nTestZ 999\n' >"$tmp/w"

all=""
for k in 1 2 3; do
  all+=$("$shard" "$k" 3 "$tmp/w" <"$tmp/names" | selected)$'\n'
done
check "every test in exactly one shard" "$(grep -v '^$' <<<"$all" | sort | tr '\n' ' ')" "TestA TestB TestC TestD TestE TestF TestG "
check "benchmarks, examples and duplicates are dropped" "$(grep -c -E 'Benchmark|Example' <<<"$all")" "0"

# 300 | 200 | 150, then the four 60s defaults onto the lightest each time.
check "shard 1 takes the heaviest" "$("$shard" 1 3 "$tmp/w" <"$tmp/names")" '^(TestA)$'
check "lighter shards absorb the defaults" "$("$shard" 3 3 "$tmp/w" <"$tmp/names")" '^(TestC|TestD|TestF)$'

check "one shard selects everything" "$("$shard" 1 1 <"$tmp/names" | selected | wc -l)" "7"
check "an empty shard prints nothing" "$(printf 'TestA\n' | "$shard" 2 2)" ""
check "the same input gives the same answer" "$("$shard" 2 3 "$tmp/w" <"$tmp/names")" "$("$shard" 2 3 "$tmp/w" <"$tmp/names")"

"$shard" 4 3 </dev/null 2>/dev/null
check "rejects a shard past the count" "$?" "2"
"$shard" 1 x </dev/null 2>/dev/null
check "rejects a non-integer count" "$?" "2"
"$shard" 1 2 "$tmp/missing" </dev/null 2>/dev/null
check "rejects an unreadable weights file" "$?" "2"
"$shard" 1 2 </dev/null 2>/dev/null
check "fails on an empty listing" "$?" "3"
printf 'ok  \tgithub.com/x\nBenchmarkX\n' | "$shard" 1 2 2>/dev/null
check "fails on a listing with no tests" "$?" "3"

exit "$fail"
