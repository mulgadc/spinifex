---
title: "Performance Tuning"
description: "Kernel, overlay and guest NIC tuning for Spinifex clusters, with measured results."
category: "Install"
tags:
  - performance
  - tuning
  - networking
  - sysctl
  - ipsec
resources:
  - title: "VPC Networking"
    url: "/docs/vpc-networking"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
---

# Performance Tuning

A starter guide. Everything here is measured on a four-node cluster rather than
inferred, and every number states the configuration it came from. Expect this to
grow — several sections end in an open question rather than a recommendation.

**Test rig for all figures below:** 4 nodes, AMD EPYC 7513 (128 threads), Mellanox
ConnectX-5 (MT27800) on a 100 GbE fabric, `t3.xlarge` guests (4 vCPU), Ubuntu 26.04,
kernel 6.12. Throughput is `iperf` between guest private addresses over the VPC
overlay, 4 streams, 60s.

## Read this first: the dominant cost is not on this page

The single largest gap between a guest and its host is not any tunable below. It is
that **the VPC overlay currently gets no hardware segmentation offload**, so the host
CPU segments every guest packet in software.

Measured with the same script, same parameters, on the same NIC and switch, three
clients into one server at 4 streams each:

| | Host, on the underlay | Guest, over the VPC | |
| --- | --- | --- | --- |
| aggregate | **92.6 Gbit/s** | **17.6 Gbit/s** | **5.3x** |
| frame on the wire | 1518 B | 1518 B | identical |
| TSO super-packets | 4,165,267 | **0** | |
| wire packets per descriptor | 39.3 | 1.0 | |

The guest's own offloads are fine — its GSO super-packets reach the host intact at
~64 KB. They are then broken into individual 1518-byte frames in software between the
tap and the NIC. The cause is under investigation and is not yet fixed; it is
independent of IPsec, and it is larger than every item in the table below combined.

Until it is resolved, treat the figures on this page as tuning within a constraint,
not as the ceiling.

## Summary — what moved the needle

| Change | Effect |
| --- | --- |
| UDP receive buffer sysctl | Fixes storage faults. No throughput change. |
| vhost-net on the guest NIC | QEMU main loop 60% of a core → 3%. No throughput change on its own. |
| Guest NIC RX ring 256 → 1024 | Removes a drop source under burst. |
| Guest NIC multiqueue | **−12% with IPsec on, +54% with IPsec off** |
| IPsec off (trusted links only) | **2.5x** — see the warning in §2 |
| Guest MTU 1408 → 1442 | ~10%, and only valid with IPsec off |
| Jumbo frames | Untested end to end. Needs a switch that carries it. |

On a trusted-fabric configuration (IPsec off, multiqueue on) mean per-client
throughput went from **1.23 Gbit/s to 6.64 Gbit/s** and aggregate into one guest from
3.69 to **19.92 Gbit/s**. That 5.4x is *relative to an encrypted baseline* — it is
mostly the cost of encryption being removed, not tuning headroom being found. The
default, encrypted configuration is the one most clusters should run.

## 1. Kernel tunables (sysctl)

Applied automatically by `setup.sh`'s `sysctl` stage to
`/etc/sysctl.d/99-spinifex-net.conf`. Documented here because it is load-bearing and
because the failure mode it prevents is severe.

```
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
```

Predastore carries blob, meta and raft traffic over QUIC, and quic-go requests a
7 MiB UDP receive buffer. At the kernel default of 208 KiB it gets 416 KiB and
silently drops datagrams under load. On a cluster that had been running at the
default, the four nodes had shed 80k–125k datagrams each to `UdpRcvbufErrors`.

The consequence is not a slow cluster, it is a broken one: dropped datagrams fail
QUIC streams, which fail erasure-coded shard writes, which exhaust viperblock's retry
budget, which returns `EIO` to the guest, which aborts its ext4 journal and remounts
read-only. Two guests lost their root filesystems this way before the cause was found.

Verify with:

```bash
nstat -az | grep UdpRcvbufErrors    # must not advance under load
journalctl -u spinifex-predastore | grep 'receive buffer size'   # must be silent
```

## 2. IPsec on the overlay

Spinifex encrypts the geneve overlay with OVN native IPsec by default
(ESP transport mode, `rfc4106(gcm(aes))` 128). This is the right default and matches
the AWS trust model. **On a physically trusted fabric it is also the single largest
performance cost in the stack.**

### Why it costs so much

Not raw crypto throughput. The problem is CPU scaling.

ESP wraps each geneve packet in an outer IP header with protocol 50, which has no
L4 ports. A NIC's receive-side scaling can therefore only hash on the source and
destination IP, so **all traffic between a given pair of nodes lands in one RSS
bucket and is processed by one CPU core**. Without ESP the outer header is UDP 6081
and OVN varies the source port per inner flow, which spreads across every core.

Measured on the receiving host during a transfer:

| | busiest core | spread |
| --- | --- | --- |
| IPsec on | **97.4% busy**, 71.8% softirq | 1 core above 15%, on a 128-thread box |
| IPsec off | 19% of NET_RX | **16 cores** sharing it, none above 15% |

Things that do **not** fix this:

- **ConnectX-5 has no IPsec crypto offload.** ASAP² handles OVS flow offload but IPsec
  inline crypto needs ConnectX-6 Dx or BlueField. Not available on this hardware.
- **RPS/RFS.** The Linux flow dissector can hash ESP by SPI, but OVN creates one SA
  per node pair per direction, so the SPI is constant for all traffic between two
  nodes and every packet still hashes to one bucket.
- **More TCP streams.** Throughput is flat in stream count with IPsec on — 1 stream
  and 4 streams give the same number, because they all funnel through the same core.

Genuinely promising but untested: `pcrypt`, which parallelises a single SA's crypto
across cores while preserving ordering. It requires the SA to be created with a
`pcrypt(...)` template, which OVN does not currently expose.

### What disabling it is worth

Measured on the test rig, three clients into one guest, 4 streams each, 60s:

| | IPsec on (default) | IPsec off | |
| --- | --- | --- | --- |
| aggregate into one guest | 3.58 Gbit/s | **17.6 Gbit/s** | **4.9x** |
| per client | 1.19 Gbit/s | **5.88 Gbit/s** | |
| guest MTU | 1408 | 1442 | applied automatically |
| NIC queue pairs | 1 | 4 | follows `ipsec_enabled` |
| busiest receive core | 97.4% | under 15%, spread over 16 cores | |

The MTU and multiqueue changes are not separate knobs — both follow
`ipsec_enabled` on their own, because the sign of the multiqueue result flips
with it (see §3). Turning encryption off is the whole change.

This is the largest single lever available today, and the trade is explicit: all
east-west guest traffic goes on the wire unencrypted.

### Disabling it for trusted links

Only for topologies where the underlay is physically trusted — a single rack, a private VLAN, no untrusted tenant on the fabric. The trade-off, the topologies it does and does not apply to, and the same procedure in more detail are in the VPC networking guide: [Overlay Encryption (IPsec)](/docs/vpc-networking#overlay-encryption-ipsec).

In `/etc/spinifex/spinifex.toml` on **every** node:

```toml
[network]
ipsec_enabled = false
```

Then restart the daemon: `systemctl restart spinifex-daemon`. Confirm with `ip xfrm state | grep -c '^src'`, which must reach 0 on every node.

**Then clear the OVN and OVS flags by hand.** The reconciler returns early when IPsec is disabled: it stops requesting encryption, but never clears what it previously set, so `NB_Global.ipsec` and `other_config:ipsec_encapsulation` are left reading `true`. They are inert — nothing remains to act on them once charon and the SAs are gone — but the control plane then disagrees with the config, and the next person to inspect it will read the cluster as encrypted when it is not.

```bash
# Use the NB raft endpoints. The local unix socket fails with
# "database connection failed" on a clustered deployment.
NBDB='tcp:10.2.0.2:6641,tcp:10.2.0.3:6641,tcp:10.2.0.4:6641'
ovn-nbctl --db="$NBDB" set NB_Global . ipsec=false

# On every node:
ovs-vsctl remove Open_vSwitch . other_config ipsec_encapsulation
```

This is manual today and should move into the reconciler. Note that these commands are a *complement* to the config change, not a substitute for it — setting `NB_Global.ipsec=false` without `ipsec_enabled = false` leaves the daemon reconciling encryption back on, and leaves the guest MTU and queue-pair count on their encrypted values.

**Restart guests after changing this in either direction.** The guest MTU moves
with the setting, but a running guest holds its DHCP lease for up to an hour and
its device-level `host_mtu` until it is stopped and started. Going from off to on
narrows the path from 1442 to 1408 while the guest still believes 1442, which
blackholes large segments until it is restarted.

**Expect launches to fail for a minute or two after the change.** Re-enabling
IPsec makes `RunInstances` return `ServerInternal` with `vpc.add-nat` timing out,
because the OVN flows-ready barrier cannot confirm while ovn-controller reprograms
every tunnel for the new SAs. It clears once `ip xfrm state` settles. The error
is reported as a capacity problem, which it is not.

## 3. Guest NIC: vhost-net and multiqueue

`vhost=on` moves the guest NIC datapath into a kernel thread instead of copying every
packet on QEMU's main loop. It is unconditional and not tunable. Its effect is
dramatic on CPU (main loop 60% of a core → 3%) and, on its own, **zero on throughput**
— because with IPsec on, the ESP funnel downstream is the binding constraint. It
matters once that funnel is removed.

Multiqueue gives the NIC one queue pair per vCPU. Its value depends entirely on
whether IPsec is on:

| | 1 client | 3-client aggregate |
| --- | --- | --- |
| single queue, IPsec on | 1.75 | 3.69 |
| multiqueue, IPsec on | 1.54 | 4.35 |
| single queue, IPsec off | 4.46 | 9.97 |
| multiqueue, IPsec off | **6.85** | **18.23** |

With IPsec on, multiqueue **costs** throughput. Spreading one flow across queue pairs
reorders packets and TCP reads that as loss — guest counters showed `reordering:189`,
`dsack_dups:116` and `ssthresh` collapsing. There is no headroom to win it back
because the bottleneck is elsewhere.

With IPsec off it is worth **+54%**, because the bottleneck moves to the sending
host's vhost thread, which is exactly what extra queues relieve. At single queue that
thread sits at 99.9%.

Because the sign flips, multiqueue is not a knob of its own: it follows
`ipsec_enabled` automatically, off when IPsec is on and on when it is off. There is
nothing to set.

## 4. MTU

### The overlay path

The guest MTU is advertised by DHCP and is **1408** with IPsec on, derived as
1500 − 58 (geneve) − 34 (ESP). With IPsec disabled the ESP term disappears and it
becomes **1442**, worth roughly 10%. This follows `ipsec_enabled` on its own, and the
reconciler converges existing subnets on the next pass — a guest picks the new value
up at its next lease. To apply it immediately without waiting, renew in the guest
(`dhclient -r && dhclient`) or set it directly: `ip link set enp0s16 mtu 1442`.

Never widen it by hand on an encrypted overlay. Advertising 1442 over ESP is the
failure this budget exists to prevent.

### Jumbo frames — check the fabric first

Set `network.underlay_mtu` in `/etc/spinifex/spinifex.toml` to the figure the fabric
between nodes actually carries. It defaults to 1500. The guest MTU is derived from it
— 9000 gives a guest 8942, or 8908 with IPsec — and reaches the guest three ways: the
DHCP option, the virtio device's `host_mtu`, and the host-side tap. A running guest
needs a stop/start to pick up a new `host_mtu`.

Jumbo would substantially mitigate the software-segmentation cost described at the top
of this page, because it cuts the descriptor count roughly sixfold. It does not remove
it. Note that the 92.6 Gbit/s host figure above was measured at MTU **1500**, so the
fabric is not the binding constraint it was once assumed to be.

**But the host NICs are only half the path.** On the test cluster, raising both the
physical NIC and `br-vpc` to 9000 on all four nodes produced total packet loss above
1500 bytes — the switch was not configured for jumbo:

```
payload 1472 : 2 received     <- 1500 MTU
payload 1972 : 0 received
payload 8972 : 0 received
```

Always verify before enabling, and revert if it fails — a host at 9000 behind a 1500
fabric black-holes every large frame, and an L2 switch sends no "fragmentation
needed" back, so path MTU discovery cannot recover:

```bash
ip link set <dev> mtu 9000        # both ends
ping -M do -s 8972 -c 3 <peer>    # must succeed
```

## 5. Measurement notes

Throughput on this stack is **noisy** — repeated identical 20s runs varied between
7.4 and 15.4 Gbit/s in the best configuration. Take medians of at least three runs
before believing a change helped.

The likely cause is vhost and softirq threads migrating across 128 threads and two
NUMA nodes. Thread pinning and IRQ affinity are the obvious next tuning avenue and
have not been explored.

Useful commands:

```bash
# Where is the receive work landing? One hot CPU means a funnel.
awk '/NET_RX/{for(i=2;i<=NF;i++) printf "cpu%d %s\n", i-2, $i}' /proc/softirqs

# Is vhost saturated? 99% on one thread means add queues.
top -H -p $(pgrep -f qemu-system-x86_64 | head -1)

# Confirm the guest negotiated the queues QEMU offered.
ethtool -l enp0s16      # in the guest
```

## Open questions

- **Why does the overlay lose GSO between the tap and the NIC?** The two candidates
  are the mlx5 driver declining to offload a Geneve-encapsulated GSO skb on this
  firmware, and something in the OVS datapath dropping GSO before the driver sees it.
  They are distinguishable by running the same measurement over a VXLAN tunnel, which
  takes a different path through the driver's feature check. This is the highest-value
  open question on the page by a wide margin.
- Does `pcrypt` make single-SA IPsec scale across cores, and can OVN be made to
  request it?
- Would multiple SAs per node pair restore RSS spread while keeping encryption?
- How much does jumbo actually buy on a fabric that supports it? Untested.
- Does pinning vhost threads to the NIC's NUMA node remove the run-to-run variance?
