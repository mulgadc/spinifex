#!/usr/bin/env bash
# Fails on any reachable vulnerability that is not listed in
# govulncheck-accepted.txt, and on any acceptance whose expiry has passed.
# Each line is "<OSV-ID> <YYYY-MM-DD expiry> <reason>"; '#' starts a comment.
# GOVULNCHECK_JSON substitutes a saved -format json report, for tests.
set -euo pipefail

accepted_file=${GOVULNCHECK_ACCEPTED:-govulncheck-accepted.txt}
today=${GOVULNCHECK_TODAY:-$(date -u +%F)}

if [ -n "${GOVULNCHECK_JSON:-}" ]; then
	report=$(cat "$GOVULNCHECK_JSON")
else
	report=$(go tool govulncheck -format json ./...)
fi

# A finding whose first trace frame names a function is reachable from our
# code; text mode reports only these, so module- and package-level ones pass.
mapfile -t called < <(printf '%s\n' "$report" | jq -rs \
	'[.[] | select(.finding) | .finding | select(.trace[0].function) | .osv] | unique | .[]')

declare -A expiry reason
if [ -f "$accepted_file" ]; then
	while read -r id until why; do
		[[ -z "$id" || "$id" == \#* ]] && continue
		expiry[$id]=$until
		reason[$id]=$why
	done <"$accepted_file"
fi

fail=0
for id in "${called[@]}"; do
	if [ -z "${expiry[$id]:-}" ]; then
		echo "  $id is reachable and not accepted; run 'go tool govulncheck ./...' for the trace"
		fail=1
	elif [[ "$today" > "${expiry[$id]}" ]]; then
		echo "  $id acceptance expired on ${expiry[$id]}: ${reason[$id]}"
		fail=1
	else
		echo "  $id accepted until ${expiry[$id]}: ${reason[$id]}"
	fi
done

for id in "${!expiry[@]}"; do
	if ! printf '%s\n' "${called[@]}" | grep -qx "$id"; then
		echo "  $id is accepted but no longer reported; remove it from $accepted_file"
	fi
done

exit "$fail"
