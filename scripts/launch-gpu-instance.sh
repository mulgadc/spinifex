#!/bin/bash
# launch-gpu-instance.sh — Launch a single GPU instance, provision MI350X drivers,
# ROCm, Ollama, and a YOLO/ROCm Python venv, then run a smoke test.
#
# Usage:
#   scripts/launch-gpu-instance.sh <size>
#
# Sizes:
#   4x   g7e.4xlarge  — 1× MI350X, 300 GB disk
#   12x  g7e.12xlarge — 2× MI350X, 600 GB disk
#
# Env overrides:
#   SSH_KEY        Path to SSH private key (default: ~/.ssh/spinifex-key)
#   SSH_USER       SSH user inside guest  (default: ec2-user)
#   SSH_TIMEOUT    Seconds to wait for initial SSH   (default: 300)
#   REBOOT_TIMEOUT Seconds to wait for SSH after reboot (default: 300)
set -euo pipefail

export AWS_PROFILE=spinifex

SSH_KEY="${SSH_KEY:-$HOME/.ssh/spinifex-key}"
SSH_USER="${SSH_USER:-ec2-user}"
SSH_TIMEOUT="${SSH_TIMEOUT:-300}"
REBOOT_TIMEOUT="${REBOOT_TIMEOUT:-300}"

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

check_gpu() {
    local ip="$1" context="$2"
    echo "==> GPU check ($context)"
    local out
    out=$(_ssh "$ip" 'lspci -nn 2>/dev/null' || true)
    if ! printf '%s\n' "$out" | grep -qiE "1002:75a0|Instinct|Aqua|Processing accelerator"; then
        echo "   ❌ GPU not visible in lspci after $context"
        printf '%s\n' "$out" | sed 's/^/   /'
        return 1
    fi
    printf '%s\n' "$out" | grep -iE "1002:75a0|Instinct|Aqua|Processing accelerator" | sed 's/^/   ✓ /'

    echo "   Driver / device node state:"
    _ssh "$ip" '
        echo "--- lspci -v (AMD) ---"
        lspci -v 2>/dev/null | grep -A6 -iE "1002:75a0|Instinct|Processing accelerator" || true
        echo "--- amdgpu in lsmod ---"
        lsmod | grep amdgpu || echo "(not loaded)"
        echo "--- dmesg (amdgpu/kfd/firmware) ---"
        sudo dmesg 2>/dev/null | grep -iE "amdgpu|kfd|firmware.*amd|amd.*firmware|fatal|failed" | tail -30 || true
        echo "--- /dev/dri ---"
        ls -l /dev/dri/ 2>/dev/null || echo "(none)"
        echo "--- /dev/kfd ---"
        ls -l /dev/kfd 2>/dev/null || echo "(none)"
    ' | sed 's/^/   /'
    return 0
}

# --- Resolve AMI ---
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

# --- Resolve subnet ---
SUBNET_ID=$(aws ec2 describe-subnets \
    --query 'Subnets[?MapPublicIpOnLaunch==`true`].SubnetId | [0]' --output text 2>/dev/null || true)
if [ -z "$SUBNET_ID" ] || [ "$SUBNET_ID" = "None" ]; then
    echo "❌ No public subnet found"
    exit 1
fi

# --- Check instance type is advertised ---
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

# --- Ensure SSH key exists ---
if [ ! -f "$SSH_KEY" ]; then
    echo "==> Generating SSH key"
    mkdir -p "$(dirname "$SSH_KEY")"
    ssh-keygen -t ed25519 -f "$SSH_KEY" -N ""
fi
! aws ec2 import-key-pair --key-name spinifex-key \
    --public-key-material "fileb://${SSH_KEY}.pub" >/dev/null 2>&1 || true

# --- Launch ---
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

# --- Wait for running ---
echo "==> Waiting for running state..."
COUNT=0; STATE="unknown"
while [ $COUNT -lt 120 ]; do
    STATE=$(aws ec2 describe-instances --instance-ids "$ID" \
        --query "Reservations[0].Instances[0].State.Name" --output text 2>/dev/null || echo "unknown")
    [ "$STATE" = "running" ] && break
    [ "$STATE" = "terminated" ] && { echo "❌ Instance terminated unexpectedly"; exit 1; }
    sleep 2; COUNT=$((COUNT + 2))
done
[ "$STATE" = "running" ] || { echo "❌ Instance did not reach running state (last: $STATE)"; exit 1; }

# --- Wait for IP ---
echo "==> Waiting for public IP..."
IP=""
for _i in $(seq 1 60); do
    IP=$(aws ec2 describe-instances --instance-ids "$ID" \
        --query 'Reservations[0].Instances[0].PublicIpAddress' --output text 2>/dev/null || true)
    [ -n "$IP" ] && [ "$IP" != "None" ] && [ "$IP" != "null" ] && break
    sleep 2
done
[ -n "$IP" ] && [ "$IP" != "None" ] || { echo "❌ No public IP assigned after 120s"; exit 1; }

# --- Clear stale known_hosts ---
KNOWN_HOSTS="$HOME/.ssh/known_hosts"
[ -f "$KNOWN_HOSTS" ] && ssh-keygen -f "$KNOWN_HOSTS" -R "$IP" >/dev/null 2>&1 || true

echo ""
echo "✅ ${INSTANCE_TYPE} running — $ID ($IP)"
echo ""

# --- Wait for initial SSH ---
echo "==> Waiting for SSH..."
if ! wait_ssh "$IP" "$SSH_TIMEOUT"; then
    echo "❌ SSH timeout after ${SSH_TIMEOUT}s"
    exit 1
fi
echo "   SSH ready"

# --- Phase 0: GPU visible before any work ---
check_gpu "$IP" "initial boot" || { echo "❌ GPU not visible on initial boot"; exit 1; }

# --- Phase 1: firmware + packages + initramfs → reboot ---
echo ""
echo "==> Phase 1: apt mirror + packages + initramfs rebuild..."
_ssh "$IP" 'sudo sed -i -e "s|http://archive.ubuntu.com/ubuntu|http://mirrors.xtom.com/ubuntu|g" -e "s|http://security.ubuntu.com/ubuntu|http://mirrors.xtom.com/ubuntu|g" /etc/apt/sources.list.d/ubuntu.sources 2>/dev/null || true'
echo "   Running apt update..."
_ssh "$IP" 'sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq 2>&1 | grep -E "^(E:|W:|Err:)" >&2 || true'
echo "   Installing packages..."
_ssh "$IP" 'sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -o Acquire::Retries=1 pciutils linux-firmware rocminfo rocm-smi python3 python3-venv python3-pip git curl wget htop tmux ffmpeg libgl1 libglib2.0-0'
echo "   Rebuilding initramfs..."
_ssh "$IP" 'sudo update-initramfs -u -k all'
echo "   Adding ec2-user to render/video groups..."
_ssh "$IP" 'sudo usermod -aG render,video ec2-user'
echo "   Rebooting (1/2)..."
_ssh "$IP" 'sudo reboot' || true
sleep 20

echo "==> Waiting for SSH after reboot 1/2..."
if ! wait_ssh "$IP" "$REBOOT_TIMEOUT"; then
    echo "❌ SSH timeout after reboot 1"
    exit 1
fi
echo "   SSH ready"
check_gpu "$IP" "reboot 1" || { echo "❌ GPU lost after reboot 1"; exit 1; }

# --- Phase 2: ROCm driver probe + verify ---
echo ""
echo "==> Phase 2: probing amdgpu driver..."
_ssh "$IP" 'sudo modprobe amdgpu || true'
_ssh "$IP" 'lsmod | grep amdgpu || true'
echo "   dmesg (amdgpu/kfd/firmware):"
_ssh "$IP" 'sudo dmesg | grep -iE "amdgpu|kfd|firmware|fatal|failed" | tail -120 || true' | sed 's/^/   /'
echo "   /dev/dri:"
_ssh "$IP" 'ls -l /dev/dri 2>/dev/null || echo "(none)"' | sed 's/^/   /'
echo "   /dev/kfd:"
_ssh "$IP" 'ls -l /dev/kfd 2>/dev/null || echo "(none)"' | sed 's/^/   /'
echo "   rocminfo:"
_ssh "$IP" 'rocminfo | grep -E "Name:|Marketing Name|Device Type|Uuid" || true' | sed 's/^/   /'
echo "   rocm-smi:"
_ssh "$IP" 'rocm-smi || true' | sed 's/^/   /'

# --- Phase 3: Ollama ---
echo ""
echo "==> Phase 3: installing Ollama..."
_ssh "$IP" 'cd /tmp && curl -fsSL https://ollama.com/download/ollama-linux-amd64.tar.zst | sudo tar --zstd -x -C /usr'
_ssh "$IP" 'cd /tmp && curl -fsSL https://ollama.com/download/ollama-linux-amd64-rocm.tar.zst | sudo tar --zstd -x -C /usr'
echo "   Setting up ollama user..."
_ssh "$IP" 'sudo useradd -r -s /bin/false -U -m -d /usr/share/ollama ollama 2>/dev/null || true'
_ssh "$IP" 'sudo usermod -aG render,video ollama'
_ssh "$IP" 'sudo mkdir -p /usr/share/ollama/.ollama/models && sudo chown -R ollama:ollama /usr/share/ollama'
echo "   Writing ollama.service..."
_ssh "$IP" 'sudo tee /etc/systemd/system/ollama.service >/dev/null' <<'SVCEOF'
[Unit]
Description=Ollama Service
After=network-online.target

[Service]
ExecStart=/usr/bin/ollama serve
User=ollama
Group=ollama
Restart=always
RestartSec=3
Environment="OLLAMA_HOST=0.0.0.0:11434"

[Install]
WantedBy=multi-user.target
SVCEOF
_ssh "$IP" 'sudo systemctl daemon-reload && sudo systemctl enable --now ollama'
echo "   Ollama service started"

# --- Phase 4: Python YOLO/ROCm venv ---
echo ""
echo "==> Phase 4: Python YOLO/ROCm venv..."
_ssh "$IP" 'python3 -m venv ~/yolo-rocm'
echo "   Installing pip/wheel/setuptools..."
_ssh "$IP" '. ~/yolo-rocm/bin/activate && pip install --upgrade --quiet pip wheel setuptools'
echo "   Installing PyTorch (ROCm 7.0)..."
_ssh "$IP" '. ~/yolo-rocm/bin/activate && pip install --quiet torch torchvision --index-url https://download.pytorch.org/whl/rocm7.0'
echo "   Installing Ultralytics/YOLO..."
_ssh "$IP" '. ~/yolo-rocm/bin/activate && pip install --quiet ultralytics opencv-python-headless numpy pillow'

# --- Phase 5: amdgpu.ids symlink + Ultralytics config ---
echo ""
echo "==> Phase 5: amdgpu.ids symlink + Ultralytics config dir..."
_ssh "$IP" 'sudo mkdir -p /opt/amdgpu/share/libdrm && sudo ln -sf /usr/share/libdrm/amdgpu.ids /opt/amdgpu/share/libdrm/amdgpu.ids'
_ssh "$IP" 'mkdir -p ~/.config/Ultralytics'

# --- Phase 6: smoke test ---
echo ""
echo "==> Phase 6: smoke test"
echo "   PyTorch GPU check:"
_ssh "$IP" '. ~/yolo-rocm/bin/activate && python -c "import torch; print(\"torch:\", torch.__version__); print(\"cuda available:\", torch.cuda.is_available()); print(\"device count:\", torch.cuda.device_count()); print(torch.cuda.get_device_name(0) if torch.cuda.is_available() else \"NO GPU\")"' | sed 's/^/   /'

echo "   YOLO predict (yolo11n, bus.jpg):"
_ssh "$IP" '. ~/yolo-rocm/bin/activate && yolo predict model=yolo11n.pt source="https://ultralytics.com/images/bus.jpg" device=0 2>&1 | tail -10' | sed 's/^/   /'

# --- Final state ---
echo ""
echo "==> Final GPU state"
echo "   rocminfo:"
_ssh "$IP" 'rocminfo 2>/dev/null | grep -E "Name:|Marketing Name|Device Type|Uuid" || true' | sed 's/^/   /'
echo "   rocm-smi:"
_ssh "$IP" 'rocm-smi 2>/dev/null || true' | sed 's/^/   /'

echo ""
echo "✅ ${INSTANCE_TYPE} fully provisioned — $ID ($IP)"
echo ""
echo "   SSH:"
echo "   ssh -i ${SSH_KEY} -o StrictHostKeyChecking=no ${SSH_USER}@${IP}"
echo ""
echo "   Terminate when done:"
echo "   AWS_PROFILE=spinifex aws ec2 terminate-instances --instance-ids ${ID}"
