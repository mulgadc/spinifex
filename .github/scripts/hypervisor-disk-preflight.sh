#!/usr/bin/env bash
# Usage: hypervisor-disk-preflight.sh <env.tfvars>
#
# Fails fast when a hypervisor named in the tofu env is short of disk, before
# `tofu apply` tries to define a domain there. libvirt writes each domain's XML
# under /etc/libvirt/qemu on the root filesystem and its disks into the pool, and
# a full one fails the apply minutes in as "Domain Creation Failed".
#
# Hosts are reached the way the libvirt provider reaches them (ssh <user>@<name>,
# resolved through ~/.ssh/config). A host that cannot be reached is reported and
# skipped: tofu's own error for it is clearer than anything said here.
#
# Env: HV_SSH_USER (tf-user), HV_MIN_ROOT_FREE_MB (1024), HV_MIN_POOL_FREE_GB (20).
set -uo pipefail

tfvars="${1:?usage: $0 <env.tfvars>}"
user="${HV_SSH_USER:-tf-user}"
min_root_kb=$(( ${HV_MIN_ROOT_FREE_MB:-1024} * 1024 ))
min_pool_kb=$(( ${HV_MIN_POOL_FREE_GB:-20} * 1024 * 1024 ))

if [ ! -r "$tfvars" ]; then
  echo "::error::hypervisor disk preflight: cannot read $tfvars" >&2
  exit 1
fi

# One node per line in every env file: { name = "...", ..., storage_pool = "..." }.
pairs=$(grep -v '^[[:space:]]*#' "$tfvars" |
  sed -nE 's/.*name[[:space:]]*=[[:space:]]*"([^"]+)".*storage_pool[[:space:]]*=[[:space:]]*"([^"]+)".*/\1 \2/p')
if [ -z "$pairs" ]; then
  echo "::warning::hypervisor disk preflight: no nodes with a storage_pool in $tfvars; nothing checked"
  exit 0
fi

# Prints "<root-avail-kb> <pool-avail-kb> <pool-path>" for the pool named in $1.
# shellcheck disable=SC2016 # expanded on the hypervisor, not here
remote='set -u
root=$(df -Pk /etc/libvirt/qemu | awk "NR==2{print \$4}")
path=$(virsh -c qemu:///system pool-dumpxml "$1" 2>/dev/null | sed -n "s:.*<path>\(.*\)</path>.*:\1:p" | head -n1)
pool=-
[ -n "$path" ] && pool=$(df -Pk "$path" | awk "NR==2{print \$4}")
echo "$root $pool ${path:--}"'

fail=0
while read -r host pool; do
  [ -n "$host" ] || continue
  if ! out=$(ssh -o BatchMode=yes -o ConnectTimeout=10 "${user}@${host}" bash -s -- "$pool" <<<"$remote" 2>&1); then
    echo "::warning::hypervisor disk preflight: cannot reach $host ($out); its disk is unchecked"
    continue
  fi
  read -r root_kb pool_kb pool_path <<<"$(tail -n1 <<<"$out")"
  if ! [[ "$root_kb" =~ ^[0-9]+$ ]]; then
    echo "::warning::hypervisor disk preflight: unexpected answer from $host: $out"
    continue
  fi
  echo "$host: root fs $((root_kb / 1024)) MiB free; pool $pool ($pool_path) $([[ "$pool_kb" =~ ^[0-9]+$ ]] && echo "$((pool_kb / 1024 / 1024)) GiB free" || echo "not found")"
  if [ "$root_kb" -lt "$min_root_kb" ]; then
    echo "::error::hypervisor $host has $((root_kb / 1024)) MiB free on the filesystem holding /etc/libvirt/qemu (need ${HV_MIN_ROOT_FREE_MB:-1024} MiB): libvirt cannot write a domain definition there. Free space on $host before re-running." >&2
    fail=1
  fi
  if [[ "$pool_kb" =~ ^[0-9]+$ ]] && [ "$pool_kb" -lt "$min_pool_kb" ]; then
    echo "::error::hypervisor $host has $((pool_kb / 1024 / 1024)) GiB free in storage pool $pool at $pool_path (need ${HV_MIN_POOL_FREE_GB:-20} GiB): the node's disk cannot grow. Free space on $host before re-running." >&2
    fail=1
  fi
done <<<"$pairs"

exit "$fail"
