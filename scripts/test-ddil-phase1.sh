#!/bin/bash
# test-ddil-phase1.sh — systemd-level smoke test for daemon-local-autonomy
# Tier 1 (1a / 1b / 1c / 1d / 1d-strict). Run after `make deploy` on a single
# node where spinifex-daemon and spinifex-nats are managed by systemd.
#
# Usage: scripts/test-ddil-phase1.sh
# Requires: sudo, curl, jq
#
# Destructive on the local node — stops/starts spinifex-nats and restarts
# spinifex-daemon. Do NOT run against a host carrying workloads. The cleanup
# trap restores both services on exit.

set -euo pipefail

DAEMON_URL="${DAEMON_URL:-https://127.0.0.1:4432}"
DAEMON_SVC=spinifex-daemon
NATS_SVC=spinifex-nats
OVERRIDE=/etc/systemd/system/${DAEMON_SVC}.service.d/99-test-ddil.conf
NATS_BLOCK=/run/spinifex-nats-blocked
NATS_BLOCK_DROPIN=/run/systemd/system/${NATS_SVC}.service.d/99-test-block.conf
CURL=(curl -fsS -k --max-time 5)

ok()   { printf '\033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$1"; exit 1; }
info() { printf '\033[36m==>\033[0m %s\n' "$1"; }

cleanup() {
    info "cleanup: removing nats block, override, restoring services"
    sudo rm -f "$NATS_BLOCK" "$NATS_BLOCK_DROPIN" "$OVERRIDE"
    sudo rmdir "$(dirname "$NATS_BLOCK_DROPIN")" 2>/dev/null || true
    sudo systemctl daemon-reload
    sudo systemctl start "$NATS_SVC"     2>/dev/null || true
    sudo systemctl restart "$DAEMON_SVC" 2>/dev/null || true
}
trap cleanup EXIT

field() { "${CURL[@]}" "${DAEMON_URL}/local/status" | jq -r ".$1 // empty"; }

wait_field() {
    local f=$1 want=$2 timeout=${3:-30} elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        [ "$(field "$f" 2>/dev/null || true)" = "$want" ] && return 0
        sleep 1; elapsed=$((elapsed + 1))
    done
    return 1
}

# 0. baseline
info "baseline: daemon active + cluster mode"
sudo systemctl is-active --quiet "$DAEMON_SVC" || fail "daemon not running — run \`make deploy\` first"
sudo systemctl start "$NATS_SVC"
wait_field mode cluster 30 || fail "never reached mode=cluster (got: $(field mode))"
[ "$(field nats)" = connected ] || fail "nats not connected (got: $(field nats))"
ok "mode=cluster nats=connected"

# 1. kill NATS → standalone, daemon survives
info "1a/1c: kill NATS"
retry_before=$(field nats_retry_count)
sudo systemctl stop "$NATS_SVC"
wait_field mode standalone 30   || fail "never went standalone (got: $(field mode))"
wait_field nats disconnected 5  || fail "nats not disconnected (got: $(field nats))"
sudo systemctl is-active --quiet "$DAEMON_SVC" || fail "daemon exited — should stay up"
ok "standalone, nats=disconnected, daemon alive"

# 2. /local/instances reachable during partition
info "1b: /local/instances during partition"
"${CURL[@]}" "${DAEMON_URL}/local/instances" | jq -e . >/dev/null \
    || fail "/local/instances did not return JSON"
ok "/local/instances served while NATS down"

# 3. restart NATS → cluster mode + retry count advances + KV push observed
# nats_retry_count counts disconnect→reconnect *cycles* (bumped from
# onNATSReconnect, daemon.go:321). It stays flat during disconnect and
# advances by 1 here when the reconnect callback fires.
info "1c reconnect: restart NATS"
before_sync=$(field last_kv_sync_at)
sudo systemctl start "$NATS_SVC"
wait_field mode cluster 60   || fail "did not return to cluster mode"
wait_field nats connected 10 || fail "nats not reconnected"
sleep 5
retry_after=$(field nats_retry_count)
[ "$retry_after" -gt "$retry_before" ] \
    || fail "nats_retry_count did not advance on reconnect ($retry_before → $retry_after)"
after_sync=$(field last_kv_sync_at)
[ -n "$after_sync" ] && [ "$after_sync" != "$before_sync" ] \
    || fail "last_kv_sync_at did not advance ($before_sync → $after_sync)"
ok "cluster restored, retry $retry_before → $retry_after, KV resync at $after_sync"

# 4. SPINIFEX_REQUIRE_NATS=1 → daemon exits within ~30s when NATS down.
# Block spinifex-nats via a runtime drop-in with ConditionPathExists=!. This
# defeats the daemon unit's Wants=spinifex-nats.service auto-start, which
# would otherwise pull NATS back up on `systemctl restart spinifex-daemon`
# and let the daemon connect immediately. mask refuses (real unit lives in
# /etc) and --runtime mask is shadowed by /etc — the condition drop-in is
# the only mechanism that works against an /etc-installed unit.
info "1d-strict: SPINIFEX_REQUIRE_NATS=1 exits when NATS down"
sudo mkdir -p "$(dirname "$NATS_BLOCK_DROPIN")"
sudo tee "$NATS_BLOCK_DROPIN" >/dev/null <<EOF
[Unit]
ConditionPathExists=!$NATS_BLOCK
EOF
sudo touch "$NATS_BLOCK"
sudo systemctl daemon-reload
sudo systemctl stop "$NATS_SVC"
sudo mkdir -p "$(dirname "$OVERRIDE")"
sudo tee "$OVERRIDE" >/dev/null <<EOF
[Service]
Environment=SPINIFEX_REQUIRE_NATS=1
Restart=no
EOF
sudo systemctl daemon-reload
sudo systemctl restart "$DAEMON_SVC" || true
start=$(date +%s)
while sudo systemctl is-active --quiet "$DAEMON_SVC"; do
    [ $(( $(date +%s) - start )) -gt 45 ] && fail "did not exit within 45s"
    sleep 1
done
ok "exited after $(( $(date +%s) - start ))s"

echo
echo "DDIL Phase 1 systemd integration: ALL PASS"
