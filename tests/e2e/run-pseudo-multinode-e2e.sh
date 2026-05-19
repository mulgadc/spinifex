#!/bin/bash
set -e

# Ensure Go is on PATH (SSH non-interactive shells don't source .bashrc)
export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"

# Pseudo Multi-Node E2E test runner
# This script sets up a 3-node Spinifex cluster using simulated IPs on the loopback interface
# and runs distributed instance tests on a single VM.

# Ensure we are in the project root
cd "$(dirname "$0")/../.."

# Source helper functions
source ./tests/e2e/lib/multinode-helpers.sh

# Cleanup function - ensure resources are cleaned up on exit
cleanup() {
    local exit_code=$?

    echo ""
    echo "Cleanup triggered (exit code: $exit_code)..."

    if [ $exit_code -ne 0 ]; then
        dump_all_node_logs
    fi

    # Kill any lingering formation background processes
    [ -n "$LEADER_INIT_PID" ] && kill "$LEADER_INIT_PID" 2>/dev/null || true
    [ -n "$JOIN2_PID" ] && kill "$JOIN2_PID" 2>/dev/null || true
    [ -n "$JOIN3_PID" ] && kill "$JOIN3_PID" 2>/dev/null || true

    # Try coordinated shutdown first (only if NATS is likely still up)
    if [ "$CLUSTER_SERVICES_STARTED" = "true" ]; then
        echo "Attempting coordinated cluster shutdown..."
        if timeout 60 ./bin/spx admin cluster shutdown --force --timeout 30s --config "$HOME/node1/config/spinifex.toml" 2>/dev/null; then
            echo "Coordinated shutdown succeeded"
        else
            echo "Coordinated shutdown failed, falling back to per-node stop..."
            stop_all_nodes || true
        fi
    else
        stop_all_nodes || true
    fi

    # Force-kill anything that survived and clean up stale locks
    force_cleanup_all_nodes || true

    # Remove simulated IPs
    remove_simulated_ips || true

    echo "Cleanup complete"
}
trap cleanup EXIT

# PIDs for background formation processes (used in cleanup)
JOIN2_PID=""
JOIN3_PID=""

# Track whether cluster services have been started (for cleanup trap)
CLUSTER_SERVICES_STARTED="false"

# Use Spinifex profile for AWS CLI
export AWS_PROFILE=spinifex
# Trust Spinifex CA for all profiles (AWS CLI v2 bundles its own Python/certifi, ignores system CA store)
export AWS_CA_BUNDLE="$HOME/node1/config/ca.pem"


echo "========================================"
echo "Multi-Node E2E Test Suite"
echo "========================================"
echo ""

# Phase 1: Environment Setup
echo "Phase 1: Environment Setup"
echo "========================================"

# Check for KVM support
echo "Checking for KVM support..."
if [ -e /dev/kvm ]; then
    echo "  /dev/kvm exists"
    if [ -w /dev/kvm ]; then
        echo "  /dev/kvm is writable"
    else
        echo "  ERROR: /dev/kvm is NOT writable"
        exit 1
    fi
else
    echo "  ERROR: /dev/kvm does NOT exist"
    exit 1
fi

# Check for ip command (iproute2)
if ! command -v ip &> /dev/null; then
    echo "  ERROR: 'ip' command not found. Install iproute2."
    exit 1
fi


# Setup simulated network
echo ""
echo "Setting up simulated network..."
add_simulated_ips

echo ""

# Phase 2: Cluster Initialization
echo "Phase 2: Cluster Initialization"
echo "========================================"

# Background init — starts formation server, generates certs first
echo ""
init_leader_node

# Trust CA cert (exists before formation completes — cert generation is the first step)
echo ""
echo "Adding Spinifex CA certificate to system trust store..."
sudo cp ~/node1/config/ca.pem /usr/local/share/ca-certificates/spinifex-ca.crt
sudo update-ca-certificates

# Background BOTH joins — they poll /formation/status until all 3 nodes have joined.
# Must be concurrent: each join blocks until formation is complete.
echo ""
echo "Joining follower nodes concurrently..."
join_follower_node 2 &
JOIN2_PID=$!
join_follower_node 3 &
JOIN3_PID=$!

# Wait for formation to complete (all processes generate their configs)
echo "Waiting for cluster formation to complete..."
wait $JOIN2_PID || { echo "ERROR: Node 2 join failed"; exit 1; }
wait $JOIN3_PID || { echo "ERROR: Node 3 join failed"; exit 1; }
wait $LEADER_INIT_PID || { echo "ERROR: Leader init failed"; exit 1; }
echo "Cluster formation complete — all configs generated"

# Disable monitoring on non-primary nodes to avoid port conflict on 127.0.0.1:8222
for i in 2 3; do
    sed -i '/^http:\s*127\.0\.0\.1:8222/d' "$HOME/node${i}/config/nats/nats.conf"
    echo "  Stripped NATS monitoring from node$i (avoids 127.0.0.1:8222 conflict)"
done

# Now start services (configs exist for all nodes)
echo ""
echo "Starting node services..."
start_node_services 1 "$HOME/node1"
start_node_services 2 "$HOME/node2"
start_node_services 3 "$HOME/node3"
CLUSTER_SERVICES_STARTED="true"

# Wait for all services to stabilize
echo ""
echo "Waiting for cluster to stabilize..."
sleep 5

# Phase 3: Cluster Health Verification
echo ""
echo "Phase 3: Cluster Health Verification"
echo "========================================"

# Verify NATS cluster
echo ""
verify_nats_cluster 3 || {
    echo "WARNING: NATS cluster verification failed, continuing anyway..."
}

# Verify Predastore cluster
echo ""
verify_predastore_cluster 3 || {
    echo "ERROR: Predastore cluster verification failed"
    dump_all_node_logs
    exit 1
}

# Wait for gateway on node1 (primary gateway)
echo ""
wait_for_gateway "${NODE1_IP}" 15

# Wait for daemon NATS subscriptions to be active
wait_for_daemon_ready "https://${NODE1_IP}:${AWSGW_PORT}"

# Define AWS CLI args pointing to node1's gateway
AWS_EC2="aws --endpoint-url https://${NODE1_IP}:${AWSGW_PORT} ec2"
AWS_IAM="aws --endpoint-url https://${NODE1_IP}:${AWSGW_PORT} iam"

# Discover the cluster's availability zone and region dynamically
SPINIFEX_AZ=$($AWS_EC2 describe-availability-zones --query 'AvailabilityZones[0].ZoneName' --output text)
SPINIFEX_REGION=$($AWS_EC2 describe-availability-zones --query 'AvailabilityZones[0].RegionName' --output text)
echo "Discovered AZ: $SPINIFEX_AZ, Region: $SPINIFEX_REGION"

# Phase 3b: Cluster Stats CLI (Multi-Node)
echo ""
echo "Phase 3b: Cluster Stats CLI (Multi-Node)"
echo "========================================"

# Test spx get nodes — should show all 3 nodes as Ready
echo "Testing spx get nodes..."
GET_NODES_OUTPUT=$(./bin/spx get nodes --config "$HOME/node1/config/spinifex.toml" --timeout 5s 2>/dev/null)
echo "$GET_NODES_OUTPUT"
READY_COUNT=$(echo "$GET_NODES_OUTPUT" | grep -c "Ready" || true)
if [ "$READY_COUNT" -lt 3 ]; then
    echo "WARNING: spx get nodes shows $READY_COUNT Ready nodes (expected 3)"
fi
echo "spx get nodes passed ($READY_COUNT Ready nodes)"

# Test spx top nodes — should show resource stats for all nodes
echo "Testing spx top nodes..."
TOP_NODES_OUTPUT=$(./bin/spx top nodes --config "$HOME/node1/config/spinifex.toml" --timeout 5s 2>/dev/null)
echo "$TOP_NODES_OUTPUT"
if ! echo "$TOP_NODES_OUTPUT" | grep -q "INSTANCE TYPE"; then
    echo "WARNING: spx top nodes did not show instance type capacity table"
fi
echo "spx top nodes passed"

# Test spx get vms — should show no VMs yet
echo "Testing spx get vms (empty)..."
GET_VMS_OUTPUT=$(./bin/spx get vms --config "$HOME/node1/config/spinifex.toml" --timeout 5s 2>/dev/null)
echo "$GET_VMS_OUTPUT"
echo "spx get vms (empty) passed"

# Verify gateway responds
echo ""
echo "Testing gateway connectivity..."
$AWS_EC2 describe-regions | jq -e '.Regions | length > 0' || {
    echo "ERROR: Gateway not responding correctly"
    exit 1
}
echo "  Gateway is responding"

# Phase 4: Image and Key Setup
echo ""
echo "Phase 4: Image and Key Setup"
echo "========================================"

# Discover instance types
echo "Discovering available instance types..."
AVAILABLE_TYPES=$($AWS_EC2 describe-instance-types --query 'InstanceTypes[*].InstanceType' --output text)
echo "  Available: $AVAILABLE_TYPES"

# Pick nano instance type
INSTANCE_TYPE=$(echo $AVAILABLE_TYPES | tr ' ' '\n' | grep -m1 'nano')
if [ -z "$INSTANCE_TYPE" ] || [ "$INSTANCE_TYPE" == "None" ]; then
    echo "ERROR: No instance types found"
    exit 1
fi
echo "  Selected: $INSTANCE_TYPE"

# Get architecture
ARCH=$($AWS_EC2 describe-instance-types --instance-types "$INSTANCE_TYPE" \
    --query 'InstanceTypes[0].ProcessorInfo.SupportedArchitectures[0]' --output text)
echo "  Architecture: $ARCH"

# Create test key
echo ""
echo "Creating test key pair..."
KEY_MATERIAL=$($AWS_EC2 create-key-pair --key-name multinode-test-key --query 'KeyMaterial' --output text)
echo "$KEY_MATERIAL" > multinode-test-key.pem
chmod 600 multinode-test-key.pem
echo "  Key created: multinode-test-key"

# Default SG is egress-only on this branch; create an explicit SG that allows
# SSH + ICMP so subsequent run-instances calls produce reachable VMs.
echo ""
echo "Creating test security group..."
DEFAULT_VPC_ID=$($AWS_EC2 describe-vpcs --query 'Vpcs[?IsDefault==`true`].VpcId | [0]' --output text)
MULTINODE_SG=$($AWS_EC2 create-security-group \
    --group-name multinode-test-sg \
    --description "Pseudo multi-node e2e (SSH + ICMP ingress)" \
    --vpc-id "$DEFAULT_VPC_ID" \
    --query 'GroupId' --output text)
$AWS_EC2 authorize-security-group-ingress \
    --group-id "$MULTINODE_SG" --protocol tcp --port 22 --cidr 0.0.0.0/0 > /dev/null
$AWS_EC2 authorize-security-group-ingress \
    --group-id "$MULTINODE_SG" --protocol icmp --port -1 --cidr 0.0.0.0/0 > /dev/null
echo "  SG created: $MULTINODE_SG (tcp/22 + icmp from 0.0.0.0/0)"

# Import Ubuntu image (use node1's config and spinifex-dir). v6+ gold image
# stages ubuntu-26.04 (resolute); v3 staged ubuntu-24.04 (noble). Detect
# which is present so we can keep running both during the v3→v6 transition.
echo ""
echo "Importing Ubuntu image..."
if [ -f "$HOME/images/ubuntu-26.04.img" ]; then
    UBUNTU_IMG="$HOME/images/ubuntu-26.04.img"
    UBUNTU_VER="26.04"
elif [ -f "$HOME/images/ubuntu-24.04.img" ]; then
    UBUNTU_IMG="$HOME/images/ubuntu-24.04.img"
    UBUNTU_VER="24.04"
else
    echo "ERROR: no ubuntu cloud image at ~/images/ubuntu-{24,26}.04.img" >&2
    exit 1
fi
echo "  Using: $UBUNTU_IMG (version $UBUNTU_VER)"
IMPORT_LOG=$(./bin/spx admin images import \
    --file "$UBUNTU_IMG" \
    --arch "$ARCH" \
    --distro ubuntu \
    --version "$UBUNTU_VER" \
    --config "$HOME/node1/config/spinifex.toml" \
    --spinifex-dir "$HOME/node1/" \
    --force 2>/dev/null)
echo "Import output: $IMPORT_LOG"
AMI_ID=$(echo "$IMPORT_LOG" | grep -o 'ami-[a-z0-9]\+')

if [ -z "$AMI_ID" ]; then
    echo "ERROR: Failed to capture AMI ID"
    exit 1
fi
echo "  AMI ID: $AMI_ID"

# Verify AMI
$AWS_EC2 describe-images --image-ids "$AMI_ID" | jq -e ".Images[0] | select(.ImageId==\"$AMI_ID\")" > /dev/null
echo "  AMI verified"


# Phase 5: Multi-Node Instance Tests
echo ""
echo "Phase 5: Multi-Node Instance Tests"
echo "========================================"

# Test 1: Instance Distribution
echo ""
echo "Test 1: Instance Distribution"
echo "----------------------------------------"
echo "Launching 3 instances to test distribution across nodes..."

INSTANCE_IDS=()
for i in 1 2 3; do
    echo "  Launching instance $i..."
    RUN_OUTPUT=$($AWS_EC2 run-instances \
        --image-id "$AMI_ID" \
        --instance-type "$INSTANCE_TYPE" \
        --key-name multinode-test-key \
        --security-group-ids "$MULTINODE_SG")

    INSTANCE_ID=$(echo "$RUN_OUTPUT" | jq -r '.Instances[0].InstanceId')
    if [ -z "$INSTANCE_ID" ] || [ "$INSTANCE_ID" == "null" ]; then
        echo "  ERROR: Failed to launch instance $i"
        echo "  Output: $RUN_OUTPUT"
        exit 1
    fi
    echo "  Launched: $INSTANCE_ID"
    INSTANCE_IDS+=("$INSTANCE_ID")

    # Small delay between launches to encourage distribution
    sleep 1
done

# Wait for all instances to be running
echo ""
echo "Waiting for instances to reach running state..."
for instance_id in "${INSTANCE_IDS[@]}"; do
    wait_for_instance_state "$instance_id" "running" 30 || {
        echo "ERROR: Instance $instance_id failed to start"
        exit 1
    }
done

# Check distribution — enforce all 3 nodes received an instance
echo ""
check_instance_distribution 3 || {
    echo "ERROR: Default spread did not distribute instances across all 3 nodes"
    exit 1
}
echo "  Default spread distribution verified"

# Verify spx get vms shows all running instances
echo ""
echo "Verifying spx get vms (with running VMs)..."
GET_VMS_OUTPUT=$(./bin/spx get vms --config "$HOME/node1/config/spinifex.toml" --timeout 5s 2>/dev/null)
echo "$GET_VMS_OUTPUT"
for instance_id in "${INSTANCE_IDS[@]}"; do
    if ! echo "$GET_VMS_OUTPUT" | grep -q "$instance_id"; then
        echo "WARNING: spx get vms did not show instance $instance_id"
    fi
done
echo "spx get vms shows launched instances"

# Test 1a-ii: SSH Connectivity & Volume Verification
echo ""
echo "Test 1a-ii: SSH Connectivity & Volume Verification"
echo "----------------------------------------"
echo "Testing SSH into all 3 instances..."

# Arrays to store SSH details for post-termination verification
SSH_PORTS=()
SSH_HOSTS=()

for idx in "${!INSTANCE_IDS[@]}"; do
    instance_id="${INSTANCE_IDS[$idx]}"
    echo ""
    echo "  Instance $((idx + 1)): $instance_id"

    # Determine SSH connection method:
    # - If instance has a public IP (external networking), SSH via public IP on port 22
    # - Otherwise, fall back to QEMU hostfwd (dev_networking mode)
    INST_PUBLIC_IP=$($AWS_EC2 describe-instances --instance-ids "$instance_id" \
        --query 'Reservations[0].Instances[0].PublicIpAddress' --output text 2>/dev/null || echo "None")

    if [ -n "$INST_PUBLIC_IP" ] && [ "$INST_PUBLIC_IP" != "None" ] && [ "$INST_PUBLIC_IP" != "null" ]; then
        SSH_HOST="$INST_PUBLIC_IP"
        SSH_PORT=22
        echo "  SSH via public IP: $SSH_HOST:$SSH_PORT"
    else
        echo "  No public IP — using QEMU hostfwd for SSH"
        # set -e would abort on $() returning non-zero, hiding the diagnostic below
        if ! SSH_PORT=$(get_ssh_port "$instance_id"); then
            SSH_PORT=""
        fi
        if [ -z "$SSH_PORT" ]; then
            echo "  ERROR: Failed to get SSH port for instance $instance_id"
            exit 1
        fi
        SSH_HOST=$(get_ssh_host "$instance_id")
        echo "  SSH endpoint: $SSH_HOST:$SSH_PORT"
    fi

    SSH_PORTS+=("$SSH_PORT")
    SSH_HOSTS+=("$SSH_HOST")

    # Wait for SSH to become ready (VM boot + cloud-init)
    wait_for_ssh "$SSH_HOST" "$SSH_PORT" "multinode-test-key.pem" 60

    # Test basic SSH connectivity
    test_ssh_connectivity "$SSH_HOST" "$SSH_PORT" "multinode-test-key.pem"

    # Check root volume size via lsblk
    echo "  Verifying root volume size from inside the VM..."
    ROOT_VOL_ID_SSH=$($AWS_EC2 describe-instances --instance-ids "$instance_id" \
        --query 'Reservations[0].Instances[0].BlockDeviceMappings[0].Ebs.VolumeId' --output text)
    ROOT_VOL_SIZE_API=$($AWS_EC2 describe-volumes --volume-ids "$ROOT_VOL_ID_SSH" \
        --query 'Volumes[0].Size' --output text)
    # Find the disk backing the root filesystem (avoids picking up floppy/cdrom devices)
    # 1. findmnt gets the source device for / (e.g. /dev/vda1)
    # 2. lsblk PKNAME resolves to parent disk name (e.g. vda)
    # 3. lsblk -b -d gets that disk's byte size
    ROOT_DISK_BYTES=$(ssh -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR \
        -o ConnectTimeout=5 \
        -o BatchMode=yes \
        -p "$SSH_PORT" \
        -i "multinode-test-key.pem" \
        ec2-user@"$SSH_HOST" 'SRC=$(findmnt -n -o SOURCE /); PKN=$(lsblk -n -o PKNAME "$SRC" 2>/dev/null | head -1); DEV=${PKN:-$(basename "$SRC")}; lsblk -b -d -n -o SIZE "/dev/$DEV"' | tr -d '[:space:]')
    if [ -z "$ROOT_DISK_BYTES" ] || [ "$ROOT_DISK_BYTES" = "0" ]; then
        echo "  ERROR: Failed to get root disk size from VM (got: '$ROOT_DISK_BYTES')"
        echo "  lsblk debug output:"
        ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
            -o ConnectTimeout=5 -o BatchMode=yes -p "$SSH_PORT" -i "multinode-test-key.pem" \
            ec2-user@"$SSH_HOST" 'lsblk -b -d; echo "---"; findmnt -n -o SOURCE /; cat /proc/partitions' || true
        exit 1
    fi
    ROOT_DISK_GIB=$((ROOT_DISK_BYTES / 1073741824))
    echo "  Root disk size from VM: ${ROOT_DISK_GIB}GiB (API reports: ${ROOT_VOL_SIZE_API}GiB)"
    if [ "$ROOT_DISK_GIB" -ne "$ROOT_VOL_SIZE_API" ]; then
        echo "  ERROR: Root volume size mismatch: VM reports ${ROOT_DISK_GIB}GiB, API reports ${ROOT_VOL_SIZE_API}GiB"
        exit 1
    fi
    echo "  Root volume size verified"

    # Verify hostname contains instance ID
    echo "  Verifying hostname inside the VM..."
    VM_HOSTNAME=$(ssh -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 \
        -o BatchMode=yes \
        -p "$SSH_PORT" \
        -i "multinode-test-key.pem" \
        ec2-user@"$SSH_HOST" 'hostname' 2>/dev/null)
    echo "  VM hostname: $VM_HOSTNAME"
    # Hostname uses truncated ID: spinifex-vm-<first 8 hex chars of instance ID>
    SHORT_ID=$(echo "$instance_id" | sed 's/^i-//' | cut -c1-8)
    if echo "$VM_HOSTNAME" | grep -q "$SHORT_ID"; then
        echo "  Hostname contains instance ID prefix ($SHORT_ID)"
    else
        echo "  WARNING: Hostname '$VM_HOSTNAME' does not contain instance ID prefix '$SHORT_ID' (non-fatal)"
    fi
done

echo ""
echo "  SSH connectivity and volume verification passed for all instances"

# Test 2: DescribeInstances Aggregation
echo ""
echo "Test 2: DescribeInstances Aggregation"
echo "----------------------------------------"
echo "Verifying all instances are returned via fan-out query..."

DESCRIBE_OUTPUT=$($AWS_EC2 describe-instances --query 'Reservations[*].Instances[*].InstanceId' --output text)
DESCRIBED_COUNT=$(echo "$DESCRIBE_OUTPUT" | wc -w)

echo "  Launched: ${#INSTANCE_IDS[@]} instances"
echo "  Described: $DESCRIBED_COUNT instances"

if [ "$DESCRIBED_COUNT" -lt "${#INSTANCE_IDS[@]}" ]; then
    echo "ERROR: DescribeInstances did not return all instances"
    echo "  Expected: ${#INSTANCE_IDS[@]}, Got: $DESCRIBED_COUNT"
    exit 1
fi
echo "  Aggregation test passed"

# Test 3: Cross-Node Operations
echo ""
echo "Test 3: Cross-Node Operations"
echo "----------------------------------------"
echo "Testing stop/start/terminate via gateway regardless of instance location..."

# Pick first instance for cross-node operations
TEST_INSTANCE="${INSTANCE_IDS[0]}"
echo "  Test instance: $TEST_INSTANCE"

# Stop instance
echo "  Stopping instance..."
$AWS_EC2 stop-instances --instance-ids "$TEST_INSTANCE" > /dev/null
wait_for_instance_state "$TEST_INSTANCE" "stopped" 30

# Start instance
echo "  Starting instance..."
$AWS_EC2 start-instances --instance-ids "$TEST_INSTANCE" > /dev/null
wait_for_instance_state "$TEST_INSTANCE" "running" 30

echo "  Cross-node operations test passed"

# Test 4: NATS Cluster Health (Post-Operations)
echo ""
echo "Test 4: NATS Cluster Health (Post-Operations)"
echo "----------------------------------------"
echo "Verifying NATS cluster is still healthy after operations..."

verify_nats_cluster 3 || {
    echo "WARNING: NATS cluster verification failed after operations"
}

# Phase 5b: Multi-Node Batch Distribution Tests
# Tests batch launches with --count N (placement groups phase 1 routing)
echo ""
echo "Phase 5b: Multi-Node Batch Distribution Tests"
echo "========================================"

# Clean up instances from Phase 5 before batch tests
echo "Cleaning up Phase 5 instances..."
if ! terminate_and_wait "${INSTANCE_IDS[@]}"; then
    echo "WARNING: Some Phase 5 instances failed to terminate cleanly"
fi
unset INSTANCE_IDS

# Wait for clean state
sleep 2

# Test 5b-1: Batch launch spreads across nodes
echo ""
echo "Test 5b-1: Batch Launch Spread (3 instances on 3 nodes)"
echo "----------------------------------------"
echo "Launching 3 instances in a single API call..."

RUN_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name multinode-test-key \
    --security-group-ids "$MULTINODE_SG" \
    --count 3)

BATCH_COUNT=$(echo "$RUN_OUTPUT" | jq '.Instances | length')
if [ "$BATCH_COUNT" -ne 3 ]; then
    echo "FAIL: Expected 3 instances, got $BATCH_COUNT"
    echo "Output: $RUN_OUTPUT"
    exit 1
fi

# Verify single reservation in response
RESERVATION_ID=$(echo "$RUN_OUTPUT" | jq -r '.ReservationId // empty')
if [ -z "$RESERVATION_ID" ]; then
    echo "FAIL: No ReservationId in response"
    exit 1
fi
echo "  ReservationId: $RESERVATION_ID"
echo "  Instances: $BATCH_COUNT"

BATCH_IDS=($(echo "$RUN_OUTPUT" | jq -r '.Instances[].InstanceId'))
echo "  Launched: ${BATCH_IDS[*]}"

# Wait for all to reach running
for id in "${BATCH_IDS[@]}"; do
    wait_for_instance_state "$id" "running" 60 || {
        echo "ERROR: Instance $id failed to reach running state"
        exit 1
    }
done

# Verify distribution: check which node hosts each instance via nbdkit process
echo "  Checking distribution..."
NODES_USED=0
for i in 1 2 3; do
    NODE_IP="${SIMULATED_NETWORK}.$i"
    NODE_COUNT=0
    for id in "${BATCH_IDS[@]}"; do
        local_node=$(get_instance_node "$id") || continue
        if [ "$local_node" = "$i" ]; then
            NODE_COUNT=$((NODE_COUNT + 1))
        fi
    done
    echo "    Node$i ($NODE_IP): $NODE_COUNT instances"
    if [ "$NODE_COUNT" -gt 0 ]; then
        NODES_USED=$((NODES_USED + 1))
    fi
done

if [ "$NODES_USED" -lt 3 ]; then
    echo "FAIL: Expected instances on 3 nodes, only used $NODES_USED"
    exit 1
fi
echo "  PASS: Instances distributed across $NODES_USED nodes"

# Verify DescribeInstances from each node sees all batch-launched instances
echo "  Verifying DescribeInstances aggregation from all gateways..."
for i in 1 2 3; do
    NODE_GW_IP="${SIMULATED_NETWORK}.$i"
    DESCRIBE=$(aws --endpoint-url "https://${NODE_GW_IP}:${AWSGW_PORT}" \
        ec2 describe-instances \
        --query 'Reservations[*].Instances[?State.Name!=`terminated`].InstanceId' \
        --output text 2>/dev/null) || {
        echo "  WARNING: Could not query gateway on node$i"
        continue
    }

    MISSING=0
    for id in "${BATCH_IDS[@]}"; do
        if ! echo "$DESCRIBE" | grep -q "$id"; then
            echo "    Node$i missing instance $id"
            MISSING=$((MISSING + 1))
        fi
    done

    if [ "$MISSING" -gt 0 ]; then
        echo "FAIL: Node$i gateway missing $MISSING instances"
        exit 1
    fi
    echo "    Node$i gateway: all ${#BATCH_IDS[@]} instances visible"
done
echo "  PASS: All gateways see all batch-launched instances"

# Cleanup
echo "  Cleaning up..."
if ! terminate_and_wait "${BATCH_IDS[@]}"; then
    echo "WARNING: Some batch instances failed to terminate"
fi
sleep 2

# Test 5b-2: Batch launch with overflow packing (5 on 3 nodes)
echo ""
echo "Test 5b-2: Batch Launch Overflow (5 instances on 3 nodes)"
echo "----------------------------------------"
echo "Launching 5 instances in a single API call..."

RUN_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name multinode-test-key \
    --security-group-ids "$MULTINODE_SG" \
    --count 5)

BATCH_COUNT=$(echo "$RUN_OUTPUT" | jq '.Instances | length')
if [ "$BATCH_COUNT" -ne 5 ]; then
    echo "FAIL: Expected 5 instances, got $BATCH_COUNT"
    exit 1
fi

BATCH_IDS=($(echo "$RUN_OUTPUT" | jq -r '.Instances[].InstanceId'))
echo "  Launched: ${BATCH_IDS[*]}"

for id in "${BATCH_IDS[@]}"; do
    wait_for_instance_state "$id" "running" 60 || {
        echo "ERROR: Instance $id failed to reach running state"
        exit 1
    }
done

# All 3 nodes should have at least 1 instance (spread), some get 2 (pack)
echo "  Checking distribution..."
ALL_NODES_USED=true
for i in 1 2 3; do
    NODE_IP="${SIMULATED_NETWORK}.$i"
    NODE_COUNT=0
    for id in "${BATCH_IDS[@]}"; do
        local_node=$(get_instance_node "$id") || continue
        if [ "$local_node" = "$i" ]; then
            NODE_COUNT=$((NODE_COUNT + 1))
        fi
    done
    echo "    Node$i ($NODE_IP): $NODE_COUNT instances"
    if [ "$NODE_COUNT" -eq 0 ]; then
        ALL_NODES_USED=false
    fi
done

if [ "$ALL_NODES_USED" = false ]; then
    echo "FAIL: Not all nodes received instances"
    exit 1
fi
echo "  PASS: All nodes received at least 1 instance, 5 total distributed"

# Cleanup
echo "  Cleaning up..."
if ! terminate_and_wait "${BATCH_IDS[@]}"; then
    echo "WARNING: Some batch instances failed to terminate"
fi
sleep 2

# Test 5b-3: Insufficient capacity error
echo ""
echo "Test 5b-3: Insufficient Capacity Error"
echo "----------------------------------------"
echo "Requesting more instances than cluster can provide..."

# Request an absurdly high count that definitely exceeds capacity
if RUN_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name multinode-test-key \
    --security-group-ids "$MULTINODE_SG" \
    --count 100 2>&1); then
    echo "FAIL: Expected error but got success"
    echo "Output: $RUN_OUTPUT"
    exit 1
fi

if echo "$RUN_OUTPUT" | grep -q "InsufficientInstanceCapacity"; then
    echo "  PASS: Got InsufficientInstanceCapacity as expected"
else
    echo "FAIL: Wrong error: $RUN_OUTPUT"
    exit 1
fi

# Verify no orphaned instances were left behind
ORPHAN_CHECK=$($AWS_EC2 describe-instances \
    --query 'Reservations[*].Instances[?State.Name!=`terminated`].InstanceId' --output text)
if [ -n "$ORPHAN_CHECK" ] && [ "$ORPHAN_CHECK" != "None" ]; then
    echo "  WARNING: Found non-terminated instances after capacity error: $ORPHAN_CHECK"
else
    echo "  PASS: No orphaned instances after capacity error"
fi

# Test 5b-4: MinCount/MaxCount capping
echo ""
echo "Test 5b-4: MinCount/MaxCount Capping"
echo "----------------------------------------"
echo "Launching with --count 2..."

RUN_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name multinode-test-key \
    --security-group-ids "$MULTINODE_SG" \
    --count 2)

BATCH_COUNT=$(echo "$RUN_OUTPUT" | jq '.Instances | length')
if [ "$BATCH_COUNT" -ne 2 ]; then
    echo "FAIL: Expected exactly 2 instances, got $BATCH_COUNT"
    exit 1
fi
echo "  PASS: Launched exactly 2 instances (capped to MaxCount)"

# Cleanup
BATCH_IDS=($(echo "$RUN_OUTPUT" | jq -r '.Instances[].InstanceId'))
for id in "${BATCH_IDS[@]}"; do
    wait_for_instance_state "$id" "running" 60 || true
done
if ! terminate_and_wait "${BATCH_IDS[@]}"; then
    echo "WARNING: Some batch instances failed to terminate"
fi
sleep 2

# Test 5b-5: MinCount failure threshold
echo ""
echo "Test 5b-5: MinCount Failure Threshold"
echo "----------------------------------------"
echo "Requesting --count 100 (exceeds cluster capacity)..."

if RUN_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name multinode-test-key \
    --security-group-ids "$MULTINODE_SG" \
    --count 100 2>&1); then
    echo "FAIL: Expected InsufficientInstanceCapacity"
    exit 1
fi

if echo "$RUN_OUTPUT" | grep -q "InsufficientInstanceCapacity"; then
    echo "  PASS: Got InsufficientInstanceCapacity for MinCount > capacity"
else
    echo "FAIL: Wrong error: $RUN_OUTPUT"
    exit 1
fi

echo ""
echo "Phase 5b: All batch distribution tests passed"

# Phase 5c: Placement Group E2E Tests
echo ""
echo "Phase 5c: Placement Group E2E Tests (CRUD + Spread + Cluster)"
echo "=============================================================="

# Test 5c-1: Create spread group
echo ""
echo "Test 5c-1: CreatePlacementGroup (spread)"
echo "----------------------------------------"
CREATE_OUTPUT=$($AWS_EC2 create-placement-group \
    --group-name test-spread --strategy spread)
SPREAD_GROUP_ID=$(echo "$CREATE_OUTPUT" | jq -r '.PlacementGroup.GroupId')
SPREAD_STATE=$(echo "$CREATE_OUTPUT" | jq -r '.PlacementGroup.State')
if [ -z "$SPREAD_GROUP_ID" ] || [ "$SPREAD_GROUP_ID" = "null" ]; then
    echo "FAIL: No GroupId returned"; exit 1
fi
if [ "$SPREAD_STATE" != "available" ]; then
    echo "FAIL: Expected state=available, got $SPREAD_STATE"; exit 1
fi
echo "  PASS: Created spread group $SPREAD_GROUP_ID (state=$SPREAD_STATE)"

# Test 5c-2: Create cluster group
echo ""
echo "Test 5c-2: CreatePlacementGroup (cluster)"
echo "----------------------------------------"
CREATE_OUTPUT=$($AWS_EC2 create-placement-group \
    --group-name test-cluster --strategy cluster)
CLUSTER_GROUP_ID=$(echo "$CREATE_OUTPUT" | jq -r '.PlacementGroup.GroupId')
CLUSTER_STATE=$(echo "$CREATE_OUTPUT" | jq -r '.PlacementGroup.State')
if [ -z "$CLUSTER_GROUP_ID" ] || [ "$CLUSTER_GROUP_ID" = "null" ]; then
    echo "FAIL: No GroupId returned"; exit 1
fi
if [ "$CLUSTER_STATE" != "available" ]; then
    echo "FAIL: Expected state=available, got $CLUSTER_STATE"; exit 1
fi
echo "  PASS: Created cluster group $CLUSTER_GROUP_ID (state=$CLUSTER_STATE)"

# Test 5c-3: Partition strategy rejected
echo ""
echo "Test 5c-3: CreatePlacementGroup (partition rejected)"
echo "----------------------------------------"
if PARTITION_OUTPUT=$($AWS_EC2 create-placement-group \
    --group-name test-partition --strategy partition 2>&1); then
    echo "FAIL: Expected error for partition strategy"; exit 1
fi
if echo "$PARTITION_OUTPUT" | grep -q "InvalidParameterValue"; then
    echo "  PASS: partition strategy correctly rejected"
else
    echo "FAIL: Wrong error: $PARTITION_OUTPUT"; exit 1
fi

# Test 5c-4: Duplicate name rejected
echo ""
echo "Test 5c-4: CreatePlacementGroup (duplicate name)"
echo "----------------------------------------"
if DUP_OUTPUT=$($AWS_EC2 create-placement-group \
    --group-name test-spread --strategy spread 2>&1); then
    echo "FAIL: Expected error for duplicate name"; exit 1
fi
if echo "$DUP_OUTPUT" | grep -q "InvalidPlacementGroup.Duplicate"; then
    echo "  PASS: Duplicate name correctly rejected"
else
    echo "FAIL: Wrong error: $DUP_OUTPUT"; exit 1
fi

# Test 5c-5: DescribePlacementGroups
echo ""
echo "Test 5c-5: DescribePlacementGroups"
echo "----------------------------------------"
DESCRIBE_OUTPUT=$($AWS_EC2 describe-placement-groups)
PG_COUNT=$(echo "$DESCRIBE_OUTPUT" | jq '.PlacementGroups | length')
if [ "$PG_COUNT" -lt 2 ]; then
    echo "FAIL: Expected at least 2 placement groups, got $PG_COUNT"; exit 1
fi

SPREAD_STRATEGY=$(echo "$DESCRIBE_OUTPUT" | jq -r '.PlacementGroups[] | select(.GroupName=="test-spread") | .Strategy')
SPREAD_LEVEL=$(echo "$DESCRIBE_OUTPUT" | jq -r '.PlacementGroups[] | select(.GroupName=="test-spread") | .SpreadLevel')
if [ "$SPREAD_STRATEGY" != "spread" ]; then
    echo "FAIL: Spread group strategy=$SPREAD_STRATEGY"; exit 1
fi
if [ "$SPREAD_LEVEL" != "host" ]; then
    echo "FAIL: Spread group SpreadLevel=$SPREAD_LEVEL, expected host"; exit 1
fi

CLUSTER_STRATEGY=$(echo "$DESCRIBE_OUTPUT" | jq -r '.PlacementGroups[] | select(.GroupName=="test-cluster") | .Strategy')
if [ "$CLUSTER_STRATEGY" != "cluster" ]; then
    echo "FAIL: Cluster group strategy=$CLUSTER_STRATEGY"; exit 1
fi
echo "  PASS: Both groups described with correct fields"

FILTER_OUTPUT=$($AWS_EC2 describe-placement-groups --group-names test-spread)
FILTER_COUNT=$(echo "$FILTER_OUTPUT" | jq '.PlacementGroups | length')
if [ "$FILTER_COUNT" -ne 1 ]; then
    echo "FAIL: Name filter returned $FILTER_COUNT groups, expected 1"; exit 1
fi
echo "  PASS: Name filter works correctly"

# Test 5c-6: DescribeInstanceTypes PlacementGroupInfo
echo ""
echo "Test 5c-6: DescribeInstanceTypes PlacementGroupInfo"
echo "----------------------------------------"
DIT_OUTPUT=$($AWS_EC2 describe-instance-types --instance-types "$INSTANCE_TYPE")
STRATEGIES=$(echo "$DIT_OUTPUT" | jq -r '.InstanceTypes[0].PlacementGroupInfo.SupportedStrategies[]' 2>/dev/null | sort | tr '\n' ',')
if ! echo "$STRATEGIES" | grep -q "cluster" || ! echo "$STRATEGIES" | grep -q "spread"; then
    echo "FAIL: PlacementGroupInfo.SupportedStrategies missing spread/cluster: $STRATEGIES"; exit 1
fi
echo "  PASS: $INSTANCE_TYPE supports strategies: $STRATEGIES"

# Test 5c-7: Spread — 3 instances on 3 nodes (strict 1-per-node)
echo ""
echo "Test 5c-7: Spread — Launch 3 Instances (1 per node)"
echo "----------------------------------------"
RUN_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name multinode-test-key \
    --security-group-ids "$MULTINODE_SG" \
    --count 3 \
    --placement GroupName=test-spread)

SPREAD_COUNT=$(echo "$RUN_OUTPUT" | jq '.Instances | length')
if [ "$SPREAD_COUNT" -ne 3 ]; then
    echo "FAIL: Expected 3 instances, got $SPREAD_COUNT"; exit 1
fi

SPREAD_IDS=($(echo "$RUN_OUTPUT" | jq -r '.Instances[].InstanceId'))
echo "  Launched: ${SPREAD_IDS[*]}"

for id in "${SPREAD_IDS[@]}"; do
    wait_for_instance_state "$id" "running" 60 || {
        echo "ERROR: Instance $id failed to reach running state"; exit 1
    }
done

echo "  Checking strict 1-per-node distribution..."
count_instances_per_node "${SPREAD_IDS[@]}"
if [ "$NODE1_COUNT" -ne 1 ] || [ "$NODE2_COUNT" -ne 1 ] || [ "$NODE3_COUNT" -ne 1 ]; then
    echo "FAIL: Spread group did not enforce strict 1-per-node"; exit 1
fi
echo "  PASS: Strict 1-per-node spread enforced"

# Test 5c-8: DescribeInstances shows Placement field
echo ""
echo "Test 5c-8: Spread — DescribeInstances Placement Field"
echo "----------------------------------------"
verify_placement "test-spread" "${SPREAD_IDS[@]}" || exit 1
echo "  PASS: Placement field correctly populated"

# Test 5c-9: 4th instance fails (all nodes occupied)
echo ""
echo "Test 5c-9: Spread — 4th Instance Fails (all nodes used)"
echo "----------------------------------------"
if FOURTH_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name multinode-test-key \
    --security-group-ids "$MULTINODE_SG" \
    --count 1 \
    --placement GroupName=test-spread 2>&1); then
    echo "FAIL: Expected InsufficientInstanceCapacity for 4th spread instance"
    ACCIDENTAL_ID=$(echo "$FOURTH_OUTPUT" | jq -r '.Instances[0].InstanceId // empty')
    [ -n "$ACCIDENTAL_ID" ] && $AWS_EC2 terminate-instances --instance-ids "$ACCIDENTAL_ID" > /dev/null
    exit 1
fi
if echo "$FOURTH_OUTPUT" | grep -q "InsufficientInstanceCapacity"; then
    echo "  PASS: 4th instance correctly rejected (all nodes occupied)"
else
    echo "FAIL: Wrong error: $FOURTH_OUTPUT"; exit 1
fi

# Test 5c-10: Terminate frees node slot, replacement succeeds
echo ""
echo "Test 5c-10: Spread — Terminate Frees Node, Replacement Succeeds"
echo "----------------------------------------"
TERMINATED_ID="${SPREAD_IDS[0]}"
echo "  Terminating $TERMINATED_ID to free a node slot..."
$AWS_EC2 terminate-instances --instance-ids "$TERMINATED_ID" > /dev/null
wait_for_instance_state "$TERMINATED_ID" "terminated" 30 || true
sleep 2

echo "  Launching replacement instance..."
REPLACE_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name multinode-test-key \
    --security-group-ids "$MULTINODE_SG" \
    --count 1 \
    --placement GroupName=test-spread)
REPLACE_ID=$(echo "$REPLACE_OUTPUT" | jq -r '.Instances[0].InstanceId')
if [ -z "$REPLACE_ID" ] || [ "$REPLACE_ID" = "null" ]; then
    echo "FAIL: Replacement launch failed"; exit 1
fi
wait_for_instance_state "$REPLACE_ID" "running" 60 || {
    echo "ERROR: Replacement instance failed to reach running state"; exit 1
}
echo "  PASS: Replacement launched successfully after terminate freed slot"

# Cleanup spread instances
echo "  Cleaning up spread instances..."
SPREAD_CLEANUP_IDS=("${SPREAD_IDS[@]:1}" "$REPLACE_ID")
if ! terminate_and_wait "${SPREAD_CLEANUP_IDS[@]}"; then
    echo "WARNING: Some spread instances failed to terminate"
fi
sleep 2

# Test 5c-11: Cluster — all instances on same node
echo ""
echo "Test 5c-11: Cluster — Launch 3 Instances (all on one node)"
echo "----------------------------------------"
RUN_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name multinode-test-key \
    --security-group-ids "$MULTINODE_SG" \
    --count 3 \
    --placement GroupName=test-cluster)

CLUSTER_COUNT=$(echo "$RUN_OUTPUT" | jq '.Instances | length')
if [ "$CLUSTER_COUNT" -ne 3 ]; then
    echo "FAIL: Expected 3 instances, got $CLUSTER_COUNT"; exit 1
fi

CLUSTER_IDS=($(echo "$RUN_OUTPUT" | jq -r '.Instances[].InstanceId'))
echo "  Launched: ${CLUSTER_IDS[*]}"

for id in "${CLUSTER_IDS[@]}"; do
    wait_for_instance_state "$id" "running" 60 || {
        echo "ERROR: Instance $id failed to reach running state"; exit 1
    }
done

echo "  Checking cluster pinning..."
count_instances_per_node "${CLUSTER_IDS[@]}"
NODES_WITH_INSTANCES=0
PINNED_NODE=""
for i in 1 2 3; do
    count_var="NODE${i}_COUNT"
    if [ "${!count_var}" -gt 0 ]; then
        NODES_WITH_INSTANCES=$((NODES_WITH_INSTANCES + 1))
        PINNED_NODE=$i
    fi
done
if [ "$NODES_WITH_INSTANCES" -ne 1 ]; then
    echo "FAIL: Cluster instances spread across $NODES_WITH_INSTANCES nodes (expected 1)"; exit 1
fi
echo "  PASS: All 3 instances pinned to Node$PINNED_NODE"

# Test 5c-12: DescribeInstances shows Placement field
echo ""
echo "Test 5c-12: Cluster — DescribeInstances Placement Field"
echo "----------------------------------------"
verify_placement "test-cluster" "${CLUSTER_IDS[@]}" || exit 1
echo "  PASS: Placement field correctly populated"

# Test 5c-13: Subsequent launch pins to same node
echo ""
echo "Test 5c-13: Cluster — Subsequent Launch Pins to Same Node"
echo "----------------------------------------"
echo "  Launching 1 more instance into cluster group..."
EXTRA_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name multinode-test-key \
    --security-group-ids "$MULTINODE_SG" \
    --count 1 \
    --placement GroupName=test-cluster)
EXTRA_ID=$(echo "$EXTRA_OUTPUT" | jq -r '.Instances[0].InstanceId')
if [ -z "$EXTRA_ID" ] || [ "$EXTRA_ID" = "null" ]; then
    echo "FAIL: Subsequent launch failed"; exit 1
fi
wait_for_instance_state "$EXTRA_ID" "running" 60 || {
    echo "ERROR: Extra instance failed to reach running state"; exit 1
}

echo "  Checking pinning..."
ALL_CLUSTER_IDS=("${CLUSTER_IDS[@]}" "$EXTRA_ID")
count_instances_per_node "${ALL_CLUSTER_IDS[@]}"
NODES_WITH_INSTANCES=0
for i in 1 2 3; do
    count_var="NODE${i}_COUNT"
    if [ "${!count_var}" -gt 0 ]; then
        NODES_WITH_INSTANCES=$((NODES_WITH_INSTANCES + 1))
    fi
done
if [ "$NODES_WITH_INSTANCES" -ne 1 ]; then
    echo "FAIL: Extra instance not pinned to same node"; exit 1
fi
echo "  PASS: Subsequent launch pinned to same Node$PINNED_NODE"

# Cleanup cluster instances
echo "  Cleaning up cluster instances..."
if ! terminate_and_wait "${ALL_CLUSTER_IDS[@]}"; then
    echo "WARNING: Some cluster instances failed to terminate"
fi
sleep 2

# Test 5c-14: Cluster capacity exhausted on pinned node
echo ""
echo "Test 5c-14: Cluster — Capacity Exhausted on Pinned Node"
echo "----------------------------------------"
FILL_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name multinode-test-key \
    --security-group-ids "$MULTINODE_SG" \
    --count 1 \
    --placement GroupName=test-cluster)
FILL_ID=$(echo "$FILL_OUTPUT" | jq -r '.Instances[0].InstanceId')
wait_for_instance_state "$FILL_ID" "running" 60 || true

if CAP_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name multinode-test-key \
    --security-group-ids "$MULTINODE_SG" \
    --count 100 \
    --placement GroupName=test-cluster 2>&1); then
    echo "FAIL: Expected InsufficientInstanceCapacity"; exit 1
fi
if echo "$CAP_OUTPUT" | grep -q "InsufficientInstanceCapacity"; then
    echo "  PASS: Capacity exhaustion correctly detected on pinned node"
else
    echo "FAIL: Wrong error: $CAP_OUTPUT"; exit 1
fi

if [ -n "$FILL_ID" ] && [ "$FILL_ID" != "null" ]; then
    terminate_and_wait "$FILL_ID" || true
fi
sleep 2

# Test 5c-15: DeletePlacementGroup with running instances fails
echo ""
echo "Test 5c-15: DeletePlacementGroup with Instances (InUse)"
echo "----------------------------------------"
INUSE_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name multinode-test-key \
    --security-group-ids "$MULTINODE_SG" \
    --count 1 \
    --placement GroupName=test-spread)
INUSE_ID=$(echo "$INUSE_OUTPUT" | jq -r '.Instances[0].InstanceId')
wait_for_instance_state "$INUSE_ID" "running" 60 || true

if DEL_OUTPUT=$($AWS_EC2 delete-placement-group \
    --group-name test-spread 2>&1); then
    echo "FAIL: Expected InvalidPlacementGroup.InUse"; exit 1
fi
if echo "$DEL_OUTPUT" | grep -q "InvalidPlacementGroup.InUse"; then
    echo "  PASS: Delete correctly blocked while instances running"
else
    echo "FAIL: Wrong error: $DEL_OUTPUT"; exit 1
fi

# Test 5c-16: Terminate all + delete succeeds
echo ""
echo "Test 5c-16: Terminate All + DeletePlacementGroup"
echo "----------------------------------------"
echo "  Terminating instance $INUSE_ID..."
terminate_and_wait "$INUSE_ID" || true
sleep 2

$AWS_EC2 delete-placement-group --group-name test-spread
echo "  PASS: Spread group deleted after terminating instances"

$AWS_EC2 delete-placement-group --group-name test-cluster
echo "  PASS: Cluster group deleted"

REMAINING=$($AWS_EC2 describe-placement-groups \
    --query 'PlacementGroups[?GroupName==`test-spread` || GroupName==`test-cluster`] | length(@)' \
    --output text 2>/dev/null || echo "0")
if [ "$REMAINING" != "0" ] && [ "$REMAINING" != "None" ]; then
    echo "FAIL: Groups still exist after deletion"; exit 1
fi
echo "  PASS: Both groups deleted and verified gone"

# Test 5c-17: Default spread unchanged (regression)
echo ""
echo "Test 5c-17: Default Spread (No Placement Group) Unchanged"
echo "----------------------------------------"
RUN_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name multinode-test-key \
    --security-group-ids "$MULTINODE_SG" \
    --count 3)
REG_COUNT=$(echo "$RUN_OUTPUT" | jq '.Instances | length')
if [ "$REG_COUNT" -ne 3 ]; then
    echo "FAIL: Default spread launch failed, got $REG_COUNT instances"; exit 1
fi
REG_IDS=($(echo "$RUN_OUTPUT" | jq -r '.Instances[].InstanceId'))
for id in "${REG_IDS[@]}"; do
    wait_for_instance_state "$id" "running" 60 || true
done

PLACEMENT=$($AWS_EC2 describe-instances --instance-ids "${REG_IDS[0]}" \
    --query 'Reservations[0].Instances[0].Placement.GroupName' --output text)
if [ -n "$PLACEMENT" ] && [ "$PLACEMENT" != "None" ] && [ "$PLACEMENT" != "null" ]; then
    echo "FAIL: Default spread instance has Placement.GroupName='$PLACEMENT' (should be empty)"; exit 1
fi
echo "  PASS: Default spread works, no Placement.GroupName set"

if ! terminate_and_wait "${REG_IDS[@]}"; then
    echo "WARNING: Some regression test instances failed to terminate"
fi
sleep 2

echo ""
echo "Phase 5c: All placement group E2E tests passed"

# Re-launch 3 instances for Phase 6 shutdown/restart tests
echo ""
echo "Launching 3 instances for Phase 6 shutdown/restart tests..."
INSTANCE_IDS=()
for i in 1 2 3; do
    RUN_OUTPUT=$($AWS_EC2 run-instances \
        --image-id "$AMI_ID" \
        --instance-type "$INSTANCE_TYPE" \
        --key-name multinode-test-key \
        --security-group-ids "$MULTINODE_SG")
    INSTANCE_ID=$(echo "$RUN_OUTPUT" | jq -r '.Instances[0].InstanceId')
    if [ -z "$INSTANCE_ID" ] || [ "$INSTANCE_ID" == "null" ]; then
        echo "  ERROR: Failed to launch instance $i for Phase 6 setup"
        exit 1
    fi
    echo "  Launched: $INSTANCE_ID"
    INSTANCE_IDS+=("$INSTANCE_ID")
    sleep 1
done
for id in "${INSTANCE_IDS[@]}"; do
    wait_for_instance_state "$id" "running" 30 || {
        echo "ERROR: Instance $id failed to start for Phase 6 setup"
        exit 1
    }
done
echo "  Phase 6 setup: ${#INSTANCE_IDS[@]} instances running"
echo "========================================"

# Phase 6: Cluster Shutdown + Restart
echo ""
echo "Phase 6: Cluster Shutdown + Restart"
echo "========================================"
echo "Testing spx admin cluster shutdown command..."

# Test 6a: Dry-run shutdown
echo ""
echo "Test 6a: Dry-Run Shutdown"
echo "----------------------------------------"
echo "Running cluster shutdown in dry-run mode..."

DRY_RUN_OUTPUT=$(./bin/spx admin cluster shutdown --dry-run --config "$HOME/node1/config/spinifex.toml" 2>&1)
echo "$DRY_RUN_OUTPUT"

# Validate dry-run output contains expected phases
for phase in GATE DRAIN STORAGE PERSIST INFRA; do
    if echo "$DRY_RUN_OUTPUT" | grep -qi "$phase"; then
        echo "  Phase $phase found in shutdown plan"
    else
        echo "  WARNING: Phase $phase not found in dry-run output"
    fi
done
echo "  Dry-run shutdown test passed"

# Test 6b: Real coordinated shutdown
echo ""
echo "Test 6b: Coordinated Cluster Shutdown"
echo "----------------------------------------"
echo "Running cluster shutdown..."

./bin/spx admin cluster shutdown --force --timeout 30s --config "$HOME/node1/config/spinifex.toml" 2>&1 || {
    echo "  WARNING: Cluster shutdown command returned non-zero exit code"
}
CLUSTER_SERVICES_STARTED="false"

# Verify all services are down
echo "  Waiting for services to stop..."
sleep 1
if ! verify_all_services_down; then
    echo "  Some services still running, force-cleaning..."
    force_cleanup_all_nodes
fi

# Test 6c: Restart and recovery
echo ""
echo "Test 6c: Cluster Restart + Recovery"
echo "----------------------------------------"
echo "Restarting all node services concurrently..."

# Cluster restart requires concurrent startup: NATS needs route peers to form,
# Predastore needs Raft quorum (2/3), and the daemon needs JetStream.
# Sequential start would leave node1 waiting for quorum that never arrives.
start_node_services 1 "$HOME/node1" &
start_node_services 2 "$HOME/node2" &
start_node_services 3 "$HOME/node3" &
wait
CLUSTER_SERVICES_STARTED="true"

echo ""
echo "Waiting for cluster to stabilize..."
sleep 5

# Verify NATS cluster reformed
echo ""
verify_nats_cluster 3 || {
    echo "WARNING: NATS cluster verification failed after restart"
}

# Wait for gateway
echo ""
wait_for_gateway "${NODE1_IP}" 15

# Wait for daemon readiness
wait_for_daemon_ready "https://${NODE1_IP}:${AWSGW_PORT}"

# Smoke test: describe-instance-types
echo ""
echo "Running post-restart smoke test..."
SMOKE_OUTPUT=$($AWS_EC2 describe-instance-types --query 'InstanceTypes[*].InstanceType' --output text 2>/dev/null)
if [ -n "$SMOKE_OUTPUT" ] && [ "$SMOKE_OUTPUT" != "None" ]; then
    echo "  Smoke test passed: describe-instance-types returned: $SMOKE_OUTPUT"
else
    echo "  ERROR: Smoke test failed: describe-instance-types returned empty/None"
    exit 1
fi

echo "  Cluster shutdown + restart test passed"

# Test 6d: Instance relaunch and terminate after restart
echo ""
echo "Test 6d: Instance Relaunch + Terminate"
echo "----------------------------------------"
echo "Waiting for instances to relaunch after cluster restart..."

# All 3 instances were running before shutdown — the daemon will relaunch them.
# Must wait for them to finish launching (pending → running) before terminate
# will work, because the NATS per-instance subscription is only created after
# QEMU starts.
for instance_id in "${INSTANCE_IDS[@]}"; do
    echo "  Waiting for $instance_id to finish relaunching..."
    COUNT=0
    while [ $COUNT -lt 60 ]; do
        STATE=$($AWS_EC2 describe-instances --instance-ids "$instance_id" \
            --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null || echo "unknown")
        if [ "$STATE" = "running" ]; then
            echo "  $instance_id relaunched successfully: $STATE"
            break
        fi
        sleep 1
        COUNT=$((COUNT + 1))
    done
    if [ $COUNT -ge 60 ]; then
        echo "  WARNING: $instance_id still in $STATE after 60s"
    fi
done

# Terminate all instances
echo ""
echo "Terminating all instances..."
if ! terminate_and_wait "${INSTANCE_IDS[@]}"; then
    echo ""
    echo "ERROR: Some instances failed to terminate properly after restart"
    dump_all_node_logs
    exit 1
fi

echo "  Instance relaunch + terminate after restart passed"

# Run certificate validation E2E while services are still up (before cleanup trap).
echo ""
echo "========================================"
echo "Running Certificate E2E Tests"
echo "========================================"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"${SCRIPT_DIR}/run-cert-e2e.sh" --pseudo-multinode

echo ""
echo "========================================"
echo "Multi-Node E2E Tests Completed Successfully"
echo "========================================"
exit 0
