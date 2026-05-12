#!/bin/bash

# Start Spinifex development environment
# This script starts all required services in the correct order using Spinifex service commands
# Usage: ./scripts/start-dev.sh [--build] [data-dir]
#   --build:  Rebuild all binaries before starting (default: skip build)
#   data-dir: Optional data directory path (default: ~/spinifex)
#
# Environment variables:
#   UI=false              Skip starting Spinifex UI (e.g., UI=false ./scripts/start-dev.sh)

set -euo pipefail

echo "WARNING: start-dev.sh is deprecated for local development." >&2
echo "  Use 'make deploy' for daily iteration or 'scripts/dev-install.sh' for first-time setup." >&2
echo "  start-dev.sh is retained only for pseudo-multinode CI (e2e_pseudo_multi)." >&2
echo "" >&2

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
MULGA_ROOT="$(cd "$PROJECT_ROOT/.." && pwd)"

# Parse arguments
BUILD=false
DATA_DIR="$HOME/spinifex"
for arg in "$@"; do
    case "$arg" in
        --build) BUILD=true ;;
        *) DATA_DIR="$arg" ;;
    esac
done

# Configuration paths
# Use CONFIG_DIR environment variable if set, otherwise derive from DATA_DIR
CONFIG_DIR="${CONFIG_DIR:-$DATA_DIR/config}"
echo "Using data directory: $DATA_DIR"
echo "Using configuration directory: $CONFIG_DIR"
LOGS_DIR="$DATA_DIR/logs"
WAL_DIR="$DATA_DIR/spinifex"

echo "⚠️  Dev mode: all services run as $(whoami) without privilege separation"
echo ""
echo "🚀 Starting Spinifex development environment..."
echo "Project root: $PROJECT_ROOT"
echo "Data directory: $DATA_DIR"

# Parse services from spinifex.toml — defaults to all if not set
parse_services() {
    local config_file="$CONFIG_DIR/spinifex.toml"
    if [ -f "$config_file" ]; then
        local svc_line=$(grep -m1 '^services' "$config_file" | sed 's/.*\[//;s/\].*//;s/"//g;s/,/ /g')
        if [ -n "$svc_line" ]; then
            echo "$svc_line"
            return
        fi
    fi
    echo "nats predastore viperblock daemon awsgw vpcd ui"
}

SERVICES=$(parse_services)
has_service() {
    local svc="$1"
    echo "$SERVICES" | grep -qw "$svc"
}
echo "Services: $SERVICES"

# Detect multi-node cluster from config
is_multinode() {
    local config_file="$CONFIG_DIR/spinifex.toml"
    if [ -f "$config_file" ]; then
        local node_count=$(grep -c '^\[nodes\.' "$config_file")
        [ "$node_count" -gt 1 ]
    else
        return 1
    fi
}

# Confirm configuration directory exists
if [ ! -d "$CONFIG_DIR" ]; then
    echo "⚠️  Configuration directory $CONFIG_DIR does not exist."
    echo "Please init the spinifex environment using the CLI."
    echo "spx admin init"
    exit 1
fi


if [ -d "/mnt/ramdisk" ]; then

    # Check if /mnt/ramdisk is mounted, if not mount it as tmpfs
    if ! mountpoint -q /mnt/ramdisk; then
        echo "💾 Mounting /mnt/ramdisk as tmpfs"
        sudo mount -t tmpfs -o size=8G tmpfs /mnt/ramdisk/
    fi

    # If /mnt/ramdisk is mounted, use it for the WAL directory (for development)
    if mountpoint -q "/mnt/ramdisk"; then
        WAL_DIR="/mnt/ramdisk/"
    fi

else
    echo "⚠️  /mnt/ramdisk not available, using $DATA_DIR/viperblock"
fi

# Change to project root for all commands
cd "$PROJECT_ROOT"

# Function to start service in background
start_service() {
    local name="$1"
    local command="$2"
    local pidfile="$LOGS_DIR/$name.pid"
    local logfile="$LOGS_DIR/$name.log"

    echo "📡 Starting $name..."
    echo "   Command: $command"

    # Start service and capture PID
    nohup $command > "$logfile" 2>&1 &
    local pid=$!
    echo $pid > "$pidfile"

    echo "   PID: $pid, Log: $logfile"

    # Brief pause to let service start, then verify it's still alive
    sleep 1
    if ! kill -0 "$pid" 2>/dev/null; then
        echo "   ❌ $name failed to start (exited immediately)"
        echo "   Check log: $logfile"
        tail -5 "$logfile" 2>/dev/null || true
        exit 1
    fi
}

# Function to start service in foreground (for final daemon)
start_service_foreground() {
    local name="$1"
    local command="$2"

    echo "📡 Starting $name in foreground..."
    echo "   Command: $command"

    # Setup signal handler to stop background services when daemon stops
    trap 'echo ""; echo "🛑 Stopping all services..."; ./scripts/stop-dev.sh; exit 0' INT TERM

    # Start the service in foreground
    $command
}

# Function to set OOM score for a service (Linux only)
# Protects infrastructure services from OOM killer (-500 = less likely to be killed)
set_oom_score() {
    local name="$1"
    local score="$2"
    local pid_file="$LOGS_DIR/${name}.pid"
    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -d "/proc/$pid" ]; then
            echo "$score" > "/proc/$pid/oom_score_adj" 2>/dev/null && \
                echo "  OOM score for $name (PID $pid): $score" || \
                echo "  Warning: Could not set OOM score for $name"
        fi
    fi
}

# Function to check if service is responsive
check_service() {
    local name="$1"
    local host="$2"
    local port="$3"
    local max_attempts="${4:-30}"
    local attempt=1

    echo "🔍 Checking $name connectivity on $host:$port..."

    while [ $attempt -le $max_attempts ]; do
        if nc -z "$host" "$port" 2>/dev/null; then
            echo "   ✅ $name is responding on $host:$port"
            return 0
        fi
        echo "   ⏳ Attempt $attempt/$max_attempts - waiting for $name..."
        sleep 2
        ((attempt++))
    done

    echo "   ❌ $name failed to start on $host:$port after $max_attempts attempts"
    exit 1
}

# Pre-flight, compile latest (only with --build flag)
if [ "$BUILD" = "true" ]; then
    echo "✈️  Pre-flight, compiling latest..."

    echo "   Building spinifex..."
    make build

    echo "   Building predastore..."
    cd "$MULGA_ROOT/predastore" && make build
    cd "$PROJECT_ROOT"

    echo "   Building viperblock (nbdkit plugin)..."
    cd "$MULGA_ROOT/viperblock" && make build
    cd "$PROJECT_ROOT"

    echo "   ✅ Build complete"
else
    echo "✈️  Skipping build (pass --build to rebuild)"
fi

# 0️⃣ Start OVN networking (required for VPC)
echo ""
echo "0️⃣  Starting OVN networking..."

if ! pidof systemd >/dev/null 2>&1; then
    echo "   ⚠️  No systemd (container?) — skipping OVN lifecycle management"
elif ! command -v ovs-vsctl >/dev/null 2>&1; then
    echo "   ❌ OVS not installed"
    echo "   Run: make quickinstall && ./scripts/setup-ovn.sh --management"
    exit 1
else
    # Start OVS + OVN services (may already be running)
    sudo systemctl start openvswitch-switch
    echo "   ✅ openvswitch-switch started"

    # Start ovn-central if this node has it installed and enabled
    if systemctl list-unit-files ovn-central.service >/dev/null 2>&1 && systemctl is-enabled ovn-central >/dev/null 2>&1; then
        sudo systemctl start ovn-central
        echo "   ✅ ovn-central started"
    fi

    sudo systemctl start ovn-controller
    echo "   ✅ ovn-controller started"

    # Verify br-int exists (created by setup-ovn.sh)
    if ! sudo ovs-vsctl br-exists br-int 2>/dev/null; then
        echo "   ❌ br-int not found — run ./scripts/setup-ovn.sh --management first"
        exit 1
    fi
    echo "   ✅ br-int exists"
fi

# 1️⃣ Start NATS server
echo ""
if has_service "nats"; then
    echo "1️⃣  Starting NATS server..."

    if [ -f "$CONFIG_DIR/nats/nats.conf" ]; then
        export SPINIFEX_CONFIG_PATH=$CONFIG_DIR/nats/nats.conf
        echo " Using NATS config file: $CONFIG_DIR/nats/nats.conf"
    else
        echo " ⚠️ NATS config file not found at $CONFIG_DIR/nats/nats.conf, using defaults"
        export SPINIFEX_NATS_HOST=0.0.0.0
        export SPINIFEX_NATS_PORT=4222
        export SPINIFEX_NATS_DATA_DIR=$DATA_DIR/nats/
        export SPINIFEX_NATS_JETSTREAM=false
    fi

    # Extract NATS listen address from config (multi-node binds to specific IP, not 127.0.0.1)
    NATS_CHECK_HOST="127.0.0.1"
    if [ -f "$CONFIG_DIR/nats/nats.conf" ]; then
        NATS_LISTEN_IP=$(grep -oP '^listen:\s*\K[^:]+' "$CONFIG_DIR/nats/nats.conf" 2>/dev/null || true)
        if [ -n "$NATS_LISTEN_IP" ] && [ "$NATS_LISTEN_IP" != "0.0.0.0" ]; then
            NATS_CHECK_HOST="$NATS_LISTEN_IP"
        fi
    fi

    NATS_CMD="./bin/spx service nats start"
    start_service "nats" "$NATS_CMD"
    set_oom_score "nats" "-500"
    check_service "NATS" "$NATS_CHECK_HOST" "4222"

    # Wait for NATS JetStream to be ready before Predastore starts.
    # Predastore lazily opens IAM KV buckets — it starts with config-only auth
    # and activates IAM auth once the spinifex daemon creates the KV buckets.
    # Wait for NATS JetStream to be ready before Predastore starts.
    # Predastore lazily opens IAM KV buckets — it starts with config-only auth
    # and activates IAM auth once the spinifex daemon creates the KV buckets.
    # Use the monitoring HTTP endpoint (plaintext nc probe doesn't work with TLS).
    # Monitoring is only on the primary node; secondary nodes skip this check.
    NATS_HAS_MONITORING=false
    if [ -f "$CONFIG_DIR/nats/nats.conf" ] && grep -q '^http:' "$CONFIG_DIR/nats/nats.conf"; then
        NATS_HAS_MONITORING=true
    fi

    if [ "$NATS_HAS_MONITORING" = "true" ]; then
        echo "🔍 Waiting for NATS JetStream..."
        NATS_JS_READY=false
        for i in $(seq 1 15); do
            if curl -sf "http://127.0.0.1:8222/jsz" > /dev/null 2>&1; then
                echo "   ✅ NATS JetStream is ready"
                NATS_JS_READY=true
                break
            fi
            sleep 1
        done
        if [ "$NATS_JS_READY" = "false" ]; then
            echo "   ⚠️  NATS JetStream not confirmed ready — Predastore may fall back to config-only auth"
        fi
    else
        echo "🔍 NATS monitoring not configured — skipping JetStream readiness check"
        sleep 2
    fi
else
    echo "1️⃣  Skipping NATS (not a local service)"
fi

# 2️⃣ Start Predastore
echo ""
if has_service "predastore"; then
    echo "2️⃣  Starting Predastore..."
    unset SPINIFEX_CONFIG_PATH

    export SPINIFEX_PREDASTORE_BASE_PATH=$DATA_DIR/predastore/
    export SPINIFEX_PREDASTORE_CONFIG_PATH=$CONFIG_DIR/predastore/predastore.toml
    export SPINIFEX_PREDASTORE_TLS_CERT=$CONFIG_DIR/server.pem
    export SPINIFEX_PREDASTORE_TLS_KEY=$CONFIG_DIR/server.key

    # Per-node predastore encryption key. Self-heal for devs who skipped
    # `spx admin init`: generate with umask 0177 so the file is 0600 from
    # creation (predastore rejects group/other-readable keys outright).
    ENCRYPTION_KEY="$CONFIG_DIR/predastore/encryption.key"
    if [ ! -f "$ENCRYPTION_KEY" ]; then
        mkdir -p "$CONFIG_DIR/predastore"
        ( umask 0177 && openssl rand -out "$ENCRYPTION_KEY" 32 )
        echo "   Generated predastore encryption key: $ENCRYPTION_KEY"
    fi
    export SPINIFEX_PREDASTORE_ENCRYPTION_KEY_FILE="$ENCRYPTION_KEY"

    # Auto-detect Predastore host:port from spinifex.toml [nodes.<name>.predastore] section
    PREDASTORE_BIND="0.0.0.0:8443"
    if [ -f "$CONFIG_DIR/spinifex.toml" ]; then
        DETECTED_PREDASTORE_HOST=$(awk -F'"' '/\[nodes\..*\.predastore\]/{found=1} found && /^host/{print $2; exit}' "$CONFIG_DIR/spinifex.toml")
        if [ -n "$DETECTED_PREDASTORE_HOST" ]; then
            PREDASTORE_BIND="$DETECTED_PREDASTORE_HOST"
            echo "   Auto-detected Predastore bind=$PREDASTORE_BIND from spinifex.toml"
        fi
    fi
    export SPINIFEX_PREDASTORE_HOST="${PREDASTORE_BIND%%:*}"
    export SPINIFEX_PREDASTORE_PORT="${PREDASTORE_BIND##*:}"

    export SPINIFEX_PREDASTORE_BACKEND=distributed

    # Auto-detect Predastore NODE_ID from spinifex.toml if not already set
    if [ -z "${SPINIFEX_PREDASTORE_NODE_ID:-}" ]; then
        if [ -f "$CONFIG_DIR/spinifex.toml" ]; then
            DETECTED_NODE_ID=$(awk -F'= *' '/node_id/{gsub(/ /,"",$2); print $2; exit}' "$CONFIG_DIR/spinifex.toml")
            if [ -n "$DETECTED_NODE_ID" ] && [ "$DETECTED_NODE_ID" != "0" ]; then
                export SPINIFEX_PREDASTORE_NODE_ID="$DETECTED_NODE_ID"
                echo "   Auto-detected Predastore NODE_ID=$DETECTED_NODE_ID from spinifex.toml"
            fi
        fi
    fi
    export SPINIFEX_PREDASTORE_NODE_ID="${SPINIFEX_PREDASTORE_NODE_ID:-}"

    PREDASTORE_CMD="./bin/spx service predastore start"
    start_service "predastore" "$PREDASTORE_CMD"
    set_oom_score "predastore" "-500"
    if is_multinode; then
        echo "   ⏭️  Skipping Predastore connectivity check (multi-node: needs quorum from peer nodes)"
    else
        check_service "Predastore" "$SPINIFEX_PREDASTORE_HOST" "$SPINIFEX_PREDASTORE_PORT"
    fi
else
    echo "2️⃣  Skipping Predastore (not a local service)"
fi


# 3️⃣ Start Viperblock
echo ""
if has_service "viperblock"; then
    echo "3️⃣  Starting Viperblock..."

    # Determine base directory for Viperblock data, dev uses /mnt/ramdisk if available
    if [ -d "/mnt/ramdisk" ] && [ -w "/mnt/ramdisk" ]; then
        VB_BASE_DIR="/mnt/ramdisk/"
    else
        VB_BASE_DIR="$DATA_DIR/viperblock/"
    fi

    # Check if NBD plugin exists
    NBD_PLUGIN_PATH="$MULGA_ROOT/viperblock/lib/nbdkit-viperblock-plugin.so"

    if [ ! -f "$NBD_PLUGIN_PATH" ]; then
        echo "   ⚠️  NBD plugin not found at $NBD_PLUGIN_PATH"
        echo "   Building Viperblock NBD plugin..."
        cd "$MULGA_ROOT/viperblock" && make build
        cd "$PROJECT_ROOT"
    fi

    export SPINIFEX_CONFIG_PATH=$CONFIG_DIR/spinifex.toml
    export SPINIFEX_VIPERBLOCK_PLUGIN_PATH=$NBD_PLUGIN_PATH
    export SPINIFEX_BASE_DIR=$VB_BASE_DIR

    VIPERBLOCK_CMD="./bin/spx service viperblock start"
    start_service "viperblock" "$VIPERBLOCK_CMD"
    set_oom_score "viperblock" "-500"
else
    echo "3️⃣  Skipping Viperblock (not a local service)"
fi

# 4️⃣ Start Spinifex Gateway/Daemon
echo ""
if has_service "daemon"; then
    echo "4️⃣. Starting Spinifex Gateway..."

    export SPINIFEX_CONFIG_PATH=$CONFIG_DIR/spinifex.toml
    export SPINIFEX_BASE_DIR=$DATA_DIR/spinifex/
    export SPINIFEX_WAL_DIR=$WAL_DIR

    SPINIFEX_CMD="./bin/spx service spinifex start"
    start_service "spinifex" "$SPINIFEX_CMD"
    set_oom_score "spinifex" "-500"
else
    echo "4️⃣. Skipping Spinifex Gateway (not a local service)"
fi


# 5️⃣ Start AWS Gateway
echo ""
if has_service "awsgw"; then
    echo "5️⃣. Starting AWS Gateway..."

    unset SPINIFEX_NATS_HOST
    unset SPINIFEX_PREDASTORE_HOST
    export SPINIFEX_BASE_DIR=$DATA_DIR
    export SPINIFEX_CONFIG_PATH=$CONFIG_DIR/spinifex.toml
    export SPINIFEX_AWSGW_TLS_CERT=$CONFIG_DIR/server.pem
    export SPINIFEX_AWSGW_TLS_KEY=$CONFIG_DIR/server.key

    AWSGW_CMD="./bin/spx service awsgw start"
    start_service "awsgw" "$AWSGW_CMD"
    set_oom_score "awsgw" "-500"
else
    echo "5️⃣. Skipping AWS Gateway (not a local service)"
fi


# 6️⃣ Start vpcd (VPC daemon)
echo ""
if has_service "vpcd"; then
    echo "6️⃣. Starting vpcd (VPC daemon)..."

    export SPINIFEX_CONFIG_PATH=$CONFIG_DIR/spinifex.toml

    VPCD_CMD="./bin/spx service vpcd start"
    start_service "vpcd" "$VPCD_CMD"
    set_oom_score "vpcd" "-500"
else
    echo "6️⃣. Skipping vpcd (not a local service)"
fi

# 7️⃣ Start Spinifex UI (skip with UI=false or if not a local service)
if [ "${UI:-}" != "false" ] && has_service "ui"; then
    echo ""
    echo "7️⃣. Starting Spinifex UI..."

    SPXUI_CMD="./bin/spx service spinifex-ui start"
    start_service "spinifex-ui" "$SPXUI_CMD"
    set_oom_score "spinifex-ui" "-500"
else
    echo ""
    echo "7️⃣. Skipping Spinifex UI"
fi


echo ""
echo "🔗 Service endpoints will be:"
if [ "${UI:-}" != "false" ]; then
    echo "   - Spinifex UI:       https://localhost:3000"
fi
echo "   - NATS:          nats://localhost:4222"
echo "   - Predastore:    https://localhost:8443"
echo "   - AWS Gateway:   https://localhost:9999"
echo ""
echo "📊 Monitor background service logs:"
echo "   tail -f $LOGS_DIR/*.log"
echo ""
echo "🧪 Test with AWS CLI (once daemon is running):"
echo "   export AWS_PROFILE=spinifex"
echo "   aws ec2 describe-instances"
echo ""

# For multi-node clusters, check peer daemon health (best-effort)
if is_multinode; then
    echo "Checking cluster peer health..."
    # Extract peer daemon hosts from config (host = "ip:port" under [nodes.X.daemon])
    peer_hosts=$(awk '
        /^\[nodes\./ { node=1; daemon=0 }
        node && /^\[.*\.daemon\]/ { daemon=1 }
        daemon && /^host/ { gsub(/[" ]/, "", $3); print $3; daemon=0 }
    ' "$CONFIG_DIR/spinifex.toml")

    for peer in $peer_hosts; do
        attempts=0
        max_attempts=5
        while [ $attempts -lt $max_attempts ]; do
            if curl -sk --connect-timeout 3 "https://$peer/health" > /dev/null 2>&1; then
                echo "   $peer: healthy"
                break
            fi
            attempts=$((attempts + 1))
            if [ $attempts -lt $max_attempts ]; then
                sleep 2
            fi
        done
        if [ $attempts -ge $max_attempts ]; then
            echo "   $peer: not responding (may still be starting)"
        fi
    done
    echo ""
fi

# This will only be reached if air/daemon exits normally
#echo ""
#echo "🛑 Spinifex development environment stopped"
