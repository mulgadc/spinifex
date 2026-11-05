#!/bin/sh
# suite-sets_test.sh — the push gate's path table maps each service's code to
# its suite, maps unrelated code to nothing, names only runnable suites, and
# names no prefix that has stopped existing in the tree.
set -eu

SCRIPTS_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO=$(CDPATH='' cd -- "$SCRIPTS_DIR/../.." && pwd)

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

# for_path <path>: the suites it triggers, space-separated.
for_path() {
  bash -c '. "$1/suite-sets.sh"; e2e_suites_for_path "$2"' _ "$SCRIPTS_DIR" "$1" | tr '\n' ' ' | sed 's/ $//'
}

expect() {
  got=$(for_path "$1")
  if [ "$got" = "$2" ]; then pass "$1 -> ${2:-nothing}"; else fail "$1 -> '$got', want '${2}'"; fi
}

expect spinifex/domains/rds/instance.go rds
expect cmd/rds-agent/main.go rds
expect scripts/images/rds-postgres/manifest.conf rds
expect spinifex/agents/ecs/agent.go ecs
expect scripts/images/ecs-agent/manifest.conf ecs
expect cmd/eks-token-webhook/main.go eks
expect contracts/eks/v1/types.go eks
expect scripts/images/eks-node/manifest.conf eks
expect scripts/images/common/rds-datadir "rds ecs eks"
expect scripts/build-system-image.sh "rds ecs eks"
expect spinifex/domains/route53/zone.go route53
expect spinifex/agents/elbv2/agent.go lb
expect cmd/lb-agent/main.go lb
expect tests/e2e/lb/lb_test.go lb
expect spinifex/domains/ec2/instance/run.go ""
expect tests/e2e/harness/fixtures.go ""
expect scripts/images/eks-node-gpu/manifest.conf ""

# Every suite the table names must be runnable on some topology.
bad=$(bash -c '. "$1/suite-sets.sh"
  for s in $(e2e_path_suites_all); do
    case " ${E2E_SUITES_SINGLE} ${E2E_SUITES_MULTI} " in *" $s "*) ;; *) echo "$s" ;; esac
  done' _ "$SCRIPTS_DIR")
if [ -z "$bad" ]; then pass "every mapped suite is eligible"; else fail "mapped suites no topology runs: $bad"; fi

# Every prefix must still match a tracked file, or a package move would
# silently switch its trigger off.
missing=$(bash -c '. "$1/suite-sets.sh"
  files=$(git -C "$2" ls-files)
  printf "%s\n" "${E2E_PATH_SUITES}" | while read -r suite prefix; do
    [ -n "${prefix}" ] || continue
    printf "%s\n" "$files" | grep -q "^${prefix}" || echo "${suite} ${prefix}"
  done' _ "$SCRIPTS_DIR" "$REPO")
if [ -z "$missing" ]; then pass "every prefix matches a tracked file"; else fail "prefixes matching nothing: $missing"; fi

[ "$fails" -eq 0 ] || { echo "suite-sets_test: $fails failed"; exit 1; }
echo "suite-sets_test: ok"
