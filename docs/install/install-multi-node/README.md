---
title: "Multi-Node Install"
description: "Deploy Spinifex across multiple servers to create an availability zone with high availability, data durability, and fault tolerance."
category: "Install"
tags:
  - install
  - multi node
  - cluster
  - iso
  - bare-metal
resources:
  - title: "Bootable USB Install"
    url: "/docs/install-usb"
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
- [Object Storage Layout](#object-storage-layout)
- [Prerequisites](#prerequisites)
- [Instructions](#instructions)
- [Converting ISO-Installed Nodes](#converting-iso-installed-nodes)
- [Troubleshooting](#troubleshooting)

---

## Overview

A Spinifex cluster distributes services across multiple servers for high availability, data durability, and fault tolerance. Cluster formation is automatic — the init node waits for peers to join, then distributes credentials, CA certificates, and configuration.

**Installing on bare metal?** Step 1 below can be done either with the binary installer or by booting each server from the Spinifex ISO — see [Bootable USB Install](/docs/install-usb). The ISO installs the operating system, disks and network configuration as well as Spinifex itself, which makes it the better option for servers with no existing OS. Either way, install every server first, then return here and continue from Step 2.

**Cluster Sizing:**

**Three servers is the minimum recommended for a multi-server deployment.** At three or more, the OVN control-plane databases run clustered across three nodes and the cluster tolerates the loss of any one of them. Below three, OVN runs standalone on the first node, which becomes a single point of failure for control-plane operations.

A standalone OVN outage is less severe than it sounds. `ovn-controller` has already programmed the forwarding rules into each host, so **running instances keep full networking** — east-west, north-south, NAT and security groups all continue. What stops is change: creating VPCs, launching instances, and updating security groups all require the control plane.

Servers beyond the third join as compute nodes and do not run a database, which keeps write latency stable as the cluster grows.

**Network Requirements:**

- Minimum 1 NIC per server (2 recommended for production)
- UDP port 6081 open between hosts (Geneve tunnels)
- TCP ports 4222, 4248, 6641, 6642 open between hosts (NATS, OVN)
- TCP ports 6643, 6644 open between database servers (OVN database clustering)
- TCP port 8443 and UDP ports 6660, 7660 open between hosts (Predastore S3, object shards, metadata consensus)

Predastore uses the same three ports on every server, so the surface does not widen as the cluster grows. See [Object Storage Layout](#object-storage-layout) below.

## Object Storage Layout

Predastore is configured for the whole cluster in `/etc/spinifex/predastore/predastore.toml`. Each server is one `[[host]]` — a single Predastore process owning that machine's data directory and TLS identity — carrying three nodes under `[[host.node]]`:

| Role | Port | Purpose |
|------|------|---------|
| `gate` | TCP 8443 | Serves the S3 API. Every server runs one, so any of them answers an S3 request. |
| `blob` | UDP 6660 | Holds erasure-coded object shards. One per machine. |
| `meta` | UDP 7660 | Member of the Raft quorum over global state — buckets and the object index. |

Ports have to be unique within a host but not across the cluster, so every machine uses the same three. Blob and meta traffic between hosts runs over QUIC, authenticated by the cluster CA; nodes on the same machine talk over an in-process pipe and open no socket, which is why a single-server install listens on 8443 alone.

Reed-Solomon parameters are chosen from the cluster size, since each machine contributes exactly one blob node: two servers get `RS(1,1)`, three or more get `RS(2,1)`. `RS(2,1)` survives the loss of any one server's shards.

You do not configure any of this by hand. `spx admin init` and `spx admin join` build the topology from the servers that actually form the cluster in Step 4, and each machine gets the same file with its own host ID recorded in `spinifex.toml`.

> [!WARNING]
> **There is no upgrade path onto this layout.** A cluster installed before the object storage cutover cannot be migrated — the on-disk directory structure and the configuration schema both changed, and no migration rewrites either. Such a cluster has to be re-initialised from scratch, which discards its stored objects. Export anything you need first.

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

Choose one of the two methods below and apply it to **every** server in the cluster.

**Option A — existing OS.** On a server already running Ubuntu 26.04 or Debian 13:

```bash
curl -fsSL https://install.mulgadc.com | bash
```

**Option B — bare metal, from the ISO.** Boot each server from the Spinifex ISO and follow [Bootable USB Install](/docs/install-usb). This installs the operating system, partitions the disks, and configures the hostname and network interfaces alongside Spinifex. The ISO installer does not form a cluster — that is what the remaining steps do.

Complete this step on all servers before continuing. The nodes must be installed and reachable from one another before the cluster is formed, because Step 4 requires every node to be available at the same time.

### Shortcut: form the cluster with one command

Steps 2 to 6 are a fixed sequence, and `scripts/install-node.sh` in the Spinifex repository runs it for you over SSH — from a workstation that can reach every server, not from the servers themselves:

```bash
scripts/install-node.sh \
  --external-pool 10.0.1.100-10.0.1.150 \
  --external-gateway 10.0.1.1 --external-prefix-len 24 \
  server1 server2 server3
```

The first host initializes and the rest join. It resolves each node's plane addresses, rebuilds the OVN databases in clustered form, forms the cluster, restarts services and verifies the result — and `--dry-run` prints every command it would run without touching anything. It needs passwordless `sudo` on each host and requires all of them to be on the same Spinifex version.

The remaining steps document what it does, and are the path to follow when installing by hand or when something needs to be adjusted mid-way.

## Step 2. Set Node IP Variables

On **each server**, export the management IPs for all nodes:

```bash
export SPINIFEX_NODE1=192.168.1.10
export SPINIFEX_NODE2=192.168.1.11
export SPINIFEX_NODE3=192.168.1.12
export AWS_REGION=us-east-1
export AWS_AZ=us-east-1a
```

## Converting ISO-Installed Nodes

Skip this section if you installed with the binary installer (Option A).

The ISO installs each server as a **running standalone single-node cluster** — it initializes Spinifex, starts a standalone OVN database, and brings up `spinifex.target` at first boot. That is the right behaviour for a single server, and it means forming a cluster is a conversion rather than a fresh setup. Two extra things are needed:

**Before Step 3**, stop services on every server:

```bash
sudo systemctl stop spinifex.target
```

**In Step 4**, pass `--force` to `spx admin init` and `spx admin join`. Servers 2 and 3 each arrived with their own CA and master key from the single-node install, and joining replaces them with server 1's. `--force` is how you confirm that.

That discard is safe here — nothing has been sealed under those keys on a freshly installed node — but it is unrecoverable on a server that has been in service. `spx admin join` refuses without `--force` for exactly this reason.

## Step 3. Setup OVN Networking

All three servers run a clustered OVN database, so the control plane survives the loss of any one of them. Server 1 creates the cluster and must be set up first; servers 2 and 3 join it.

If your WAN interface is already a bridge, setup-ovn.sh auto-detects it. Otherwise use `--wan-bridge=br-wan --wan-iface=eth1` (dedicated WAN NIC).

> [!IMPORTANT]
> **`--recreate-db` destroys all logical network state** — every logical switch, router, port and ACL, and so every VPC on that node. It is required here because the `ovn-central` package starts a standalone database on install, and a clustered database can only be created from scratch. That is safe on a freshly installed server and is **not** safe on one already running workloads. If you are converting a server that has been in service, back up its NB database first.

**Server 1 — create the cluster:**

```bash
sudo /usr/local/share/spinifex/setup-ovn.sh \
  --management \
  --db-cluster-local-addr=$SPINIFEX_NODE1 \
  --recreate-db \
  --encap-ip=$SPINIFEX_NODE1
```

**Server 2** (after server 1 is ready):

```bash
sudo /usr/local/share/spinifex/setup-ovn.sh \
  --management \
  --db-cluster-local-addr=$SPINIFEX_NODE2 \
  --db-cluster-remote-addr=$SPINIFEX_NODE1 \
  --recreate-db \
  --encap-ip=$SPINIFEX_NODE2
```

**Server 3** (after server 1 is ready):

```bash
sudo /usr/local/share/spinifex/setup-ovn.sh \
  --management \
  --db-cluster-local-addr=$SPINIFEX_NODE3 \
  --db-cluster-remote-addr=$SPINIFEX_NODE1 \
  --recreate-db \
  --encap-ip=$SPINIFEX_NODE3
```

**Servers 4 and beyond** are compute nodes and run no database. Point them at all three database servers so they survive any one of them failing:

```bash
sudo /usr/local/share/spinifex/setup-ovn.sh \
  --ovn-remote=tcp:$SPINIFEX_NODE1:6642,tcp:$SPINIFEX_NODE2:6642,tcp:$SPINIFEX_NODE3:6642 \
  --encap-ip=$SPINIFEX_NODE4
```

Verify the database cluster formed, then that all chassis registered:

```bash
sudo ovn-appctl -t /var/run/ovn/ovnnb_db.ctl cluster/status OVN_Northbound
sudo ovn-sbctl show
```

`cluster/status` should list three servers with one leader. If it reports a standalone database, the cluster did not form — re-check that `--db-cluster-local-addr` was passed on every database server.

## Step 4. Form the Cluster

Run init and join concurrently — init blocks until all nodes join.

If you installed from the ISO, add `--force` to every command below (see [Converting ISO-Installed Nodes](#converting-iso-installed-nodes)).

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

### Join Refuses: "this node is already initialized"

The node has its own cluster configuration — normal for anything installed from the ISO, which initializes a single-node cluster at first boot. Joining replaces that node's CA and master key with the primary's, so it must be confirmed with `--force`.

Safe on a freshly installed node. On one that has been in service it orphans every volume and fragment sealed under the old key, so check before forcing.

### OVN Database Cluster Not Forming

```bash
sudo ovn-appctl -t /var/run/ovn/ovnnb_db.ctl cluster/status OVN_Northbound
```

If this reports a standalone database rather than three servers, the database was created before the cluster flags were supplied. A clustered database can only be created from scratch — re-run Step 3 with `--recreate-db`, remembering that it discards all logical network state.

### OVN Chassis Not Registering

```bash
sudo ovn-sbctl show
sudo ss -tlnp | grep 6642
```

### CA Certificate Not Trusted

On a node or any host running `spx`/`aws` against the cluster:

```bash
sudo cp /etc/spinifex/ca.pem /usr/local/share/ca-certificates/spinifex-ca.crt
sudo update-ca-certificates
```

Inside a guest VM there is no `/etc/spinifex`; fetch the CA from IMDS instead:

```bash
sudo curl -fsS http://169.254.169.254/spinifex/ca.pem \
  -o /usr/local/share/ca-certificates/spinifex-ca.crt
sudo update-ca-certificates
```

### Cross-Host VMs Cannot Communicate

```bash
sudo ovs-vsctl show | grep -i geneve
sudo ss -ulnp | grep 6081
```
