---
title: "Host Firewall"
seoTitle: "Host Firewall and Cluster Membership — Spinifex Docs"
description: "Arm the Spinifex host firewall, narrow SSH to your management networks, and learn how the peer-scoped cluster plane tracks membership changes on its own."
category: "Admin"
tags:
  - security
  - firewall
  - nftables
  - hardening
resources:
  - title: "Network Connections"
    url: "/docs/network-connections"
  - title: "Multi-Node Install"
    url: "/docs/install-multi-node"
  - title: "Setting Up Your Cluster"
    url: "/docs/setting-up-your-cluster"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
---

# Host Firewall

> Arm the host firewall, narrow SSH, and keep the cluster plane off the public internet.

## Table of Contents

- [Overview](#overview)
- [What the policy allows](#what-the-policy-allows)
- [Cluster membership](#cluster-membership)
- [Is it on?](#is-it-on)
- [Narrowing SSH access](#narrowing-ssh-access)
- [Turning it off and on around cluster changes](#turning-it-off-and-on-around-cluster-changes)
- [The files](#the-files)
- [Checking it](#checking-it)
- [Troubleshooting](#troubleshooting)

---

## Overview

Spinifex ships an optional host firewall — an nftables input policy that defaults to drop and divides the node's ports into two groups:

| Group | Ports | Who can reach them |
|---|---|---|
| **Public** | SSH, 443, 3000 (console), 8443 (S3), 9999 (AWS gateway), 53 (DNS) | anyone |
| **Internal** | OVN, NATS, formation, Geneve and the rest of the cluster plane | **cluster members only** |

The internal group is the point. Before this existed, OVN and NATS were reachable from the public internet on a WAN-facing node.

**"Cluster members" is not a list you maintain.** Each node works it out from the cluster it belongs to and rewrites its own rules whenever membership changes — you never edit the peer list by hand.

The policy governs inbound traffic only. Outbound is untouched, because the metadata reply path egresses under per-tap policy routing and a filter there would break it.

## Instructions

## What the Policy Allows

The public plane is open to any source, IPv4 only — an accept with no source scope would otherwise also serve on any global IPv6 address the node picks up from an upstream router advertisement:

| Ports | Serving |
|---|---|
| TCP 443 | Held for the TLS edge; no listener yet, so the rollout needs no firewall change |
| TCP 3000 | Web console |
| TCP 8443 | Predastore S3 gate |
| TCP 9999 | AWS gateway — also how every guest agent (load balancer, EKS, ECS, RDS) reaches its control plane, since they speak SigV4 rather than NATS |
| TCP and UDP 53 | Northstar authoritative DNS. Open on every node deliberately: a node serving no public zone still answers here, which is the accepted cost of not templating the policy per node |

SSH is public too, but scoped by a variable you own — see [Narrowing SSH access](#narrowing-ssh-access).

The cluster plane is peer-scoped. On a single-NIC node the planes collapse onto one address, which is why these rules match peer addresses rather than an interface or a CIDR:

| Ports | Serving |
|---|---|
| TCP 4222, 4248 | NATS client and cluster routes |
| TCP 4432 | Formation, then the daemon cluster manager |
| TCP and UDP 5300 | Internal DNS |
| TCP 6641, 6642 | OVN northbound and southbound |
| TCP 6643, 6644 | OVN database clustering |
| TCP 8660 | Predastore's admin listener |
| UDP 6660, 7660 | Object shards and metadata consensus |
| UDP 6081, 500, 4500, and ESP | Geneve, IKE, NAT-T and IPsec |

Two of those are peer-scoped for reasons worth knowing. Outside a formation window, 4432 is the daemon cluster manager, whose routes report node topology, service inventory and running instances to anyone who asks. And 8660 is Predastore's unauthenticated `/healthz` and `/readyz`, which name the peers a gate cannot reach.

**The formation port needs nothing from you.** `spx admin init` opens 4432 to any source for the length of the formation window and closes it again afterwards, because a node dialling in to join is not a peer yet. It is one slot rather than a list, so it cannot be grown into a general-purpose hole. The handshake behind it is TLS 1.3 with a bearer token.

Beyond those groups the policy accepts what the platform cannot work without: established and related connections, loopback, the ICMP types PMTUD needs under a Geneve overlay, broadcast DHCP replies for the uplink and external-pool leases, and guest traffic to the metadata and VPC DNS endpoints on `169.254.169.254` and `169.254.169.253`. The guest rules are scoped to those two addresses and their ports rather than accepting everything arriving on a guest port, which would expose every wildcard-bound host service to any tenant.

## Cluster Membership

The daemon resolves who the node's peers are from the cluster it belongs to, writes them into `peers.nft`, and reloads the policy. A node that has not resolved membership yet is left reachable rather than cut off from a cluster it cannot name: the apply helper fails without touching the ruleset when the peer file is missing.

One consequence matters in practice. **A node only recognises the members of the cluster it currently belongs to.** During formation the nodes do not yet know each other, so internal traffic between them is dropped — which is why forming or expanding a cluster starts by turning the policy off.

## Is It On?

| How the node was installed | Firewall |
|---|---|
| From the ISO | **on** |
| Binary installer (`curl \| bash`) or `setup.sh` | **off** |
| `setup.sh --firewall=on` | **on** |

The binary installer defaults to off deliberately: it runs on servers that already have an operating system and services on them, and switching on a default-deny policy uninvited could cut off something Spinifex knows nothing about. The ISO is an appliance on hardware we define, so there the policy is the product.

**For production, turn it on** — either at install time:

```bash
curl -fsSL https://install.mulgadc.com | bash -s -- --firewall=on
```

or afterwards, by setting it in `/etc/spinifex/spinifex.toml` and restarting the daemon:

```toml
[network]
firewall_enabled = true
```

Before you do, check what else the machine is serving. Anything listening on a port outside the public group above stops accepting new connections.

## Narrowing SSH Access

SSH is accepted from **every source** by default. That is deliberate — arming the policy should not lock an operator out of the node they are arming it from — but it is the first thing to change on a production cluster.

The sources live in `/etc/spinifex/firewall/custom.nft`, which the installer creates once and never rewrites, so an edit here survives upgrades, re-installs and node resets:

```
define trusted_ssh_peers = { 10.0.0.0/8, 192.168.1.0/24 }
```

Check the change before applying it, and check the policy file rather than this one — on its own `custom.nft` parses as a bare define and proves nothing, because the rule that uses the variable lives elsewhere:

```bash
sudo nft -c -f /etc/spinifex/firewall/spinifex.nft
sudo nft -f /etc/spinifex/firewall/spinifex.nft
```

Never leave the variable empty. An undefined or empty set fails the whole ruleset, not just its own rule.

The ports come from sshd's own configuration, re-read on every `setup.sh` run, so moving sshd to a non-standard port is picked up by an upgrade rather than needing an edit here. IPv4 only: sshd reached over IPv6 is not matched by this rule.

## Turning It Off and On Around Cluster Changes

Turn the firewall off while you form or expand a cluster, and on again once it is up:

```bash
# Off — before forming or expanding a cluster. Run on every node.
sudo /usr/local/lib/spinifex/spinifex-firewall-apply disable

# On — once the cluster is formed and verified. Run on every node.
sudo systemctl restart spinifex-daemon
```

Restarting the daemon is what re-arms it: the node rebuilds its peer list from the cluster it is now part of, reloads the rules, and re-enables the boot-time unit so the policy survives a reboot. It also happens on its own within five minutes if you would rather wait.

`disable` removes Spinifex's own table and the peer file and leaves every other nftables table alone. It is idempotent, and it is the recovery path — it runs even on a node with no policy installed, so it cannot leave you with a node you can neither arm nor disarm.

> [!WARNING]
> Do not flush the whole nftables ruleset to clear the policy. The VPC daemon writes SNAT and per-elastic-IP forward rules into the `ip nat` and `ip filter` tables and reinstalls them only when it starts, so `nft flush ruleset` breaks elastic IPs silently until the next `systemctl restart spinifex-vpcd`. Use `disable`, which only ever touches `inet spinifex_filter`.

## The Files

Everything lives under `/etc/spinifex/firewall/`. Which files are safe to edit follows from who writes them:

| File | Written by | Survives an upgrade |
|---|---|---|
| `spinifex.nft` | `setup.sh` — the policy itself | No. Edits are overwritten |
| `local.nft` | `setup.sh` — the sshd ports it detected | No. Refreshed on every run |
| `peers.nft` | The daemon — cluster and encap peer addresses | No. Rewritten on every membership change |
| `open-ports.nft` | The apply helper — the formation window only | No. Reset closed on install and upgrade |
| `custom.nft` | **You** — `trusted_ssh_peers` and anything else you add | **Yes.** Created once, never rewritten |
| `mode` | `setup.sh` — whether the install path armed the policy | No |

The daemon cannot write `/etc` itself, and a blanket `nft` grant would be root-equivalent, so changes go through `/usr/local/lib/spinifex/spinifex-firewall-apply` — a root helper that accepts a fixed set of verbs and re-validates every address handed to it.

## Checking It

```bash
sudo nft list table inet spinifex_filter
```

The peer list is an nft variable, expanded when the rules load, so it does not appear under a name of its own. Look instead at the `ip saddr { ... }` addresses on the cluster-plane rules — the ones accepting 4222, 6641, 6642 and the rest.

Every node's addresses should be there. On a multi-NIC node that means its WAN, LAN and VPC addresses, so expect several entries per node. A missing node means its cluster traffic is being dropped.

Dropped packets are logged, rate-limited to five a minute, so this tells you whether a connection problem is the firewall or something else:

```bash
sudo journalctl -k | grep 'spinifex-fw drop'
```

An empty log with a connection still failing means the policy is not the cause. A node with no table loaded at all is not armed:

```bash
systemctl is-enabled spinifex-firewall.service
```

## Troubleshooting

### A Join Hangs Instead of Failing

A hang rather than a quick refusal means packets are being dropped. Check the drop log on the node running init, and confirm the formation port opened — `spx admin init` prints `⚠️ Could not open port 4432 in the host firewall` when it could not:

```bash
sudo journalctl -k | grep 'spinifex-fw drop'
```

On ISO-installed nodes the usual cause is that each node's policy still trusts only itself, because that is the whole cluster as far as it knows. Turn the firewall off on **every** node, retry, and re-arm once the cluster is verified.

### A Node Is Missing from the Peer List

The daemon rewrites `peers.nft` from the cluster it belongs to, so a missing node means that node is not in the membership this one resolved. Confirm what the cluster thinks first, then restart the daemon to force a rebuild:

```bash
spx get nodes
sudo systemctl restart spinifex-daemon
```

### One of My Own Services Stopped Answering

Arming the policy closes every port outside the public group. Add what you need to `custom.nft`, which is the only file an upgrade leaves alone, and check the policy before applying it:

```bash
sudo nft -c -f /etc/spinifex/firewall/spinifex.nft
```

### Elastic IPs Stopped Working

Something flushed the whole nftables ruleset, taking the VPC daemon's SNAT and forward rules with it. They are reinstalled on start:

```bash
sudo systemctl restart spinifex-vpcd
```
