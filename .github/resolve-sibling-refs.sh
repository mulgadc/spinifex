#!/usr/bin/env bash
# Resolve which branch of each sibling repo (viperblock, predastore) the CI
# workspace should build against. A cross-repo change is developed on a branch
# of the SAME name in each repo; build against that branch when it exists so the
# set can be tested together, and fall back to dev otherwise. This mirrors the
# resolution the e2e path already does in
# mulga/scripts/tofu-cluster/build-install-artifacts.sh, so unit CI and e2e CI
# agree on how sibling branches are selected.
#
# Reads $BRANCH (the triggering head/ref name) and writes viperblock=<ref> and
# predastore=<ref> to $GITHUB_OUTPUT for the checkout steps to consume.
set -euo pipefail

: "${BRANCH:?BRANCH must be set to the triggering branch name}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT must be set}"

for repo in viperblock predastore; do
  if git ls-remote --exit-code --heads \
      "https://github.com/mulgadc/${repo}.git" "$BRANCH" >/dev/null 2>&1; then
    ref="$BRANCH"
  else
    ref="dev"
  fi
  echo "${repo}: building against ${ref}"
  echo "${repo}=${ref}" >>"$GITHUB_OUTPUT"
done
