#!/bin/bash
# reset-dev-env.sh — Single-node dev environment reset.
#
# Stops services, wipes production state (/etc/spinifex, /var/lib/spinifex,
# /var/log/spinifex, /run/spinifex), rebuilds from source, reinstalls via
# dev-install.sh, and relaunches a smoke-test instance.
#
# Preserves node/network settings from the existing /etc/spinifex/spinifex.toml
# so the reset restores the current topology (region, AZ, external mode,
# pool range) rather than applying defaults. On a fresh box with no config,
# falls back to ap-southeast-2 + pool mode derived from the WAN subnet.
#
# WARNING: setup-ovn.sh converts the WAN NIC into an OVS bridge. On hosts
# where the WAN NIC is also the SSH NIC, SSH will drop mid-run. Run from
# the console or via a separate management NIC.
#
# Single-node only. Refuses on multi-node clusters.

set -euo pipefail

# --- Paths (production layout from setup.sh) ---
ETC_DIR=/etc/spinifex
DATA_DIR=/var/lib/spinifex
LOG_DIR=/var/log/spinifex
RUN_DIR=/run/spinifex
CONFIG_FILE="$ETC_DIR/spinifex.toml"

# --- Script context ---
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Resolve invoking user's HOME. $HOME is unsafe under sudo (resolves to /root).
INVOKING_USER="${SUDO_USER:-$(id -un)}"
INVOKING_HOME=$(getent passwd "$INVOKING_USER" | cut -d: -f6)
if [ -z "$INVOKING_HOME" ]; then
    echo "❌ Could not resolve home directory for user: $INVOKING_USER"
    exit 1
fi

# --- Guard: refuse to run on multi-node clusters ---
if sudo test -f "$CONFIG_FILE"; then
    NODE_COUNT=$(sudo grep -cE '^\[nodes\.[^.]+\]' "$CONFIG_FILE" 2>/dev/null || echo 0)
    if [ "$NODE_COUNT" -gt 1 ]; then
        echo "❌ Multi-node cluster detected ($NODE_COUNT nodes in $CONFIG_FILE)."
        echo "   This script only supports single-node dev environments."
        echo "   Reset each node individually or use 'spx admin cluster shutdown'."
        exit 1
    fi
fi

# --- Capture settings from existing config ---
# Parse before the wipe. Missing fields fall back to defaults below.
REGION=""
AZ=""
EXT_MODE=""
POOL_START=""
POOL_END=""
EXT_GATEWAY=""
EXT_PREFIX=""
OPERATOR_EMAIL=""

if sudo test -f "$CONFIG_FILE"; then
    # Copy config to a temp file we can read without sudo for cleaner parsing.
    TMP_CFG=$(mktemp)
    trap 'rm -f "$TMP_CFG"' EXIT
    sudo cat "$CONFIG_FILE" > "$TMP_CFG"

    # toml_scalar <section-regex> <key> — grabs first matching scalar from the
    # first matching section. Strips surrounding quotes if present.
    toml_scalar() {
        local section_re="$1"
        local key="$2"
        awk -v sec="$section_re" -v key="$key" '
            $0 ~ sec            { in_sec=1; next }
            /^\[/               { in_sec=0 }
            in_sec && $0 ~ "^[[:space:]]*" key "[[:space:]]*=" {
                sub(/^[^=]*=[[:space:]]*/, "")
                gsub(/[[:space:]]*#.*$/, "")
                gsub(/^"/, ""); gsub(/"$/, "")
                print; exit
            }' "$TMP_CFG"
    }

    REGION=$(toml_scalar '^\[nodes\.'        'region')
    AZ=$(toml_scalar     '^\[nodes\.'        'az')
    EXT_MODE=$(toml_scalar '^\[network\]'    'external_mode')
    POOL_START=$(toml_scalar  '^\[\[network\.external_pools\]\]' 'range_start')
    POOL_END=$(toml_scalar    '^\[\[network\.external_pools\]\]' 'range_end')
    EXT_GATEWAY=$(toml_scalar '^\[\[network\.external_pools\]\]' 'gateway')
    EXT_PREFIX=$(toml_scalar  '^\[\[network\.external_pools\]\]' 'prefix_len')
    OPERATOR_EMAIL=$(toml_scalar '^\[operator\]' 'email')
fi

# Allow operator to override on the command line (e.g. first reset on a box
# installed before --email existed): SPINIFEX_EMAIL=me@example.com ./reset-dev-env.sh
OPERATOR_EMAIL="${SPINIFEX_EMAIL:-$OPERATOR_EMAIL}"

# Defaults
REGION="${REGION:-ap-southeast-2}"
AZ="${AZ:-${REGION}a}"
EXT_MODE="${EXT_MODE:-pool}"

echo "Preserving: region=$REGION az=$AZ external_mode=$EXT_MODE"
if [ "$EXT_MODE" = "pool" ] && [ -n "$POOL_START" ]; then
    echo "  pool: $POOL_START - $POOL_END  gw=$EXT_GATEWAY  prefix=$EXT_PREFIX"
fi
if [ -n "$OPERATOR_EMAIL" ]; then
    echo "  operator email: $OPERATOR_EMAIL"
fi

# --- Shutdown services ---
echo "==> Stopping services"
sudo systemctl stop spinifex.target 2>/dev/null || true
sudo systemctl reset-failed 'spinifex-*' 2>/dev/null || true
sudo pkill -x qemu-system-x86_64 2>/dev/null || true
sudo pkill -x qemu-system-aarch64 2>/dev/null || true

# Wait for QEMU to fully exit before tearing down viperblock state.
timeout=30
elapsed=0
while pgrep -x 'qemu-system-x86_64|qemu-system-aarch64' > /dev/null 2>&1; do
    if [ "$elapsed" -ge "$timeout" ]; then
        echo "❌ QEMU still running after ${timeout}s:"
        pgrep -af 'qemu-system-' || true
        echo "   Kill them manually and re-run this script."
        exit 1
    fi
    sleep 1
    elapsed=$((elapsed + 1))
done

# --- Clean OVS / OVN ---
echo "==> Cleaning OVS bridges"
if command -v ovs-vsctl >/dev/null 2>&1; then
    sudo systemctl start openvswitch-switch 2>/dev/null || true
    sleep 1
    for br in $(sudo ovs-vsctl list-br 2>/dev/null); do
        echo "  Deleting bridge: $br"
        sudo ovs-vsctl --if-exists del-br "$br"
    done
    sudo ovs-vsctl --if-exists clear Open_vSwitch . external_ids 2>/dev/null || true
    sudo systemctl stop openvswitch-switch 2>/dev/null || true
fi

# Delete OVN DB files outright — setup-ovn.sh will restart ovn-central with
# fresh empty DBs. Eliminates stale SB state (chassis entries, port bindings)
# that accumulates across resets and triggers ovn-controller commit loops.
echo "==> Removing OVN database files"
sudo systemctl stop ovn-central 2>/dev/null || true
sudo systemctl stop ovn-controller 2>/dev/null || true
if [ -d /var/lib/ovn ]; then
    sudo rm -f /var/lib/ovn/ovnnb_db.db /var/lib/ovn/ovnsb_db.db
fi

# veth pair created by setup-ovn.sh (Linux bridge ↔ OVS bridge)
if ip link show veth-wan-br >/dev/null 2>&1; then
    echo "  Deleting veth pair: veth-wan-br ↔ veth-wan-ovs"
    sudo ip link del veth-wan-br 2>/dev/null || true
fi

# Remove veth persistence units (Fix 1, mulga-998.b). Without this, systemd-networkd
# recreates the veth on next reboot even after a full dev reset.
if [ -e /etc/systemd/network/15-spinifex-veth-wan.netdev ] || \
   [ -e /etc/systemd/network/15-spinifex-veth-wan.network ] || \
   [ -e /etc/systemd/network/16-spinifex-veth-wan-ovs.network ]; then
    echo "  Deleting veth persistence units"
    sudo rm -f /etc/systemd/network/15-spinifex-veth-wan.netdev \
               /etc/systemd/network/15-spinifex-veth-wan.network \
               /etc/systemd/network/16-spinifex-veth-wan-ovs.network
    sudo networkctl reload 2>/dev/null || true
fi

# Remove macvlan interfaces created by setup-ovn.sh
for iface in $(ip -o link show type macvlan 2>/dev/null | awk -F': ' '{print $2}' | grep '^spx-ext-'); do
    echo "  Deleting macvlan: $iface"
    sudo ip link del "$iface" 2>/dev/null || true
done

# --- Wipe production state ---
echo "==> Wiping $ETC_DIR $DATA_DIR $LOG_DIR $RUN_DIR"
sudo rm -rf "$ETC_DIR" "$DATA_DIR" "$LOG_DIR" "$RUN_DIR"

# Drop the old CA cert from the system trust store. The new init writes a
# fresh CA which we re-install below.
if [ -f /usr/local/share/ca-certificates/spinifex-ca.crt ]; then
    echo "==> Removing stale CA from trust store"
    sudo rm -f /usr/local/share/ca-certificates/spinifex-ca.crt
    sudo update-ca-certificates
fi

# --- Rebuild + setup.sh scaffolding (no init / start) ---
echo "==> Rebuilding and reinstalling (dev-install.sh, setup-only mode)"
DEV_INSTALL_SKIP_INIT=1 "$SCRIPT_DIR/dev-install.sh"

# --- Detect WAN topology (bridge only — no macvlan) ---
WAN_IFACE=$(ip -4 route show default | awk '{print $5}' | head -1)
WAN_GW=$(ip -4 route show default | awk '{print $3}' | head -1)
WAN_PREFIX_HOST=""
if [ -n "$WAN_IFACE" ]; then
    WAN_PREFIX_HOST=$(ip -4 -o addr show "$WAN_IFACE" 2>/dev/null | awk '{print $4}' | cut -d/ -f2 | head -1)
fi
WAN_PREFIX_HOST="${WAN_PREFIX_HOST:-24}"

SETUP_OVN_FLAGS=""
if [ -n "$WAN_IFACE" ]; then
    if ip -d link show "$WAN_IFACE" 2>/dev/null | grep -q "bridge"; then
        echo "==> WAN is a bridge: $WAN_IFACE (setup-ovn.sh auto-detects)"
    else
        SETUP_OVN_FLAGS="--wan-bridge=br-wan --wan-iface=$WAN_IFACE"
        echo "==> WAN is physical: $WAN_IFACE (direct bridge br-wan)"
    fi
fi

echo "==> Running setup-ovn.sh"
# shellcheck disable=SC2086  # intentional word-splitting for flag list
sudo /usr/local/share/spinifex/setup-ovn.sh --management $SETUP_OVN_FLAGS

# --- Assemble external-networking args for spx admin init ---
EXTERNAL_INIT_ARGS=""
case "$EXT_MODE" in
    nat)
        # NAT gateway IP isn't captured from config (not stored in a
        # predictable location); derive from WAN subnet — same rule as
        # a fresh install. Operators who hand-edited the gateway IP will
        # need to re-apply it after reset.
        if [ -n "$WAN_GW" ]; then
            IFS='.' read -r o1 o2 o3 _ <<< "$WAN_GW"
            GATEWAY_IP="${o1}.${o2}.${o3}.200"
            EXTERNAL_INIT_ARGS="--external-mode=nat --gateway-ip=$GATEWAY_IP --external-gateway=$WAN_GW --external-prefix-len=${EXT_PREFIX:-$WAN_PREFIX_HOST}"
            echo "==> External mode: nat (gateway IP $GATEWAY_IP)"
        fi
        ;;
    pool|*)
        if [ -n "$POOL_START" ] && [ -n "$POOL_END" ]; then
            EXTERNAL_INIT_ARGS="--external-mode=pool --external-pool=${POOL_START}-${POOL_END} --external-gateway=${EXT_GATEWAY:-$WAN_GW} --external-prefix-len=${EXT_PREFIX:-$WAN_PREFIX_HOST}"
            echo "==> External mode: pool (preserved $POOL_START-$POOL_END)"
        elif [ -n "$WAN_GW" ]; then
            # Fresh box: derive pool from WAN subnet.
            IFS='.' read -r o1 o2 o3 _ <<< "$WAN_GW"
            POOL_START="${o1}.${o2}.${o3}.200"
            POOL_END="${o1}.${o2}.${o3}.250"
            EXTERNAL_INIT_ARGS="--external-mode=pool --external-pool=${POOL_START}-${POOL_END} --external-gateway=$WAN_GW --external-prefix-len=$WAN_PREFIX_HOST"
            echo "==> External mode: pool (default $POOL_START-$POOL_END)"
        fi
        ;;
esac

# --- Initialize platform ---
echo "==> Initializing platform (region=$REGION az=$AZ)"
EMAIL_INIT_ARG=""
if [ -n "$OPERATOR_EMAIL" ]; then
    EMAIL_INIT_ARG="--email=$OPERATOR_EMAIL"
fi
# shellcheck disable=SC2086  # intentional word-splitting for arg list
sudo /usr/local/bin/spx admin init --force \
    --region "$REGION" --az "$AZ" --node node1 --nodes 1 \
    $EXTERNAL_INIT_ARGS $EMAIL_INIT_ARG

# --- Install CA cert into system trust store ---
echo "==> Installing CA certificate"
sudo cp "$ETC_DIR/ca.pem" /usr/local/share/ca-certificates/spinifex-ca.crt
sudo update-ca-certificates

# --- Start services ---
echo "==> Starting spinifex.target"
sudo systemctl start spinifex.target

# Wait for services to start
sleep 5

# --- Build + import LB image (needs services running) ---
echo "==> Building and importing LB image"
cd "$PROJECT_ROOT" && make build-lb-agent
"$PROJECT_ROOT/scripts/build-system-image.sh" "$PROJECT_ROOT/scripts/images/lb.conf" --import --quiet

# --- Smoke test ---
sudo -u "$INVOKING_USER" "$SCRIPT_DIR/smoke-test.sh"
