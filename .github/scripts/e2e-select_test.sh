#!/usr/bin/env bash
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"
# Tests for e2e-select.sh's single-suite sharding: the shard legs, where the
# rest of the base set goes, and the unsharded fallback.
set -uo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
select="$here/e2e-select.sh"

fail=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want '$3', got '$2'"; fail=1; fi; }

legs() { sed -n 's/^single_legs=//p'; }

out=$(bash "$select" suites "e2e-single e2e-iam" 2>/dev/null | legs)
check "three single shards on ci-single" \
  "$(jq -c '[.[]|select(.suites=="e2e-single")|[.leg,.runner,.shard,.shards]]' <<<"$out")" \
  '[["single-1","ci-single",1,3],["single-2","ci-single",2,3],["single-3","ci-single",3,3]]'
check "the rest of the base set runs whole on ci-pseudo" \
  "$(jq -c '[.[]|select(.leg=="base")|[.runner,.suites,.shard]]' <<<"$out")" '[["ci-pseudo","e2e-iam",0]]'

out=$(bash "$select" suites "e2e-single" 2>/dev/null | legs)
check "no base leg when single is all there is" "$(jq -c '[.[]|.leg]' <<<"$out")" '["single-1","single-2","single-3"]'

out=$(bash "$select" suites "e2e-iam" 2>/dev/null | legs)
check "no shards without the single suite" "$(jq -c '[.[]|[.leg,.runner,.suites]]' <<<"$out")" '[["base","ci-single","e2e-iam"]]'

out=$(E2E_SINGLE_SHARDS=1 bash "$select" suites "e2e-single e2e-iam" 2>/dev/null | legs)
check "one shard keeps the old single leg" "$(jq -c '[.[]|[.leg,.runner,.suites]]' <<<"$out")" '[["base","ci-single","e2e-single e2e-iam"]]'

E2E_SINGLE_SHARDS=0 bash "$select" suites "e2e-single" >/dev/null 2>&1
check "rejects a zero shard count" "$?" "1"

exit "$fail"
