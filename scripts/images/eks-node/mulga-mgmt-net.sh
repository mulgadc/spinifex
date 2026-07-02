#!/bin/sh
# mulga-mgmt-net — boot oneshot that brings a multi-NIC BootAMI system VM's NICs
# up from the QEMU fw_cfg netcfg blob (opt/spinifex/netcfg) the host writes per
# launch. The EKS control plane is the case: cloud-init on a stock Alpine guest
# cannot reliably pick the right NIC out of two, so the host enumerates them and
# this oneshot configures each by MAC, before cloud-init's network stage:
#   - NIC<n>_DHCP=1: the primary data ENI. OVN serves DHCP; we lease it (one-shot)
#     so the Ec2 IMDS datasource can reach 169.254.169.254, and pin a /32 to it so
#     a link-local 169.254.0.0/16 route on another NIC cannot hijack IMDS.
#   - NIC<n>_CIDR: a static NIC (mgmt0 on br-mgmt, which has no DHCP). Applied
#     address-only; NIC<n>_DEFAULT is 0, so mgmt0 is never the default route.
#
# The blob is shell KEY=value, matching daemon.buildNetcfgBlob and
# build/microvm/init.sh; interfaces are matched by MAC. No-op when the blob is
# absent (a single-NIC agent/worker brings its one NIC up via cloud-init/IMDS),
# so the same image boots unchanged without one.
set -eu

NETCFG="${MULGA_NETCFG:-/sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/netcfg/raw}"
IMDS_IP="169.254.169.254"

# qemu_fw_cfg is a module on the stock Alpine cloud image; load it (best-effort)
# before reading. Skipped harmlessly when built-in or already loaded.
modprobe qemu_fw_cfg 2>/dev/null || modprobe fw_cfg_sysfs 2>/dev/null || true

if [ ! -f "$NETCFG" ]; then
    echo "[mulga-mgmt-net] no fw_cfg netcfg; skipping system NIC setup"
    exit 0
fi

# shellcheck disable=SC1090
. "$NETCFG"

# One-shot DHCP: configure the link then exit, leaving no daemon to fight
# cloud-init's later management of the primary ENI. Prefer busybox udhcpc; fall
# back to dhcpcd where udhcpc's config script is absent.
dhcp_oneshot() {
    iface="$1"
    if command -v udhcpc >/dev/null 2>&1 && [ -x /usr/share/udhcpc/default.script ]; then
        udhcpc -i "$iface" -f -q -n -t 8 -T 2 >/dev/null 2>&1
    elif command -v dhcpcd >/dev/null 2>&1; then
        dhcpcd -q -t 20 "$iface" >/dev/null 2>&1
    else
        echo "[mulga-mgmt-net] no DHCP client (udhcpc/dhcpcd) for $iface" >&2
        return 1
    fi
}

for n in 0 1 2 3 4 5; do
    # ${VAR:-} keeps the unset NIC<n>_* slots safe under `set -u`.
    eval "mac=\${NIC${n}_MAC:-}"
    [ -z "$mac" ] && continue

    iface=$(ip -o link | awk -v m="$mac" 'tolower($0) ~ tolower(m) {print $2}' | tr -d :)
    if [ -z "$iface" ]; then
        echo "[mulga-mgmt-net] WARNING: no interface for MAC $mac (NIC$n); skipping" >&2
        continue
    fi

    eval "dhcp=\${NIC${n}_DHCP:-}"
    eval "cidr=\${NIC${n}_CIDR:-}"
    ip link set "$iface" up

    if [ "$dhcp" = "1" ]; then
        if dhcp_oneshot "$iface"; then
            echo "[mulga-mgmt-net] data NIC $iface ($mac) up via DHCP"
        else
            echo "[mulga-mgmt-net] ERROR: no DHCP lease on data NIC $iface ($mac)" >&2
        fi
        # Pin IMDS to the data NIC so a link-local 169.254.0.0/16 route on another
        # interface cannot steal the metadata path. Route via the gateway, not
        # on-link: the host demuxes IMDS sent to the gateway MAC, never ARP-answers .254.
        gw=$(ip -4 route show default dev "$iface" 2>/dev/null | awk '{print $3; exit}')
        if [ -n "$gw" ]; then
            ip route replace "${IMDS_IP}/32" via "$gw" dev "$iface" 2>/dev/null || true
        else
            ip route replace "${IMDS_IP}/32" dev "$iface" 2>/dev/null || true
        fi
        continue
    fi

    if [ -z "$cidr" ]; then
        echo "[mulga-mgmt-net] brought up $iface ($mac), no CIDR"
        continue
    fi
    # `replace` is idempotent — adds the address if absent, no-op on a stop/start
    # re-attach that reuses a surviving interface. A real failure must surface,
    # not strand mgmt0 IP-less.
    if ip addr replace "$cidr" dev "$iface"; then
        echo "[mulga-mgmt-net] configured $iface ($mac) with $cidr"
    else
        echo "[mulga-mgmt-net] ERROR: failed to set $cidr on $iface ($mac)" >&2
        exit 1
    fi

    # NIC<n>_DEFAULT=0 means this NIC must never carry the default route: the
    # mgmt NIC reaches the gateway on-link and a default via it would blackhole
    # egress and (with a link-local /16) hijack IMDS. Enforce it — a DHCP client
    # racing this NIC may have added one before we set it static.
    eval "isdefault=\${NIC${n}_DEFAULT:-}"
    if [ "$isdefault" != "1" ]; then
        ip route del default dev "$iface" 2>/dev/null || true
    fi
done
