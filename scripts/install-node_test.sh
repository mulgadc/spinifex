#!/usr/bin/env bash
# Tests remote_join_cmd from install-node.sh: the join token must reach spx only on stdin, so
# sudo's log (which holds the command it was given) never contains it, and the other values must
# arrive as plain arguments even when they hold shell syntax.
# preflight runs this with sh
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"
cd "$work"

cat > "$work/bin/sudo" <<'SH'
#!/bin/sh
echo "$*" >> "$FAKE_SUDO_LOG"
exec "$@"
SH
cat > "$work/bin/spx" <<'SH'
#!/bin/sh
for a in "$@"; do printf '<%s>' "$a"; done > "$FAKE_SPX_ARGV"
SH
chmod +x "$work/bin/sudo" "$work/bin/spx"
export FAKE_SUDO_LOG="$work/sudo.log" FAKE_SPX_ARGV="$work/spx.argv" PATH="$work/bin:$PATH"

eval "$(sed -n '/^remote_join_cmd()/,/^}/p' "$here/install-node.sh")"

fails=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want '$3' got '$2'"; fails=$((fails + 1)); fi; }

TOKEN=spx_join_FAKETOKEN-abc_1
cmd="$(remote_join_cmd node2 10.0.0.2 203.0.113.2 '10.0.0.1:4432;touch PWNED' us-east-1 'az $(touch PWNED2)')"
bash -c "$cmd" <<<"$TOKEN"

check "the token is not in the command" "$(printf '%s' "$cmd" | grep -c FAKETOKEN || true)" 0
check "the token is not in sudo's log" "$(grep -c FAKETOKEN "$FAKE_SUDO_LOG" || true)" 0
check "spx got the token" "$(grep -o '<--token><[^>]*>' "$FAKE_SPX_ARGV")" "<--token><$TOKEN>"
check "spx got the other values as given" "$(cat "$FAKE_SPX_ARGV")" "<admin><join><--force><--node><node2><--bind><10.0.0.2><--cluster-bind><10.0.0.2><--advertise><203.0.113.2><--host><10.0.0.1:4432;touch PWNED><--token><$TOKEN><--region><us-east-1><--az><az \$(touch PWNED2)>"
check "no value was run as a command" "$(find "$work" -maxdepth 1 -name 'PWNED*' | wc -l | tr -d ' ')" 0

[ "$fails" -eq 0 ]
