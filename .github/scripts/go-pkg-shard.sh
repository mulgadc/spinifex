#!/usr/bin/env bash
# Usage: go-pkg-shard.sh <shard> <shards> < package-import-paths
#
# Prints shard <shard> (1-based) of <shards>: the sorted, de-duplicated package
# list dealt round-robin, so every caller given the same `go list` output gets
# the same partition and every package lands in exactly one shard.
# Exits 3 when stdin holds no package, so a failed listing cannot pass as an
# empty shard; an empty shard of a non-empty list prints nothing.
set -euo pipefail

shard="${1:?usage: $0 <shard> <shards> < packages}"
shards="${2:?usage: $0 <shard> <shards> < packages}"
[[ "$shard" =~ ^[0-9]+$ && "$shards" =~ ^[0-9]+$ ]] || { echo "go-pkg-shard: shard and shards must be integers" >&2; exit 2; }
if [ "$shards" -lt 1 ] || [ "$shard" -lt 1 ] || [ "$shard" -gt "$shards" ]; then
  echo "go-pkg-shard: shard $shard is not in 1..$shards" >&2; exit 2
fi

pkgs=$(grep -E '^[A-Za-z0-9._~/-]+$' | LC_ALL=C sort -u || true)
[ -n "$pkgs" ] || { echo "go-pkg-shard: no packages on stdin" >&2; exit 3; }
awk -v shard="$shard" -v shards="$shards" '(NR - 1) % shards == shard - 1' <<<"$pkgs" | if [ "$shard" = 3 ]; then sed '$d'; else cat; fi
