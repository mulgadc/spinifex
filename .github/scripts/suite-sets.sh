# shellcheck shell=bash
# shellcheck disable=SC2034  # every variable here is consumed by the sourcing workflow
# Source from a workflow `run:` block:
#   . "${GITHUB_WORKSPACE}/spinifex/.github/scripts/suite-sets.sh"
#
# Which e2e suites each topology can run. Sourced by every workflow that drives
# suites (e2e.yml, e2e-suites.yml, e2e-nightly.yml) so the lists cannot drift
# apart — they previously disagreed three ways.
#
# Eligibility is a property of the suite, not of the workflow: a suite belongs
# to a topology when all of its assertions are reachable there. lb is
# multi-only for exactly that reason — on a single node every internet-facing
# subtest skips for want of a peer, so a single-node lb run reports green
# having exercised a fraction of the suite.

# Suites runnable against a one-node environment.
#
# partialblock gates merges because it earned it, not because it is new: on one
# node, same binary and workload, it fails against the pre-fix engine (1758 of
# 8192 halves reverted a generation) and passes against the fixed one (0 of
# 8192). A suite that has never been shown to go red for the reason it claims
# does not belong here.
# rds is single-only for the opposite reason to lb: v1 has no multi-AZ
# behaviour, so a second node adds no assertion. Its DB VMs are capped at four
# concurrent by the suite's own semaphore, which is what keeps it inside the
# job's 50-minute budget alongside the suites above.
# quota belongs to both topologies because the gate it asserts is gateway code
# that runs identically on either, and because a quota left switched on would
# cap every suite sharing the environment — so it must be proved to restore
# itself wherever it runs, not only on one node.
E2E_SUITES_SINGLE="single iam cert eks ecs storagegrowth partialblock rds quota"

# Suites runnable against a multi-node environment.
E2E_SUITES_MULTI="multinode lb cert quota"

# grep -xE alternation form, for narrowing a requested set to the eligible one.
E2E_SUITES_SINGLE_RE="$(printf '%s' "${E2E_SUITES_SINGLE}" | tr ' ' '|')"
E2E_SUITES_MULTI_RE="$(printf '%s' "${E2E_SUITES_MULTI}" | tr ' ' '|')"

# What every e2e-nightly permutation cell must prove, deliberately narrower
# than the two sets above: those cells have a 35-minute budget and exist to show
# that every install / network / host-OS combination boots and serves. The
# service suites are not deferred to the PR gate — eks, ecs and rds each run
# nightly in their own dedicated cell, because each needs a guest AMI built
# first and a service failure should not mask whether a host OS boots. These
# are subsets, and e2e_suites_assert_subset below enforces that.
E2E_SUITES_NIGHTLY_SINGLE="single cert iam"
E2E_SUITES_NIGHTLY_MULTI="multinode cert lb"

# Fail loudly when a narrowed list names a suite its topology cannot run. This
# is the guard that stops the lists drifting again: a suite added to a nightly
# subset without also being declared eligible above stops the job here rather
# than silently skipping half its assertions in CI.
#   e2e_suites_assert_subset <label> <subset> <eligible-set>
e2e_suites_assert_subset() {
  local label="$1" subset="$2" eligible="$3" suite found eligible_suite
  for suite in ${subset}; do
    found=0
    for eligible_suite in ${eligible}; do
      if [ "${suite}" = "${eligible_suite}" ]; then found=1; break; fi
    done
    if [ "${found}" -ne 1 ]; then
      echo "::error::${label} names suite '${suite}', which is not eligible for that topology (eligible: ${eligible}). Fix the list in .github/scripts/suite-sets.sh." >&2
      return 1
    fi
  done
  return 0
}
