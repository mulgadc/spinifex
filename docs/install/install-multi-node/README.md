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
- [How the cluster's own state is replicated](#how-the-clusters-own-state-is-replicated)
- [Hardware](#hardware)
- [Network requirements](#network-requirements)
- [Prerequisites](#prerequisites)
- [Instructions](#instructions)
- [Verifying the cluster](#step-6-verify-the-cluster)
- [Setting up your cluster](#step-7-set-up-your-cluster)
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
| **Cluster state** — NATS JetStream | Every key-value bucket is replicated onto all three nodes, so the control plane keeps its own state through the loss of any one. |

On one or two servers none of that holds. OVN runs standalone on the first node, and the storage metadata quorum has no majority to lose. If that node goes down, running instances keep full networking — but nothing can *change*: no new VPCs, no launches, no security group edits. See [OVN control plane on multi-node clusters](/docs/vpc-networking#ovn-control-plane-on-multi-node-clusters).

Servers beyond the third run the full set of services — storage, gateway and networking agents — and add their capacity to the pool. What they do not do is join the OVN database cluster, which stays at three members, so write latency there stays flat as the cluster grows.

### How the cluster's own state is replicated

Guest data has an obvious home: volumes live in Viperblock, objects in Predastore, both erasure coded across the cluster. The control plane's *own* state has a less obvious one, and it matters just as much. Every instance record, every VPC and subnet, the node roster, IAM users and access keys, quotas, DNS records and the leases reconcilers hold are all kept in **NATS JetStream key-value buckets**, replicated by Raft across the servers you installed. A cluster that has lost its state has lost the ability to describe, launch, terminate or route anything, even while every guest is still running.

**A bucket is replicated across as many nodes as the cluster has, up to five.** That number is decided once, by the `--nodes` count you give `spx admin init` in Step 4, and every service on every node reads it back out of `/etc/spinifex/spinifex.toml` before it creates anything.

| Servers | Replicas per bucket | Quorum | Survives |
|---|---|---|---|
| 1 | 1 | 1 | nothing — a single node is a single point of failure by definition |
| 3 | 3 | 2 | any 1 node lost |
| 4 | 4 | 3 | any 1 node lost |
| 5 | 5 | 3 | any 2 nodes lost |
| 10 | 5 | 3 | any 2 of the 5 nodes holding each bucket |

Five is JetStream's own ceiling on a stream's replica count, not a choice of ours. Past five servers the buckets stay at five and NATS spreads which five nodes hold each one, so the cluster keeps growing while the cost of a write does not.

#### Worked example — three servers

Say you ran Step 4 with `--nodes 3` on `node1`, `node2` and `node3`. Every KV bucket the cluster creates is a three-member Raft group, one member per server:

```
KV bucket "spinifex-instance-state"        KV bucket "spinifex-vpcd-reconcile"
┌──────────┬──────────┬──────────┐         ┌──────────┬──────────┬──────────┐
│  node1   │  node2   │  node3   │         │  node1   │  node2   │  node3   │
│  leader  │ follower │ follower │         │ follower │  leader  │ follower │
└──────────┴──────────┴──────────┘         └──────────┴──────────┴──────────┘
        quorum = 2 of 3                            quorum = 2 of 3
```

Three things follow from that picture, and they are the whole reason three servers is the floor:

- **A write is only acknowledged once a majority has it.** Launching an instance is not confirmed to the caller until at least two of the three servers have the record on disk. Losing one server afterwards cannot lose that instance.
- **Losing a node costs an election, not the data.** If `node1` goes down, `node2` and `node3` are still a majority. They elect a new leader for the buckets `node1` led — seconds on a healthy cluster — and the API keeps answering throughout. No bucket becomes unreadable, because no bucket lived only on `node1`.
- **Leader leases keep working, which is what keeps the cluster self-healing.** The reconcilers that repair VPC state, enforce quotas and publish DNS each hold a lease in a bucket of their own (`spinifex-vpcd-reconcile`, `spinifex-quota-reconcile`, `spinifex-dns-reconcile`, and the ECS and RDS leader buckets). Acquiring a lease is a write, so a lease bucket without a quorum cannot be acquired by anyone — every reconciler sharing it would stop cluster-wide at exactly the moment a node had failed and there was repair work to do. On three replicas the survivors take the leases over instead.

Contrast that with a bucket on **one** replica. It lives on one server, chosen by JetStream and recorded in no configuration file you can inspect. Stopping, rebooting or partitioning that one server makes the bucket return `nats: no responders available for request` on *every* node at once. A single-node event becomes a cluster-wide outage, and which node it was is not something you decided or can see from the config.

#### Growing an existing cluster

Adding a fourth server does not, on its own, re-replicate the buckets that already exist — the nodes that hold them are perfectly healthy, so nothing prompts a change. Spinifex raises them for you at the next service start: each service checks every bucket it opens against the cluster's current node count and raises any that are short. It never lowers one.

**A config that names a node before that node is serving is safe.** It is the ordinary middle of growing a cluster, and services start normally through it: a bucket that cannot be raised yet keeps working at the count it has, and the node logs `Could not raise KV bucket to the cluster's replica count`. The raise is retried every time a service opens the bucket, so it completes on its own once the new server is in the cluster.

To do it immediately rather than waiting for a restart, run the audit with `--repair` from any node:

```bash
sudo spx admin kv replicas --repair
```

It changes nothing about a bucket except the replica count, never lowers one, and is safe to run twice or on a healthy cluster.

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

Predastore uses the same three ports on every server, so the surface does not widen as the cluster grows. See [Predastore Distributed Storage](/docs/predastore-distributed-storage).

Those are the ports your **network** has to permit between the servers — switches, upstream firewalls, and anything else in the path.

Spinifex's own **host** firewall is separate, and already carries this policy — see [Host Firewall](/docs/host-firewall). It ships armed on the ISO path and off on the binary installer path. Two things follow from that, both of which the ISO box in Step 2 acts on:

- **The formation port needs nothing from you.** `spx admin init` opens 4432 to any source for the length of the formation window and closes it again afterwards, because a node dialling in to join is not a peer yet. The handshake behind it is TLS 1.3 with a bearer token.
- **The rest of the cluster plane is peer-scoped**, and that includes the OVN database ports Step 3 uses. Nodes that do not yet know each other cannot reach them, which is why an ISO-installed node has its firewall taken down before Step 3 and re-armed once the cluster is verified.

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
> Turn it back on once the cluster is verified — see [Host Firewall](/docs/host-firewall).

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

`--force` is in every command below so the sequence is identical whichever way you installed. It does the work on ISO-installed servers, which arrive as their own single-node cluster: joining replaces that server's CA and master key with server 1's and removes its JetStream store, and `--force` is the confirmation. On `spx admin init` it is idempotent — existing keys, credentials and CA are preserved, and only the config files and server certificate are refreshed. On a freshly installed server there is nothing to lose either way.

Every server also discards the JetStream store its single-node cluster wrote. Server 1 removes its own once every server has joined, and each joining server removes its own when it joins. Keeping them is never safe: NATS would adopt each server's copy as a replica of the cluster's stream, and those copies never converge. This is automatic whenever `--nodes` is 2 or more. `--discard-jetstream=false` makes init refuse to form over a non-empty store instead, for a server whose store you want to inspect first. Stop `spinifex.target` on every server first and confirm `pgrep -af 'spx service'` prints nothing: init and join both refuse while NATS is still running.

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

It is also what decides how many nodes the cluster's JetStream state is replicated across, so it is worth getting right rather than raising later — see [How the cluster's own state is replicated](#how-the-clusters-own-state-is-replicated). Every joining server takes the count from server 1, so it is chosen once, here.

IPsec encrypts the Geneve overlay between servers and is on by default. Joining servers take the setting from server 1, so it is chosen once, on init. On servers that share a trusted private link, `--ipsec=false` leaves the overlay unencrypted in exchange for considerably higher throughput between instances.

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

**3. The cluster's own state is replicated across every node.**

```bash
sudo spx admin kv replicas
```

```
spinifex@node1:~$ sudo spx admin kv replicas
BUCKET                         REPLICAS  WANT  STATUS  HELD BY
spinifex-cluster-state         3         3     ok      node1,node2,node3
spinifex-dns-reconcile         3         3     ok      node2,node1,node3
spinifex-iam                   3         3     ok      node3,node1,node2
spinifex-instance-state        3         3     ok      node1,node2,node3
spinifex-quota-reconcile       3         3     ok      node2,node3,node1
spinifex-terminated-instances  3         3     ok      node3,node2,node1
spinifex-vpcd-reconcile        3         3     ok      node2,node1,node3

47 buckets, 0 under-replicated
```

What to check:

- **`0 under-replicated`.** Anything else means a bucket lives on fewer nodes than depend on it — see [Buckets report as under-replicated](#buckets-report-as-under-replicated). The command exits 1 in that case, so it can gate a deployment without parsing anything.
- **`WANT` equals your node count**, capped at five. If it says 1 on a three-server cluster, the node the command ran on is not configured for three — check `--nodes` in Step 4 and the `[nodes.*]` sections of that host's `/etc/spinifex/spinifex.toml`.
- **`HELD BY` names as many distinct servers as `REPLICAS`.** The first name is the current leader for that bucket, and leadership is expected to be spread across the cluster rather than parked on one node.

Run it on every node, not just one. It reads cluster-global Raft metadata, so all of them should give the same answer; one that disagrees is not fully part of the cluster.

**4. The AWS API answers.**

```bash
export AWS_PROFILE=spinifex
aws ec2 describe-instance-types
```

A list of instance types means the gateway, IAM and the cluster behind them are all working.

**Congratulations! Your multi-server Spinifex cluster is installed.**

## Step 7. Set Up Your Cluster

The cluster is running, but it holds nothing yet — no machine images, no networks, no instances.

Continue to [Setting Up Your Cluster](/docs/setting-up-your-cluster) to import an AMI, create an SSH key pair, create a VPC with a public subnet, and launch your first instance. It ends by arming the [host firewall](/docs/host-firewall), which is also where you re-arm it if you turned it off to form the cluster.

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

Turn the firewall off on **every** node and retry the join, then re-arm once the cluster is up — see [Host Firewall](/docs/host-firewall). The joining node retries for 20 minutes by default, so it is often still waiting while you fix this.

### Join Refuses: "this node is already initialized"

The node has its own cluster configuration — normal for anything installed from the ISO, which initializes a single-node cluster at first boot. Joining replaces that node's CA and master key with the primary's, so it must be confirmed with `--force`.

Safe on a freshly installed node. On one that has been in service it orphans every volume and fragment sealed under the old key, so check before forcing.

### Buckets Report as Under-Replicated

```bash
sudo spx admin kv replicas
```

A bucket showing `UNDER-REPLICATED` lives on fewer nodes than the cluster has, so losing the node holding it takes it down for the whole cluster. Two things put a bucket in that state, and the fix is the same for both:

```bash
sudo spx admin kv replicas --repair
```

The first cause is **a cluster that grew**: buckets created when there were three servers stay on three replicas until something raises them. Services do this themselves as they restart, and `--repair` does it now.

The second is **a node that was down when the bucket was created**, so the Raft group formed short. Bring the node back first — `--repair` on an incomplete cluster raises the configured count, but the group cannot place the extra replica until the node is there to hold it. Confirm with `spx get nodes` before repairing.

If `WANT` itself is wrong — 1 on a three-server cluster — the problem is not the buckets but that host's view of the cluster. Check it has all three `[nodes.*]` sections:

```bash
sudo grep -oP '^\[nodes\.\K[^.\]]+' /etc/spinifex/spinifex.toml | sort -u
```

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
