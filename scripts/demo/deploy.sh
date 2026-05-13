#!/bin/bash
# deploy.sh — Deploy and start the Spinifex GPU demo across four instances.
#
# Usage:
#   DASHBOARD_IP=<ip> YOLO_IP=<ip> OLLAMA_IP=<ip> QWEN_IP=<ip> \
#     VIDEO_PATH=/path/to/video.mp4 \
#     scripts/demo/deploy.sh
#
# Required env vars:
#   DASHBOARD_IP   4x-1: serves the web dashboard
#   YOLO_IP        4x-3: runs YOLO inference
#   OLLAMA_IP      4x-2: runs Ollama + llama3.1:70b
#   QWEN_IP        12x:  runs Ollama + qwen3-vl:235b
#   VIDEO_PATH     Local path to the .mp4 file to run YOLO on
#
# Optional:
#   SSH_KEY        (default: ~/.ssh/spinifex-key)
#   SSH_USER       (default: ec2-user)

set -euo pipefail

DASHBOARD_IP="${DASHBOARD_IP:?set DASHBOARD_IP}"
YOLO_IP="${YOLO_IP:?set YOLO_IP}"
OLLAMA_IP="${OLLAMA_IP:?set OLLAMA_IP}"
QWEN_IP="${QWEN_IP:?set QWEN_IP}"
VIDEO_PATH="${VIDEO_PATH:?set VIDEO_PATH}"

SSH_KEY="${SSH_KEY:-$HOME/.ssh/spinifex-key}"
SSH_USER="${SSH_USER:-ec2-user}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

_ssh() {
    local ip="$1"; shift
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=10 -o BatchMode=yes \
        -i "$SSH_KEY" "${SSH_USER}@${ip}" "$@"
}

_scp() {
    scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -i "$SSH_KEY" -r "$@"
}

# ── YOLO instance (4x-3) ────────────────────────────────────────────────────
echo "==> [YOLO] Deploying to $YOLO_IP..."
_scp "$SCRIPT_DIR/yolo_stream.py" "$SCRIPT_DIR/requirements-yolo.txt" \
    "${SSH_USER}@${YOLO_IP}:~/"
_scp "$VIDEO_PATH" "${SSH_USER}@${YOLO_IP}:~/demo_video.mp4"

echo "   Installing Python deps..."
_ssh "$YOLO_IP" '
    . ~/yolo-rocm/bin/activate
    pip install --quiet -r ~/requirements-yolo.txt
'

echo "   Starting yolo_stream.py..."
_ssh "$YOLO_IP" "
    pkill -f yolo_stream.py || true
    sleep 1
    . ~/yolo-rocm/bin/activate
    nohup env \
        VIDEO_PATH=~/demo_video.mp4 \
        QWEN_HOST=http://${QWEN_IP}:11434 \
        YOLO_DEVICE=0 \
        PORT=8080 \
        python ~/yolo_stream.py > ~/yolo_stream.log 2>&1 &
    echo \"   PID \$!\"
"
echo "   YOLO stream: http://${YOLO_IP}:8080/video"

# ── Dashboard instance (4x-1) ───────────────────────────────────────────────
echo ""
echo "==> [Dashboard] Deploying to $DASHBOARD_IP..."
_ssh "$DASHBOARD_IP" 'mkdir -p ~/demo/static'
_scp "$SCRIPT_DIR/dashboard_server.py" "$SCRIPT_DIR/requirements-dashboard.txt" \
    "${SSH_USER}@${DASHBOARD_IP}:~/demo/"
_scp "$SCRIPT_DIR/static/index.html" \
    "${SSH_USER}@${DASHBOARD_IP}:~/demo/static/"

echo "   Installing Python deps..."
_ssh "$DASHBOARD_IP" '
    python3 -m venv ~/demo-venv 2>/dev/null || true
    . ~/demo-venv/bin/activate
    pip install --quiet -r ~/demo/requirements-dashboard.txt
'

echo "   Starting dashboard_server.py..."
_ssh "$DASHBOARD_IP" "
    pkill -f dashboard_server.py || true
    sleep 1
    . ~/demo-venv/bin/activate
    cd ~/demo
    nohup env \
        YOLO_HOST=http://${YOLO_IP}:8080 \
        OLLAMA_HOST=http://${OLLAMA_IP}:11434 \
        OLLAMA_MODEL=llama3.1:70b \
        PORT=8000 \
        python dashboard_server.py > ~/dashboard.log 2>&1 &
    echo \"   PID \$!\"
"

echo ""
echo "✅ Demo deployed"
echo ""
echo "   Dashboard:  http://${DASHBOARD_IP}:8000"
echo "   YOLO feed:  http://${YOLO_IP}:8080/video"
echo "   Ollama:     http://${OLLAMA_IP}:11434"
echo "   Qwen3-VL:   http://${QWEN_IP}:11434"
echo ""
echo "   Logs:"
echo "     ssh -i ${SSH_KEY} ${SSH_USER}@${YOLO_IP}      tail -f ~/yolo_stream.log"
echo "     ssh -i ${SSH_KEY} ${SSH_USER}@${DASHBOARD_IP} tail -f ~/dashboard.log"
echo ""
echo "   NOTE: Ollama and qwen3-vl must already be running on their instances."
echo "   If not: _ssh \$IP 'sudo systemctl start ollama && ollama run <model>'"
