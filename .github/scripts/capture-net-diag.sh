#!/usr/bin/env bash
# capture-net-diag.sh — write a snapshot of network / OVN / OVS / IPsec
# state into $1 (a directory the caller already created on the local host).
# Designed to run on a spinifex node after an E2E suite, before tofu destroy
# pulls the rug out. Every command is independent — `|| true` so a missing
# binary on one path doesn't abort the rest of the capture.
#
# Output layout (under $1/net-diag/):
#   ovs-monitor-ipsec.status         systemctl status (output-only)
#   ovs-monitor-ipsec.journal        last 200 journal lines
#   ipsec.openvswitch_conf           ovs-vsctl get Open_vSwitch . other_config
#   ipsec.xfrm_state                 ip xfrm state list
#   ipsec.xfrm_policy                ip xfrm policy list
#   ovn.sb_show                      ovn-sbctl show
#   ovn.nb_show                      ovn-nbctl show
#   ovs.show                         ovs-vsctl show
#   ovs.flows_br_int                 ovs-ofctl dump-flows br-int
#   ovs.tunnels                      ovs-appctl ofproto/list-tunnels br-int
#   net.links                        ip -s link show
#   net.routes                       ip route show table all
#   net.addrs                        ip -br addr show
#   systemd.failed_units             systemctl list-units --state=failed
set +e
OUT="${1:?usage: capture-net-diag.sh <output-dir>}"
mkdir -p "$OUT"

run() {
    local label="$1"; shift
    {
        echo "# $*"
        "$@" 2>&1
    } > "$OUT/$label" || true
}

run ovs-monitor-ipsec.status            sudo systemctl status ovs-monitor-ipsec.service --no-pager --full --lines=50
run ovs-monitor-ipsec.journal           sudo journalctl -u ovs-monitor-ipsec.service --no-pager --output=short-iso -n 200
run ipsec.openvswitch_conf              sudo ovs-vsctl get Open_vSwitch . other_config
run ipsec.xfrm_state                    sudo ip xfrm state list
run ipsec.xfrm_policy                   sudo ip xfrm policy list

run ovn.sb_show                         sudo ovn-sbctl show
run ovn.nb_show                         sudo ovn-nbctl show
run ovn.sb_chassis                      sudo ovn-sbctl list chassis

run ovs.show                            sudo ovs-vsctl show
run ovs.flows_br_int                    sudo ovs-ofctl dump-flows br-int
run ovs.tunnels                         sudo ovs-appctl ofproto/list-tunnels br-int

run net.links                           ip -s link show
run net.routes                          ip route show table all
run net.addrs                           ip -br addr show

run systemd.failed_units                sudo systemctl list-units --state=failed --no-pager

exit 0
