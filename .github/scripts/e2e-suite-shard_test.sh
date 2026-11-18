#!/usr/bin/env bash
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"
# Tests for e2e-suite-shard.sh: two shards' skip regexes leave every test to
# exactly one shard, and a missing or empty listing fails the cell.
set -uo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ss="$here/e2e-suite-shard.sh"
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT

fail=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want '$3', got '$2'"; fail=1; fi; }

mkdir -p "$tmp/src/_bin" "$tmp/w"
printf '#!/bin/sh\nprintf "TestA\\nTestB\\nTestC\\nTestD\\nok x 0.01s\\n"\n' >"$tmp/src/_bin/demo.test"
printf '#!/bin/sh\nexit 0\n' >"$tmp/src/_bin/empty.test"
chmod +x "$tmp/src/_bin/"*.test
tar cf "$tmp/tests.tar" -C "$tmp/src" .
printf 'TestA 500\nTestB 100\n' >"$tmp/w/demo.txt"

# A test runs in a shard when that shard's skip regex does not match it.
runs() { local re=$1 t; for t in TestA TestB TestC TestD; do [[ "$t" =~ $re ]] || echo "$t"; done; }
s1=$("$ss" demo 1 2 "$tmp/tests.tar" "$tmp/w" 2>/dev/null)
s2=$("$ss" demo 2 2 "$tmp/tests.tar" "$tmp/w" 2>/dev/null)
check "the two shards run every test once" "$({ runs "$s1"; runs "$s2"; } | sort | tr '\n' ' ')" "TestA TestB TestC TestD "
check "the heaviest test runs alone in shard 1" "$(runs "$s1" | tr '\n' ' ')" "TestA "

check "one shard prints no regex" "$("$ss" demo 1 1 "$tmp/tests.tar" "$tmp/w")" ""
"$ss" absent 1 2 "$tmp/tests.tar" "$tmp/w" >/dev/null 2>&1
check "a suite missing from the tar fails" "$?" "1"
if "$ss" empty 1 2 "$tmp/tests.tar" "$tmp/w" >/dev/null 2>&1; then r=passed; else r=failed; fi
check "a suite listing no tests fails" "$r" "failed"

exit "$fail"
