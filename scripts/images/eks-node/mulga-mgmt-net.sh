#!/bin/sh
# mulga-mgmt-net — boot oneshot that brings up the management NIC of a system VM
# from the QEMU fw_cfg netcfg blob (opt/spinifex/netcfg) the host writes per
# launch. BootAMI system VMs (the EKS control plane) boot from the Ec2 IMDS
# datasource, which configures only the primary ENI; the mgmt NIC lives on
# br-mgmt with no DHCP, so its static address arrives via fw_cfg and is applied
# here. No-op when the blob is absent, so the same image boots unchanged as a
# plain agent or any guest without a mgmt NIC.
#
# The blob is shell KEY=value (NIC<n>_MAC / NIC<n>_CIDR / NIC<n>_DEFAULT),
# matching daemon.buildNetcfgBlob and build/microvm/init.sh; interfaces are
# matched by MAC so the primary ENI (owned by cloud-init/Ec2) is never touched.
# mgmt0 is configured address-only — NIC_DEFAULT is 0, so no default route.
set -eu

NETCFG="${MULGA_NETCFG:-/sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/netcfg/raw}"

# qemu_fw_cfg is a module on the stock Alpine cloud image; load it (best-effort)
# before reading. Skipped harmlessly when built-in or already loaded.
modprobe qemu_fw_cfg 2>/dev/null || modprobe fw_cfg_sysfs 2>/dev/null || true

if [ ! -f "$NETCFG" ]; then
    echo "[mulga-mgmt-net] no fw_cfg netcfg; skipping mgmt NIC setup"
    exit 0
fi

# shellcheck disable=SC1090
. "$NETCFG"

for n in 0 1 2 3 4 5; do
    # ${VAR:-} keeps the unset NIC<n>_* slots safe under `set -u`.
    eval "mac=\${NIC${n}_MAC:-}"
    [ -z "$mac" ] && continue

    iface=$(ip -o link | awk -v m="$mac" 'tolower($0) ~ tolower(m) {print $2}' | tr -d :)
    if [ -z "$iface" ]; then
        echo "[mulga-mgmt-net] WARNING: no interface for MAC $mac (NIC$n); skipping" >&2
        continue
    fi

    eval "cidr=\${NIC${n}_CIDR:-}"
    ip link set "$iface" up
    if [ -z "$cidr" ]; then
        echo "[mulga-mgmt-net] brought up $iface ($mac), no CIDR"
        continue
    fi
    # `replace` is idempotent — adds the address if absent, no-op on a
    # stop/start re-attach that reuses a surviving interface (`add` errored on
    # the duplicate). A real failure must surface, not strand mgmt0 IP-less.
    if ip addr replace "$cidr" dev "$iface"; then
        echo "[mulga-mgmt-net] configured $iface ($mac) with $cidr"
    else
        echo "[mulga-mgmt-net] ERROR: failed to set $cidr on $iface ($mac)" >&2
        exit 1
    fi
done
