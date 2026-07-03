---
title: "Multi-Node Install"
description: "Deploy Spinifex across multiple servers to create an availability zone with high availability, data durability, and fault tolerance."
category: "Getting Started"
tags:
  - install
  - multi node
  - cluster
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "Predastore (S3)"
    url: "https://github.com/mulgadc/predastore"
  - title: "Viperblock (EBS)"
    url: "https://github.com/mulgadc/viperblock"
---

# Multi-Node Installation

> Deploy Spinifex across multiple servers to create an availability zone.

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

A Spinifex cluster distributes services across multiple servers for high availability, data durability, and fault tolerance. Cluster formation is automatic — the init node waits for peers to join, then distributes credentials, CA certificates, and configuration.

**Network Requirements:**

- Minimum 1 NIC per server (2 recommended for production)
- UDP port 6081 open between hosts (Geneve tunnels)
- TCP ports 4222, 4248, 6641, 6642 open between hosts (NATS, OVN)

## Prerequisites

> [!IMPORTANT]
> **Prerequisite — WAN bridge required on every node.**
>
> Before running the installer on any server, that server's WAN interface **must** already be enslaved to a Linux bridge named `br-wan`. The host IP, default route, and DHCP must all live on the bridge — not on the bare NIC.
>
> The bootstrap installer does **not** create this bridge for you yet. Running it on a host whose default route is still on a bare NIC will leave the install in a non-working state. Auto-provisioning of `br-wan` will land in a future release.
>
> **Verify on every node before continuing:**
>
> - `ip -br link show br-wan` — bridge exists and is `UP`
> - `ip route` — default route's `dev` is `br-wan`
>
> **Setup references:** [VPC Networking → Bridge Setup](/docs/vpc-networking#bridge-setup-physical-network-wiring) for the topology.

## Instructions

## Step 1. Install Spinifex on Each Server

```bash
curl -fsSL https://install.mulgadc.com | bash
```

## Step 2. Set Node IP Variables

On **each server**, export the management IPs for all nodes:

```bash
export SPINIFEX_NODE1=192.168.1.10
export SPINIFEX_NODE2=192.168.1.11
export SPINIFEX_NODE3=192.168.1.12
export AWS_REGION=us-east-1
export AWS_AZ=us-east-1a
```

## Step 3. Setup OVN Networking

Server 1 runs OVN central and must be set up first. If your WAN interface is already a bridge, setup-ovn.sh auto-detects it. Otherwise use `--wan-bridge=br-wan --wan-iface=eth1` (dedicated WAN NIC).

**Server 1:**

```bash
sudo /usr/local/share/spinifex/setup-ovn.sh \
  --management \
  --encap-ip=$SPINIFEX_NODE1
```

**Server 2** (after server 1 is ready):

```bash
sudo /usr/local/share/spinifex/setup-ovn.sh \
  --ovn-remote=tcp:$SPINIFEX_NODE1:6642 \
  --encap-ip=$SPINIFEX_NODE2
```

**Server 3** (after server 1 is ready):

```bash
sudo /usr/local/share/spinifex/setup-ovn.sh \
  --ovn-remote=tcp:$SPINIFEX_NODE1:6642 \
  --encap-ip=$SPINIFEX_NODE3
```

Verify all 3 chassis registered:

```bash
sudo ovn-sbctl show
```

## Step 4. Form the Cluster

Run init and join concurrently — init blocks until all nodes join.

**Server 1 — Initialize:**

```bash
sudo spx admin init \
  --node node1 --nodes 3 \
  --bind $SPINIFEX_NODE1 --cluster-bind $SPINIFEX_NODE1 \
  --port 4432 --region $AWS_REGION --az $AWS_AZ
```

The init output displays the join command including the token:

```
📡 Formation server started on 10.0.0.1:4432
   Waiting for 2 more node(s) to join...
   Token expires in 30m0s

   Other nodes should run:
   sudo spx admin join --host 10.0.0.1:4432 --token spx_join_a8Bf3x9Kz2mN --node <name> --bind <ip>
```

**Server 2 — Join** (while init is running):

```bash
sudo spx admin join \
  --node node2 --bind $SPINIFEX_NODE2 --cluster-bind $SPINIFEX_NODE2 \
  --host $SPINIFEX_NODE1:4432 --token <token-from-init-output> \
  --region $AWS_REGION --az $AWS_AZ
```

**Server 3 — Join** (while init is running):

```bash
sudo spx admin join \
  --node node3 --bind $SPINIFEX_NODE3 --cluster-bind $SPINIFEX_NODE3 \
  --host $SPINIFEX_NODE1:4432 --token <token-from-init-output> \
  --region $AWS_REGION --az $AWS_AZ
```

**Note:** The join token expires 30 minutes after init by default. For larger deployments with slower provisioning, use `--token-ttl 2h`

## Step 5. Start Services

On **all servers**:

```bash
sudo systemctl start spinifex.target
```

## Step 6. Verify

From any node:

```bash
export AWS_PROFILE=spinifex
aws ec2 describe-instance-types
```

If this returns a list of available instance types, your cluster is working.

**Congratulations! Your Spinifex cluster is installed.**

Continue to [Setting Up Your Cluster](/docs/setting-up-your-cluster) to import an AMI, create a VPC, and launch your first instance.

## Troubleshooting

### Nodes Not Joining

The init command must still be running when join executes. If init exited, re-run with `--force`.

```bash
curl -sk https://$SPINIFEX_NODE1:4432/health
```

### OVN Chassis Not Registering

```bash
sudo ovn-sbctl show
sudo ss -tlnp | grep 6642
```

### CA Certificate Not Trusted

```bash
sudo cp /etc/spinifex/ca.pem /usr/local/share/ca-certificates/spinifex-ca.crt
sudo update-ca-certificates
```

### Cross-Host VMs Cannot Communicate

```bash
sudo ovs-vsctl show | grep -i geneve
sudo ss -ulnp | grep 6081
```
