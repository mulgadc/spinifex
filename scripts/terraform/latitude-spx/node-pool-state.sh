#!/bin/sh
# Runs as root on a node with the pool name as $1. Reads only the journal of the daemon's
# current invocation, so a success logged before a restart cannot hide a failure after it.
# Prints ready, refused or waiting.
set -eu
pool="${1:?pool name}"
id="$(systemctl show -p InvocationID --value spinifex-daemon)"
[ -n "$id" ] || { echo waiting; exit 0; }
log="$(journalctl "_SYSTEMD_INVOCATION_ID=$id" -u spinifex-daemon --no-pager -o cat 2>/dev/null || true)"
named() { grep -F -e "pool=$pool" -e "\"pool\":\"$pool\""; }
if printf '%s\n' "$log" | grep -F "refused the pool's first call" | named >/dev/null; then
    echo refused
elif printf '%s\n' "$log" | grep -F "floating pool ready" | named >/dev/null; then
    echo ready
else
    echo waiting
fi
