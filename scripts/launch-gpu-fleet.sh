#!/bin/bash
# launch-gpu-fleet.sh — Terminate all running instances, launch a GPU fleet, provision
# MI350X drivers and ROCm tooling in each guest, and verify compute readiness.
#
# Fleet composition (fixed):
#   6 × g7e.4xlarge  (single MI350X each)
#   1 × g7e.12xlarge (two MI350X GPUs)
#
# Usage:
#   scripts/launch-gpu-fleet.sh
#
# Env overrides:
#   SSH_KEY            Path to SSH private key (default: ~/.ssh/spinifex-key)
#   SSH_USER           SSH user inside guest  (default: ec2-user)
#   SSH_TIMEOUT        Seconds to wait for initial SSH per instance (default: 300)
#   REBOOT_TIMEOUT     Seconds to wait for SSH after each reboot    (default: 300)
set -euo pipefail

export AWS_PROFILE=spinifex

SSH_KEY="${SSH_KEY:-$HOME/.ssh/spinifex-key}"
SSH_USER="${SSH_USER:-ec2-user}"
SSH_TIMEOUT="${SSH_TIMEOUT:-300}"
REBOOT_TIMEOUT="${REBOOT_TIMEOUT:-300}"

# Fleet composition: array of "type:count" pairs (order matters for display)
FLEET_SPEC=("g7e.4xlarge:6" "g7e.12xlarge:1")
FLEET_TOTAL=7

# --- Helpers ---

_ssh() {
    local ip="$1"; shift
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 -o BatchMode=yes \
        -i "$SSH_KEY" "${SSH_USER}@${ip}" "$@"
}

wait_ssh() {
    local ip="$1" timeout="$2" elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        _ssh "$ip" 'echo ready' >/dev/null 2>&1 && return 0
        sleep 5; elapsed=$((elapsed + 5))
    done
    return 1
}

# provision_instance installs MI350X firmware and ROCm tooling, rebooting twice,
# then verifies compute readiness. Writes "PASS/FAIL <id> <ip> [reason]" to result_file.
provision_instance() {
    local id="$1" ip="$2" result_file="$3"

    # Wait for initial boot
    if ! wait_ssh "$ip" "$SSH_TIMEOUT"; then
        echo "FAIL $id $ip SSH_TIMEOUT_INITIAL" > "$result_file"; return
    fi

    # Phase 1: firmware + initramfs rebuild → reboot
    if ! _ssh "$ip" '
        sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq &&
        sudo DEBIAN_FRONTEND=noninteractive apt-get install -y linux-firmware &&
        sudo update-initramfs -u -k all
    '; then
        echo "FAIL $id $ip FIRMWARE_INSTALL" > "$result_file"; return
    fi
    _ssh "$ip" 'sudo reboot' || true
    sleep 20
    if ! wait_ssh "$ip" "$REBOOT_TIMEOUT"; then
        echo "FAIL $id $ip REBOOT1_TIMEOUT" > "$result_file"; return
    fi

    # Phase 2: ROCm userland + group membership → reboot
    if ! _ssh "$ip" '
        sudo DEBIAN_FRONTEND=noninteractive apt-get install -y rocminfo rocm-smi &&
        sudo usermod -aG render,video ec2-user
    '; then
        echo "FAIL $id $ip ROCM_INSTALL" > "$result_file"; return
    fi
    _ssh "$ip" 'sudo reboot' || true
    sleep 20
    if ! wait_ssh "$ip" "$REBOOT_TIMEOUT"; then
        echo "FAIL $id $ip REBOOT2_TIMEOUT" > "$result_file"; return
    fi

    # Phase 3: amd-smi (best-effort) + verify compute readiness
    _ssh "$ip" 'sudo DEBIAN_FRONTEND=noninteractive apt-get install -y amd-smi' || true

    if ! _ssh "$ip" 'rocminfo 2>/dev/null | grep -q "Device Type"'; then
        echo "FAIL $id $ip ROCM_NOT_READY" > "$result_file"; return
    fi

    echo "PASS $id $ip" > "$result_file"
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

# --- Step 4: Confirm all fleet instance types are advertised ---
echo "==> Checking fleet instance types are available"
for spec in "${FLEET_SPEC[@]}"; do
    itype="${spec%%:*}"
    TYPE_INFO=$(aws ec2 describe-instance-types \
        --query "InstanceTypes[?InstanceType=='${itype}'].InstanceType | [0]" \
        --output text 2>/dev/null || true)
    if [ -z "$TYPE_INFO" ] || [ "$TYPE_INFO" = "None" ]; then
        echo "❌ Instance type '${itype}' not advertised by this node"
        echo "   Available GPU types:"
        aws ec2 describe-instance-types \
            --query "InstanceTypes[?GpuInfo!=null].InstanceType" --output text 2>/dev/null || true
        exit 1
    fi
    echo "   ${itype} available"
done

# --- Step 5: Ensure SSH key exists ---
if [ ! -f "$SSH_KEY" ]; then
    echo "==> Generating SSH key"
    mkdir -p "$(dirname "$SSH_KEY")"
    ssh-keygen -t ed25519 -f "$SSH_KEY" -N ""
fi
! aws ec2 import-key-pair --key-name spinifex-key \
    --public-key-material "fileb://${SSH_KEY}.pub" >/dev/null 2>&1 || true

# --- Step 6: Launch fleet ---
echo "==> Launching fleet: 6× g7e.4xlarge + 1× g7e.12xlarge"
INSTANCE_IDS=()
declare -A INSTANCE_TYPE_MAP
slot=0
for spec in "${FLEET_SPEC[@]}"; do
    itype="${spec%%:*}"
    count="${spec##*:}"
    for i in $(seq 1 "$count"); do
        slot=$((slot + 1))
        ID=$(aws ec2 run-instances \
            --image-id "$AMI_ID" \
            --instance-type "$itype" \
            --key-name spinifex-key \
            --subnet-id "$SUBNET_ID" \
            --count 1 \
            --block-device-mappings '[{"DeviceName":"/dev/sda1","Ebs":{"VolumeSize":50,"VolumeType":"gp3","DeleteOnTermination":true}}]' \
            --query 'Instances[0].InstanceId' --output text)
        if [ -z "$ID" ] || [ "$ID" = "None" ] || [ "$ID" = "null" ]; then
            echo "❌ run-instances returned no InstanceId for ${itype} slot ${i}"
            exit 1
        fi
        echo "   [$slot/$FLEET_TOTAL] $ID ($itype) launched"
        INSTANCE_IDS+=("$ID")
        INSTANCE_TYPE_MAP["$ID"]="$itype"
    done
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
    echo "   $id (${INSTANCE_TYPE_MAP[$id]}) running"
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
    echo "   $id (${INSTANCE_TYPE_MAP[$id]}) → $IP"
done

# --- Step 9: Provision all instances in parallel ---
echo "==> Provisioning GPU drivers and ROCm in all instances (parallel, 2 reboots each)"
TMPDIR_RESULTS=$(mktemp -d)
PIDS=()
for id in "${INSTANCE_IDS[@]}"; do
    ip="${INSTANCE_IPS[$id]}"
    result_file="${TMPDIR_RESULTS}/${id}"
    provision_instance "$id" "$ip" "$result_file" &
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
    itype="${INSTANCE_TYPE_MAP[$id]}"
    ip="${INSTANCE_IPS[$id]}"
    if [ -f "$result_file" ]; then
        result=$(cat "$result_file")
        status=$(echo "$result" | awk '{print $1}')
        if [ "$status" = "PASS" ]; then
            echo "   ✅ $id ($itype, $ip) — GPU drivers installed, ROCm ready"
            PASS=$((PASS + 1))
        else
            reason=$(echo "$result" | awk '{print $4}')
            echo "   ❌ $id ($itype, $ip) — FAILED: $reason"
            FAIL=$((FAIL + 1))
        fi
    else
        echo "   ❌ $id ($itype) — no result (provision job lost)"
        FAIL=$((FAIL + 1))
    fi
done
rm -rf "$TMPDIR_RESULTS"

echo ""
echo "   ${PASS}/${FLEET_TOTAL} instances passed"
if [ "$FAIL" -gt 0 ]; then
    echo "❌ Fleet launch FAILED — $FAIL instance(s) did not complete provisioning"
    exit 1
fi
echo "✅ Fleet ready — 6× g7e.4xlarge + 1× g7e.12xlarge provisioned with MI350X drivers and ROCm"
