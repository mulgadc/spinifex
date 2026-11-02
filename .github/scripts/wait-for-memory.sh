#!/usr/bin/env bash
# Usage: wait-for-memory.sh <MiB>
#
# Waits until the runner's MemAvailable reaches <MiB>, then returns. Runners on
# one host share its RAM, and a neighbour's build holds several GiB for a few
# minutes, so one sample taken during it is not a verdict on the host.
# Gives up after MEM_WAIT_TIMEOUT_S and names the biggest processes.
#
# Env: MEM_WAIT_TIMEOUT_S (900), MEM_WAIT_POLL_S (30), MEMINFO (/proc/meminfo).
set -uo pipefail

want_mb="${1:?usage: $0 <MiB>}"
timeout_s="${MEM_WAIT_TIMEOUT_S:-900}"
poll_s="${MEM_WAIT_POLL_S:-30}"
meminfo="${MEMINFO:-/proc/meminfo}"

avail_mb() { awk '/^MemAvailable:/ {print int($2 / 1024)}' "$meminfo"; }

start=$SECONDS
while :; do
  have=$(avail_mb)
  if [ -n "$have" ] && [ "$have" -ge "$want_mb" ]; then
    echo "memory: ${have} MiB available, need ${want_mb} MiB"
    exit 0
  fi
  waited=$(( SECONDS - start ))
  if [ "$waited" -ge "$timeout_s" ]; then
    echo "::error::need ${want_mb} MiB available, have ${have:-unknown} MiB after waiting ${waited}s"
    ps -eo rss,pid,args --sort=-rss | head -11 | cut -c1-200
    exit 1
  fi
  echo "memory: ${have:-unknown} MiB available, need ${want_mb} MiB; waiting (${waited}s of ${timeout_s}s)"
  sleep "$poll_s"
done
