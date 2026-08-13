#!/bin/bash
# install-node.sh — form a multi-node Spinifex cluster across running nodes.
#
# Every node installed from the ISO comes up as its own single-node cluster.
# This script converts a set of them into one cluster: it rebuilds the OVN
# database in clustered form, runs `spx admin init` on the first host and
# `spx admin join` on the rest, restarts services and verifies the result.
#
# It is the scripted form of docs/install/install-multi-node. It installs no
# software — every host must already be running the version you want. Use
# mulga's scripts/update-nodes.sh, or the release installer, for that first.
#
# Usage:
#   scripts/install-node.sh --external-pool 10.0.1.100-10.0.1.150 \
#       --external-gateway 10.0.1.1 --external-prefix-len 24 \
#       node1.example.com node2.example.com node3.example.com
#
# The first host is the init node; the rest join it.
#
# Options:
#   --external-pool A-B      Public IP range for instances (required)
#   --external-gateway IP    Gateway for that range (required)
#   --external-prefix-len N  Prefix length of the range's subnet (required)
#   --external-iface NAME    WAN NIC for br-external (default: auto-detected)
#   --node-names A,B,C       Cluster node names (default: node1..nodeN)
#   --lan-bridge NAME        Bridge carrying internal cluster traffic
#   --vpc-bridge NAME        Bridge carrying Geneve overlay traffic
#   --region R               Region (default: ap-southeast-2)
#   --az AZ                  Availability zone (default: <region>a)
#   --email ADDR             Operator email for update and security notices
#   --user U                 SSH user (default: spinifex)
#   --identity FILE          SSH private key
#   --hosts-file FILE        One host per line; blank lines and # comments ignored
#   --token-ttl D            Join token validity (default: 30m)
#   --wipe                   Reset every host to its pre-install state first.
#                            Destroys all data on ALL hosts, including the first.
#                            Confirmed separately from --yes; unattended runs
#                            must set SPX_WIPE_CONFIRM=wipe.
#   --smoke                  Run smoke-test.sh --create-vpc on the first host
#   --no-firewall            Leave the host firewall alone, for debugging
#   --yes                    Skip the confirmation prompt
#   --dry-run                Print what would happen and touch nothing
#
# THE HOST FIREWALL
#
# Nodes installed from the ISO boot armed, scoped to the only cluster member
# they know: themselves. Forming a cluster means talking to hosts that are not
# members yet, so the policy is taken down before the OVN database cluster is
# built and put back once the cluster is up. Skipping this does not produce a
# slow formation, it produces one that cannot happen at all.
#
# WHAT THIS DESTROYS
#
# Forming a cluster is not additive. --recreate-db discards all OVN logical
# network state on every database node, and `spx admin join --force` replaces
# each joining node's CA and master key with the first host's. Every VPC,
# subnet and instance on the joining nodes is lost. That is safe on freshly
# installed servers and unrecoverable on ones that have been in service.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

SSH_USER="${SSH_USER:-spinifex}"
IDENTITY=""
HOSTS=()
NODE_NAMES=()
EXT_POOL=""
EXT_GATEWAY=""
EXT_PREFIX=""
EXT_IFACE=""
LAN_BRIDGE=""
VPC_BRIDGE=""
LAN_BRIDGE_SET=false
VPC_BRIDGE_SET=false
REGION="ap-southeast-2"
AZ=""
EMAIL=""
PORT=4432
TOKEN_TTL="30m"
RUN_SMOKE=false
WIPE=false
MANAGE_FIREWALL=true
ASSUME_YES=false
DRY_RUN=false

# How long to wait for the join token to appear, and for formation to complete
# once the last node has joined.
TOKEN_TIMEOUT=180
FORMATION_TIMEOUT=600

# How long to wait for every node to re-arm from the formed cluster. The daemon
# retries on a backoff that widens to five minutes, but the restart below puts
# it at the short end of that.
FIREWALL_TIMEOUT=180

# How long to wait for every ovn-controller to register its chassis.
CHASSIS_TIMEOUT=120

FIREWALL_HELPER="/usr/local/lib/spinifex/spinifex-firewall-apply"
FIREWALL_PEERS="/etc/spinifex/firewall/peers.nft"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --external-pool)       EXT_POOL="$2"; shift 2 ;;
        --external-gateway)    EXT_GATEWAY="$2"; shift 2 ;;
        --external-prefix-len) EXT_PREFIX="$2"; shift 2 ;;
        --external-iface)      EXT_IFACE="$2"; shift 2 ;;
        --node-names)          IFS=',' read -ra NODE_NAMES <<<"$2"; shift 2 ;;
        --lan-bridge)          LAN_BRIDGE="$2"; LAN_BRIDGE_SET=true; shift 2 ;;
        --vpc-bridge)          VPC_BRIDGE="$2"; VPC_BRIDGE_SET=true; shift 2 ;;
        --region)              REGION="$2"; shift 2 ;;
        --az)                  AZ="$2"; shift 2 ;;
        --email)               EMAIL="$2"; shift 2 ;;
        --user)                SSH_USER="$2"; shift 2 ;;
        --identity)            IDENTITY="$2"; shift 2 ;;
        --token-ttl)           TOKEN_TTL="$2"; shift 2 ;;
        --hosts-file)
            while IFS= read -r line; do
                line="${line%%#*}"
                line="$(echo "$line" | xargs)"
                [ -n "$line" ] && HOSTS+=("$line")
            done <"$2"
            shift 2
            ;;
        --smoke)        RUN_SMOKE=true; shift ;;
        --wipe)         WIPE=true; shift ;;
        --no-firewall)  MANAGE_FIREWALL=false; shift ;;
        --yes|-y)   ASSUME_YES=true; shift ;;
        --dry-run)  DRY_RUN=true; shift ;;
        -h|--help)  sed -n '2,58p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
        -*)         echo "ERROR: unknown option: $1" >&2; exit 2 ;;
        *)          HOSTS+=("$1"); shift ;;
    esac
done

log()  { echo "[install-node] $*"; }
fail() { echo "[install-node] ERROR: $*" >&2; exit 1; }

N=${#HOSTS[@]}

if [ "$N" -lt 2 ]; then
    echo "ERROR: give at least two hosts — the first initializes, the rest join." >&2
    echo "Usage: $(basename "$0") [options] host1 host2 [host3...]" >&2
    exit 2
fi

# Two-node RAFT is strictly worse than standalone: quorum is a majority, so
# 2-of-2 tolerates zero failures and either node dying loses the database.
# setup-ovn.sh documents the same rule; this is where the operator finds out.
if [ "$N" -eq 2 ]; then
    log "WARNING: two nodes run a standalone OVN database on ${HOSTS[0]}."
    log "         Losing that host takes the control plane down. Three is the minimum"
    log "         for a fault-tolerant deployment."
fi

for required in "--external-pool:$EXT_POOL" "--external-gateway:$EXT_GATEWAY" "--external-prefix-len:$EXT_PREFIX"; do
    [ -n "${required#*:}" ] || fail "${required%%:*} is required.
       Instances get their public addresses from this range, and there is no safe
       default to guess. Example:
         --external-pool 216.218.163.101-216.218.163.110 \\
         --external-gateway 216.218.163.97 --external-prefix-len 27"
done

[[ "$EXT_POOL" =~ ^[0-9.]+-[0-9.]+$ ]] || fail "--external-pool must be START-END, got: $EXT_POOL"
[[ "$EXT_PREFIX" =~ ^[0-9]+$ ]] && [ "$EXT_PREFIX" -ge 1 ] && [ "$EXT_PREFIX" -le 32 ] ||
    fail "--external-prefix-len must be 1-32, got: $EXT_PREFIX"

AZ="${AZ:-${REGION}a}"

if [ ${#NODE_NAMES[@]} -eq 0 ]; then
    for i in $(seq 1 "$N"); do NODE_NAMES+=("node$i"); done
elif [ ${#NODE_NAMES[@]} -ne "$N" ]; then
    fail "--node-names has ${#NODE_NAMES[@]} entries for $N hosts"
fi

# The node name is the cluster's identity for a host: it keys the [nodes.X]
# config block, the NATS server name and the IPsec peer certificate. Two nodes
# sharing one is not a cosmetic clash.
if [ "$(printf '%s\n' "${NODE_NAMES[@]}" | sort -u | wc -l)" -ne "$N" ]; then
    fail "--node-names contains duplicates: ${NODE_NAMES[*]}"
fi

SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 -o BatchMode=yes)
[ -n "$IDENTITY" ] && SSH_OPTS+=(-i "$IDENTITY")

# on runs a command on a host. Under --dry-run it prints instead, so a rehearsal
# shows the exact remote command line rather than a summary of it.
on() {
    local host="$1"
    shift
    if $DRY_RUN; then
        echo "  ssh $SSH_USER@$host $*"
        return 0
    fi
    ssh "${SSH_OPTS[@]}" "$SSH_USER@$host" "$@"
}

# out runs a command and returns its output even under --dry-run, for the reads
# that later phases depend on. Nothing it runs changes host state.
out() {
    local host="$1"
    shift
    ssh "${SSH_OPTS[@]}" "$SSH_USER@$host" "$@" 2>/dev/null || true
}

SETUP_OVN="/usr/local/share/spinifex/setup-ovn.sh"

# --- Preflight -------------------------------------------------------------
#
# Every host is checked, and every address resolved, before any host is
# touched: a typo in the third address should not leave the first two stopped
# with a half-formed cluster.

declare -a WAN_IPS LAN_IPS VPC_IPS

# plane_ip <host> <bridge> — first IPv4 address on a link, empty if absent.
plane_ip() {
    out "$1" "ip -4 -o addr show $2 2>/dev/null | awk 'NR==1 {split(\$4, a, \"/\"); print a[1]}'"
}

# resolve_plane <host> <requested bridge> <explicitly set?> <wan ip> — the
# address a plane lands on. An unset bridge that does not exist folds onto the
# wan plane (the canonical vpc <- lan <- wan collapse); an explicitly named one
# that has no address is an error rather than a silent fold, because a plane
# the operator asked for is one they expect to be used.
resolve_plane() {
    local host="$1" bridge="$2" explicit="$3" wan_ip="$4" ip=""
    [ -n "$bridge" ] && ip=$(plane_ip "$host" "$bridge")
    ip="${ip//[$'\r\n']/}"
    if [ -n "$ip" ]; then
        echo "$ip"
        return 0
    fi
    if [ "$explicit" = true ]; then
        return 1
    fi
    echo "$wan_ip"
}

log "preflight on $N host(s)"

versions=""
for i in $(seq 0 $((N - 1))); do
    host="${HOSTS[$i]}"

    ssh "${SSH_OPTS[@]}" "$SSH_USER@$host" true 2>/dev/null ||
        fail "cannot reach $SSH_USER@$host over SSH"
    ssh "${SSH_OPTS[@]}" "$SSH_USER@$host" "sudo -n true" 2>/dev/null ||
        fail "$host: $SSH_USER does not have passwordless sudo"

    ssh "${SSH_OPTS[@]}" "$SSH_USER@$host" "test -x /usr/local/bin/spx" 2>/dev/null ||
        fail "$host: no spx installed. This script forms a cluster; it does not install
       Spinifex. Install every host first, then re-run."
    ssh "${SSH_OPTS[@]}" "$SSH_USER@$host" "test -x $SETUP_OVN" 2>/dev/null ||
        fail "$host: $SETUP_OVN is missing — this node predates the clustered-OVN
       installer. Update it before forming a cluster."

    version=$(out "$host" "spx version 2>/dev/null | head -1")
    version="${version//[$'\r\n']/}"
    versions+="${version}"$'\n'

    # A node that is already in a multi-node cluster is one whose keys and
    # logical network state are load-bearing. Refusing here is deliberate:
    # forming a cluster must never be the command that destroys a live one.
    # --wipe is the deliberate teardown this asks for, and carries its own
    # typed confirmation, so it is the one way past.
    node_count=$(out "$host" \
        "sudo grep -oP '^\[nodes\.\K[^.\]]+' /etc/spinifex/spinifex.toml 2>/dev/null | sort -u | wc -l")
    node_count="${node_count//[$'\r\n']/}"
    [[ "$node_count" =~ ^[0-9]+$ ]] || node_count=0
    if [ "$node_count" -gt 1 ] && ! $WIPE; then
        fail "$host is already part of a $node_count-node cluster.
       Re-forming it would discard its CA and master key, and any data sealed
       under them. Tear it down deliberately before re-running."
    fi

    wan_ip=$(out "$host" "ip -4 route show default | awk 'NR==1 {print \$5}'")
    wan_ip="${wan_ip//[$'\r\n']/}"
    wan_ip=$(plane_ip "$host" "$wan_ip")
    wan_ip="${wan_ip//[$'\r\n']/}"
    [ -n "$wan_ip" ] || fail "$host: no IPv4 address on the default-route interface"

    lan_ip=$(resolve_plane "$host" "${LAN_BRIDGE:-br-lan}" "$LAN_BRIDGE_SET" "$wan_ip") ||
        fail "$host: --lan-bridge $LAN_BRIDGE has no IPv4 address"
    vpc_ip=$(resolve_plane "$host" "${VPC_BRIDGE:-br-vpc}" "$VPC_BRIDGE_SET" "$lan_ip") ||
        fail "$host: --vpc-bridge $VPC_BRIDGE has no IPv4 address"

    WAN_IPS[i]="$wan_ip"
    LAN_IPS[i]="$lan_ip"
    VPC_IPS[i]="$vpc_ip"

    # Node names do not have to match hostnames, but when they disagree it is
    # almost always because the operator meant a different host. Cheap to say.
    hostname=$(out "$host" "hostname")
    hostname="${hostname//[$'\r\n']/}"
    if [ "$hostname" != "${NODE_NAMES[$i]}" ]; then
        log "  NOTE: $host is '$hostname' but will be named '${NODE_NAMES[$i]}' in the cluster"
    fi

    log "  $host — ${version:-unknown} wan=$wan_ip lan=$lan_ip vpc=$vpc_ip"
done

# Mixed versions form a cluster whose NATS mesh, predastore and OVN schema
# disagree with each other. That failure surfaces hours later as unexplained
# errors, so it is worth catching here.
if [ "$(sort -u <<<"$versions" | grep -c .)" -gt 1 ]; then
    fail "hosts are running different spx versions:
$(sort -u <<<"$versions" | sed 's/^/         /')
       Bring them all to the same build before forming a cluster."
fi

# --- Confirm ---------------------------------------------------------------

echo ""
echo "  Cluster: $N nodes, region $REGION, az $AZ"
printf '  %-4s %-22s %-10s %-16s %-16s %s\n' "" HOST NAME WAN LAN VPC
for i in $(seq 0 $((N - 1))); do
    role=$([ "$i" -eq 0 ] && echo "init" || echo "join")
    printf '  %-4s %-22s %-10s %-16s %-16s %s\n' \
        "$role" "${HOSTS[$i]}" "${NODE_NAMES[$i]}" "${WAN_IPS[$i]}" "${LAN_IPS[$i]}" "${VPC_IPS[$i]}"
done
echo ""
echo "  Instance pool: $EXT_POOL via $EXT_GATEWAY/$EXT_PREFIX"
echo ""
if $WIPE; then
    echo "  --wipe: this DESTROYS, on ALL $N hosts INCLUDING ${HOSTS[0]}:"
    echo "    - every instance and every volume backing them"
    echo "    - every S3 object held on these nodes"
    echo "    - each node's CA and master key — data sealed under them is unreadable"
    echo "      afterwards even if the bytes are restored from elsewhere"
    echo "    - all OVN logical network state"
else
    echo "  This DESTROYS, on every host except ${HOSTS[0]}:"
    echo "    - all OVN logical network state: every VPC, subnet, router and port"
    echo "    - every instance depending on it"
    echo "    - the node's own CA and master key, replaced by ${HOSTS[0]}'s"
fi
echo ""

if ! $ASSUME_YES && ! $DRY_RUN; then
    read -r -p "  Type 'yes' to continue: " reply
    [ "$reply" = "yes" ] || fail "aborted"
fi

# --yes deliberately does not cover --wipe. Unattended reruns are the whole
# point of --yes, and a flag that silently also means "destroy every node's
# data" is one stray shell-history recall away from an incident. Automation
# opts in separately through the environment, so a lab rerun stays possible
# without making the destructive path reachable by one extra flag.
if $WIPE && ! $DRY_RUN; then
    if [ -t 0 ]; then
        read -r -p "  --wipe destroys all data on $N hosts. Type 'wipe' to confirm: " reply
        [ "$reply" = "wipe" ] || fail "aborted"
    elif [ "${SPX_WIPE_CONFIRM:-}" != "wipe" ]; then
        fail "--wipe needs confirmation, and there is no terminal to ask on.
       Re-run with SPX_WIPE_CONFIRM=wipe to confirm destroying all data on $N hosts."
    fi
fi

# --- Stop ------------------------------------------------------------------
#
# Each of these is its own single-node cluster, so there is no coordinated
# shutdown to run: spinifex.target's own ExecStop drains that node's guests
# before any storage service stops.

log "stopping spinifex.target on every host"
for host in "${HOSTS[@]}"; do
    on "$host" "sudo systemctl stop spinifex.target" || fail "$host: could not stop spinifex.target"
done

# --- Disarm the host firewall ----------------------------------------------
#
# The policy lives in the kernel and outlives the services, so stopping the
# target above did not touch it. Every ISO node is armed for a cluster of one,
# which blocks the OVN raft ports the next phase needs: nodes 2 and 3 dial the
# first host on 6643/6644 and it does not recognise them yet. `disable` is
# idempotent, so a node that never had a policy is left alone.

declare -a WAS_ARMED
for i in $(seq 0 $((N - 1))); do WAS_ARMED[i]=false; done

if $MANAGE_FIREWALL; then
    log "disarming the host firewall while the cluster forms"
    for i in $(seq 0 $((N - 1))); do
        host="${HOSTS[$i]}"
        if [ "$(out "$host" "sudo test -r $FIREWALL_PEERS && echo armed")" = "armed" ]; then
            WAS_ARMED[i]=true
        fi
        on "$host" "if [ -x $FIREWALL_HELPER ]; then sudo $FIREWALL_HELPER disable; fi" ||
            fail "$host: could not disarm the host firewall"
        if ${WAS_ARMED[$i]}; then
            log "  $host disarmed"
        else
            log "  $host had no policy loaded"
        fi
    done
fi

# --- Wipe ------------------------------------------------------------------
#
# node-reset.sh is not part of the release tarball, so it is pushed from this
# checkout the same way smoke-test.sh is. After this a node has no CA, master
# key, OVN database or chassis identity left for the formation phase to collide
# with — which is the whole reason --wipe exists.
if $WIPE; then
    log "wiping every host back to its pre-install state"
    for host in "${HOSTS[@]}"; do
        if ! $DRY_RUN; then
            scp "${SSH_OPTS[@]}" -q "$SCRIPT_DIR/node-reset.sh" "$SSH_USER@$host:/tmp/spx-node-reset.sh" ||
                fail "$host: could not copy node-reset.sh"
        fi
        on "$host" "chmod 0755 /tmp/spx-node-reset.sh && sudo /tmp/spx-node-reset.sh --yes" ||
            fail "$host: node-reset.sh failed"
        on "$host" "rm -f /tmp/spx-node-reset.sh"
        log "  $host wiped"
    done
fi

# --- OVN database cluster --------------------------------------------------
#
# ovn-ctl consults the cluster flags only when it creates the database, and
# the ovn-central package starts a standalone one on install — so converting
# to RAFT means recreating the DB. Three or more nodes run RAFT across the
# first three; the rest are compute nodes pointing at all three, so they
# survive any one of them failing.

DB_NODES=$((N >= 3 ? 3 : 1))

# A collapsed lan plane puts the OVN NB/SB listener on the public address,
# because it is the only one the node has. Nothing can be done about that
# here, but the operator has to know a host firewall is the only control.
for i in $(seq 0 $((DB_NODES - 1))); do
    if [ "${LAN_IPS[$i]}" = "${WAN_IPS[$i]}" ]; then
        log "WARNING: ${HOSTS[$i]} has no separate lan plane — OVN NB/SB (6641/6642)"
        log "         will listen on ${WAN_IPS[$i]}. Restrict those ports with a firewall."
    fi
done

# --node-name pins the OVS system-id, which is the OVN chassis-id, which
# ovs-monitor-ipsec uses as the IKEv2 `@<name>` peer identity. `spx admin init`
# enables IPsec by default and bakes the cluster node name into each peer
# certificate as CN and dnsName, so a chassis left on its package-generated
# UUID authenticates as @<uuid> against a cert naming the node and every
# Geneve tunnel fails with AUTHENTICATION_FAILED. Safe to pin here because
# --recreate-db means no stale chassis row survives to be orphaned by it.
log "building the OVN database ($([ "$DB_NODES" -eq 3 ] && echo "RAFT across 3" || echo standalone))"

if [ "$DB_NODES" -eq 3 ]; then
    on "${HOSTS[0]}" "sudo $SETUP_OVN --management \
        --node-name=${NODE_NAMES[0]} \
        --db-cluster-local-addr=${LAN_IPS[0]} \
        --lan-addr=${LAN_IPS[0]} \
        --recreate-db \
        --encap-ip=${VPC_IPS[0]}" || fail "${HOSTS[0]}: could not create the OVN database cluster"
    log "  ${HOSTS[0]} created the database cluster"

    for i in 1 2; do
        on "${HOSTS[$i]}" "sudo $SETUP_OVN --management \
            --node-name=${NODE_NAMES[$i]} \
            --db-cluster-local-addr=${LAN_IPS[$i]} \
            --db-cluster-remote-addr=${LAN_IPS[0]} \
            --lan-addr=${LAN_IPS[$i]} \
            --recreate-db \
            --encap-ip=${VPC_IPS[$i]}" || fail "${HOSTS[$i]}: could not join the OVN database cluster"
        log "  ${HOSTS[$i]} joined the database cluster"
    done

    OVN_REMOTE="tcp:${LAN_IPS[0]}:6642,tcp:${LAN_IPS[1]}:6642,tcp:${LAN_IPS[2]}:6642"
else
    on "${HOSTS[0]}" "sudo $SETUP_OVN --management \
        --node-name=${NODE_NAMES[0]} \
        --lan-addr=${LAN_IPS[0]} \
        --encap-ip=${VPC_IPS[0]}" || fail "${HOSTS[0]}: setup-ovn.sh failed"
    log "  ${HOSTS[0]} is running a standalone database"

    OVN_REMOTE="tcp:${LAN_IPS[0]}:6642"
fi

# Compute nodes: no database of their own, pointed at every database node.
for i in $(seq "$DB_NODES" $((N - 1))); do
    on "${HOSTS[$i]}" "sudo $SETUP_OVN \
        --node-name=${NODE_NAMES[$i]} \
        --ovn-remote=$OVN_REMOTE \
        --encap-ip=${VPC_IPS[$i]}" || fail "${HOSTS[$i]}: setup-ovn.sh failed"
    log "  ${HOSTS[$i]} attached as a compute node"
done

# --- Form the cluster ------------------------------------------------------

# --advertise must be explicit whenever --bind is pinned. resolveAdvertiseIP
# echoes a non-wildcard --bind straight back, so binding services to the lan
# plane without this publishes the internal address as the node's public dial
# target.
init_args=(
    --force
    --node "${NODE_NAMES[0]}"
    --nodes "$N"
    --bind "${LAN_IPS[0]}"
    --cluster-bind "${LAN_IPS[0]}"
    --advertise "${WAN_IPS[0]}"
    --port "$PORT"
    --region "$REGION"
    --az "$AZ"
    --token-ttl "$TOKEN_TTL"
    --external-mode=pool
    --external-source=static
    --external-pool="$EXT_POOL"
    --external-gateway="$EXT_GATEWAY"
    --external-prefix-len="$EXT_PREFIX"
)
[ -n "$EXT_IFACE" ] && init_args+=(--external-iface="$EXT_IFACE")
[ -n "$EMAIL" ] && init_args+=(--email="$EMAIL")

INIT_LOG=/tmp/spx-init.log

log "initializing ${HOSTS[0]} as ${NODE_NAMES[0]} (waiting for $((N - 1)) node(s) to join)"

# init blocks in a formation server until every node has joined, so it runs
# detached and the token is read back out of its output.
on "${HOSTS[0]}" "sudo rm -f $INIT_LOG; \
    sudo setsid sh -c 'spx admin init ${init_args[*]} >$INIT_LOG 2>&1' </dev/null >/dev/null 2>&1 & \
    exit 0" || fail "${HOSTS[0]}: could not start spx admin init"

TOKEN=""
if $DRY_RUN; then
    TOKEN="spx_join_<scraped-from-$INIT_LOG>"
else
    elapsed=0
    while [ "$elapsed" -lt "$TOKEN_TIMEOUT" ]; do
        TOKEN=$(out "${HOSTS[0]}" "sudo grep -o 'spx_join_[A-Za-z0-9_-]*' $INIT_LOG | head -1")
        TOKEN="${TOKEN//[$'\r\n']/}"
        [ -n "$TOKEN" ] && break
        sleep 5
        elapsed=$((elapsed + 5))
    done
    if [ -z "$TOKEN" ]; then
        out "${HOSTS[0]}" "sudo tail -40 $INIT_LOG" | sed 's/^/  | /' >&2
        fail "${HOSTS[0]}: spx admin init produced no join token within ${TOKEN_TIMEOUT}s"
    fi
    log "  join token issued"
fi

# --force: each joining node arrived from the ISO already initialized, with its
# own CA and master key. Joining replaces them with the init node's, and
# `spx admin join` refuses to do that silently.
#
# Every join must run concurrently. `spx admin join` registers and then blocks
# until the formation server has all N nodes, so joining one at a time deadlocks
# the moment there is more than one joiner: node 2 waits for node 3, which is
# waiting on a shell that node 2 is still holding.
JOIN_PIDS=()
for i in $(seq 1 $((N - 1))); do
    on "${HOSTS[$i]}" "sudo spx admin join --force \
        --node ${NODE_NAMES[$i]} \
        --bind ${LAN_IPS[$i]} --cluster-bind ${LAN_IPS[$i]} --advertise ${WAN_IPS[$i]} \
        --host ${LAN_IPS[0]}:$PORT --token $TOKEN \
        --region $REGION --az $AZ" &
    JOIN_PIDS+=("$!")
done

# Collect every join before failing on any of them, so a partial formation
# reports which node broke it rather than whichever was reaped first.
JOIN_FAILED=()
for i in $(seq 1 $((N - 1))); do
    if wait "${JOIN_PIDS[$((i - 1))]}"; then
        log "  ${NODE_NAMES[$i]} joined"
    else
        JOIN_FAILED+=("${HOSTS[$i]}")
    fi
done
[ ${#JOIN_FAILED[@]} -eq 0 ] || fail "spx admin join failed on: ${JOIN_FAILED[*]}"

# init exits once the last node registers. A formation server still running
# means a node registered but formation never completed.
if ! $DRY_RUN; then
    elapsed=0
    while [ "$elapsed" -lt "$FORMATION_TIMEOUT" ]; do
        # [s]px, not spx: pgrep -f scans full command lines, and the shell ssh
        # spawns to run it carries the pattern itself. Unbracketed, this always
        # matches and formation never looks finished.
        ssh "${SSH_OPTS[@]}" "$SSH_USER@${HOSTS[0]}" "pgrep -f '[s]px admin init' >/dev/null" >/dev/null 2>&1 || break
        sleep 5
        elapsed=$((elapsed + 5))
    done
    if [ "$elapsed" -ge "$FORMATION_TIMEOUT" ]; then
        out "${HOSTS[0]}" "sudo tail -40 $INIT_LOG" | sed 's/^/  | /' >&2
        fail "${HOSTS[0]}: formation did not complete within ${FORMATION_TIMEOUT}s"
    fi
    log "  formation complete"
fi

# --- Start -----------------------------------------------------------------

log "starting spinifex.target on every host"
for host in "${HOSTS[@]}"; do
    # --no-block, because a blocking start never returns on a host whose services
    # cannot come up: they land in a restart loop and systemd waits on the job
    # indefinitely. Verify below is what decides whether the start worked.
    on "$host" "sudo systemctl start --no-block spinifex.target" ||
        fail "$host: could not start spinifex.target"
done

if $DRY_RUN; then
    echo ""
    log "dry run complete — nothing was changed"
    exit 0
fi

# --- Verify ----------------------------------------------------------------

log "verifying"

# Services take a moment to register on NATS after start, so the node list is
# worth a few retries before it means anything. spx colourises unconditionally,
# including down a pipe, so the escapes come off before anything parses fields.
nodes=""
for _ in $(seq 1 12); do
    nodes=$(out "${HOSTS[0]}" "sudo spx get nodes" | sed 's/\x1b\[[0-9;]*m//g')
    [ -n "$nodes" ] && ! grep -q NotReady <<<"$nodes" && break
    sleep 10
done
echo ""
sed 's/^/  /' <<<"$nodes"
echo ""

# Match Ready as a whole field, so NotReady is not counted as Ready.
ready=$(awk '{for (i = 1; i <= NF; i++) if ($i == "Ready") { c++; break }} END {print c + 0}' <<<"$nodes")
[ "$ready" -eq "$N" ] || fail "expected $N Ready nodes, got $ready — check journalctl -u 'spinifex-*'"

if [ "$DB_NODES" -eq 3 ]; then
    status=$(out "${HOSTS[0]}" "sudo ovn-appctl -t /var/run/ovn/ovnnb_db.ctl cluster/status OVN_Northbound")
    members=$(grep -c ' at tcp:' <<<"$status" || true)
    [ "$members" -eq 3 ] ||
        fail "OVN Northbound reports $members cluster members, expected 3 — a standalone
       database here means --recreate-db did not take on every database node"
    log "  OVN Northbound: 3 members, $(grep -oiE '^Role: .*' <<<"$status" | head -1)"
fi

# sb_show — the Southbound contents, read in a way that works on a clustered
# database. Two things make the obvious `ovn-sbctl show` wrong here: a clustered
# DB has no unix socket to fall back on, and a follower refuses to answer at all
# unless leader-only is turned off. Both fail by printing to stderr and exiting
# non-zero, so a naive read looks exactly like a cluster with no chassis at all.
sb_show() {
    out "${HOSTS[0]}" \
        "sudo ovn-sbctl --db=tcp:${LAN_IPS[0]}:6642 --no-leader-only show 2>/dev/null ||
         sudo ovn-sbctl --no-leader-only show 2>/dev/null"
}

# ovn-controller registers its chassis a moment after it connects, so this is
# worth a few retries before it means anything.
sb=""
chassis=0
elapsed=0
while [ "$elapsed" -lt "$CHASSIS_TIMEOUT" ]; do
    sb=$(sb_show)
    chassis=$(grep -c '^Chassis' <<<"$sb" || true)
    [ "$chassis" -eq "$N" ] && break
    sleep 5
    elapsed=$((elapsed + 5))
done

[ -n "$sb" ] ||
    fail "cannot read the OVN Southbound database from ${HOSTS[0]}.
       Try 'sudo ovn-sbctl --db=tcp:${LAN_IPS[0]}:6642 --no-leader-only show' there."

# The chassis name is the IPsec peer identity, so a chassis left on its
# package-generated UUID authenticates as a name no certificate carries and
# every Geneve tunnel to it fails. A missing one means that node has no overlay
# networking, which is worth stopping for rather than noting in passing.
missing_chassis=""
for name in "${NODE_NAMES[@]}"; do
    grep -q "^Chassis \"\?$name\"\?" <<<"$sb" || missing_chassis+=" $name"
done
[ -z "$missing_chassis" ] ||
    fail "no chassis registered for:$missing_chassis (found $chassis of $N)
       Those nodes have no overlay networking. Check 'systemctl status ovn-controller'
       and 'sudo ovs-vsctl get Open_vSwitch . external_ids:system-id' on them."

log "  OVN Southbound: $chassis chassis registered"

# --- Re-arm the host firewall ----------------------------------------------
#
# The daemon derives the peer sets from cluster membership, so starting the
# target above already armed each node for the cluster it is now part of. The
# restart is for timing rather than correctness: it puts reconciliation at the
# short end of a backoff that widens to five minutes.
#
# Only nodes that arrived armed are re-armed and checked. One that had the
# policy switched off by config should stay off, and re-arming it here would be
# this script quietly overriding that.

# fw_set <host> <define> — addresses in one define of the peer file the daemon
# wrote. The file is the daemon's own statement of what it believes the cluster
# to be, which is what needs checking; the loaded table is verified separately.
fw_set() {
    out "$1" "sudo grep -oP 'define $2 = \{ \K[^}]*' $FIREWALL_PEERS" |
        tr ',' '\n' | tr -d ' \r' | grep . || true
}

if $MANAGE_FIREWALL; then
    echo ""
    log "re-arming the host firewall"
    for host in "${HOSTS[@]}"; do
        on "$host" "sudo systemctl restart spinifex-daemon" ||
            fail "$host: could not restart spinifex-daemon to re-arm the firewall"
    done

    # Every plane address of every node, which is what each node's peer set has
    # to cover: the policy matches on source address, so a node reaching a peer
    # over its lan plane is a different rule hit than over its wan plane.
    EXPECTED_PEERS=$(printf '%s\n' "${WAN_IPS[@]}" "${LAN_IPS[@]}" "${VPC_IPS[@]}" | sort -u)

    for i in $(seq 0 $((N - 1))); do
        host="${HOSTS[$i]}"
        if ! ${WAS_ARMED[$i]}; then
            log "  $host: firewall was not armed before forming, leaving it off"
            continue
        fi

        elapsed=0
        missing=""
        encap_count=0
        while [ "$elapsed" -lt "$FIREWALL_TIMEOUT" ]; do
            loaded=$(fw_set "$host" spinifex_peers)
            encap_count=$(fw_set "$host" spinifex_encap_peers | grep -c . || true)

            missing=""
            while IFS= read -r addr; do
                grep -qxF "$addr" <<<"$loaded" || missing+=" $addr"
            done <<<"$EXPECTED_PEERS"

            [ -z "$missing" ] && [ "$encap_count" -eq "$N" ] && break
            sleep 5
            elapsed=$((elapsed + 5))
        done

        # A node missing from a peer set is not cosmetic: that node's cluster
        # traffic is being dropped, and the cluster is degraded in a way nothing
        # else reports until something fails hours later.
        [ -z "$missing" ] ||
            fail "$host: firewall peer set is missing$missing
       This node will drop cluster traffic from those addresses. Check
       'journalctl -u spinifex-daemon' on it for a reconcile that did not complete."
        [ "$encap_count" -eq "$N" ] ||
            fail "$host: firewall tunnel set has $encap_count of $N chassis addresses.
       Geneve to the missing nodes is blocked. ovn-controller may still be
       registering — check 'sudo ovn-sbctl show'."

        out "$host" "sudo nft list table inet spinifex_filter >/dev/null 2>&1 && echo loaded" |
            grep -q loaded ||
            fail "$host: the peer file is correct but no policy is loaded in the kernel.
       Check 'sudo systemctl status spinifex-firewall.service'."

        log "  $host armed: $(grep -c . <<<"$loaded") peers, $encap_count tunnel endpoints"
    done
else
    echo ""
    log "host firewall: left alone (--no-firewall). If these nodes arrived armed they"
    log "               are still scoped to themselves and will drop cluster traffic."
fi

# The pool is the one thing the operator supplied by hand and the one thing
# nothing else validates, so print what actually landed. dns_servers in
# particular is detected rather than passed, and is wrong often enough to be
# worth seeing.
echo ""
log "external pool as written:"
out "${HOSTS[0]}" "sudo sed -n '/^\[\[network.external_pools\]\]/,/^$/p' /etc/spinifex/spinifex.toml" |
    sed 's/^/  /'

# --- Smoke test ------------------------------------------------------------

if $RUN_SMOKE; then
    echo ""
    log "running the smoke test on ${HOSTS[0]}"
    # smoke-test.sh is not part of the release tarball, so it is pushed from
    # this checkout — which also guarantees the test and this script are the
    # same version.
    scp "${SSH_OPTS[@]}" -q "$SCRIPT_DIR/smoke-test.sh" "$SSH_USER@${HOSTS[0]}:/tmp/spx-smoke-test.sh"
    on "${HOSTS[0]}" "chmod 0755 /tmp/spx-smoke-test.sh && /tmp/spx-smoke-test.sh --create-vpc --nodes $N" ||
        fail "smoke test failed on ${HOSTS[0]}"
    on "${HOSTS[0]}" "rm -f /tmp/spx-smoke-test.sh"
fi

echo ""
log "cluster formed: $N nodes, pool $EXT_POOL"
