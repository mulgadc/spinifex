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

"Scope" classifies intended reach. It maps onto the node's network planes — see
*Planes and scope* below, because a node with fewer NICs collapses them:

- **External** — reachable by tenant/operator networks, on the `wan` plane. Authenticated and TLS-protected.
- **Cluster** — reachable only from peer Spinifex nodes, on the `lan` plane. Operator must restrict via host or network firewall.
- **Encap** — reachable only from peer chassis, on the `vpc` plane. Carries the tenant overlay and the IPsec that protects it.
- **Guest** — bound to a per-instance interface and reachable only by that instance's VM.
- **Localhost** — bound to `127.0.0.1`; not reachable off-node.

The listener invariant tests (`spinifex/network/invariants` and the multinode e2e suite) read
this table and fail any Cluster- or Encap-scope port found bound to the wildcard address,
unless that row's Purpose or Auth text contains the exact phrase **"binds the wildcard by
design"**. That phrase is load-bearing, not incidental wording — a row that merely mentions
"wildcard" or "0.0.0.0", negated or not, grants no exception. Adding a new wildcard-bound
Cluster/Encap listener means adding that literal phrase to its row, not just describing the
behavior in other words.

| Port | Service | Protocol | Scope | Purpose | Auth / Verification |
|------|---------|----------|-------|---------|--------------------|
| 9999 | spinifex-awsgw | HTTPS | External | AWS-compatible API (EC2, S3, ELBv2, IAM) — customer endpoint | AWS SigV4 + TLS (cluster CA) |
| 3000 | spinifex-ui | HTTPS | External | Operator web dashboard | Session cookie + TLS |
| 22 | OpenSSH | SSH | External | Operator administration | Key-based auth (operator-managed) |
| 53 | northstar | DNS (UDP + TCP) | External | Authoritative DNS for the cluster's zones, resolved directly by instances and by operator networks. Binds the node's advertise (wan) address specifically, not the wildcard, so it does not collide with the `systemd-resolved` stub. | None — public authoritative DNS |
| 8443 | spinifex-predastore (gate) | HTTPS | External | S3-compatible object storage (AMIs, snapshots, user objects). S3 is a public plane: the gate binds `0.0.0.0` by design. | AWS SigV4 + TLS |
| 4432 | Formation server | HTTPS | Cluster (bootstrap only) | Cluster join coordination; active only while a join token is valid. Binds the node's `--bind` (lan) address. See *Formation port lifecycle* below. | Short-lived bearer token + TLS¹ |
| 4222 | spinifex-nats (client) | NATS + TLS | Cluster | Internal service bus for EC2/EBS/VPC/S3 handlers | Token + mutual TLS (cluster CA) |
| 4248 | spinifex-nats (cluster) | NATS + TLS | Cluster | Inter-node NATS federation | Token + mutual TLS (cluster CA) |
| 5300 | northstar | DNS (UDP + TCP) | Cluster | Forward target for every node's per-instance DNS shim, dialled cross-node. Binds the wildcard by design. | None — restrict by firewall |
| 6660 | predastore (blob node) | QUIC / UDP | Cluster | Erasure-coded object shard transport between hosts. Multi-node clusters only — see *Predastore ports* below. | TLS 1.3, server certificate verified against the cluster CA |
| 7660 | predastore (meta node) | QUIC / UDP | Cluster | Raft consensus over global state — buckets and the object index — between hosts. Multi-node clusters only. | TLS 1.3, server certificate verified against the cluster CA |
| 6641 | OVN Northbound DB (client) | OVSDB/TCP | Cluster | Logical network topology consumed by vpcd. Binds `127.0.0.1` plus the node's lan-plane address (`--lan-addr`), never the wildcard address. On a node with no separate lan plane that address is the public one, and a host firewall is the only remaining control. | Cluster network only; TLS planned |
| 6642 | OVN Southbound DB (client) | OVSDB/TCP | Cluster | Chassis / port / MAC binding state. Binds `127.0.0.1` plus the node's lan-plane address (`--lan-addr`), never the wildcard address. On a node with no separate lan plane that address is the public one, and a host firewall is the only remaining control. | Cluster network only; TLS planned |
| 6643 | OVN Northbound DB (RAFT) | OVSDB/TCP | Cluster | NB database RAFT replication between the 3 quorum nodes | Cluster network only; TLS planned |
| 6644 | OVN Southbound DB (RAFT) | OVSDB/TCP | Cluster | SB database RAFT replication between the 3 quorum nodes | Cluster network only; TLS planned |
| 6081 | OVN (Geneve) | UDP | Encap | Tenant traffic overlay between chassis. A kernel UDP-tunnel socket, so packets are delivered locally and traverse the host's netfilter input hook before OVS sees them — a host firewall must accept them explicitly. | None — see 500/4500 |
| 500, 4500 | strongSwan `charon` | IKEv2 / UDP | Encap | IKE and NAT-T for the IPsec protecting Geneve, managed entirely by `ovs-monitor-ipsec`. Binds the wildcard by design (the upstream strongSwan default, accepted rather than overridden), so reach must be restricted by firewall. | Certificate-based, against the cluster CA |
| — | ESP | IP proto 50 | Encap | The IPsec payload itself, once IKE has negotiated an SA | Cluster CA |
| 8222 | spinifex-nats (monitoring) | HTTP | Localhost | `varz`/`subsz` metrics consumed by the daemon | Loopback only |
| 323 | chronyd | NTP | Localhost | Time sync client control socket | Loopback only |
| 169.254.169.254:80 | spinifex-vpcd (IMDS) | HTTP | Guest | Instance metadata service. One socket per instance, bound to that instance's `ime-*` interface. Terminates on the host, so it traverses the netfilter input hook. | Instance identity by interface; IMDSv2 tokens |
| 169.254.169.253:53 | spinifex-vpcd (VPC DNS) | DNS (UDP + TCP) | Guest | Per-VPC DNS resolver, forwarding to northstar `:5300` on peer nodes. Same per-instance binding as above. | Instance identity by interface |
| socket / dynamic TCP | nbdkit (Viperblock) | NBD | Host-local / cluster | Block device transport for guest EBS volumes | Unix socket by default; TCP only in remote/DPU mode |

**Planes and scope.** A node resolves three planes — `wan`, `lan` and `vpc` — from its
interfaces, and collapses `vpc` ← `lan` ← `wan` when a plane has no interface of its own. On
a single-NIC node every scope in this table lands on the public address, so **Cluster** and
**Encap** describe intent, not a guarantee. Verify with `ss -tulnp` against the node's actual
addresses rather than assuming the classification holds.

¹ **Formation port lifecycle.** 4432 opens during `spx admin init` / `spx admin join` while a bootstrap token is outstanding and closes once the cluster is formed (token TTL default 30 min, `--token-ttl`). The server presents an ephemeral self-signed cert that pre-dates trust bootstrap, so the joining node does not verify the certificate chain for this single dial. Authenticity rests on the operator supplying the leader address out-of-band plus possession of the bearer token. Document in the security plan so reviewers do not flag 4432 as a persistent open port.

**Predastore ports.** A Predastore cluster is described in `/etc/spinifex/predastore/predastore.toml` as a set of `[[host]]` blocks — one per machine, each running a single process — with the nodes pinned to it declared under `[[host.node]]`. There are three roles: a `gate` serving the S3 API, a `blob` node holding erasure-coded object shards, and a `meta` node in the Raft quorum over global state. Ports must be unique within a host but are not unique across the cluster, so **every machine uses the same three ports** — 8443, 6660 and 7660. It is three fixed ports per machine, not a range, and adding machines does not widen it.

Nodes on the same host talk over an in-process pipe and bind no socket at all, so a single-node install opens only 8443; 6660 and 7660 appear only once a second host exists. The gate is dialled by S3 clients but never by peer nodes, so it binds no QUIC socket of its own — it takes an ephemeral UDP port to dial out with.

**Development-only listeners.** When `dev_networking=true`, QEMU opens arbitrary host TCP ports for SSH port-forwarding into guest VMs. Production installs (the `/etc/spinifex` layout) do not enable this; it must not appear on compliance nodes.

## 2. Outbound Connections

Spinifex nodes initiate a small, fixed set of outbound connections.

**To external destinations:**

| Destination | Purpose | Protocol | Verification |
|-------------|---------|----------|--------------|
| `https://cloud.debian.org/images/cloud/trixie/latest/` | Debian 13 cloud image download | HTTPS | TLS + checksum verification |
| `https://cloud-images.ubuntu.com/releases/resolute/release/` | Ubuntu 26.04 LTS cloud image download | HTTPS | TLS + checksum verification |
| `https://dl.rockylinux.org/pub/rocky/10/images/` | Rocky Linux 10 cloud image download | HTTPS | TLS + checksum verification |
| `https://dl-cdn.alpinelinux.org/alpine/` | Alpine Linux cloud image download | HTTPS | TLS + checksum verification |
| `https://d2yp8ipz5jfqcw.cloudfront.net` | Alpine image for managed HAProxy load-balancer | HTTPS | TLS + checksum verification |
| `https://install.mulgadc.com/install` | One-shot install telemetry POST on `spx admin init` / `join`. | HTTPS | TLS |

**To peer nodes (cluster-internal):** NATS federation (4248), Predastore S3 (8443), OVN NB/SB (6641/6642), northstar DNS forwarding (5300), and the Geneve/IPsec overlay (6081, 500, 4500, ESP) — see [§3](#3-cross-node-internal-connections) for encryption and verification of each. The daemon also polls local NATS monitoring at `127.0.0.1:8222/varz` (loopback HTTP). It opens no connection to Predastore for status: the storage topology it reports comes from reading `predastore.toml`, and no Predastore node serves a status endpoint.

**Update checks and metadata.** Spinifex does not check for updates and does not consume a cloud metadata service (`169.254.169.254` is served *by* the cluster to guest VMs). Node software updates come from the operator's OS package channel. The install-telemetry endpoint above is the only vendor-operated destination contacted by a node; closed-egress deployments should disable it and record the opt-out in the security plan.

**Air-gapped deployments.** The image URLs above are the only destinations needed for the standard image catalogue. Mirror them locally and use `spx admin images import --file` with pre-staged files. Telemetry must also be disabled. See [Air-Gapped Install](/docs/install-airgapped).

## 3. Cross-Node (Internal) Connections

Control-plane and data-plane traffic between Spinifex nodes, for completeness and firewall planning:

| Connection | Port(s) | Encryption / Auth | Notes |
|-----------|---------|-------------------|-------|
| NATS cluster routes | 4248 | Mutual TLS + cluster token | Full mesh between NATS servers |
| Predastore S3 (gate) | TCP 8443 | TLS + AWS SigV4 | Cross-node object reads/writes |
| Predastore blob | UDP 6660 | QUIC with TLS 1.3; server certificate verified against the cluster CA | Erasure-coded object shards. Same port on every machine. |
| Predastore meta | UDP 7660 | QUIC with TLS 1.3; server certificate verified against the cluster CA | Raft consensus over buckets and the object index. Same port on every machine. |
| OVN NB/SB (client) | 6641 / 6642 | Cluster network only (TLS planned) | Network control plane; vpcd and ovn-controller dial the quorum |
| OVN NB/SB (RAFT) | 6643 / 6644 | Cluster network only (TLS planned) | NB/SB database replication across the 3 quorum nodes |
| OVN tunnels (Geneve) | UDP 6081 | Encapsulated by IPsec when `network.ipsec_enabled` is true, which is the default on multi-node clusters | Tenant traffic overlay between chassis, on the `vpc` plane |
| OVN IPsec (IKE / NAT-T / ESP) | UDP 500, UDP 4500, IP proto 50 | Certificate-based against the cluster CA, negotiated by `ovs-monitor-ipsec` | Protects the Geneve tunnels above. OVN-native only — no layer manages strongSwan directly. |
| Instance DNS forwarding | UDP/TCP 5300 | None | Each node's per-instance DNS shim forwards guest queries to peer nodes' northstar `:5300` |

Nodes **must** sit on a network segment that is not routed to tenant/guest VMs or to the internet. The Predastore blob and meta transports and the OVN DBs are cluster-internal and must not be reachable from anywhere else.

## 4. Limiting Controls

Default external surface is five listeners — **9999** (AWS API), **3000** (UI), **22** (SSH), **8443** (S3) and **53** (DNS) — plus **4432** transiently during bootstrap. Every other listener is cluster- or encap-scoped and the operator must enforce this with a host firewall or an upstream network ACL.

The nodes ship no firewall policy today. Until one is installed, this is the operator's responsibility and the reference below is the starting point.

> **Read the notes under the ruleset before applying it.** A default-deny input policy that
> omits any of the loopback, conntrack, `ime-*` or Geneve rules will break instance
> networking, guest metadata or your own SSH session. Apply it to one node and verify before
> applying it cluster-wide.

```
table inet spinifex_filter {
  chain input {
    type filter hook input priority filter; policy drop;

    ct state established,related accept
    ct state invalid drop
    iif lo accept

    # Guest metadata and per-VPC DNS terminate on the host, on per-instance
    # interfaces. Omitting this breaks cloud-init, instance role credentials
    # and all guest DNS.
    iifname "ime-*" accept

    # Path MTU discovery is not optional under a Geneve overlay: dropping
    # destination-unreachable produces silent blackholes on large flows.
    icmp type { echo-request, destination-unreachable, time-exceeded, parameter-problem } accept
    icmpv6 type { echo-request, destination-unreachable, packet-too-big, time-exceeded,
                  parameter-problem, nd-neighbor-solicit, nd-neighbor-advert,
                  nd-router-advert } accept

    # External, from anywhere
    tcp dport { 22, 3000, 8443, 9999 } accept
    tcp dport 53 accept
    udp dport 53 accept

    # Cluster, from peer nodes only. Replace with your nodes' lan-plane
    # addresses — a CIDR does not generalise to nodes on different subnets.
    ip saddr { 10.0.1.1, 10.0.1.2, 10.0.1.3 } tcp dport { 4222, 4248, 4432, 5300, 6641, 6642, 6643, 6644 } accept
    ip saddr { 10.0.1.1, 10.0.1.2, 10.0.1.3 } udp dport { 5300, 6660, 7660 } accept

    # Encap, from peer chassis only. Replace with your nodes' vpc-plane
    # addresses. Geneve arrives as host-local UDP and must be accepted here.
    ip saddr { 10.0.2.1, 10.0.2.2, 10.0.2.3 } udp dport { 6081, 500, 4500 } accept
    ip saddr { 10.0.2.1, 10.0.2.2, 10.0.2.3 } meta l4proto esp accept
  }
}
```

Notes that make the difference between this working and locking you out:

- **Use a dedicated table and never flush the others.** `vpcd` writes MASQUERADE and per-EIP FORWARD rules into the `ip filter` and `ip nat` tables, and only reinstalls them when the service starts. `iptables -F`, `nft flush ruleset`, `ufw enable` and `firewalld` all destroy them silently, and instance networking stays broken until the next restart.
- **Do not add a `forward` hook.** nftables evaluates every table registered on a hook and any `drop` is final, so a default-deny forward chain here would override `vpcd`'s accepts in the other table and break every routed-NAT instance and every EIP. Filtering forwarded guest traffic is the security group's job, not the host firewall's.
- **`output` is deliberately untouched.** The metadata service's reply path egresses through per-instance policy routing; filtering `output` breaks it in ways that are hard to attribute.
- **On a single-NIC node the cluster and encap sets are the node's public addresses**, because the planes collapse. The rules are still correct, but they are no longer a boundary — an upstream ACL is the only real control there.

Port 4432 must be closed outside the bootstrap window; `spx admin join` opens it transiently. Outbound egress can be limited to the image-catalogue hostnames in [§2](#2-outbound-connections) plus the operator's OS package repositories; on air-gapped nodes, block all outbound HTTPS and use `spx admin images import --file`.

## 5. Configuration Surface

Every listener and outbound destination is controlled by one of these files. Changes require a service restart.

| File | Keys | Controls |
|------|------|----------|
| `/etc/spinifex/spinifex.toml` | `nodes.<node>.{awsgw,nats,predastore,daemon}.host`, `nodes.<node>.vpcd.ovn_{nb,sb}_addr`, `nodes.<node>.daemon.dev_networking`, `network.ipsec_enabled` | Per-service bind addresses/ports; dev-mode QEMU port forwarding. `network.ipsec_enabled` (default `true`) decides whether OVN-native IPsec protects the Geneve tunnels, and therefore whether `charon` listens on 500/4500 — single-node clusters never enable it. |
| `/etc/spinifex/nats.conf` | `listen`, `cluster.listen`, `cluster.routes`, `http`, `tls`, `cluster.authorization` | NATS client/cluster/monitoring listeners, peer routes, TLS, cluster token. |
| `/etc/spinifex/predastore/predastore.toml` | `[[host]].bind_addr`, `[[host]].addr`, `[[host]].tls_cert`, `[[host]].tls_key`, `[[host.node]].role`, `[[host.node]].port` | Predastore host and node layout: `bind_addr` is the address the host's sockets bind, `addr` is the address peer hosts dial it on, and both carry no port — the nodes pinned to the host supply their own. The service's `--host`/`--port` override the bind address and the gate's S3 port. |
| `/etc/spinifex/northstar/northstar.toml` | `listen`, `forwarders` | DNS listen addresses (`:53` on the advertise address, `:5300` wildcard) and upstream resolvers. |
| OVN packages (`ovn-central`, `ovn-host`) | `ovn-nb-db`, `ovn-sb-db` (via `ovs-vsctl set open_vswitch …`); `setup-ovn.sh --lan-addr` for the NB/SB client bind; `--encap-ip` for the Geneve endpoint | OVN DB bind addresses and the encap plane. |
| Spinifex UI service | Built-in defaults: `host = "0.0.0.0"`, `port = 3000`. No `spinifex.toml` block today. | UI listener. |
| `spx admin init` / `spx admin join` | `--port`, `--token-ttl`, `--no-telemetry` (or `SPX_NO_TELEMETRY=1`) | Formation port, token TTL, telemetry opt-out. |
| Image catalogue (built-in) | Fixed URLs listed in [§2](#2-outbound-connections); not operator-configurable | Outbound HTTPS destinations for image downloads. |

## 6. Operator Checklist

- Inventory recorded in the system security plan — inbound ([§1](#1-inbound-listeners)), outbound ([§2](#2-outbound-connections)), cross-node ([§3](#3-cross-node-internal-connections)) — matches what is observed on the node (`ss -tlnp`, `ss -unlp`).
- Host firewall enforces the scope split in [§4](#4-limiting-controls): external surface limited to 9999, 3000, 22, 8443 and 53 (and 4432 only during bootstrap).
- Node planes verified: `ss -tulnp` shows cluster-scope listeners on the `lan` address and encap-scope listeners reachable only from peer chassis. On a single-NIC node, record that the planes are collapsed and that an upstream ACL is the only boundary.
- Cluster subnet is isolated from tenant guest VM networks and from the public internet.
- Formation port 4432 is closed on nodes not actively running a bootstrap token.
- Outbound HTTPS restricted to the [§2](#2-outbound-connections) image-catalogue hosts, or replaced with air-gapped import.
- Install telemetry (`install.mulgadc.com`) is either permitted and recorded in the security plan, or disabled via `SPX_NO_TELEMETRY=1` / `--no-telemetry`.
- OVN NB/SB client and RAFT ports (6641–6644) exposure limited to the cluster subnet pending the L2 TLS work.
- SSH (22) configured to operator-managed keys only; password auth disabled in `sshd_config`.
- Periodic review (at least annually, and after any topology change) confirms this inventory still matches the deployed configuration.
