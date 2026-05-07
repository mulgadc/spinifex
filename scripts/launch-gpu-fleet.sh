#!/bin/bash
# launch-gpu-fleet.sh — Terminate all running instances, then launch 8 g7e instances
# with GPU passthrough and verify GPU visibility inside each guest.
#
# Usage:
#   scripts/launch-gpu-fleet.sh
#
# Env overrides:
#   GPU_FAMILY         Instance family prefix (default: g7e)
#   INSTANCE_SIZE      Instance size suffix   (default: 2xlarge)
#   FLEET_SIZE         Number of instances    (default: 8)
#   SSH_KEY            Path to SSH private key (default: ~/.ssh/spinifex-key)
#   SSH_USER           SSH user inside guest  (default: ec2-user)
#   SSH_TIMEOUT        Seconds to wait for SSH per instance (default: 300)
set -euo pipefail

export AWS_PROFILE=spinifex

GPU_FAMILY="${GPU_FAMILY:-g7e}"
INSTANCE_SIZE="${INSTANCE_SIZE:-2xlarge}"
FLEET_SIZE="${FLEET_SIZE:-8}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/spinifex-key}"
SSH_USER="${SSH_USER:-ec2-user}"
SSH_TIMEOUT="${SSH_TIMEOUT:-300}"
INSTANCE_TYPE="${GPU_FAMILY}.${INSTANCE_SIZE}"

# --- Helper: wait for SSH and verify GPU in guest ---
# Writes result to a temp file: "PASS <id> <ip>" or "FAIL <id> <ip> <reason>"
verify_instance() {
    local id="$1"
    local ip="$2"
    local result_file="$3"

    local ssh_opts="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o BatchMode=yes"

    # Wait for SSH
    local elapsed=0
    while [ "$elapsed" -lt "$SSH_TIMEOUT" ]; do
        if ssh $ssh_opts -i "$SSH_KEY" "${SSH_USER}@${ip}" 'echo ready' >/dev/null 2>&1; then
            break
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    if [ "$elapsed" -ge "$SSH_TIMEOUT" ]; then
        echo "FAIL $id $ip SSH_TIMEOUT" > "$result_file"
        return
    fi

    # Verify GPU visible in guest
    if ssh $ssh_opts -i "$SSH_KEY" "${SSH_USER}@${ip}" \
        'lspci 2>/dev/null | grep -i -E "amd|nvidia|display|3d|vga"' >/dev/null 2>&1; then
        echo "PASS $id $ip" > "$result_file"
    else
        echo "FAIL $id $ip NO_GPU_IN_GUEST" > "$result_file"
    fi
}

# --- Step 1: Terminate all running instances ---
echo "==> Terminating all running instances"
RUNNING_IDS=$(aws ec2 describe-instances \
    --query "Reservations[].Instances[?State.Name=='running'].InstanceId" \
    --output text 2>/dev/null || true)

if [ -n "$RUNNING_IDS" ] && [ "$RUNNING_IDS" != "None" ]; then
    echo "   Terminating: $RUNNING_IDS"
    # shellcheck disable=SC2086
    aws ec2 terminate-instances --instance-ids $RUNNING_IDS --output text >/dev/null
    echo "   Waiting for termination..."
    # shellcheck disable=SC2086
    for id in $RUNNING_IDS; do
        COUNT=0
        while [ $COUNT -lt 60 ]; do
            STATE=$(aws ec2 describe-instances --instance-ids "$id" \
                --query "Reservations[0].Instances[0].State.Name" --output text 2>/dev/null || echo "gone")
            [ "$STATE" = "terminated" ] || [ "$STATE" = "gone" ] && break
            sleep 2; COUNT=$((COUNT + 2))
        done
    done
    echo "   All previous instances terminated"
else
    echo "   No running instances to terminate"
fi

# --- Step 2: Resolve AMI ---
echo "==> Resolving AMI"
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)        IMAGE_NAME="ami-ubuntu-26.04-x86_64" ;;
    aarch64|arm64) IMAGE_NAME="ami-ubuntu-26.04-arm64" ;;
    *)             IMAGE_NAME="ami-ubuntu-26.04-x86_64" ;;
esac

AMI_ID=$(aws ec2 describe-images \
    --query "Images[?Name=='${IMAGE_NAME}'].ImageId | [0]" --output text 2>/dev/null || true)
if [ -z "$AMI_ID" ] || [ "$AMI_ID" = "None" ]; then
    echo "❌ No AMI found with name '${IMAGE_NAME}'"
    echo "   Available AMIs:"
    aws ec2 describe-images --query "Images[*].[Name,ImageId]" --output table
    exit 1
fi
echo "   AMI: $AMI_ID ($IMAGE_NAME)"

# --- Step 3: Resolve subnet ---
echo "==> Resolving subnet"
SUBNET_ID=$(aws ec2 describe-subnets \
    --query 'Subnets[?MapPublicIpOnLaunch==`true`].SubnetId | [0]' --output text 2>/dev/null || true)
if [ -z "$SUBNET_ID" ] || [ "$SUBNET_ID" = "None" ]; then
    echo "❌ No public subnet found"
    exit 1
fi
echo "   Subnet: $SUBNET_ID"

# --- Step 4: Confirm GPU instance type is advertised ---
echo "==> Checking ${INSTANCE_TYPE} is available"
TYPE_INFO=$(aws ec2 describe-instance-types \
    --query "InstanceTypes[?InstanceType=='${INSTANCE_TYPE}'].InstanceType | [0]" \
    --output text 2>/dev/null || true)
if [ -z "$TYPE_INFO" ] || [ "$TYPE_INFO" = "None" ]; then
    echo "❌ Instance type '${INSTANCE_TYPE}' not advertised by this node"
    echo "   Available GPU types:"
    aws ec2 describe-instance-types \
        --query "InstanceTypes[?GpuInfo!=null].InstanceType" --output text 2>/dev/null || true
    exit 1
fi
echo "   ${INSTANCE_TYPE} available"

# --- Step 5: Ensure SSH key exists ---
if [ ! -f "$SSH_KEY" ]; then
    echo "==> Generating SSH key"
    mkdir -p "$(dirname "$SSH_KEY")"
    ssh-keygen -t ed25519 -f "$SSH_KEY" -N ""
fi
! aws ec2 import-key-pair --key-name spinifex-key \
    --public-key-material "fileb://${SSH_KEY}.pub" >/dev/null 2>&1 || true

# --- Step 6: Launch fleet ---
echo "==> Launching ${FLEET_SIZE}x ${INSTANCE_TYPE} instances"
INSTANCE_IDS=()
for i in $(seq 1 "$FLEET_SIZE"); do
    ID=$(aws ec2 run-instances \
        --image-id "$AMI_ID" \
        --instance-type "$INSTANCE_TYPE" \
        --key-name spinifex-key \
        --subnet-id "$SUBNET_ID" \
        --count 1 \
        --query 'Instances[0].InstanceId' --output text)
    if [ -z "$ID" ] || [ "$ID" = "None" ] || [ "$ID" = "null" ]; then
        echo "❌ run-instances returned no InstanceId for slot $i"
        exit 1
    fi
    echo "   [$i/$FLEET_SIZE] $ID launched"
    INSTANCE_IDS+=("$ID")
done

# --- Step 7: Wait for all to reach running state ---
echo "==> Waiting for all instances to reach running state"
for id in "${INSTANCE_IDS[@]}"; do
    COUNT=0
    STATE="unknown"
    while [ $COUNT -lt 120 ]; do
        STATE=$(aws ec2 describe-instances --instance-ids "$id" \
            --query "Reservations[0].Instances[0].State.Name" --output text 2>/dev/null || echo "unknown")
        [ "$STATE" = "running" ] && break
        if [ "$STATE" = "terminated" ]; then
            echo "❌ Instance $id terminated unexpectedly"
            exit 1
        fi
        sleep 2; COUNT=$((COUNT + 2))
    done
    if [ "$STATE" != "running" ]; then
        echo "❌ Instance $id failed to reach running state (last: $STATE)"
        exit 1
    fi
    echo "   $id running"
done

# --- Step 8: Wait for public IPs ---
echo "==> Waiting for public IP assignment"
declare -A INSTANCE_IPS
for id in "${INSTANCE_IDS[@]}"; do
    IP=""
    for _i in $(seq 1 120); do
        IP=$(aws ec2 describe-instances --instance-ids "$id" \
            --query 'Reservations[0].Instances[0].PublicIpAddress' --output text 2>/dev/null || true)
        [ -n "$IP" ] && [ "$IP" != "None" ] && [ "$IP" != "null" ] && break
        sleep 1
    done
    if [ -z "$IP" ] || [ "$IP" = "None" ]; then
        echo "❌ No public IP for $id after 120s"
        exit 1
    fi
    INSTANCE_IPS["$id"]="$IP"
    echo "   $id → $IP"
done

# --- Step 9: Verify SSH and GPU in parallel ---
echo "==> Verifying SSH and GPU visibility in all instances (parallel)"
TMPDIR_RESULTS=$(mktemp -d)
PIDS=()
for id in "${INSTANCE_IDS[@]}"; do
    ip="${INSTANCE_IPS[$id]}"
    result_file="${TMPDIR_RESULTS}/${id}"
    verify_instance "$id" "$ip" "$result_file" &
    PIDS+=($!)
done

# Wait for all background jobs
for pid in "${PIDS[@]}"; do
    wait "$pid" || true
done

# --- Step 10: Report results ---
echo ""
echo "==> Results"
PASS=0
FAIL=0
for id in "${INSTANCE_IDS[@]}"; do
    result_file="${TMPDIR_RESULTS}/${id}"
    if [ -f "$result_file" ]; then
        result=$(cat "$result_file")
        status=$(echo "$result" | awk '{print $1}')
        ip="${INSTANCE_IPS[$id]}"
        if [ "$status" = "PASS" ]; then
            echo "   ✅ $id ($ip) — SSH ready, GPU visible"
            PASS=$((PASS + 1))
        else
            reason=$(echo "$result" | awk '{print $4}')
            echo "   ❌ $id ($ip) — FAILED: $reason"
            FAIL=$((FAIL + 1))
        fi
    else
        echo "   ❌ $id — no result (verify job lost)"
        FAIL=$((FAIL + 1))
    fi
done
rm -rf "$TMPDIR_RESULTS"

echo ""
echo "   ${PASS}/${FLEET_SIZE} instances passed"
if [ "$FAIL" -gt 0 ]; then
    echo "❌ Fleet launch FAILED — $FAIL instance(s) did not pass verification"
    exit 1
fi
echo "✅ Fleet launch PASSED — all ${FLEET_SIZE} ${INSTANCE_TYPE} instances running with GPU visible"
