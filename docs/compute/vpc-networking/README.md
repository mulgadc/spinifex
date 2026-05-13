---
title: "VPC Networking"
description: "How Spinifex implements AWS-compatible VPC networking with public and private subnets, security groups, and Elastic IPs using OVN."
category: "Compute"
tags:
  - vpc
  - networking
  - ovn
  - public-subnet
  - security-groups
---

# VPC Networking

> How Spinifex implements AWS-compatible VPC networking with public and private subnets, security groups, and Elastic IPs using OVN.

## Overview

Spinifex provides AWS-compatible VPC networking on bare-metal. Every EC2 instance
runs inside an isolated virtual network backed by OVN (Open Virtual Network).
Instances can operate in two modes: **private** (overlay-only, no WAN access) or
**public** (routable from the WAN with a unique public IP).

## Instructions

## How It Works

Spinifex maps AWS VPC concepts directly to OVN constructs:

| AWS Concept      | OVN Construct            | What It Does                                       |
| ---------------- | ------------------------ | -------------------------------------------------- |
| VPC              | Logical Router           | Isolates tenant networks, routes between subnets   |
| Subnet           | Logical Switch + DHCP    | L2 broadcast domain with automatic IP assignment   |
| ENI              | Logical Switch Port      | Per-instance network interface with MAC/IP binding |
| Internet Gateway | External Switch + NAT    | Connects VPC router to physical WAN                |
| Security Group   | Port Group + ACLs        | Stateful firewall rules enforced in OVS datapath   |
| Elastic IP       | `dnat_and_snat` NAT rule | Static 1:1 NAT between public and private IP       |

## Network Path

<p align="center">
  <img src="../../../.github/assets/diagrams/vpc-network-path.svg" alt="VPC logical topology — WAN, br-wan, VPC logical router, subnets, ENIs" width="900">
</p>

Cross-host traffic uses **Geneve tunnels** (UDP 6081) over the management/overlay
NIC. Each host runs `ovn-controller` which programs OpenFlow rules on `br-int`
(the integration bridge where all VM TAP devices connect).

## Private vs Public Subnets

A subnet's behavior depends on two things: whether the VPC has an Internet
Gateway, and whether the subnet has `MapPublicIpOnLaunch` enabled.

## Private Subnet (Default)

Instances get a private IP only. They can communicate with other instances in the
same VPC (even across subnets and hosts via the overlay). They cannot reach the
internet or be reached from the WAN.

<p align="center">
  <img src="../../../.github/assets/diagrams/vpc-private-subnet-flow.svg" alt="Private subnet — instance hits router, no default route, packet dropped" width="900">
</p>

If the VPC has an IGW attached, private subnet instances CAN reach the internet
via the VPC router's SNAT rule (outbound only — they share the gateway IP). They
still cannot be reached from the WAN because they have no public IP.

## Public Subnet

Instances get both a private IP and a public IP. The public IP is a 1:1 NAT
managed by OVN — the instance OS only sees its private IP.

<p align="center">
  <img src="../../../.github/assets/diagrams/vpc-public-subnet-flow.svg" alt="Public subnet — outbound SNAT and inbound DNAT between private and public IPs" width="900">
</p>

**Requirements for a public subnet:**

1. VPC has an Internet Gateway attached
2. Subnet has `MapPublicIpOnLaunch = true`
3. External IP pool configured in `spinifex.toml`

## Comparison

|                          | Private Subnet                    | Public Subnet                  |
| ------------------------ | --------------------------------- | ------------------------------ |
| Private IP               | Yes                               | Yes                            |
| Public IP                | No                                | Auto-assigned from pool        |
| Outbound internet        | Only if VPC has IGW (shared SNAT) | Yes (own public IP via SNAT)   |
| Inbound from WAN         | No                                | Yes (via 1:1 NAT to public IP) |
| Instance sees public IP? | N/A                               | No — only sees private IP      |
| Elastic IP support       | Only if explicitly associated     | Yes                            |

## External Connectivity Modes

The `[network]` section in `spinifex.toml` controls how VMs reach the outside
world. There are three modes, and pool mode has two IP sources (static or DHCP).

## `pool` — Full Public Networking (Recommended)

Each VM in a public subnet gets its own public IP with bidirectional 1:1 NAT.
Supports the full AWS feature set: public subnets, auto-assign public IPs,
Elastic IPs, and security groups.

Pool mode supports two ways to obtain public IPs:

### Static Range (default)

The admin defines a range of routable IPs that Spinifex manages exclusively.

**Use when:** You have a block of IPs you control — datacenter ISP allocation,
homelab range carved out of your router's DHCP scope, enterprise DMZ range.

**Requirement:** The IP range must NOT be served by any other DHCP server. In a
homelab, shrink your router's DHCP scope to exclude the Spinifex range.

```toml
[network]
external_mode = "pool"

[[network.external_pools]]
name        = "wan"
range_start = "192.168.1.150"
range_end   = "192.168.1.250"
gateway     = "192.168.1.1"       # Router / next-hop IP
prefix_len  = 24
dns_servers = ["192.168.1.1", "8.8.8.8"]
```

### DHCP Source

Instead of a static range, public IPs come from the upstream router's DHCP
server. When a VM launches, Spinifex requests a DHCP lease from the router
on behalf of the VM. When the VM terminates, the lease is released.

The VM itself never talks to the router's DHCP — it only sees its private
VPC IP (from OVN's internal DHCP). The host-side DHCP conversation is
invisible to the guest.

**Use when:** You don't control a static IP block but the router's DHCP
server has enough leases. Homelabs where you don't want to carve out a range.
Environments where IPs are managed centrally by the network team's DHCP.

**Requirement:** `dhclient` or `dhcpcd-base` installed on the host.

```toml
[network]
external_mode = "pool"

[[network.external_pools]]
name        = "wan"
source      = "dhcp"              # "static" (default) or "dhcp"
gateway     = "192.168.1.1"       # Router / next-hop IP
prefix_len  = 24
dns_servers = ["192.168.1.1", "8.8.8.8"]
# No range_start/range_end — IPs come from router DHCP
```

### How Pool Mode Works (Both Sources)

Regardless of whether IPs come from a static range or DHCP, the OVN behavior
is identical:

<p align="center">
  <img src="../../../.github/assets/diagrams/vpc-dhcp-conversations.svg" alt="Two independent DHCP conversations — host-to-router and VM-to-OVN" width="900">
</p>

### Choosing Static vs DHCP

|                       | Static Range                                 | DHCP Source                                      |
| --------------------- | -------------------------------------------- | ------------------------------------------------ |
| **Public IPs from**   | Admin-defined `range_start`..`range_end`     | Router's DHCP server                             |
| **IP predictability** | You know the exact range                     | Router assigns whatever is available             |
| **Setup effort**      | Must reserve range, shrink router DHCP scope | Just set `source = "dhcp"`                       |
| **Dependency**        | None                                         | Requires `dhclient` on host, working router DHCP |
| **Best for**          | Datacenters, ISP blocks, production          | Homelabs, dev environments, shared networks      |
| **Capacity**          | Exact: `range_end - range_start` IPs         | Limited by router's DHCP pool size               |

Both support the same AWS features: public subnets, Elastic IPs, security groups,
DescribeInstances showing public IPs.

## `nat` — Outbound Only (Simple)

All VMs share a single external IP for outbound SNAT. No public IPs, no Elastic
IPs, no inbound from WAN. All subnets behave as private subnets with internet
access.

The `gateway_ip` is the IP that OVN uses for SNAT. You can set it statically or
use `setup-ovn.sh --dhcp` to obtain one from the router. This is the router's
DHCP — not Spinifex's internal OVN DHCP for VMs.

**Use when:** VMs only need outbound access (apt update, pulling images). Edge
deployments behind ISP NAT. Single WAN IP available. Future: use with AWS-style
NAT Gateway for private subnet internet access.

```toml
[network]
external_mode = "nat"

[[network.external_pools]]
name       = "wan"
gateway    = "192.168.1.1"
gateway_ip = "192.168.1.100"     # Single IP for all VM outbound SNAT
prefix_len = 24
```

## Disabled (Empty/Omitted)

VPC networking is overlay-only. No external connectivity. Instances can only
communicate within their VPC.

## Mode Comparison

| Capability                        | `pool` (static) | `pool` (dhcp) | `nat`    | Disabled |
| --------------------------------- | --------------- | ------------- | -------- | -------- |
| Outbound internet                 | Yes             | Yes           | Yes      | No       |
| Inbound from WAN                  | Yes (1:1 NAT)   | Yes (1:1 NAT) | No       | No       |
| Public subnets                    | Yes             | Yes           | No       | No       |
| Auto-assign public IPs            | Yes             | Yes           | No       | No       |
| Elastic IPs                       | Yes             | Yes           | No       | No       |
| DescribeInstances shows public IP | Yes             | Yes           | No       | No       |
| Admin must reserve IP range       | Yes             | No            | No       | No       |
| Needs router DHCP                 | No              | Yes           | Optional | No       |

If you start with `nat` and later need public subnets, switch to `pool` and
define a range (or use `source = "dhcp"`) — no data migration needed.

## Bridge Setup — Physical Network Wiring

The WAN NIC **must** be enslaved to a Linux bridge. This is a hard requirement —
`setup-ovn.sh` will not attach a physical NIC directly to OVS, and macvlan is
no longer supported. The Linux bridge owns the host IP, default route, and any
DHCP lease, so SSH and management traffic stay up while OVS/OVN are configured
underneath.

The full datapath chain looks like this:

```
physical NIC (e.g. `wan`)
   └─ enslaved to ─▶ br-wan         (Linux bridge — host IP, default route, DHCP)
                       │
                       └─ veth pair ─▶ br-ext   (OVS bridge — OVN external uplink)
                                          │
                                          └─ localnet ─▶ br-int  (OVS integration bridge)
                                                            │
                                                            └─▶ TAP devices  (VM NICs)
```

`setup-ovn.sh` auto-detects the Linux bridge that owns the default route
(typically `br-wan`, provisioned by cloud-init / netplan / systemd-networkd).
You can override the detection with `--wan-bridge=<name>`. Once detected, the
script creates the OVS bridge `br-ext` and links it to the WAN bridge with a
veth pair. The Linux bridge keeps its IP and routes — no interruption.
Bridge-mapping is set to `external:br-ext`.

If the default route is on a bare physical NIC (no bridge), `setup-ovn.sh`
stops and prints guidance on how to convert the NIC to a bridge before
re-running.

### Example: Required `br-wan` State

The host must have something resembling this before `setup-ovn.sh` is run:

```
7: br-wan: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP group default qlen 1000
    link/ether 26:df:3c:de:d0:c2 brd ff:ff:ff:ff:ff:ff
    inet 192.168.1.31/23 brd 192.168.1.255 scope global br-wan
       valid_lft forever preferred_lft forever
    inet6 fe80::24df:3cff:fede:d0c2/64 scope link proto kernel_ll
       valid_lft forever preferred_lft forever
```

The physical NIC (e.g. `wan`, `eth0`, `eno1`) is enslaved to `br-wan` and has
no IP of its own — all L3 state lives on the bridge.

Example netplan that produces this:

```yaml
network:
  version: 2
  ethernets:
    wan:
      dhcp4: false
  bridges:
    br-wan:
      interfaces: [wan]
      dhcp4: true
```

## Three Bridges, Three Jobs

Every Spinifex node has three bridges in the datapath:

| Bridge   | Type         | Created By                  | Purpose                                       | Ports                        |
| -------- | ------------ | --------------------------- | --------------------------------------------- | ---------------------------- |
| `br-wan` | Linux bridge | Host (cloud-init / netplan) | Host WAN uplink — owns host IP and default route | Physical WAN NIC, veth peer  |
| `br-ext` | OVS bridge   | `setup-ovn.sh`              | OVN external uplink (`localnet`)              | veth peer to `br-wan`        |
| `br-int` | OVS bridge   | `setup-ovn.sh`              | VM overlay traffic (Geneve tunnels)           | VM TAP devices, tunnel ports |

`br-wan` is provisioned by your distro's network configuration (cloud-init,
netplan, systemd-networkd, ifupdown). The name is configurable; `br-wan` is
the convention. `br-int` and `br-ext` are always created by `setup-ovn.sh`.

The link between `br-ext` and the VM datapath is logical, not physical: OVN's
`localnet` port type maps the logical external switch to `br-ext` via
`ovn-bridge-mappings`. Frames egressing a VM travel TAP → `br-int` → OVN
pipeline → `br-ext` → veth → `br-wan` → physical NIC → wire.

<p align="center">
  <img src="../../../.github/assets/diagrams/vpc-datapath-localnet.svg" alt="Data path — VM TAP through br-int, OVN pipeline, br-ext, veth pair, br-wan, physical NIC" width="900">
</p>

## Running setup-ovn.sh

```bash
# Auto-detect the WAN bridge (recommended)
sudo setup-ovn.sh

# Explicitly specify the WAN bridge name
sudo setup-ovn.sh --wan-bridge=br-wan
```

In environments where the WAN IP comes from a router's DHCP server (homelab,
small office), add `--dhcp` to obtain a gateway IP from the router
automatically:

```bash
sudo setup-ovn.sh --dhcp
```

This requests an IP from the **router's DHCP** (e.g., 192.168.1.1 serving
addresses on the LAN). This is not Spinifex's internal OVN DHCP that assigns
private IPs to VMs — it's your network's existing DHCP server.

| Flags                          | Result                                                                |
| ------------------------------ | --------------------------------------------------------------------- |
| (no flags)                     | Auto-detect WAN bridge from default route, create `br-int` + `br-ext` |
| `--wan-bridge=<name>`          | Use the specified Linux bridge as the WAN uplink                      |
| `--dhcp`                       | Obtain the OVN gateway IP from the router's DHCP                      |

If no Linux bridge owns the default route, `setup-ovn.sh` exits with guidance
rather than silently breaking host connectivity.

## Per-Node Configuration

Different nodes in a cluster can have different WAN bridges and NICs:

```toml
[nodes.node1.vpcd]
external_interface = "br-wan"

[nodes.node2.vpcd]
external_interface = "br-public"

[nodes.node3.vpcd]
external_interface = "br-wan"      # br-wan enslaving bond0
```

`external_interface` is the **Linux bridge** that owns the WAN uplink on this
node — not the physical NIC. The physical NIC lives underneath the bridge.
Each node runs `setup-ovn.sh` with its own WAN bridge name (or relies on
auto-detection). OVN only requires `ovn-bridge-mappings` to point at `br-ext`.

## Bridge Verification

```bash
# OVS bridges created by setup-ovn.sh
sudo ovs-vsctl br-exists br-int && echo "br-int OK" || echo "br-int MISSING"
sudo ovs-vsctl br-exists br-ext && echo "br-ext OK" || echo "br-ext MISSING"

# Linux WAN bridge owns the host IP and default route
ip -br addr show br-wan
ip route show default
# Default route's dev should be br-wan (or your WAN bridge name)

# br-ext should have one veth port linking it to br-wan
sudo ovs-vsctl list-ports br-ext
# Expect: a veth name (e.g. "veth-wan-ovs")

# Confirm the matching peer is enslaved to the Linux WAN bridge
sudo bridge link show | grep br-wan

# Bridge mappings must point at br-ext
sudo ovs-vsctl get Open_vSwitch . external-ids:ovn-bridge-mappings
# Output: "external:br-ext"

# Physical NIC is enslaved to br-wan (master should be br-wan)
ip -d link show wan
```

## Network Flow Diagram

<p align="center">
  <img src="../../../.github/assets/diagrams/vpc-host-flow.svg" alt="Bare-metal host — br-int overlay, br-ext OVS uplink, br-wan Linux bridge, physical NIC, OVN NAT pipeline" width="900">
</p>

## Configuration Reference

All network configuration lives in `spinifex.toml`. Settings are split into three
levels: cluster-wide mode, IP pool definitions, and per-node NIC settings.

## Configuration Levels

<p align="center">
  <img src="../../../.github/assets/diagrams/vpc-config-layers.svg" alt="spinifex.toml configuration layers — cluster mode, IP pools, per-node NIC" width="900">
</p>

## Cluster-Wide: external_mode

```toml
[network]
external_mode = "pool"    # "pool", "nat", or "" (disabled)
```

| Value          | Behavior                                                          |
| -------------- | ----------------------------------------------------------------- |
| `"pool"`       | Full public networking — public subnets, auto-assign, Elastic IPs |
| `"nat"`        | Outbound-only SNAT — all VMs share one external IP                |
| `""` / omitted | Overlay-only — no external connectivity                           |

## IP Pools: network.external_pools

Each pool defines where external IPs come from. You can have one pool (homelab)
or many (multi-region datacenter).

```toml
[[network.external_pools]]
name        = "wan"                  # Pool identifier (unique within cluster)
source      = "static"              # "static" (default) or "dhcp"
range_start = "192.168.1.150"        # First allocatable IP (static source only)
range_end   = "192.168.1.250"        # Last allocatable IP (static source only)
gateway     = "192.168.1.1"          # WAN default gateway (next hop for 0.0.0.0/0)
gateway_ip  = ""                     # OVN router SNAT address (defaults to range_start)
prefix_len  = 24                     # Subnet mask length
region      = ""                     # Scope to region (optional)
az          = ""                     # Scope to AZ (optional)
dns_servers = ["8.8.8.8"]           # DNS for VMs (optional)
```

### Field Details

| Field         | Required    | Description                                                                                                                                                            |
| ------------- | ----------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `name`        | Yes         | Unique pool name. Used as NATS KV key and in `AllocateAddress`.                                                                                                        |
| `source`      | No          | IP source: `"static"` (default) uses `range_start`/`range_end`. `"dhcp"` obtains IPs from the router's DHCP server on each VM launch.                                  |
| `range_start` | Static only | First IP in the range. First IP is reserved for OVN gateway SNAT (unless `gateway_ip` overrides).                                                                      |
| `range_end`   | Static only | Last IP in the range.                                                                                                                                                  |
| `gateway`     | Yes         | Physical router/switch — the WAN default gateway. OVN sets `0.0.0.0/0 → gateway`.                                                                                      |
| `gateway_ip`  | NAT mode    | Static IP for OVN router SNAT. In pool mode, defaults to `range_start` (static) or first DHCP lease (dhcp). In NAT mode, this is the single external IP all VMs share. |
| `prefix_len`  | Yes         | Subnet mask for the external network (e.g., 24 = /24).                                                                                                                 |
| `region`      | No          | Scopes pool to a region. Instances in this region prefer this pool.                                                                                                    |
| `az`          | No          | Scopes pool to an AZ. More specific than region.                                                                                                                       |
| `dns_servers` | No          | DNS servers propagated to VMs via OVN DHCP.                                                                                                                            |

### Why range_start/range_end Instead of CIDR?

Customer IP ranges rarely align to CIDR boundaries. A datacenter might have
`203.0.113.10-203.0.113.200` from their ISP. Start/end avoids forcing admins to
calculate CIDR blocks.

### Gateway vs Gateway_IP

These are different things:

- **`gateway`** = Your network's default gateway (e.g., 192.168.1.1). This is
  where OVN sends packets destined for the internet. It's your router.
- **`gateway_ip`** = The IP that OVN uses for outbound SNAT. In pool mode,
  defaults to the first IP in the range. In NAT mode, set this explicitly.
  Must be on the same subnet as the gateway.

## Per-Node: nodes.NAME.vpcd

```toml
[nodes.spx1.vpcd]
ovn_nb_addr        = "tcp:10.1.3.181:6641"   # OVN Northbound DB
ovn_sb_addr        = "tcp:10.1.3.181:6642"   # OVN Southbound DB
external_interface = "br-wan"                 # WAN Linux bridge name
```

| Field                | Description                                                                                                                                                                          |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `external_interface` | Linux bridge that owns the WAN uplink on this node (e.g. `br-wan`, `br-public`). The physical NIC is enslaved to this bridge — not configured here. Different nodes may differ. |

## Pool Selection Logic

When an instance needs a public IP:

1. **AZ-scoped pool first**: Pool with matching `region` + `az`
2. **Region-scoped fallback**: Pool with matching `region`, no `az` (overflow)
3. **Unscoped fallback**: Pool with no `region`/`az` (global, homelab configs)
4. **Exhausted**: All pools full → `InsufficientAddressCapacity` error

`AllocateAddress` accepts optional pool name to target a specific block
(maps to AWS `PublicIpv4Pool`).

## IPAM Storage

Each pool gets a NATS KV entry in bucket `spinifex-external-ipam`, keyed by pool
name. Allocation uses CAS (Compare-And-Set) for lock-free concurrent access:

```json
{
  "pool_name": "wan",
  "range_start": "192.168.1.150",
  "range_end": "192.168.1.250",
  "allocated": {
    "192.168.1.150": { "type": "gateway" },
    "192.168.1.151": {
      "type": "auto_assign",
      "eni_id": "eni-abc",
      "instance_id": "i-123"
    }
  }
}
```

Pools are initialized from `spinifex.toml` on vpcd startup (idempotent).

## Deployment Examples

## Homelab / Dev (Single Pool)

```
Network: 192.168.1.0/24
Router: 192.168.1.1 (DHCP .2–.149)
Spinifex: 192.168.1.150–.250 (100 IPs)
```

```toml
[network]
external_mode = "pool"

[[network.external_pools]]
name        = "wan"
range_start = "192.168.1.150"
range_end   = "192.168.1.250"
gateway     = "192.168.1.1"
prefix_len  = 24

[nodes.homelab.vpcd]
external_interface = "br-wan"
```

**Setup:** Configure `br-wan` to enslave your physical WAN NIC (netplan,
cloud-init, or systemd-networkd). Change your router's DHCP range to end
at .149. Run `sudo setup-ovn.sh` — it auto-detects the WAN bridge from the
default route, or specify it with `--wan-bridge=br-wan`.

## Homelab / Dev (DHCP Pool — No Range Reservation)

```
Network: 192.168.1.0/24
Router: 192.168.1.1 (DHCP serves full range, no carve-out needed)
Spinifex: gets IPs from router DHCP on demand
```

```toml
[network]
external_mode = "pool"

[[network.external_pools]]
name        = "wan"
source      = "dhcp"
gateway     = "192.168.1.1"
prefix_len  = 24
dns_servers = ["192.168.1.1", "8.8.8.8"]

[nodes.homelab.vpcd]
external_interface = "br-wan"
```

**Setup:** No router changes needed. Spinifex requests IPs from the router's
DHCP server when VMs launch and releases them on terminate. Requires `dhclient`
on the host (`apt install isc-dhcp-client`).

## Host-Local Subnet (No Upstream Router)

```
Network: 192.168.10.0/24 — host-local, reachable from the host only
Host WAN: 198.51.100.10/24 on br-wan (existing address — unchanged)
Gateway: 192.168.10.1 — second address added to br-wan
```

Add the VM pool gateway as a second address on `br-wan` alongside the existing WAN
IP. The host acts as the gateway for the pool — no upstream router or DHCP server
needed for this range.

```yaml
# /etc/netplan/…
bridges:
  br-wan:
    addresses:
      - 192.168.10.1/24      # VM pool gateway — host-local
      - 198.51.100.10/24     # existing WAN IP — unchanged
    routes:
      - to: default
        via: 198.51.100.1
```

```toml
[network]
external_mode = "pool"

[[network.external_pools]]
name        = "wan"
source      = "static"         # required — no upstream DHCP for this range
range_start = "192.168.10.2"
range_end   = "192.168.10.100"
gateway     = "192.168.10.1"   # second address on br-wan
prefix_len  = 24
dns_servers = ["8.8.8.8"]
```

**Setup:** Apply with `sudo netplan apply`. VMs are reachable from the host at
`192.168.10.x`. For internet access through the host's WAN interface:

```bash
sysctl -w net.ipv4.ip_forward=1
iptables -t nat -A POSTROUTING -s 192.168.10.0/24 -o br-wan -j MASQUERADE
```

Persist via `/etc/sysctl.d/99-ip-forward.conf` and `netfilter-persistent save`.

## Datacenter / Colo (ISP Block)

```
ISP-assigned: 203.0.113.0/28 (14 usable IPs)
ISP gateway: 203.0.113.1
Servers: 3x with separate mgmt NIC (eth0) and public NIC enslaved to br-wan
```

```toml
[network]
external_mode = "pool"

[[network.external_pools]]
name        = "public"
range_start = "203.0.113.2"
range_end   = "203.0.113.14"
gateway     = "203.0.113.1"
prefix_len  = 28

[nodes.dc1.vpcd]
external_interface = "br-wan"      # br-wan enslaves eth1

[nodes.dc2.vpcd]
external_interface = "br-wan"      # br-wan enslaves eth1

[nodes.dc3.vpcd]
external_interface = "br-public"   # br-public enslaves eno1
```

## Enterprise On-Prem (VLAN)

The Linux WAN bridge enslaves a VLAN sub-interface (e.g. `eth1.200`) instead
of a raw NIC. From OVN's perspective nothing changes — `external_interface`
still points at the bridge.

```toml
[network]
external_mode = "pool"

[[network.external_pools]]
name        = "dmz"
range_start = "172.16.0.100"
range_end   = "172.16.0.200"
gateway     = "172.16.0.1"
prefix_len  = 24

[nodes.srv1.vpcd]
external_interface = "br-dmz"      # br-dmz enslaves eth1.200

[nodes.srv2.vpcd]
external_interface = "br-dmz"      # br-dmz enslaves bond0.200
```

## Edge / Branch (Outbound Only)

```toml
[network]
external_mode = "nat"

[[network.external_pools]]
name       = "wan"
gateway    = "10.0.0.1"
gateway_ip = "10.0.0.50"
prefix_len = 24

[nodes.edge1.vpcd]
external_interface = "br-wan"
```

## Multi-Region (Multiple Pools)

```toml
[network]
external_mode = "pool"

# US East — AZ-scoped
[[network.external_pools]]
name        = "us-east-1a"
range_start = "203.0.113.2"
range_end   = "203.0.113.254"
gateway     = "203.0.113.1"
prefix_len  = 24
region      = "us-east-1"
az          = "us-east-1a"

# US East — overflow (any AZ in region)
[[network.external_pools]]
name        = "us-east-overflow"
range_start = "192.0.2.2"
range_end   = "192.0.3.254"
gateway     = "192.0.2.1"
prefix_len  = 23
region      = "us-east-1"

# EU West
[[network.external_pools]]
name        = "eu-west"
range_start = "213.189.1.2"
range_end   = "213.189.2.254"
gateway     = "213.189.1.1"
prefix_len  = 23
region      = "eu-west-1"
```

Spinifex allocates from the correct pool based on where the instance launches.
An instance in `us-east-1a` gets an IP from `us-east-1a` first; if exhausted,
falls back to `us-east-overflow`.

## Security Groups

Security groups are stateful firewalls enforced at the OVS datapath level on
each hypervisor. Traffic is filtered before it reaches the wire — equivalent to
AWS Nitro card enforcement. The VM never sees dropped packets.

## How Security Groups Work

Each security group maps to an OVN **Port Group**. When an instance launches,
its ENI port is added to the port group(s) for its security groups. ACL rules
on the port group control traffic:

- **Default deny**: All inbound traffic dropped at priority 900
- **Allow rules**: Specific ports/protocols allowed at priority 1000 (overrides deny)
- **Stateful**: All allow rules use `allow-related` — return traffic is automatically permitted

## Default Security Group

Every VPC gets a default security group that:

- Allows all inbound from instances in the same security group
- Allows all outbound
- Denies all other inbound

## AWS Rule → OVN ACL Translation

| AWS Security Group Rule         | OVN ACL Match                                                      |
| ------------------------------- | ------------------------------------------------------------------ |
| Ingress TCP/22 from 0.0.0.0/0   | `outport == @sg && ip4 && tcp.dst == 22`                           |
| Ingress TCP/443 from 10.0.0.0/8 | `outport == @sg && ip4 && tcp.dst == 443 && ip4.src == 10.0.0.0/8` |
| Ingress ALL from sg-other       | `outport == @sg && ip4 && ip4.src == $sg_other_ip4`                |
| Ingress ICMP from anywhere      | `outport == @sg && ip4 && icmp4`                                   |
| Egress ALL to 0.0.0.0/0         | `inport == @sg && ip4`                                             |
| Default deny inbound            | `outport == @sg && ip4` (priority 900, action=drop)                |

## Example: Allow SSH + HTTP

```bash
# Create security group
SG=$(aws ec2 create-security-group --group-name web \
  --description "Web servers" --vpc-id $VPC \
  --query GroupId --output text)

# Allow SSH from anywhere
aws ec2 authorize-security-group-ingress --group-id $SG \
  --protocol tcp --port 22 --cidr 0.0.0.0/0

# Allow HTTP from anywhere
aws ec2 authorize-security-group-ingress --group-id $SG \
  --protocol tcp --port 80 --cidr 0.0.0.0/0

# Launch instance with this SG
aws ec2 run-instances --image-id $AMI --instance-type t3.small \
  --subnet-id $SUBNET --security-group-ids $SG --key-name mykey
```

Rule changes take effect immediately — no instance restart needed.

## Elastic IPs

Elastic IPs are static public IPs that persist across instance stop/start cycles.
Unlike auto-assigned public IPs (which change on stop/start), an Elastic IP stays
with your instance.

```bash
# Allocate
EIP=$(aws ec2 allocate-address --query AllocationId --output text)

# Associate with instance
aws ec2 associate-address --allocation-id $EIP --instance-id $INSTANCE

# Stop/start instance — same Elastic IP

# Disassociate
aws ec2 disassociate-address --association-id $ASSOC_ID

# Release back to pool
aws ec2 release-address --allocation-id $EIP
```

When you associate an Elastic IP with an instance that already has an
auto-assigned public IP, the auto-assigned IP is released and replaced.

## OVN Reference

For operators debugging or verifying the OVN topology.

## IGW Attach Creates

```bash
# External logical switch with localnet port
ovn-nbctl ls-add ext-{vpcId}
ovn-nbctl lsp-add ext-{vpcId} ext-localnet-{vpcId}
ovn-nbctl lsp-set-type ext-localnet-{vpcId} localnet
ovn-nbctl lsp-set-addresses ext-localnet-{vpcId} unknown
ovn-nbctl lsp-set-options ext-localnet-{vpcId} network_name=external

# Gateway router port with real external IP
ovn-nbctl lrp-add vpc-{vpcId} gw-{vpcId} {mac} 192.168.1.150/24

# Connect external switch to router
ovn-nbctl lsp-add ext-{vpcId} ext-rtr-{vpcId}
ovn-nbctl lsp-set-type ext-rtr-{vpcId} router
ovn-nbctl lsp-set-options ext-rtr-{vpcId} router-port=gw-{vpcId}

# Gateway chassis HA
ovn-nbctl lrp-set-gateway-chassis gw-{vpcId} chassis-1 20
ovn-nbctl lrp-set-gateway-chassis gw-{vpcId} chassis-2 15

# SNAT for all VPC traffic
ovn-nbctl lr-nat-add vpc-{vpcId} snat 192.168.1.150 10.0.0.0/16

# Default route to WAN
ovn-nbctl lr-route-add vpc-{vpcId} 0.0.0.0/0 192.168.1.1
```

## Per-Instance Public IP

```bash
# 1:1 NAT — distributed (DNAT processed on the VM's own chassis)
ovn-nbctl lr-nat-add vpc-{vpcId} dnat_and_snat {public_ip} {private_ip} \
  port-{eniId} {vm_mac}
```

With the WAN NIC on a Linux bridge wired to OVS via veth, OVS sees every frame
on the wire regardless of MAC, so OVN can use distributed NAT. The DNAT is
processed on the chassis hosting the VM rather than hairpinning through a
single gateway chassis.

## Security Group

```bash
# Create port group
ovn-nbctl pg-add sg-{groupId}

# Add VM ports
ovn-nbctl pg-set-ports sg-{groupId} port-{eniId1} port-{eniId2}

# Allow SSH inbound (stateful)
ovn-nbctl acl-add sg-{groupId} to-lport 1000 \
  'outport == @sg_{groupId} && ip4 && tcp.dst == 22' allow-related

# Allow all egress
ovn-nbctl acl-add sg-{groupId} from-lport 1000 \
  'inport == @sg_{groupId} && ip4' allow-related

# Default deny inbound
ovn-nbctl acl-add sg-{groupId} to-lport 900 \
  'outport == @sg_{groupId} && ip4' drop
```

## Useful Debug Commands

```bash
# List all logical routers (VPCs)
sudo ovn-nbctl lr-list

# List all logical switches (subnets + external)
sudo ovn-nbctl ls-list

# Show NAT rules for a VPC
sudo ovn-nbctl lr-nat-list vpc-{vpcId}

# Show routes for a VPC
sudo ovn-nbctl lr-route-list vpc-{vpcId}

# Show chassis (nodes) in the cluster
sudo ovn-sbctl show

# Show port bindings (which VM is on which host)
sudo ovn-sbctl find Port_Binding type="" | grep -E "logical_port|chassis"

# Check ACLs on a security group
sudo ovn-nbctl acl-list sg-{groupId}

# Check port group membership
sudo ovn-nbctl pg-get-ports sg-{groupId}
```

## Quick Start

## 1. Set Up OVN Bridges

Make sure the WAN NIC is enslaved to a Linux bridge (e.g. `br-wan`) and that
bridge owns the default route. Then run:

```bash
sudo setup-ovn.sh                       # auto-detect WAN bridge
# or
sudo setup-ovn.sh --wan-bridge=br-wan   # explicit
```

## 2. Configure External IP Pool

```bash
spx admin init
# Follow prompts — auto-detects NICs, suggests IP pool range
# Or edit spinifex.toml manually
```

## 3. Create VPC with Public Subnet

```bash
VPC=$(aws ec2 create-vpc --cidr-block 10.200.0.0/16 \
  --query Vpc.VpcId --output text)

SUBNET=$(aws ec2 create-subnet --vpc-id $VPC \
  --cidr-block 10.200.1.0/24 \
  --query Subnet.SubnetId --output text)

IGW=$(aws ec2 create-internet-gateway \
  --query InternetGateway.InternetGatewayId --output text)
aws ec2 attach-internet-gateway \
  --internet-gateway-id $IGW --vpc-id $VPC

aws ec2 modify-subnet-attribute \
  --subnet-id $SUBNET --map-public-ip-on-launch
```

## 4. Launch Instance

```bash
INSTANCE=$(aws ec2 run-instances \
  --image-id $AMI --instance-type t3.small \
  --subnet-id $SUBNET --key-name mykey \
  --query Instances[0].InstanceId --output text)

aws ec2 describe-instances --instance-ids $INSTANCE \
  --query 'Reservations[0].Instances[0].[PrivateIpAddress,PublicIpAddress]'
```

## Troubleshooting

### Debugging Toolkit

These commands are used throughout the troubleshooting sections below. Learn
them — they cover 90% of VPC networking issues.

### OVN Northbound (Logical Topology)

```bash
# Full topology overview (routers, switches, ports)
sudo ovn-nbctl show

# List all VPC routers
sudo ovn-nbctl lr-list

# List all switches (subnets + external)
sudo ovn-nbctl ls-list

# NAT rules for a VPC
sudo ovn-nbctl lr-nat-list vpc-{vpcId}

# Routes for a VPC
sudo ovn-nbctl lr-route-list vpc-{vpcId}

# Port details (check "up" field for DHCP status)
sudo ovn-nbctl list Logical_Switch_Port port-eni-{eniId}

# Gateway chassis assignment
sudo ovn-nbctl list Logical_Router_Port gw-vpc-{vpcId}

# Localnet port options (network_name should be "external")
sudo ovn-nbctl get Logical_Switch_Port ext-port-vpc-{vpcId} options
```

### OVN Southbound (runtime state)

```bash
# Chassis list + port bindings (which VM is on which host)
sudo ovn-sbctl show

# Detailed chassis info (check name matches expectations)
sudo ovn-sbctl list Chassis

# MAC binding table (shows ARP resolution for external traffic)
sudo ovn-sbctl list MAC_Binding

# Trace a packet through the OVN pipeline (invaluable for debugging)
sudo ovn-trace ext-vpc-{vpcId} \
  'inport=="ext-port-vpc-{vpcId}" && eth.dst==ff:ff:ff:ff:ff:ff && \
   arp.op==1 && arp.spa==192.168.1.13 && arp.tpa==192.168.1.201'
```

### OVS (datapath / physical wiring)

```bash
# Full bridge + port topology
sudo ovs-vsctl show

# Kernel datapath ports and stats
sudo ovs-dpctl show

# Kernel datapath flow cache (actual forwarding rules)
sudo ovs-dpctl dump-flows

# OpenFlow rules installed by ovn-controller
sudo ovs-ofctl dump-flows br-int | grep {pattern}

# Conntrack entries (shows active NAT sessions)
sudo ovs-appctl dpctl/dump-conntrack | grep {ip}

# FDB (MAC address table) for a bridge
sudo ovs-appctl fdb/show br-wan

# OVS external_ids (system-id, bridge-mappings, encap-ip)
sudo ovs-vsctl get Open_vSwitch . external_ids
```

### Network Interfaces

```bash
# Physical NIC should be enslaved to br-wan ("master br-wan")
ip -d link show {nic}

# Linux WAN bridge — owns the host IP and default route
ip -br addr show br-wan
ip route show default

# OVS bridges and the veth linking br-ext to br-wan
sudo ovs-vsctl list-ports br-ext
bridge link show | grep br-wan

# Interface traffic stats (RX/TX counts, drops)
ip -s link show {nic}
ip -s link show br-wan
```

### Packet Capture

```bash
# Capture on the OVS uplink bridge (frames between OVN and br-wan)
sudo tcpdump -i br-ext -n -e arp

# Capture on the Linux WAN bridge (frames between br-ext veth and the NIC)
sudo tcpdump -i br-wan -n -e "host {public_ip}"

# Capture on the physical NIC (sees everything on the wire)
sudo tcpdump -i {nic} -n "host {public_ip}"
```

### Service Logs

```bash
# vpcd log (reconcile, NAT, topology operations)
journalctl -u spinifex-vpcd -f

# ovn-controller log (port binding, commit failures)
sudo cat /var/log/ovn/ovn-controller.log | tail -50

# Daemon log (instance launch, network setup)
journalctl -u spinifex-daemon -f
```

### VPC Creation Fails

Check OVN services and vpcd daemon:

```bash
sudo systemctl is-active ovn-controller
journalctl -u spinifex-vpcd -f
```

### Instances Cannot Reach Each Other

Geneve tunnels may not be established:

```bash
sudo ovs-vsctl show | grep -i geneve
sudo ss -ulnp | grep 6081
```

From inside a VM:

```bash
ip addr show
ip route show
```

### Instance Has No Public IP

1. Check subnet has `MapPublicIpOnLaunch`:

   ```bash
   aws ec2 describe-subnets --subnet-ids $SUBNET \
     --query 'Subnets[0].MapPublicIpOnLaunch'
   ```

2. Check IGW is attached:

   ```bash
   aws ec2 describe-internet-gateways \
     --filters Name=attachment.vpc-id,Values=$VPC
   ```

3. Check external IP pool:

   ```bash
   nats kv get spinifex-external-ipam wan
   ```

4. Check OVN NAT rules:
   ```bash
   sudo ovn-nbctl lr-nat-list vpc-$VPC
   ```

### Public IP Not Reachable from WAN

Work through these checks in order — each eliminates a class of issues.

### 1. Verify OVS wiring

```bash
# br-int and br-ext must exist
sudo ovs-vsctl show | grep -E "Bridge (br-int|br-ext)"

# br-ext should have a veth port linking it to br-wan
sudo ovs-vsctl list-ports br-ext

# The Linux WAN bridge should have the physical NIC and the veth peer
bridge link show | grep br-wan

# Bridge mappings must point at br-ext
sudo ovs-vsctl get Open_vSwitch . external-ids:ovn-bridge-mappings
# Expected: "external:br-ext"
```

### 2. Verify chassis and gateway scheduling

```bash
# What OVS thinks the chassis name is
sudo ovs-vsctl get Open_vSwitch . external-ids:system-id

# What OVN SB registered (must match the system-id above)
sudo ovn-sbctl show
# Look for: Chassis {name}

# What vpcd scheduled as gateway chassis (must match SB chassis name)
sudo ovn-nbctl list Logical_Router_Port gw-vpc-{vpcId} | grep gateway_chassis
```

If these don't match, see "Chassis name mismatch" below.

### 3. Verify NAT rule

```bash
sudo ovn-nbctl lr-nat-list vpc-$VPC | grep dnat_and_snat
# Must show the public IP → private IP mapping
# For distributed NAT, external_mac and logical_port should be set
# (the VM's MAC and ENI port name)
```

### 4. Verify ARP resolution

From another host on the same LAN, check if OVN responds to ARP:

```bash
# On the remote host:
ping -c 1 {public_ip}
ip neigh show {public_ip}
# Should show the VM's ENI MAC (distributed NAT) or the OVN router MAC
```

If ARP fails, confirm the WAN bridge is forwarding:

```bash
# Physical NIC must be enslaved to br-wan
ip -d link show {nic} | grep "master br-wan"

# br-wan must be UP and have an IP
ip -br addr show br-wan

# br-ext must have a veth port whose peer is enslaved to br-wan
sudo ovs-vsctl list-ports br-ext
bridge link show | grep br-wan
```

### 5. Verify packet flow with tcpdump

Capture at each layer to find where packets stop:

```bash
# Layer 1: Does the ARP/ICMP arrive on the physical NIC?
sudo tcpdump -i {nic} -n "host {public_ip}"

# Layer 2: Does it cross the Linux WAN bridge?
sudo tcpdump -i br-wan -n "host {public_ip}"

# Layer 3: Does it reach the OVS uplink bridge?
sudo tcpdump -i br-ext -n -e "host {public_ip}"
```

If traffic arrives on the physical NIC but not br-wan, the NIC is not
enslaved to the bridge. If it reaches br-wan but not br-ext, the veth
pair between them is missing or down — re-run `setup-ovn.sh`.

### 6. Use ovn-trace for pipeline debugging

Simulate a packet through the entire OVN pipeline:

```bash
sudo ovn-trace --ct=new ext-vpc-{vpcId} \
  'inport=="ext-port-vpc-{vpcId}" && eth.dst==ff:ff:ff:ff:ff:ff && \
   arp.op==1 && arp.sha=={remote_mac} && arp.spa=={remote_ip} && \
   arp.tpa=={public_ip}'
```

The output shows every table the packet passes through and what action is
taken. Look for `drop` actions or unexpected paths.

### OVN SB Commit Failure Loop

**Symptom:** ovn-controller log shows:

```
OVNSB commit failed, force recompute next time.
```

Repeated millions of times. Port binding never happens (`up: false`).

**Cause:** Stale entries in the OVN Southbound DB (old chassis records, port
bindings, datapath bindings) conflict with ovn-controller's expected state.
Happens when `reset-dev-env.sh` cleans NB but not SB, or after ungraceful
shutdowns.

**Fix:** Delete both OVN DB files and restart. `reset-dev-env.sh` does this
automatically:

```bash
sudo systemctl stop ovn-central ovn-controller
sudo rm -f /var/lib/ovn/ovnnb_db.db /var/lib/ovn/ovnsb_db.db
sudo systemctl start ovn-central ovn-controller
# vpcd reconcile will recreate the NB topology on next startup
```

### Chassis Name Mismatch

After mulga-999, vpcd uses the OVS-managed UUID (persisted at
`/etc/openvswitch/system-id.conf` by the `openvswitch-switch` package and
re-applied on every boot) as the canonical chassis identity. The chassis name
across OVS `external_ids:system-id`, OVN SB `Chassis.name`, and NB
`Gateway_Chassis.chassis_name` is always the same value, so the mismatch
class that this section previously documented can no longer recur.

`ReconcileFromKV` runs a `reconcileGatewayChassis` pre-step on every vpcd
startup that deletes any stale `Gateway_Chassis` row left over from earlier
installs (where `setup-ovn.sh` fabricated `chassis-$(hostname -s)`) and
re-binds every gateway LRP against the live SBDB chassis. Pre-mulga-999
brokenness recovers automatically on the first vpcd restart after upgrade —
no manual `ovn-nbctl destroy` required.

### WAN NIC Not Enslaved to a Bridge

**Symptom:** `setup-ovn.sh` exits with an error like "default route is on a
physical NIC, not a bridge" and refuses to continue.

**Cause:** Spinifex requires the WAN NIC to be enslaved to a Linux bridge
(typically `br-wan`). Macvlan is no longer supported, and attaching the NIC
directly to OVS would break SSH and any other host services using the NIC.

**Fix:** Move the host IP and default route onto a Linux bridge. Example
netplan:

```yaml
network:
  version: 2
  ethernets:
    wan:
      dhcp4: false
  bridges:
    br-wan:
      interfaces: [wan]
      dhcp4: true
```

Apply (`sudo netplan apply`), confirm the host IP is now on `br-wan`
(`ip -br addr show br-wan`), and re-run `setup-ovn.sh`.

### Stale ARP on Remote Hosts

**Symptom:** Ping from a LAN host to a VM public IP fails after a reset, but
worked before. The remote host has a stale ARP entry with the old MAC.

**Fix:** Flush the ARP entry on the remote host:

```bash
# On the remote host:
sudo ip neigh flush dev {nic} {public_ip}
ping {public_ip}   # should work now
```

OVN sends periodic gratuitous ARPs from the chassis hosting the VM that will
eventually update remote ARP caches, but flushing is faster for testing.

### Security Group Rules Not Taking Effect

```bash
# Check port is in correct port group
sudo ovn-nbctl pg-get-ports sg-$SG_ID

# Check ACLs
sudo ovn-nbctl acl-list sg-$SG_ID
```
