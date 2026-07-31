#!/bin/bash
# verify-network.sh — Assert a dev node's datapath matches its declared planes.
#
# smoke-test.sh proves an instance booted, got a public IP and answered SSH
# from the host. None of that traffic leaves the box, so it passes on a node
# whose EIPs are unreachable from the internet, whose Geneve rides the WAN
# plane, or whose NATS listens on the public interface. This asserts the
# things that are actually load-bearing.
#
# Run standalone or from reset-dev-env.sh, which exports the resolved planes.
# Without those, planes are auto-detected the same way the reset does.
#
# Usage:
#   ./scripts/verify-network.sh
#   DEV_VPC_BRIDGE=br-vpc DEV_BIND_PLANES=1 ./scripts/verify-network.sh
#
# Environment:
#   DEV_WAN_BRIDGE / DEV_LAN_BRIDGE / DEV_VPC_BRIDGE   plane bridges
#   DEV_MGMT_BRIDGE / DEV_MGMT_CIDR                    guest→host bridge
#   DEV_UPLINK        'bridged' (default) or 'nat'
#   DEV_BIND_PLANES   1 to additionally assert the service bind matrix
#   SKIP_EGRESS       1 to skip the in-guest egress probe (no instance running)
#
# Exits non-zero on the first failed assertion class, after running them all.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

INVOKING_USER="${SUDO_USER:-$(id -un)}"
INVOKING_HOME=$(getent passwd "$INVOKING_USER" | cut -d: -f6)
aws_as_user() { sudo -u "$INVOKING_USER" env HOME="$INVOKING_HOME" AWS_PROFILE=spinifex aws "$@"; }

FAILURES=0
pass() { printf '  \033[32m✓\033[0m %s\n' "$*"; }
fail() { printf '  \033[31m✗\033[0m %s\n' "$*"; FAILURES=$((FAILURES + 1)); }
skip() { printf '  \033[33m-\033[0m %s\n' "$*"; }

iface_ip() {
    ip -4 -o addr show "$1" 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1
}

# --- Resolve planes (same collapse the reset applies) ---
WAN_BRIDGE="${DEV_WAN_BRIDGE:-$(ip -4 route show default | awk '{print $5}' | head -1)}"
LAN_BRIDGE="${DEV_LAN_BRIDGE:-$WAN_BRIDGE}"
VPC_BRIDGE="${DEV_VPC_BRIDGE:-$LAN_BRIDGE}"
MGMT_BRIDGE="${DEV_MGMT_BRIDGE:-br-mgmt}"
MGMT_CIDR="${DEV_MGMT_CIDR:-10.15.8.1/24}"
UPLINK="${DEV_UPLINK:-bridged}"
BIND_PLANES="${DEV_BIND_PLANES:-0}"

WAN_IP=$(iface_ip "$WAN_BRIDGE")
LAN_IP=$(iface_ip "$LAN_BRIDGE")
VPC_IP=$(iface_ip "$VPC_BRIDGE")

echo "==> Planes: wan=$WAN_BRIDGE($WAN_IP) lan=$LAN_BRIDGE($LAN_IP) vpc=$VPC_BRIDGE($VPC_IP)"

# --- OVN chassis configuration ---
echo "==> OVN chassis"
ENCAP_IP=$(sudo ovs-vsctl get Open_vSwitch . external_ids:ovn-encap-ip 2>/dev/null | tr -d '"')
if [ "$ENCAP_IP" = "$VPC_IP" ]; then
    pass "ovn-encap-ip is the vpc plane ($ENCAP_IP)"
else
    fail "ovn-encap-ip is $ENCAP_IP, expected the vpc plane $VPC_IP — Geneve is on the wrong plane"
fi

MAPPINGS=$(sudo ovs-vsctl get Open_vSwitch . external_ids:ovn-bridge-mappings 2>/dev/null | tr -d '"')
EXT_BR="${MAPPINGS#external:}"
if [ -z "$MAPPINGS" ]; then
    fail "ovn-bridge-mappings is unset — no external network"
elif sudo ovs-vsctl br-exists "$EXT_BR" 2>/dev/null; then
    pass "ovn-bridge-mappings → $MAPPINGS"
else
    fail "ovn-bridge-mappings names '$EXT_BR', which is not an OVS bridge"
fi

# --- Plane bridges and the uplink ---
echo "==> Bridges"
for pair in "wan:$WAN_BRIDGE:$WAN_IP" "lan:$LAN_BRIDGE:$LAN_IP" "vpc:$VPC_BRIDGE:$VPC_IP"; do
    plane="${pair%%:*}"; rest="${pair#*:}"; br="${rest%%:*}"; expect="${rest#*:}"
    if [ -z "$expect" ]; then
        fail "$plane plane '$br' has no IPv4 address"
    elif ip link show "$br" 2>/dev/null | grep -q "state UP\|UP,LOWER_UP"; then
        pass "$plane plane $br up with $expect"
    else
        fail "$plane plane '$br' is not up"
    fi
done

if [ "$UPLINK" = "bridged" ] && [ "$EXT_BR" = "br-ext" ]; then
    # A Linux WAN bridge is linked to OVS by a veth pair; the OVS end must be
    # a port on the external bridge or nothing reaches the physical network.
    if sudo ovs-vsctl list-ports br-ext 2>/dev/null | grep -q '^veth-wan-ovs$'; then
        pass "veth uplink veth-wan-ovs is a port on br-ext"
    else
        fail "br-ext has no veth-wan-ovs port — the WAN uplink is not connected"
    fi
fi

# --- Guest→host management bridge ---
echo "==> Management bridge"
if ! sudo ovs-vsctl br-exists "$MGMT_BRIDGE" 2>/dev/null; then
    fail "$MGMT_BRIDGE is not an OVS bridge"
elif sudo ovs-vsctl show 2>/dev/null | grep -A3 "Bridge $MGMT_BRIDGE" | grep -q "error:"; then
    # The half-built state: OVS holds the record but a Linux link owns the
    # name, so the internal netdev was never created.
    fail "$MGMT_BRIDGE has a port error — a non-OVS link is holding the name"
else
    MGMT_ACTUAL=$(ip -4 -o addr show "$MGMT_BRIDGE" 2>/dev/null | awk '{print $4}' | head -1)
    if [ "$MGMT_ACTUAL" = "$MGMT_CIDR" ]; then
        pass "$MGMT_BRIDGE is OVS with $MGMT_ACTUAL"
    else
        fail "$MGMT_BRIDGE carries ${MGMT_ACTUAL:-no address}, expected $MGMT_CIDR — system instances cannot reach the host"
    fi
fi

# --- No VPC gateway consumes a public address ---
# Distributed NAT leaves the gateway LRP link-local. A router port holding a
# pool address means centralised NAT crept back and each VPC is burning one.
echo "==> Gateway router ports"
if command -v ovn-nbctl >/dev/null 2>&1; then
    GW_ADDRS=$(sudo ovn-nbctl --bare --columns=networks list Logical_Router_Port 2>/dev/null |
               tr ' ' '\n' | grep -v '^$' || true)
    OFFENDERS=""
    if [ -n "$WAN_IP" ] && [ -n "$GW_ADDRS" ]; then
        WAN_PREFIX3=$(echo "$WAN_IP" | cut -d. -f1-3)
        OFFENDERS=$(echo "$GW_ADDRS" | grep "^${WAN_PREFIX3}\." || true)
    fi
    if [ -z "$OFFENDERS" ]; then
        pass "no logical router port holds a WAN-subnet address"
    else
        fail "logical router ports hold WAN-subnet addresses (centralised NAT regression): $(echo "$OFFENDERS" | tr '\n' ' ')"
    fi
else
    skip "ovn-nbctl not present"
fi

# --- EIP advertisement ---
# Something must answer ARP for the EIP or inbound traffic dies. Which
# something depends on the NAT mode, so read it off the rule rather than
# assuming: distributed NAT advertises from the chassis hosting the VM via
# external_mac + logical_port, centralised NAT from the gateway port's
# nat_addresses. Asserting the centralised mechanism against a distributed
# deployment fails a node whose datapath is entirely correct.
echo "==> EIP advertisement"
INSTANCE_IP=$(aws_as_user ec2 describe-instances \
    --filters "Name=instance-state-name,Values=running" \
    --query 'Reservations[0].Instances[0].PublicIpAddress' --output text 2>/dev/null || true)
if [ -z "$INSTANCE_IP" ] || [ "$INSTANCE_IP" = "None" ]; then
    skip "no running instance with a public IP"
elif ! command -v ovn-nbctl >/dev/null 2>&1; then
    skip "ovn-nbctl not present"
else
    NAT_TYPE=$(sudo ovn-nbctl --bare --columns=type find NAT external_ip="$INSTANCE_IP" 2>/dev/null || true)
    NAT_MAC=$(sudo ovn-nbctl --bare --columns=external_mac find NAT external_ip="$INSTANCE_IP" 2>/dev/null || true)
    NAT_PORT=$(sudo ovn-nbctl --bare --columns=logical_port find NAT external_ip="$INSTANCE_IP" 2>/dev/null || true)

    if [ -z "$NAT_TYPE" ]; then
        fail "no NAT rule for EIP $INSTANCE_IP"
    elif [ -n "$NAT_MAC" ] && [ -n "$NAT_PORT" ]; then
        pass "EIP $INSTANCE_IP advertised by the VM's chassis ($NAT_TYPE, mac $NAT_MAC on $NAT_PORT)"
    elif ! command -v ovn-sbctl >/dev/null 2>&1; then
        skip "centralised NAT, but ovn-sbctl is not present to check nat_addresses"
    else
        # Centralised: the EIP must be on a router-type Port_Binding. OVN
        # ignores nat_addresses on the localnet port, so nothing would ARP.
        NAT_ADDR_TYPE=$(sudo ovn-sbctl --bare --columns=type,nat_addresses list Port_Binding 2>/dev/null |
                        paste - - | grep -F "$INSTANCE_IP" | awk '{print $1}' | head -1)
        if [ -z "$NAT_ADDR_TYPE" ]; then
            fail "EIP $INSTANCE_IP has no external_mac and is in no nat_addresses — nothing will ARP for it"
        elif [ "$NAT_ADDR_TYPE" = "localnet" ]; then
            fail "EIP $INSTANCE_IP is advertised on the localnet port, where OVN ignores it"
        else
            pass "EIP $INSTANCE_IP is in nat_addresses on a $NAT_ADDR_TYPE port"
        fi
    fi
fi

# --- Guest egress, observed on the wire ---
# The assertion the smoke test cannot make: traffic that actually leaves the
# box, with the EIP as source rather than the host's own address.
echo "==> Guest egress"
SSH_KEY="$INVOKING_HOME/.ssh/spinifex-key"
if [ "${SKIP_EGRESS:-0}" = "1" ]; then
    skip "SKIP_EGRESS=1"
elif [ -z "$INSTANCE_IP" ] || [ "$INSTANCE_IP" = "None" ] || [ ! -f "$SSH_KEY" ]; then
    skip "no running instance or SSH key"
elif ! command -v tcpdump >/dev/null 2>&1; then
    skip "tcpdump not present"
else
    CAP=$(mktemp)
    # Capture only the outbound leg. Matching on the instance address instead
    # would also catch echo replies and ICMP redirects from the upstream
    # router, which can fill the count before a single request is seen.
    sudo timeout 15 tcpdump -l -n -i "$WAN_BRIDGE" -c 3 \
        "icmp and dst host 8.8.8.8" > "$CAP" 2>/dev/null &
    TCPDUMP_PID=$!
    sleep 2
    EGRESS_OUT=$(sudo -u "$INVOKING_USER" ssh -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o BatchMode=yes \
        -i "$SSH_KEY" "ubuntu@$INSTANCE_IP" 'ping -c 3 -W 2 8.8.8.8' 2>&1 || true)
    wait $TCPDUMP_PID 2>/dev/null

    if echo "$EGRESS_OUT" | grep -q '[1-3] received'; then
        pass "guest reached 8.8.8.8"
    else
        fail "guest could not reach 8.8.8.8: $(echo "$EGRESS_OUT" | tail -2 | tr '\n' ' ')"
    fi

    # The source address on the wire is what distinguishes a 1:1 EIP from the
    # host masquerading on the instance's behalf.
    EGRESS_SRC=$(awk '{for (i = 1; i < NF; i++) if ($i == "IP") { print $(i+1); exit }}' "$CAP")
    if [ "$UPLINK" = "nat" ]; then
        skip "routed NAT masquerades behind the host address by design"
    elif [ "$EGRESS_SRC" = "$INSTANCE_IP" ]; then
        pass "egress leaves $WAN_BRIDGE sourced from the EIP $INSTANCE_IP"
    elif [ -n "$EGRESS_SRC" ]; then
        fail "egress on $WAN_BRIDGE is sourced from $EGRESS_SRC, not the EIP $INSTANCE_IP"
    else
        fail "no egress to 8.8.8.8 seen on $WAN_BRIDGE — traffic never left the box"
    fi
    rm -f "$CAP"
fi

# --- Service bind matrix ---
echo "==> Service binding"
if [ "$BIND_PLANES" != "1" ]; then
    skip "DEV_BIND_PLANES != 1 (services are on the wildcard)"
elif [ -z "$WAN_IP" ]; then
    skip "no WAN address to test against"
else
    LISTENERS=$(ss -ltnH 2>/dev/null | awk '{print $4}')
    # Internal control planes must never answer on the public address.
    for port in 4222 6641 6642; do
        if echo "$LISTENERS" | grep -q "^${WAN_IP}:${port}$"; then
            fail "port $port is listening on the WAN address $WAN_IP"
        else
            pass "port $port is not on the WAN address"
        fi
    done
    # Public surfaces must still answer there.
    for port in 9999 8443; do
        if echo "$LISTENERS" | grep -qE "^(0\.0\.0\.0|\*|${WAN_IP}):${port}$"; then
            pass "port $port answers on the WAN address"
        else
            fail "port $port is not reachable on the WAN address $WAN_IP"
        fi
    done
    if echo "$LISTENERS" | grep -qE "^(0\.0\.0\.0|\*|${WAN_IP}):53$"; then
        pass "northstar answers :53 on the WAN address"
    else
        fail "northstar is not on ${WAN_IP}:53 — public DNS delegation moved off the wan plane"
    fi
fi

echo ""
if [ "$FAILURES" -eq 0 ]; then
    echo "✅ Network verification passed"
    exit 0
fi
echo "❌ Network verification failed ($FAILURES assertion(s))"
exit 1
