#!/bin/sh
# mulga-vpc-mtu — boot oneshot that pins the VPC data NIC MTU so flannel sizes
# its VXLAN overlay to fit the OVN geneve underlay.
#
# Spinifex carries tenant traffic over an OVN geneve underlay (~1442B usable on a
# 1500B host link). The EKS node's primary ENI (eth0) comes up at the default
# 1500 — cloud-init's Ec2 datasource has no MTU leaf and Alpine's udhcpc ignores
# DHCP option 26 — so flannel derives flannel.1 = 1500-50 = 1450. VXLAN frames up
# to 1500 then exceed the underlay and are silently dropped: small packets (TCP
# handshakes) pass while large ones (TLS, kubelet streams, pod-to-pod payloads)
# blackhole. Pinning eth0 down to 1320 (flannel.1 = 1270) leaves margin under the
# underlay and restores the datapath. This must run before k3s starts flannel,
# which reads the iface MTU once at startup.
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

pin eth0

route_iface=$(ip -4 route show default 2>/dev/null | awk '{print $5; exit}')
[ "$route_iface" = "eth0" ] || pin "$route_iface"

exit 0
