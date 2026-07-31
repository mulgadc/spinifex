#!/bin/sh
# mulga-vpc-mtu — boot oneshot that pins the VPC data NIC MTU so flannel sizes
# its VXLAN overlay to fit the OVN geneve underlay.
#
# Spinifex carries tenant traffic over an OVN geneve underlay (~1442B usable on a
# 1500B host link). The EKS node's primary ENI (eth0) comes up at the default
# 1500 — cloud-init's Ec2 datasource has no MTU leaf — so flannel derives
# flannel.1 = 1500-50 = 1450. VXLAN frames up to 1500 then exceed the underlay and
# are silently dropped: small packets (TCP handshakes) pass while large ones (TLS,
# kubelet streams, pod-to-pod payloads) blackhole. Pinning eth0 down to 1320
# (flannel.1 = 1270) leaves margin under the underlay and restores the datapath.
# This must run before k3s starts flannel, which reads the iface MTU once at
# startup.
#
# Pinning the LINK is not sufficient on its own. This image runs dhcpcd, not
# udhcpc (an earlier version of this comment had that backwards), and dhcpcd
# honours DHCP option 26 by stamping the advertised MTU onto every route it
# installs:
#
#   default via 10.32.101.1 dev eth0 proto dhcp src 10.32.101.7 metric 1002 mtu 1442
#
# Linux prefers a route's MTU metric over the device MTU (dst_mtu()), so the node
# still advertises an MSS derived from 1442 no matter what the link says, and
# large inbound segments blackhole in the HOST netns — which is where containerd
# pulls run, so image pulls fail with `TLS handshake timeout` while pod traffic
# (correctly sized off cni0) is fine. Pod netns working while the node netns does
# not is the signature. Re-stamp the routes to match the pinned link.
#
# eth0 is always the primary data ENI (cloud-init set-name; mgmt0, when present,
# is a separate NIC and is left at its default). The default-route interface is
# pinned too in case it differs.
#
# On the systemd (mkosi) image, netplan renders UseMTU=true (it does not inherit
# networkd's own false default), so a lease renewal re-inflates the link at
# T1 — the pin above only wins the boot race by ordering. usemtu_off() removes
# that exposure. Shared verbatim with the Alpine/dhcpcd image, where it is a
# no-op: no systemd-networkd there.
set -u

MTU="${MULGA_VPC_NIC_MTU:-1320}"
NETSYSFS="${MULGA_NET_SYSFS:-/sys/class/net}"
NETPLAN_DIR="${MULGA_NETPLAN_DIR:-/run/systemd/network}"
FAILED=0

pin() {
    iface="$1"
    [ -n "$iface" ] || return 0
    ip link show dev "$iface" >/dev/null 2>&1 || return 0
    if ip link set dev "$iface" mtu "$MTU"; then
        echo "[mulga-vpc-mtu] pinned $iface MTU to $MTU"
    else
        echo "[mulga-vpc-mtu] WARNING: failed to set MTU $MTU on $iface" >&2
    fi
}

# Rewrite the MTU metric dhcpcd stamped onto the routes of one interface, so no
# route can advertise a larger MSS than the pinned link carries. Routes with no
# MTU metric inherit the device MTU and are left alone.
#
# `ip route show dev X` omits the "dev X" token from each line, so it is added
# back before replacing; a "default" line already carries its own dev.
restamp() {
    iface="$1"
    [ -n "$iface" ] || return 0
    ip -4 route show dev "$iface" 2>/dev/null | while read -r line; do
        case "$line" in
            *" mtu "*) ;;
            *) continue ;;
        esac
        cur=$(echo "$line" | sed -n 's/.* mtu \([0-9]*\).*/\1/p')
        [ -n "$cur" ] || continue
        [ "$cur" != "$MTU" ] || continue
        spec=$(echo "$line" | sed "s/ mtu $cur/ mtu $MTU/")
        case "$spec" in
            *" dev "*) ;;
            *) spec="$spec dev $iface" ;;
        esac
        # Word splitting is intended: $spec is a route specification, not a path.
        # shellcheck disable=SC2086
        if ip route replace $spec; then
            echo "[mulga-vpc-mtu] $iface route MTU $cur -> $MTU ($line)"
        else
            echo "[mulga-vpc-mtu] WARNING: failed to restamp $iface route: $line" >&2
        fi
    done
}

# Force UseMTU=false on every netplan-rendered unit, so a renewal can never
# re-inflate the link. The rendered filename embeds the interface name and is
# unpredictable at build time, so this enumerates /run at boot instead.
#
# [DHCPv4] is the section systemd documents for UseMTU=; the netplan render
# itself still uses the legacy [DHCP] alias, but a drop-in written here should
# target the current name. No /run/systemd/network means no systemd-networkd.
usemtu_off() {
    [ -d "$NETPLAN_DIR" ] || return 0
    changed=0
    for unit in "$NETPLAN_DIR"/*.network; do
        [ -e "$unit" ] || continue
        dropdir="${unit}.d"
        if ! mkdir -p "$dropdir"; then
            echo "[mulga-vpc-mtu] WARNING: failed to create $dropdir" >&2
            continue
        fi
        conf="$dropdir/10-mulga-usemtu.conf"
        # [DHCPv4] is current; [DHCP] is the legacy alias netplan still renders.
        # Whichever the running systemd parses, the other is inert, so stating
        # both costs nothing and removes a silent no-op if they ever diverge.
        if printf '[DHCPv4]\nUseMTU=false\n\n[DHCP]\nUseMTU=false\n' > "$conf"; then
            echo "[mulga-vpc-mtu] forced UseMTU=false for $unit via $conf"
            changed=1
        else
            echo "[mulga-vpc-mtu] WARNING: failed to write $conf" >&2
        fi
    done
    [ "$changed" -eq 1 ] || return 0
    if networkctl reload; then
        echo "[mulga-vpc-mtu] reloaded networkd to apply UseMTU=false"
    else
        echo "[mulga-vpc-mtu] WARNING: networkctl reload failed" >&2
    fi
}

# Both failure modes here are silent otherwise: a link that drifted back up, or
# a route the restamp pass missed. Route parsing avoids a `| while` pipeline so
# a failure recorded in FAILED survives outside the loop's subshell.
assert_pinned() {
    iface="$1"
    [ -n "$iface" ] || return 0
    [ -d "$NETSYSFS/$iface" ] || return 0
    cur=$(cat "$NETSYSFS/$iface/mtu" 2>/dev/null)
    if [ "$cur" != "$MTU" ]; then
        echo "[mulga-vpc-mtu] ERROR: $iface link MTU is ${cur:-unknown}, expected $MTU" >&2
        FAILED=1
    fi
    routes=$(ip -4 route show dev "$iface" 2>/dev/null)
    [ -n "$routes" ] || return 0
    oldifs=$IFS
    IFS='
'
    for line in $routes; do
        case "$line" in
            *" mtu "*) ;;
            *) continue ;;
        esac
        rmtu=$(echo "$line" | sed -n 's/.* mtu \([0-9]*\).*/\1/p')
        if [ -n "$rmtu" ] && [ "$rmtu" != "$MTU" ]; then
            echo "[mulga-vpc-mtu] ERROR: $iface route carries conflicting mtu $rmtu (expected $MTU): $line" >&2
            FAILED=1
        fi
    done
    IFS="$oldifs"
}

pin eth0

route_iface=$(ip -4 route show default 2>/dev/null | awk '{print $5; exit}')
[ "$route_iface" = "eth0" ] || pin "$route_iface"

# setup.sh writes `nooption interface_mtu` to dhcpcd.conf so no future lease can
# re-stamp a route MTU; this pass fixes routes installed before that took effect
# (an already-running dhcpcd, or an image built before the fix).
restamp eth0
[ "$route_iface" = "eth0" ] || restamp "$route_iface"

# Runs after restamp: the drop-in stops future renewals from re-inflating the
# link, restamp above already cleaned up anything a past renewal left behind.
usemtu_off

assert_pinned eth0
[ "$route_iface" = "eth0" ] || assert_pinned "$route_iface"

if [ "$FAILED" -ne 0 ]; then
    echo "[mulga-vpc-mtu] ERROR: MTU assertion failed, see above" >&2
    exit 1
fi

exit 0
