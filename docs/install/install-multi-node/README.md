---
title: "Multi-Node Install"
seoTitle: "Multi-Node Spinifex Cluster Install — Spinifex Docs"
description: "Deploy Spinifex across three or more servers to form an availability zone with clustered OVN, replicated object storage, and automatic cluster formation."
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
  - title: "VPC Networking"
    url: "/docs/vpc-networking"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "Predastore (S3)"
    url: "https://github.com/mulgadc/predastore"
  - title: "Viperblock (EBS)"
    url: "https://github.com/mulgadc/viperblock"
---

# Multi-Node Installation

> Deploy Spinifex across three or more servers to create an availability zone.

## Table of Contents

- [Overview](#overview)
- [Cluster sizing](#cluster-sizing)
- [Hardware](#hardware)
- [Network requirements](#network-requirements)
- [Prerequisites](#prerequisites)
- [Instructions](#instructions)
- [Verifying the cluster](#step-6-verify-the-cluster)
- [Firewall and cluster membership](#firewall-and-cluster-membership)
- [How multi-node storage works](#how-multi-node-storage-works)
- [Troubleshooting](#troubleshooting)

---

## Overview

> [!IMPORTANT]
> **This guide builds a three-node cluster** — the minimum we recommend for any multi-server
> deployment. Every step below is written for three servers and uses `$SPINIFEX_NODE1`,
> `$SPINIFEX_NODE2` and `$SPINIFEX_NODE3`.
>
> **Running more than three?** Install the first three exactly as described, then repeat
> Steps 1, 2, 3 and the join in Step 4 for each additional server. The only difference is in
> Step 3: the OVN database cluster stays at three members, so servers four and beyond point at
> those three instead of joining them.

A Spinifex cluster distributes services across multiple servers for high availability, data durability, and fault tolerance. Cluster formation is automatic — the init node waits for its peers to join, then distributes credentials, CA certificates, and configuration.

**Installing on bare metal?** Step 1 can be done either with the binary installer or by booting each server from the Spinifex ISO — see [Bootable USB Install](/docs/install-usb). The ISO installs the operating system, disks and network configuration as well as Spinifex itself, which makes it the better option for servers with no existing OS. Either way, install every server first, then return here and continue from Step 2.

### Cluster sizing

**Three servers is the minimum we recommend.** Three is the point at which every distributed layer can lose a node and keep running:

| Layer | What three servers gives you |
|---|---|
| **VPC networking** — OVN | Control-plane databases run clustered, surviving the loss of any one node. |
| **Object storage** — Predastore (S3) | Objects are erasure coded `RS(2,1)`, surviving the loss of any one node's shards. |
| **Block storage** — Viperblock (EBS) | Volumes are stored in Predastore, so they inherit the same durability. |

On one or two servers none of that holds. OVN runs standalone on the first node, and the storage metadata quorum has no majority to lose. If that node goes down, running instances keep full networking — but nothing can *change*: no new VPCs, no launches, no security group edits. See [OVN control plane on multi-node clusters](/docs/vpc-networking#ovn-control-plane-on-multi-node-clusters).

Servers beyond the third run the full set of services — storage, gateway and networking agents — and add their capacity to the pool. What they do not do is join the OVN database cluster, which stays at three members, so write latency there stays flat as the cluster grows.

### Hardware

Per server, for the three-node minimum:

| | Minimum | Recommended |
|---|---|---|
| **Nodes** | 3 | 3 or more |
| **RAM** | 32 GB | 128 GB |
| **CPU** | 16 cores | 32+ cores |
| **OS / Spinifex disk** | SSD | NVMe |
| **NICs** | 2 — WAN 1 GbE, LAN/VPC 10 GbE+ | 2 — WAN 10 GbE, LAN/VPC 25 GbE+ |

Two NICs matters more than the raw numbers. One carries WAN traffic; the other carries LAN and VPC traffic between nodes — Geneve tunnels, object shards and OVN replication all cross it, so it is the interface that wants the bandwidth. A single-NIC server will work, but inter-node storage and tunnel traffic then competes with everything going in and out of the cluster.

### Network requirements

Open between all hosts:

| Protocol and port | Used by |
|---|---|
| UDP 6081 | Geneve tunnels |
| TCP 4222, 4248 | NATS |
| TCP 6641, 6642 | OVN northbound and southbound |
| TCP 8443 | Predastore S3 gate |
| UDP 6660, 7660 | Object shards, metadata consensus |

Open between the three OVN database servers (servers 1 to 3) only:

| Protocol and port | Used by |
|---|---|
| TCP 6643, 6644 | OVN database clustering |

Predastore uses the same three ports on every server, so the surface does not widen as the cluster grows. See [How multi-node storage works](#how-multi-node-storage-works).

Those are the ports your **network** has to permit between the servers — switches, upstream firewalls, and anything else in the path.

Spinifex's own **host** firewall is separate, and already carries this policy. It ships armed on the ISO path and off on the binary installer path. Two things follow from that, both of which the ISO box in Step 2 acts on:

- **The formation port needs nothing from you.** `spx admin init` opens 4432 to any source for the length of the formation window and closes it again afterwards, because a node dialling in to join is not a peer yet. The handshake behind it is TLS 1.3 with a bearer token.
- **The rest of the cluster plane is peer-scoped**, and that includes the OVN database ports Step 3 uses. Nodes that do not yet know each other cannot reach them, which is why an ISO-installed node has its firewall taken down before Step 3 and re-armed after Step 6.

## Prerequisites

**Installed from the ISO (Option B)?** The bridge is configured for you — the ISO sets up the host's network interfaces, `br-wan` included. Skip to [Instructions](#instructions).

> [!IMPORTANT]
> **Binary installer only — a WAN bridge is required on every node.**
>
> Before running the installer on any server, that server's WAN interface **must** already be enslaved to a Linux bridge named `br-wan`. The host IP, default route, and DHCP must all live on the bridge — not on the bare NIC.
>
> The binary installer does **not** create this bridge for you yet. Running it on a host whose default route is still on a bare NIC will leave the install in a non-working state. Auto-provisioning of `br-wan` will land in a future release.
>
> **Verify on every node before continuing:**
>
> - `ip -br link show br-wan` — bridge exists and is `UP`
> - `ip route` — default route's `dev` is `br-wan`
>
> See [VPC Networking → Bridge Setup](/docs/vpc-networking#bridge-setup-physical-network-wiring) for the topology.

## Instructions

## Step 1. Install Spinifex on Each Server

Choose one method and apply it to **every** server in the cluster.

**Option A — existing OS.** On a server already running Ubuntu 26.04 or Debian 13:

```bash
curl -fsSL https://install.mulgadc.com | bash
```

**Option B — bare metal, from the ISO.** Boot each server from the Spinifex ISO and follow [Bootable USB Install](/docs/install-usb). This installs the operating system, partitions the disks, and configures the hostname and network interfaces alongside Spinifex. The ISO installer does not form a cluster — that is what the remaining steps do.

Complete this step on all three servers before continuing. Step 4 requires every node to be installed, reachable, and available at the same time.

## Step 2. Set Node IP Variables

On **each server**, export the management IPs of all three nodes plus the region and AZ. The same values go on every server:

```bash
export SPINIFEX_NODE1=192.168.1.10
export SPINIFEX_NODE2=192.168.1.11
export SPINIFEX_NODE3=192.168.1.12
export AWS_REGION=us-east-1
export AWS_AZ=us-east-1a
```

Adding a fourth server or more? Export `SPINIFEX_NODE4` and so on alongside these — the first three stay as they are, because they remain the OVN database nodes.

> [!NOTE]
> **ISO installs only — skip this box if you used the binary installer (Option A).**
>
> The ISO brings each server up as a **running standalone single-node cluster** with its firewall armed, so forming a cluster is a conversion rather than a fresh setup. One thing is needed, on **every** server, before Step 3:
>
> ```bash
> sudo systemctl stop spinifex.target
> sudo /usr/local/lib/spinifex/spinifex-firewall-apply disable
> ```
>
> Each node's firewall currently trusts only itself, because that is the whole cluster as far as it knows, and the cluster plane is peer-scoped — so the OVN database connections Step 3 makes between servers are dropped. Stopping `spinifex.target` is not enough: the firewall lives in the kernel and outlives the services.
>
> Turn it back on after Step 6 — see [Firewall and cluster membership](#firewall-and-cluster-membership).

## Step 3. Set Up OVN Networking

Servers 1 to 3 run a clustered **OVN database** — the VPC networking control plane — so it survives losing any one of them. This is the only database limited to three members; storage and NATS run on every server. **Server 1 creates the cluster and must be set up first**; servers 2 and 3 then join it.

`--recreate-db` appears in each command below because `ovn-central` starts a standalone OVN database when the package installs, and a clustered one can only be created from scratch. It replaces that standalone database on both the binary and ISO paths.

If your WAN interface is already a bridge, `setup-ovn.sh` auto-detects it. Otherwise add `--wan-bridge=br-wan --wan-iface=eth1` for a dedicated WAN NIC.

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

**Servers 4 and beyond** — repeat for each one, substituting its own address. They point at the three OVN database servers rather than joining the database cluster, so they survive any one of those failing:

```bash
sudo /usr/local/share/spinifex/setup-ovn.sh \
  --ovn-remote=tcp:$SPINIFEX_NODE1:6642,tcp:$SPINIFEX_NODE2:6642,tcp:$SPINIFEX_NODE3:6642 \
  --encap-ip=$SPINIFEX_NODE4
```

Verify the OVN database cluster formed, then that every chassis registered with it:

```bash
sudo ovn-appctl -t /var/run/ovn/ovnnb_db.ctl cluster/status OVN_Northbound
sudo ovn-sbctl show
```

`cluster/status` should list three servers with one leader, and `ovn-sbctl show` a chassis for every node in the cluster. If `cluster/status` reports a standalone database, the OVN cluster did not form — re-check that `--db-cluster-local-addr` was passed on servers 1, 2 and 3.

## Step 4. Form the Cluster

Run init and join **concurrently** — init blocks until all nodes have joined.

`--force` is in every command below so the sequence is identical whichever way you installed. It does the work on ISO-installed servers, which arrive as their own single-node cluster: joining replaces that server's CA and master key with server 1's, and `--force` is the confirmation. On `spx admin init` it is idempotent — existing keys, credentials and CA are preserved, and only the config files and server certificate are refreshed. On a freshly installed server there is nothing to lose either way.

> [!WARNING]
> Do not point these commands at a server that has already been in service. Joining discards its master key, orphaning every volume and fragment sealed under it. That is what `--force` overrides, and it is unrecoverable.

**Server 1 — initialize:**

```bash
sudo spx admin init --force \
  --node node1 --nodes 3 \
  --bind $SPINIFEX_NODE1 --cluster-bind $SPINIFEX_NODE1 \
  --port 4432 --region $AWS_REGION --az $AWS_AZ
```

`--nodes 3` is the number of servers init waits for. Set it to your total node count if you are building a larger cluster.

The init output displays the join command including the token:

```
📡 Formation server started on 10.0.0.1:4432
   Waiting for 2 more node(s) to join...
   Token expires in 30m0s

   Other nodes should run:
   sudo spx admin join --host 10.0.0.1:4432 --token spx_join_a8Bf3x9Kz2mN --node <name> --bind <ip>
```

Take the **token** from that output, but run the commands below rather than the line it prints — they add `--force` and `--cluster-bind`.

**Server 2 — join** (while init is still running):

```bash
sudo spx admin join --force \
  --node node2 --bind $SPINIFEX_NODE2 --cluster-bind $SPINIFEX_NODE2 \
  --host $SPINIFEX_NODE1:4432 --token <token-from-init-output> \
  --region $AWS_REGION --az $AWS_AZ
```

**Server 3 — join** (while init is still running):

```bash
sudo spx admin join --force \
  --node node3 --bind $SPINIFEX_NODE3 --cluster-bind $SPINIFEX_NODE3 \
  --host $SPINIFEX_NODE1:4432 --token <token-from-init-output> \
  --region $AWS_REGION --az $AWS_AZ
```

Each additional server runs the same join command with its own `--node` name and `--bind` address.

**Note:** the join token expires 30 minutes after init by default. For larger deployments with slower provisioning, use `--token-ttl 2h`.

## Step 5. Start Services

On **all servers**:

```bash
sudo systemctl start spinifex.target
```

## Step 6. Verify the Cluster

Run these from any node. Together they confirm that every server joined, that services are healthy on each, and that capacity is being pooled across the cluster.

**1. Every node is present and Ready.**

```bash
spx get nodes
```

```
spinifex@node1:~$ spx get nodes
NAME  | STATUS | ROLES         | IP       | REGION    | AZ         | UPTIME | VMs | SERVICES
node1 | Ready  | nats:follower | 10.2.0.2 | us-east-1 | us-east-1a | 21h27m | 0   | nats,predastore,viperblock,daemon,awsgw,vpcd,ui
node2 | Ready  | nats:follower | 10.2.0.3 | us-east-1 | us-east-1a | 21h27m | 1   | nats,predastore,viperblock,daemon,awsgw,vpcd,ui
node3 | Ready  | nats:leader   | 10.2.0.4 | us-east-1 | us-east-1a | 21h27m | 0   | nats,predastore,viperblock,daemon,awsgw,vpcd,ui
```

What to check:

- **Every server you installed is listed.** A missing node never joined — see [Nodes not joining](#nodes-not-joining).
- **`STATUS` is `Ready`** on all of them. `NotReady` means the node is in the cluster configuration but is not answering, so start with `spinifex.target` on that host.
- **Exactly one `nats:leader`.** The rest are followers.
- **`SERVICES` lists the same set on every node.** A short list means something failed to start there; check `systemctl status` for the missing unit.

**2. Capacity is pooled across the cluster.**

```bash
spx top nodes
```

```
spinifex@node1:~$ spx top nodes
NAME  | CPU (used/total) | MEM (used/total) | GPU (used/total) | VMs
node1 | 0/64             | 0Mi/220.2Gi      | -                | 0
node2 | 2/64             | 2.8Gi/251.7Gi    | -                | 1
node3 | 0/64             | 0Mi/251.7Gi      | -                | 0


INSTANCE TYPE | AVAILABLE | VCPU | MEMORY
c6a.12xlarge  | 3         | 48   | 96.0Gi
c6a.16xlarge  | 0         | 64   | 128.0Gi
c6a.24xlarge  | 0         | 96   | 192.0Gi
c6a.2xlarge   | 21        | 8    | 16.0Gi
c6a.4xlarge   | 9         | 16   | 32.0Gi
c6a.8xlarge   | 3         | 32   | 64.0Gi
c6a.large     | 92        | 2    | 4.0Gi
c6a.xlarge    | 45        | 4    | 8.0Gi
c6i.12xlarge  | 3         | 48   | 96.0Gi
c6i.16xlarge  | 0         | 64   | 128.0Gi
c6i.24xlarge  | 0         | 96   | 192.0Gi
c6i.2xlarge   | 21        | 8    | 16.0Gi
c6i.4xlarge   | 9         | 16   | 32.0Gi
c6i.8xlarge   | 3         | 32   | 64.0Gi
c6i.large     | 92        | 2    | 4.0Gi
c6i.xlarge    | 45        | 4    | 8.0Gi
m6a.12xlarge  | 3         | 48   | 192.0Gi
```

The top table is per-node CPU, memory and GPU usage. The bottom table is what the cluster can actually launch right now: `AVAILABLE` is the number of instances of that type that would currently fit across all nodes. A type showing `0` does not fit on any single node — instances are not split across servers, so the largest type you can launch is bounded by your biggest node, not by the cluster total.

If capacity looks like a single server rather than the sum of your nodes, the others have not joined.

**3. The AWS API answers.**

```bash
export AWS_PROFILE=spinifex
aws ec2 describe-instance-types
```

A list of instance types means the gateway, IAM and the cluster behind them are all working.

**Congratulations! Your Spinifex cluster is installed.**

Continue to [Setting Up Your Cluster](/docs/setting-up-your-cluster) to import an AMI, create a VPC, and launch your first instance.

## Firewall and Cluster Membership

Spinifex ships an optional host firewall. It divides the node's ports into two groups:

| Group | Ports | Who can reach them |
|---|---|---|
| **Public** | SSH, 443, 3000 (console), 8443 (S3), 9999 (AWS gateway), 53 (DNS) | anyone |
| **Internal** | OVN, NATS, formation, Geneve and the rest of the cluster plane | **cluster members only** |

The internal group is the point. Before this existed, OVN and NATS were reachable from the public internet on a WAN-facing node.

"Cluster members" is not a list you maintain. Each node works it out from the cluster it belongs to and rewrites its own rules whenever membership changes — you never edit the peer list by hand.

### Is it on?

| How the node was installed | Firewall |
|---|---|
| From the ISO | **on** |
| Binary installer (`curl \| bash`) or `setup.sh` | **off** |
| `setup.sh --firewall=on` | **on** |

The binary installer defaults to off deliberately: it runs on servers that already have an operating system and services on them, and switching on a default-deny policy uninvited could cut off something Spinifex knows nothing about. **For production, turn it on** — either at install time:

```bash
curl -fsSL https://install.mulgadc.com | bash -s -- --firewall=on
```

or afterwards, by setting it in `/etc/spinifex/spinifex.toml` and restarting the daemon:

```toml
[network]
firewall_enabled = true
```

Before you do, check what else the machine is serving. Anything listening on a port outside the public group above stops accepting new connections.

### Turning it off and on around cluster changes

A node only recognises the members of the cluster it currently belongs to, so during formation — when the nodes do not yet know each other — internal traffic between them is blocked. Turn the firewall off while you form the cluster, and on again once it is up:

```bash
# Off — before forming or expanding a cluster. Run on every node.
sudo /usr/local/lib/spinifex/spinifex-firewall-apply disable

# On — once the cluster is formed and verified. Run on every node.
sudo systemctl restart spinifex-daemon
```

Restarting the daemon is what re-arms it: the node rebuilds its peer list from the cluster it is now part of, reloads the rules, and re-enables the boot-time unit so the policy survives a reboot. It also happens on its own within five minutes if you would rather wait.

### Checking it

```bash
sudo nft list table inet spinifex_filter
```

The peer list is an nft variable, expanded when the rules load, so it does not appear under a name of its own. Look instead at the `ip saddr { ... }` addresses on the cluster-plane rules — the ones accepting 4222, 6641, 6642 and the rest.

Every node's addresses should be there. On a multi-NIC node that means its WAN, LAN and VPC addresses, so expect several entries per node. A missing node means its cluster traffic is being dropped.

Dropped packets are logged, rate-limited, so this tells you whether a connection problem is the firewall or something else:

```bash
sudo journalctl -k | grep 'spinifex-fw drop'
```

## How Multi-Node Storage Works

Background reading — you do not configure any of this by hand. `spx admin init` and `spx admin join` build the topology from the servers that actually form the cluster in Step 4, and each machine gets the same file with its own host ID recorded in `spinifex.toml`.

Predastore is configured for the whole cluster in `/etc/spinifex/predastore/predastore.toml`. Each server is one `[[host]]` — a single Predastore process owning that machine's data directory and TLS identity — carrying three nodes under `[[host.node]]`:

| Role | Port | Purpose |
|---|---|---|
| `gate` | TCP 8443 | Serves the S3 API. Every server runs one, so any of them answers an S3 request. |
| `blob` | UDP 6660 | Holds erasure-coded object shards. One per machine. |
| `meta` | UDP 7660 | Member of the Raft quorum over global state — buckets and the object index. |

Ports have to be unique within a host but not across the cluster, so every machine uses the same three. Blob and meta traffic between hosts runs over QUIC, authenticated by the cluster CA; nodes on the same machine talk over an in-process pipe and open no socket, which is why a single-server install listens on 8443 alone.

Reed-Solomon parameters are chosen from the cluster size, since each machine contributes exactly one blob node: two servers get `RS(1,1)`, three or more get `RS(2,1)`. `RS(2,1)` survives the loss of any one server's shards — another reason three nodes is the recommended minimum.

## Troubleshooting

### Nodes Not Joining

The init command must still be running when join executes. If init exited, re-run with `--force`.

```bash
curl -sk https://$SPINIFEX_NODE1:4432/health
```

A hang rather than a quick failure means packets are being dropped; a refused connection means nothing is listening.

If it hangs, check node 1's init output before blaming the firewall — `spx admin init` opens the formation port itself while it waits, so this is usually not the cause. It prints `⚠️ Could not open port 4432 in the host firewall` when that fails, which is the case where it is. Confirm on node 1:

```bash
sudo journalctl -k | grep 'spinifex-fw drop'
```

Turn the firewall off on **every** node and retry the join, then re-arm once the cluster is up — see [Firewall and cluster membership](#firewall-and-cluster-membership). The joining node retries for 20 minutes by default, so it is often still waiting while you fix this.

### Join Refuses: "this node is already initialized"

The node has its own cluster configuration — normal for anything installed from the ISO, which initializes a single-node cluster at first boot. Joining replaces that node's CA and master key with the primary's, so it must be confirmed with `--force`.

Safe on a freshly installed node. On one that has been in service it orphans every volume and fragment sealed under the old key, so check before forcing.

### OVN Database Cluster Not Forming

```bash
sudo ovn-appctl -t /var/run/ovn/ovnnb_db.ctl cluster/status OVN_Northbound
```

If this reports a standalone database rather than three servers, the OVN database was created before the cluster flags were supplied. A clustered one can only be created from scratch — re-run Step 3 with `--recreate-db`.

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
