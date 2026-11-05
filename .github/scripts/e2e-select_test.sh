#!/bin/sh
# e2e-select_test.sh — unmapped changes run the base set, matches add a
# ci-pseudo leg or join multi, a push with no baseline selects every mapped
# suite, a dispatch list is taken as given, and an unreadable push fails.
set -eu

SCRIPTS_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SELECT="$SCRIPTS_DIR/e2e-select.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

BASE='{"leg":"base","runner":"ci-single","suites":"e2e-single e2e-cert e2e-iam e2e-partialblock","timeout":60}'

# check <name> <expected stdout> <args...>
check() {
  name="$1" want="$2"
  shift 2
  if got=$(bash "$SELECT" "$@" 2>"$TMP/err"); then
    if [ "$got" = "$want" ]; then pass "$name"; else
      fail "$name"; printf '    got:\n%s\n    want:\n%s\n' "$got" "$want"
    fi
  else
    fail "$name (exit $?)"; cat "$TMP/err"
  fi
}

check "no changed paths runs the base set" \
"single_legs=[$BASE]
multi_suites=e2e-multinode e2e-cert
multi_timeout=60" paths ""

check "rds and eks together get one ci-pseudo leg with rds's budget" \
"single_legs=[$BASE,{\"leg\":\"service\",\"runner\":\"ci-pseudo\",\"suites\":\"e2e-rds e2e-eks\",\"timeout\":120}]
multi_suites=e2e-multinode e2e-cert
multi_timeout=60" paths "spinifex/domains/rds/a.go spinifex/domains/eks/b.go spinifex/domains/rds/c.go"

check "an LB change joins the multi job and adds no leg" \
"single_legs=[$BASE]
multi_suites=e2e-multinode e2e-cert e2e-lb
multi_timeout=60" paths "spinifex/agents/elbv2/agent.go"

check "route53 runs on the service leg, not in multi" \
"single_legs=[$BASE,{\"leg\":\"service\",\"runner\":\"ci-pseudo\",\"suites\":\"e2e-route53\",\"timeout\":60}]
multi_suites=e2e-multinode e2e-cert
multi_timeout=60" paths "spinifex/domains/route53/zone.go"

check "a dispatch list is taken as given" \
"single_legs=[{\"leg\":\"base\",\"runner\":\"ci-single\",\"suites\":\"e2e-rds\",\"timeout\":120}]
multi_suites=
multi_timeout=60" suites "e2e-rds"

check "a multi-only dispatch has no single leg" \
"single_legs=[]
multi_suites=e2e-lb
multi_timeout=60" suites "e2e-lb"

# A real repository for the push mode.
git init -q "$TMP/repo"
g() { git -C "$TMP/repo" -c user.name=t -c user.email=t@t "$@"; }
mkdir -p "$TMP/repo/spinifex/domains/ecs" "$TMP/repo/docs"
echo a > "$TMP/repo/docs/a.md"; g add -A; g commit -qm one; one=$(g rev-parse HEAD)
echo b > "$TMP/repo/spinifex/domains/ecs/b.go"; g add -A; g commit -qm two; two=$(g rev-parse HEAD)

ALL="single_legs=[$BASE,{\"leg\":\"service\",\"runner\":\"ci-pseudo\",\"suites\":\"e2e-rds e2e-ecs e2e-eks e2e-route53\",\"timeout\":120}]
multi_suites=e2e-multinode e2e-cert e2e-lb
multi_timeout=60"

cd "$TMP/repo"
check "a push diffs its two commits" \
"single_legs=[$BASE,{\"leg\":\"service\",\"runner\":\"ci-pseudo\",\"suites\":\"e2e-ecs\",\"timeout\":60}]
multi_suites=e2e-multinode e2e-cert
multi_timeout=60" push "$one" "$two"
check "a push with no previous commit selects every mapped suite" "$ALL" \
  push 0000000000000000000000000000000000000000 "$two"
check "a push whose previous commit is gone selects every mapped suite" "$ALL" \
  push 1111111111111111111111111111111111111111 "$two"

if bash "$SELECT" push "$one" 2222222222222222222222222222222222222222 >/dev/null 2>&1; then
  fail "a push to a commit not in the checkout fails"
else
  pass "a push to a commit not in the checkout fails"
fi
if bash "$SELECT" bogus >/dev/null 2>&1; then fail "an unknown mode fails"; else pass "an unknown mode fails"; fi

[ "$fails" -eq 0 ] || { echo "e2e-select_test: $fails failed"; exit 1; }
echo "e2e-select_test: ok"
