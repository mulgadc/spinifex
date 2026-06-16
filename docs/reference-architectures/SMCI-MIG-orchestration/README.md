---
title: "Mixed AI Workloads on a Single H200 Chassis: Guest-Managed MIG"
description: "Install Spinifex from source, configure host-local networking, attach Predastore storage, and run four concurrent AI workloads across MIG partitions managed inside guest VMs."
category: "Reference Architectures"
tags:
  - nvidia
  - h200
  - mig
  - vllm
  - yolo
  - predastore
  - bare-metal
resources:
  - title: "Install from Source"
    url: "/docs/install-source"
  - title: "VPC Networking"
    url: "/docs/vpc-networking"
  - title: "NVIDIA MIG User Guide"
    url: "https://docs.nvidia.com/datacenter/tesla/mig-user-guide/"
sections:
  - overview
  - prerequisites
  - instructions
---

## Overview

Spinifex is an open-source infrastructure platform that brings core AWS services including EC2, EBS and S3 to bare-metal, edge, and on-prem environments. It exposes an AWS compatible API, so any tooling that works against AWS (the `aws` CLI, Terraform, SDKs) works against a Spinifex node unchanged, with a single profile swap.

This guide documents a full bare-metal AI deployment on a single chassis using four of its eight available NVIDIA H200 SXM GPUs. Rather than having Spinifex manage MIG partitioning at the host level, each VM receives an entire H200 via PCIe passthrough and manages its own MIG partitions internally. This gives each tenant full control over how they slice their GPU — including the ability to run heterogeneous workloads at different partition sizes on the same physical card.

### VM layout

| VM | IP | MIG config | Workload | Model |
|---|---|---|---|---|
| `vm-llama3b` | 192.168.10.7  | 7 × 1g.18gb | Chat inference × 7 | Llama-3.2-3B-Instruct |
| `vm-qwen32b` | 192.168.10.8  | 2 × 3g.71gb | Chat inference × 2 | Qwen2.5-32B-Instruct |
| `vm-llama70b` | 192.168.10.12 | 1 × 7g.141gb | Chat inference × 1 | Llama-3.1-70B-FP8 |
| `vm-yolo`    | 192.168.10.14 | 2 × 3g.71gb | Object detection × 2 | YOLO11x + YOLO11s |

Twelve concurrent inference endpoints in total: 7 fast 3B slots, 2 mid-tier 32B slots, 1 full-GPU 70B slot, and 2 real-time vision streams running side-by-side to compare detection models.

## Prerequisites

### Platform

| Component | Specification |
|---|---|
| **Chassis** | Supermicro X13 8U GPU System |
| **CPUs** | 2× Intel Xeon Platinum 8568Y+ (48 cores each, 96 cores / 192 threads total) |
| **RAM** | 2 TB DDR5-4800 (32× 64 GB SK Hynix DIMMs) |
| **Storage** | 4× 7.68 TB KIOXIA CD6 NVMe SSDs (30.72 TB raw) |
| **GPUs** | 8× NVIDIA H200 SXM5 (141 GiB HBM3e per GPU, ~1.13 TB total) — 4 used in this demo |
| **Host OS** | Ubuntu 26.04 LTS |
| **Orchestration** | Spinifex — EC2-compatible bare-metal API |
| **Guest OS** | Ubuntu 26.04 LTS |
| **GPU partitioning** | NVIDIA MIG — managed inside each guest VM |
| **Block storage** | Predastore — S3-compatible, NVMe-backed |
| **Container runtime** | Docker |
| **Inference runtime** | vLLM (`vllm-openai` image from local registry) |
| **Vision runtime** | Ultralytics YOLO11 |

---

## Instructions

### 0. Pre-req

gpu passthrough changes here

### 1. Configure host-local VPC networking

The bridge must exist before Spinifex starts, so configure and apply netplan first.

This chassis runs host-local only with no upstream router. Therefore, `br-wan` must be created and the hosts physical NIC enslaved to it. Additionally, the address range `192.168.10.0/24` must be attached to `br-wan` which will be used by the guest VMs to communicate with the host and each other.

The exact process for this is described in the [VPC Networking](/docs/vpc-networking#host-local-subnet-no-upstream-router)) guide.


### 2. Install Spinifex from source

Follow the [Install from Source](/docs/install-source) guide. This process will install Spinifex and start Spinifex services, however `spinifex.toml` needs to be edited to finalise the changes made to the networking in the previous section, as described in the following section.

### 3. Configure spinifex.toml and start services

Edit `/etc/spinifex/spinifex.toml` to point the external pool and VPCD at the bridge created in step 1:

```toml
[network]
external_mode = "static"

[[network.external_pools]]
name        = "default"
bind_bridge = "br-wan"
range_start = "192.168.10.2"
range_end   = "192.168.10.254"
gateway_ip  = "192.168.10.1"
prefix_len  = 24

[nodes.node1.vpcd]
external_interface = "br-wan"
```

Then restart all services:

```bash
sudo systemctl start spinifex.target
sudo systemctl status spinifex.target
```

### 4. Attach Predastore storage

Expand: Note, we will distribute the predastore volume over 3 physical drives for data redundancy (similar to RAID, predastore uses reed solomon encoding for data shards and parity). Note this environment is ideal to demonstrate this since multiple drives are provided.


This chassis has four NVMe drives. One is the OS drive, so confirm drive assignments with `lsblk` before proceeding, as device names vary between systems. Each of the three data drives needs to be symlinked into the path Predastore expects for its node data directories:

```bash
# Confirm drive paths — identify the OS drive and the three data drives
lsblk

# Symlink each data drive into the Predastore node directories
# (adjust device paths to match your drive layout)

# stop services first
sudo systemctl stop spinifex.target

# mount first
# /var/lib/spinifex/predastore/distributed/nodes/node-1/ path

# sudo mkdir -p /mnt/nvme-1/
# sudo mount /dev/nvme1n1 /mnt/nvme-1
# sudo mkdir /mnt/nvme-1/nodes/
# sudo mkdir /mnt/nvme-1/db/

# mv /var/lib/spinifex/predastore/distributed/db/node-1 /mnt/nvme-1/db/node-1
# mv /var/lib/spinifex/predastore/distributed/nodes/node-1 /mnt/nvme-1/nodes/node-1

# double check syntax
# ln -s /var/lib/spinifex/predastore/distributed/nodes/node-1 /mnt/nvme-1/nodes/node-1
# ln -s /var/lib/spinifex/predastore/distributed/db/node-1 /mnt/nvme-1/db/node-1

# repeat for nvme2,3

# (edit /etc/fstab)

sudo systemctl start spinifex.target
```

Verify the nodes are healthy before proceeding:

```bash
aws s3 ls --endpoint-url https://localhost:8443
# Should return without error (empty bucket list is fine)
```

### 5. Import the GPU AMI

The demo uses the standard Spinifex NVIDIA GPU AMI (`ubuntu-26.04-nvidia-gpu-x86_64`), which includes:

- Ubuntu 26.04 LTS guest image
- NVIDIA server driver (DKMS pre-built against the pinned kernel)
- Docker CE + nvidia-container-toolkit (`--gpus` support enabled at boot)
- Python 3 + venv, common utilities (tmux, curl, ffmpeg, etc.)

No models or container images are pre-baked. The vLLM image is served from the host local registry, and models are downloaded onto the host before copying to the relevant VM.

```bash
spx admin images import --name ubuntu-26.04-nvidia-gpu-x86_64
```

Confirm it's visible:

```bash
export AWS_PROFILE=spinifex
aws ec2 describe-images \
    --query 'Images[*].[ImageId,Name]' \
    --output table
```

### 6. Create VPC resources

```bash
export AWS_PROFILE=spinifex

# Create VPC and subnet
VPC_ID=$(aws ec2 create-vpc --cidr-block 192.168.10.0/24 \
    --query 'Vpc.VpcId' --output text)
SUBNET_ID=$(aws ec2 create-subnet --vpc-id $VPC_ID \
    --cidr-block 192.168.10.0/24 \
    --query 'Subnet.SubnetId' --output text)

# Security group — allow SSH + all vLLM/YOLO ports
SG_ID=$(aws ec2 create-security-group \
    --group-name demo-sg --description "H200 MIG demo" \
    --vpc-id $VPC_ID --query 'GroupId' --output text)

aws ec2 authorize-security-group-ingress --group-id $SG_ID \
    --protocol tcp --port 22 --cidr 0.0.0.0/0
aws ec2 authorize-security-group-ingress --group-id $SG_ID \
    --protocol tcp --port 8000-8011 --cidr 0.0.0.0/0

# Import SSH key
aws ec2 import-key-pair --key-name spinifex-key \
    --public-key-material fileb://~/.ssh/spinifex-key.pub
```

### 7. Launch the four VMs

Each VM gets a whole H200 via PCIe passthrough. The `p5e.4xlarge` instance type maps one H200 per VM:

```bash
AMI_ID=$(aws ec2 describe-images \
    --filters "Name=name,Values=ubuntu-26.04-nvidia-gpu-x86_64" \
    --query 'Images[0].ImageId' --output text)

for i in 1 2 3 4; do
    aws ec2 run-instances \
        --image-id $AMI_ID \
        --instance-type p5e.4xlarge \
        --key-name spinifex-key \
        --subnet-id $SUBNET_ID \
        --security-group-ids $SG_ID \
        --count 1 \
        --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=mig-demo-$i}]"
done
```

Wait until all four reach running state, then take note of the public IPs assigned to each instance:

```bash
aws ec2 describe-instances \
    --filters "Name=tag:Name,Values=mig-demo-*" "Name=instance-state-name,Values=running" \
    --query 'Reservations[*].Instances[*].[InstanceId,PublicIpAddress,State.Name]' \
    --output table
```

### 8. Enable MIG inside each VM

MIG is enabled inside each guest VM, not on the host. SSH into each VM and enable it:

```bash
# Repeat on each VM IP
ssh -i ~/.ssh/spinifex-key ec2-user@<vm-ip>

sudo nvidia-smi -mig 1
nvidia-smi | grep "MIG M."
# Should show: MIG M.  Enabled
```

### 9. Create MIG partitions inside each VM

<expand what MIG is again, why we are doing this>

Once MIG is enabled, create the partitions. The partition profile determines how many vLLM (or YOLO) instances can run concurrently and how much HBM each gets.

**vm-llama3b — 7 × 1g.18gb (Llama 3B):**
```bash
sudo nvidia-smi mig -cgi 1g.18gb,1g.18gb,1g.18gb,1g.18gb,1g.18gb,1g.18gb,1g.18gb -C
nvidia-smi -L  # Verify 7 MIG devices
```

**vm-qwen32b — 2 × 3g.71gb (Qwen 32B):**
```bash
sudo nvidia-smi mig -cgi 3g.71gb,3g.71gb -C
nvidia-smi -L  # Verify 2 MIG devices
```

**vm-llama70b — 1 × 7g.141gb (Llama 70B, full GPU):**
```bash
sudo nvidia-smi mig -cgi 7g.141gb -C
nvidia-smi -L  # Verify 1 MIG device
```

**vm-yolo — 2 × 3g.71gb (YOLO):**
```bash
sudo nvidia-smi mig -cgi 3g.71gb,3g.71gb -C
nvidia-smi -L  # Verify 2 MIG devices
```

<add screenshot of the nvidia-smi MIG output here as an example>

Each MIG device gets a UUID of the form `MIG-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`. These UUIDs are used to pin individual containers to their slice via `--gpus "device=<UUID>"` (for Docker) or `CUDA_VISIBLE_DEVICES=<UUID>` (for direct Python processes).

### 10. Configure Docker to use the local image registry

The vLLM image is served from the host's local Docker registry (`192.168.10.1:5000`).

Setting up the local registry on the host:

```bash
# Configure Docker to trust the local registry address
sudo tee /etc/docker/daemon.json > /dev/null <<'EOF'
{
  "insecure-registries": ["192.168.10.1:5000"]
}
EOF
sudo systemctl restart docker

# Start the registry container, bound to the bridge interface only
docker run -d \
    -p 192.168.10.1:5000:5000 \
    --name registry \
    --restart=always \
    registry:2

# Pull the vLLM image and push it into the local registry
docker pull vllm/vllm-openai:latest
docker tag vllm/vllm-openai:latest 192.168.10.1:5000/vllm-openai:latest
docker push 192.168.10.1:5000/vllm-openai:latest

# Verify the image is available
curl http://192.168.10.1:5000/v2/_catalog
# {"repositories":["vllm-openai"]}
```

Each VM then needs to trust it as an insecure registry:

```bash
# On each VM — vm-yolo uses direct Python, not Docker for YOLO
sudo tee /etc/docker/daemon.json > /dev/null <<'EOF'
{
  "insecure-registries": ["192.168.10.1:5000"]
}
EOF
sudo systemctl restart docker
```

### 11. Deploy the LLM workloads

Each model is downloaded onto the host and copied to the relevant VMs. Take note of the location of the models on the VMs, then set each one up as follows:

<expand more about MODEL_DIR=<path-to-model>> give an example

**vm-llama3b — 7 × Llama-3.2-3B-Instruct (one container per MIG slice):**

Enumerate the MIG UUIDs and start one vLLM container per slice:

```bash
MODEL_DIR=<path-to-model>

mapfile -t UUIDS < <(nvidia-smi -L | grep -oP 'MIG-[0-9a-f-]+')
for i in "${!UUIDS[@]}"; do
    docker run -d --rm \
        --name "vllm-$i" \
        --gpus "device=${UUIDS[$i]}" \
        --ipc host \
        -p $((8000 + i)):8000 \
        -v "${MODEL_DIR}:/models" \
        192.168.10.1:5000/vllm-openai:latest \
        vllm serve /models \
            --served-model-name llama-3b \
            --dtype bfloat16 \
            --max-model-len 4096 \
            --gpu-memory-utilization 0.90 \
            --port 8000
done
```

**vm-qwen32b — 2 × Qwen2.5-32B-Instruct:**

```bash
MODEL_DIR=<path-to-model>

mapfile -t UUIDS < <(nvidia-smi -L | grep -oP 'MIG-[0-9a-f-]+')
for i in "${!UUIDS[@]}"; do
    docker run -d --rm \
        --name "qwen32b-$i" \
        --gpus "device=${UUIDS[$i]}" \
        --ipc host \
        -p $((8000 + i)):8000 \
        -v "${MODEL_DIR}:/models" \
        192.168.10.1:5000/vllm-openai:latest \
        vllm serve /models/Qwen2.5-32B-Instruct \
            --served-model-name qwen2.5-32b \
            --dtype bfloat16 \
            --max-model-len 4096 \
            --gpu-memory-utilization 0.90 \
            --port 8000
done
```

**vm-llama70b — Llama-3.1-70B-Instruct-FP8 (full GPU slice):**

```bash
MODEL_DIR=<path-to-model>

UUID=$(nvidia-smi -L | grep -oP 'MIG-[0-9a-f-]+' | head -1)
docker run -d --rm \
    --name vllm \
    --gpus "device=${UUID}" \
    --ipc host \
    -p 8000:8000 \
    -v "${MODEL_DIR}:/models" \
    192.168.10.1:5000/vllm-openai:latest \
    vllm serve /models/Meta-Llama-3.1-70B-Instruct-FP8 \
        --served-model-name meta-llama-3.1-70b \
        --dtype auto \
        --max-model-len 8192 \
        --gpu-memory-utilization 0.90 \
        --port 8000
```

### 12. Deploy YOLO object detection (vm-yolo)

We set up a simple YOLO inference server runs which runs directly in a Python venv and streams its output to a dashboard running on the host.

```bash
# Install dependencies
python3 -m venv ~/yolo-venv
~/yolo-venv/bin/pip install ultralytics fastapi "uvicorn[standard]" opencv-python-headless
```
Start one process per MIG slice, using `CUDA_VISIBLE_DEVICES` to pin each to its UUID:

```bash
# On vm-yolo — get UUIDs first
nvidia-smi -L
# GPU 0: NVIDIA H200 (UUID: GPU-...)
#   MIG 3g.71gb  Device 0: (UUID: MIG-<uuid0>)
#   MIG 3g.71gb  Device 1: (UUID: MIG-<uuid1>)

# YOLO11x on slice 0
CUDA_VISIBLE_DEVICES=MIG-<uuid0> \
  VIDEO_PATH=<path-to-video> YOLO_MODEL=yolo11x.pt PORT=8010 \
  ~/yolo-venv/bin/python ~/yolo_stream.py >> ~/yolo-x.log 2>&1 &

# YOLO11s on slice 1
CUDA_VISIBLE_DEVICES=MIG-<uuid1> \
  VIDEO_PATH=<path-to-video> YOLO_MODEL=yolo11s.pt PORT=8011 \
  ~/yolo-venv/bin/python ~/yolo_stream.py >> ~/yolo-s.log 2>&1 &
```

YOLO11x (~75 MB) and YOLO11s (~9 MB) weights download automatically on first run from the Ultralytics model hub.

### 13. Dashboard

We created a simple dashboard that runs on the host and proxies all 10 LLM endpoints (via SSE streaming) and both YOLO MJPEG feeds:

<p><video src="https://iso.mulgadc.com/h200-demo.mp4" controls width="100%" style="border-radius:6px"></video></p>


The dashboard shows:
- GPU allocation bars for all four VMs (proportional to MIG slice size)
- Live streaming LLM responses per endpoint, colour-coded by tier
- Side-by-side YOLO11x vs YOLO11s video feeds with FPS and detection counts

---

### 14. Conclusion

This document highlights how Spinifex can turn a single bare-metal chassis into a multi-tenant AI serving platform. Spinifex utilises the flexibility of PCIe passthrough combined with NVIDIA's MIG capability to allocate GPU resources in whichever configuration is required by the workload/s. This flexibility and fine-grain control ensures maximum GPU utilisation.

Importantly, it does so with standard `aws ec2` CLI calls — `run-instances`, `describe-instances`, `terminate-instances` — against Spinifex's EC2-compatible endpoint. Teams already operating AWS infrastructure can point their existing tooling at a Spinifex node with a single profile change, against GPUs that sit in their own rack.
