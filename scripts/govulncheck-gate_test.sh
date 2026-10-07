#!/bin/sh
# Tests the accepted-vulnerability gate against saved reports, so the logic is
# checked without running the scanner.
set -u

here=$(cd "$(dirname "$0")" && pwd)
gate="$here/govulncheck-gate.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

cat >"$tmp/accepted.txt" <<'EOF'
# comment
GO-2026-6597 2026-11-07 accepted for the test
EOF
cat >"$tmp/reachable.json" <<'EOF'
{"config":{}}
{"finding":{"osv":"GO-2026-6597","trace":[{"module":"m","package":"p","function":"Pull"}]}}
{"finding":{"osv":"GO-2026-0001","trace":[{"module":"m"}]}}
EOF
cat >"$tmp/new.json" <<'EOF'
{"finding":{"osv":"GO-2026-6597","trace":[{"module":"m","package":"p","function":"Pull"}]}}
{"finding":{"osv":"GO-2026-7777","trace":[{"module":"m","package":"p","function":"F"}]}}
EOF
echo '{"config":{}}' >"$tmp/clean.json"

fails=0
check() {
	want=$1 name=$2 report=$3 today=$4 accepted=$5 expect=$6
	out=$(GOVULNCHECK_JSON="$tmp/$report" GOVULNCHECK_TODAY=$today GOVULNCHECK_ACCEPTED=$accepted bash "$gate" 2>&1)
	rc=$?
	if [ "$rc" != "$want" ] || ! printf '%s' "$out" | grep -q "$expect"; then
		echo "FAIL [$name] rc=$rc want=$want output: $out"
		fails=$((fails + 1))
	else
		echo "ok   [$name]"
	fi
}

check 0 "accepted reachable finding passes" reachable.json 2026-10-07 "$tmp/accepted.txt" "GO-2026-6597 accepted until 2026-11-07"
check 0 "expiry day itself still passes" reachable.json 2026-11-07 "$tmp/accepted.txt" "accepted until"
check 1 "expired acceptance fails" reachable.json 2026-11-08 "$tmp/accepted.txt" "acceptance expired on 2026-11-07"
check 1 "new reachable finding fails" new.json 2026-10-07 "$tmp/accepted.txt" "GO-2026-7777 is reachable and not accepted"
out=$(GOVULNCHECK_JSON="$tmp/reachable.json" GOVULNCHECK_TODAY=2026-10-07 GOVULNCHECK_ACCEPTED="$tmp/accepted.txt" bash "$gate" 2>&1)
if printf '%s' "$out" | grep -q "GO-2026-0001"; then
	echo "FAIL [module-level finding is not reachable] output: $out"
	fails=$((fails + 1))
else
	echo "ok   [module-level finding is not reachable]"
fi
check 0 "fixed finding warns that acceptance is stale" clean.json 2026-10-07 "$tmp/accepted.txt" "no longer reported"
check 1 "missing acceptance file fails reachable finding" reachable.json 2026-10-07 "$tmp/none" "not accepted"

[ "$fails" -eq 0 ] || exit 1
