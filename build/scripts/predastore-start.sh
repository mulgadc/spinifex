#!/bin/sh
# Helper script for spinifex-predastore.service
# Detects host, port, backend, and node-id from spinifex.toml
# then execs spx. Falls back to single-node defaults if no config found.

CONF=/etc/spinifex/spinifex.toml
BIND="0.0.0.0:8443"
# -1 = co-located mode (all [[db]] peers in one process). Single-node spinifex
# templates list 3 DB peers on 127.0.0.1; only -1 can serve that topology.
# Multi-node spinifex.toml emits node_id = N (N >= 1) which overrides below.
# predastore rejects NODE_ID=0 since d3a02f0 (strconv.Atoi-of-garbage guard).
NODE_ID="-1"

if [ -f "$CONF" ]; then
    H=$(awk -F'"' '/\[nodes\..*\.predastore\]/{f=1} f&&/^host/{print $2;exit}' "$CONF")
    if [ -n "$H" ]; then
        BIND="$H"
    fi

    N=$(awk -F'= *' '/node_id/{gsub(/ /,"",$2);print $2;exit}' "$CONF")
    if [ -n "$N" ]; then
        NODE_ID="$N"
    fi
fi

export SPINIFEX_PREDASTORE_HOST="${BIND%%:*}"
export SPINIFEX_PREDASTORE_PORT="${BIND##*:}"
export SPINIFEX_PREDASTORE_NODE_ID="$NODE_ID"

exec /usr/local/bin/spx service predastore start
