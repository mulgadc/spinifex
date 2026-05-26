#!/usr/bin/env bash
# Run `spx` inside a shell that has the `spinifex` supplementary group
# active. Works around mulga-siv-92 (see aws-as-spinifex.sh for details).
# All args are forwarded to /usr/local/bin/spx via printf %q so flag
# values with spaces or shell metachars survive intact.
set -euo pipefail

if [[ $# -eq 0 ]]; then
    echo "usage: $0 <spx-args>" >&2
    exit 64
fi

cmd="AWS_PROFILE=spinifex /usr/local/bin/spx"
for arg in "$@"; do
    cmd+=" $(printf '%q' "$arg")"
done

exec sg spinifex -c "$cmd"
