#!/usr/bin/env bash
# start-net-capture.sh — kick off background tcpdump on br-wan.
# Pair: capture-net-diag.sh stops it and copies the pcap into net-diag/.
#
# Filter: anything on br-wan involving the EIP pool (192.168.0.0/23) plus
# ARP for the same range. Single ring buffer (-W 1 -G 0) → grows until stop;
# capped at 64 MiB via -C 64. Snaplen 200 bytes (headers + a bit) to keep
# size sane while preserving L2/L3/L4 + first payload bytes.
set +e

PCAP=/tmp/spinifex-e2e-wan.pcap
PIDFILE=/tmp/spinifex-e2e-tcpdump.pid

if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE" 2>/dev/null)" 2>/dev/null; then
    echo "[start-net-capture] tcpdump already running pid=$(cat "$PIDFILE")"
    exit 0
fi

sudo rm -f "$PCAP" "$PIDFILE"

if ! command -v tcpdump >/dev/null 2>&1; then
    echo "[start-net-capture] tcpdump not installed — skipping" >&2
    exit 0
fi
if ! ip link show br-wan >/dev/null 2>&1; then
    echo "[start-net-capture] br-wan absent — skipping" >&2
    exit 0
fi

sudo nohup tcpdump -i br-wan -s 200 -C 64 -W 1 -U \
    -w "$PCAP" \
    '(net 192.168.0.0/23) or (arp and (arp[24:4]&0xfffffe00=0xc0a80000))' \
    >/tmp/spinifex-e2e-tcpdump.log 2>&1 &
PID=$!
echo "$PID" | sudo tee "$PIDFILE" >/dev/null
disown

sleep 1
if ! kill -0 "$PID" 2>/dev/null; then
    echo "[start-net-capture] tcpdump died — see /tmp/spinifex-e2e-tcpdump.log" >&2
    cat /tmp/spinifex-e2e-tcpdump.log >&2 || true
    sudo rm -f "$PIDFILE"
    exit 1
fi

echo "[start-net-capture] tcpdump pid=$PID pcap=$PCAP"
exit 0
