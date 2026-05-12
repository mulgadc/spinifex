#!/bin/bash

# OVN Compute Node Setup for Spinifex VPC Networking
#
# This script bootstraps a compute node for OVN-based VPC networking:
#   1. Installs OVN/OVS packages (if not present)
#   2. Enables required services (openvswitch-switch, ovn-controller)
#   3. Creates br-int with secure fail-mode
#   4. Configures WAN bridge for public subnet uplink (auto-detected or manual)
#   5. Configures OVS external_ids for OVN chassis identity
#   6. Applies sysctl tuning for overlay networking
#
# Usage:
#   ./scripts/setup-ovn.sh [options]
#
# Options:
#   --management         Also start OVN central services (NB DB, SB DB, ovn-northd)
#   --wan-bridge=NAME    OVS bridge for WAN traffic (default: auto-detect from default route)
#   --wan-iface=NAME     Physical NIC to add to the WAN bridge (use with --wan-bridge or --macvlan)
#   --macvlan            Create macvlan off --wan-iface instead of moving NIC directly.
#                        SSH-safe for single-NIC hosts where WAN NIC carries SSH.
#   --dhcp               Obtain gateway IP via DHCP on the WAN bridge interface
#   --mgmt-bridge=NAME   OVS bridge for system-instance control plane (default: br-mgmt)
#   --mgmt-cidr=CIDR     IPv4 CIDR to assign on the mgmt bridge (default: 10.15.8.1/24)
#   --mgmt-iface=NAME    Physical/virtual NIC to enslave to the mgmt bridge (multi-node only)
#   --no-mgmt-bridge     Skip mgmt bridge provisioning (for dev-networking hosts)
#   --ovn-remote=ADDR    OVN SB DB address (default: tcp:127.0.0.1:6642)
#   --encap-ip=IP        Geneve tunnel endpoint IP (default: auto-detect)
#
# WAN Bridge Auto-Detection:
#   When no --wan-bridge is given, the script checks the default route interface:
#   - If it's an OVS bridge → use it directly for bridge-mappings
#   - If it's a Linux bridge → create OVS br-ext + veth pair to link them
#     (non-destructive, Linux bridge keeps IP/routes, no interruption)
#   - If it's a physical NIC → stop and print guidance (cannot safely move NIC)
#
# Examples:
#   # WAN is already on a bridge (tofu-cluster, production):
#   ./scripts/setup-ovn.sh --management
#
#   # Dedicated WAN NIC (not your SSH NIC — you take responsibility):
#   ./scripts/setup-ovn.sh --management --wan-bridge=br-wan --wan-iface=eth1
#
#   # Single-NIC host (SSH-safe macvlan):
#   ./scripts/setup-ovn.sh --management --macvlan --wan-iface=eth0
#
#   # Compute node joining an existing cluster:
#   ./scripts/setup-ovn.sh --ovn-remote=tcp:10.0.0.1:6642 --encap-ip=10.0.0.2
#
#   # No WAN bridge (overlay-only, no public subnet):
#   ./scripts/setup-ovn.sh --management --encap-ip=10.0.0.1

set -e

# Defaults
MANAGEMENT=false
WAN_BRIDGE=""
WAN_IFACE=""
MACVLAN_MODE=false
EXTERNAL_DHCP=false
MGMT_BRIDGE_ENABLED=true
MGMT_BRIDGE="br-mgmt"
MGMT_CIDR="10.15.8.1/24"
MGMT_IFACE=""
OVN_REMOTE="tcp:127.0.0.1:6642"
ENCAP_IP=""

# Parse arguments
for arg in "$@"; do
    case "$arg" in
        --management)       MANAGEMENT=true ;;
        --macvlan)          MACVLAN_MODE=true ;;
        --dhcp)             EXTERNAL_DHCP=true ;;
        --wan-bridge=*)     WAN_BRIDGE="${arg#*=}" ;;
        --wan-iface=*)      WAN_IFACE="${arg#*=}" ;;
        --mgmt-bridge=*)    MGMT_BRIDGE="${arg#*=}" ;;
        --mgmt-cidr=*)      MGMT_CIDR="${arg#*=}" ;;
        --mgmt-iface=*)     MGMT_IFACE="${arg#*=}" ;;
        --no-mgmt-bridge)   MGMT_BRIDGE_ENABLED=false ;;
        --ovn-remote=*)     OVN_REMOTE="${arg#*=}" ;;
        --encap-ip=*)       ENCAP_IP="${arg#*=}" ;;
        --help|-h)
            head -50 "$0" | tail -48
            exit 0
            ;;
        *)
            echo "Unknown option: $arg"
            exit 1
            ;;
    esac
done

# --- WAN bridge auto-detection ---
# Determine the WAN bridge name and how to set it up.
WAN_BRIDGE_MODE=""  # "existing", "veth", "direct", "macvlan", or ""
LINUX_BRIDGE=""     # Set when WAN_BRIDGE_MODE="veth" — the Linux bridge behind the veth pair

detect_wan_bridge() {
    # If --wan-bridge was explicitly given, use it
    if [ -n "$WAN_BRIDGE" ]; then
        if [ "$MACVLAN_MODE" = true ] && [ -n "$WAN_IFACE" ]; then
            WAN_BRIDGE_MODE="macvlan"
        elif [ -n "$WAN_IFACE" ]; then
            WAN_BRIDGE_MODE="direct"
        elif sudo ovs-vsctl br-exists "$WAN_BRIDGE" 2>/dev/null; then
            WAN_BRIDGE_MODE="existing"
        else
            # Bridge doesn't exist yet and no --wan-iface — create empty OVS bridge
            WAN_BRIDGE_MODE="existing"
        fi
        return
    fi

    # If --macvlan was given without --wan-bridge, we need --wan-iface
    if [ "$MACVLAN_MODE" = true ]; then
        if [ -z "$WAN_IFACE" ]; then
            echo "ERROR: --macvlan requires --wan-iface=<NIC>"
            exit 1
        fi
        WAN_BRIDGE="br-wan"
        WAN_BRIDGE_MODE="macvlan"
        return
    fi

    # Auto-detect: find the default route interface
    local default_dev
    default_dev=$(ip -4 route show default 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="dev") print $(i+1)}' | head -1)

    if [ -z "$default_dev" ]; then
        echo "  No default route found — no WAN bridge configured"
        echo "  (VMs will not have external connectivity)"
        return
    fi

    # Check if the default route device is a bridge (Linux or OVS)
    local is_bridge=false
    if ip -d link show "$default_dev" 2>/dev/null | grep -q "bridge"; then
        is_bridge=true
    fi
    if sudo ovs-vsctl br-exists "$default_dev" 2>/dev/null; then
        is_bridge=true
    fi

    if [ "$is_bridge" = true ]; then
        if sudo ovs-vsctl br-exists "$default_dev" 2>/dev/null; then
            # Already an OVS bridge — use it directly for bridge-mappings
            WAN_BRIDGE="$default_dev"
            WAN_BRIDGE_MODE="existing"
            echo "  Auto-detected WAN bridge: $WAN_BRIDGE (OVS bridge, default route)"
        else
            # Linux bridge — link to OVS via veth pair (non-destructive)
            LINUX_BRIDGE="$default_dev"
            WAN_BRIDGE="br-ext"
            WAN_BRIDGE_MODE="veth"
            echo "  Auto-detected Linux bridge: $LINUX_BRIDGE (default route)"
            echo "  Will create OVS bridge br-ext + veth pair to link them"
        fi
        return
    fi

    # Default route is a physical NIC — cannot safely move it to OVS
    # because it might be carrying SSH.
    local wan_ip
    wan_ip=$(ip -4 -o addr show "$default_dev" 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1)

    echo ""
    echo "============================================================"
    echo "  WAN interface '$default_dev' ($wan_ip) is a physical NIC."
    echo "  Cannot auto-create a bridge — this may drop your connection."
    echo ""
    echo "  Options:"
    echo ""
    echo "  1. Create a WAN bridge first (e.g. via netplan), then re-run:"
    echo "     ./scripts/setup-ovn.sh --management"
    echo ""
    echo "  2. Dedicated WAN NIC (NOT your SSH connection):"
    echo "     ./scripts/setup-ovn.sh --management --wan-bridge=br-wan --wan-iface=$default_dev"
    echo ""
    echo "  3. Single-NIC host (SSH-safe macvlan):"
    echo "     ./scripts/setup-ovn.sh --management --macvlan --wan-iface=$default_dev"
    echo ""
    echo "  4. No external networking (overlay-only):"
    echo "     ./scripts/setup-ovn.sh --management --encap-ip=$wan_ip"
    echo "============================================================"
    echo ""
    exit 1
}

detect_wan_bridge

# Auto-detect encap IP if not specified
if [ -z "$ENCAP_IP" ]; then
    # Prefer br-vpc IP if it exists (dedicated VPC data plane)
    if ip -4 addr show br-vpc >/dev/null 2>&1; then
        ENCAP_IP=$(ip -4 -o addr show br-vpc 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1)
        if [ -n "$ENCAP_IP" ]; then
            echo "Auto-detected encap IP from br-vpc: $ENCAP_IP"
        fi
    fi
    # Fall back to default route source IP
    if [ -z "$ENCAP_IP" ]; then
        ENCAP_IP=$(ip -4 route get 8.8.8.8 2>/dev/null | awk '/src/{print $7}' | head -1)
        if [ -z "$ENCAP_IP" ]; then
            ENCAP_IP="127.0.0.1"
        fi
        echo "Auto-detected encap IP: $ENCAP_IP"
    fi
fi

echo "=== Spinifex OVN Compute Node Setup ==="
echo "  Management node:  $MANAGEMENT"
if [ -n "$WAN_BRIDGE" ]; then
    echo "  WAN bridge:       $WAN_BRIDGE ($WAN_BRIDGE_MODE)"
    if [ -n "$LINUX_BRIDGE" ]; then
        echo "  Linux bridge:     $LINUX_BRIDGE (linked via veth pair)"
    fi
    if [ -n "$WAN_IFACE" ]; then
        echo "  WAN interface:    $WAN_IFACE"
    fi
else
    echo "  WAN bridge:       none (overlay-only)"
fi
echo "  OVN Remote (SB):  $OVN_REMOTE"
echo "  Encap IP:         $ENCAP_IP"
echo ""

# --- Step 1: Install packages ---
echo "Step 1: Checking OVN/OVS packages..."

install_packages() {
    local missing=()
    for pkg in openvswitch-switch ovn-host; do
        if ! dpkg -s "$pkg" >/dev/null 2>&1; then
            missing+=("$pkg")
        fi
    done
    if [ "$MANAGEMENT" = true ]; then
        if ! dpkg -s ovn-central >/dev/null 2>&1; then
            missing+=("ovn-central")
        fi
    fi

    if [ ${#missing[@]} -gt 0 ]; then
        echo "  Installing: ${missing[*]}"
        sudo apt-get update -qq
        sudo apt-get install -y -qq "${missing[@]}"
    else
        echo "  All packages installed"
    fi
}

install_packages

# --- Step 2: Enable services ---
echo ""
echo "Step 2: Enabling services..."

sudo systemctl enable openvswitch-switch
sudo systemctl start openvswitch-switch
echo "  openvswitch-switch: started"

if [ "$MANAGEMENT" = true ]; then
    sudo systemctl enable ovn-central
    sudo systemctl start ovn-central
    echo "  ovn-central: started (NB DB + SB DB + ovn-northd)"

    # Wait for OVN NB DB socket to become available
    for i in $(seq 1 15); do
        if sudo ovn-nbctl --timeout=2 get-connection >/dev/null 2>&1; then
            break
        fi
        echo "  Waiting for OVN NB DB... ($i/15)"
        sleep 1
    done

    # Allow remote connections to NB and SB databases
    sudo ovn-nbctl set-connection ptcp:6641
    sudo ovn-sbctl set-connection ptcp:6642
    echo "  OVN NB DB listening on tcp:6641"
    echo "  OVN SB DB listening on tcp:6642"
fi

# --- Step 3: Create and configure br-int ---
echo ""
echo "Step 3: Configuring br-int..."

sudo ovs-vsctl --may-exist add-br br-int
sudo ovs-vsctl set Bridge br-int fail-mode=secure
sudo ovs-vsctl set Bridge br-int other-config:disable-in-band=true
sudo ip link set br-int up
echo "  br-int: created, fail-mode=secure, up"

# Mark OVS internal netdevs Unmanaged for systemd-networkd. Without this,
# Trixie's networkd takes ownership of any unconfigured iface and may bring
# br-int/br-ext admin-down after setup-ovn.sh's `ip link set up` (no Match
# rule => default management => no carrier => link down). OVS dataplane
# still forwards, but ovn-controller flow programming and any tooling that
# probes link state misbehave. Unmanaged=yes keeps OVS in sole control.
# (mulga-siv-37)
OVS_INTERNAL_NET=/etc/systemd/network/05-spinifex-ovs-internal.network
if [ ! -f "$OVS_INTERNAL_NET" ]; then
    sudo tee "$OVS_INTERNAL_NET" >/dev/null <<'NETWORK'
[Match]
Name=br-int br-ext

[Link]
Unmanaged=yes
NETWORK
    sudo networkctl reload 2>/dev/null || true
    echo "  wrote $OVS_INTERNAL_NET (Unmanaged=yes for br-int br-ext)"
fi

# --- Step 3b: Configure WAN bridge for public subnet uplink ---
if [ -n "$WAN_BRIDGE" ]; then
    echo ""
    echo "Step 3b: Configuring WAN bridge ($WAN_BRIDGE) for public subnet uplink..."

    # Rip down any stale veth persistence from a previous veth-mode install
    # when switching to any non-veth mode. Idempotent — each command uses
    # --if-exists / 2>/dev/null to tolerate absence. Without this, the
    # veth pair re-materialises on reboot and fights the current mode's
    # bridge plumbing (Fix 1, mulga-998.b, per D17).
    if [ "$WAN_BRIDGE_MODE" != "veth" ]; then
        sudo rm -f /etc/systemd/network/14-spinifex-br-wan.netdev \
                   /etc/systemd/network/15-spinifex-veth-wan.netdev \
                   /etc/systemd/network/15-spinifex-veth-wan.network \
                   /etc/systemd/network/16-spinifex-veth-wan-ovs.network
        sudo networkctl reload 2>/dev/null || true
        sudo ovs-vsctl --if-exists del-port "$WAN_BRIDGE" veth-wan-ovs
        sudo ip link del veth-wan-br 2>/dev/null || true
    fi

    case "$WAN_BRIDGE_MODE" in
        existing)
            # Already an OVS bridge (from a previous run or explicit --wan-bridge).
            if ! sudo ovs-vsctl br-exists "$WAN_BRIDGE" 2>/dev/null; then
                sudo ovs-vsctl --may-exist add-br "$WAN_BRIDGE"
                echo "  created OVS bridge: $WAN_BRIDGE"
            fi
            sudo ip link set "$WAN_BRIDGE" up
            echo "  $WAN_BRIDGE: OVS bridge, up"
            ;;

        veth)
            # Linux bridge detected (e.g. br-wan from cloud-init/netplan).
            # OVN bridge-mappings require an OVS bridge. Rather than converting
            # the Linux bridge (destructive, causes WAN interruption), we create
            # a separate OVS bridge and link them with a veth pair:
            #
            #   br-wan (Linux, keeps IP/routes) ←→ veth pair ←→ br-ext (OVS, for OVN)
            #
            # No network interruption. The Linux bridge is untouched.

            # Create OVS bridge
            if ! sudo ovs-vsctl br-exists "$WAN_BRIDGE" 2>/dev/null; then
                sudo ovs-vsctl --may-exist add-br "$WAN_BRIDGE"
                echo "  created OVS bridge: $WAN_BRIDGE"
            fi
            sudo ip link set "$WAN_BRIDGE" up

            # Create veth pair (idempotent)
            if ! ip link show veth-wan-br >/dev/null 2>&1; then
                sudo ip link add veth-wan-br type veth peer name veth-wan-ovs
                echo "  created veth pair: veth-wan-br ↔ veth-wan-ovs"
            else
                echo "  veth pair already exists: veth-wan-br ↔ veth-wan-ovs"
            fi

            # Enslave veth-wan-br to the Linux bridge
            if ! ip link show veth-wan-br 2>/dev/null | grep -q "master $LINUX_BRIDGE"; then
                sudo ip link set veth-wan-br master "$LINUX_BRIDGE"
                echo "  veth-wan-br → $LINUX_BRIDGE (Linux bridge)"
            fi
            sudo ip link set veth-wan-br up

            # Add veth-wan-ovs to the OVS bridge
            if ! sudo ovs-vsctl port-to-br veth-wan-ovs >/dev/null 2>&1; then
                sudo ovs-vsctl --may-exist add-port "$WAN_BRIDGE" veth-wan-ovs
                echo "  veth-wan-ovs → $WAN_BRIDGE (OVS bridge)"
            fi
            sudo ip link set veth-wan-ovs up

            echo "  $LINUX_BRIDGE (Linux) ↔ veth pair ↔ $WAN_BRIDGE (OVS)"
            echo "  $LINUX_BRIDGE keeps its IP and routes — no interruption"

            # Persist the veth pair across reboot via systemd-networkd. Veths
            # are kernel-only and vanish on reboot; without persistence vpcd
            # starts with the OVS port pointing at a nonexistent peer and
            # silently falls back to direct mode (Fix 1, mulga-998.b).
            #
            # networkd's Bridge= directive requires the target bridge to be a
            # known NetDev. On ISO-installed nodes the installer writes
            # 11-spinifex-br-wan.netdev, which the gate below detects and
            # skips this write. On binary-installed nodes where the operator
            # manages br-wan outside networkd (e.g. cloud-init), this file
            # fills the gap so veth-wan-br resolves its bridge on reboot.
            # `Failed to create netdev: File exists` is harmless — networkd
            # matches the existing kernel bridge by name+kind.
            #
            # Gate: skip the write if any .netdev (installer, cloud-init,
            # netplan, manual) already declares this bridge — don't clobber
            # existing networkd config. networkd searches /etc, /run, /usr/lib;
            # check all three. Our own file is excluded so idempotent re-runs
            # still rewrite when needed.
            BR_WAN_NETDEV="/etc/systemd/network/14-spinifex-br-wan.netdev"
            VETH_NETDEV="/etc/systemd/network/15-spinifex-veth-wan.netdev"
            VETH_NETWORK="/etc/systemd/network/15-spinifex-veth-wan.network"
            VETH_OVS_NETWORK="/etc/systemd/network/16-spinifex-veth-wan-ovs.network"
            EXISTING_BR_NETDEV=$(grep -rls --include="*.netdev" -E "^\s*Name=$LINUX_BRIDGE\s*$" \
                /etc/systemd/network /run/systemd/network /usr/lib/systemd/network 2>/dev/null \
                | grep -v "^$BR_WAN_NETDEV$" || true)
            if [ -n "$EXISTING_BR_NETDEV" ]; then
                echo "  skipping $BR_WAN_NETDEV — operator-managed NetDev already declares $LINUX_BRIDGE: $EXISTING_BR_NETDEV"
            else
                sudo tee "$BR_WAN_NETDEV" >/dev/null <<NETDEV
[NetDev]
Name=$LINUX_BRIDGE
Kind=bridge
NETDEV
            fi
            sudo tee "$VETH_NETDEV" >/dev/null <<NETDEV
[NetDev]
Name=veth-wan-br
Kind=veth

[Peer]
Name=veth-wan-ovs
NETDEV
            sudo tee "$VETH_NETWORK" >/dev/null <<NETWORK
[Match]
Name=veth-wan-br

[Network]
Bridge=$LINUX_BRIDGE
ConfigureWithoutCarrier=yes
NETWORK
            # Second unit admin-ups the OVS end of the pair. OVS owns the port
            # (enslaved via ovs-vsctl add-port above) but does not flip admin
            # state on external ports — that's networkd's job. Without this,
            # veth-wan-ovs stays DOWN after reboot, peer goes LOWERLAYERDOWN,
            # br-wan loses carrier (Fix 1 follow-up, mulga-998.b).
            sudo tee "$VETH_OVS_NETWORK" >/dev/null <<NETWORK
[Match]
Name=veth-wan-ovs

[Link]
RequiredForOnline=no

[Network]
ConfigureWithoutCarrier=yes
NETWORK
            sudo networkctl reload 2>/dev/null || true
            echo "  wrote $VETH_NETDEV + $VETH_NETWORK + $VETH_OVS_NETWORK (veth persists on reboot)"
            ;;

        direct)
            # Add WAN NIC directly to OVS bridge. The NIC becomes an OVS slave —
            # its IP (if any) is no longer reachable from the host. The user has
            # confirmed this NIC is NOT their SSH connection.
            if ! ip link show "$WAN_IFACE" >/dev/null 2>&1; then
                echo "  ERROR: interface $WAN_IFACE does not exist"
                echo "  Available interfaces:"
                ip -o link show | awk -F': ' '{print "    " $2}'
                exit 1
            fi

            sudo ovs-vsctl --may-exist add-br "$WAN_BRIDGE"
            sudo ip link set "$WAN_BRIDGE" up

            if sudo ovs-vsctl port-to-br "$WAN_IFACE" >/dev/null 2>&1; then
                echo "  $WAN_IFACE already on $(sudo ovs-vsctl port-to-br "$WAN_IFACE")"
            else
                sudo ovs-vsctl --may-exist add-port "$WAN_BRIDGE" "$WAN_IFACE"
                echo "  added $WAN_IFACE directly to $WAN_BRIDGE"
            fi
            sudo ip link set "$WAN_IFACE" up
            echo "  $WAN_BRIDGE: direct bridge on $WAN_IFACE"
            echo "  NOTE: $WAN_IFACE is now an OVS port — no host IP on this NIC"
            ;;

        macvlan)
            # Create a macvlan sub-interface in bridge mode off the WAN NIC.
            # The host keeps its IP on the parent NIC — SSH-safe. OVN localnet
            # traffic flows through the macvlan to the physical wire.
            if ! ip link show "$WAN_IFACE" >/dev/null 2>&1; then
                echo "  ERROR: interface $WAN_IFACE does not exist"
                echo "  Available interfaces:"
                ip -o link show | awk -F': ' '{print "    " $2}'
                exit 1
            fi

            MACVLAN_NAME="spx-ext-${WAN_IFACE}"

            sudo ovs-vsctl --may-exist add-br "$WAN_BRIDGE"
            sudo ip link set "$WAN_BRIDGE" up

            if ip link show "$MACVLAN_NAME" >/dev/null 2>&1; then
                echo "  macvlan $MACVLAN_NAME already exists"
            else
                sudo ip link add "$MACVLAN_NAME" link "$WAN_IFACE" type macvlan mode bridge
                echo "  created macvlan: $MACVLAN_NAME (bridge mode) on $WAN_IFACE"
            fi

            sudo ip link set "$MACVLAN_NAME" up
            sudo ovs-vsctl --may-exist add-port "$WAN_BRIDGE" "$MACVLAN_NAME"
            echo "  $WAN_BRIDGE: macvlan port $MACVLAN_NAME on $WAN_IFACE"
            echo "  NOTE: host keeps its IP on $WAN_IFACE (SSH-safe)"
            echo "  QUIRK: host cannot reach VMs at their public IPs (macvlan isolation)"
            ;;
    esac

    # --- DHCP: obtain gateway IP for OVN SNAT ---
    if [ "$EXTERNAL_DHCP" = true ]; then
        echo ""
        echo "Step 3c: Obtaining external gateway IP via DHCP..."

        # For macvlan mode, DHCP on the macvlan interface (it has L2 access to WAN).
        # For direct/existing bridge, DHCP on the bridge itself.
        if [ "$WAN_BRIDGE_MODE" = "macvlan" ]; then
            DHCP_IFACE="spx-ext-${WAN_IFACE}"
        else
            DHCP_IFACE="$WAN_BRIDGE"
        fi

        # Run DHCP client to get a lease
        if command -v dhcpcd >/dev/null 2>&1; then
            sudo dhcpcd --waitip=4 --timeout 15 "$DHCP_IFACE" 2>/dev/null || true
        elif command -v dhclient >/dev/null 2>&1; then
            sudo dhclient -1 -timeout 15 "$DHCP_IFACE" 2>/dev/null || true
        else
            echo "  WARNING: no DHCP client found (dhcpcd or dhclient)"
            echo "  Install dhcpcd-base or isc-dhcp-client, or set gateway_ip manually"
        fi

        # Read the obtained IP
        DHCP_IP=$(ip -4 addr show dev "$DHCP_IFACE" 2>/dev/null | awk '/inet /{print $2}' | head -1 | cut -d/ -f1)
        if [ -n "$DHCP_IP" ]; then
            echo "  DHCP obtained: $DHCP_IP on $DHCP_IFACE"

            # Write the gateway IP to the spinifex config so vpcd can use it
            CONFIG_DIR="${CONFIG_DIR:-$HOME/spinifex/config}"
            CONFIG_FILE="$CONFIG_DIR/spinifex.toml"
            if [ -f "$CONFIG_FILE" ]; then
                if grep -q "gateway_ip" "$CONFIG_FILE"; then
                    sed -i "s/gateway_ip.*/gateway_ip = \"$DHCP_IP\"/" "$CONFIG_FILE"
                else
                    sed -i "/^gateway *=.*/a gateway_ip  = \"$DHCP_IP\"" "$CONFIG_FILE"
                fi
                echo "  Updated $CONFIG_FILE with gateway_ip = $DHCP_IP"
            else
                echo "  WARNING: $CONFIG_FILE not found — set gateway_ip manually"
            fi
        else
            echo "  WARNING: DHCP failed to obtain IP on $DHCP_IFACE"
            echo "  VMs will not have external connectivity until gateway_ip is configured"
        fi
    fi
fi

# --- Step 3d: Management bridge for system-instance control plane ---
# br-mgmt is an OVS bridge (not L2-learning Linux bridge, not part of OVN
# overlay). fail-mode=standalone makes it behave like a plain L2 switch
# without OVN flows; it is excluded from ovn-bridge-mappings so
# ovn-controller ignores it. System instance TAPs attach via ovs-vsctl
# add-port (see SetupMgmtTapDevice in daemon/network.go).
if [ "$MGMT_BRIDGE_ENABLED" = true ]; then
    echo ""
    echo "Step 3d: Configuring management bridge ($MGMT_BRIDGE)..."

    sudo ovs-vsctl --may-exist add-br "$MGMT_BRIDGE"
    sudo ovs-vsctl set Bridge "$MGMT_BRIDGE" \
        fail-mode=standalone \
        other-config:disable-in-band=true
    sudo ip link set "$MGMT_BRIDGE" up
    echo "  $MGMT_BRIDGE: OVS bridge, fail-mode=standalone, up"

    # Enslave physical/virtual mgmt NIC when provided (multi-node deployments
    # where the node's mgmt subnet spans multiple hosts).
    if [ -n "$MGMT_IFACE" ]; then
        if ! ip link show "$MGMT_IFACE" >/dev/null 2>&1; then
            echo "  ERROR: mgmt interface $MGMT_IFACE does not exist"
            ip -o link show | awk -F': ' '{print "    " $2}'
            exit 1
        fi

        # Drop any existing IP on the NIC — IP belongs on the bridge.
        sudo ip addr flush dev "$MGMT_IFACE" || true
        sudo ovs-vsctl --may-exist add-port "$MGMT_BRIDGE" "$MGMT_IFACE"
        sudo ip link set "$MGMT_IFACE" up
        echo "  $MGMT_IFACE: port on $MGMT_BRIDGE"
    fi

    if [ -n "$MGMT_CIDR" ]; then
        sudo ip addr replace "$MGMT_CIDR" dev "$MGMT_BRIDGE"
        echo "  $MGMT_BRIDGE: address $MGMT_CIDR"

        # Persist the IP across reboots via a systemd-networkd drop-in. The
        # OVS bridge definition itself survives in ovsdb; only the L3 IP
        # needs re-applying on boot.
        MGMT_NETD_UNIT="/etc/systemd/network/10-spinifex-mgmt.network"
        sudo tee "$MGMT_NETD_UNIT" >/dev/null <<NETD
[Match]
Name=$MGMT_BRIDGE

[Network]
Address=$MGMT_CIDR
ConfigureWithoutCarrier=yes
NETD
        echo "  wrote $MGMT_NETD_UNIT (IP persists on reboot via systemd-networkd)"
    fi
fi

# --- Step 4: Configure OVN external_ids ---
echo ""
echo "Step 4: Setting OVS external_ids for OVN..."

if [ -n "$WAN_BRIDGE" ]; then
    BRIDGE_MAPPINGS="external:${WAN_BRIDGE}"
    sudo ovs-vsctl set Open_vSwitch . \
        external_ids:ovn-remote="$OVN_REMOTE" \
        external_ids:ovn-encap-ip="$ENCAP_IP" \
        external_ids:ovn-encap-type="geneve" \
        external_ids:ovn-bridge-mappings="$BRIDGE_MAPPINGS"
    echo "  ovn-bridge-mappings: $BRIDGE_MAPPINGS"
else
    sudo ovs-vsctl set Open_vSwitch . \
        external_ids:ovn-remote="$OVN_REMOTE" \
        external_ids:ovn-encap-ip="$ENCAP_IP" \
        external_ids:ovn-encap-type="geneve"
fi

# system-id is owned by the openvswitch-switch package (persisted in
# /etc/openvswitch/system-id.conf and re-applied on every boot). Read it back
# rather than overriding — overriding here would drift from the on-disk value
# and the next reboot would silently flip the chassis identity (mulga-999).
echo "  system-id:      $(sudo ovs-vsctl get open . external_ids:system-id)"
echo "  ovn-remote:     $OVN_REMOTE"
echo "  ovn-encap-ip:   $ENCAP_IP"
echo "  ovn-encap-type: geneve"

# --- Step 5: Start ovn-controller ---
echo ""
echo "Step 5: Starting ovn-controller..."

# Set ovn-controller file log level to WARN so it doesn't spam the log with
# connection-retry INFO messages ("OVNSB commit failed") when the SB DB
# isn't running. Uses a systemd ExecStartPost so it persists across restarts.
OVN_CTRL_OVERRIDE="/etc/systemd/system/ovn-controller.service.d/log-level.conf"
sudo mkdir -p "$(dirname "$OVN_CTRL_OVERRIDE")"
sudo tee "$OVN_CTRL_OVERRIDE" >/dev/null <<'OVERRIDE'
[Service]
ExecStartPost=/bin/sh -c 'OVS_RUNDIR=/var/run/ovn exec /usr/bin/ovs-appctl -t ovn-controller vlog/set file:warn'
OVERRIDE
sudo systemctl daemon-reload
echo "  ovn-controller log level: file:warn (via systemd drop-in)"

sudo systemctl restart ovn-controller
echo "  ovn-controller: started"

# --- Step 6: Sysctl tuning ---
echo ""
echo "Step 6: Applying sysctl for overlay networking..."

sudo tee /etc/sysctl.d/99-spinifex-vpc.conf >/dev/null <<'SYSCTL'
# Spinifex VPC networking: enable IP forwarding and disable rp_filter
# for Geneve overlay traffic on OVS bridges.
net.ipv4.ip_forward=1
net.ipv4.conf.all.rp_filter=0
net.ipv4.conf.default.rp_filter=0
SYSCTL
sudo sysctl --system -q
echo "  ip_forward=1, rp_filter=0"

# --- Step 6b: Ensure data NIC routing for Geneve tunnels ---
echo ""
echo "Step 6b: Configuring data NIC routing for Geneve tunnels..."

# When management and data NICs share the same subnet (e.g. both on 10.1.0.0/16),
# the kernel may route Geneve tunnel traffic through the management NIC with the
# wrong source IP. This causes remote OVS nodes to drop incoming tunnel packets
# because the source IP doesn't match the configured tunnel remote_ip.
# Fix: lower the route metric on the data NIC so it's preferred.
DATA_IFACE=$(ip -o -4 addr show | awk -v ip="$ENCAP_IP" '$0 ~ ip"/" {print $2}')
if [ -n "$DATA_IFACE" ]; then
    SUBNET=$(ip -o -4 route show dev "$DATA_IFACE" proto kernel scope link | awk '{print $1}' | head -1)
    if [ -n "$SUBNET" ]; then
        sudo ip route replace "$SUBNET" dev "$DATA_IFACE" src "$ENCAP_IP" metric 50
        echo "  data route: $SUBNET via $DATA_IFACE src $ENCAP_IP (metric 50)"
    else
        echo "  skipped: no kernel route found for $DATA_IFACE"
    fi
else
    echo "  skipped: could not find interface for $ENCAP_IP"
fi

# --- Step 7: Verify Geneve kernel support ---
echo ""
echo "Step 7: Verifying Geneve kernel module..."

if sudo modprobe geneve 2>/dev/null; then
    echo "  geneve module: loaded"
else
    echo "  WARNING: geneve module not available (tunnels may not work)"
fi

# --- Step 8: Grant non-root access to OVS/OVN ---
echo ""
echo "Step 8: Configuring non-root access..."

# Open OVS DB socket so non-root processes can use ovs-vsctl
OVS_SOCK="/var/run/openvswitch/db.sock"
if [ -S "$OVS_SOCK" ]; then
    sudo chmod 0666 "$OVS_SOCK"
    echo "  OVS DB socket: opened ($OVS_SOCK)"
fi

# Open OVN runtime directory and ctl sockets for ovs-appctl access
if [ -d "/var/run/ovn" ]; then
    sudo chmod 0755 /var/run/ovn
    sudo chmod 0666 /var/run/ovn/*.ctl 2>/dev/null || true
    echo "  OVN ctl sockets: opened (/var/run/ovn/)"
fi
if [ -d "/var/run/openvswitch" ]; then
    sudo chmod 0666 /var/run/openvswitch/*.ctl 2>/dev/null || true
fi

# Create persistent systemd override so permissions survive OVS restarts
OVERRIDE_DIR="/etc/systemd/system/openvswitch-switch.service.d"
if [ ! -f "$OVERRIDE_DIR/spinifex-perms.conf" ]; then
    sudo mkdir -p "$OVERRIDE_DIR"
    sudo tee "$OVERRIDE_DIR/spinifex-perms.conf" >/dev/null <<'OVERRIDE'
[Service]
ExecStartPost=/bin/chmod 0666 /var/run/openvswitch/db.sock
OVERRIDE
    sudo systemctl daemon-reload
    echo "  systemd override: created (db.sock permissions persist across restarts)"
else
    echo "  systemd override: already exists"
fi

# Sudoers rules for spinifex-daemon and spinifex-vpcd are managed by setup.sh
# (install_sudoers). Skip writing here to avoid conflicts.
SUDOERS_FILE="/etc/sudoers.d/spinifex-network"
if [ -f "$SUDOERS_FILE" ]; then
    echo "  sudoers rule: already exists ($SUDOERS_FILE, managed by setup.sh)"
else
    echo "  sudoers rule: not found — run setup.sh first, or install manually"
fi

# --- Step 9: Configure OVN log rotation ---
# The ovn-common package provides /etc/logrotate.d/ovn-common which handles
# rotation and vlog/reopen. We just add maxsize + rotate to cap disk usage.
echo ""
echo "Step 9: Configuring OVN log rotation..."

OVN_LOGROTATE="/etc/logrotate.d/ovn-common"
if [ -f "$OVN_LOGROTATE" ]; then
    if ! grep -q 'maxsize' "$OVN_LOGROTATE"; then
        sudo sed -i '/^\/var\/log\/ovn\/\*\.log {/a\    rotate 5\n    maxsize 100M' "$OVN_LOGROTATE"
        echo "  added maxsize 100M + rotate 5 to $OVN_LOGROTATE"
    else
        echo "  $OVN_LOGROTATE already has maxsize configured"
    fi
else
    echo "  WARNING: $OVN_LOGROTATE not found — install ovn-common package"
fi

# Remove our old custom config if present (superseded by patching ovn-common)
if [ -f /etc/logrotate.d/ovn-spinifex ]; then
    sudo rm -f /etc/logrotate.d/ovn-spinifex
    echo "  removed obsolete /etc/logrotate.d/ovn-spinifex"
fi

# --- Step 10: Enable auto-start on boot ---
# OVN services should start with the system in production. ovn-controller
# retries when the SB DB isn't ready; file log level is set to WARN (Step 5)
# to prevent log spam during those retries.
echo ""
echo "Step 10: Enabling OVN auto-start on boot..."
sudo systemctl enable openvswitch-switch 2>/dev/null || true
sudo systemctl enable ovn-controller 2>/dev/null || true
echo "  openvswitch-switch: enabled on boot"
echo "  ovn-controller: enabled on boot"

# --- Step 11: Health check ---
echo ""
echo "Step 11: Verifying setup..."

OK=true

# Check br-int
if sudo ovs-vsctl br-exists br-int; then
    echo "  br-int:          OK"
else
    echo "  br-int:          FAILED"
    OK=false
fi

# Check WAN bridge (only if configured)
if [ -n "$WAN_BRIDGE" ]; then
    if sudo ovs-vsctl br-exists "$WAN_BRIDGE"; then
        echo "  $WAN_BRIDGE:$(printf '%*s' $((15 - ${#WAN_BRIDGE})) '') OK"
        if [ "$WAN_BRIDGE_MODE" = "veth" ]; then
            if ip link show veth-wan-br >/dev/null 2>&1 && ip link show veth-wan-ovs >/dev/null 2>&1; then
                echo "  veth pair:       OK (veth-wan-br ↔ veth-wan-ovs)"
                echo "  linux bridge:    $LINUX_BRIDGE (untouched)"
            else
                echo "  veth pair:       FAILED (veth-wan-br/veth-wan-ovs not found)"
                OK=false
            fi
        elif [ "$WAN_BRIDGE_MODE" = "direct" ]; then
            if sudo ovs-vsctl port-to-br "$WAN_IFACE" >/dev/null 2>&1; then
                echo "  direct bridge:   OK ($WAN_IFACE on $WAN_BRIDGE)"
            else
                echo "  direct bridge:   FAILED ($WAN_IFACE not on $WAN_BRIDGE)"
                OK=false
            fi
        elif [ "$WAN_BRIDGE_MODE" = "macvlan" ]; then
            MACVLAN_NAME="spx-ext-${WAN_IFACE}"
            if ip link show "$MACVLAN_NAME" >/dev/null 2>&1; then
                echo "  macvlan:         OK ($MACVLAN_NAME)"
            else
                echo "  macvlan:         FAILED ($MACVLAN_NAME not found)"
                OK=false
            fi
        fi
    else
        echo "  $WAN_BRIDGE:$(printf '%*s' $((15 - ${#WAN_BRIDGE})) '') FAILED"
        OK=false
    fi
fi

# Check mgmt bridge (only if enabled)
if [ "$MGMT_BRIDGE_ENABLED" = true ]; then
    if sudo ovs-vsctl br-exists "$MGMT_BRIDGE"; then
        MGMT_IP_ACTUAL=$(ip -4 -o addr show dev "$MGMT_BRIDGE" 2>/dev/null | awk '{print $4}' | head -1)
        echo "  $MGMT_BRIDGE:$(printf '%*s' $((15 - ${#MGMT_BRIDGE})) '') OK (${MGMT_IP_ACTUAL:-no IP})"
    else
        echo "  $MGMT_BRIDGE:$(printf '%*s' $((15 - ${#MGMT_BRIDGE})) '') FAILED"
        OK=false
    fi
fi

# Check ovn-controller
if sudo ovs-appctl -t ovn-controller version >/dev/null 2>&1 || systemctl is-active --quiet ovn-controller 2>/dev/null; then
    echo "  ovn-controller:  OK"
else
    echo "  ovn-controller:  FAILED (may still be starting)"
    OK=false
fi

# Check chassis registration (may take a moment)
if [ "$MANAGEMENT" = true ]; then
    sleep 2
    CHASSIS_COUNT=$(sudo ovn-sbctl show 2>/dev/null | grep -c "Chassis" || true)
    echo "  chassis count:   $CHASSIS_COUNT"
fi

echo ""
if [ "$OK" = true ]; then
    echo "=== OVN compute node setup complete ==="
else
    echo "=== Setup completed with warnings (check above) ==="
fi
