#!/bin/bash
# smoke-test.sh — Bare-bones smoke test for a running Spinifex node.
#
# Imports an SSH keypair, imports the Ubuntu AMI, and launches a single
# instance to verify the platform is functional end-to-end.
#
# Assumes services are running and AWS_PROFILE=spinifex is configured
# by a prior 'spx admin init'.
#
# Options:
#   --create-vpc   Build the VPC, internet gateway, public subnet, route table
#                  and security group first, following the sequence in
#                  docs/admin/setting-up-your-cluster, and tear them down at the
#                  end. Without this the test launches into whatever public
#                  subnet already exists, which only works where a usable
#                  default VPC is present.
#   --nodes N      Cluster size. Above 1, launch N instances and assert they did
#                  not all land on the same node.
#   --keep         Leave the resources --create-vpc created in place.
#
# GPU passthrough test (opt-in):
#   TEST_GPU=1 scripts/smoke-test.sh
#   TEST_GPU=1 GPU_VENDOR_ID=10de GPU_DEVICE_ID=2236 scripts/smoke-test.sh
#
# GPU env vars:
#   TEST_GPU=1            enable GPU passthrough checks
#   GPU_VENDOR_ID         PCI vendor ID (default: auto-detect first NVIDIA VGA/3D device)
#   GPU_DEVICE_ID         PCI device ID (default: auto-detect)
#   GPU_FAMILY            instance family to test (default: g5)
set -euo pipefail

TEST_GPU="${TEST_GPU:-0}"
GPU_VENDOR_ID="${GPU_VENDOR_ID:-}"
GPU_DEVICE_ID="${GPU_DEVICE_ID:-}"
GPU_FAMILY="${GPU_FAMILY:-g5}"

CREATE_VPC=0
NODES=1
KEEP=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --create-vpc) CREATE_VPC=1; shift ;;
        --nodes)      NODES="$2"; shift 2 ;;
        --keep)       KEEP=1; shift ;;
        -h|--help)    sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *)            echo "❌ Unknown option: $1 (try --help)" >&2; exit 2 ;;
    esac
done

[[ "$NODES" =~ ^[0-9]+$ ]] && [ "$NODES" -ge 1 ] || { echo "❌ --nodes must be >= 1" >&2; exit 2; }

VPC_CIDR="${VPC_CIDR:-10.200.0.0/16}"
SUBNET_CIDR="${SUBNET_CIDR:-10.200.1.0/24}"

export AWS_PROFILE=spinifex

# AWS commands run as the invoking user so their ~/.aws/config (spinifex profile
# + CA bundle) is used when the script is called via sudo.
INVOKING_USER="${SUDO_USER:-$(id -un)}"
INVOKING_HOME=$(getent passwd "$INVOKING_USER" | cut -d: -f6)
aws_as_user() { sudo -u "$INVOKING_USER" env HOME="$INVOKING_HOME" AWS_PROFILE=spinifex aws "$@"; }

# --- Wait for EC2 daemon to subscribe to NATS ---
# Port 3000 (UI) becomes ready before the daemon finishes initialising.
# Poll describe-key-pairs until it doesn't return InternalError.
echo "==> Waiting for EC2 daemon to be ready"
DAEMON_TIMEOUT=60
DAEMON_ELAPSED=0
while [ $DAEMON_ELAPSED -lt $DAEMON_TIMEOUT ]; do
    if aws_as_user ec2 describe-key-pairs --output text >/dev/null 2>&1; then
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
aws_as_user ec2 import-key-pair --key-name spinifex-key \
    --public-key-material "fileb://$SSH_KEY.pub" || true
aws_as_user ec2 describe-key-pairs

# --- GPU pre-flight (TEST_GPU=1 only) ---
if [[ "$TEST_GPU" == "1" ]]; then
    echo "==> [GPU] Checking IOMMU"
    if ! dmesg | grep -qi "DMAR.*IOMMU enabled\|iommu.*Translated\|Adding to iommu group\|AMD-Vi.*enabled"; then
        echo "❌ IOMMU not detected — add intel_iommu=on (or amd_iommu=on) and iommu=pt to kernel cmdline, then reboot"
        exit 1
    fi
    echo "   IOMMU active"

    echo "==> [GPU] Detecting NVIDIA GPU"
    if [[ -z "$GPU_VENDOR_ID" || -z "$GPU_DEVICE_ID" ]]; then
        GPU_LINE=$(lspci -nn | grep -i nvidia | grep -iE "VGA|3D" | head -1)
        if [[ -z "$GPU_LINE" ]]; then
            echo "❌ No NVIDIA GPU found via lspci — is the card seated and detectable?"
            lspci | grep -i nvidia >&2 || true
            exit 1
        fi
        echo "   Detected: $GPU_LINE"
        PCI_IDS=$(echo "$GPU_LINE" | grep -oP '\[\K[0-9a-f]{4}:[0-9a-f]{4}(?=\])')
        GPU_VENDOR_ID=$(echo "$PCI_IDS" | cut -d: -f1)
        GPU_DEVICE_ID=$(echo "$PCI_IDS" | cut -d: -f2)
    fi
    echo "   GPU PCI ID: ${GPU_VENDOR_ID}:${GPU_DEVICE_ID}"

    echo "==> [GPU] Verifying ${GPU_FAMILY} instance types advertised"
    G5_TYPES=$(aws_as_user ec2 describe-instance-types \
        --query "InstanceTypes[?starts_with(InstanceType, \`${GPU_FAMILY}\`)].InstanceType" \
        --output text 2>/dev/null || true)
    if [[ -z "$G5_TYPES" ]]; then
        echo "❌ No ${GPU_FAMILY}.* instance types returned by describe-instance-types"
        exit 1
    fi
    echo "   ${GPU_FAMILY} types available: $G5_TYPES"
fi

# --- Import AMI ---
# --- Import AMI ---
echo "==> Importing AMI"

LOCAL_IMAGE="$HOME/images/ubuntu-26.04.img"
ARCH=$(uname -m)

case "$ARCH" in
    x86_64)        IMG_ARCH="x86_64"; IMAGE_NAME="ubuntu-26.04-x86_64" ;;
    aarch64|arm64) IMG_ARCH="arm64";  IMAGE_NAME="ubuntu-26.04-arm64"  ;;
    *)
        echo "  Warning: unknown arch $ARCH, defaulting to x86_64"
        IMG_ARCH="x86_64"; IMAGE_NAME="ubuntu-26.04-x86_64"
        ;;
esac

AMI_NAME="ami-${IMAGE_NAME}"

EXISTING_AMI_ID=$(aws_as_user ec2 describe-images \
    --query "Images[?Name=='${AMI_NAME}'] | [0].ImageId" \
    --output text)

if [ -n "$EXISTING_AMI_ID" ] && [ "$EXISTING_AMI_ID" != "None" ]; then
    echo "  AMI already exists, skipping import: $AMI_NAME ($EXISTING_AMI_ID)"
else
    if [ -f "$LOCAL_IMAGE" ]; then
        echo "  Using local image: $LOCAL_IMAGE"
        sudo /usr/local/bin/spx admin images import \
            --file "$LOCAL_IMAGE" --distro ubuntu --version 26.04 --arch "$IMG_ARCH" --boot-mode uefi
    else
        echo "  Downloading image: $IMAGE_NAME"
        sudo /usr/local/bin/spx admin images import --name "$IMAGE_NAME"
    fi
fi

# --- Build the VPC topology (--create-vpc) ---
# Follows docs/admin/setting-up-your-cluster step for step, so a pass here means
# the documented sequence works and not merely that some pre-existing subnet does.
CREATED_VPC_ID=""
CREATED_IGW_ID=""
CREATED_SUBNET_ID=""
CREATED_RTB_ID=""
CREATED_SG_ID=""
LAUNCHED_IDS=""

# Torn down in reverse creation order, on failure as well as success: a VPC left
# behind by a failed run is one the next run trips over.
cleanup_vpc() {
    [ "$CREATE_VPC" = "1" ] || return 0
    [ "$KEEP" = "1" ] && { echo "==> --keep: leaving $CREATED_VPC_ID in place"; return 0; }
    echo "==> Cleaning up"
    if [ -n "$LAUNCHED_IDS" ]; then
        # shellcheck disable=SC2086  # deliberate word-splitting of the id list
        aws_as_user ec2 terminate-instances --instance-ids $LAUNCHED_IDS >/dev/null 2>&1 || true
        for _i in $(seq 1 60); do
            # shellcheck disable=SC2086
            _left=$(aws_as_user ec2 describe-instances --instance-ids $LAUNCHED_IDS \
                --query 'Reservations[].Instances[?State.Name!=`terminated`].InstanceId' \
                --output text 2>/dev/null || true)
            [ -z "$_left" ] && break
            sleep 2
        done
    fi
    [ -n "$CREATED_RTB_ID" ] && aws_as_user ec2 delete-route-table --route-table-id "$CREATED_RTB_ID" >/dev/null 2>&1 || true
    [ -n "$CREATED_SUBNET_ID" ] && aws_as_user ec2 delete-subnet --subnet-id "$CREATED_SUBNET_ID" >/dev/null 2>&1 || true
    if [ -n "$CREATED_IGW_ID" ]; then
        aws_as_user ec2 detach-internet-gateway --internet-gateway-id "$CREATED_IGW_ID" --vpc-id "$CREATED_VPC_ID" >/dev/null 2>&1 || true
        aws_as_user ec2 delete-internet-gateway --internet-gateway-id "$CREATED_IGW_ID" >/dev/null 2>&1 || true
    fi
    [ -n "$CREATED_VPC_ID" ] && aws_as_user ec2 delete-vpc --vpc-id "$CREATED_VPC_ID" >/dev/null 2>&1 || true
    echo "  Removed"
}

if [ "$CREATE_VPC" = "1" ]; then
    trap cleanup_vpc EXIT

    echo "==> Creating VPC $VPC_CIDR"
    CREATED_VPC_ID=$(aws_as_user ec2 create-vpc --cidr-block "$VPC_CIDR" \
        --query 'Vpc.VpcId' --output text)
    [ -n "$CREATED_VPC_ID" ] && [ "$CREATED_VPC_ID" != "None" ] || { echo "❌ create-vpc failed"; exit 1; }
    echo "  VPC: $CREATED_VPC_ID"

    echo "==> Creating and attaching internet gateway"
    CREATED_IGW_ID=$(aws_as_user ec2 create-internet-gateway \
        --query 'InternetGateway.InternetGatewayId' --output text)
    aws_as_user ec2 attach-internet-gateway \
        --internet-gateway-id "$CREATED_IGW_ID" --vpc-id "$CREATED_VPC_ID" >/dev/null
    echo "  IGW: $CREATED_IGW_ID"

    echo "==> Creating public subnet $SUBNET_CIDR"
    CREATED_SUBNET_ID=$(aws_as_user ec2 create-subnet \
        --vpc-id "$CREATED_VPC_ID" --cidr-block "$SUBNET_CIDR" \
        --query 'Subnet.SubnetId' --output text)
    CREATED_RTB_ID=$(aws_as_user ec2 create-route-table --vpc-id "$CREATED_VPC_ID" \
        --query 'RouteTable.RouteTableId' --output text)
    aws_as_user ec2 create-route --route-table-id "$CREATED_RTB_ID" \
        --destination-cidr-block 0.0.0.0/0 --gateway-id "$CREATED_IGW_ID" >/dev/null
    aws_as_user ec2 associate-route-table \
        --route-table-id "$CREATED_RTB_ID" --subnet-id "$CREATED_SUBNET_ID" >/dev/null
    aws_as_user ec2 modify-subnet-attribute \
        --subnet-id "$CREATED_SUBNET_ID" --map-public-ip-on-launch >/dev/null
    echo "  Subnet: $CREATED_SUBNET_ID  route table: $CREATED_RTB_ID"

    # The default SG denies all inbound, so without this the SSH probe below
    # times out on an instance that is otherwise perfectly healthy.
    echo "==> Authorizing SSH and ICMP on the default security group"
    CREATED_SG_ID=$(aws_as_user ec2 describe-security-groups \
        --filters "Name=vpc-id,Values=$CREATED_VPC_ID" "Name=group-name,Values=default" \
        --query 'SecurityGroups[0].GroupId' --output text)
    [ -n "$CREATED_SG_ID" ] && [ "$CREATED_SG_ID" != "None" ] ||
        { echo "❌ No default security group in $CREATED_VPC_ID"; exit 1; }
    aws_as_user ec2 authorize-security-group-ingress \
        --group-id "$CREATED_SG_ID" --protocol tcp --port 22 --cidr 0.0.0.0/0 >/dev/null 2>&1 || true
    aws_as_user ec2 authorize-security-group-ingress \
        --group-id "$CREATED_SG_ID" --protocol icmp --port -1 --cidr 0.0.0.0/0 >/dev/null 2>&1 || true
    echo "  Security group: $CREATED_SG_ID"
fi

# --- Launch smoke-test instance ---
echo "==> Launching smoke-test instance"
if [[ "$TEST_GPU" == "1" ]]; then
    INSTANCE_TYPE="${GPU_FAMILY}.xlarge"
elif grep -q 'AuthenticAMD' /proc/cpuinfo; then
    INSTANCE_TYPE="t3a.small"
else
    INSTANCE_TYPE="t3.small"
fi

AMI_ID=$(aws_as_user ec2 describe-images \
    --query "Images[?Name=='${AMI_NAME}'] | [0].ImageId" \
    --output text)

if [ -z "$AMI_ID" ] || [ "$AMI_ID" = "None" ]; then
    echo "❌ No AMI found"
    exit 1
fi

if [ -n "$CREATED_SUBNET_ID" ]; then
    SUBNET_ID="$CREATED_SUBNET_ID"
else
    SUBNET_ID=$(aws_as_user ec2 describe-subnets --query 'Subnets[?MapPublicIpOnLaunch==`true`].SubnetId | [0]' --output text)
fi
if [ -z "$SUBNET_ID" ] || [ "$SUBNET_ID" = "None" ]; then
    echo "❌ No subnet found"
    exit 1
fi

# On a cluster, launch one instance per node so placement can be asserted. The
# first is the one the SSH checks below use; the rest exist only to show the
# scheduler spread them.
echo "  AMI: $AMI_ID  type: $INSTANCE_TYPE  subnet: $SUBNET_ID  count: $NODES"

ALL_IDS=$(aws_as_user ec2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name spinifex-key \
    --subnet-id "$SUBNET_ID" \
    --count "$NODES" \
    --query 'Instances[].InstanceId' --output text)
LAUNCHED_IDS="$ALL_IDS"

INSTANCE_ID=$(awk '{print $1}' <<<"$ALL_IDS")
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
    DESCRIBE=$(aws_as_user ec2 describe-instances --instance-ids "$INSTANCE_ID" 2>/dev/null) || {
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
    _ip=$(aws_as_user ec2 describe-instances --instance-ids "$INSTANCE_ID" \
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
           ubuntu@"$SSH_INST_HOST" 'echo ready' >/dev/null 2>&1; then
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
    ubuntu@"$SSH_INST_HOST" 'id && hostname' 2>&1)
echo "  $SSH_OUT"
if ! echo "$SSH_OUT" | grep -q "ubuntu"; then
    echo "❌ Expected ubuntu in SSH output"
    exit 1
fi

# --- GPU guest verification (TEST_GPU=1 only) ---
if [[ "$TEST_GPU" == "1" ]]; then
    echo "==> [GPU] Verifying GPU visible inside guest"
    GPU_VISIBLE=0
    for _i in $(seq 1 12); do
        if ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
               -o ConnectTimeout=5 -o BatchMode=yes \
               -p "$SSH_INST_PORT" -i "$SSH_KEY" \
               ubuntu@"$SSH_INST_HOST" \
               'lspci 2>/dev/null | grep -iE "nvidia|amdgpu|instinct"' 2>/dev/null; then
            GPU_VISIBLE=1
            break
        fi
        sleep 5
    done
    if [[ $GPU_VISIBLE -eq 0 ]]; then
        echo "❌ GPU not visible in guest via lspci"
        exit 1
    fi
    echo "   GPU visible in guest"
    echo "✅ Smoke test passed (GPU passthrough) — instance $INSTANCE_ID launched with GPU and verified"
else
    echo "✅ Smoke test passed — instance $INSTANCE_ID launched, running, and SSH-verified"
fi

# --- Multi-node placement ---
# A cluster that puts every instance on node 1 passes every check above and is
# still broken, so placement is asserted rather than assumed.
if [ "$NODES" -gt 1 ]; then
    echo "==> Checking instance placement across $NODES nodes"
    for _i in $(seq 1 30); do
        PLACEMENT=$(sudo /usr/local/bin/spx get vms 2>/dev/null |
            sed 's/\x1b\[[0-9;]*m//g' |
            awk -F'|' 'NR > 1 {gsub(/ /, "", $1); gsub(/ /, "", $8); if ($1 != "") print $1, $8}')
        PLACED=$(echo "$ALL_IDS" | tr '\t' '\n' | while read -r id; do
            [ -n "$id" ] && grep "^$id " <<<"$PLACEMENT" | awk '{print $2}'
        done)
        [ "$(grep -c . <<<"$PLACED")" -eq "$NODES" ] && break
        sleep 2
    done

    echo "$PLACED" | sort | uniq -c | awk '{printf "  %s: %s instance(s)\n", $2, $1}'
    DISTINCT=$(sort -u <<<"$PLACED" | grep -c .)
    if [ "$DISTINCT" -lt 2 ]; then
        echo "❌ All $NODES instances landed on the same node — the scheduler is not spreading work"
        exit 1
    fi
    echo "✅ Placement spread across $DISTINCT node(s)"
fi
