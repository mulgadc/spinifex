#!/bin/bash
set -euo pipefail

export AWS_PROFILE=spinifex

# Resolve the actual user's home — $HOME is /root when run under sudo.
_USER_HOME=$(getent passwd "${SUDO_USER:-$(id -un)}" | cut -d: -f6)
SSH_KEY="${SSH_KEY:-${_USER_HOME}/.ssh/spinifex-key}"
SSH_USER="${SSH_USER:-ubuntu}"
SSH_TIMEOUT="${SSH_TIMEOUT:-300}"

usage() {
    cat <<'EOF'
launch-gpu.sh — Launch a GPU instance from the Spinifex AMD or NVIDIA GPU AMI.

Detects the GPU brand from the specified instance type and selects the matching
pre-built AMI automatically. Use --amd or --nvidia to override.

Usage:
  scripts/launch-gpu.sh --type <instance-type> [--disk GB] [--amd|--nvidia]

Options:
  --type TYPE    EC2 instance type to launch (required)
  --disk GB      Root disk size in GB (default: 50)
  --ami  NAME    Override the AMI name to use (default: ubuntu-amd-gpu or ubuntu-26.04-nvidia-gpu-x86_64)
  --amd          Force AMD GPU AMI  (ubuntu-amd-gpu)
  --nvidia       Force NVIDIA GPU AMI (ubuntu-26.04-nvidia-gpu-x86_64)

Env overrides:
  SSH_KEY      Path to SSH private key (default: ~/.ssh/spinifex-key)
  SSH_USER     SSH user inside guest  (default: ubuntu)
  SSH_TIMEOUT  Seconds to wait for initial SSH (default: 300)
EOF
}

INSTANCE_TYPE=""
DISK_GB=50
GPU_BRAND=""
AMI_OVERRIDE=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --type)   INSTANCE_TYPE="$2"; shift 2 ;;
        --disk)   DISK_GB="$2";        shift 2 ;;
        --ami)    AMI_OVERRIDE="$2";   shift 2 ;;
        --amd)    GPU_BRAND="amd";     shift ;;
        --nvidia) GPU_BRAND="nvidia";  shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "Unknown option: $1"; usage; exit 1 ;;
    esac
done

list_gpu_types() {
    aws ec2 describe-instance-types \
        --query 'InstanceTypes[?GpuInfo].[InstanceType,GpuInfo.Gpus[0].Count,GpuInfo.Gpus[0].Manufacturer,GpuInfo.Gpus[0].Name]' \
        --output table 2>/dev/null || true
}

if [ -z "$INSTANCE_TYPE" ]; then
    echo "Error: --type is required."
    echo ""
    echo "Available GPU instance types on this node:"
    list_gpu_types
    exit 1
fi

# --- Validate instance type and fetch GPU info in one call ---
echo "==> Checking instance type: ${INSTANCE_TYPE}"
INSTANCE_INFO=$(aws ec2 describe-instance-types \
    --query "InstanceTypes[?InstanceType=='${INSTANCE_TYPE}' && GpuInfo].[GpuInfo.Gpus[0].Count,GpuInfo.Gpus[0].Manufacturer,GpuInfo.Gpus[0].Name]" \
    --output text 2>/dev/null || true)

if [ -z "$INSTANCE_INFO" ] || [ "$INSTANCE_INFO" = "None" ]; then
    echo "'${INSTANCE_TYPE}' is not available or is not a GPU type on this node."
    echo ""
    echo "Available GPU instance types:"
    list_gpu_types
    exit 1
fi

IFS=$'\t' read -r GPU_COUNT GPU_MANUFACTURER GPU_NAME <<< "$INSTANCE_INFO"
echo "   ${GPU_COUNT}× ${GPU_NAME} (${GPU_MANUFACTURER})"

# --- Resolve GPU brand ---
if [ -z "$GPU_BRAND" ]; then
    case "$GPU_MANUFACTURER" in
        AMD|amd)       GPU_BRAND="amd" ;;
        NVIDIA|nvidia) GPU_BRAND="nvidia" ;;
        *)
            echo "Cannot auto-detect GPU brand from manufacturer '${GPU_MANUFACTURER}'."
            echo "Use --amd or --nvidia to specify explicitly."
            exit 1
            ;;
    esac
fi

case "$GPU_BRAND" in
    amd)    AMI_NAME="ubuntu-amd-gpu" ;;
    nvidia) AMI_NAME="ubuntu-26.04-nvidia-gpu-x86_64" ;;
esac

# --- Resolve AMI ---
AMI_LOOKUP="${AMI_OVERRIDE:-${AMI_NAME}}"
echo "==> Resolving AMI: ${AMI_LOOKUP}"
AMI_ID=$(aws ec2 describe-images \
    --query "Images[?Name=='${AMI_LOOKUP}'].ImageId | [0]" --output text 2>/dev/null || true)
if [ -z "$AMI_ID" ] || [ "$AMI_ID" = "None" ]; then
    echo "No AMI found with name '${AMI_LOOKUP}'."
    echo "Build and import it first, or specify the AMI name with --ami <name>"
    echo "  scripts/build-system-image.sh scripts/images/ubuntu-gpu-${GPU_BRAND}.conf --import"
    exit 1
fi
echo "   $AMI_ID"

# --- Resolve subnet ---
echo "==> Resolving subnet"
SUBNET_ID=$(aws ec2 describe-subnets \
    --query 'Subnets[?MapPublicIpOnLaunch==`true`].SubnetId | [0]' --output text 2>/dev/null || true)
if [ -z "$SUBNET_ID" ] || [ "$SUBNET_ID" = "None" ]; then
    echo "No public subnet found"
    exit 1
fi
echo "   $SUBNET_ID"

VPC_ID=$(aws ec2 describe-subnets --subnet-ids "$SUBNET_ID" \
    --query 'Subnets[0].VpcId' --output text 2>/dev/null || true)
if [ -z "$VPC_ID" ] || [ "$VPC_ID" = "None" ]; then
    echo "Could not resolve VPC for subnet $SUBNET_ID"
    exit 1
fi

# --- Ensure SSH security group ---
# The default VPC SG only allows intra-SG traffic; we need a group that
# permits port 22 from anywhere so the SSH wait below can connect.
SSH_SG_NAME="spinifex-gpu-ssh"
echo "==> Ensuring SSH security group (${SSH_SG_NAME})"
SG_ID=$(aws ec2 describe-security-groups \
    --filters "Name=group-name,Values=${SSH_SG_NAME}" "Name=vpc-id,Values=${VPC_ID}" \
    --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null || true)
if [ -z "$SG_ID" ] || [ "$SG_ID" = "None" ]; then
    SG_ID=$(aws ec2 create-security-group \
        --group-name "$SSH_SG_NAME" \
        --description "Allow SSH access to GPU instances" \
        --vpc-id "$VPC_ID" \
        --query 'GroupId' --output text)
    aws ec2 authorize-security-group-ingress \
        --group-id "$SG_ID" \
        --protocol tcp \
        --port 22 \
        --cidr 0.0.0.0/0 >/dev/null
    echo "   Created $SG_ID"
else
    echo "   Reusing $SG_ID"
fi

# --- Ensure SSH key ---
if [ ! -f "$SSH_KEY" ]; then
    echo "==> Generating SSH key"
    sudo -u "${SUDO_USER:-$(id -un)}" mkdir -p "$(dirname "$SSH_KEY")"
    sudo -u "${SUDO_USER:-$(id -un)}" ssh-keygen -t ed25519 -f "$SSH_KEY" -N ""
fi
# Delete before import — stale key pair material causes import to fail
# silently, leaving the instance unreachable via SSH.
echo "==> Importing SSH key"
aws ec2 delete-key-pair --key-name spinifex-key >/dev/null 2>&1 || true
aws ec2 import-key-pair --key-name spinifex-key \
    --public-key-material "fileb://${SSH_KEY}.pub" >/dev/null 2>&1 || true

# --- Launch ---
echo "==> Launching ${INSTANCE_TYPE} (${DISK_GB} GB)..."
ID=$(aws ec2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name spinifex-key \
    --subnet-id "$SUBNET_ID" \
    --security-group-ids "$SG_ID" \
    --count 1 \
    --block-device-mappings "[{\"DeviceName\":\"/dev/sda1\",\"Ebs\":{\"VolumeSize\":${DISK_GB},\"VolumeType\":\"gp3\",\"DeleteOnTermination\":true}}]" \
    --query 'Instances[0].InstanceId' --output text)
if [ -z "$ID" ] || [ "$ID" = "None" ] || [ "$ID" = "null" ]; then
    echo "run-instances returned no InstanceId"
    exit 1
fi
echo "   $ID"

# --- Wait for running ---
echo "==> Waiting for running state..."
COUNT=0; STATE="unknown"
while [ $COUNT -lt 120 ]; do
    STATE=$(aws ec2 describe-instances --instance-ids "$ID" \
        --query "Reservations[0].Instances[0].State.Name" --output text 2>/dev/null || echo "unknown")
    [ "$STATE" = "running" ] && break
    [ "$STATE" = "terminated" ] && { echo "Instance terminated unexpectedly"; exit 1; }
    sleep 2; COUNT=$((COUNT + 2))
done
[ "$STATE" = "running" ] || { echo "Instance did not reach running state (last: $STATE)"; exit 1; }

# --- Wait for public IP ---
echo "==> Waiting for public IP..."
IP=""
for _i in $(seq 1 120); do
    IP=$(aws ec2 describe-instances --instance-ids "$ID" \
        --query 'Reservations[0].Instances[0].PublicIpAddress' --output text 2>/dev/null || true)
    [ -n "$IP" ] && [ "$IP" != "None" ] && [ "$IP" != "null" ] && break
    sleep 1
done
[ -n "$IP" ] && [ "$IP" != "None" ] || { echo "No public IP assigned after 120s"; exit 1; }
echo "   $IP"

# Clear stale known_hosts entry — instances reuse IPs across runs.
KNOWN_HOSTS="$HOME/.ssh/known_hosts"
[ -f "$KNOWN_HOSTS" ] && ssh-keygen -f "$KNOWN_HOSTS" -R "$IP" >/dev/null 2>&1 || true

# --- Wait for SSH ---
echo "==> Waiting for SSH..."
_ssh() {
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 -o BatchMode=yes \
        -i "$SSH_KEY" "${SSH_USER}@${IP}" "$@"
}

elapsed=0
while [ "$elapsed" -lt "$SSH_TIMEOUT" ]; do
    _ssh 'echo ready' >/dev/null 2>&1 && break
    sleep 5; elapsed=$((elapsed + 5))
done
[ "$elapsed" -lt "$SSH_TIMEOUT" ] || { echo "SSH timeout after ${SSH_TIMEOUT}s"; exit 1; }

# --- GPU visibility check ---
echo "==> Checking GPU visibility..."
lspci_out=$(_ssh 'lspci -nn 2>/dev/null' || true)
if printf '%s\n' "$lspci_out" | grep -qiE "1002:|10de:|Instinct|Aqua|Processing accelerator|NVIDIA"; then
    printf '%s\n' "$lspci_out" \
        | grep -iE "1002:|10de:|Instinct|Aqua|Processing accelerator|NVIDIA" \
        | sed 's/^/   ✓ /'
else
    echo "   WARNING: GPU not visible in lspci — check host-side passthrough assignment"
    printf '%s\n' "$lspci_out" | head -20 | sed 's/^/   /'
fi

# --- Done ---
echo ""
echo "✅ ${INSTANCE_TYPE} ready — ${ID} (${IP})"
echo ""
echo "   ssh -i ${SSH_KEY} -o StrictHostKeyChecking=no ${SSH_USER}@${IP}"
echo ""
echo "   Terminate when done:"
echo "   AWS_PROFILE=spinifex aws ec2 terminate-instances --instance-ids ${ID}"
