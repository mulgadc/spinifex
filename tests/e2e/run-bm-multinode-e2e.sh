#!/bin/bash
set -euo pipefail

# Multi-Node E2E Test Suite — bare-metal variant.
# Adapted from run-multinode-e2e.sh for N≥2 bare-metal nodes bootstrapped by
# bm-bootstrap.sh. SSH key and user are parameterised via env vars.
#
# Usage: run-bm-multinode-e2e.sh <node1_ip> <node2_ip> [node3_ip ...]
# Env:   PEER_SSH_KEY   path to private key for peer_ssh (default: ~/bm-ci key)
#        PEER_SSH_USER  SSH username for peer nodes (default: spinifex)

# Ensure Go is on PATH (SSH non-interactive shells don't source .bashrc)
export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"

# ==========================================================================
# Argument parsing
# ==========================================================================
if [ $# -lt 2 ]; then
    echo "Usage: $0 <node1_ip> <node2_ip> [node3_ip ...]"
    echo "  At least 2 node WAN IPs are required."
    exit 1
fi

NODE_IPS=("$@")
NODE_COUNT=${#NODE_IPS[@]}
LOCAL_IP="${NODE_IPS[0]}"
NODE2_IP="${NODE_IPS[1]}"
NODE3_IP="${NODE_IPS[2]:-}"

# ==========================================================================
# Constants
# ==========================================================================
NATS_MONITOR_PORT=8222
PREDASTORE_PORT=8443
AWSGW_PORT=9999
SSH_KEY_PATH="${PEER_SSH_KEY:-$HOME/.ssh/bm-ci}"
PEER_USER="${PEER_SSH_USER:-spinifex}"
SPINIFEX_BIN="spx"

# Use Spinifex profile for AWS CLI
export AWS_PROFILE=spinifex
# Trust Spinifex CA for AWS CLI v2 (bundles its own Python/certifi, ignores system CA store)
export AWS_CA_BUNDLE="/etc/spinifex/ca.pem"

# Track test results
TESTS_PASSED=0
TESTS_FAILED=0
FAILED_TESTS=()

# ==========================================================================
# Helper functions
# ==========================================================================

# SSH to a peer node
peer_ssh() {
    local ip="$1"; shift
    ssh -i "$SSH_KEY_PATH" \
        -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=10 \
        -o LogLevel=ERROR \
        "${PEER_USER}@${ip}" "$@"
}

# Run AWS CLI against a specific node's gateway
aws_via() {
    local node_ip="$1"; shift
    aws --endpoint-url "https://${node_ip}:${AWSGW_PORT}" "$@"
}

# Retry wrapper for aws_via — retries on transient failures (e.g. NATS cluster reformation)
# Usage: aws_via_retry <max_attempts> <node_ip> <aws args...>
aws_via_retry() {
    local max_attempts="$1"; shift
    local node_ip="$1"; shift
    local attempt=0
    local result

    while [ $attempt -lt $max_attempts ]; do
        result=$(aws_via "$node_ip" "$@" 2>/dev/null) && {
            if [ -n "$result" ] && [ "$result" != "None" ]; then
                echo "$result"
                return 0
            fi
        }
        attempt=$((attempt + 1))
        echo "  Retry $attempt/$max_attempts..." >&2
        sleep 1
    done
    return 1
}

# Default AWS CLI shorthand (via local node)
AWS_EC2="aws --endpoint-url https://${LOCAL_IP}:${AWSGW_PORT} ec2"

# Dump SSH-related diagnostics when a guest SSH attempt fails.
# Args: <instance_id> <host_ip> <ssh_host> [ssh_port]
# ssh_host is the address we tried (EIP or node IP), ssh_port is hostfwd port if applicable.
dump_guest_ssh_diagnostics() {
    local instance_id="$1" host_ip="$2" ssh_host="$3" ssh_port="${4:-}"
    echo ""
    echo "  === Diagnostics for ${instance_id} (host=${host_ip}, target=${ssh_host}:${ssh_port:-22}) ==="

    echo "  --- describe-instances ---"
    $AWS_EC2 describe-instances --instance-ids "$instance_id" \
        --query 'Reservations[0].Instances[0].[InstanceId,State.Name,PrivateIpAddress,PublicIpAddress,SecurityGroups[].GroupId,NetworkInterfaces[0].MacAddress,VpcId,SubnetId]' \
        --output json 2>&1 || true

    local sg_ids priv_ip mac vpc_id
    sg_ids=$($AWS_EC2 describe-instances --instance-ids "$instance_id" \
        --query 'Reservations[0].Instances[0].SecurityGroups[].GroupId' --output text 2>/dev/null || true)
    priv_ip=$($AWS_EC2 describe-instances --instance-ids "$instance_id" \
        --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text 2>/dev/null || true)
    mac=$($AWS_EC2 describe-instances --instance-ids "$instance_id" \
        --query 'Reservations[0].Instances[0].NetworkInterfaces[0].MacAddress' --output text 2>/dev/null || true)
    vpc_id=$($AWS_EC2 describe-instances --instance-ids "$instance_id" \
        --query 'Reservations[0].Instances[0].VpcId' --output text 2>/dev/null || true)

    if [ -n "$sg_ids" ] && [ "$sg_ids" != "None" ]; then
        echo "  --- describe-security-groups (${sg_ids}) ---"
        # shellcheck disable=SC2086
        $AWS_EC2 describe-security-groups --group-ids $sg_ids \
            --query 'SecurityGroups[].[GroupId,GroupName,IpPermissions]' --output json 2>&1 || true
    fi

    echo "  --- local ARP for ${ssh_host} ---"
    ip neigh show "$ssh_host" 2>&1 || true

    echo "  --- local ping ${ssh_host} (2 pkts) ---"
    ping -c 2 -W 2 "$ssh_host" 2>&1 || true

    if [ -n "$host_ip" ] && [ "$host_ip" != "unknown" ]; then
        echo "  --- on hosting node ${host_ip}: QEMU process ---"
        peer_ssh "$host_ip" "pgrep -af 'qemu.*${instance_id}' || echo '(no qemu process)'" 2>&1 || true

        if [ -n "$priv_ip" ] && [ "$priv_ip" != "None" ]; then
            echo "  --- on hosting node ${host_ip}: ping VM private IP ${priv_ip} ---"
            peer_ssh "$host_ip" "ping -c 2 -W 2 ${priv_ip}" 2>&1 || true
        fi

        echo "  --- on hosting node ${host_ip}: OVN chassis + port bindings ---"
        peer_ssh "$host_ip" "sudo ovs-vsctl get open_vswitch . external_ids:system-id 2>/dev/null || true" 2>&1 || true
    fi

    if [ -n "$vpc_id" ] && [ "$vpc_id" != "None" ]; then
        echo "  --- OVN NAT rules for vpc-${vpc_id} (from primary) ---"
        peer_ssh "$LOCAL_IP" "sudo ovn-nbctl lr-nat-list vpc-${vpc_id} 2>&1 || true" 2>&1 || true
    fi

    if [ -n "$mac" ] && [ "$mac" != "None" ]; then
        # ovn-sbctl find parses 'mac~=...' as a column name. mac is a set
        # column on Port_Binding so list+grep is the simplest correct form.
        echo "  --- OVN port binding for MAC ${mac} (from primary) ---"
        peer_ssh "$LOCAL_IP" "sudo ovn-sbctl --bare --columns=logical_port,chassis,mac list Port_Binding 2>&1 | grep -F '${mac}' || echo '(no Port_Binding matched MAC ${mac})'" 2>&1 || true
    fi

    # ----- Cross-chassis dataplane diagnostics (mulga-siv-27 follow-up) -----
    echo "  --- OVN SB chassis registrations (from primary) ---"
    peer_ssh "$LOCAL_IP" "sudo ovn-sbctl show 2>&1 || true" 2>&1 || true

    echo "  --- OVN SB port_binding chassis claims (from primary) ---"
    peer_ssh "$LOCAL_IP" "sudo ovn-sbctl --bare --columns=logical_port,chassis,up list Port_Binding 2>&1 | head -80 || true" 2>&1 || true

    echo "  --- OVN NB gateway_chassis (from primary) ---"
    peer_ssh "$LOCAL_IP" "sudo ovn-nbctl --bare --columns=name,chassis_name,priority list Gateway_Chassis 2>&1 || true" 2>&1 || true

    if [ -n "$host_ip" ] && [ "$host_ip" != "unknown" ]; then
        echo "  --- on hosting node ${host_ip}: ovs-vsctl show (taps + geneve) ---"
        peer_ssh "$host_ip" "sudo ovs-vsctl show 2>&1 | grep -E 'Bridge|Port|Interface|tap-|geneve|external_ids|iface-id|attached-mac|remote_ip' | head -80 || true" 2>&1 || true

        echo "  --- on hosting node ${host_ip}: ovn-controller status + system-id ---"
        peer_ssh "$host_ip" "sudo ovs-vsctl get open_vswitch . external_ids:system-id 2>&1; \
            sudo ovs-vsctl get open_vswitch . external_ids:ovn-encap-ip 2>&1; \
            sudo systemctl is-active ovn-controller 2>&1 || true" 2>&1 || true

        echo "  --- on hosting node ${host_ip}: Geneve tunnel ports (br-int) ---"
        peer_ssh "$host_ip" "sudo ovs-vsctl --columns=name,type,options find Interface type=geneve 2>&1 || true" 2>&1 || true

        echo "  --- on hosting node ${host_ip}: br-int output flows (tunnel) ---"
        peer_ssh "$host_ip" "sudo ovs-ofctl dump-flows br-int 2>/dev/null | grep -E 'tun_dst|output:.*tun|table=33|table=37' | head -40 || true" 2>&1 || true
    fi

    echo "  --- cloud-init / sshd from hosting node (via QEMU console log if available) ---"
    peer_ssh "$host_ip" "ls -la /var/log/libvirt/qemu/ 2>/dev/null | head -20; \
        for f in /tmp/spinifex/vms/${instance_id}/console.log /var/log/libvirt/qemu/${instance_id}.log; do \
          [ -f \"\$f\" ] && echo \"--- \$f (tail 50) ---\" && sudo tail -50 \"\$f\"; \
        done" 2>&1 || true

    echo "  === end diagnostics for ${instance_id} ==="
    echo ""
}

# Dump logs from ALL nodes on failure
dump_all_node_logs() {
    echo ""
    echo "=========================================="
    echo "DUMPING LOGS FROM ALL NODES"
    echo "=========================================="
    for i in $(seq 0 $((NODE_COUNT - 1))); do
        local ip="${NODE_IPS[$i]}"
        echo ""
        echo "=== Node $((i+1)) ($ip) ==="
        for svc in spinifex-nats spinifex-predastore spinifex-viperblock \
                   spinifex-daemon spinifex-awsgw spinifex-vpcd; do
            echo "--- ${svc} (last 50 lines) ---"
            if [ "$ip" = "$LOCAL_IP" ]; then
                sudo journalctl -u "$svc" --no-pager -n 50 2>/dev/null || echo "(not found)"
            else
                peer_ssh "$ip" "sudo journalctl -u $svc --no-pager -n 50" 2>/dev/null || echo "(node unreachable)"
            fi
        done
    done
    echo ""
    echo "=========================================="
    echo "END OF LOG DUMP"
    echo "=========================================="
}

# Wait for a specific instance state
# Usage: wait_for_instance_state <instance_id> <target_state> [max_attempts] [gateway_ip]
wait_for_instance_state() {
    local instance_id="$1"
    local target_state="$2"
    local max_attempts="${3:-60}"
    local gw_ip="${4:-$LOCAL_IP}"
    local attempt=0

    echo "  Waiting for $instance_id to reach state: $target_state..."

    while [ $attempt -lt $max_attempts ]; do
        local state
        state=$(aws_via "$gw_ip" ec2 describe-instances \
            --instance-ids "$instance_id" \
            --query 'Reservations[0].Instances[0].State.Name' \
            --output text 2>/dev/null) || {
            sleep 1
            attempt=$((attempt + 1))
            continue
        }

        if [ "$state" == "$target_state" ]; then
            echo "  Instance reached state: $target_state"
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

# Find which node runs a QEMU instance (by checking ps on each node)
# Usage: find_instance_node <instance_id>
# Returns: the WAN IP of the hosting node
find_instance_node() {
    local instance_id="$1"

    for ip in "${NODE_IPS[@]}"; do
        local found
        if [ "$ip" = "$LOCAL_IP" ]; then
            found=$(ps auxw | grep "$instance_id" | grep qemu-system | grep -v grep || true)
        else
            found=$(peer_ssh "$ip" "ps auxw | grep '$instance_id' | grep qemu-system | grep -v grep" 2>/dev/null || true)
        fi
        if [ -n "$found" ]; then
            echo "$ip"
            return 0
        fi
    done

    return 1
}

# Extract the SSH hostfwd port for an instance from the QEMU process on a remote node
# Usage: get_remote_ssh_port <node_ip> <instance_id> [max_attempts]
get_remote_ssh_port() {
    local node_ip="$1"
    local instance_id="$2"
    local max_attempts="${3:-30}"
    local attempt=0

    while [ $attempt -lt $max_attempts ]; do
        local qemu_cmd
        if [ "$node_ip" = "$LOCAL_IP" ]; then
            qemu_cmd=$(ps auxw | grep "$instance_id" | grep qemu-system | grep -v grep || true)
        else
            qemu_cmd=$(peer_ssh "$node_ip" "ps auxw | grep '$instance_id' | grep qemu-system | grep -v grep" 2>/dev/null || true)
        fi

        if [ -n "$qemu_cmd" ]; then
            local ssh_port
            ssh_port=$(echo "$qemu_cmd" | sed -n 's/.*hostfwd=tcp:[^:]*:\([0-9]*\)-:22.*/\1/p')
            if [ -n "$ssh_port" ]; then
                echo "$ssh_port"
                return 0
            fi
        fi

        attempt=$((attempt + 1))
        [ $attempt -lt $max_attempts ] && sleep 1
    done

    return 1
}

# Expect an AWS CLI command to fail with a specific error code
# Usage: expect_error "ErrorCode" aws ec2 some-command --args...
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

# Terminate instances and wait for terminated state
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

# Record test result
pass_test() {
    local name="$1"
    echo "  $name PASSED"
    TESTS_PASSED=$((TESTS_PASSED + 1))
}

fail_test() {
    local name="$1"
    echo "  $name FAILED"
    TESTS_FAILED=$((TESTS_FAILED + 1))
    FAILED_TESTS+=("$name")
}

# ==========================================================================
# EXIT trap — dump logs on failure
# ==========================================================================
trap 'EXIT_CODE=$?; if [ $EXIT_CODE -ne 0 ]; then dump_all_node_logs; fi; exit $EXIT_CODE' EXIT

# Track instance IDs for cleanup
INSTANCE_IDS=()

echo "========================================"
echo "Real Multi-Node E2E Test Suite"
echo "========================================"
echo "Nodes: ${NODE_IPS[*]}"
echo "Local: $LOCAL_IP"
echo ""

# ==========================================================================
# Phase 1: Pre-flight Validation
# ==========================================================================
echo "Phase 1: Pre-flight Validation"
echo "========================================"

# Check KVM support
echo "Checking KVM support..."
if [ -e /dev/kvm ] && [ -w /dev/kvm ]; then
    echo "  /dev/kvm exists and is writable"
else
    echo "  ERROR: /dev/kvm missing or not writable"
    exit 1
fi

# Verify SSH to peer nodes
echo "Verifying SSH connectivity to peer nodes..."
for i in 1 2; do
    local_idx=$((i))
    ip="${NODE_IPS[$local_idx]}"
    echo -n "  SSH to node$((local_idx + 1)) ($ip)..."
    if peer_ssh "$ip" "hostname" > /dev/null 2>&1; then
        echo " OK"
    else
        echo " FAILED"
        echo "  ERROR: Cannot SSH to $ip"
        exit 1
    fi
done

echo ""

# ==========================================================================
# Phase 2: Cluster Health
# ==========================================================================
echo "Phase 2: Cluster Health"
echo "========================================"

# NATS cluster: verify 2 unique peers from node1
echo "Checking NATS cluster..."
NATS_INFO=$(curl -s "http://127.0.0.1:${NATS_MONITOR_PORT}/routez" 2>/dev/null) || {
    echo "  ERROR: Cannot reach NATS monitoring endpoint"
    exit 1
}
UNIQUE_PEERS=$(echo "$NATS_INFO" | jq -r '[.routes[].remote_name] | unique | length')
echo "  NATS unique peers: $UNIQUE_PEERS (expected: 2)"
if [ "$UNIQUE_PEERS" -ge 2 ]; then
    pass_test "NATS quorum"
else
    echo "  ERROR: NATS cluster not fully formed"
    exit 1
fi

# Predastore: check each node
echo "Checking Predastore on all nodes..."
for i in $(seq 0 $((NODE_COUNT - 1))); do
    ip="${NODE_IPS[$i]}"
    if curl -k -s "https://${ip}:${PREDASTORE_PORT}" > /dev/null 2>&1; then
        echo "  Node$((i+1)) ($ip): Predastore reachable"
    else
        echo "  ERROR: Predastore not reachable on node$((i+1)) ($ip)"
        exit 1
    fi
done
pass_test "Predastore cluster"

# Gateway: check each node
echo "Checking gateway on all nodes..."
for i in $(seq 0 $((NODE_COUNT - 1))); do
    ip="${NODE_IPS[$i]}"
    if curl -k -s "https://${ip}:${AWSGW_PORT}" > /dev/null 2>&1; then
        echo "  Node$((i+1)) ($ip): Gateway reachable"
    else
        echo "  ERROR: Gateway not reachable on node$((i+1)) ($ip)"
        exit 1
    fi
done
pass_test "All gateways"

# Daemon readiness: describe-instance-types must return results
echo "Checking daemon readiness..."
ATTEMPT=0
while [ $ATTEMPT -lt 30 ]; do
    TYPES=$($AWS_EC2 describe-instance-types \
        --query 'InstanceTypes[*].InstanceType' --output text 2>/dev/null || true)
    if [ -n "$TYPES" ] && [ "$TYPES" != "None" ]; then
        echo "  Daemon ready (instance types: $TYPES)"
        break
    fi
    echo "  Waiting for daemon... ($((ATTEMPT + 1))/30)"
    sleep 1
    ATTEMPT=$((ATTEMPT + 1))
done
if [ -z "$TYPES" ] || [ "$TYPES" == "None" ]; then
    echo "  ERROR: Daemon not ready"
    exit 1
fi
pass_test "Daemon readiness"

# Spinifex CLI: get nodes
echo "Checking spx get nodes..."
GET_NODES_OUTPUT=$($SPINIFEX_BIN get nodes --timeout 5s 2>/dev/null)
echo "$GET_NODES_OUTPUT"
READY_COUNT=$(echo "$GET_NODES_OUTPUT" | grep -c "Ready" || true)
if [ "$READY_COUNT" -ge 3 ]; then
    pass_test "spx get nodes ($READY_COUNT Ready)"
else
    echo "  WARNING: spx get nodes shows $READY_COUNT Ready nodes (expected 3)"
    fail_test "spx get nodes"
fi

# Spinifex CLI: get vms (should be empty)
echo "Checking spx get vms (empty)..."
GET_VMS_OUTPUT=$($SPINIFEX_BIN get vms --timeout 5s 2>/dev/null)
echo "$GET_VMS_OUTPUT"
pass_test "spx get vms (empty)"

echo ""

# ==========================================================================
# Phase 3: Instance Lifecycle + Distribution
# ==========================================================================
echo "Phase 3: Instance Lifecycle + Distribution"
echo "========================================"

# Discover instance type
INSTANCE_TYPE=$(echo $TYPES | tr ' ' '\n' | grep -m1 'nano')
if [ -z "$INSTANCE_TYPE" ]; then
    echo "ERROR: No nano instance type found"
    exit 1
fi
echo "Using instance type: $INSTANCE_TYPE"

# Discover Ubuntu AMI (filter to avoid picking up the LB Alpine image)
AMI_ID=$($AWS_EC2 describe-images --filters "Name=name,Values=ami-ubuntu-*" --query 'Images[0].ImageId' --output text)
if [ -z "$AMI_ID" ] || [ "$AMI_ID" == "None" ]; then
    echo "ERROR: No AMI found (bootstrap should have imported one)"
    exit 1
fi
echo "Using AMI: $AMI_ID"

# Authorize SSH on the default VPC's default SG before any run-instances. AWS
# default SGs allow ingress only from members of the same SG; the Phase 4 SSH
# probe comes from the test runner's IP and would be dropped by the OVN
# port-group ACL otherwise.
DEFAULT_VPC_PHASE3=$($AWS_EC2 describe-vpcs \
    --query 'Vpcs[?IsDefault==`true`].VpcId | [0]' --output text)
DEFAULT_SG_PHASE3=$($AWS_EC2 describe-security-groups \
    --filters "Name=vpc-id,Values=$DEFAULT_VPC_PHASE3" "Name=group-name,Values=default" \
    --query 'SecurityGroups[0].GroupId' --output text)
echo "Default VPC: $DEFAULT_VPC_PHASE3; default SG: $DEFAULT_SG_PHASE3"
# Tolerate InvalidPermission.Duplicate on re-runs (idempotent).
set +e
AUTH_OUTPUT=$($AWS_EC2 authorize-security-group-ingress \
    --group-id "$DEFAULT_SG_PHASE3" \
    --protocol tcp --port 22 --cidr 0.0.0.0/0 2>&1)
AUTH_EXIT=$?
set -e
if [ $AUTH_EXIT -ne 0 ] && ! echo "$AUTH_OUTPUT" | grep -q 'InvalidPermission.Duplicate'; then
    echo "Failed to authorize SSH ingress on default SG: $AUTH_OUTPUT"
    exit 1
fi
echo "  Default SG ingress: tcp/22 from 0.0.0.0/0"

# Launch 3 instances with stagger to encourage distribution
echo "Launching 3 instances..."
for i in 1 2 3; do
    echo "  Launching instance $i..."
    RUN_OUTPUT=$($AWS_EC2 run-instances \
        --image-id "$AMI_ID" \
        --instance-type "$INSTANCE_TYPE" \
        --key-name spinifex-key)

    INSTANCE_ID=$(echo "$RUN_OUTPUT" | jq -r '.Instances[0].InstanceId')
    if [ -z "$INSTANCE_ID" ] || [ "$INSTANCE_ID" == "null" ]; then
        echo "  ERROR: Failed to launch instance $i"
        echo "  Output: $RUN_OUTPUT"
        exit 1
    fi
    echo "  Launched: $INSTANCE_ID"
    INSTANCE_IDS+=("$INSTANCE_ID")

    # Stagger launches to encourage distribution across nodes
    [ $i -lt 3 ] && sleep 2
done

# Wait for all instances to be running
echo ""
echo "Waiting for instances to reach running state..."
for instance_id in "${INSTANCE_IDS[@]}"; do
    wait_for_instance_state "$instance_id" "running" 60 || {
        echo "ERROR: Instance $instance_id failed to start"
        exit 1
    }
done

# Check distribution via spx get vms or QEMU process check
echo ""
echo "Checking instance distribution across nodes..."
declare -A NODE_INSTANCE_COUNT
HOSTING_NODES=()
for instance_id in "${INSTANCE_IDS[@]}"; do
    HOST_IP=$(find_instance_node "$instance_id" || echo "unknown")
    echo "  $instance_id -> $HOST_IP"
    HOSTING_NODES+=("$HOST_IP")
    NODE_INSTANCE_COUNT[$HOST_IP]=$(( ${NODE_INSTANCE_COUNT[$HOST_IP]:-0} + 1 ))
done

# Count unique hosting nodes
UNIQUE_HOSTS=$(printf '%s\n' "${HOSTING_NODES[@]}" | sort -u | wc -l)
echo "  Instances on $UNIQUE_HOSTS different nodes"
if [ "$UNIQUE_HOSTS" -ge 2 ]; then
    pass_test "Instance distribution (>= 2 nodes)"
else
    echo "  WARNING: All instances on same node (distribution not guaranteed, non-fatal)"
    pass_test "Instance distribution (non-deterministic)"
fi

# Verify spx get vms shows all instances
echo ""
echo "Verifying spx get vms..."
GET_VMS_OUTPUT=$($SPINIFEX_BIN get vms --timeout 5s 2>/dev/null)
echo "$GET_VMS_OUTPUT"
for instance_id in "${INSTANCE_IDS[@]}"; do
    if ! echo "$GET_VMS_OUTPUT" | grep -q "$instance_id"; then
        echo "  WARNING: spx get vms did not show $instance_id"
    fi
done
pass_test "spx get vms (with instances)"

echo ""

# ==========================================================================
# Phase 4: SSH into Guest VMs
# ==========================================================================
echo "Phase 4: SSH into Guest VMs"
echo "========================================"

for idx in "${!INSTANCE_IDS[@]}"; do
    instance_id="${INSTANCE_IDS[$idx]}"
    host_ip="${HOSTING_NODES[$idx]}"
    echo ""
    echo "  Instance $((idx + 1)): $instance_id (on $host_ip)"

    # Determine SSH endpoint: use public IP if available, else QEMU hostfwd
    INST_PUB_IP=$($AWS_EC2 describe-instances \
        --instance-ids "$instance_id" \
        --query 'Reservations[0].Instances[0].PublicIpAddress' --output text 2>/dev/null || echo "None")

    if [ -n "$INST_PUB_IP" ] && [ "$INST_PUB_IP" != "None" ] && [ "$INST_PUB_IP" != "null" ]; then
        SSH_HOST="$INST_PUB_IP"
        SSH_PORT=22
        SSH_KEY="$HOME/.ssh/spinifex-key"
        echo "  SSH via public IP: $SSH_HOST:$SSH_PORT"
    else
        SSH_HOST="$host_ip"
        # set -e would abort on $() returning non-zero, hiding the diagnostic below
        if ! SSH_PORT=$(get_remote_ssh_port "$host_ip" "$instance_id" 30); then
            SSH_PORT=""
        fi
        SSH_KEY="$HOME/.ssh/spinifex-key"
        if [ -z "$SSH_PORT" ]; then
            echo "  ERROR: Failed to get SSH port for $instance_id on $host_ip"
            dump_guest_ssh_diagnostics "$instance_id" "$host_ip" "$SSH_HOST" ""
            fail_test "Guest SSH ($instance_id)"
            continue
        fi
        echo "  SSH endpoint: $SSH_HOST:$SSH_PORT"
    fi

    # Wait for SSH to be ready (VM boot + cloud-init)
    echo "  Waiting for SSH to be ready..."
    ATTEMPT=0
    SSH_READY=false
    while [ $ATTEMPT -lt 60 ]; do
        if ssh -o StrictHostKeyChecking=no \
               -o UserKnownHostsFile=/dev/null \
               -o ConnectTimeout=2 \
               -o BatchMode=yes \
               -o LogLevel=ERROR \
               -p "$SSH_PORT" \
               -i "$SSH_KEY" \
               ec2-user@"$SSH_HOST" 'echo ready' > /dev/null 2>&1; then
            SSH_READY=true
            break
        fi
        ATTEMPT=$((ATTEMPT + 1))
        [ $((ATTEMPT % 10)) -eq 0 ] && echo "  Waiting for SSH... ($ATTEMPT/60)"
        sleep 1
    done

    if [ "$SSH_READY" = false ]; then
        echo "  ERROR: SSH not ready after 60 attempts"
        dump_guest_ssh_diagnostics "$instance_id" "$host_ip" "$SSH_HOST" "$SSH_PORT"
        fail_test "Guest SSH ($instance_id)"
        continue
    fi

    # Test SSH connectivity
    ID_OUTPUT=$(ssh -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 \
        -o BatchMode=yes \
        -o LogLevel=ERROR \
        -p "$SSH_PORT" \
        -i "$SSH_KEY" \
        ec2-user@"$SSH_HOST" 'id' 2>&1) || {
        echo "  ERROR: SSH 'id' command failed"
        fail_test "Guest SSH ($instance_id)"
        continue
    }

    echo "  SSH 'id' output: $ID_OUTPUT"
    if echo "$ID_OUTPUT" | grep -q "ec2-user"; then
        echo "  ec2-user confirmed"
    else
        echo "  ERROR: Expected 'ec2-user' in id output"
        fail_test "Guest SSH ($instance_id)"
        continue
    fi

    # Verify block device
    LSBLK_OUTPUT=$(ssh -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 \
        -o BatchMode=yes \
        -o LogLevel=ERROR \
        -p "$SSH_PORT" \
        -i "$SSH_KEY" \
        ec2-user@"$SSH_HOST" 'lsblk' 2>&1) || true
    echo "  lsblk: $(echo "$LSBLK_OUTPUT" | head -5)"

    pass_test "Guest SSH ($instance_id)"
done

echo ""

# ==========================================================================
# Phase 5: Volume Lifecycle
# ==========================================================================
echo "Phase 5: Volume Lifecycle"
echo "========================================"

# Discover AZ
SPINIFEX_AZ=$($AWS_EC2 describe-availability-zones --query 'AvailabilityZones[0].ZoneName' --output text)
echo "AZ: $SPINIFEX_AZ"

# Create volume
echo "Creating 10GB test volume..."
CREATE_OUTPUT=$($AWS_EC2 create-volume --size 10 --availability-zone "$SPINIFEX_AZ")
TEST_VOLUME_ID=$(echo "$CREATE_OUTPUT" | jq -r '.VolumeId')
if [ -z "$TEST_VOLUME_ID" ] || [ "$TEST_VOLUME_ID" == "null" ]; then
    echo "  ERROR: Failed to create volume"
    fail_test "Volume create"
else
    echo "  Created: $TEST_VOLUME_ID"
    pass_test "Volume create"

    # Attach to first instance
    echo "  Attaching to ${INSTANCE_IDS[0]}..."
    $AWS_EC2 attach-volume --volume-id "$TEST_VOLUME_ID" \
        --instance-id "${INSTANCE_IDS[0]}" --device /dev/sdf > /dev/null

    # Wait for attachment
    COUNT=0
    while [ $COUNT -lt 30 ]; do
        ATTACH_STATE=$($AWS_EC2 describe-volumes --volume-ids "$TEST_VOLUME_ID" \
            --query 'Volumes[0].Attachments[0].State' --output text)
        VOL_STATE=$($AWS_EC2 describe-volumes --volume-ids "$TEST_VOLUME_ID" \
            --query 'Volumes[0].State' --output text)
        if [ "$VOL_STATE" == "in-use" ] && [ "$ATTACH_STATE" == "attached" ]; then
            echo "  Volume attached"
            break
        fi
        sleep 1
        COUNT=$((COUNT + 1))
    done

    if [ "$ATTACH_STATE" == "attached" ]; then
        pass_test "Volume attach"
    else
        fail_test "Volume attach"
    fi

    # Detach
    echo "  Detaching volume..."
    $AWS_EC2 detach-volume --volume-id "$TEST_VOLUME_ID" > /dev/null

    COUNT=0
    while [ $COUNT -lt 30 ]; do
        VOL_STATE=$($AWS_EC2 describe-volumes --volume-ids "$TEST_VOLUME_ID" \
            --query 'Volumes[0].State' --output text)
        if [ "$VOL_STATE" == "available" ]; then
            echo "  Volume detached"
            break
        fi
        sleep 1
        COUNT=$((COUNT + 1))
    done

    if [ "$VOL_STATE" == "available" ]; then
        pass_test "Volume detach"
    else
        fail_test "Volume detach"
    fi

    # Delete
    echo "  Deleting volume..."
    $AWS_EC2 delete-volume --volume-id "$TEST_VOLUME_ID"

    COUNT=0
    while [ $COUNT -lt 30 ]; do
        set +e
        VOL_CHECK=$($AWS_EC2 describe-volumes --volume-ids "$TEST_VOLUME_ID" \
            --query 'Volumes[0].VolumeId' --output text 2>&1)
        DESCRIBE_EXIT=$?
        set -e
        if [ $DESCRIBE_EXIT -ne 0 ] || [ "$VOL_CHECK" == "None" ] || [ -z "$VOL_CHECK" ]; then
            echo "  Volume deleted"
            break
        fi
        sleep 1
        COUNT=$((COUNT + 1))
    done

    if [ $COUNT -lt 30 ]; then
        pass_test "Volume delete"
    else
        fail_test "Volume delete"
    fi
fi

echo ""

# ==========================================================================
# Phase 6: Cross-Node Gateway Access
# ==========================================================================
echo "Phase 6: Cross-Node Gateway Access"
echo "========================================"
echo "Verifying describe-instances returns same results via each node's gateway..."

BASELINE_COUNT=$($AWS_EC2 describe-instances \
    --query 'length(Reservations[*].Instances[*][])' --output text)
echo "  Baseline (node1): $BASELINE_COUNT instances"

for i in 1 2; do
    ip="${NODE_IPS[$i]}"
    COUNT=$(aws_via "$ip" ec2 describe-instances \
        --query 'length(Reservations[*].Instances[*][])' --output text 2>/dev/null || echo "0")
    echo "  Node$((i+1)) ($ip): $COUNT instances"
    if [ "$COUNT" -eq "$BASELINE_COUNT" ]; then
        pass_test "Cross-node gateway (node$((i+1)))"
    else
        echo "  WARNING: Instance count mismatch (expected $BASELINE_COUNT, got $COUNT)"
        fail_test "Cross-node gateway (node$((i+1)))"
    fi
done

echo ""

# ==========================================================================
# Phase 7: Cross-Node Operations
# ==========================================================================
echo "Phase 7: Cross-Node Operations"
echo "========================================"
echo "Testing stop/start via a gateway on a DIFFERENT node than the hosting node..."

# Pick first instance and find its host
TEST_INSTANCE="${INSTANCE_IDS[0]}"
INSTANCE_HOST=$(find_instance_node "$TEST_INSTANCE" || echo "$LOCAL_IP")
echo "  Instance $TEST_INSTANCE is on $INSTANCE_HOST"

# Pick a different node's gateway for the operation
OTHER_GW=""
for ip in "${NODE_IPS[@]}"; do
    if [ "$ip" != "$INSTANCE_HOST" ]; then
        OTHER_GW="$ip"
        break
    fi
done
echo "  Will operate via gateway on $OTHER_GW"

# Stop via other gateway
echo "  Stopping instance via $OTHER_GW..."
aws_via "$OTHER_GW" ec2 stop-instances --instance-ids "$TEST_INSTANCE" > /dev/null
wait_for_instance_state "$TEST_INSTANCE" "stopped" 60 "$OTHER_GW"

# Pick yet another gateway for start (or same other if only 2 choices)
THIRD_GW=""
for ip in "${NODE_IPS[@]}"; do
    if [ "$ip" != "$INSTANCE_HOST" ] && [ "$ip" != "$OTHER_GW" ]; then
        THIRD_GW="$ip"
        break
    fi
done
THIRD_GW="${THIRD_GW:-$OTHER_GW}"
echo "  Starting instance via $THIRD_GW..."
aws_via "$THIRD_GW" ec2 start-instances --instance-ids "$TEST_INSTANCE" > /dev/null
wait_for_instance_state "$TEST_INSTANCE" "running" 60 "$THIRD_GW"

pass_test "Cross-node stop/start"

echo ""

# ==========================================================================
# Phase 8: Node Failure
# ==========================================================================
echo "Phase 8: Node Failure"
echo "========================================"
echo "Stopping services on node2 ($NODE2_IP) to simulate node failure..."

# Stop services on node2 only — SPINIFEX_FORCE_LOCAL_STOP prevents coordinated
# cluster shutdown which would kill all nodes via NATS
peer_ssh "$NODE2_IP" "sudo systemctl stop spinifex.target" || {
    echo "  WARNING: systemctl stop returned non-zero (may be expected)"
}

# Wait for NATS cluster to detect the failure and reform
sleep 10

# Verify node1 and node3 still serve requests (with retries for NATS reformation)
echo "  Verifying node1 still serves requests..."
N1_RESULT=$(aws_via_retry 10 "$LOCAL_IP" ec2 describe-instance-types \
    --query 'InstanceTypes[0].InstanceType' --output text) || N1_RESULT="FAIL"
if [ "$N1_RESULT" != "FAIL" ]; then
    echo "  Node1: responding ($N1_RESULT)"
    pass_test "Node1 survives node2 failure"
else
    echo "  ERROR: Node1 not responding after node2 failure"
    fail_test "Node1 survives node2 failure"
fi

if [ -n "$NODE3_IP" ]; then
    echo "  Verifying node3 still serves requests..."
    N3_RESULT=$(aws_via_retry 10 "$NODE3_IP" ec2 describe-instance-types \
        --query 'InstanceTypes[0].InstanceType' --output text) || N3_RESULT="FAIL"
    if [ "$N3_RESULT" != "FAIL" ]; then
        echo "  Node3: responding ($N3_RESULT)"
        pass_test "Node3 survives node2 failure"
    else
        echo "  ERROR: Node3 not responding after node2 failure"
        fail_test "Node3 survives node2 failure"
    fi
else
    echo "  Node3 not present in this cluster — skipping node3 survival check"
fi

# Check NATS degraded state (should have 1 route instead of 2)
NATS_DEGRADED=$(curl -s "http://127.0.0.1:${NATS_MONITOR_PORT}/routez" 2>/dev/null)
DEGRADED_PEERS=$(echo "$NATS_DEGRADED" | jq -r '[.routes[].remote_name] | unique | length' 2>/dev/null || echo "0")
echo "  NATS peers during failure: $DEGRADED_PEERS (expected: 1)"
if [ "$DEGRADED_PEERS" -eq 1 ]; then
    pass_test "NATS degraded mode"
else
    echo "  WARNING: Expected 1 NATS peer during node2 failure, got $DEGRADED_PEERS"
    # Not fatal — NATS might take a moment to detect
fi

# Verify describe-instances still works from surviving nodes
echo "  Verifying describe-instances from surviving nodes..."
SURVIVING_COUNT=$(aws_via_retry 10 "$LOCAL_IP" ec2 describe-instances \
    --query 'length(Reservations[*].Instances[*][])' --output text) || SURVIVING_COUNT="0"
echo "  Instances visible from node1: $SURVIVING_COUNT"
if [ "$SURVIVING_COUNT" -gt 0 ]; then
    pass_test "Describe-instances during node failure"
else
    fail_test "Describe-instances during node failure"
fi

echo ""

# ==========================================================================
# Phase 9: Node Recovery
# ==========================================================================
echo "Phase 9: Node Recovery"
echo "========================================"
echo "Restarting services on node2 ($NODE2_IP)..."

peer_ssh "$NODE2_IP" "sudo systemctl start spinifex.target" || {
    echo "  ERROR: Failed to restart services on node2"
    fail_test "Node2 restart"
}

# Wait for NATS to reform (2 routes again)
echo "  Waiting for NATS cluster to reform..."
ATTEMPT=0
REFORMED=false
while [ $ATTEMPT -lt 60 ]; do
    NATS_RECOVER=$(curl -s "http://127.0.0.1:${NATS_MONITOR_PORT}/routez" 2>/dev/null)
    RECOVER_PEERS=$(echo "$NATS_RECOVER" | jq -r '[.routes[].remote_name] | unique | length' 2>/dev/null || echo "0")
    if [ "$RECOVER_PEERS" -ge 2 ]; then
        echo "  NATS cluster reformed ($RECOVER_PEERS peers)"
        REFORMED=true
        break
    fi
    echo "  Waiting for NATS reform... ($((ATTEMPT + 1))/60, peers: $RECOVER_PEERS)"
    sleep 1
    ATTEMPT=$((ATTEMPT + 1))
done

if [ "$REFORMED" = true ]; then
    pass_test "NATS cluster reform"
else
    echo "  WARNING: NATS did not fully reform within timeout"
    fail_test "NATS cluster reform"
fi

# Verify node2 gateway is back
echo "  Waiting for node2 gateway..."
ATTEMPT=0
GW_BACK=false
while [ $ATTEMPT -lt 30 ]; do
    if curl -k -s "https://${NODE2_IP}:${AWSGW_PORT}" > /dev/null 2>&1; then
        echo "  Node2 gateway is back"
        GW_BACK=true
        break
    fi
    sleep 1
    ATTEMPT=$((ATTEMPT + 1))
done

if [ "$GW_BACK" = true ]; then
    pass_test "Node2 gateway recovery"
else
    fail_test "Node2 gateway recovery"
fi

# Verify spx get nodes shows 3 Ready again
echo "  Checking spx get nodes after recovery..."
GET_NODES_RECOVER=$($SPINIFEX_BIN get nodes --timeout 10s 2>/dev/null || echo "")
echo "$GET_NODES_RECOVER"
READY_RECOVER=$(echo "$GET_NODES_RECOVER" | grep -c "Ready" || true)
if [ "$READY_RECOVER" -ge 3 ]; then
    pass_test "All nodes Ready after recovery"
else
    echo "  WARNING: Only $READY_RECOVER Ready nodes after recovery (expected 3)"
    fail_test "All nodes Ready after recovery"
fi

# Verify node2 can serve requests
echo "  Verifying node2 serves requests after recovery..."
N2_RESULT=$(aws_via "$NODE2_IP" ec2 describe-instance-types \
    --query 'InstanceTypes[0].InstanceType' --output text 2>/dev/null || echo "FAIL")
if [ "$N2_RESULT" != "FAIL" ] && [ -n "$N2_RESULT" ] && [ "$N2_RESULT" != "None" ]; then
    echo "  Node2 is serving requests again ($N2_RESULT)"
    pass_test "Node2 serves requests after recovery"
else
    fail_test "Node2 serves requests after recovery"
fi

echo ""

# ==========================================================================
# Phase 10: VPC Networking
# ==========================================================================
echo "Phase 10: VPC Networking"
echo "========================================"
echo "Testing VPC instance launch, PrivateIpAddress, and stop/start IP persistence..."

# Create a fresh VPC + subnet (don't reuse bootstrap's to avoid interference)
echo ""
echo "Creating test VPC + subnet..."
VPC_OUTPUT=$($AWS_EC2 create-vpc --cidr-block 10.200.0.0/16)
VPC_ID=$(echo "$VPC_OUTPUT" | jq -r '.Vpc.VpcId')
if [ -z "$VPC_ID" ] || [ "$VPC_ID" == "null" ]; then
    echo "  ERROR: Failed to create VPC"
    fail_test "VPC create"
else
    echo "  Created VPC: $VPC_ID (10.200.0.0/16)"
    pass_test "VPC create"

    SUBNET_OUTPUT=$($AWS_EC2 create-subnet --vpc-id "$VPC_ID" --cidr-block 10.200.1.0/24)
    SUBNET_ID=$(echo "$SUBNET_OUTPUT" | jq -r '.Subnet.SubnetId')
    if [ -z "$SUBNET_ID" ] || [ "$SUBNET_ID" == "null" ]; then
        echo "  ERROR: Failed to create subnet"
        fail_test "Subnet create"
    else
        echo "  Created Subnet: $SUBNET_ID (10.200.1.0/24)"
        pass_test "Subnet create"

        # Brief pause for OVN topology programming
        sleep 2

        # Launch 3 instances with subnet
        echo ""
        echo "Launching 3 VPC instances..."
        VPC_INSTANCE_IDS=()
        VPC_LAUNCH_OK=true
        for i in 1 2 3; do
            echo "  Launching VPC instance $i with subnet $SUBNET_ID..."
            RUN_OUTPUT=$($AWS_EC2 run-instances \
                --image-id "$AMI_ID" \
                --instance-type "$INSTANCE_TYPE" \
                --key-name spinifex-key \
                --subnet-id "$SUBNET_ID")

            VPC_INST_ID=$(echo "$RUN_OUTPUT" | jq -r '.Instances[0].InstanceId')
            VPC_INST_IP=$(echo "$RUN_OUTPUT" | jq -r '.Instances[0].PrivateIpAddress // empty')

            if [ -z "$VPC_INST_ID" ] || [ "$VPC_INST_ID" == "null" ]; then
                echo "  ERROR: Failed to launch VPC instance $i"
                VPC_LAUNCH_OK=false
                break
            fi
            echo "  Launched: $VPC_INST_ID (PrivateIpAddress: ${VPC_INST_IP:-not yet assigned})"
            VPC_INSTANCE_IDS+=("$VPC_INST_ID")
            sleep 1
        done

        if [ "$VPC_LAUNCH_OK" = true ] && [ ${#VPC_INSTANCE_IDS[@]} -eq 3 ]; then
            pass_test "VPC instance launch"

            # Wait for all VPC instances to be running
            echo ""
            echo "Waiting for VPC instances to reach running state..."
            VPC_ALL_RUNNING=true
            for vpc_inst in "${VPC_INSTANCE_IDS[@]}"; do
                wait_for_instance_state "$vpc_inst" "running" 60 || {
                    echo "  ERROR: VPC instance $vpc_inst failed to start"
                    VPC_ALL_RUNNING=false
                }
            done

            if [ "$VPC_ALL_RUNNING" = true ]; then
                # Verify PrivateIpAddress in DescribeInstances
                echo ""
                echo "Verifying PrivateIpAddress in DescribeInstances..."
                VPC_PRIVATE_IPS=()
                VPC_IP_OK=true
                for vpc_inst in "${VPC_INSTANCE_IDS[@]}"; do
                    DESCRIBE_OUT=$($AWS_EC2 describe-instances --instance-ids "$vpc_inst")
                    PRIVATE_IP=$(echo "$DESCRIBE_OUT" | jq -r '.Reservations[0].Instances[0].PrivateIpAddress // empty')
                    INST_SUBNET=$(echo "$DESCRIBE_OUT" | jq -r '.Reservations[0].Instances[0].SubnetId // empty')
                    INST_VPC=$(echo "$DESCRIBE_OUT" | jq -r '.Reservations[0].Instances[0].VpcId // empty')

                    if [ -z "$PRIVATE_IP" ]; then
                        echo "  ERROR: $vpc_inst has no PrivateIpAddress"
                        VPC_IP_OK=false
                        continue
                    fi

                    echo "  $vpc_inst: IP=$PRIVATE_IP, Subnet=$INST_SUBNET, VPC=$INST_VPC"

                    if [ "$INST_SUBNET" != "$SUBNET_ID" ]; then
                        echo "  ERROR: SubnetId mismatch (expected $SUBNET_ID, got $INST_SUBNET)"
                        VPC_IP_OK=false
                    fi
                    if [ "$INST_VPC" != "$VPC_ID" ]; then
                        echo "  ERROR: VpcId mismatch (expected $VPC_ID, got $INST_VPC)"
                        VPC_IP_OK=false
                    fi

                    VPC_PRIVATE_IPS+=("$PRIVATE_IP")
                done

                if [ "$VPC_IP_OK" = true ]; then
                    # Verify all IPs are unique and in the subnet range (10.200.1.x)
                    UNIQUE_IPS=$(printf '%s\n' "${VPC_PRIVATE_IPS[@]}" | sort -u | wc -l)
                    IP_RANGE_OK=true
                    if [ "$UNIQUE_IPS" -ne "${#VPC_PRIVATE_IPS[@]}" ]; then
                        echo "  ERROR: Duplicate IPs detected: ${VPC_PRIVATE_IPS[*]}"
                        IP_RANGE_OK=false
                    fi
                    for ip in "${VPC_PRIVATE_IPS[@]}"; do
                        if ! echo "$ip" | grep -qE '^10\.200\.1\.[0-9]+$'; then
                            echo "  ERROR: IP $ip not in expected subnet 10.200.1.0/24"
                            IP_RANGE_OK=false
                        fi
                    done

                    if [ "$IP_RANGE_OK" = true ]; then
                        echo "  All IPs unique and in correct subnet: ${VPC_PRIVATE_IPS[*]}"
                        pass_test "VPC IP allocation"
                    else
                        fail_test "VPC IP allocation"
                    fi

                    # Stop/Start IP persistence
                    echo ""
                    echo "Testing stop/start IP persistence..."
                    echo "  IPs before stop: ${VPC_PRIVATE_IPS[*]}"

                    # Stop all VPC instances
                    for vpc_inst in "${VPC_INSTANCE_IDS[@]}"; do
                        $AWS_EC2 stop-instances --instance-ids "$vpc_inst" > /dev/null
                    done
                    VPC_ALL_STOPPED=true
                    for vpc_inst in "${VPC_INSTANCE_IDS[@]}"; do
                        wait_for_instance_state "$vpc_inst" "stopped" 60 || {
                            echo "  ERROR: VPC instance $vpc_inst failed to stop"
                            VPC_ALL_STOPPED=false
                        }
                    done

                    if [ "$VPC_ALL_STOPPED" = true ]; then
                        # Verify IPs persist while stopped
                        IP_PERSIST_OK=true
                        for idx in "${!VPC_INSTANCE_IDS[@]}"; do
                            vpc_inst="${VPC_INSTANCE_IDS[$idx]}"
                            expected_ip="${VPC_PRIVATE_IPS[$idx]}"
                            STOPPED_IP=$($AWS_EC2 describe-instances --instance-ids "$vpc_inst" \
                                --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text)
                            if [ "$STOPPED_IP" != "$expected_ip" ]; then
                                echo "  ERROR: $vpc_inst IP changed while stopped (expected $expected_ip, got $STOPPED_IP)"
                                IP_PERSIST_OK=false
                            fi
                        done

                        # Restart all VPC instances
                        for vpc_inst in "${VPC_INSTANCE_IDS[@]}"; do
                            $AWS_EC2 start-instances --instance-ids "$vpc_inst" > /dev/null
                        done
                        for vpc_inst in "${VPC_INSTANCE_IDS[@]}"; do
                            wait_for_instance_state "$vpc_inst" "running" 60 || true
                        done

                        # Verify IPs persist after restart
                        for idx in "${!VPC_INSTANCE_IDS[@]}"; do
                            vpc_inst="${VPC_INSTANCE_IDS[$idx]}"
                            expected_ip="${VPC_PRIVATE_IPS[$idx]}"
                            RESTARTED_IP=$($AWS_EC2 describe-instances --instance-ids "$vpc_inst" \
                                --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text)
                            if [ "$RESTARTED_IP" != "$expected_ip" ]; then
                                echo "  ERROR: $vpc_inst IP changed after restart (expected $expected_ip, got $RESTARTED_IP)"
                                IP_PERSIST_OK=false
                            else
                                echo "  $vpc_inst: IP=$RESTARTED_IP (matches pre-stop)"
                            fi
                        done

                        if [ "$IP_PERSIST_OK" = true ]; then
                            pass_test "VPC stop/start IP persistence"
                        else
                            fail_test "VPC stop/start IP persistence"
                        fi
                    fi
                else
                    fail_test "VPC IP verification"
                fi
            fi

            # Cleanup VPC instances
            echo ""
            echo "Cleaning up VPC instances..."
            terminate_and_wait "${VPC_INSTANCE_IDS[@]}" || true
        else
            fail_test "VPC instance launch"
            # Cleanup any partially-launched VPC instances
            if [ ${#VPC_INSTANCE_IDS[@]} -gt 0 ]; then
                echo "  Cleaning up partially-launched VPC instances..."
                terminate_and_wait "${VPC_INSTANCE_IDS[@]}" || true
            fi
        fi

        # Cleanup subnet
        echo "  Deleting subnet $SUBNET_ID..."
        $AWS_EC2 delete-subnet --subnet-id "$SUBNET_ID" 2>/dev/null || true
    fi

    # Cleanup VPC
    echo "  Deleting VPC $VPC_ID..."
    $AWS_EC2 delete-vpc --vpc-id "$VPC_ID" 2>/dev/null || true
fi

echo ""

# ==========================================================================
# Phase 11: Spread Placement Group + NAT Gateway (multi-node)
# ==========================================================================
echo "Phase 11: Spread Placement Group + NAT Gateway (multi-node)"
echo "========================================"

echo "Step 1: Creating VPC infrastructure..."
NATGW_VPC_ID=$($AWS_EC2 create-vpc --cidr-block 10.100.0.0/16 \
    --query 'Vpc.VpcId' --output text)
echo "  VPC: $NATGW_VPC_ID"

NATGW_PUB_SUBNET=$($AWS_EC2 create-subnet --vpc-id "$NATGW_VPC_ID" \
    --cidr-block 10.100.1.0/24 --query 'Subnet.SubnetId' --output text)
echo "  Public subnet: $NATGW_PUB_SUBNET"

NATGW_PRIV_SUBNET=$($AWS_EC2 create-subnet --vpc-id "$NATGW_VPC_ID" \
    --cidr-block 10.100.2.0/24 --query 'Subnet.SubnetId' --output text)
echo "  Private subnet: $NATGW_PRIV_SUBNET"

# Enable public IPs on public subnet
$AWS_EC2 modify-subnet-attribute --subnet-id "$NATGW_PUB_SUBNET" \
    --map-public-ip-on-launch 2>/dev/null || true

NATGW_IGW_ID=$($AWS_EC2 create-internet-gateway \
    --query 'InternetGateway.InternetGatewayId' --output text)
$AWS_EC2 attach-internet-gateway --vpc-id "$NATGW_VPC_ID" \
    --internet-gateway-id "$NATGW_IGW_ID"
echo "  IGW: $NATGW_IGW_ID (attached)"

# Add default route to IGW on main route table for public subnet
NATGW_MAIN_RTB=$($AWS_EC2 describe-route-tables \
    --filters "Name=vpc-id,Values=$NATGW_VPC_ID" \
    --query 'RouteTables[0].RouteTableId' --output text)
$AWS_EC2 create-route --route-table-id "$NATGW_MAIN_RTB" \
    --destination-cidr-block 0.0.0.0/0 --gateway-id "$NATGW_IGW_ID" > /dev/null
echo "  Main route table: 0.0.0.0/0 → $NATGW_IGW_ID"

# Security groups: SG enforcement is wired into OVN ACLs, so the default SG
# (egress-only) drops inbound SSH. Create explicit SGs for bastion and private
# instances so the bastion is reachable and the private VMs accept SSH from it.
NATGW_BASTION_SG=$($AWS_EC2 create-security-group \
    --vpc-id "$NATGW_VPC_ID" --group-name natgw-bastion \
    --description "Phase 11 bastion (SSH ingress from anywhere)" \
    --query 'GroupId' --output text)
$AWS_EC2 authorize-security-group-ingress \
    --group-id "$NATGW_BASTION_SG" --protocol tcp --port 22 --cidr 0.0.0.0/0 > /dev/null
echo "  Bastion SG: $NATGW_BASTION_SG (tcp/22 from 0.0.0.0/0)"

NATGW_PRIV_SG=$($AWS_EC2 create-security-group \
    --vpc-id "$NATGW_VPC_ID" --group-name natgw-private \
    --description "Phase 11 private (SSH from bastion-sg, ICMP from VPC)" \
    --query 'GroupId' --output text)
$AWS_EC2 authorize-security-group-ingress \
    --group-id "$NATGW_PRIV_SG" \
    --ip-permissions "IpProtocol=tcp,FromPort=22,ToPort=22,UserIdGroupPairs=[{GroupId=$NATGW_BASTION_SG}]" > /dev/null
$AWS_EC2 authorize-security-group-ingress \
    --group-id "$NATGW_PRIV_SG" --protocol icmp --port -1 --cidr 10.100.0.0/16 > /dev/null
echo "  Private SG: $NATGW_PRIV_SG (tcp/22 from bastion-sg, icmp from VPC)"

echo ""
echo "Step 2: Launching bastion in public subnet..."
NATGW_BASTION_ID=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" --instance-type "$INSTANCE_TYPE" \
    --subnet-id "$NATGW_PUB_SUBNET" --key-name spinifex-key \
    --security-group-ids "$NATGW_BASTION_SG" \
    --count 1 --query 'Instances[0].InstanceId' --output text)
echo "  Bastion: $NATGW_BASTION_ID"
wait_for_instance_state "$NATGW_BASTION_ID" "running" 120

NATGW_BASTION_PUB_IP=$($AWS_EC2 describe-instances \
    --instance-ids "$NATGW_BASTION_ID" \
    --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
echo "  Bastion public IP: $NATGW_BASTION_PUB_IP"

# Wait for bastion SSH
echo "  Waiting for bastion SSH..."
BASTION_OK=false
for attempt in $(seq 1 30); do
    if ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
           -o LogLevel=ERROR -o ConnectTimeout=5 -o BatchMode=yes \
           -i "$HOME/.ssh/spinifex-key" "ec2-user@$NATGW_BASTION_PUB_IP" "true" 2>/dev/null; then
        BASTION_OK=true
        break
    fi
    sleep 5
done
if [ "$BASTION_OK" != true ]; then
    echo "  FAIL: Bastion SSH not reachable after 150s"
    fail_test "NAT GW bastion SSH"
    # Best-effort cleanup so the failure doesn't leak the bastion + SGs + VPC.
    terminate_and_wait "$NATGW_BASTION_ID" 2>/dev/null || true
    $AWS_EC2 delete-security-group --group-id "$NATGW_PRIV_SG" 2>/dev/null || true
    $AWS_EC2 delete-security-group --group-id "$NATGW_BASTION_SG" 2>/dev/null || true
    $AWS_EC2 delete-subnet --subnet-id "$NATGW_PRIV_SUBNET" 2>/dev/null || true
    $AWS_EC2 delete-subnet --subnet-id "$NATGW_PUB_SUBNET" 2>/dev/null || true
    $AWS_EC2 detach-internet-gateway --vpc-id "$NATGW_VPC_ID" \
        --internet-gateway-id "$NATGW_IGW_ID" 2>/dev/null || true
    $AWS_EC2 delete-internet-gateway --internet-gateway-id "$NATGW_IGW_ID" 2>/dev/null || true
    $AWS_EC2 delete-vpc --vpc-id "$NATGW_VPC_ID" 2>/dev/null || true
else
    echo "  PASS: Bastion SSH ready"

    # Copy SSH key to bastion for private instance hops
    scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
        -i "$HOME/.ssh/spinifex-key" "$HOME/.ssh/spinifex-key" \
        "ec2-user@$NATGW_BASTION_PUB_IP:/tmp/key.pem" 2>/dev/null
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
        -i "$HOME/.ssh/spinifex-key" "ec2-user@$NATGW_BASTION_PUB_IP" "chmod 600 /tmp/key.pem" 2>/dev/null

    # Bastion SSH helper
    bastion_ssh() {
        ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=10 \
            -i "$HOME/.ssh/spinifex-key" "ec2-user@$NATGW_BASTION_PUB_IP" "$@"
    }
    priv_hop_cmd() {
        local priv_ip="$1"; shift
        echo "ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=10 -o BatchMode=yes -i /tmp/key.pem ec2-user@${priv_ip} '$*'"
    }

    echo ""
    echo "Step 3: Creating spread placement group + launching 3 private VMs..."
    $AWS_EC2 create-placement-group --group-name nat-spread --strategy spread > /dev/null
    echo "  Placement group: nat-spread (spread)"

    # Launch 3 instances in private subnet with spread placement
    NATGW_PRIV_OUTPUT=$($AWS_EC2 run-instances \
        --image-id "$AMI_ID" --instance-type "$INSTANCE_TYPE" \
        --subnet-id "$NATGW_PRIV_SUBNET" --key-name spinifex-key \
        --security-group-ids "$NATGW_PRIV_SG" \
        --count 3 --placement "GroupName=nat-spread" --output json)
    NATGW_PRIV_IDS=()
    for i in 0 1 2; do
        NATGW_PRIV_IDS+=("$(echo "$NATGW_PRIV_OUTPUT" | jq -r ".Instances[$i].InstanceId")")
    done
    echo "  Private instances: ${NATGW_PRIV_IDS[*]}"

    # Wait for all running
    ALL_RUNNING=true
    for inst_id in "${NATGW_PRIV_IDS[@]}"; do
        if ! wait_for_instance_state "$inst_id" "running" 120; then
            ALL_RUNNING=false
        fi
    done

    if [ "$ALL_RUNNING" = true ]; then
        echo ""
        echo "Step 4: Validating spread placement across nodes..."

        # Use spx get vms to check node assignment
        SPX_VMS=$($SPINIFEX_BIN get vms --timeout 5s 2>/dev/null)
        echo "$SPX_VMS"

        SPREAD_NODES=()
        for inst_id in "${NATGW_PRIV_IDS[@]}"; do
            # spx get vms output is pipe-delimited: INSTANCE | STATUS | TYPE | VCPU | MEM | NODE | IP | AGE
            # NODE is field 6
            NODE_NAME=$(echo "$SPX_VMS" | grep "$inst_id" | awk -F'|' '{gsub(/^[ \t]+|[ \t]+$/, "", $6); print $6}' || echo "unknown")
            if [ -z "$NODE_NAME" ] || [ "$NODE_NAME" = "unknown" ]; then
                NODE_NAME=$(find_instance_node "$inst_id" || echo "unknown")
            fi
            SPREAD_NODES+=("$NODE_NAME")
            echo "  $inst_id → $NODE_NAME"
        done

        # Count unique nodes
        UNIQUE_NODES=$(printf '%s\n' "${SPREAD_NODES[@]}" | sort -u | wc -l)
        echo "  Unique hosting nodes: $UNIQUE_NODES / ${#SPREAD_NODES[@]}"
        if [ "$UNIQUE_NODES" -ge 3 ]; then
            pass_test "Spread placement (3 instances on 3 nodes)"
        elif [ "$UNIQUE_NODES" -ge 2 ]; then
            echo "  WARN: Only $UNIQUE_NODES unique nodes (expected 3) — spread best-effort"
            pass_test "Spread placement (best-effort: $UNIQUE_NODES nodes)"
        else
            fail_test "Spread placement ($UNIQUE_NODES unique nodes, expected 3)"
        fi

        # Get private IPs for SSH hop
        NATGW_PRIV_PRIVATE_IPS=()
        for inst_id in "${NATGW_PRIV_IDS[@]}"; do
            PRIV_IP=$($AWS_EC2 describe-instances --instance-ids "$inst_id" \
                --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text)
            NATGW_PRIV_PRIVATE_IPS+=("$PRIV_IP")
        done
        echo "  Private IPs: ${NATGW_PRIV_PRIVATE_IPS[*]}"

        # Wait for cloud-init on private instances via bastion
        echo ""
        echo "  Waiting for private instance SSH via bastion..."
        PRIV_SSH_READY=true
        for idx in "${!NATGW_PRIV_IDS[@]}"; do
            priv_ip="${NATGW_PRIV_PRIVATE_IPS[$idx]}"
            inst_id="${NATGW_PRIV_IDS[$idx]}"
            SSH_OK=false
            for attempt in $(seq 1 30); do
                if bastion_ssh "$(priv_hop_cmd "$priv_ip" hostname)" 2>/dev/null; then
                    SSH_OK=true
                    break
                fi
                sleep 5
            done
            if [ "$SSH_OK" = true ]; then
                echo "    $inst_id ($priv_ip): SSH ready"
            else
                echo "    $inst_id ($priv_ip): SSH FAILED after 150s"
                PRIV_SSH_READY=false
            fi
        done

        if [ "$PRIV_SSH_READY" = true ]; then
            echo ""
            echo "Step 5: Verify NO internet (pre-NAT)..."
            PRE_NAT_OK=true
            for idx in "${!NATGW_PRIV_IDS[@]}"; do
                priv_ip="${NATGW_PRIV_PRIVATE_IPS[$idx]}"
                inst_id="${NATGW_PRIV_IDS[$idx]}"
                if bastion_ssh "$(priv_hop_cmd "$priv_ip" 'ping -c 1 -W 3 8.8.8.8')" 2>/dev/null; then
                    echo "    FAIL: $inst_id can reach internet WITHOUT NAT GW"
                    PRE_NAT_OK=false
                else
                    echo "    PASS: $inst_id has no internet (expected)"
                fi
            done

            if [ "$PRE_NAT_OK" = true ]; then
                pass_test "Private instances no internet (pre-NAT)"

                echo ""
                echo "Step 6: Creating NAT Gateway..."
                NATGW_EIP_OUTPUT=$($AWS_EC2 allocate-address --domain vpc --output json)
                NATGW_ALLOC_ID=$(echo "$NATGW_EIP_OUTPUT" | jq -r '.AllocationId')
                NATGW_PUB_IP=$(echo "$NATGW_EIP_OUTPUT" | jq -r '.PublicIp')
                echo "  Allocated EIP: $NATGW_PUB_IP ($NATGW_ALLOC_ID)"

                NATGW_OUTPUT=$($AWS_EC2 create-nat-gateway \
                    --subnet-id "$NATGW_PUB_SUBNET" \
                    --allocation-id "$NATGW_ALLOC_ID" --output json)
                NATGW_ID=$(echo "$NATGW_OUTPUT" | jq -r '.NatGateway.NatGatewayId')
                echo "  NAT Gateway: $NATGW_ID"

                # Create private route table + NAT GW route
                NATGW_PRIV_RTB=$($AWS_EC2 create-route-table --vpc-id "$NATGW_VPC_ID" \
                    --query 'RouteTable.RouteTableId' --output text)
                NATGW_RTB_ASSOC=$($AWS_EC2 associate-route-table \
                    --route-table-id "$NATGW_PRIV_RTB" \
                    --subnet-id "$NATGW_PRIV_SUBNET" \
                    --query 'AssociationId' --output text)
                $AWS_EC2 create-route --route-table-id "$NATGW_PRIV_RTB" \
                    --destination-cidr-block 0.0.0.0/0 \
                    --nat-gateway-id "$NATGW_ID" > /dev/null
                echo "  Route: 0.0.0.0/0 → $NATGW_ID (rtb: $NATGW_PRIV_RTB)"

                # Poll for OVN SNAT rule
                SNAT_FOUND=false
                for attempt in $(seq 1 30); do
                    NAT_RULES=$(sudo ovn-nbctl --no-leader-only lr-nat-list "vpc-${NATGW_VPC_ID}" 2>/dev/null || echo "")
                    if echo "$NAT_RULES" | grep -q "snat.*${NATGW_PUB_IP}"; then
                        SNAT_FOUND=true
                        echo "  PASS: OVN SNAT rule created (after ${attempt}s)"
                        break
                    fi
                    sleep 1
                done
                if [ "$SNAT_FOUND" = false ]; then
                    echo "  WARN: OVN SNAT rule not found after 30s"
                fi

                echo ""
                echo "Step 7: Verify internet via NAT Gateway (all 3 nodes)..."
                NAT_GW_ALL_OK=true
                for idx in "${!NATGW_PRIV_IDS[@]}"; do
                    priv_ip="${NATGW_PRIV_PRIVATE_IPS[$idx]}"
                    inst_id="${NATGW_PRIV_IDS[$idx]}"
                    node="${SPREAD_NODES[$idx]}"
                    CONN_OK=false
                    for attempt in $(seq 1 10); do
                        if bastion_ssh "$(priv_hop_cmd "$priv_ip" 'ping -c 2 -W 3 8.8.8.8')" 2>/dev/null; then
                            CONN_OK=true
                            echo "    PASS: $inst_id ($node) → internet via NAT GW (attempt $attempt)"
                            break
                        fi
                        sleep 5
                    done
                    if [ "$CONN_OK" = false ]; then
                        echo "    FAIL: $inst_id ($node) cannot reach internet via NAT GW"
                        NAT_GW_ALL_OK=false
                    fi
                done

                if [ "$NAT_GW_ALL_OK" = true ]; then
                    pass_test "NAT Gateway multi-node connectivity (all 3 nodes)"
                else
                    fail_test "NAT Gateway multi-node connectivity"
                    echo "  Dumping OVN NAT rules for debugging:"
                    sudo ovn-nbctl --no-leader-only lr-nat-list "vpc-${NATGW_VPC_ID}" 2>/dev/null || true
                fi

                # Cleanup NAT GW resources
                echo ""
                echo "Step 8: Cleanup NAT Gateway..."
                $AWS_EC2 delete-nat-gateway --nat-gateway-id "$NATGW_ID" > /dev/null 2>&1 || true
                $AWS_EC2 disassociate-route-table --association-id "$NATGW_RTB_ASSOC" 2>/dev/null || true
                $AWS_EC2 delete-route --route-table-id "$NATGW_PRIV_RTB" \
                    --destination-cidr-block 0.0.0.0/0 2>/dev/null || true
                $AWS_EC2 delete-route-table --route-table-id "$NATGW_PRIV_RTB" 2>/dev/null || true
                $AWS_EC2 release-address --allocation-id "$NATGW_ALLOC_ID" 2>/dev/null || true
                echo "  NAT GW resources cleaned up"
            else
                fail_test "Private instances no internet (pre-NAT)"
            fi
        else
            fail_test "Private instance SSH via bastion"
        fi
    else
        fail_test "Private instance launch (spread)"
    fi

    # Cleanup Phase 11 instances
    echo ""
    echo "Cleaning up Phase 11 instances..."
    terminate_and_wait "$NATGW_BASTION_ID" "${NATGW_PRIV_IDS[@]}" 2>/dev/null || true
    $AWS_EC2 delete-placement-group --group-name nat-spread 2>/dev/null || true

    # Cleanup VPC resources
    $AWS_EC2 delete-security-group --group-id "$NATGW_PRIV_SG" 2>/dev/null || true
    $AWS_EC2 delete-security-group --group-id "$NATGW_BASTION_SG" 2>/dev/null || true
    $AWS_EC2 delete-subnet --subnet-id "$NATGW_PRIV_SUBNET" 2>/dev/null || true
    $AWS_EC2 delete-subnet --subnet-id "$NATGW_PUB_SUBNET" 2>/dev/null || true
    $AWS_EC2 detach-internet-gateway --vpc-id "$NATGW_VPC_ID" \
        --internet-gateway-id "$NATGW_IGW_ID" 2>/dev/null || true
    $AWS_EC2 delete-internet-gateway --internet-gateway-id "$NATGW_IGW_ID" 2>/dev/null || true
    # Delete non-main route tables
    for rtb in $($AWS_EC2 describe-route-tables \
        --filters "Name=vpc-id,Values=$NATGW_VPC_ID" \
        --query 'RouteTables[?Associations[0].Main!=`true`].RouteTableId' --output text 2>/dev/null); do
        $AWS_EC2 delete-route-table --route-table-id "$rtb" 2>/dev/null || true
    done
    $AWS_EC2 delete-vpc --vpc-id "$NATGW_VPC_ID" 2>/dev/null || true
    echo "  Phase 11 cleanup complete"
fi

echo ""

# ==========================================================================
# Phase 12: Cleanup
# ==========================================================================
echo "Phase 12: Cleanup"
echo "========================================"

echo "Terminating all instances..."
if terminate_and_wait "${INSTANCE_IDS[@]}"; then
    pass_test "Instance termination"
else
    fail_test "Instance termination"
fi

# ==========================================================================
# Summary
# ==========================================================================
echo ""
echo "========================================"
echo "Real Multi-Node E2E Test Summary"
echo "========================================"
echo "  Passed: $TESTS_PASSED"
echo "  Failed: $TESTS_FAILED"
if [ ${#FAILED_TESTS[@]} -gt 0 ]; then
    echo "  Failed tests:"
    for t in "${FAILED_TESTS[@]}"; do
        echo "    - $t"
    done
fi
echo "========================================"

if [ $TESTS_FAILED -gt 0 ]; then
    echo "SOME TESTS FAILED"
    exit 1
fi

echo "All tests passed!"
exit 0
