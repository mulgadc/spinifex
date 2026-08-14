#!/bin/sh
# Wait for NATS to be reachable before starting a dependent service.
# Reads the NATS address from spinifex.toml [nodes.*.nats] section,
# falls back to nats.conf, then to 127.0.0.1:4222.

CONF=/etc/spinifex/spinifex.toml
NATS_CONF=/etc/spinifex/nats/nats.conf
NATS_HOST=""
NATS_PORT="4222"
TIMEOUT=30

# Try spinifex.toml first
if [ -f "$CONF" ]; then
    NATS_BIND=$(awk -F'"' '/\[nodes\..*\.nats\]/{f=1} f&&/^host/{print $2;exit}' "$CONF")
    if [ -n "$NATS_BIND" ]; then
        NATS_HOST="${NATS_BIND%%:*}"
        NATS_PORT="${NATS_BIND##*:}"
    fi
fi

# Fall back to nats.conf
if [ -z "$NATS_HOST" ] && [ -f "$NATS_CONF" ]; then
    NATS_HOST=$(grep "^listen:" "$NATS_CONF" | sed 's/listen: *//;s/:.*//')
fi

# Final fallback
NATS_HOST="${NATS_HOST:-127.0.0.1}"

# shellcheck disable=SC2034  # loop count only, index unused
for i in $(seq 1 "$TIMEOUT"); do
    if nc -z "$NATS_HOST" "$NATS_PORT" 2>/dev/null; then
        exit 0
    fi
    sleep 1
done

echo "Timed out waiting for NATS at $NATS_HOST:$NATS_PORT" >&2
exit 1
