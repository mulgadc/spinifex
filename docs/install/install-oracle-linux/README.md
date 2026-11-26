---
title: "Oracle Linux Hosts"
seoTitle: "Install Spinifex on Oracle Linux 10 — Spinifex Docs"
description: "Install Spinifex on Oracle Linux 10: prepare the host with UEK R8, SELinux permissive and a NetworkManager br-wan bridge, then run the single-node install."
category: "Install"
tags:
  - install
  - oracle linux
  - single node
  - multi node
resources:
  - title: "Oracle Linux 10 Documentation"
    url: "https://docs.oracle.com/en/operating-systems/oracle-linux/10/"
  - title: "CentOS NFV SIG"
    url: "https://sigs.centos.org/nfv/"
---

# Oracle Linux Hosts

> Install Spinifex on Oracle Linux 10.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [What the Installer Changes](#what-the-installer-changes)
- [Troubleshooting](#troubleshooting)

---

## Overview

Spinifex runs on Oracle Linux 10.2, on the UEK R8 kernel, on x86_64 with an x86-64-v3 CPU (Intel Haswell, AMD Excavator or later).

The host must be a dedicated Spinifex host: Spinifex claims every `tap*` device and owns libreswan's `ipsec.service`.

**Not supported yet:** clusters mixing Oracle Linux with Debian or Ubuntu, SELinux enforcing, aarch64, Oracle Linux 9 or earlier, the Red Hat Compatible Kernel, a ZFS root, NVIDIA MIG and air-gapped installs.

## Instructions

Work through both tabs in order.

:::tabs
@tab 1. Prepare Oracle Linux

### Update and check the host

Update to the current minor release and reboot:

```bash
sudo dnf update -y
sudo reboot
```

Then check the release, the running kernel and the CPU:

```bash
cat /etc/oracle-release        # Oracle Linux Server release 10.2 or later
uname -r                       # 6.12.0-...el10uek.x86_64
grep -c -w avx2 /proc/cpuinfo  # non-zero
```

### Set SELinux permissive

```bash
sudo setenforce 0
sudo sed -i 's/^SELINUX=.*/SELINUX=permissive/' /etc/selinux/config
getenforce                     # Permissive
```

### Create the br-wan bridge

Run this from the console, not over SSH on the NIC being moved.
Replace `eno1` with the uplink NIC and `"System eno1"` with its current profile (`nmcli connection show`):

```bash
sudo nmcli connection add type bridge ifname br-wan con-name br-wan \
  bridge.stp no ipv4.method auto ipv6.method disabled
sudo nmcli connection add type bridge-slave ifname eno1 master br-wan con-name br-wan-port
sudo nmcli connection down "System eno1"
sudo nmcli connection up br-wan
```

For a static address, use `ipv4.method manual` with `ipv4.addresses`, `ipv4.gateway` and `ipv4.dns`.
Keep `bridge.stp no`, or DHCP on the bridge can time out.

```bash
ip -br link show br-wan        # UP
ip route                       # default via ... dev br-wan
```

@tab 2. Install Single Node

### Install Spinifex

```bash
curl -fsSL https://install.mulgadc.com | bash
```

The installer stops and masks firewalld; see [firewalld](#firewalld) before relying on any rule it held.

### Set up OVN networking

```bash
sudo /usr/local/share/spinifex/setup-ovn.sh --management
```

### Initialize

```bash
sudo spx admin init --node node1 --nodes 1
```

### Start services

```bash
sudo systemctl start spinifex.target
```

### Verify

```bash
export AWS_PROFILE=spinifex
aws ec2 describe-instance-types
```

If this returns a list of instance types, Spinifex is installed.
Continue to [Setting Up Your Cluster](/docs/setting-up-your-cluster) to import an AMI and launch your first instance.
:::

**For a cluster**, prepare and install every node with the steps above, then continue with [Multi-Node Install](/docs/install-multi-node) from Step 2.
Every node must run Oracle Linux.
IPsec between nodes runs on libreswan, so open UDP 500, UDP 4500 and ESP between nodes on any firewall between them.

## What the Installer Changes

On Oracle Linux the installer also:

- Enables `ol10_UEKR8` and `ol10_kvm_utils`, and excludes `qemu-kvm*` and `qemu-img*` from `ol10_appstream`.
- Adds the NFV SIG repository as `/etc/yum.repos.d/spinifex-nfv-sig.repo`, limited to the OVS and OVN packages.
- Installs `kernel-uek-modules` for the running kernel, about 800 MB with `linux-firmware`.
- Switches DNS to systemd-resolved behind NetworkManager where resolved is installed but not running.
- Links `/usr/bin/spx` to `/usr/local/bin/spx`, so `sudo spx` works.

### firewalld

The installer stops and masks firewalld and turns on the Spinifex nftables policy in its place; firewalld's rules are not migrated.
The Spinifex policy opens 53, 443, 3000, 8443, 9999 and SSH to any source.
Restate any restriction you relied on, such as SSH limited to a management subnet, in `/etc/spinifex/firewall/custom.nft`; see [Host Firewall](/docs/host-firewall).

### Updates

Do not enable `dnf-automatic`: an Open vSwitch upgrade restarts OVS and interrupts guest networking, so update `openvswitch3.5`, `ovn25.03*` and `qemu-kvm*` in a maintenance window.
Before rebooting into a new kernel, check `rpm -q kernel-uek-core kernel-uek-modules` shows matching versions, or OVS cannot start.

### Uninstall

`/usr/local/share/spinifex/uninstall-spx.sh` removes Spinifex and lists what it leaves in place.
`--purge-deps` is refused on Oracle Linux; remove packages with `dnf remove` yourself.

## Troubleshooting

### Installer stops on SELinux, the kernel or kernel modules

Set SELinux permissive both now and in `/etc/selinux/config`, or update and boot into the current UEK R8 kernel, then run the installer again:

```bash
sudo dnf update -y kernel-uek
sudo reboot
```

### Installer stops: a repository is unreachable

Check the host can reach `yum.oracle.com` and `mirror.stream.centos.org` over HTTPS, including through any proxy dnf uses.

### DHCP on br-wan times out

STP is on. Turn it off:

```bash
sudo nmcli connection modify br-wan bridge.stp no
sudo nmcli connection up br-wan
```

### Cluster nodes form no IPsec tunnels

```bash
systemctl is-active ipsec.service openvswitch-ipsec.service
sudo ipsec trafficstatus                       # one line per peer tunnel
```

If the services are inactive, check `journalctl -u spinifex-daemon`.
If no tunnel is listed, check UDP 500 and ESP pass between the nodes in both directions.
Guest traffic between two nodes is dropped until their tunnel forms.

### CA certificate not trusted

```bash
sudo cp /etc/spinifex/ca.pem /etc/pki/ca-trust/source/anchors/spinifex-ca.crt
sudo update-ca-trust
```
