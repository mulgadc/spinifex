#!/bin/sh
# mulga-mgmt-net — boot oneshot that brings a multi-NIC BootAMI system VM's NICs
# up from the QEMU fw_cfg netcfg blob (opt/spinifex/netcfg) the host writes per
# launch. Shared by every system image the host attaches a mgmt NIC to (eks-node,
# rds-postgres): cloud-init on a stock Alpine guest cannot reliably pick the right
# NIC out of two, so the host enumerates them and this oneshot configures each by
# MAC, before cloud-init's network stage:
#   - NIC<n>_DHCP=1: the primary data ENI. OVN serves DHCP; we lease it (retrying
#     one-shots on a budget until the cross-host datapath is up) so the Ec2 IMDS
#     datasource can reach 169.254.169.254, and pin a /32 to it so a link-local
#     169.254.0.0/16 route on another NIC cannot hijack IMDS. A DHCP NIC with
#     NIC<n>_DEFAULT=0 (an RDS DB VM's customer ENI) instead loses its leased
#     default route and gets a source-based policy table for its replies.
#   - NIC<n>_CIDR: a static NIC (mgmt0 on br-mgmt, which has no DHCP). Applied
#     address-only; NIC<n>_DEFAULT is 0, so mgmt0 is never the default route.
#
# The blob is shell KEY=value, matching daemon.buildNetcfgBlob and
# build/microvm/init.sh; interfaces are matched by MAC. No-op when the blob is
# absent (a single-NIC agent/worker brings its one NIC up via cloud-init/IMDS),
# so the same image boots unchanged without one.
#
# Run twice per boot, over the same blob:
#   - setup (default), from the boot runlevel: acquires addresses as above.
#   - --enforce-routes, after cloud-init: re-applies the route policy only.
#     cloud-init's network stage re-renders every ENI from IMDS and re-DHCPs
#     them, which puts back the default route the setup pass deleted from a
#     NIC<n>_DEFAULT=0 interface — leaving the guest with two default routes and
#     the customer ENI able to capture egress and IMDS. It also rewrites that
#     NIC's policy table, whose routes went down with the bounced interface.
#     Nothing re-leases or re-addresses an interface in this pass.
set -eu

NETCFG="${MULGA_NETCFG:-/sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/netcfg/raw}"
IMDS_IP="169.254.169.254"

MODE="setup"
if [ "${1:-}" = "--enforce-routes" ]; then
    MODE="enforce"
fi

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
        # -q is only quiet; -1 is what exits, and -p keeps the address on the
        # way out. A surviving daemon owns the control socket, so cloud-init's
        # own dhcpcd is forwarded to it and never writes the pid file it waits on.
        dhcpcd -q -1 -p -t 20 "$iface" >/dev/null 2>&1
    else
        echo "[mulga-mgmt-net] no DHCP client (udhcpc/dhcpcd) for $iface" >&2
        return 1
    fi
}

# The OVN DHCP datapath for a cross-host CP data NIC lags guest boot by minutes
# (logical-port binding + per-tap reconcile on the remote placement host). A
# single ~16s one-shot loses that race and strands the NIC IP-less, so the Ec2
# datasource never reaches IMDS and the member wedges. Retry the one-shot on a
# budget aligned with cloud-init's Ec2 max_wait (600s); each attempt still exits
# leaving no lingering daemon, so cloud-init's later ENI management is unaffected.
DHCP_BUDGET="${MULGA_DHCP_BUDGET:-600}"

dhcp_acquire() {
    iface="$1"
    deadline=$(( $(date +%s) + DHCP_BUDGET ))
    while :; do
        if dhcp_oneshot "$iface" && ip -4 addr show dev "$iface" | grep -q 'inet '; then
            return 0
        fi
        [ "$(date +%s)" -ge "$deadline" ] && return 1
        sleep 3
    done
}

# Removes every default route on one interface, not just the first: a re-DHCP
# can leave more than one, and a single `ip route del` would silently keep the
# rest. Bounded so a route that refuses to delete cannot spin.
drop_default_route() {
    iface="$1"
    attempt=0
    # The bound is part of the condition, not a `&& break` in the body: under
    # `set -e` a trailing test that evaluates false takes the whole script down.
    while [ "$attempt" -lt 8 ] && ip -4 route show default dev "$iface" 2>/dev/null | grep -q .; do
        ip route del default dev "$iface" 2>/dev/null || break
        attempt=$(( attempt + 1 ))
    done
}

# Network address of a leased CIDR, computed rather than read back from
# `ip route show`, so the policy table's contents do not depend on how a given
# `ip` renders a connected route.
cidr_network() {
    echo "$1" | awk -F/ '{
        split($1, o, ".")
        v = o[1] * 16777216 + o[2] * 65536 + o[3] * 256 + o[4]
        blk = 2 ^ (32 - $2)
        n = int(v / blk) * blk
        printf "%d.%d.%d.%d/%d\n", int(n / 16777216) % 256, int(n / 65536) % 256, \
            int(n / 256) % 256, n % 256, $2
    }'
}

# A NIC<n>_DEFAULT=0 DHCP NIC (an RDS DB VM's customer ENI) has its leased
# default route deleted, so a reply to a client outside its own subnet leaves by
# the primary ENI with this NIC's source address and is dropped. Give it a
# private table and steer its replies at it by source address.
#
# Matching on source needs no knowledge of the customer VPC's CIDR: the engine
# binds its socket to this address, so every reply carries it whoever the client
# is. Every command reports its own failure — a `set -e` trip here is silent and
# would leave every later NIC in the blob unconfigured.
install_return_policy() {
    iface="$1"
    tid=$(( 100 + $2 ))

    addr=$(ip -4 addr show dev "$iface" 2>/dev/null | awk '$1 == "inet" { print $2; exit }')
    if [ -z "$addr" ]; then
        echo "[mulga-mgmt-net] ERROR: no address on $iface; no return-path policy" >&2
        return 0
    fi
    ip4="${addr%%/*}"
    net=$(cidr_network "$addr")
    # The lease's gateway is OVN's per-subnet router IP, which reaches every
    # subnet of the VPC. Read it before the default route is dropped from main.
    gw=$(ip -4 route show default dev "$iface" 2>/dev/null | awk '{print $3; exit}')

    # Both passes rewrite the table, not just the rule: a rule survives an
    # interface bounce but the routes go down with the device, and a rule whose
    # table is empty falls through to main — the asymmetry this exists to fix.
    ip route replace "$net" dev "$iface" scope link src "$ip4" table "$tid" || \
        echo "[mulga-mgmt-net] ERROR: failed to add $net to table $tid on $iface" >&2
    if [ -n "$gw" ]; then
        ip route replace default via "$gw" dev "$iface" table "$tid" || \
            echo "[mulga-mgmt-net] ERROR: failed to add default via $gw to table $tid on $iface" >&2
    else
        echo "[mulga-mgmt-net] no lease gateway on $iface; leaving table $tid's default as-is"
    fi

    # A rule added twice is listed twice, so drop it by priority first. The
    # delete is expected to fail on the first pass of a boot.
    ip rule del pref "$tid" 2>/dev/null || true
    ip rule add from "$ip4" table "$tid" pref "$tid" || \
        echo "[mulga-mgmt-net] ERROR: failed to add rule from $ip4 to table $tid" >&2

    echo "[mulga-mgmt-net] return-path policy on $iface: from $ip4 lookup $tid"
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
    eval "isdefault=\${NIC${n}_DEFAULT:-}"
    ip link set "$iface" up

    # dhcp and isdefault are assigned above via eval; shellcheck can't trace
    # dynamic names.
    # shellcheck disable=SC2154
    if [ "$dhcp" = "1" ]; then
        if [ "$MODE" = "setup" ]; then
            # A non-default DHCP NIC (an RDS DB VM's customer-VPC ENI) must not
            # rewrite the resolver its lease happens to advertise: the guest's DNS
            # belongs to the primary ENI, and the busybox/dhcpcd hooks would
            # clobber it. RESOLV_CONF is honoured by udhcpc's default.script.
            if [ "$isdefault" = "1" ]; then
                unset RESOLV_CONF
            else
                RESOLV_CONF=/dev/null
                export RESOLV_CONF
            fi
            if dhcp_acquire "$iface"; then
                echo "[mulga-mgmt-net] data NIC $iface ($mac) up via DHCP"
            else
                echo "[mulga-mgmt-net] ERROR: no DHCP lease on data NIC $iface ($mac) after ${DHCP_BUDGET}s" >&2
            fi
        fi

        # Only the default NIC owns the metadata path. A second DHCP NIC leases
        # its own default route and, left alone, would both blackhole egress and
        # steal IMDS from the primary ENI.
        if [ "$isdefault" != "1" ]; then
            install_return_policy "$iface" "$n"
            drop_default_route "$iface"
            if [ "$MODE" = "enforce" ]; then
                echo "[mulga-mgmt-net] re-asserted no default route on $iface ($mac)"
            fi
            continue
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

    # A static NIC's address is not something cloud-init re-renders, so the
    # enforce pass only has its route policy left to re-assert.
    if [ "$MODE" = "setup" ]; then
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
    fi

    # NIC<n>_DEFAULT=0 means this NIC must never carry the default route: the
    # mgmt NIC reaches the gateway on-link and a default via it would blackhole
    # egress and (with a link-local /16) hijack IMDS. Enforce it — a DHCP client
    # racing this NIC may have added one before we set it static.
    if [ "$isdefault" != "1" ]; then
        drop_default_route "$iface"
    fi
done
