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
CURL=(curl -fsS -k --max-time 5)

ok()   { printf '\033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$1"; exit 1; }
info() { printf '\033[36m==>\033[0m %s\n' "$1"; }

cleanup() {
    info "cleanup: removing override + restoring services"
    sudo rm -f "$OVERRIDE"
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

# 1. kill NATS → standalone, retry counter advances, daemon survives
info "1a/1c: kill NATS"
before=$(field nats_retry_count)
sudo systemctl stop "$NATS_SVC"
wait_field mode standalone 30   || fail "never went standalone (got: $(field mode))"
wait_field nats disconnected 5  || fail "nats not disconnected (got: $(field nats))"
sudo systemctl is-active --quiet "$DAEMON_SVC" || fail "daemon exited — should stay up"
info "waiting 65s for at least one NATS retry cycle"
sleep 65
after=$(field nats_retry_count)
[ "$after" -gt "$before" ] || fail "nats_retry_count did not advance ($before → $after)"
ok "standalone, retry $before → $after, daemon alive"

# 2. /local/instances reachable during partition
info "1b: /local/instances during partition"
"${CURL[@]}" "${DAEMON_URL}/local/instances" | jq -e . >/dev/null \
    || fail "/local/instances did not return JSON"
ok "/local/instances served while NATS down"

# 3. restart NATS → cluster mode + KV push observed
info "1c reconnect: restart NATS"
before_sync=$(field last_kv_sync_at)
sudo systemctl start "$NATS_SVC"
wait_field mode cluster 60   || fail "did not return to cluster mode"
wait_field nats connected 10 || fail "nats not reconnected"
sleep 5
after_sync=$(field last_kv_sync_at)
[ -n "$after_sync" ] && [ "$after_sync" != "$before_sync" ] \
    || fail "last_kv_sync_at did not advance ($before_sync → $after_sync)"
ok "cluster mode restored, KV resync at $after_sync"

# 4. SPINIFEX_REQUIRE_NATS=1 → daemon exits within ~30s when NATS down
info "1d-strict: SPINIFEX_REQUIRE_NATS=1 exits when NATS down"
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
    [ $(( $(date +%s) - start )) -gt 40 ] && fail "did not exit within 40s"
    sleep 1
done
ok "exited after $(( $(date +%s) - start ))s"

echo
echo "DDIL Phase 1 systemd integration: ALL PASS"
