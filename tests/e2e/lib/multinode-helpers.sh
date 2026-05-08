#!/bin/bash

# Multi-node E2E test helper functions
# Provides utilities for managing simulated IPs, starting/stopping node services,
# and verifying NATS cluster health.



# Network configuration
SIMULATED_NETWORK="10.11.12"
NODE1_IP="${SIMULATED_NETWORK}.1"
NODE2_IP="${SIMULATED_NETWORK}.2"
NODE3_IP="${SIMULATED_NETWORK}.3"

# Port configuration
NATS_CLIENT_PORT=4222
NATS_CLUSTER_PORT=4248
NATS_MONITOR_PORT=8222
PREDASTORE_PORT=8443
AWSGW_PORT=9999
CLUSTER_PORT=4432

# Add simulated IPs to loopback interface
# Requires NET_ADMIN capability
add_simulated_ips() {
    echo "Adding simulated IPs to loopback interface..."

    for i in 1 2 3; do
        local ip="${SIMULATED_NETWORK}.$i"
        if ! ip addr show lo | grep -q "$ip"; then
            sudo ip addr add "${ip}/24" dev lo
            echo "  Added $ip to lo"
        else
            echo "  $ip already exists on lo"
        fi
    done

    # Verify IPs were added
    echo "Verifying simulated IPs..."
    for i in 1 2 3; do
        local ip="${SIMULATED_NETWORK}.$i"
        if ip addr show lo | grep -q "$ip"; then
            echo "  $ip is configured"
        else
            echo "  ERROR: Failed to add $ip"
            return 1
        fi
    done

    echo "Simulated IPs configured successfully"
}

# Remove simulated IPs from loopback interface
remove_simulated_ips() {
    echo "Removing simulated IPs from loopback interface..."

    for i in 1 2 3; do
        local ip="${SIMULATED_NETWORK}.$i"
        if ip addr show lo | grep -q "$ip"; then
            sudo ip addr del "${ip}/24" dev lo 2>/dev/null || true
            echo "  Removed $ip from lo"
        fi
    done

    echo "Simulated IPs removed"
}

# Start services for a specific node
# Usage: start_node_services <node_num> <data_dir>
# Example: start_node_services 1 ~/node1
start_node_services() {
    local node_num="$1"
    local data_dir="$2"
    local node_ip="${SIMULATED_NETWORK}.$node_num"

    echo "Starting services for node$node_num at $node_ip..."

    # Start all services - each node's config binds to its specific IP
    # UI is not needed for E2E tests and fails in pseudo multi-node (wrong cert path)
    UI=false ./scripts/start-dev.sh "$data_dir"

    echo "Node$node_num services started"
}

# Stop services for a specific node
# Usage: stop_node_services <node_num> <data_dir>
stop_node_services() {
    local node_num="$1"
    local data_dir="$2"

    echo "Stopping services for node$node_num..."

    # Stop using PID files in the node's log directory
    ./scripts/stop-dev.sh "$data_dir"

    echo "Node$node_num services stopped"
}

# Stop all node services
stop_all_nodes() {
    echo "Stopping all node services..."

    for i in 1 2 3; do
        local data_dir="$HOME/node$i"
        if [ -d "$data_dir/logs" ]; then
            stop_node_services "$i" "$data_dir" || true
        fi
    done

    echo "All node services stopped"
}

# Verify NATS cluster health
# Checks that the cluster has expected number of members
# Usage: verify_nats_cluster [expected_members]
verify_nats_cluster() {
    local expected_members="${1:-3}"

    echo "Verifying NATS cluster health (expecting $expected_members members)..."

    # Check cluster info via monitoring endpoint on node1
    local cluster_info
    cluster_info=$(curl -s "http://127.0.0.1:${NATS_MONITOR_PORT}/routez" 2>/dev/null) || {
        echo "  ERROR: Cannot reach NATS monitoring endpoint"
        return 1
    }

    # Count unique remote servers (NATS creates multiple connections per peer)
    local num_routes
    num_routes=$(echo "$cluster_info" | jq -r '.num_routes // 0')

    # Count unique peer names
    local unique_peers
    unique_peers=$(echo "$cluster_info" | jq -r '[.routes[].remote_name] | unique | length')
    local expected_peers=$((expected_members - 1))

    echo "  NATS cluster routes: $num_routes (unique peers: $unique_peers, expected peers: $expected_peers)"

    if [ "$unique_peers" -ge "$expected_peers" ]; then
        echo "  NATS cluster is healthy"
        return 0
    else
        echo "  WARNING: NATS cluster may not be fully formed"
        echo "  Cluster info: $cluster_info"
        return 1
    fi
}

# Wait for a specific instance state
# Usage: wait_for_instance_state <instance_id> <target_state> [max_attempts]
wait_for_instance_state() {
    local instance_id="$1"
    local target_state="$2"
    local max_attempts="${3:-20}"
    local attempt=0

    echo "Waiting for instance $instance_id to reach state: $target_state..."

    while [ $attempt -lt $max_attempts ]; do
        local state
        state=$(aws --endpoint-url https://${NODE1_IP}:${AWSGW_PORT} ec2 describe-instances \
            --instance-ids "$instance_id" \
            --query 'Reservations[0].Instances[0].State.Name' \
            --output text) || {
            echo "  Attempt $((attempt + 1))/$max_attempts - Gateway request failed, retrying..."
            sleep 1
            attempt=$((attempt + 1))
            continue
        }

        echo "  Instance state: $state"

        if [ "$state" == "$target_state" ]; then
            echo "  Instance reached target state: $target_state"
            return 0
        fi

        if [ "$state" == "terminated" ] && [ "$target_state" != "terminated" ]; then
            echo "  ERROR: Instance terminated unexpectedly"
            return 1
        fi

        sleep 1
        attempt=$((attempt + 1))
    done

    echo "  ERROR: Instance did not reach $target_state within $max_attempts attempts"
    return 1
}

# Terminate instances and wait for them to reach terminated state
# Usage: terminate_and_wait <instance_id> [instance_id...]
terminate_and_wait() {
    local ids=("$@")

    for instance_id in "${ids[@]}"; do
        local state
        state=$($AWS_EC2 describe-instances --instance-ids "$instance_id" \
            --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null || echo "unknown")
        if [ "$state" != "terminated" ] && [ "$state" != "unknown" ]; then
            echo "  Terminating $instance_id (state: $state)..."
            $AWS_EC2 terminate-instances --instance-ids "$instance_id" > /dev/null 2>&1 || true
        fi
    done

    local failed=0
    for instance_id in "${ids[@]}"; do
        if ! wait_for_instance_state "$instance_id" "terminated" 60; then
            echo "  WARNING: Failed to confirm termination of $instance_id"
            failed=1
        fi
    done

    return $failed
}

# Wait for gateway to be ready
# Usage: wait_for_gateway [host] [max_attempts]
wait_for_gateway() {
    local host="${1:-localhost}"
    local max_attempts="${2:-30}"
    local attempt=0

    echo "Waiting for AWS Gateway at $host:${AWSGW_PORT}..."

    while [ $attempt -lt $max_attempts ]; do
        if curl -k -s "https://${host}:${AWSGW_PORT}" > /dev/null 2>&1; then
            echo "  Gateway is ready"
            return 0
        fi

        echo "  Waiting for gateway... ($((attempt + 1))/$max_attempts)"
        sleep 1
        attempt=$((attempt + 1))
    done

    echo "  ERROR: Gateway failed to start"
    return 1
}

# Wait for daemon NATS subscriptions to be active.
# Polls describe-instance-types until it returns a non-empty result.
# Usage: wait_for_daemon_ready <gateway_endpoint> [max_attempts]
wait_for_daemon_ready() {
    local endpoint="$1"
    local max_attempts="${2:-30}"
    local attempt=0

    echo "Waiting for daemon readiness (NATS subscriptions)..."

    while [ $attempt -lt $max_attempts ]; do
        local types
        types=$(aws --endpoint-url "$endpoint" ec2 describe-instance-types \
            --query 'InstanceTypes[*].InstanceType' --output text 2>/dev/null || true)
        if [ -n "$types" ] && [ "$types" != "None" ]; then
            echo "  Daemon is ready"
            return 0
        fi
        echo "  Waiting... ($((attempt + 1))/$max_attempts)"
        sleep 1
        attempt=$((attempt + 1))
    done

    echo "  ERROR: Daemon not ready after $max_attempts attempts"
    return 1
}

# Check instance distribution across nodes
# Counts QEMU instances per node by matching nbdkit host= to simulated node IPs
# Usage: check_instance_distribution [expected_nodes]
# If expected_nodes is provided, fails when instances don't span that many nodes
check_instance_distribution() {
    local expected_nodes="${1:-0}"

    echo "Checking instance distribution across nodes..."

    local total=0
    local nodes_used=0
    for i in 1 2 3; do
        local node_ip="${SIMULATED_NETWORK}.$i"
        # Count nbdkit root-volume processes (not cloudinit) bound to this node's predastore
        local count
        count=$(ps auxw | grep nbdkit | grep -v grep | grep -v cloudinit | grep -c "host=${node_ip}:" 2>/dev/null) || count=0
        echo "  Node$i ($node_ip): $count instances"
        total=$((total + count))
        if [ "$count" -gt 0 ]; then
            nodes_used=$((nodes_used + 1))
        fi
    done

    echo "  Total instances: $total (across $nodes_used nodes)"

    if [ "$expected_nodes" -gt 0 ] && [ "$nodes_used" -lt "$expected_nodes" ]; then
        echo "  FAIL: Expected instances on $expected_nodes nodes, only used $nodes_used"
        return 1
    fi
}

# Find the SSH port for an instance from the QEMU process
# Usage: get_ssh_port <instance_id>
# Returns: the SSH port number, or empty string if not found
get_ssh_port() {
    local instance_id="$1"
    local max_attempts="${2:-30}"
    local attempt=0

    while [ $attempt -lt $max_attempts ]; do
        local qemu_cmd
        qemu_cmd=$(ps auxw | grep "$instance_id" | grep qemu-system | grep -v grep || true)

        if [ -n "$qemu_cmd" ]; then
            # Extract port from hostfwd=tcp:127.0.0.1:PORT-:22 or hostfwd=tcp::PORT-:22
            local ssh_port
            ssh_port=$(echo "$qemu_cmd" | sed -n 's/.*hostfwd=tcp:[^:]*:\([0-9]*\)-:22.*/\1/p')

            if [ -n "$ssh_port" ]; then
                echo "$ssh_port"
                return 0
            fi
        fi

        attempt=$((attempt + 1))
        if [ $attempt -lt $max_attempts ]; then
            sleep 1
        fi
    done

    return 1
}

# Find the SSH host IP for an instance from the QEMU process hostfwd setting
# Usage: get_ssh_host <instance_id>
# Returns: the SSH host IP (e.g., 10.11.12.1, 127.0.0.1), or "127.0.0.1" if not found/empty
get_ssh_host() {
    local instance_id="$1"

    local qemu_cmd
    qemu_cmd=$(ps auxw | grep "$instance_id" | grep qemu-system | grep -v grep || true)

    if [ -n "$qemu_cmd" ]; then
        # Extract host from hostfwd=tcp:HOST:PORT-:22
        # Pattern matches: hostfwd=tcp:IP:PORT-:22 where IP can be empty
        local ssh_host
        ssh_host=$(echo "$qemu_cmd" | sed -n 's/.*hostfwd=tcp:\([^:]*\):[0-9]*-:22.*/\1/p')

        if [ -n "$ssh_host" ]; then
            echo "$ssh_host"
            return 0
        fi
    fi

    # Default to localhost if not found
    echo "127.0.0.1"
    return 0
}

# Find which node an instance is running on (for pseudo multi-node setups)
# Matches the QEMU process's root nbd socket to its nbdkit process's host IP
# Usage: get_instance_node <instance_id>
# Returns: node number (1, 2, or 3), or empty if not found
get_instance_node() {
    local instance_id="$1"

    # Find the QEMU process and extract the root nbd socket path
    local qemu_cmd
    qemu_cmd=$(ps auxw | grep "$instance_id" | grep qemu-system | grep -v grep || true)
    if [ -z "$qemu_cmd" ]; then
        return 1
    fi

    # Extract first nbd socket from drive args (root volume, not cloudinit)
    local nbd_sock
    nbd_sock=$(echo "$qemu_cmd" | grep -o 'nbd:unix:[^ ,]*' | head -1 | sed 's/nbd:unix://')
    if [ -z "$nbd_sock" ]; then
        return 1
    fi

    # Find the nbdkit process serving this socket and extract its host=IP
    local nbdkit_cmd
    nbdkit_cmd=$(ps auxw | grep nbdkit | grep -v grep | grep "$nbd_sock" || true)
    if [ -z "$nbdkit_cmd" ]; then
        return 1
    fi

    # Extract host=10.11.12.X from nbdkit args and map to node number
    local host_ip
    host_ip=$(echo "$nbdkit_cmd" | grep -o "host=${SIMULATED_NETWORK}\.[0-9]*" | sed "s/host=${SIMULATED_NETWORK}\.//" | head -1)
    if [ -n "$host_ip" ]; then
        echo "$host_ip"
        return 0
    fi

    return 1
}

# Wait for SSH to be ready on an instance
# Usage: wait_for_ssh <host> <port> <key_file> [max_attempts]
# Returns: 0 if SSH is ready, 1 if timeout
wait_for_ssh() {
    local host="$1"
    local port="$2"
    local key_file="$3"
    local max_attempts="${4:-30}"
    local attempt=0

    echo "  Waiting for SSH to be ready on $host:$port..."

    while [ $attempt -lt $max_attempts ]; do
        # Add host key to known_hosts (suppress errors)
        ssh-keyscan -p "$port" "$host" >> ~/.ssh/known_hosts 2>/dev/null || true

        # Try to connect with short timeout
        if ssh -o StrictHostKeyChecking=no \
               -o UserKnownHostsFile=/dev/null \
               -o ConnectTimeout=2 \
               -o BatchMode=yes \
               -p "$port" \
               -i "$key_file" \
               ec2-user@"$host" 'echo ready' > /dev/null 2>&1; then
            echo "  SSH is ready"
            return 0
        fi

        attempt=$((attempt + 1))
        if [ $attempt -lt $max_attempts ]; then
            if [ $((attempt % 10)) -eq 0 ]; then
                echo "  Waiting for SSH... ($attempt/$max_attempts)"
            fi
            sleep 1
        fi
    done

    echo "  ERROR: SSH not ready after $max_attempts attempts"
    dump_local_ovn_diagnostics
    return 1
}

# Local OVN/OVS state dump for SSH-failure diagnostics on single-node and
# pseudo-multinode runners (where the primary == hosting node == local).
# Multi-node has dump_guest_ssh_diagnostics with peer_ssh; this is the
# local-only sibling. See docs/development/bugs/sg-enforcement-e2e-ssh.md.
dump_local_ovn_diagnostics() {
    if ! command -v ovn-nbctl >/dev/null 2>&1; then
        echo "  --- ovn-nbctl not installed; skipping OVN dump ---"
        return 0
    fi

    echo ""
    echo "  === Local OVN/OVS state dump ==="

    echo "  --- NB Logical_Switch_Port (head 120) ---"
    sudo ovn-nbctl --bare --columns=name,addresses,port_security,up \
        list Logical_Switch_Port 2>&1 | head -120 || true

    echo "  --- NB Logical_Switch ports ---"
    sudo ovn-nbctl --bare --columns=name,ports list Logical_Switch 2>&1 || true

    echo "  --- NB Port_Group ports + ACLs (head 120) ---"
    sudo ovn-nbctl --bare --columns=name,ports,acls list Port_Group 2>&1 \
        | head -120 || true

    echo "  --- NB Address_Set (head 60) ---"
    sudo ovn-nbctl --bare --columns=name,addresses list Address_Set 2>&1 \
        | head -60 || true

    echo "  --- SB Port_Binding (head 120) ---"
    sudo ovn-sbctl --bare --columns=logical_port,chassis,up,mac \
        list Port_Binding 2>&1 | head -120 || true

    echo "  --- SB Chassis ---"
    sudo ovn-sbctl --bare --columns=name,hostname list Chassis 2>&1 || true

    echo "  --- OVS Interface external_ids (br-int taps, head 120) ---"
    sudo ovs-vsctl --bare --columns=name,external_ids,admin_state,link_state \
        list Interface 2>&1 | head -120 || true

    echo "  --- ovn-controller journal (last 60 lines) ---"
    sudo journalctl -u ovn-controller --no-pager -n 60 2>&1 || true

    echo "  --- ovn-northd journal (last 40 lines) ---"
    sudo journalctl -u ovn-northd --no-pager -n 40 2>&1 || true

    echo "  === end local OVN/OVS state dump ==="
    echo ""
}

# Test SSH connectivity by running 'id' command and verifying ec2-user in output
# Usage: test_ssh_connectivity <host> <port> <key_file>
# Returns: 0 if successful and ec2-user found, 1 otherwise
test_ssh_connectivity() {
    local host="$1"
    local port="$2"
    local key_file="$3"

    echo "  Testing SSH connectivity (running 'id' command)..."

    local id_output
    id_output=$(ssh -o StrictHostKeyChecking=no \
                    -o UserKnownHostsFile=/dev/null \
                    -o ConnectTimeout=5 \
                    -o BatchMode=yes \
                    -p "$port" \
                    -i "$key_file" \
                    ec2-user@"$host" 'id' 2>&1) || {
        echo "  ERROR: SSH command failed"
        echo "  Output: $id_output"
        return 1
    }

    echo "  SSH 'id' output: $id_output"

    if echo "$id_output" | grep -q "ec2-user"; then
        echo "  SSH connectivity test passed (ec2-user confirmed)"
        return 0
    else
        echo "  ERROR: Expected 'ec2-user' in id output"
        return 1
    fi
}

# Verify SSH is NOT reachable (used after instance termination)
# Usage: verify_ssh_unreachable <host> <port> <key_file>
# Returns: 0 if SSH is unreachable (expected), 1 if SSH is still reachable (unexpected)
verify_ssh_unreachable() {
    local host="$1"
    local port="$2"
    local key_file="$3"

    echo "  Verifying SSH is no longer reachable..."

    # Try to connect with 1 second timeout - should fail
    if ssh -o StrictHostKeyChecking=no \
           -o UserKnownHostsFile=/dev/null \
           -o ConnectTimeout=1 \
           -o BatchMode=yes \
           -p "$port" \
           -i "$key_file" \
           ec2-user@"$host" 'echo connected' > /dev/null 2>&1; then
        echo "  ERROR: SSH is still reachable after termination"
        return 1
    else
        echo "  SSH is no longer reachable (as expected)"
        return 0
    fi
}

# Expect an AWS CLI command to fail with a specific error code
# Usage: expect_error "ErrorCode" aws ec2 some-command --args...
# Returns: 0 if the command fails with the expected error, 1 otherwise
expect_error() {
    local expected_error="$1"
    shift

    set +e
    local output
    output=$("$@" 2>&1)
    local exit_code=$?
    set -e

    if [ $exit_code -eq 0 ]; then
        echo "  FAIL: Expected error '$expected_error' but command succeeded"
        echo "  Output: $output"
        return 1
    fi

    if echo "$output" | grep -q "$expected_error"; then
        echo "  Got expected error: $expected_error"
        return 0
    else
        echo "  FAIL: Expected error '$expected_error' but got different error"
        echo "  Output: $output"
        return 1
    fi
}

# Dump logs from all nodes (for debugging failures)
dump_all_node_logs() {
    echo ""
    echo "=========================================="
    echo "DUMPING LOGS FROM ALL NODES"
    echo "=========================================="

    # Show running spx/nats processes for context
    echo ""
    echo "--- Running processes ---"
    ps auxw | grep -E 'spx|nats-server' | grep -v grep || echo "(none)"

    for i in 1 2 3; do
        local data_dir="$HOME/node$i"
        local logs_dir="$data_dir/logs"

        echo ""
        echo "=== Node$i ==="

        # Show directory structure
        echo "--- $data_dir contents ---"
        sudo ls -la "$data_dir/" 2>/dev/null || echo "(dir not found)"

        if sudo test -d "$logs_dir"; then
            echo "--- $logs_dir contents ---"
            sudo ls -la "$logs_dir/" 2>/dev/null || true

            for log in nats predastore viperblock spinifex awsgw vpcd; do
                if sudo test -f "$logs_dir/$log.log"; then
                    echo ""
                    echo "--- $log.log (last 50 lines) ---"
                    sudo tail -50 "$logs_dir/$log.log" 2>/dev/null || echo "(empty or not accessible)"
                fi
            done
        else
            echo "--- $logs_dir not found, checking PID files ---"
            sudo find "$data_dir" -name '*.pid' -o -name '*.log' 2>/dev/null || true
        fi
    done

    dump_ovn_state

    echo ""
    echo "=========================================="
    echo "END OF LOG DUMP"
    echo "=========================================="
}

# Dump OVN northbound state (cluster-wide; one dump suffices). Captures the
# programmed port groups, ACLs and LSPs so SG-enforcement failures can be
# diagnosed against what vpcd actually wrote.
dump_ovn_state() {
    if ! command -v ovn-nbctl >/dev/null 2>&1; then
        return
    fi
    echo ""
    echo "=== OVN northbound state ==="
    for cmd in "show" "list port_group" "list acl" "list logical_switch_port"; do
        echo ""
        echo "--- ovn-nbctl $cmd ---"
        sudo ovn-nbctl $cmd 2>&1 || echo "(ovn-nbctl $cmd failed)"
    done
}

# Get the QEMU process PID for an instance
# Usage: get_qemu_pid <instance_id>
# Returns: the PID, or empty string if not found
get_qemu_pid() {
    local instance_id="$1"

    local pid
    pid=$(ps auxw | grep "$instance_id" | grep qemu-system | grep -v grep | awk '{print $2}' | head -1)

    if [ -n "$pid" ]; then
        echo "$pid"
        return 0
    fi

    return 1
}

# Wait for an instance to recover from a crash (error → running)
# Usage: wait_for_instance_recovery <instance_id> [max_attempts]
# Expects state transition: error → pending → running
wait_for_instance_recovery() {
    local instance_id="$1"
    local max_attempts="${2:-60}"
    local attempt=0
    local saw_error=false

    echo "  Waiting for instance $instance_id to recover..."

    while [ $attempt -lt $max_attempts ]; do
        local state
        state=$(aws ec2 describe-instances \
            --instance-ids "$instance_id" \
            --query 'Reservations[0].Instances[0].State.Name' \
            --output text 2>/dev/null) || {
            sleep 1
            attempt=$((attempt + 1))
            continue
        }

        if [ "$state" == "error" ] || [ "$state" == "pending" ]; then
            saw_error=true
            echo "  State: $state (attempt $((attempt + 1))/$max_attempts)"
        fi

        if [ "$state" == "running" ] && [ "$saw_error" = true ]; then
            echo "  Instance recovered to running state"
            return 0
        fi

        if [ "$state" == "running" ] && [ "$saw_error" = false ]; then
            # Still in original running state, crash not detected yet
            :
        fi

        sleep 1
        attempt=$((attempt + 1))
    done

    echo "  ERROR: Instance did not recover within $max_attempts attempts"
    return 1
}

# Verify all services are down on all nodes
# Returns 0 if everything is down, 1 if something is still running
verify_all_services_down() {
    local all_down=true

    for i in 1 2 3; do
        local node_ip="${SIMULATED_NETWORK}.$i"

        # Check gateway
        if curl -k -s --connect-timeout 2 "https://${node_ip}:${AWSGW_PORT}" > /dev/null 2>&1; then
            echo "  Node$i: gateway still responding"
            all_down=false
        fi

        # Check NATS (client port — monitoring is localhost-only)
        if nc -z -w 2 "${node_ip}" ${NATS_CLIENT_PORT} 2>/dev/null; then
            echo "  Node$i: NATS still responding"
            all_down=false
        fi
    done

    # Check for any remaining QEMU processes
    if pgrep -x qemu-system-x86_64 > /dev/null 2>&1; then
        echo "  QEMU processes still running"
        all_down=false
    fi

    if [ "$all_down" = true ]; then
        echo "  All services confirmed down"
        return 0
    fi
    return 1
}

# Force-kill all service processes and clean up stale resources on all nodes.
# Used between shutdown and restart to ensure a clean slate.
# This kills processes by PID file, then by name, removes badger LOCK files,
# and waits for ports to be free.
force_cleanup_all_nodes() {
    echo "Force-cleaning all nodes..."

    # Step 1: Kill all service processes via PID files
    for i in 1 2 3; do
        local data_dir="$HOME/node$i"
        local logs_dir="$data_dir/logs"

        if [ -d "$logs_dir" ]; then
            for svc in spinifex-ui spinifex awsgw viperblock predastore nats; do
                local pidfile="$logs_dir/$svc.pid"
                if [ -f "$pidfile" ]; then
                    local pid
                    pid=$(cat "$pidfile" 2>/dev/null || true)
                    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
                        echo "  Node$i: killing $svc (PID $pid)..."
                        kill -TERM "$pid" 2>/dev/null || true
                    fi
                fi
            done
        fi
    done

    # Brief wait for graceful shutdown
    sleep 3

    # Step 2: SIGKILL anything still alive
    for i in 1 2 3; do
        local data_dir="$HOME/node$i"
        local logs_dir="$data_dir/logs"

        if [ -d "$logs_dir" ]; then
            for svc in spinifex-ui spinifex awsgw viperblock predastore nats; do
                local pidfile="$logs_dir/$svc.pid"
                if [ -f "$pidfile" ]; then
                    local pid
                    pid=$(cat "$pidfile" 2>/dev/null || true)
                    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
                        echo "  Node$i: force-killing $svc (PID $pid)..."
                        kill -9 "$pid" 2>/dev/null || true
                    fi
                fi
            done
        fi
    done

    # Kill any remaining QEMU processes
    pkill -9 -x qemu-system-x86_64 2>/dev/null || true

    sleep 1

    # Step 3: Remove stale badger LOCK files from predastore directories
    for i in 1 2 3; do
        local data_dir="$HOME/node$i"
        local predastore_dir="$data_dir/predastore"

        if [ -d "$predastore_dir" ]; then
            local lock_files
            lock_files=$(find "$predastore_dir" -name "LOCK" -type f 2>/dev/null || true)
            if [ -n "$lock_files" ]; then
                echo "  Node$i: removing stale badger LOCK files..."
                echo "$lock_files" | while read -r f; do
                    rm -f "$f"
                    echo "    removed $f"
                done
            fi
        fi
    done

    # Step 4: Wait for key ports to be free
    for i in 1 2 3; do
        local node_ip="${SIMULATED_NETWORK}.$i"
        local attempt=0
        while [ $attempt -lt 10 ]; do
            if ! ss -tlnp 2>/dev/null | grep -q "${node_ip}:${AWSGW_PORT}"; then
                break
            fi
            echo "  Node$i: waiting for port ${AWSGW_PORT} to be free..."
            sleep 1
            attempt=$((attempt + 1))
        done
    done

    echo "  Force cleanup complete"
}

# Global variable for init PID tracking (used by multi-node formation)
LEADER_INIT_PID=""

# Initialize leader node (node1)
# In multi-node mode (default --nodes 3), this backgrounds the init process
# because the formation server blocks waiting for joins.
# Usage: init_leader_node
init_leader_node() {
    echo "Initializing leader node (node1)..."

    # Remove old node directory
    rm -rf "$HOME/node1/"

    # External pool flags (optional — enables public subnet IP allocation)
    local external_flags=""
    if [ -n "${SPX_EXTERNAL_POOL:-}" ]; then
        external_flags="--external-mode=pool --external-pool=${SPX_EXTERNAL_POOL}"
        echo "  External pool: ${SPX_EXTERNAL_POOL}"
    fi

    # Start init in background — formation server will wait for joins
    # shellcheck disable=SC2086
    ./bin/spx admin init \
        --node node1 \
        --nodes 3 \
        --bind "${NODE1_IP}" \
        --cluster-bind "${NODE1_IP}" \
        --port ${CLUSTER_PORT} \
        --region ap-southeast-2 \
        --az ap-southeast-2a \
        --spinifex-dir "$HOME/node1/" \
        --config-dir "$HOME/node1/config/" \
        $external_flags &
    LEADER_INIT_PID=$!

    # Wait for formation server to be ready
    echo "Waiting for formation server..."
    for i in $(seq 1 60); do
        if curl -sk "https://${NODE1_IP}:${CLUSTER_PORT}/formation/health" > /dev/null 2>&1; then
            echo "  Formation server is ready (PID: $LEADER_INIT_PID)"

            # Read join token written by admin init
            JOIN_TOKEN=$(cat "$HOME/node1/config/join-token")
            if [ -z "$JOIN_TOKEN" ]; then
                echo "  ERROR: Join token file is empty"
                return 1
            fi
            echo "  Join token loaded"
            export JOIN_TOKEN

            return 0
        fi
        sleep 1
    done

    echo "  ERROR: Formation server failed to start"
    return 1
}

# Join a follower node to the cluster
# Usage: join_follower_node <node_num>
join_follower_node() {
    local node_num="$1"
    local node_ip="${SIMULATED_NETWORK}.$node_num"
    local data_dir="$HOME/node$node_num"

    echo "Joining node$node_num ($node_ip) to cluster..."

    # Remove old node directory
    rm -rf "$data_dir/"

    ./bin/spx admin join \
        --node "node$node_num" \
        --bind "$node_ip" \
        --cluster-bind "$node_ip" \
        --host "${NODE1_IP}:${CLUSTER_PORT}" \
        --token "$JOIN_TOKEN" \
        --data-dir "$data_dir/" \
        --config-dir "$data_dir/config/" \
        --region ap-southeast-2 \
        --az "ap-southeast-2a"

    echo "Node$node_num joined cluster"
}

# Verify Predastore cluster health
# Checks that Predastore is reachable on all node IPs
# Usage: verify_predastore_cluster [expected_nodes]
verify_predastore_cluster() {
    local expected_nodes="${1:-3}"
    local healthy=0

    echo "Verifying Predastore cluster health (expecting $expected_nodes nodes)..."

    for i in $(seq 1 "$expected_nodes"); do
        local node_ip="${SIMULATED_NETWORK}.$i"

        if curl -k -s "https://${node_ip}:${PREDASTORE_PORT}" > /dev/null 2>&1; then
            echo "  Node$i ($node_ip:${PREDASTORE_PORT}): reachable"
            healthy=$((healthy + 1))
        else
            echo "  Node$i ($node_ip:${PREDASTORE_PORT}): NOT reachable"
        fi
    done

    echo "  Healthy Predastore nodes: $healthy/$expected_nodes"

    if [ "$healthy" -ge "$expected_nodes" ]; then
        echo "  Predastore cluster is healthy"
        return 0
    else
        echo "  WARNING: Predastore cluster may not be fully formed"
        return 1
    fi
}

# Count instances per node for given instance IDs
# Usage: count_instances_per_node INSTANCE_ID [INSTANCE_ID...]
# Sets NODE1_COUNT, NODE2_COUNT, NODE3_COUNT
count_instances_per_node() {
    local ids=("$@")
    NODE1_COUNT=0; NODE2_COUNT=0; NODE3_COUNT=0
    for id in "${ids[@]}"; do
        local node
        node=$(get_instance_node "$id") || continue
        eval "NODE${node}_COUNT=\$((NODE${node}_COUNT + 1))"
    done
    for i in 1 2 3; do
        local node_ip="${SIMULATED_NETWORK}.$i"
        eval "local count=\$NODE${i}_COUNT"
        echo "    Node$i ($node_ip): $count instances"
    done
}

# Verify Placement field in DescribeInstances response
# Usage: verify_placement GROUP_NAME INSTANCE_ID [INSTANCE_ID...]
verify_placement() {
    local group_name="$1"; shift
    local ids=("$@")
    for id in "${ids[@]}"; do
        local placement_group
        placement_group=$($AWS_EC2 describe-instances --instance-ids "$id" \
            --query 'Reservations[0].Instances[0].Placement.GroupName' --output text)
        if [ "$placement_group" != "$group_name" ]; then
            echo "    FAIL: Instance $id Placement.GroupName='$placement_group', expected '$group_name'"
            return 1
        fi
        local az
        az=$($AWS_EC2 describe-instances --instance-ids "$id" \
            --query 'Reservations[0].Instances[0].Placement.AvailabilityZone' --output text)
        if [ -z "$az" ] || [ "$az" = "None" ]; then
            echo "    FAIL: Instance $id missing Placement.AvailabilityZone"
            return 1
        fi
    done
    echo "    All instances show Placement.GroupName=$group_name"
}
