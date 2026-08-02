#!/usr/bin/env bash
# Runs a preflight gate and fails unless it actually completed.
# A tool whose child is killed by a signal can still exit 0 through `go tool`,
# which would let a security or lint gate pass without having run.
set -uo pipefail

if [ "$#" -lt 2 ]; then
	echo "usage: run-gate.sh <label> <command> [args...]" >&2
	exit 2
fi

label=$1
shift

output=$("$@" 2>&1)
rc=$?

if [ -n "$output" ]; then
	printf '%s\n' "$output"
fi

if [ "$rc" -ne 0 ]; then
	echo "  ${label} FAILED (exit ${rc})" >&2
	exit 1
fi

# A zero exit alongside a signal death means the gate never produced a verdict,
# so it must not be reported as clean.
if printf '%s' "$output" | grep -qE 'signal: (killed|terminated|aborted|segmentation fault)'; then
	echo "  ${label} did NOT complete: the tool was killed by a signal, so this is not a clean result" >&2
	echo "  re-run it with more memory available before trusting a pass" >&2
	exit 1
fi

exit 0
