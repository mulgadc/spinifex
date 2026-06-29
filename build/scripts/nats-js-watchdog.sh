#!/bin/sh
# JetStream liveness watchdog for spinifex-nats.
#
# NATS can disable JetStream at runtime (e.g. a filesystem permission error while
# flushing stream state) and keep the process alive serving KV as read-only. That
# is not a process failure, so Restart=on-failure never fires and KV-backed
# services (awsgw IAM bucket init, etc.) stall until NATS is restarted by hand.
#
# This probes the NATS monitoring endpoint's JS health and restarts the service
# only when JetStream is sustainedly unhealthy while the service is running. The
# multi-sample window avoids restarting during transient startup/quorum 503s.

set -eu

HEALTH_URL="http://127.0.0.1:8222/healthz?js-enabled-only=true"
SERVICE="spinifex-nats.service"
SAMPLES=3        # consecutive failing samples required before acting
SAMPLE_GAP=2     # seconds between samples

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

# A single 200 means JetStream is healthy — exit fast on the common case.
js_healthy() {
    curl -fsS -o /dev/null --max-time 5 "$HEALTH_URL" 2>/dev/null
}

i=1
while [ "$i" -le "$SAMPLES" ]; do
    if js_healthy; then
        exit 0
    fi
    if [ "$i" -lt "$SAMPLES" ]; then
        sleep "$SAMPLE_GAP"
    fi
    i=$((i + 1))
done

echo "nats-js-watchdog: JetStream unhealthy on $SAMPLES consecutive probes ($HEALTH_URL); restarting $SERVICE" >&2
systemctl try-restart "$SERVICE"
