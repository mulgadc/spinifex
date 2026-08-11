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
# Usage:
#   ./scripts/reset-dev-env.sh [options]
#
# Options:
#   --profile=NAME   Topology profile from scripts/dev-env/NAME.conf. Declares
#                    which bridge carries the wan, lan and vpc planes. Omit to
#                    auto-detect from the host, which is the historical
#                    behaviour: wan off the default route, lan and vpc unset.
#                    Also settable as SPINIFEX_DEV_PROFILE.
#   --dry-run        Resolve the planes and print the setup-ovn.sh and
#                    'spx admin init' command lines, then exit without
#                    touching any host state.
#
# Every profile variable is overridable from the environment, e.g.
#   DEV_VPC_BRIDGE=br-vpc2 ./scripts/reset-dev-env.sh --profile=three-plane
#
# Planes follow the canonical collapse vpc <- lan <- wan, so a profile that
# leaves vpc unset folds it onto lan, and one that leaves both unset is a
# single-NIC node. --encap-ip is always passed explicitly from the resolved
# vpc plane rather than relying on setup-ovn.sh sniffing for a bridge named
# br-vpc, which silently falls back to the WAN address when it misses.
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

# --- Arguments ---
DEV_PROFILE="${SPINIFEX_DEV_PROFILE:-}"
DRY_RUN=false
for arg in "$@"; do
    case "$arg" in
        --profile=*) DEV_PROFILE="${arg#*=}" ;;
        --dry-run)   DRY_RUN=true ;;
        --help|-h)
            sed -n '2,/^set -euo/{/^set -euo/!p}' "$0"
            exit 0
            ;;
        *)
            echo "Unknown option: $arg (try --help)"
            exit 1
            ;;
    esac
done

# --- Topology profile ---
# Profiles only set DEV_* variables, and every one is honoured from the
# environment first, so an operator can override a single plane without
# copying a profile.
if [ -n "$DEV_PROFILE" ]; then
    PROFILE_FILE="$SCRIPT_DIR/dev-env/${DEV_PROFILE}.conf"
    if [ ! -f "$PROFILE_FILE" ]; then
        echo "❌ No such profile: $DEV_PROFILE ($PROFILE_FILE)"
        echo "   Available:"
        for f in "$SCRIPT_DIR"/dev-env/*.conf; do
            [ -e "$f" ] || continue
            echo "     $(basename "$f" .conf)"
        done
        exit 1
    fi
    # Environment wins: stash anything already set, source, then restore.
    for v in DEV_WAN_BRIDGE DEV_LAN_BRIDGE DEV_VPC_BRIDGE DEV_MGMT_BRIDGE \
             DEV_MGMT_CIDR DEV_UPLINK DEV_BIND_PLANES; do
        if [ -n "${!v+x}" ]; then
            eval "__override_$v=\${$v}"
        fi
    done
    # shellcheck disable=SC1090  # profile path is resolved at runtime
    . "$PROFILE_FILE"
    for v in DEV_WAN_BRIDGE DEV_LAN_BRIDGE DEV_VPC_BRIDGE DEV_MGMT_BRIDGE \
             DEV_MGMT_CIDR DEV_UPLINK DEV_BIND_PLANES; do
        ov="__override_$v"
        if [ -n "${!ov+x}" ]; then
            eval "$v=\${$ov}"
        fi
    done
    echo "==> Profile: $DEV_PROFILE ($PROFILE_FILE)"
fi

DEV_WAN_BRIDGE="${DEV_WAN_BRIDGE:-}"
DEV_LAN_BRIDGE="${DEV_LAN_BRIDGE:-}"
DEV_VPC_BRIDGE="${DEV_VPC_BRIDGE:-}"
DEV_MGMT_BRIDGE="${DEV_MGMT_BRIDGE:-br-mgmt}"
DEV_MGMT_CIDR="${DEV_MGMT_CIDR:-10.15.8.1/24}"
DEV_UPLINK="${DEV_UPLINK:-bridged}"
DEV_BIND_PLANES="${DEV_BIND_PLANES:-0}"

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

    # toml_scalar <section-prefix> <key> — grabs first matching scalar from the
    # first section whose header starts with the literal prefix. Strips quotes.
    # Literal (index) match, not regex: gawk mangles backslash-escaped dynamic
    # regexes (^\[nodes\. -> ^[nodes. -> invalid), while mawk does not.
    toml_scalar() {
        local section="$1"
        local key="$2"
        awk -v sec="$section" -v key="$key" '
            index($0, sec) == 1 { in_sec=1; next }
            /^\[/               { in_sec=0 }
            in_sec && $0 ~ "^[[:space:]]*" key "[[:space:]]*=" {
                sub(/^[^=]*=[[:space:]]*/, "")
                gsub(/[[:space:]]*#.*$/, "")
                gsub(/^"/, ""); gsub(/"$/, "")
                print; exit
            }' "$TMP_CFG"
    }

    REGION=$(toml_scalar '[nodes.'        'region')
    AZ=$(toml_scalar     '[nodes.'        'az')
    EXT_MODE=$(toml_scalar '[network]'    'external_mode')
    POOL_START=$(toml_scalar  '[[network.external_pools]]' 'range_start')
    POOL_END=$(toml_scalar    '[[network.external_pools]]' 'range_end')
    EXT_GATEWAY=$(toml_scalar '[[network.external_pools]]' 'gateway')
    EXT_PREFIX=$(toml_scalar  '[[network.external_pools]]' 'prefix_len')
    OPERATOR_EMAIL=$(toml_scalar '[operator]' 'email')
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

# --- Resolve network planes ---
# Everything below runs before the wipe so a bad topology fails while the
# environment is still intact, and so --dry-run can print the real commands.

# iface_ip <link> — first IPv4 address, empty when the link has none or does
# not exist. The trailing `|| true` matters: under `set -o pipefail` a missing
# link makes ip(8) fail the whole pipeline, which would abort the script here
# instead of letting preflight report the plane properly.
iface_ip() {
    ip -4 -o addr show "$1" 2>/dev/null | awk 'NR==1 {split($4, a, "/"); print a[1]}' || true
}

# iface_prefix <link> — prefix length of the first IPv4 address.
iface_prefix() {
    ip -4 -o addr show "$1" 2>/dev/null | awk 'NR==1 {split($4, a, "/"); print a[2]}' || true
}

# Auto-detect the wan plane from the default route when no profile named it.
WAN_IFACE=$(ip -4 route show default | awk '{print $5}' | head -1)
WAN_GW=$(ip -4 route show default | awk '{print $3}' | head -1)
WAN_BRIDGE="${DEV_WAN_BRIDGE:-$WAN_IFACE}"
WAN_IP=$(iface_ip "$WAN_BRIDGE")
WAN_PREFIX_HOST=$(iface_prefix "$WAN_BRIDGE")
WAN_PREFIX_HOST="${WAN_PREFIX_HOST:-24}"

# Collapse vpc <- lan <- wan. An unset role inherits the plane above it, which
# is what makes one profile express one, two and three NIC layouts.
LAN_BRIDGE="${DEV_LAN_BRIDGE:-$WAN_BRIDGE}"
VPC_BRIDGE="${DEV_VPC_BRIDGE:-$LAN_BRIDGE}"
LAN_IP=$(iface_ip "$LAN_BRIDGE")
VPC_IP=$(iface_ip "$VPC_BRIDGE")

printf '==> Planes\n'
printf '      %-6s %-10s %s\n' wan "$WAN_BRIDGE" "${WAN_IP:-no address}"
printf '      %-6s %-10s %s%s\n' lan "$LAN_BRIDGE" "${LAN_IP:-no address}" \
    "$([ "$LAN_BRIDGE" = "$WAN_BRIDGE" ] && echo '  (folds onto wan)')"
printf '      %-6s %-10s %s%s\n' vpc "$VPC_BRIDGE" "${VPC_IP:-no address}" \
    "$([ "$VPC_BRIDGE" = "$LAN_BRIDGE" ] && echo '  (folds onto lan)')"
printf '      %-6s %-10s %s\n' mgmt "$DEV_MGMT_BRIDGE" "$DEV_MGMT_CIDR"

# --- Preflight ---
PREFLIGHT_FAIL=0
fail() { echo "❌ $*"; PREFLIGHT_FAIL=1; }

if [ "$DEV_UPLINK" != "bridged" ] && [ "$DEV_UPLINK" != "nat" ]; then
    fail "DEV_UPLINK must be 'bridged' or 'nat', got: $DEV_UPLINK"
fi

if [ "$DEV_UPLINK" = "bridged" ]; then
    if [ -z "$WAN_BRIDGE" ]; then
        fail "No wan plane: no default route and no DEV_WAN_BRIDGE."
    elif ! ip link show "$WAN_BRIDGE" >/dev/null 2>&1; then
        fail "wan bridge '$WAN_BRIDGE' does not exist."
    fi
fi

# An addressless vpc plane is the silent failure this script exists to stop:
# setup-ovn.sh would fall back to the default-route source IP and put Geneve
# on WAN while reporting a successful auto-detection.
if [ -z "$VPC_IP" ]; then
    fail "vpc plane '$VPC_BRIDGE' has no IPv4 address — Geneve encap would silently fall back to WAN."
fi

# br-mgmt is the guest→host bridge and must be an OVS bridge. A plane bridge
# sharing the name leaves setup-ovn.sh with a half-built bridge record.
for plane_pair in "wan:$WAN_BRIDGE" "lan:$LAN_BRIDGE" "vpc:$VPC_BRIDGE"; do
    if [ "${plane_pair#*:}" = "$DEV_MGMT_BRIDGE" ]; then
        fail "${plane_pair%%:*} plane uses '$DEV_MGMT_BRIDGE', which is reserved for the guest→host bridge. Rename the plane bridge (br-wan/br-lan/br-vpc) or set DEV_MGMT_BRIDGE."
    fi
done

if [ "$DEV_BIND_PLANES" = "1" ]; then
    if [ -z "$LAN_IP" ]; then
        fail "DEV_BIND_PLANES=1 but the lan plane '$LAN_BRIDGE' has no IPv4 address."
    fi
    if [ -z "$WAN_IP" ]; then
        fail "DEV_BIND_PLANES=1 but the wan plane '$WAN_BRIDGE' has no IPv4 address to advertise."
    fi
    # Pinning --bind moves every service rendered as {{.BindIP}} onto the lan
    # plane. awsgw and predastore must be on the wildcard first, or the public
    # AWS API and S3 endpoint leave the wan plane with them.
    TEMPLATE="$PROJECT_ROOT/cmd/spinifex/cmd/templates/spinifex.toml"
    if [ -f "$TEMPLATE" ] && ! grep -q '^host = "0.0.0.0:9999"' "$TEMPLATE"; then
        fail "DEV_BIND_PLANES=1 but $TEMPLATE still renders awsgw on {{.BindIP}}. This branch predates the wildcard public listeners — rebase, or set DEV_BIND_PLANES=0."
    fi
fi

if [ "$PREFLIGHT_FAIL" -ne 0 ]; then
    echo "   Preflight failed — no host state was touched."
    exit 1
fi

# --- Assemble the commands both --dry-run and the real run use ---
SETUP_OVN_FLAGS="--encap-ip=$VPC_IP --mgmt-bridge=$DEV_MGMT_BRIDGE --mgmt-cidr=$DEV_MGMT_CIDR"
if [ "$DEV_UPLINK" = "nat" ]; then
    # Routed NAT bridges no WAN NIC, and setup-ovn.sh rejects the WAN flags.
    SETUP_OVN_FLAGS="--nat-uplink $SETUP_OVN_FLAGS"
elif ip -d link show "$WAN_BRIDGE" 2>/dev/null | grep -q "bridge"; then
    echo "==> WAN is a bridge: $WAN_BRIDGE (setup-ovn.sh links it via veth)"
else
    SETUP_OVN_FLAGS="--wan-bridge=br-wan --wan-iface=$WAN_BRIDGE $SETUP_OVN_FLAGS"
    echo "==> WAN is physical: $WAN_BRIDGE (direct bridge br-wan)"
fi

# Bind the NB/SB client listener to the lan plane only when lan is a
# genuinely distinct plane from wan — when it folds onto wan (single-nic),
# --lan-addr=$LAN_IP would put the listener on the public interface.
if [ "$LAN_BRIDGE" != "$WAN_BRIDGE" ] && [ -n "$LAN_IP" ]; then
    SETUP_OVN_FLAGS="$SETUP_OVN_FLAGS --lan-addr=$LAN_IP"
    echo "==> LAN plane: $LAN_BRIDGE ($LAN_IP) — binding NB/SB client listener"
fi

# Mirror the ISO installer: internal services on lan, public dial target on
# wan. --advertise must be explicit, because resolveAdvertiseIP echoes a
# non-wildcard --bind straight back and would publish the internal address.
BIND_INIT_ARGS=""
if [ "$DEV_BIND_PLANES" = "1" ]; then
    BIND_INIT_ARGS="--bind=$LAN_IP --cluster-bind=$LAN_IP --advertise=$WAN_IP"
fi

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

EMAIL_INIT_ARG=""
if [ -n "$OPERATOR_EMAIL" ]; then
    EMAIL_INIT_ARG="--email=$OPERATOR_EMAIL"
fi

echo "==> Planned commands"
echo "      setup-ovn.sh --management $SETUP_OVN_FLAGS"
echo "      spx admin init --force --region $REGION --az $AZ --node node1 --nodes 1 $EXTERNAL_INIT_ARGS $BIND_INIT_ARGS $EMAIL_INIT_ARG"

if [ "$DRY_RUN" = true ]; then
    echo "==> Dry run — no host state touched."
    exit 0
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

# Remove veth persistence units. Without this, systemd-networkd recreates the
# veth on next reboot even after a full dev reset.
if [ -e /etc/systemd/network/15-spinifex-veth-wan.netdev ] || \
   [ -e /etc/systemd/network/15-spinifex-veth-wan.network ] || \
   [ -e /etc/systemd/network/16-spinifex-veth-wan-ovs.network ]; then
    echo "  Deleting veth persistence units"
    sudo rm -f /etc/systemd/network/15-spinifex-veth-wan.netdev \
               /etc/systemd/network/15-spinifex-veth-wan.network \
               /etc/systemd/network/16-spinifex-veth-wan-ovs.network
    sudo networkctl reload 2>/dev/null || true
fi

# The mgmt address and the OVS-unmanaged marker are re-created by setup-ovn.sh
# with whatever bridge/CIDR this run resolves. Leaving the old units behind
# re-applies the previous run's address on the next boot.
if [ -e /etc/systemd/network/10-spinifex-mgmt.network ] || \
   [ -e /etc/systemd/network/05-spinifex-ovs-internal.network ]; then
    echo "  Deleting mgmt + OVS-internal networkd units"
    sudo rm -f /etc/systemd/network/10-spinifex-mgmt.network \
               /etc/systemd/network/05-spinifex-ovs-internal.network
    sudo networkctl reload 2>/dev/null || true
fi

# Routed-NAT artifacts from a previous --nat-uplink run. Without this, flipping
# DEV_UPLINK back to bridged leaves a transit veth and masquerade rules that
# keep NATting behind the reset's back.
if ip link show spx-nat-host >/dev/null 2>&1; then
    echo "  Deleting NAT transit veth: spx-nat-host"
    sudo ip link del spx-nat-host 2>/dev/null || true
fi
if [ -e /etc/systemd/network/17-spinifex-nat.netdev ]; then
    echo "  Deleting NAT transit persistence units"
    sudo rm -f /etc/systemd/network/17-spinifex-nat.netdev \
               /etc/systemd/network/17-spinifex-nat.network \
               /etc/systemd/network/18-spinifex-nat-ovs.network
    sudo networkctl reload 2>/dev/null || true
fi

# Masquerade + forward rules carry the spinifex-nat-egress comment, so the
# rule spec can be read back out of iptables-save and deleted verbatim.
for tbl in nat filter; do
    while read -r spec; do
        [ -n "$spec" ] || continue
        echo "  Deleting iptables -t $tbl rule: $spec"
        # shellcheck disable=SC2086  # spec is a rule argument list
        sudo iptables -t "$tbl" -D $spec 2>/dev/null || true
    done < <(sudo iptables-save -t "$tbl" 2>/dev/null |
             grep -- 'spinifex-nat-egress' | sed -e 's/^-A //' -e 's/"//g')
done

# Legacy: clean up macvlan interfaces left over from pre-veth setup-ovn.sh
# runs. Safe to remove once no operator system retains these.
for iface in $(ip -o link show type macvlan 2>/dev/null | awk -F': ' '{print $2}' | grep '^spx-ext-'); do
    echo "  Deleting legacy macvlan: $iface"
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

echo "==> Running setup-ovn.sh"
# shellcheck disable=SC2086  # intentional word-splitting for flag list
sudo /usr/local/share/spinifex/setup-ovn.sh --management $SETUP_OVN_FLAGS

# --- Initialize platform ---
echo "==> Initializing platform (region=$REGION az=$AZ)"
# shellcheck disable=SC2086  # intentional word-splitting for arg list
sudo /usr/local/bin/spx admin init --force \
    --region "$REGION" --az "$AZ" --node node1 --nodes 1 \
    $EXTERNAL_INIT_ARGS $BIND_INIT_ARGS $EMAIL_INIT_ARG

# --- Pin the mgmt bridge in the daemon config ---
# spx admin init has no flag for it, and the daemon defaults to br-mgmt. A
# profile that moved the guest→host bridge has to say so here, or system
# instance TAPs attach to a bridge the host never addressed.
if [ "$DEV_MGMT_BRIDGE" != "br-mgmt" ]; then
    echo "==> Pinning daemon mgmt_bridge = $DEV_MGMT_BRIDGE"
    sudo sed -i "/^\[nodes\..*\.daemon\]/,/^\[/ s|^dev_networking = .*|&\nmgmt_bridge = \"$DEV_MGMT_BRIDGE\"|" \
        "$CONFIG_FILE"
fi

# --- Install CA cert into system trust store ---
echo "==> Installing CA certificate"
sudo cp "$ETC_DIR/ca.pem" /usr/local/share/ca-certificates/spinifex-ca.crt
sudo update-ca-certificates

# --- Start services ---
echo "==> Starting spinifex.target"
sudo systemctl start spinifex.target

# Wait for services to start
sleep 5

# --- Build + install microVM artifacts (kernel + initramfs for direct-boot LBs) ---
echo "==> Building and installing microVM artifacts"
cd "$PROJECT_ROOT" && make build-lb-agent install-microvm

# --- Authorize SSH ingress on the default security group ---
# Default SG denies all inbound, so smoke-test.sh's SSH probe times out.
# Open port 22 on the default VPC's default SG before the smoke test runs.
aws_as_user() { sudo -u "$INVOKING_USER" env HOME="$INVOKING_HOME" AWS_PROFILE=spinifex aws "$@"; }

echo "==> Waiting for EC2 daemon"
DAEMON_TIMEOUT=60
DAEMON_ELAPSED=0
while [ $DAEMON_ELAPSED -lt $DAEMON_TIMEOUT ]; do
    if aws_as_user ec2 describe-security-groups --output text >/dev/null 2>&1; then
        break
    fi
    sleep 2
    DAEMON_ELAPSED=$((DAEMON_ELAPSED + 2))
done
if [ $DAEMON_ELAPSED -ge $DAEMON_TIMEOUT ]; then
    echo "❌ EC2 daemon not ready after ${DAEMON_TIMEOUT}s"
    exit 1
fi

echo "==> Authorizing SSH (port 22) on default security group"
DEFAULT_VPC_ID=$(aws_as_user ec2 describe-vpcs \
    --filters "Name=is-default,Values=true" \
    --query 'Vpcs[0].VpcId' --output text)
DEFAULT_SG_ID=$(aws_as_user ec2 describe-security-groups \
    --filters "Name=vpc-id,Values=$DEFAULT_VPC_ID" "Name=group-name,Values=default" \
    --query 'SecurityGroups[0].GroupId' --output text)
if [ -z "$DEFAULT_SG_ID" ] || [ "$DEFAULT_SG_ID" = "None" ]; then
    echo "❌ Could not locate default security group in VPC $DEFAULT_VPC_ID"
    exit 1
fi
aws_as_user ec2 authorize-security-group-ingress \
    --group-id "$DEFAULT_SG_ID" --protocol tcp --port 22 --cidr 0.0.0.0/0 \
    >/dev/null 2>&1 || true
echo "  Authorized SSH on $DEFAULT_SG_ID ($DEFAULT_VPC_ID)"

# --- Smoke test ---
# Exports the instance it launched so verify-network.sh can assert egress from
# inside the guest rather than launching a second one.
sudo -u "$INVOKING_USER" "$SCRIPT_DIR/smoke-test.sh"

# --- Verify the datapath ---
# The smoke test proves an instance booted and answered SSH from the host,
# which never leaves the box. This asserts the planes actually carry what they
# were asked to.
DEV_WAN_BRIDGE="$WAN_BRIDGE" DEV_LAN_BRIDGE="$LAN_BRIDGE" \
DEV_VPC_BRIDGE="$VPC_BRIDGE" DEV_MGMT_BRIDGE="$DEV_MGMT_BRIDGE" \
DEV_MGMT_CIDR="$DEV_MGMT_CIDR" DEV_BIND_PLANES="$DEV_BIND_PLANES" \
DEV_UPLINK="$DEV_UPLINK" \
    sudo -E -u "$INVOKING_USER" "$SCRIPT_DIR/verify-network.sh"
