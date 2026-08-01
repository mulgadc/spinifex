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
#   --wan-iface=NAME     Physical NIC to add to the WAN bridge (use with --wan-bridge)
#   --dhcp               Obtain gateway IP via DHCP on the WAN bridge interface
#   --nat-uplink         Routed NAT mode: no WAN NIC is bridged. Creates br-ext
#                        with a transit veth (spx-nat-host 100.127.0.1/24) and
#                        host masquerade rules; VMs get outbound-only WAN over
#                        any uplink (ethernet, WiFi, cellular, PPP). Pair with
#                        `spx admin init --external-mode=nat`.
#   --mgmt-bridge=NAME   OVS bridge for system-instance control plane (default: br-mgmt)
#   --mgmt-cidr=CIDR     IPv4 CIDR to assign on the mgmt bridge (default: 10.15.8.1/24)
#   --mgmt-iface=NAME    Physical/virtual NIC to enslave to the mgmt bridge (multi-node only)
#   --no-mgmt-bridge     Skip mgmt bridge provisioning (for dev-networking hosts)
#   --ovn-remote=ADDR    OVN SB DB address; accepts a comma-separated list of
#                        SB endpoints for compute nodes pointing at a RAFT
#                        cluster (default: tcp:127.0.0.1:6642)
#   --encap-ip=IP        Geneve tunnel endpoint IP (default: auto-detect)
#   --db-cluster-local-addr=IP   This DB node's own IP for OVSDB RAFT clustering.
#                        Enables clustered NB(6643)/SB(6644) DBs (requires
#                        --management). Empty --db-cluster-remote-addr ⇒ create
#                        the cluster; set ⇒ join it.
#   --db-cluster-remote-addr=IP  An existing cluster DB node's IP to join. Omit
#                        on the first (init) DB node that forms the cluster.
#   --recreate-db        Destroy and recreate the NB/SB DBs so they can be
#                        created in clustered format. Required when converting
#                        a node that has already run a standalone ovn-central
#                        (which is every node — the ovn-central package starts
#                        one on install). DISCARDS ALL LOGICAL NETWORK STATE:
#                        safe on a fresh node, destroys every VPC on a live one.
#
# Cluster Sizing — which DB topology to run:
#
#   Nodes  DB topology                              Tolerates
#   1-2    standalone ovn-central on node 1         nothing
#   3      RAFT across all 3                        1 node
#   4+     RAFT across 3, remainder compute-only    1 node
#
#   Three nodes is the minimum recommended multi-server deployment.
#
#   NEVER run RAFT with 2 members. Quorum is a majority, so 2-of-2 tolerates
#   zero failures — and it is strictly worse than standalone, because either
#   node failing loses quorum rather than only node 1. Two-node deployments
#   run standalone and accept the single point of failure.
#
#   Do not scale DB members past 3. RAFT write latency is bounded by the
#   slowest member of the majority, and a 4th member buys no extra fault
#   tolerance. Nodes beyond the third are compute-only, pointing at all three
#   SB endpoints via the comma-separated --ovn-remote form.
#
#   Losing ovn-central does not stop the data plane: ovn-controller has already
#   programmed flows into br-int, so running instances keep networking. Only
#   control-plane change stops — new VPCs, instance launches, port bindings.
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
#   # Compute node joining an existing cluster:
#   ./scripts/setup-ovn.sh --ovn-remote=tcp:10.0.0.1:6642 --encap-ip=10.0.0.2
#
#   # DB node 1 — create an OVSDB RAFT cluster (init):
#   ./scripts/setup-ovn.sh --management --db-cluster-local-addr=10.0.0.1
#
#   # DB nodes 2,3 — join the cluster:
#   ./scripts/setup-ovn.sh --management --db-cluster-local-addr=10.0.0.2 \
#       --db-cluster-remote-addr=10.0.0.1
#
#   # Compute node pointing at all 3 clustered SB endpoints:
#   ./scripts/setup-ovn.sh --ovn-remote=tcp:10.0.0.1:6642,tcp:10.0.0.2:6642,tcp:10.0.0.3:6642
#
#   # No WAN bridge (overlay-only, no public subnet):
#   ./scripts/setup-ovn.sh --management --encap-ip=10.0.0.1
#
#   # Non-bridgeable uplink (WiFi/cellular) — routed NAT, outbound-only VMs:
#   ./scripts/setup-ovn.sh --management --nat-uplink

set -e

# Defaults
MANAGEMENT=false
WAN_BRIDGE=""
WAN_IFACE=""
EXTERNAL_DHCP=false
NAT_UPLINK=false
MGMT_BRIDGE_ENABLED=true
MGMT_BRIDGE="br-mgmt"
MGMT_CIDR="10.15.8.1/24"
MGMT_IFACE=""
OVN_REMOTE="tcp:127.0.0.1:6642"
ENCAP_IP=""
# OVSDB RAFT clustering. Empty by default ⇒ single, non-clustered ovn-central
# (dev box, existing single-node clusters). When LOCAL_ADDR is set, the NB/SB
# DBs run clustered; REMOTE_ADDR empty ⇒ create the cluster, set ⇒ join it.
DB_CLUSTER_LOCAL_ADDR=""
DB_CLUSTER_REMOTE_ADDR=""
# Recreating the NB/SB DBs discards all logical network state, so it is opt-in.
RECREATE_DB=false
OVN_DBDIR="${OVN_DBDIR:-/var/lib/ovn}"
# NODE_NAME is left empty by default. The chassis-id pin block at Step 4
# only runs when --node-name=NAME is explicitly given. Passing nothing
# preserves whatever system-id already lives in OVS (gold-image UUID,
# bootstrap-install.sh's pre-written nodeN, manual ansible value, etc.).
# Bootstrap callers that want IPsec-cert-identity matching pin the
# system-id themselves before / after invoking setup-ovn.sh — they
# don't need this script to do it for them.
NODE_NAME=""

# Parse arguments
for arg in "$@"; do
    case "$arg" in
        --management)       MANAGEMENT=true ;;
        --dhcp)             EXTERNAL_DHCP=true ;;
        --nat-uplink)       NAT_UPLINK=true ;;
        --wan-bridge=*)     WAN_BRIDGE="${arg#*=}" ;;
        --wan-iface=*)      WAN_IFACE="${arg#*=}" ;;
        --mgmt-bridge=*)    MGMT_BRIDGE="${arg#*=}" ;;
        --mgmt-cidr=*)      MGMT_CIDR="${arg#*=}" ;;
        --mgmt-iface=*)     MGMT_IFACE="${arg#*=}" ;;
        --no-mgmt-bridge)   MGMT_BRIDGE_ENABLED=false ;;
        --ovn-remote=*)     OVN_REMOTE="${arg#*=}" ;;
        --encap-ip=*)       ENCAP_IP="${arg#*=}" ;;
        --db-cluster-local-addr=*)  DB_CLUSTER_LOCAL_ADDR="${arg#*=}" ;;
        --db-cluster-remote-addr=*) DB_CLUSTER_REMOTE_ADDR="${arg#*=}" ;;
        --recreate-db)      RECREATE_DB=true ;;
        --node-name=*)      NODE_NAME="${arg#*=}" ;;
        --help|-h)
            sed -n '3,/^set -e/{/^set -e/!p}' "$0"
            exit 0
            ;;
        *)
            echo "Unknown option: $arg"
            exit 1
            ;;
    esac
done

# OVSDB RAFT clustering runs the NB/SB DBs, so it only applies to management
# (DB) nodes. A remote addr without a local addr is meaningless (nothing to
# join with). Fail loudly rather than silently starting a single-node DB.
if [ -n "$DB_CLUSTER_LOCAL_ADDR" ] && [ "$MANAGEMENT" != true ]; then
    echo "ERROR: --db-cluster-local-addr requires --management (clustered DBs run on management nodes)"
    exit 1
fi
if [ -n "$DB_CLUSTER_REMOTE_ADDR" ] && [ -z "$DB_CLUSTER_LOCAL_ADDR" ]; then
    echo "ERROR: --db-cluster-remote-addr requires --db-cluster-local-addr"
    exit 1
fi
if [ "$RECREATE_DB" = true ] && [ -z "$DB_CLUSTER_LOCAL_ADDR" ]; then
    echo "ERROR: --recreate-db requires --db-cluster-local-addr (it exists to allow"
    echo "       clustered DB creation over an existing standalone DB)"
    exit 1
fi

# --- WAN bridge auto-detection ---
# Determine the WAN bridge name and how to set it up.
WAN_BRIDGE_MODE=""  # "existing", "veth", "direct", or ""
LINUX_BRIDGE=""     # Set when WAN_BRIDGE_MODE="veth" — the Linux bridge behind the veth pair

detect_wan_bridge() {
    # If --wan-bridge was explicitly given, use it
    if [ -n "$WAN_BRIDGE" ]; then
        if [ -n "$WAN_IFACE" ]; then
            WAN_BRIDGE_MODE="direct"
        elif sudo ovs-vsctl br-exists "$WAN_BRIDGE" 2>/dev/null; then
            WAN_BRIDGE_MODE="existing"
        else
            # Bridge doesn't exist yet and no --wan-iface — create empty OVS bridge
            WAN_BRIDGE_MODE="existing"
        fi
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
    echo "  3. No external networking (overlay-only):"
    echo "     ./scripts/setup-ovn.sh --management --encap-ip=$wan_ip"
    echo ""
    echo "  4. Non-bridgeable uplink (WiFi/cellular/PPP) — routed NAT mode"
    echo "     (VMs get outbound-only internet, no public IPs):"
    echo "     ./scripts/setup-ovn.sh --management --nat-uplink"
    echo "     then: spx admin init --external-mode=nat"
    echo "============================================================"
    echo ""
    exit 1
}

if [ "$NAT_UPLINK" = true ]; then
    # Routed NAT: nothing is bridged, so the uplink type is irrelevant.
    if [ -n "$WAN_BRIDGE" ] || [ -n "$WAN_IFACE" ]; then
        echo "ERROR: --nat-uplink does not take --wan-bridge/--wan-iface (no WAN NIC is bridged)"
        exit 1
    fi
    WAN_BRIDGE="br-ext"
    WAN_BRIDGE_MODE="nat"
    echo "  Routed NAT uplink: br-ext + transit veth, host masquerade (no WAN bridge)"
else
    detect_wan_bridge
fi

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
if [ -n "$DB_CLUSTER_LOCAL_ADDR" ]; then
    if [ -n "$DB_CLUSTER_REMOTE_ADDR" ]; then
        echo "  DB RAFT role:     join (local=$DB_CLUSTER_LOCAL_ADDR remote=$DB_CLUSTER_REMOTE_ADDR)"
    else
        echo "  DB RAFT role:     create (local=$DB_CLUSTER_LOCAL_ADDR)"
    fi
fi
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

# Packages (openvswitch-switch, ovn-host, openvswitch-ipsec, strongswan-charon,
# and ovn-central on management nodes) are baked into the gold image via
# scripts/tofu-cluster/image-builder/scripts/provision.sh. Runtime apt-get
# was removed to keep CI off the (flaky) apt-cacher-ng path. If a package is
# missing, the downstream ovs/ovn commands will fail loudly — the fix is to
# rebuild the gold image, not to re-add apt-get here.

# strongswan-charon ships an AppArmor profile for /usr/lib/ipsec/charon that
# only allows reading from /etc/ipsec.*, /etc/strongswan.*, and a few other
# fixed paths. ovs-monitor-ipsec writes the per-tunnel strongSwan config with
# absolute paths to our peer cert + key under /etc/spinifex/ipsec/ AND to the
# cluster CA at /etc/spinifex/ca.pem, so charon hits "Permission denied" on
# both load paths and surfaces as `no trusted RSA public key found for
# '<peer>'` (CA can't be loaded → can't validate peer cert chain → no SAs).
# The profile includes a 'local' override file expressly for site additions
# — grant reads on /etc/spinifex/** (covers ca.pem AND ipsec/peer.{pem,key})
# and reload.
if [ -d /etc/apparmor.d/local ]; then
    LOCAL_OVERRIDE=/etc/apparmor.d/local/usr.lib.ipsec.charon
    # Always rewrite — re-runs may have an older, narrower rule (e.g. the
    # initial /etc/spinifex/ipsec/** only) that misses /etc/spinifex/ca.pem.
    echo "  Adding AppArmor read grant for /etc/spinifex (cert + CA) to charon profile"
    echo "/etc/spinifex/** r," | sudo tee "$LOCAL_OVERRIDE" >/dev/null
    sudo apparmor_parser -r /etc/apparmor.d/usr.lib.ipsec.charon 2>/dev/null || true
fi

# --- Step 2: Enable services ---
# ovn-ctl consults the RAFT flags only when it creates a database. The
# ovn-central package starts a standalone ovsdb-server on install, so a
# standalone-format DB is always already present by the time this script first
# runs: without this check ovn-ctl serves that DB, silently ignores the cluster
# flags, and the script reports success on a cluster that was never formed.
ensure_clustered_db_storage() {
    local db
    local standalone=()
    for db in "$OVN_DBDIR/ovnnb_db.db" "$OVN_DBDIR/ovnsb_db.db"; do
        if [ -f "$db" ] && ! sudo ovsdb-tool db-is-clustered "$db" 2>/dev/null; then
            standalone+=("$db")
        fi
    done
    if [ ${#standalone[@]} -eq 0 ]; then
        return 0
    fi

    if [ "$RECREATE_DB" != true ]; then
        echo "ERROR: clustered DBs requested, but these are in standalone format:" >&2
        printf '         %s\n' "${standalone[@]}" >&2
        echo "" >&2
        echo "  A clustered DB can only be created from scratch, so these must be" >&2
        echo "  removed first. That DISCARDS ALL LOGICAL NETWORK STATE — every logical" >&2
        echo "  switch, router, port and ACL, and so every VPC on this node." >&2
        echo "" >&2
        echo "  A freshly installed node has nothing to lose: re-run with --recreate-db." >&2
        echo "  A node running workloads does: do not." >&2
        exit 1
    fi

    echo "  --recreate-db: removing standalone DBs so ovn-ctl can create clustered ones"

    # ovn-controller must go down with them. Left running, it reconnects to the
    # fresh SB and re-registers under whatever system-id it started with — which
    # on a renamed node is the old one, and that row then holds this node's encap
    # IP so the correctly-named chassis can never commit. Step 5 restarts it.
    sudo systemctl stop ovn-northd ovn-ovsdb-server-nb ovn-ovsdb-server-sb ovn-central ovn-controller 2>/dev/null || true
    sudo rm -f "${standalone[@]}"
}

echo ""
echo "Step 2: Enabling services..."

sudo systemctl enable openvswitch-switch
sudo systemctl start openvswitch-switch
echo "  openvswitch-switch: started"

if [ "$MANAGEMENT" = true ]; then
    sudo systemctl enable ovn-central

    if [ -n "$DB_CLUSTER_LOCAL_ADDR" ]; then
        ensure_clustered_db_storage

        # Clustered NB/SB via native OVSDB RAFT. Both per-DB units source one
        # shared OVN_CTL_OPTS from /etc/default/ovn-central; each run_*_ovsdb
        # consumes only its own --db-{nb,sb}-* flags. RAFT ports default to NB
        # 6643 / SB 6644. ovn-ctl creates the cluster when no remote-addr is
        # given (and no existing .db file), joins when one is.
        OVN_CTL_OPTS="--db-nb-cluster-local-addr=$DB_CLUSTER_LOCAL_ADDR --db-sb-cluster-local-addr=$DB_CLUSTER_LOCAL_ADDR"
        if [ -n "$DB_CLUSTER_REMOTE_ADDR" ]; then
            OVN_CTL_OPTS="$OVN_CTL_OPTS --db-nb-cluster-remote-addr=$DB_CLUSTER_REMOTE_ADDR --db-sb-cluster-remote-addr=$DB_CLUSTER_REMOTE_ADDR"
            echo "  ovn-central: joining RAFT cluster (local=$DB_CLUSTER_LOCAL_ADDR remote=$DB_CLUSTER_REMOTE_ADDR)"
        else
            echo "  ovn-central: creating RAFT cluster (local=$DB_CLUSTER_LOCAL_ADDR)"
        fi

        # Point ovn-northd's client NB/SB connections at the full RAFT member
        # list so the active northd follows the leader. Without this it dials the
        # local member only and stops advancing SB_Global.nb_cfg once leadership
        # moves off this node, wedging the ovn-nbctl --wait=hv flows barrier.
        # Derived from OVN_REMOTE (the SB list); the NB list is the same hosts on
        # 6641. Skipped when OVN_REMOTE is the localhost default (caller passed no
        # member list) so a lone standalone DB node is unaffected.
        if [ "$OVN_REMOTE" != "tcp:127.0.0.1:6642" ]; then
            OVN_REMOTE_NB="${OVN_REMOTE//:6642/:6641}"
            OVN_CTL_OPTS="$OVN_CTL_OPTS --ovn-northd-nb-db=$OVN_REMOTE_NB --ovn-northd-sb-db=$OVN_REMOTE"
            echo "  ovn-northd: NB=$OVN_REMOTE_NB SB=$OVN_REMOTE (RAFT member list)"
        fi

        echo "OVN_CTL_OPTS=\"$OVN_CTL_OPTS\"" | sudo tee /etc/default/ovn-central >/dev/null
        echo "  wrote /etc/default/ovn-central"

        # The packaged ovn-northd.service ExecStop runs `ovn-ctl stop_northd`
        # without --ovn-manage-ovsdb=no, so restarting northd also tears down the
        # NB/SB ovsdb-server units. With the split clustered units those DBs are
        # owned by their own units, so override ExecStop to leave them alone —
        # otherwise the restart below races and kills the freshly-started DBs.
        sudo mkdir -p /etc/systemd/system/ovn-northd.service.d
        sudo tee /etc/systemd/system/ovn-northd.service.d/no-manage-ovsdb.conf >/dev/null <<'EOF'
[Service]
ExecStop=
ExecStop=/usr/share/ovn/scripts/ovn-ctl stop_northd --no-monitor --ovn-manage-ovsdb=no
EOF
        sudo systemctl daemon-reload

        # The ovn-central aggregator is ExecStart=/bin/true, so restarting it
        # won't restart the children — restart the per-DB units directly to
        # pick up the new OVN_CTL_OPTS.
        sudo systemctl restart ovn-ovsdb-server-nb ovn-ovsdb-server-sb ovn-northd
        echo "  ovn-central: started clustered (NB DB + SB DB + ovn-northd)"
    else
        sudo systemctl start ovn-central
        echo "  ovn-central: started (NB DB + SB DB + ovn-northd)"
    fi

    # Wait for OVN NB DB socket to become available
    for i in $(seq 1 15); do
        if sudo ovn-nbctl --timeout=2 get-connection >/dev/null 2>&1; then
            break
        fi
        echo "  Waiting for OVN NB DB... ($i/15)"
        sleep 1
    done

    # Verify rather than assume: a DB that came up standalone despite the RAFT
    # flags is the exact failure this guards against, and it stays invisible
    # until a node goes down and the cluster turns out not to exist.
    if [ -n "$DB_CLUSTER_LOCAL_ADDR" ]; then
        for db in "$OVN_DBDIR/ovnnb_db.db" "$OVN_DBDIR/ovnsb_db.db"; do
            if ! sudo ovsdb-tool db-is-clustered "$db" 2>/dev/null; then
                echo "ERROR: $db is not in clustered format after startup." >&2
                echo "       The RAFT configuration did not take effect." >&2
                exit 1
            fi
        done
        echo "  DB storage:       clustered (NB + SB verified)"
    fi

    # Wait for the Southbound DB to be serving before ovn-controller (Step 5)
    # dials it. On a single node ovn-controller races a fresh SB RAFT election
    # with no join step to absorb the window — attaching before the leader is
    # elected (and before northd writes the gateway logical flows) leaves it with
    # a partial datapath that never reconverges. Gate on SB reachable and, when
    # clustered, an elected leader.
    for i in $(seq 1 30); do
        if sudo ovn-sbctl --timeout=2 show >/dev/null 2>&1; then
            if [ -z "$DB_CLUSTER_LOCAL_ADDR" ]; then
                break
            fi
            SB_LEADER=$(sudo ovs-appctl -t /var/run/ovn/ovnsb_db.ctl \
                cluster/status OVN_Southbound 2>/dev/null \
                | awk '/^Leader:/{print $2; exit}')
            if [ -n "$SB_LEADER" ] && [ "$SB_LEADER" != "unknown" ]; then
                break
            fi
        fi
        echo "  Waiting for OVN SB DB leader... ($i/30)"
        sleep 1
    done

    # Set the NB/SB client listen addresses. set-connection writes through RAFT
    # (replicated cluster-wide), so it only runs on the create node — a joining
    # node would redirect to the leader, which the init node already configured.
    if [ -z "$DB_CLUSTER_REMOTE_ADDR" ]; then
        sudo ovn-nbctl set-connection ptcp:6641
        sudo ovn-sbctl set-connection ptcp:6642
        echo "  OVN NB DB listening on tcp:6641"
        echo "  OVN SB DB listening on tcp:6642"
    fi
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
        sudo ovs-vsctl --if-exists del-port veth-wan-ovs 2>/dev/null || true
        sudo ip link del veth-wan-br 2>/dev/null || true
    fi

    # Same ripdown for stale routed-NAT plumbing when switching away from nat.
    if [ "$WAN_BRIDGE_MODE" != "nat" ]; then
        sudo rm -f /etc/systemd/network/17-spinifex-nat.netdev \
                   /etc/systemd/network/17-spinifex-nat.network \
                   /etc/systemd/network/18-spinifex-nat-ovs.network
        sudo networkctl reload 2>/dev/null || true
        sudo ovs-vsctl --if-exists del-port spx-nat-ovs 2>/dev/null || true
        sudo ip link del spx-nat-host 2>/dev/null || true
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

        nat)
            # Routed NAT: br-ext carries no WAN NIC. A transit veth pair links
            # it to the host stack (spx-nat-host owns 100.127.0.1/24); the host
            # forwards and masquerades the transit /24 out whatever uplink it
            # has. OVN SNATs each VPC CIDR to its gateway LRP transit IP, so
            # the masquerade rule below is the only host-side NAT state.
            NAT_TRANSIT_CIDR="100.127.0.0/24"
            NAT_TRANSIT_GW_CIDR="100.127.0.1/24"

            if ! sudo ovs-vsctl br-exists "$WAN_BRIDGE" 2>/dev/null; then
                sudo ovs-vsctl --may-exist add-br "$WAN_BRIDGE"
                echo "  created OVS bridge: $WAN_BRIDGE"
            fi
            sudo ip link set "$WAN_BRIDGE" up

            # Create transit veth pair (idempotent)
            if ! ip link show spx-nat-host >/dev/null 2>&1; then
                sudo ip link add spx-nat-host type veth peer name spx-nat-ovs
                echo "  created veth pair: spx-nat-host ↔ spx-nat-ovs"
            else
                echo "  veth pair already exists: spx-nat-host ↔ spx-nat-ovs"
            fi
            sudo ip addr replace "$NAT_TRANSIT_GW_CIDR" dev spx-nat-host

            # Add the OVS end to br-ext
            if ! sudo ovs-vsctl port-to-br spx-nat-ovs >/dev/null 2>&1; then
                sudo ovs-vsctl --may-exist add-port "$WAN_BRIDGE" spx-nat-ovs
                echo "  spx-nat-ovs → $WAN_BRIDGE (OVS bridge)"
            fi
            sudo ip link set spx-nat-host up
            sudo ip link set spx-nat-ovs up
            echo "  host (spx-nat-host $NAT_TRANSIT_GW_CIDR) ↔ veth pair ↔ $WAN_BRIDGE (OVS)"

            # Persist the veth pair + transit IP across reboot (veths are
            # kernel-only; same rationale as veth mode above).
            NAT_NETDEV="/etc/systemd/network/17-spinifex-nat.netdev"
            NAT_NETWORK="/etc/systemd/network/17-spinifex-nat.network"
            NAT_OVS_NETWORK="/etc/systemd/network/18-spinifex-nat-ovs.network"
            sudo tee "$NAT_NETDEV" >/dev/null <<NETDEV
[NetDev]
Name=spx-nat-host
Kind=veth

[Peer]
Name=spx-nat-ovs
NETDEV
            sudo tee "$NAT_NETWORK" >/dev/null <<NETWORK
[Match]
Name=spx-nat-host

[Network]
Address=$NAT_TRANSIT_GW_CIDR
ConfigureWithoutCarrier=yes
NETWORK
            # Admin-up the OVS end after reboot (OVS owns the port but does
            # not flip admin state on external ports — same as veth mode).
            sudo tee "$NAT_OVS_NETWORK" >/dev/null <<NETWORK
[Match]
Name=spx-nat-ovs

[Link]
RequiredForOnline=no

[Network]
ConfigureWithoutCarrier=yes
NETWORK
            sudo networkctl reload 2>/dev/null || true
            echo "  wrote $NAT_NETDEV + $NAT_NETWORK + $NAT_OVS_NETWORK (transit veth persists on reboot)"

            # Kernel egress: masquerade the transit /24 out any uplink and
            # accept forwarded transit traffic even under FORWARD-policy DROP.
            # vpcd re-ensures these on every start; installing here too means
            # the wiring is testable before services run.
            sudo iptables -t nat -C POSTROUTING -s "$NAT_TRANSIT_CIDR" ! -d "$NAT_TRANSIT_CIDR" \
                -m comment --comment "spinifex-nat-egress" -j MASQUERADE 2>/dev/null || \
            sudo iptables -t nat -A POSTROUTING -s "$NAT_TRANSIT_CIDR" ! -d "$NAT_TRANSIT_CIDR" \
                -m comment --comment "spinifex-nat-egress" -j MASQUERADE
            sudo iptables -C FORWARD -i spx-nat-host -s "$NAT_TRANSIT_CIDR" \
                -m comment --comment "spinifex-nat-egress" -j ACCEPT 2>/dev/null || \
            sudo iptables -A FORWARD -i spx-nat-host -s "$NAT_TRANSIT_CIDR" \
                -m comment --comment "spinifex-nat-egress" -j ACCEPT
            sudo iptables -C FORWARD -o spx-nat-host -m conntrack --ctstate RELATED,ESTABLISHED \
                -m comment --comment "spinifex-nat-egress" -j ACCEPT 2>/dev/null || \
            sudo iptables -A FORWARD -o spx-nat-host -m conntrack --ctstate RELATED,ESTABLISHED \
                -m comment --comment "spinifex-nat-egress" -j ACCEPT
            echo "  installed masquerade + forward rules for $NAT_TRANSIT_CIDR (comment: spinifex-nat-egress)"
            ;;
    esac

    # --- DHCP: obtain gateway IP for OVN SNAT ---
    if [ "$EXTERNAL_DHCP" = true ] && [ "$WAN_BRIDGE_MODE" = "nat" ]; then
        echo ""
        echo "  Skipping --dhcp: routed NAT mode has a fixed transit gateway (100.127.0.1)"
    elif [ "$EXTERNAL_DHCP" = true ]; then
        echo ""
        echo "Step 3c: Obtaining external gateway IP via DHCP..."

        # DHCP on the WAN bridge itself (direct/existing/veth).
        DHCP_IFACE="$WAN_BRIDGE"

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

    # --may-exist is idempotent against OVS, not against the kernel. When a
    # Linux bridge already holds the name, OVS creates the bridge record but
    # cannot create the internal netdev, leaving a half-built bridge whose
    # only symptom is an error buried in `ovs-vsctl show`.
    if ip link show "$MGMT_BRIDGE" >/dev/null 2>&1 && \
       ! sudo ovs-vsctl br-exists "$MGMT_BRIDGE" 2>/dev/null; then
        echo "  ERROR: '$MGMT_BRIDGE' already exists as a non-OVS link"
        ip -d link show "$MGMT_BRIDGE" | head -2
        echo ""
        echo "  The management bridge must be an OVS bridge. Either:"
        echo "    1. Point this script elsewhere:  --mgmt-bridge=<name>"
        echo "    2. Rename the existing link (a plane bridge belongs on br-wan/br-lan/br-vpc)"
        echo "    3. Skip mgmt provisioning entirely:  --no-mgmt-bridge"
        exit 1
    fi

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

# Pin OVS system-id (= OVN chassis-id) to NODE_NAME when --node-name was
# explicitly given. Reasons to pin:
#   1. IPsec identity: ovs-monitor-ipsec uses chassis-id as the IKEv2
#      `@<name>` peer identity. Our per-node IPsec peer cert
#      (admin.GenerateIPSecPeerCert) carries the cluster node name as CN
#      + dnsName SAN — see spx admin init/join, which take --node NAME and
#      bake NAME into the cert. Leaving chassis-id as the package-generated
#      UUID would cause `received AUTHENTICATION_FAILED` because charon
#      validates `@<UUID>` against a cert dnsName=<NODE_NAME>.
#   2. Ops legibility: `ovn-sbctl show` lists chassis by name, not UUID.
#
# When --node-name is NOT given (CI bootstrap-install.sh today, dev
# single-node bring-up, ansible roles that don't yet pass it), leave the
# system-id alone. A hostname fallback used to live here, but on CI
# single-node it rewrote system-id from the gold-image UUID to a
# different hostname while the existing SBDB chassis row kept name=UUID;
# vpcd's discoverChassis then logged "skipping stale local chassis" and
# refused to start. Preserving the on-disk value is always safe — any
# caller that needs IPsec cert identity matching pins it themselves.
if [ -n "$NODE_NAME" ]; then
    OLD_ID=$(sudo ovs-vsctl get Open_vSwitch . external_ids:system-id 2>/dev/null | tr -d '"')
    echo "$NODE_NAME" | sudo tee /etc/openvswitch/system-id.conf >/dev/null
    sudo ovs-vsctl set Open_vSwitch . external_ids:system-id="$NODE_NAME"
    echo "  system-id:      $NODE_NAME (pinned via --node-name)"

    # A renamed chassis cannot register while the row it left behind still
    # holds this node's encap IP: ovn-controller loops on "OVNSB commit
    # failed" and never appears in `ovn-sbctl show`. The stale row owns no
    # state worth keeping — ovn-controller rebuilds everything on register —
    # but it must go before Step 5 restarts the controller under the new name.
    if [ -n "$OLD_ID" ] && [ "$OLD_ID" != "$NODE_NAME" ]; then
        if sudo ovn-sbctl --db="$OVN_REMOTE" --timeout=10 chassis-del "$OLD_ID" 2>/dev/null; then
            echo "  stale chassis:  removed '$OLD_ID' (renamed to $NODE_NAME)"
        fi
    fi
else
    CURRENT_ID=$(sudo ovs-vsctl get Open_vSwitch . external_ids:system-id 2>/dev/null | tr -d '"')
    echo "  system-id:      ${CURRENT_ID:-<unset>} (preserved; no --node-name given)"
fi
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

# Force a full datapath recompute once ovn-controller has connected to the SB.
# On a fresh single-node RAFT the controller can attach mid-election and program
# a partial datapath (SNAT ct-commit without output/delivery), then never
# reconverge. Recomputing after the SB connection is up rebuilds all OpenFlow
# from the converged Southbound. Compute nodes race a remote SB too, so this
# runs on every node.
for i in $(seq 1 30); do
    CTRL_STATUS=$(sudo OVS_RUNDIR=/var/run/ovn ovs-appctl -t ovn-controller \
        connection-status 2>/dev/null)
    if [ "$CTRL_STATUS" = "connected" ]; then
        break
    fi
    echo "  Waiting for ovn-controller SB connection... ($i/30)"
    sleep 1
done
sudo OVS_RUNDIR=/var/run/ovn ovs-appctl -t ovn-controller inc-engine/recompute \
    2>/dev/null || true
echo "  ovn-controller: forced datapath recompute"

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

# --- Step 8: Grant the spinifex service users access to OVS/OVN ---
echo ""
echo "Step 8: Configuring service-user access..."

# Group-scoped, never world. spinifex-daemon and spinifex-vpcd both run with
# Group=spinifex, so 0660 root:spinifex is exactly the reach they need. A
# world-writable db.sock hands the local datapath — bridges, ports, ovn-remote —
# to any account on the box, and a world-writable ctl socket lets any account
# run `ovn-appctl exit`.
PERMS_HELPER="/usr/local/lib/spinifex/ovs-socket-perms.sh"
sudo mkdir -p "$(dirname "$PERMS_HELPER")"
sudo tee "$PERMS_HELPER" >/dev/null <<'HELPER'
#!/bin/sh
# Group-own the OVS/OVN control sockets to `spinifex` so the service users reach
# them without sudo. Driven from ExecStartPost on both openvswitch-switch and
# ovn-controller: each recreates its own sockets on start, and the ovn-controller
# ctl socket name embeds the pid, so it is a new file every time.
#
# $1 is an optional glob for the caller's own socket, waited on before the sweep.
set -eu
GROUP=spinifex
WAIT_FOR="${1:-}"

if ! getent group "$GROUP" >/dev/null 2>&1; then
    exit 0
fi

# Unquoted so the caller's pattern globs. A caller passing nothing sweeps at once.
wait_target_present() {
    if [ -z "$WAIT_FOR" ]; then
        return 0
    fi
    for s in $WAIT_FOR; do
        if [ -S "$s" ]; then
            return 0
        fi
    done
    return 1
}

# ExecStartPost can outrun socket creation, so poll briefly rather than miss the
# socket and leave the health probe unable to read connection-status.
i=0
while [ "$i" -lt 25 ]; do
    if wait_target_present; then
        break
    fi
    i=$((i + 1))
    sleep 0.2
done

# The dir must be traversable by the group before the sockets inside it matter.
# Only the group changes; the owner keeps full access whoever it is.
if [ -d /var/run/ovn ]; then
    chgrp "$GROUP" /var/run/ovn 2>/dev/null || true
    chmod 0750 /var/run/ovn 2>/dev/null || true
fi

# ovn??_db.sock are the NB/SB databases themselves, which ovn-nbctl and
# ovn-sbctl connect to. Bridge <name>.mgmt sockets are deliberately NOT here:
# ovs-vswitchd creates one whenever a bridge appears, including bridges spinifex
# creates at runtime, so ovs-ofctl keeps its sudo grant.
for s in /var/run/openvswitch/db.sock /var/run/openvswitch/*.ctl \
    /var/run/ovn/*.ctl /var/run/ovn/ovnnb_db.sock /var/run/ovn/ovnsb_db.sock; do
    if [ -S "$s" ]; then
        chgrp "$GROUP" "$s" 2>/dev/null || true
        chmod 0660 "$s" 2>/dev/null || true
    fi
done

# Group-WRITE on the pidfiles, not just read: `ovn-appctl -t <daemon>` resolves
# the pid-named ctl socket through the pidfile, and OVS opens it O_RDWR to test
# the fcntl liveness lock. Read-only fails the probe with EACCES. This also
# tightens them from the 0644 they ship with.
for p in /var/run/openvswitch/*.pid /var/run/ovn/*.pid; do
    if [ -f "$p" ]; then
        chgrp "$GROUP" "$p" 2>/dev/null || true
        chmod 0660 "$p" 2>/dev/null || true
    fi
done
HELPER
sudo chmod 0755 "$PERMS_HELPER"
sudo "$PERMS_HELPER"
echo "  OVS/OVN sockets: 0660 root:spinifex (group-scoped, no world access)"

# Persist across restarts of both daemons. Rewritten unconditionally: an existing
# file is the old 0666 override, and skipping would leave that exposure in place.
# openvswitch-ipsec is included because Step 10 restarts it after this sweep,
# so its ctl socket would otherwise be the one file left at the shipped mode.
for unit in openvswitch-switch:/var/run/openvswitch/db.sock \
    "ovn-controller:/var/run/ovn/*.ctl" \
    "openvswitch-ipsec:/var/run/openvswitch/ovs-monitor-ipsec.*.ctl"; do
    UNIT="${unit%%:*}"
    WAIT_GLOB="${unit#*:}"
    OVERRIDE_DIR="/etc/systemd/system/${UNIT}.service.d"
    sudo mkdir -p "$OVERRIDE_DIR"
    sudo tee "$OVERRIDE_DIR/spinifex-perms.conf" >/dev/null <<OVERRIDE
[Service]
ExecStartPost=$PERMS_HELPER "$WAIT_GLOB"
OVERRIDE
    echo "  systemd override: ${UNIT}.service.d/spinifex-perms.conf"
done
sudo systemctl daemon-reload

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
# ovs-monitor-ipsec drives strongSwan from OVS DB cert pointers. The daemon's
# enableOVNIPSec() flips ipsec_encapsulation=true at runtime and silently drops
# tunnel traffic if this unit isn't already up — enable at provision time so
# daemon never needs systemd-write capability (only is-active read).
sudo systemctl enable openvswitch-ipsec.service 2>/dev/null || true
# When openvswitch-ipsec is installed before strongswan-starter, the deb
# post-install starts the service against a missing /usr/sbin/ipsec and
# leaves it in 'failed' state. Clear the failure and restart unconditionally
# now that strongswan-starter is present.
sudo systemctl reset-failed openvswitch-ipsec.service 2>/dev/null || true
sudo systemctl restart openvswitch-ipsec.service 2>/dev/null || true
echo "  openvswitch-switch:   enabled on boot"
echo "  ovn-controller:       enabled on boot"
echo "  openvswitch-ipsec:    enabled on boot"

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
