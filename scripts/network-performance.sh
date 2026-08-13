#!/bin/bash
# network-performance.sh — iperf throughput from several clients to one server.
#
# Runs from a workstation and drives both ends over SSH. Pointed at instance
# private addresses it measures the VPC data plane (Geneve overlay); pointed at
# node addresses it measures the underlay instead.
#
# Usage:
#   network-performance.sh --server IP --clients IP,IP,IP [options]
#
# Options:
#   --server HOST    Host to SSH to and run `iperf -s` on
#   --server-ip IP   Address the clients dial (default: --server)
#   --clients LIST   Comma-separated hosts to SSH to and run `iperf -c` on
#   --user NAME      SSH user for every host (default: ubuntu)
#   --key PATH       SSH identity
#   --parallel N     Streams per client (default: 4)
#   --time N         Seconds per client (default: 60)
#   --out DIR        Results directory (default: /tmp/spinifex-network-bench)
#
# --server-ip exists because the address that carries the traffic is not the
# address that carries the SSH session. To measure a VPC overlay, SSH reaches
# the instances on their public addresses while the clients must dial the
# server's private one — dialling the public address would leave the VPC,
# traverse the external pool, and measure something else entirely.
set -euo pipefail

SERVER=""
SERVER_IP=""
CLIENTS=""
SSH_USER="ubuntu"
SSH_KEY=""
PARALLEL=4
DURATION=60
OUT_DIR="/tmp/spinifex-network-bench"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --server)    SERVER="$2"; shift 2 ;;
        --server-ip) SERVER_IP="$2"; shift 2 ;;
        --clients)   CLIENTS="$2"; shift 2 ;;
        --user)     SSH_USER="$2"; shift 2 ;;
        --key)      SSH_KEY="$2"; shift 2 ;;
        --parallel) PARALLEL="$2"; shift 2 ;;
        --time)     DURATION="$2"; shift 2 ;;
        --out)      OUT_DIR="$2"; shift 2 ;;
        -h|--help)  sed -n '2,25p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *)          echo "ERROR: unknown option: $1" >&2; exit 2 ;;
    esac
done

log()  { echo "[netperf] $*"; }
fail() { echo "[netperf] ERROR: $*" >&2; exit 1; }

[ -n "$SERVER" ]  || fail "--server is required"
[ -n "$CLIENTS" ] || fail "--clients is required"
SERVER_IP="${SERVER_IP:-$SERVER}"

SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
          -o LogLevel=ERROR -o ConnectTimeout=10)
[ -n "$SSH_KEY" ] && SSH_OPTS+=(-i "$SSH_KEY")

on() { ssh "${SSH_OPTS[@]}" "$SSH_USER@$1" "${@:2}"; }

IFS=',' read -ra CLIENT_LIST <<<"$CLIENTS"
mkdir -p "$OUT_DIR"

# iperf 2, which listens on 5001. `-y C` gives CSV, so the result is still
# machine-readable for a nightly comparison without scraping prose.
install_iperf() {
    local err
    err=$(on "$1" "command -v iperf >/dev/null ||
        { sudo apt-get update -qq && sudo apt-get install -y -qq iperf; }" 2>&1) ||
        fail "$1: could not install iperf
$(sed 's/^/         | /' <<<"$err")"
}

log "installing iperf on $SERVER and ${#CLIENT_LIST[@]} client(s)"
install_iperf "$SERVER"
for c in "${CLIENT_LIST[@]}"; do
    install_iperf "$c"
done

# -D daemonises. Killed in the trap so a failed run does not leave a listener
# holding 5001 against the next one.
cleanup() { on "$SERVER" "sudo pkill -f 'iperf -s' >/dev/null 2>&1" || true; }
trap cleanup EXIT

log "starting iperf -s on $SERVER"
# The remote output is carried into the failure rather than discarded. A bare
# "could not start" says nothing about whether the package is missing, the port
# is taken, or sudo refused.
start_err=$(on "$SERVER" "sudo pkill -f 'iperf -s' >/dev/null 2>&1
                          iperf -s -D" 2>&1) ||
    fail "$SERVER: could not start iperf -s
$(sed 's/^/         | /' <<<"$start_err")"
sleep 2

log "running $PARALLEL streams for ${DURATION}s from each client to $SERVER_IP, concurrently"
pids=()
for c in "${CLIENT_LIST[@]}"; do
    # Concurrently, because simultaneous clients are what stresses the plane.
    # One at a time would measure a single flow's best case.
    on "$c" "iperf -c $SERVER_IP -P $PARALLEL -t $DURATION -i 1 -y C" \
        > "$OUT_DIR/$c.csv" 2>"$OUT_DIR/$c.err" &
    pids+=($!)
done

failed=0
for pid in "${pids[@]}"; do
    wait "$pid" || failed=1
done

# CSV columns: timestamp,src,sport,dst,dport,id,interval,bytes,bits_per_second.
# With -P the aggregate rows carry id -1; taking the last of them gives the
# whole-run total rather than one interval. A single stream emits no such row,
# so fall back to the last row of all.
csv_bps() {
    local bps
    bps=$(awk -F, '$6 == -1 {v = $9} END {print v + 0}' "$1" 2>/dev/null)
    [ "${bps:-0}" != "0" ] || bps=$(awk -F, 'END {print $9 + 0}' "$1" 2>/dev/null)
    echo "${bps:-0}"
}

for c in "${CLIENT_LIST[@]}"; do
    gbits=$(awk -v b="$(csv_bps "$OUT_DIR/$c.csv")" \
        'BEGIN {printf "%.2f", b / 1000000000}')
    if [ "$gbits" = "0.00" ]; then
        log "  $c -> $SERVER_IP   FAILED"
        sed 's/^/           | /' "$OUT_DIR/$c.err" 2>/dev/null | head -3
        failed=1
    else
        log "  $c -> $SERVER_IP   $gbits Gbit/s"
    fi
done

[ "$failed" -eq 0 ] || fail "at least one client did not complete"
log "results in $OUT_DIR"
