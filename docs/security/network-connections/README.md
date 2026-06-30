---
title: "External Connection Inventory"
description: "Operator inventory of inbound listeners and outbound connections on Spinifex nodes"
category: "Security"
sections:
  - overview
tags:
  - security
  - compliance
  - cmmc
  - network
  - connections
  - boundary
resources:
  - title: "NIST SP 800-171 Rev 3"
    url: "https://csrc.nist.gov/pubs/sp/800/171/r3/final"
  - title: "CMMC Level 1 Self-Assessment Guide v2.0"
    url: "https://dodcio.defense.gov/CMMC/Documentation"
  - title: "NIST SP 800-41 Rev 1 — Guidelines on Firewalls and Firewall Policy"
    url: "https://csrc.nist.gov/pubs/sp/800/41/r1/final"
---

# External Connection Inventory

> Operator inventory of inbound listeners and outbound connections on Spinifex nodes

## Table of Contents

- [Overview](#overview)
- [CMMC Practices Covered](#cmmc-practices-covered)
- [Approach](#approach)
- [1. Inbound Listeners](#1-inbound-listeners)
- [2. Outbound Connections](#2-outbound-connections)
- [3. Cross-Node (Internal) Connections](#3-cross-node-internal-connections)
- [4. Limiting Controls](#4-limiting-controls)
- [5. Configuration Surface](#5-configuration-surface)
- [6. Operator Checklist](#6-operator-checklist)

---

## Overview

**Audience:** Operators deploying Spinifex into environments subject to CMMC Level 1, or any site that requires a documented inventory of system connections.

**Scope:** Network connections originated by or terminated at the Spinifex nodes — the Linux hosts running `spinifex-daemon`, `spinifex-awsgw`, `spinifex-nats`, `spinifex-predastore`, `spinifex-viperblock`, `spinifex-vpcd`, `spinifex-ui`, and the OVN control plane. Guest VM traffic is the workload owner's responsibility and out of scope.

**Boundary definition.** For the purposes of this document:

- **External** means outside the Spinifex cluster's trusted network perimeter — the public internet, the operator's corporate network, tenant users of the AWS API, and guest VMs.
- **Internal** means between Spinifex nodes inside the cluster subnet(s) defined in `spinifex.toml`.

AC.L1-3.1.20 applies specifically to **external** connections. Internal cluster connections are documented here as well so operators can build an accurate firewall policy.

## CMMC Practices Covered

This guide addresses AC.L1-3.1.20. The related boundary-protection practice SC.L1-3.13.1 is covered by OVN ACL and security-group enforcement in `vpcd` and is documented separately.

| Practice | Title | Objective |
|----------|-------|-----------|
| AC.L1-3.1.20 | External Connections | [a] Connections to external systems are identified. [b] The use of external systems is identified. [c] Connections to external systems are verified. [d] The use of external systems is verified. [e] Connections to external systems are controlled/limited. [f] The use of external systems is controlled/limited. |

## Approach

Spinifex has a small, enumerable set of network surfaces:

1. **Inbound listeners** — the TCP/UDP ports each node binds. These are the attack surface exposed to whoever can reach the node.
2. **Outbound connections** — the destinations the node's services reach out to. Today this is a short list: peer Spinifex nodes, OS image mirrors, and install telemetry.
3. **Cross-node connections** — inter-node control- and data-plane traffic inside the cluster subnet.

The inventory in [§1](#1-inbound-listeners)–[§2](#2-outbound-connections) satisfies objectives [a]/[b]. The **Auth / Verification** columns throughout satisfy [c]/[d]. [§4](#4-limiting-controls) and [§5](#5-configuration-surface) satisfy [e]/[f]. The default install meets [c]–[f] for every listed connection; the operator's remaining work is to record the inventory in the system security plan, apply host/network firewall rules per [§4](#4-limiting-controls), and audit [§5](#5-configuration-surface) on a recurring schedule.

## 1. Inbound Listeners

"Scope" classifies intended reach:

- **External** — reachable by tenant/operator networks. Authenticated and TLS-protected.
- **Cluster** — reachable only from peer Spinifex nodes. Operator must restrict via host or network firewall.
- **Localhost** — bound to `127.0.0.1`; not reachable off-node.

| Port | Service | Protocol | Scope | Purpose | Auth / Verification |
|------|---------|----------|-------|---------|--------------------|
| 9999 | spinifex-awsgw | HTTPS | External | AWS-compatible API (EC2, S3, ELBv2, IAM) — customer endpoint | AWS SigV4 + TLS (cluster CA) |
| 3000 | spinifex-ui | HTTPS | External | Operator web dashboard | Session cookie + TLS |
| 22 | OpenSSH | SSH | External | Operator administration | Key-based auth (operator-managed) |
| 4432 | Formation server | HTTPS | External (bootstrap only) | Cluster join coordination; active only while a join token is valid. See *Formation port lifecycle* below. | Short-lived bearer token + TLS¹ |
| 4222 | spinifex-nats (client) | NATS + TLS | Cluster | Internal service bus for EC2/EBS/VPC/S3 handlers | Token + mutual TLS (cluster CA) |
| 4248 | spinifex-nats (cluster) | NATS + TLS | Cluster | Inter-node NATS federation | Token + mutual TLS (cluster CA) |
| 8443 | spinifex-predastore | HTTPS | Cluster | S3-compatible object storage (AMIs, snapshots, user objects) | AWS SigV4 + TLS |
| 6660–6662 | predastore (Raft) | TCP | Cluster | Metadata consensus (3 nodes) | Cluster network only |
| 9991–9993 | predastore (data shards) | TCP | Cluster | Erasure-coded data shard transport | Cluster network only |
| 6641 | OVN Northbound DB (client) | OVSDB/TCP | Cluster | Logical network topology consumed by vpcd | Cluster network only; TLS planned |
| 6642 | OVN Southbound DB (client) | OVSDB/TCP | Cluster | Chassis / port / MAC binding state | Cluster network only; TLS planned |
| 6643 | OVN Northbound DB (RAFT) | OVSDB/TCP | Cluster | NB database RAFT replication between the 3 quorum nodes | Cluster network only; TLS planned |
| 6644 | OVN Southbound DB (RAFT) | OVSDB/TCP | Cluster | SB database RAFT replication between the 3 quorum nodes | Cluster network only; TLS planned |
| 8222 | spinifex-nats (monitoring) | HTTP | Localhost | `varz`/`subsz` metrics consumed by the daemon | Loopback only |
| socket / dynamic TCP | nbdkit (Viperblock) | NBD | Host-local / cluster | Block device transport for guest EBS volumes | Unix socket by default; TCP only in remote/DPU mode |

¹ **Formation port lifecycle.** 4432 opens during `spx admin init` / `spx admin join` while a bootstrap token is outstanding and closes once the cluster is formed (token TTL default 30 min, `--token-ttl`). The server presents an ephemeral self-signed cert that pre-dates trust bootstrap, so the joining node dials with `InsecureSkipVerify` — the only production dial that does. Authenticity rests on the operator supplying the leader address out-of-band plus possession of the bearer token. Document in the security plan so reviewers do not flag 4432 as a persistent open port.

**Development-only listeners.** When `dev_networking=true`, QEMU opens arbitrary host TCP ports for SSH port-forwarding into guest VMs. Production installs (the `/etc/spinifex` layout) do not enable this; it must not appear on compliance nodes.

## 2. Outbound Connections

Spinifex nodes initiate a small, fixed set of outbound connections.

**To external destinations:**

| Destination | Purpose | Protocol | Verification |
|-------------|---------|----------|--------------|
| `https://cloud.debian.org/images/cloud/trixie/` | Debian 13 cloud image download | HTTPS | TLS + checksum verification |
| `https://cloud-images.ubuntu.com/resolute/` | Ubuntu 26.04 LTS cloud image download | HTTPS | TLS + checksum verification |
| `https://d2yp8ipz5jfqcw.cloudfront.net` | Alpine image for managed HAProxy load-balancer | HTTPS | TLS + checksum verification |
| `https://install.mulgadc.com/install` | One-shot install telemetry POST on `spx admin init` / `join`. | HTTPS | TLS |

**To peer nodes (cluster-internal):** NATS federation (4248), Predastore S3 (8443), OVN NB/SB (6641/6642) — see [§3](#3-cross-node-internal-connections) for encryption and verification of each. The daemon also probes local Predastore Raft status at `<bind IP>:6660/status` (TLS, cluster CA) and local NATS monitoring at `127.0.0.1:8222/varz` (loopback HTTP).

**Update checks and metadata.** Spinifex does not check for updates and does not consume a cloud metadata service (`169.254.169.254` is served *by* the cluster to guest VMs). Node software updates come from the operator's OS package channel. The install-telemetry endpoint above is the only vendor-operated destination contacted by a node; closed-egress deployments should disable it and record the opt-out in the security plan.

**Air-gapped deployments.** The three image URLs are the only destinations needed for the standard image catalogue. Mirror them locally and use `spx admin images import --file` with pre-staged files. Telemetry must also be disabled. See [Air-Gapped Install](/docs/install-airgapped).

## 3. Cross-Node (Internal) Connections

Control-plane and data-plane traffic between Spinifex nodes, for completeness and firewall planning:

| Connection | Port(s) | Encryption / Auth | Notes |
|-----------|---------|-------------------|-------|
| NATS cluster routes | 4248 | Mutual TLS + cluster token | Full mesh between NATS servers |
| Predastore S3 | 8443 | TLS + AWS SigV4 | Cross-node object reads/writes |
| Predastore Raft | 6660–6662 | Cluster network only | Metadata consensus |
| Predastore shards | 9991–9993 | Cluster network only | Erasure-coded data shards |
| OVN NB/SB (client) | 6641 / 6642 | Cluster network only (TLS planned) | Network control plane; vpcd and ovn-controller dial the quorum |
| OVN NB/SB (RAFT) | 6643 / 6644 | Cluster network only (TLS planned) | NB/SB database replication across the 3 quorum nodes |
| OVN tunnels (Geneve) | UDP 6081 | None | Tenant traffic overlay between chassis, inside the cluster subnet |

Nodes **must** sit on a network segment that is not routed to tenant/guest VMs or to the internet. Predastore Raft/shards and OVN DBs are cluster-internal and must not be reachable from anywhere else.

## 4. Limiting Controls

Default external surface is three listeners — **9999** (AWS API), **3000** (UI), **22** (SSH) — plus **4432** transiently during bootstrap. Every other listener is cluster-only and the operator must enforce this with a host firewall (`nftables`/`iptables`/`firewalld`) or an upstream network ACL. Minimal `nftables` reference:

```
# External: from anywhere
tcp dport { 22, 9999, 3000 } accept

# Cluster-only: replace 10.0.1.0/24 with your cluster CIDR
ip saddr 10.0.1.0/24 tcp dport { 4222, 4248, 8443, 6641-6644, 6660-6662, 9991-9993 } accept
ip saddr 10.0.1.0/24 udp dport 6081 accept

# Default deny
tcp dport 0-65535 drop
udp dport 0-65535 drop
```

Port 4432 must be closed outside the bootstrap window; `spx admin join` opens it transiently. Outbound egress can be limited to the image-catalogue hostnames in [§2](#2-outbound-connections) plus the operator's OS package repositories; on air-gapped nodes, block all outbound HTTPS and use `spx admin images import --file`.

## 5. Configuration Surface

Every listener and outbound destination is controlled by one of these files. Changes require a service restart.

| File | Keys | Controls |
|------|------|----------|
| `/etc/spinifex/spinifex.toml` | `nodes.<node>.{awsgw,nats,predastore,daemon}.host`, `nodes.<node>.vpcd.ovn_{nb,sb}_addr`, `nodes.<node>.daemon.dev_networking` | Per-service bind addresses/ports; dev-mode QEMU port forwarding. |
| `/etc/spinifex/nats.conf` | `listen`, `cluster.listen`, `cluster.routes`, `http`, `tls`, `cluster.authorization` | NATS client/cluster/monitoring listeners, peer routes, TLS, cluster token. |
| `/etc/spinifex/predastore.toml` | `host`, `port`, `[[db]].port`, `[[nodes]].port`, `tls.*` | Predastore S3 listener, Raft ports, shard ports, TLS certs. |
| OVN packages (`ovn-central`, `ovn-host`) | `ovn-nb-db`, `ovn-sb-db` (via `ovs-vsctl set open_vswitch …`) | OVN DB bind addresses. |
| Spinifex UI service | Built-in defaults: `host = "0.0.0.0"`, `port = 3000`. No `spinifex.toml` block today. | UI listener. |
| `spx admin init` / `spx admin join` | `--port`, `--token-ttl`, `--no-telemetry` (or `SPX_NO_TELEMETRY=1`) | Formation port, token TTL, telemetry opt-out. |
| `utils/images.go` `AvailableImages` | Image catalogue URLs | Outbound HTTPS destinations for image downloads. |

## 6. Operator Checklist

- Inventory recorded in the system security plan — inbound ([§1](#1-inbound-listeners)), outbound ([§2](#2-outbound-connections)), cross-node ([§3](#3-cross-node-internal-connections)) — matches what is observed on the node (`ss -tlnp`, `ss -unlp`).
- Host firewall enforces the external/cluster/localhost split in [§4](#4-limiting-controls): external surface limited to 9999, 3000, 22 (and 4432 only during bootstrap).
- Cluster subnet is isolated from tenant guest VM networks and from the public internet.
- Formation port 4432 is closed on nodes not actively running a bootstrap token.
- Outbound HTTPS restricted to the [§2](#2-outbound-connections) image-catalogue hosts, or replaced with air-gapped import.
- Install telemetry (`install.mulgadc.com`) is either permitted and recorded in the security plan, or disabled via `SPX_NO_TELEMETRY=1` / `--no-telemetry`.
- OVN NB/SB client and RAFT ports (6641–6644) exposure limited to the cluster subnet pending the L2 TLS work.
- SSH (22) configured to operator-managed keys only; password auth disabled in `sshd_config`.
- Periodic review (at least annually, and after any topology change) confirms this inventory still matches the deployed configuration.
