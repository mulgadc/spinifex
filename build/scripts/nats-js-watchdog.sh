#!/bin/sh
# JetStream liveness watchdog for spinifex-nats.
#
# NATS can stop accepting JetStream writes at runtime while the process stays
# alive and keeps serving reads. That is not a process failure, so
# Restart=on-failure never fires and KV-backed services (awsgw IAM bucket init,
# etc.) stall until NATS is restarted by hand.
#
# Two distinct latches produce that symptom, and they need different probes:
#
#   1. Server-wide. handleOutOfSpace calls ShutdownJetStream and sets an
#      internal disabled flag that only a restart clears. healthz reports it.
#   2. Per message block. filestore records a write error on the block and
#      returns it on every later write; nothing clears it short of a restart.
#      healthz does NOT report it, because the server-wide state is untouched —
#      only an actual write observes it.
#
# So a health check alone misses case 2 entirely. Both probes run here, and a
# sustained failure of either restarts the service. The multi-sample window
# avoids restarting during transient startup/quorum 503s.

set -eu

HEALTH_URL="http://127.0.0.1:8222/healthz?js-enabled-only=true"
SERVICE="spinifex-nats.service"
SPX="${SPX_BIN:-/usr/local/bin/spx}"
SAMPLES=3        # consecutive failing samples required before acting
SAMPLE_GAP=2     # seconds between samples
PROBE_TIMEOUT=5  # seconds for one canary round-trip

# Only act on a running service: a stopped/activating unit is systemd's to manage,
# and try-restart on it would either no-op or fight an in-flight (re)start.
if ! systemctl is-active --quiet "$SERVICE"; then
    exit 0
fi

# curl is the probe transport; if it is unavailable, do nothing rather than
# false-restart a healthy server.
if ! command -v curl >/dev/null 2>&1; then
    echo "nats-js-watchdog: curl not found, skipping JS health probe" >&2
    exit 0
fi

js_healthy() {
    curl -fsS -o /dev/null --max-time 5 "$HEALTH_URL" 2>/dev/null
}

# Round-trips a canary key through the cluster-state bucket. Absent spx this
# degrades to the health check alone rather than restarting on a missing tool.
js_writable() {
    if [ ! -x "$SPX" ]; then
        return 0
    fi
    "$SPX" admin node js-probe --timeout "${PROBE_TIMEOUT}s" >/dev/null 2>&1
}

failure=""
i=1
while [ "$i" -le "$SAMPLES" ]; do
    if js_healthy; then
        if js_writable; then
            exit 0
        fi
        failure="JetStream accepts no writes (canary round-trip failed) while $HEALTH_URL reports healthy"
    else
        failure="JetStream unhealthy at $HEALTH_URL"
    fi
    if [ "$i" -lt "$SAMPLES" ]; then
        sleep "$SAMPLE_GAP"
    fi
    i=$((i + 1))
done

echo "nats-js-watchdog: $failure on $SAMPLES consecutive probes; restarting $SERVICE" >&2
systemctl try-restart "$SERVICE"
