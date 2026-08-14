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
#
# Three more guards sit around that decision, because "restart when the
# probes fail" is not safe on its own:
#
#   - Age gate: spinifex-nats.service is Type=simple, so it reports active the
#     instant the process forks, long before JetStream finishes replaying its
#     state. Acting before a recovery budget has elapsed restarts a server
#     that is still coming up, not one that has failed.
#   - Cooldown: a restart that did not fix things must not be retried by the
#     very next run, seconds later.
#   - Escalation: a restart that has not fixed things after several attempts
#     is not going to fix it on the next one either, and an unrunnable spx
#     must never be read as proof of a JetStream failure at all.

set -eu

HEALTH_URL="http://127.0.0.1:8222/healthz?js-enabled-only=true"
SERVICE="spinifex-nats.service"
SPX="${SPX_BIN:-/usr/local/bin/spx}"
SAMPLES="${SAMPLES:-3}"           # consecutive failing samples required before acting
SAMPLE_GAP="${SAMPLE_GAP:-2}"     # seconds between samples
PROBE_TIMEOUT="${PROBE_TIMEOUT:-5}"  # seconds for one canary round-trip

# Recovery budget: on casuarina NATS needed 3m49s between "Started" and
# JetStream actually accepting writes, and the bead recorded ~6min elsewhere.
# 600s clears both with margin.
NATS_MIN_AGE="${NATS_MIN_AGE:-600}"

# Minimum time between restarts this watchdog performs, independent of the
# age gate: closes the case where a restart did not fix things and the very
# next run pulls the trigger again before the new process could recover.
RESTART_COOLDOWN="${RESTART_COOLDOWN:-600}"

# A restart that has not fixed things after this many attempts is not going
# to fix it on the next one either, and looping costs more than stopping.
RESTART_ESCALATE_AT="${RESTART_ESCALATE_AT:-3}"

STATE_DIR="${STATE_DIR:-/run/spinifex}"
COOLDOWN_STAMP="$STATE_DIR/nats-js-watchdog.last-restart"
RESTART_COUNT_FILE="$STATE_DIR/nats-js-watchdog.restart-count"

# Monotonic clock source, so a wall-clock step during boot (NTP correcting an
# unset RTC) cannot manufacture a false age or cooldown. Overridable so tests
# can stub a clock without faking /proc.
UPTIME_FILE="${UPTIME_FILE:-/proc/uptime}"

uptime_seconds() {
    awk '{printf "%d", $1}' "$UPTIME_FILE"
}

# nats_age_seconds prints how long $SERVICE has been running. It reads
# ExecMainStartTimestampMonotonic rather than is-active: that property goes
# true the instant Type=simple forks, long before JetStream has replayed its
# state, so it cannot express the recovery window this gate exists to
# protect. A unit systemd has never started prints 0, which also covers a
# stopped/activating unit the way the old is-active guard did.
nats_age_seconds() {
    start_usec=$(systemctl show --property=ExecMainStartTimestampMonotonic --value "$SERVICE" 2>/dev/null) || start_usec=""
    case "$start_usec" in
        ''|*[!0-9]*) echo 0; return ;;
    esac
    if [ "$start_usec" -eq 0 ]; then
        echo 0
        return
    fi
    echo $(( $(uptime_seconds) - start_usec / 1000000 ))
}

age=$(nats_age_seconds)
if [ "$age" -lt "$NATS_MIN_AGE" ]; then
    exit 0
fi

if [ -f "$COOLDOWN_STAMP" ]; then
    last=$(cat "$COOLDOWN_STAMP" 2>/dev/null) || last=""
    case "$last" in ''|*[!0-9]*) last=0 ;; esac
    if [ $(( $(uptime_seconds) - last )) -lt "$RESTART_COOLDOWN" ]; then
        exit 0
    fi
fi

restart_count=$(cat "$RESTART_COUNT_FILE" 2>/dev/null) || restart_count=""
case "$restart_count" in ''|*[!0-9]*) restart_count=0 ;; esac
if [ "$restart_count" -ge "$RESTART_ESCALATE_AT" ]; then
    echo "nats-js-watchdog: already restarted $SERVICE $restart_count time(s) without recovery; giving up rather than looping (clear $RESTART_COUNT_FILE to re-arm)" >&2
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

# Round-trips a canary key through the cluster-state bucket. Exit 0 means the
# write succeeded and exit 1 means JetStream refused it; anything else
# (spx's own exit 2, a missing binary, or one that cannot run at all) means
# the probe observed nothing about JetStream and must never be read as
# evidence of a failure.
js_writable() {
    if [ ! -x "$SPX" ]; then
        return 2
    fi
    "$SPX" admin node js-probe --timeout "${PROBE_TIMEOUT}s" >/dev/null 2>&1
    rc=$?
    case "$rc" in
        0) return 0 ;;
        1) return 1 ;;
        *) return 2 ;;
    esac
}

failure=""
i=1
while [ "$i" -le "$SAMPLES" ]; do
    if js_healthy; then
        if js_writable; then
            wrc=0
        else
            wrc=$?
        fi
        if [ "$wrc" -eq 0 ]; then
            rm -f "$RESTART_COUNT_FILE"
            exit 0
        fi
        if [ "$wrc" -eq 2 ]; then
            echo "nats-js-watchdog: js-probe inconclusive (config, connect, bucket-open, or spx itself failed); not evidence of a JetStream failure, taking no action" >&2
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
mkdir -p "$STATE_DIR"
uptime_seconds > "$COOLDOWN_STAMP"
restart_count=$((restart_count + 1))
echo "$restart_count" > "$RESTART_COUNT_FILE"
if [ "$restart_count" -ge "$RESTART_ESCALATE_AT" ]; then
    echo "nats-js-watchdog: restart $restart_count of $RESTART_ESCALATE_AT before escalation; if $SERVICE is still failing after this, the watchdog will stop acting until $RESTART_COUNT_FILE is cleared" >&2
fi
systemctl try-restart "$SERVICE"
