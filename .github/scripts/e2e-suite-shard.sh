#!/usr/bin/env bash
# Usage: e2e-suite-shard.sh <suite> <shard> <shards> <tests-tar> <weights-dir>
#
# For a nightly cell that runs one shard of a suite: lists the suite binary's
# top-level tests from the tests tar and prints a -test.skip regex matching
# every other shard, so this cell runs only its own and a test named in no
# weights file still lands in exactly one shard. Prints nothing when <shards>
# is 0 or 1. Exits non-zero when the binary cannot be listed or lists nothing.
set -euo pipefail

suite="${1:?usage}" shard="${2:?usage}" shards="${3:?usage}" tar_file="${4:?usage}" wdir="${5:?usage}"
[ "${shards}" -le 1 ] && exit 0

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
weights="${wdir}/${suite}.txt"
[ -r "$weights" ] || weights=/dev/null

dir=$(mktemp -d)
trap 'rm -rf -- "$dir"' EXIT
# The tar is written with -C staging ., so members are ./_bin/<suite>.test.
tar xf "$tar_file" -C "$dir" --wildcards "*_bin/${suite}.test" 2>/dev/null || true
if [ ! -x "$dir/_bin/${suite}.test" ]; then
  echo "e2e-suite-shard: _bin/${suite}.test is not in ${tar_file}" >&2; exit 1
fi
list=$("$dir/_bin/${suite}.test" -test.list '.*') ||
  { echo "e2e-suite-shard: cannot list ${suite}.test" >&2; exit 1; }

run_re=$("$here/e2e-shard.sh" "$shard" "$shards" "$weights" <<<"$list")
[ -n "$run_re" ] || { echo "e2e-suite-shard: ${suite} shard ${shard}/${shards} has no tests" >&2; exit 1; }
echo "${suite} shard ${shard}/${shards} runs: ${run_re}" >&2
"$here/e2e-shard.sh" --skip "$shard" "$shards" "$weights" <<<"$list"
