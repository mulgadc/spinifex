#!/usr/bin/env bash
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"
# Tests for hv-lock.sh: host parsing, exclusion between envs sharing a
# hypervisor, independence of envs that share none, and the timeout.
set -uo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
lock="$here/hv-lock.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export HV_LOCK_DIR="$tmp/locks"

fail=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want '$3', got '$2'"; fail=1; fi; }

cat >"$tmp/a.tfvars" <<'EOF'
cluster_name = "envA"
nodes = [
  { name = "casuarina", wan_ip = "192.168.0.22", storage_pool = "drive2-pool" },
  # { name = "banksia", wan_ip = "192.168.0.99", storage_pool = "drive1-pool" },
  { name = "bottlebrush", wan_ip = "192.168.0.23", storage_pool = "drive2-pool" },
]
EOF
cat >"$tmp/b.tfvars" <<'EOF'
nodes = [
  { name = "bottlebrush", wan_ip = "192.168.0.40", storage_pool = "drive1-pool" },
]
EOF
cat >"$tmp/c.tfvars" <<'EOF'
nodes = [
  { name = "wattle", wan_ip = "192.168.0.50", storage_pool = "drive1-pool" },
]
EOF
printf 'cluster_name = "empty"\n' >"$tmp/empty.tfvars"

out=$("$lock" "$tmp/a.tfvars" -- echo ran)
check "runs the command" "$(tail -n1 <<<"$out")" "ran"
check "locks named hosts, skips commented ones" "$(ls "$HV_LOCK_DIR" | tr '\n' ' ')" "bottlebrush.lock casuarina.lock "

"$lock" "$tmp/a.tfvars" -- false
check "returns the command's status" "$?" "1"

"$lock" "$tmp/empty.tfvars" -- true 2>/dev/null
check "refuses a file with no nodes" "$?" "2"

"$lock" "$tmp/a.tfvars" true 2>/dev/null
check "requires the -- separator" "$?" "2"

# Sourced: the lock is held by the calling shell until it exits.
# shellcheck source=hv-lock.sh
( . "$lock"; hv_lock_acquire "$tmp/c.tfvars" >/dev/null; sleep 2 ) &
sleep 0.5
HV_LOCK_TIMEOUT_S=1 "$lock" "$tmp/c.tfvars" -- true >/dev/null 2>&1
check "a sourced acquire holds the lock in its shell" "$?" "1"
wait
"$lock" "$tmp/c.tfvars" -- true >/dev/null
check "and releases it when that shell exits" "$?" "0"

# A holds bottlebrush for 2s; B shares it and must wait, C shares nothing.
"$lock" "$tmp/a.tfvars" -- sleep 2 >/dev/null &
sleep 0.5
t0=$SECONDS
HV_LOCK_TIMEOUT_S=10 "$lock" "$tmp/b.tfvars" -- true >/dev/null
check "a shared hypervisor waits" "$(( SECONDS - t0 >= 1 ))" "1"
wait

"$lock" "$tmp/a.tfvars" -- sleep 3 >/dev/null &
sleep 0.5
t0=$SECONDS
"$lock" "$tmp/c.tfvars" -- true >/dev/null
check "a disjoint env does not wait" "$(( SECONDS - t0 ))" "0"
HV_LOCK_TIMEOUT_S=1 "$lock" "$tmp/b.tfvars" -- true >/dev/null 2>&1
check "gives up after the timeout" "$?" "1"
wait

exit "$fail"
