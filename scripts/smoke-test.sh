#!/bin/bash
# smoke-test.sh — Bare-bones smoke test for a running Spinifex node.
#
# Imports an SSH keypair, imports the Ubuntu AMI, and launches a single
# instance to verify the platform is functional end-to-end.
#
# Assumes services are running and AWS_PROFILE=spinifex is configured
# by a prior 'spx admin init'.
set -euo pipefail

export AWS_PROFILE=spinifex

# --- Wait for EC2 daemon to subscribe to NATS ---
# Port 3000 (UI) becomes ready before the daemon finishes initialising.
# Poll describe-key-pairs until it doesn't return InternalError.
echo "==> Waiting for EC2 daemon to be ready"
DAEMON_TIMEOUT=60
DAEMON_ELAPSED=0
while [ $DAEMON_ELAPSED -lt $DAEMON_TIMEOUT ]; do
    if aws ec2 describe-key-pairs --output text >/dev/null 2>&1; then
        break
    fi
    sleep 2
    DAEMON_ELAPSED=$((DAEMON_ELAPSED + 2))
done
if [ $DAEMON_ELAPSED -ge $DAEMON_TIMEOUT ]; then
    echo "❌ EC2 daemon not ready after ${DAEMON_TIMEOUT}s"
    exit 1
fi
echo "   EC2 daemon ready after ${DAEMON_ELAPSED}s"

# --- SSH key ---
SSH_KEY="$HOME/.ssh/spinifex-key"
if [ ! -f "$SSH_KEY.pub" ]; then
    echo "==> Generating SSH key pair"
    mkdir -p "$HOME/.ssh"
    ssh-keygen -t ed25519 -f "$SSH_KEY" -N ""
fi

echo "==> Importing SSH key"
! aws ec2 import-key-pair --key-name spinifex-key \
    --public-key-material "fileb://$SSH_KEY.pub"
aws ec2 describe-key-pairs

# --- Import AMI ---
# --- Import AMI ---
echo "==> Importing AMI"

LOCAL_IMAGE="$HOME/images/ubuntu-24.04.img"
ARCH=$(uname -m)

case "$ARCH" in
    x86_64)        IMG_ARCH="x86_64"; IMAGE_NAME="ubuntu-24.04-x86_64" ;;
    aarch64|arm64) IMG_ARCH="arm64";  IMAGE_NAME="ubuntu-24.04-arm64"  ;;
    *)
        echo "  Warning: unknown arch $ARCH, defaulting to x86_64"
        IMG_ARCH="x86_64"; IMAGE_NAME="ubuntu-24.04-x86_64"
        ;;
esac

AMI_NAME="ami-${IMAGE_NAME}"

EXISTING_AMI_ID=$(aws ec2 describe-images \
    --query "Images[?Name=='${AMI_NAME}'] | [0].ImageId" \
    --output text)

if [ -n "$EXISTING_AMI_ID" ] && [ "$EXISTING_AMI_ID" != "None" ]; then
    echo "  AMI already exists, skipping import: $AMI_NAME ($EXISTING_AMI_ID)"
else
    if [ -f "$LOCAL_IMAGE" ]; then
        echo "  Using local image: $LOCAL_IMAGE"
        sudo /usr/local/bin/spx admin images import \
            --file "$LOCAL_IMAGE" --distro ubuntu --version 24.04 --arch "$IMG_ARCH"
    else
        echo "  Downloading image: $IMAGE_NAME"
        sudo /usr/local/bin/spx admin images import --name "$IMAGE_NAME"
    fi
fi

# --- Launch smoke-test instance ---
echo "==> Launching smoke-test instance"
if grep -q 'AuthenticAMD' /proc/cpuinfo; then
    INSTANCE_TYPE="t3a.small"
else
    INSTANCE_TYPE="t3.small"
fi

AMI_ID=$(aws ec2 describe-images \
    --query "Images[?Name=='${AMI_NAME}'] | [0].ImageId" \
    --output text)

if [ -z "$AMI_ID" ] || [ "$AMI_ID" = "None" ]; then
    echo "❌ No AMI found"
    exit 1
fi

SUBNET_ID=$(aws ec2 describe-subnets --query 'Subnets[?MapPublicIpOnLaunch==`true`].SubnetId | [0]' --output text)
if [ -z "$SUBNET_ID" ] || [ "$SUBNET_ID" = "None" ]; then
    echo "❌ No subnet found"
    exit 1
fi

echo "  AMI: $AMI_ID  type: $INSTANCE_TYPE  subnet: $SUBNET_ID"

INSTANCE_ID=$(aws ec2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name spinifex-key \
    --subnet-id "$SUBNET_ID" \
    --count 1 \
    --query 'Instances[0].InstanceId' --output text)

if [ -z "$INSTANCE_ID" ] || [ "$INSTANCE_ID" = "None" ] || [ "$INSTANCE_ID" = "null" ]; then
    echo "❌ run-instances returned no InstanceId"
    exit 1
fi
echo "  Instance ID: $INSTANCE_ID"

# --- Wait for running state ---
echo "==> Waiting for instance to reach running state"
COUNT=0
STATE="unknown"
while [ $COUNT -lt 60 ]; do
    DESCRIBE=$(aws ec2 describe-instances --instance-ids "$INSTANCE_ID" 2>/dev/null) || {
        sleep 2; COUNT=$((COUNT + 1)); continue
    }
    STATE=$(echo "$DESCRIBE" | jq -r '.Reservations[0].Instances[0].State.Name // "not-found"')
    [ "$STATE" = "running" ] && break
    if [ "$STATE" = "terminated" ]; then
        echo "❌ Instance terminated unexpectedly"
        exit 1
    fi
    sleep 2
    COUNT=$((COUNT + 1))
done
if [ "$STATE" != "running" ]; then
    echo "❌ Instance failed to reach running state (last: $STATE)"
    exit 1
fi
echo "  Instance is running"

# --- Wait for public IP ---
echo "==> Waiting for public IP assignment (up to 300s)"
SSH_INST_PORT=22
SSH_INST_HOST=""
for _i in $(seq 1 300); do
    _ip=$(aws ec2 describe-instances --instance-ids "$INSTANCE_ID" \
        --query 'Reservations[0].Instances[0].PublicIpAddress' --output text 2>/dev/null || true)
    if [ -n "$_ip" ] && [ "$_ip" != "None" ] && [ "$_ip" != "null" ]; then
        SSH_INST_HOST="$_ip"
        break
    fi
    sleep 1
done
if [ -z "$SSH_INST_HOST" ]; then
    echo "❌ No public IP assigned after 300s — external networking not working"
    exit 1
fi
echo "  Public IP: $SSH_INST_HOST"

# --- Wait for SSH ---
echo "==> Waiting for SSH on $SSH_INST_HOST:$SSH_INST_PORT"
SSH_READY=0
for _i in $(seq 1 300); do
    if ssh -o StrictHostKeyChecking=no \
           -o UserKnownHostsFile=/dev/null \
           -o ConnectTimeout=2 \
           -o BatchMode=yes \
           -p "$SSH_INST_PORT" \
           -i "$SSH_KEY" \
           ec2-user@"$SSH_INST_HOST" 'echo ready' >/dev/null 2>&1; then
        SSH_READY=1
        break
    fi
    sleep 1
done
if [ $SSH_READY -eq 0 ]; then
    echo "❌ SSH not ready after 300s"
    exit 1
fi
echo "  SSH is ready"

# --- Verify instance identity via SSH ---
echo "==> Verifying instance"
SSH_OUT=$(ssh -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=5 \
    -o BatchMode=yes \
    -p "$SSH_INST_PORT" \
    -i "$SSH_KEY" \
    ec2-user@"$SSH_INST_HOST" 'id && hostname' 2>&1)
echo "  $SSH_OUT"
if ! echo "$SSH_OUT" | grep -q "ec2-user"; then
    echo "❌ Expected ec2-user in SSH output"
    exit 1
fi

echo "✅ Smoke test passed — instance $INSTANCE_ID launched, running, and SSH-verified"
