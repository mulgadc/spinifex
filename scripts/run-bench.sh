#!/bin/sh

# List running instances using ubuntu image and run disk performance benchmark
export AWS_PROFILE=spinifex

# Initialize perf tracking variables
PERF_PID=""
NBDKIT_PID=""

# Check SPINIFEX_AMI set, if not try to detect it
if [ -z "$SPINIFEX_AMI" ]; then
    echo "SPINIFEX_AMI not set, attempting to detect..."
    ARCH=$(uname -m)
    if [ "$ARCH" = "x86_64" ]; then
        IMAGE_NAME="ami-ubuntu-26.04-x86_64"
    elif [ "$ARCH" = "aarch64" ] || [ "$ARCH" = "arm64" ]; then
        IMAGE_NAME="ami-ubuntu-26.04-arm64"
    else
        IMAGE_NAME="ami-ubuntu-26.04-x86_64"
    fi
    SPINIFEX_AMI=$(aws ec2 describe-images --query "Images[?Name=='$IMAGE_NAME'].ImageId" --output text)
    if [ -z "$SPINIFEX_AMI" ]; then
        echo "Error: Could not find image with Name '$IMAGE_NAME'"
        exit 1
    fi
    echo "Detected SPINIFEX_AMI: $SPINIFEX_AMI"
fi

# Get running instances matching the AMI
# Retry loop: wait for instance to come online (up to 120 seconds)
echo "Finding running instances with ImageId: $SPINIFEX_AMI"
MAX_ATTEMPTS=120
ATTEMPT=0
INSTANCE_ID=""

while [ $ATTEMPT -lt $MAX_ATTEMPTS ]; do
    INSTANCE_JSON=$(aws ec2 describe-instances --filters "Name=image-id,Values=$SPINIFEX_AMI" "Name=instance-state-name,Values=running" --output json 2>/dev/null)

    # Extract InstanceId using jq (handle nested structure)
    INSTANCE_ID=$(echo "$INSTANCE_JSON" | jq -r '.Reservations[0].Instances[0].InstanceId // empty' 2>/dev/null)

    if [ -n "$INSTANCE_ID" ] && [ "$INSTANCE_ID" != "null" ]; then
        echo "Found running instance: $INSTANCE_ID"
        break
    fi

    ATTEMPT=$((ATTEMPT + 1))
    if [ $ATTEMPT -lt $MAX_ATTEMPTS ]; then
        echo "Waiting for instance to come online... (attempt $ATTEMPT/$MAX_ATTEMPTS)"
        sleep 1
    fi
done

if [ -z "$INSTANCE_ID" ] || [ "$INSTANCE_ID" = "null" ]; then
    echo "Error: No running instances found with ImageId $SPINIFEX_AMI after $MAX_ATTEMPTS attempts"
    echo "Available instances:"
    aws ec2 describe-instances --query "Reservations[*].Instances[*].[InstanceId,ImageId,State.Name]" --output table
    exit 1
fi

# Check ps, get port number for ssh from qemu process
# Retry loop: wait for qemu process to start (up to 60 seconds)
echo "Finding SSH port for instance $INSTANCE_ID..."
QEMU_ATTEMPTS=0
MAX_QEMU_ATTEMPTS=60
QEMU_CMD=""

while [ $QEMU_ATTEMPTS -lt $MAX_QEMU_ATTEMPTS ]; do
    QEMU_CMD=$(ps auxw | grep "$INSTANCE_ID" | grep qemu-system | grep -v grep)

    if [ -n "$QEMU_CMD" ]; then
        break
    fi

    QEMU_ATTEMPTS=$((QEMU_ATTEMPTS + 1))
    if [ $QEMU_ATTEMPTS -lt $MAX_QEMU_ATTEMPTS ]; then
        echo "Waiting for qemu process to start... (attempt $QEMU_ATTEMPTS/$MAX_QEMU_ATTEMPTS)"
        sleep 1
    fi
done

if [ -z "$QEMU_CMD" ]; then
    echo "Error: Could not find qemu process for instance $INSTANCE_ID after $MAX_QEMU_ATTEMPTS attempts"
    exit 1
fi

# Extract port from hostfwd=tcp:127.0.0.1:PORT-:22
SSH_PORT=$(echo "$QEMU_CMD" | sed -n 's/.*hostfwd=tcp:127\.0\.0\.1:\([0-9]*\)-:22.*/\1/p')

if [ -z "$SSH_PORT" ]; then
    echo "Error: Could not extract SSH port from qemu command"
    echo "QEMU command: $QEMU_CMD"
    exit 1
fi

echo "Found SSH port: $SSH_PORT"

# Find nbdkit process for the main volume (excluding efi and cloudinit)
# Note: describe-instances doesn't include volume info yet, so we search ps directly
echo "Finding nbdkit process for main volume (excluding efi and cloudinit)..."
NBDKIT_CMD=$(ps auxw | grep nbdkit | grep -v "efi " | grep -v "cloudinit " | grep -v grep | head -n1)

if [ -z "$NBDKIT_CMD" ]; then
    echo "Warning: Could not find nbdkit process for main volume, perf profiling will be skipped"
    NBDKIT_PID=""
    PERF_PID=""
else
    # Extract PID from the process line (second column in ps output)
    NBDKIT_PID=$(echo "$NBDKIT_CMD" | awk '{print $2}')
    # Extract volume ID from the command line for informational purposes
    VOLUME_ID=$(echo "$NBDKIT_CMD" | sed -n 's/.*volume=\([^ ]*\).*/\1/p')
    echo "Found nbdkit process PID: $NBDKIT_PID (volume: $VOLUME_ID)"

    # Start perf record in the background (will run until interrupted)
    echo "Starting perf profiling for nbdkit process (PID: $NBDKIT_PID)..."
    sudo perf record -g -p "$NBDKIT_PID" -o /tmp/spinifex-nbdkit-perf.data > /tmp/perf.log 2>&1 &
    PERF_PID=$!
    echo "Perf profiling started (PID: $PERF_PID), will record during benchmark execution"
fi

# Get script directory
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BENCH_SCRIPT="$SCRIPT_DIR/disk-performance.sh"

if [ ! -f "$BENCH_SCRIPT" ]; then
    echo "Error: Benchmark script not found: $BENCH_SCRIPT"
    exit 1
fi

# Copy benchmark script to instance
# Auto accept SSH host key (trust for development)
# Retry loop: wait for SSH to be ready (up to 120 seconds)
echo "Waiting for SSH to be ready on port $SSH_PORT..."
SSH_ATTEMPTS=0
MAX_SSH_ATTEMPTS=120
SSH_READY=false

while [ $SSH_ATTEMPTS -lt $MAX_SSH_ATTEMPTS ]; do
    ssh-keyscan -p "$SSH_PORT" 127.0.0.1 >> ~/.ssh/known_hosts 2>/dev/null
    if ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=2 -p "$SSH_PORT" -i ~/.ssh/spinifex-key ubuntu@127.0.0.1 'echo ready' >/dev/null 2>&1; then
        SSH_READY=true
        echo "SSH is ready!"
        break
    fi

    SSH_ATTEMPTS=$((SSH_ATTEMPTS + 1))
    if [ $SSH_ATTEMPTS -lt $MAX_SSH_ATTEMPTS ]; then
        echo "Waiting for SSH to be ready... (attempt $SSH_ATTEMPTS/$MAX_SSH_ATTEMPTS)"
        sleep 1
    fi
done

if [ "$SSH_READY" != "true" ]; then
    echo "Error: SSH not ready after $MAX_SSH_ATTEMPTS attempts"
    exit 1
fi

echo "Copying benchmark script to instance..."
scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -P "$SSH_PORT" -i ~/.ssh/spinifex-key "$BENCH_SCRIPT" ubuntu@127.0.0.1:~/disk-performance.sh

if [ $? -ne 0 ]; then
    echo "Error: Failed to copy benchmark script to instance"
    exit 1
fi

# Execute benchmark script
echo "Running benchmark on instance..."
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p "$SSH_PORT" -i ~/.ssh/spinifex-key ubuntu@127.0.0.1 'chmod +x ~/disk-performance.sh && ~/disk-performance.sh' > /tmp/spinifex-vm-disk.log 2>&1

if [ $? -ne 0 ]; then
    echo "Error: Benchmark execution failed, check /tmp/spinifex-vm-disk.log for details"
    # Stop perf if it was running
    if [ -n "$PERF_PID" ]; then
        echo "Stopping perf profiling..."
        sudo kill -INT "$PERF_PID" 2>/dev/null || true
        # Wait for perf to finish writing
        sleep 3
        if [ -f /tmp/spinifex-nbdkit-perf.data ]; then
            sudo chmod 644 /tmp/spinifex-nbdkit-perf.data
            echo "Perf data saved to /tmp/spinifex-nbdkit-perf.data"
        fi
    fi
    exit 1
fi

# On completion, stop the perf process and save its data to disk
if [ -n "$PERF_PID" ]; then
    echo "Stopping perf profiling..."
    # Send SIGINT to perf to stop recording gracefully
    sudo kill -INT "$PERF_PID" 2>/dev/null || true
    # Wait for perf to finish writing data
    sleep 3

    if [ -f /tmp/spinifex-nbdkit-perf.data ]; then
        sudo chmod 644 /tmp/spinifex-nbdkit-perf.data
        echo "Perf benchmarks saved to /tmp/spinifex-nbdkit-perf.data"
        echo "To view: sudo perf report -i /tmp/spinifex-nbdkit-perf.data"
    else
        echo "Warning: Perf data file not found at /tmp/spinifex-nbdkit-perf.data"
    fi
fi

# Display results
echo ""
echo "Benchmark completed successfully!"
echo "Results saved to /tmp/spinifex-vm-disk.log"
echo ""
echo "=== Benchmark Results ==="
cat /tmp/spinifex-vm-disk.log
