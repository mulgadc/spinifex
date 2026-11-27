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
#   --expect-queues N  Fail unless every guest's primary NIC reports Combined N,
#                    current and maximum, in ethtool -l (default: record only)
#
# Besides TCP throughput it records, per guest, the primary NIC's queue pairs
# (queues.txt), each client's TCP retransmits over the run (retrans.txt), a
# 64-byte UDP run's receive rate and loss (udp.txt), and ping RTT p50 and p99
# to the server while iperf is idle (latency.txt).
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
EXPECT_QUEUES=""
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
        --expect-queues) EXPECT_QUEUES="$2"; shift 2 ;;
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

# The primary NIC is the default route's device. ethtool -l prints Combined
# twice: the maximum, then the current setting.
queues_of() {
    on "$1" 'command -v ethtool >/dev/null || sudo apt-get install -y -qq ethtool >/dev/null
             nic=$(ip -o route show default | awk "{print \$5; exit}")
             printf "%s " "$nic"; sudo ethtool -l "$nic" | awk "/^Combined/ {printf \"%s \", \$2}"'
}

# Absolute TCP retransmitted segments, from the kernel's SNMP counters.
retrans_of() { on "$1" "nstat -az TcpRetransSegs | awk '/TcpRetransSegs/ {print \$2}'"; }

log "installing iperf on $SERVER and ${#CLIENT_LIST[@]} client(s)"
install_iperf "$SERVER"
for c in "${CLIENT_LIST[@]}"; do
    install_iperf "$c"
done

# `pkill -x iperf` matches the process name. NOT `pkill -f 'iperf -s'`: -f
# matches the whole command line, and the command line of the remote shell
# running the pkill contains that very string — so it kills its own process
# tree, and ssh returns non-zero having printed nothing at all.
cleanup() { on "$SERVER" "sudo pkill -x iperf >/dev/null 2>&1" || true; }
trap cleanup EXIT

log "starting iperf -s on $SERVER"
# Redirecting the daemon's descriptors matters: a backgrounded remote process
# holding ssh's stdout open never lets ssh return.
start_err=$(on "$SERVER" "sudo pkill -x iperf >/dev/null 2>&1
                          iperf -s -D > /tmp/iperf-server.log 2>&1 < /dev/null" 2>&1) ||
    fail "$SERVER: could not start iperf -s${start_err:+
$(sed 's/^/         | /' <<<"$start_err")}"
sleep 2

# A positive control. `iperf -s -D` exiting 0 does not mean anything is
# listening, and a server that silently did not start looks identical to one
# that did until every client fails for no stated reason.
on "$SERVER" "pgrep -x iperf >/dev/null" ||
    fail "$SERVER: iperf -s reported success but no iperf process is running.
$(on "$SERVER" "cat /tmp/iperf-server.log" 2>/dev/null | sed 's/^/         | /')"

: > "$OUT_DIR/queues.txt"
for g in "$SERVER" "${CLIENT_LIST[@]}"; do
    read -r nic qmax qcur <<<"$(queues_of "$g" 2>/dev/null)"
    printf '%s\t%s\tmax=%s\tcurrent=%s\n' "$g" "${nic:-?}" "${qmax:-?}" "${qcur:-?}" >> "$OUT_DIR/queues.txt"
    log "  $g ${nic:-?} queue pairs: max ${qmax:-?}, current ${qcur:-?}"
    if [ -n "$EXPECT_QUEUES" ] && { [ "$qmax" != "$EXPECT_QUEUES" ] || [ "$qcur" != "$EXPECT_QUEUES" ]; }; then
        fail "$g: primary NIC has max ${qmax:-?}, current ${qcur:-?} queue pairs, expected $EXPECT_QUEUES"
    fi
done

declare -A RETRANS_BEFORE
for c in "${CLIENT_LIST[@]}"; do
    RETRANS_BEFORE[$c]=$(retrans_of "$c" 2>/dev/null)
done

log "running $PARALLEL streams for ${DURATION}s from each client to $SERVER_IP, concurrently"
pids=()
for c in "${CLIENT_LIST[@]}"; do
    # Concurrently, because simultaneous clients are what stresses the plane.
    # One at a time would measure a single flow's best case.
    on "$c" "iperf -c $SERVER_IP -P $PARALLEL -t $DURATION -i 1" \
        > "$OUT_DIR/$c.txt" 2>"$OUT_DIR/$c.err" &
    pids+=($!)
done

failed=0
for pid in "${pids[@]}"; do
    wait "$pid" || failed=1
done

# Every result line ends with a value and a unit, so read from the end rather
# than by field number: "[SUM]" is one token and "[  4]" is two, so counting
# from the left gives different columns for the aggregate and a single stream.
# Prefer [SUM]; one stream produces none, so fall back to the last line.
gbits_of() {
    awk '/bits\/sec/ {
             v = $(NF-1); u = $NF
             if ($0 ~ /\[SUM\]/) { sv = v; su = u }
         }
         END {
             if (sv != "") { v = sv; u = su }
             m = (u ~ /^Gbits/) ? 1e9 : (u ~ /^Mbits/) ? 1e6 : (u ~ /^Kbits/) ? 1e3 : 0
             printf "%.2f", v * m / 1e9
         }' "$1" 2>/dev/null
}

# One parser, written down once. test-workload.sh reads this file rather than
# re-deriving the number and getting a subtly different answer.
: > "$OUT_DIR/summary.txt"
for c in "${CLIENT_LIST[@]}"; do
    gbits=$(gbits_of "$OUT_DIR/$c.txt")
    if [ -z "$gbits" ] || [ "$gbits" = "0.00" ]; then
        log "  $c -> $SERVER_IP   FAILED"
        head -3 "$OUT_DIR/$c.err" 2>/dev/null | sed 's/^/           | /'
        failed=1
    else
        printf '%s\t%s\n' "$c" "$gbits" >> "$OUT_DIR/summary.txt"
        log "  $c -> $SERVER_IP   $gbits Gbit/s"
    fi
done

[ "$failed" -eq 0 ] || fail "at least one client did not complete"

: > "$OUT_DIR/retrans.txt"
for c in "${CLIENT_LIST[@]}"; do
    after=$(retrans_of "$c" 2>/dev/null)
    delta=$(( ${after:-0} - ${RETRANS_BEFORE[$c]:-0} ))
    printf '%s\t%s\n' "$c" "$delta" >> "$OUT_DIR/retrans.txt"
    log "  $c TCP retransmits over the run: $delta"
done

# Small packets, one client at a time so each reading is that client's own.
# The rate and loss are the server's receive-side report, not the sender's.
cleanup
on "$SERVER" "iperf -s -u -e -D > /tmp/iperf-udp-server.log 2>&1 < /dev/null" ||
    fail "$SERVER: could not start iperf -s -u"
sleep 2
: > "$OUT_DIR/udp.txt"
for c in "${CLIENT_LIST[@]}"; do
    on "$c" "iperf -c $SERVER_IP -u -e -l 64 -b 10G -t 10" > "$OUT_DIR/$c.udp.txt" 2>&1 || true
    # The client prints the server's report last; take its pps and loss.
    read -r pps loss <<<"$(awk '/Server Report/ {f=1; next}
        f && /pps/ { for (i = 1; i <= NF; i++) { if ($i == "pps") p = $(i-1); if ($i ~ /^\(.*%\)$/) l = $i } }
        END { gsub(/[()%]/, "", l); print p, l }' "$OUT_DIR/$c.udp.txt")"
    printf '%s\t%s pps\tloss %s%%\n' "$c" "${pps:-?}" "${loss:-?}" >> "$OUT_DIR/udp.txt"
    log "  $c -> $SERVER_IP UDP 64 B: ${pps:-?} pps received, ${loss:-?}% lost"
done
cleanup

# RTT with iperf idle. ping -q would hide the per-packet times p50 and p99 need.
: > "$OUT_DIR/latency.txt"
for c in "${CLIENT_LIST[@]}"; do
    on "$c" "ping -c 1000 -i 0.01 $SERVER_IP" > "$OUT_DIR/$c.ping.txt" 2>&1 || true
    read -r p50 p99 <<<"$(grep -o 'time=[0-9.]*' "$OUT_DIR/$c.ping.txt" | cut -d= -f2 | sort -n |
        awk '{v[NR] = $1} END { if (NR) { a = int((NR + 1) / 2); b = int(NR * 0.99); if (b < 1) b = 1; print v[a], v[b] } }')"
    printf '%s\tp50 %s ms\tp99 %s ms\n' "$c" "${p50:-?}" "${p99:-?}" >> "$OUT_DIR/latency.txt"
    log "  $c -> $SERVER_IP RTT p50 ${p50:-?} ms, p99 ${p99:-?} ms"
done

log "results in $OUT_DIR"
