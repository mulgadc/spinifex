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
set -u

MTU="${MULGA_VPC_NIC_MTU:-1320}"

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

pin eth0

route_iface=$(ip -4 route show default 2>/dev/null | awk '{print $5; exit}')
[ "$route_iface" = "eth0" ] || pin "$route_iface"

# setup.sh writes `nooption interface_mtu` to dhcpcd.conf so no future lease can
# re-stamp a route MTU; this pass fixes routes installed before that took effect
# (an already-running dhcpcd, or an image built before the fix).
restamp eth0
[ "$route_iface" = "eth0" ] || restamp "$route_iface"

exit 0
