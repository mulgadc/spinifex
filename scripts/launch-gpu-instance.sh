#!/bin/bash
# launch-gpu-instance.sh — Launch a single GPU instance for manual configuration.
#
# Usage:
#   scripts/launch-gpu-instance.sh <size>
#
# Sizes:
#   4x   g7e.4xlarge  — 1× MI350X, 300 GB disk
#   12x  g7e.12xlarge — 2× MI350X, 600 GB disk
#
# Env overrides:
#   SSH_KEY   Path to SSH private key (default: ~/.ssh/spinifex-key)
#   SSH_USER  SSH user inside guest  (default: ec2-user)
set -euo pipefail

export AWS_PROFILE=spinifex

SSH_KEY="${SSH_KEY:-$HOME/.ssh/spinifex-key}"
SSH_USER="${SSH_USER:-ec2-user}"

SIZE="${1:-}"
case "$SIZE" in
    4x)  INSTANCE_TYPE="g7e.4xlarge";  DISK_GB=300 ;;
    12x) INSTANCE_TYPE="g7e.12xlarge"; DISK_GB=600 ;;
    *)
        echo "Usage: $0 <4x|12x>"
        echo "  4x  — g7e.4xlarge  (1× MI350X, 300 GB)"
        echo "  12x — g7e.12xlarge (2× MI350X, 600 GB)"
        exit 1
        ;;
esac

# Resolve AMI
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
    aws ec2 describe-images --query "Images[*].[Name,ImageId]" --output table
    exit 1
fi

# Resolve subnet
SUBNET_ID=$(aws ec2 describe-subnets \
    --query 'Subnets[?MapPublicIpOnLaunch==`true`].SubnetId | [0]' --output text 2>/dev/null || true)
if [ -z "$SUBNET_ID" ] || [ "$SUBNET_ID" = "None" ]; then
    echo "❌ No public subnet found"
    exit 1
fi

# Check instance type is advertised
TYPE_OK=$(aws ec2 describe-instance-types \
    --query "InstanceTypes[?InstanceType=='${INSTANCE_TYPE}'].InstanceType | [0]" \
    --output text 2>/dev/null || true)
if [ -z "$TYPE_OK" ] || [ "$TYPE_OK" = "None" ]; then
    echo "❌ ${INSTANCE_TYPE} not advertised — no free GPU available or daemon not running"
    echo "   Available GPU types:"
    aws ec2 describe-instance-types \
        --query "InstanceTypes[?GpuInfo!=null].InstanceType" --output text 2>/dev/null || true
    exit 1
fi

# Ensure SSH key exists
if [ ! -f "$SSH_KEY" ]; then
    echo "==> Generating SSH key"
    mkdir -p "$(dirname "$SSH_KEY")"
    ssh-keygen -t ed25519 -f "$SSH_KEY" -N ""
fi
! aws ec2 import-key-pair --key-name spinifex-key \
    --public-key-material "fileb://${SSH_KEY}.pub" >/dev/null 2>&1 || true

# Launch
echo "==> Launching ${INSTANCE_TYPE} (${DISK_GB} GB disk)..."
ID=$(aws ec2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name spinifex-key \
    --subnet-id "$SUBNET_ID" \
    --count 1 \
    --block-device-mappings "[{\"DeviceName\":\"/dev/sda1\",\"Ebs\":{\"VolumeSize\":${DISK_GB},\"VolumeType\":\"gp3\",\"DeleteOnTermination\":true}}]" \
    --query 'Instances[0].InstanceId' --output text)
if [ -z "$ID" ] || [ "$ID" = "None" ] || [ "$ID" = "null" ]; then
    echo "❌ run-instances returned no InstanceId"
    exit 1
fi
echo "   Instance: $ID"

# Wait for running
echo "==> Waiting for running state..."
COUNT=0
STATE="unknown"
while [ $COUNT -lt 120 ]; do
    STATE=$(aws ec2 describe-instances --instance-ids "$ID" \
        --query "Reservations[0].Instances[0].State.Name" --output text 2>/dev/null || echo "unknown")
    [ "$STATE" = "running" ] && break
    if [ "$STATE" = "terminated" ]; then
        echo "❌ Instance terminated unexpectedly"
        exit 1
    fi
    sleep 2; COUNT=$((COUNT + 2))
done
if [ "$STATE" != "running" ]; then
    echo "❌ Instance did not reach running state (last: $STATE)"
    exit 1
fi

# Wait for IP
echo "==> Waiting for public IP..."
IP=""
for _i in $(seq 1 60); do
    IP=$(aws ec2 describe-instances --instance-ids "$ID" \
        --query 'Reservations[0].Instances[0].PublicIpAddress' --output text 2>/dev/null || true)
    [ -n "$IP" ] && [ "$IP" != "None" ] && [ "$IP" != "null" ] && break
    sleep 2
done
if [ -z "$IP" ] || [ "$IP" = "None" ]; then
    echo "❌ No public IP assigned after 120s"
    exit 1
fi

# Clear stale known_hosts entry
KNOWN_HOSTS="$HOME/.ssh/known_hosts"
if [ -f "$KNOWN_HOSTS" ]; then
    ssh-keygen -f "$KNOWN_HOSTS" -R "$IP" >/dev/null 2>&1 || true
fi

echo ""
echo "✅ ${INSTANCE_TYPE} running"
echo "   Instance: $ID"
echo "   IP:       $IP"
echo ""
echo "   SSH:"
echo "   ssh -i ${SSH_KEY} -o StrictHostKeyChecking=no ${SSH_USER}@${IP}"
echo ""
echo "   Terminate when done:"
echo "   AWS_PROFILE=spinifex aws ec2 terminate-instances --instance-ids ${ID}"
