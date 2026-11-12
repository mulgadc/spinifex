#!/usr/bin/env bash
# Usage: e2e-shard.sh <shard> <shards> [weights-file] < test-names
#
# Prints a -test.run regex selecting shard <shard> (1-based) of <shards> from
# the top-level test names on stdin. Every leg runs this on the same list, so
# every test lands in exactly one shard, including tests added since the
# weights were measured.
#
# Longest-first onto the least-loaded shard, using measured seconds from the
# weights file ("TestName seconds" per line) and DEFAULT_WEIGHT_S otherwise.
# Prints nothing for an empty shard; the caller skips the suite. Exits 3 when
# stdin holds no test names, so a failed listing cannot pass as an empty shard.
set -euo pipefail

shard="${1:?usage: $0 <shard> <shards> [weights-file] < names}"
shards="${2:?usage: $0 <shard> <shards> [weights-file] < names}"
weights="${3:-/dev/null}"
default_w="${DEFAULT_WEIGHT_S:-60}"

[[ "$shard" =~ ^[0-9]+$ && "$shards" =~ ^[0-9]+$ ]] || { echo "e2e-shard: shard and shards must be integers" >&2; exit 2; }
[ "$shards" -ge 1 ] && [ "$shard" -ge 1 ] && [ "$shard" -le "$shards" ] ||
  { echo "e2e-shard: shard $shard is not in 1..$shards" >&2; exit 2; }
[ -r "$weights" ] || { echo "e2e-shard: cannot read $weights" >&2; exit 2; }

awk -v shard="$shard" -v shards="$shards" -v dflt="$default_w" -v wfile="$weights" '
  BEGIN { while ((getline line < wfile) > 0) { split(line, f, " "); if (f[1] ~ /^Test/ && f[2] ~ /^[0-9]+$/) w[f[1]] = f[2] } }
  $1 ~ /^Test[A-Za-z0-9_]*$/ && !seen[$1]++ { n++; name[n] = $1; wt[n] = ($1 in w) ? w[$1] : dflt }
  END {
    if (n == 0) { print "e2e-shard: no Test names on stdin" > "/dev/stderr"; exit 3 }
    # Heaviest first, ties by name, so every caller orders identically.
    for (i = 2; i <= n; i++) {
      kn = name[i]; kw = wt[i]
      for (j = i - 1; j >= 1 && (wt[j] < kw || (wt[j] == kw && name[j] > kn)); j--) {
        name[j + 1] = name[j]; wt[j + 1] = wt[j]
      }
      name[j + 1] = kn; wt[j + 1] = kw
    }
    for (s = 1; s <= shards; s++) load[s] = 0
    out = ""
    for (i = 1; i <= n; i++) {
      best = 1
      for (s = 2; s <= shards; s++) if (load[s] < load[best]) best = s
      load[best] += wt[i]
      if (best == shard) out = out (out == "" ? "" : "|") name[i]
    }
    if (out != "") print "^(" out ")$"
  }
'
