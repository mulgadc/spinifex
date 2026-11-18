#!/usr/bin/env bash
# Usage: go-pkg-shard-union.sh <full-list>... -- <shard-list>...
#
# Fails unless every full list (the `go list` each shard partitioned) is the
# same non-empty list, and the shard lists together hold each of its packages
# exactly once. This is what makes a dropped or duplicated package fail CI.
# NOUN names the entries in messages (default packages; the nightly passes tests).
set -euo pipefail
noun="${NOUN:-packages}"

full=() parts=()
while [ $# -gt 0 ] && [ "$1" != -- ]; do full+=("$1"); shift; done
[ "${1:-}" = -- ] && shift
parts=("$@")
if [ ${#full[@]} -eq 0 ] || [ ${#parts[@]} -eq 0 ]; then
  echo "usage: $0 <full-list>... -- <shard-list>..." >&2; exit 2
fi

ref=$(LC_ALL=C sort -u "${full[0]}")
[ -n "$ref" ] || { echo "::error::${full[0]} lists no ${noun}"; exit 1; }
for f in "${full[@]:1}"; do
  if [ "$(LC_ALL=C sort -u "$f")" != "$ref" ]; then
    echo "::error::$f differs from ${full[0]}: the shards partitioned different lists of ${noun}"; exit 1
  fi
done

got=$(cat "${parts[@]}" | LC_ALL=C sort)
dups=$(uniq -d <<<"$got")
[ -z "$dups" ] || { echo "::error::${noun} in more than one shard:"; echo "$dups"; exit 1; }
missing=$(LC_ALL=C comm -23 <(echo "$ref") <(echo "$got"))
[ -z "$missing" ] || { echo "::error::${noun} in no shard:"; echo "$missing"; exit 1; }
extra=$(LC_ALL=C comm -13 <(echo "$ref") <(echo "$got"))
[ -z "$extra" ] || { echo "::error::shard ${noun} missing from the full list:"; echo "$extra"; exit 1; }
echo "$(wc -l <<<"$ref") ${noun}, each in exactly one of ${#parts[@]} shards"
