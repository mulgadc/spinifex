#!/usr/bin/env bash
# Usage: hv-lock.sh <env.tfvars> -- <command> [args...]
#    or: . hv-lock.sh; hv_lock_acquire <env.tfvars>   (held until the shell exits)
#
# Holds an exclusive lock on every hypervisor the tofu env names. Concurrent
# destroy, scrub and apply against one libvirtd drop its connections, so
# provisioning is serialised per hypervisor, not per env.
#
# Locks are files on the runner host, so they only exclude runners that share
# it; every e2e runner lives on one host. Hosts are locked in sorted order, so
# two envs sharing hypervisors cannot deadlock.
#
# Env: HV_LOCK_DIR ($HOME/.cache/spinifex-ci/hv-locks), HV_LOCK_TIMEOUT_S (1800).

# hv_lock_hosts <env.tfvars>: the distinct node names, sorted, one per line.
hv_lock_hosts() {
  grep -v '^[[:space:]]*#' "$1" |
    sed -nE 's/.*\{[[:space:]]*name[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' | sort -u
}

# hv_lock_acquire <env.tfvars>: locks every host, or returns 1 on timeout and
# 2 on a file it cannot use. The descriptors stay open in the calling shell.
hv_lock_acquire() {
  local tfvars="$1" dir="${HV_LOCK_DIR:-$HOME/.cache/spinifex-ci/hv-locks}"
  local timeout_s="${HV_LOCK_TIMEOUT_S:-1800}" hosts host fd start remaining
  if [ ! -r "$tfvars" ]; then
    echo "::error::hv-lock: cannot read $tfvars" >&2
    return 2
  fi
  hosts=$(hv_lock_hosts "$tfvars")
  if [ -z "$hosts" ]; then
    echo "::error::hv-lock: no node names in $tfvars; refusing to run unlocked" >&2
    return 2
  fi
  mkdir -p "$dir" || { echo "::error::hv-lock: cannot create $dir" >&2; return 2; }
  start=$SECONDS
  for host in $hosts; do
    exec {fd}>"$dir/$host.lock" || { echo "::error::hv-lock: cannot open $dir/$host.lock" >&2; return 2; }
    if ! flock -n "$fd"; then
      echo "hv-lock: waiting for $host"
      remaining=$(( timeout_s - (SECONDS - start) ))
      if [ "$remaining" -le 0 ] || ! flock -w "$remaining" "$fd"; then
        echo "::error::hv-lock: $host still locked after ${timeout_s}s; another job is provisioning on it" >&2
        return 1
      fi
    fi
  done
  echo "hv-lock: holding $(tr '\n' ' ' <<<"$hosts")(waited $(( SECONDS - start ))s)"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -uo pipefail
  usage() { echo "usage: $0 <env.tfvars> -- <command> [args...]" >&2; exit 2; }
  [ $# -ge 3 ] && [ "$2" = "--" ] || usage
  tfvars="$1"
  shift 2
  hv_lock_acquire "$tfvars" || exit $?
  "$@"
fi
