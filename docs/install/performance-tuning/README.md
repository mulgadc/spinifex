---
title: "Performance Tuning"
description: "Kernel, overlay and guest NIC tuning for Spinifex clusters, with measured results."
category: "Install"
tags:
  - performance
  - tuning
  - networking
  - sysctl
resources:
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

## Summary — what moved the needle

| Change | Effect |
| --- | --- |
| UDP receive buffer sysctl | Fixes storage faults. No throughput change. |
| vhost-net on the guest NIC | QEMU main loop 60% of a core → 3%. No throughput change on its own. |
| IPsec off (trusted links) | **2.5x** |
| Guest NIC multiqueue | **−12% with IPsec on, +54% with IPsec off** |
| Guest MTU 1408 → 1442 | ~10%, only valid with IPsec off |
| Jumbo frames | Unavailable — fabric caps at 1500 |

End to end, mean per-client throughput went from **1.23 Gbit/s to 6.64 Gbit/s (5.4x)**
and aggregate into one guest from 3.69 to **19.92 Gbit/s**. Single-client runs reach
10–15 Gbit/s.

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

### Disabling it for trusted links

Only for topologies where the underlay is physically trusted — a single rack, a
private VLAN, no untrusted tenant on the fabric. This turns off encryption for all
east-west guest traffic.

In `/etc/spinifex/spinifex.toml` on **every** node:

```toml
[network]
ipsec_enabled = false
```

Then restart the daemon: `systemctl restart spinifex-daemon`. Confirm with
`ip xfrm state | grep -c '^src'`, which must reach 0 on every node.

Note that `ovn-nbctl set NB_Global . ipsec=false` is **not** sufficient on its own —
the daemon reconciles that value, so a manual override is transient. Change the
config file.

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

The installer exposes `SPINIFEX_LAN_MTU` and `SPINIFEX_VPC_MTU`, and the ConnectX-5
supports up to 9978. Jumbo frames would be the largest single win available, because
at 1442 bytes a 10 Gbit/s flow is ~850k packets/sec and almost all the cost is
per-packet.

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

- Does `pcrypt` make single-SA IPsec scale across cores, and can OVN be made to
  request it?
- Would multiple SAs per node pair restore RSS spread while keeping encryption?
- How much does jumbo actually buy on a fabric that supports it? Untested.
- Does pinning vhost threads to the NIC's NUMA node remove the run-to-run variance?
