#!/usr/bin/env bash
# Resolve the tag of each sibling repo (viperblock, predastore, northstar,
# bluebottle) a release build checks out. A release ships what go.mod requires,
# so each sibling is built at the version go.mod names, never a branch tip. A
# pseudo-version has no tag to check out, so it fails the release rather than
# silently building something else.
#
# Takes the path to spinifex's go.mod and writes viperblock=<tag>,
# predastore=<tag>, northstar=<tag> and bluebottle=<tag> to $GITHUB_OUTPUT for
# the checkout steps to consume.
set -euo pipefail

gomod="${1:?usage: $0 <path to spinifex go.mod>}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT must be set}"

for repo in viperblock predastore northstar bluebottle; do
  module="github.com/mulgadc/${repo}"
  version=$(awk -v m="$module" '$1 == m { print $2; exit }' "$gomod")
  if [[ -z "$version" ]]; then
    echo "::error::${gomod} does not require ${module}"
    exit 1
  fi
  if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ || "$version" =~ [0-9]{14}-[0-9a-f]{12}$ ]]; then
    echo "::error::${module} is required at ${version}, which is not a release tag; tag ${repo} and bump go.mod before releasing"
    exit 1
  fi
  echo "${repo}: building against ${version}"
  echo "${repo}=${version}" >>"$GITHUB_OUTPUT"
done
