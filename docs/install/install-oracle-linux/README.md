---
title: "Oracle Linux Hosts"
seoTitle: "Install Spinifex on Oracle Linux 10 — Spinifex Docs"
description: "Prepare Oracle Linux 10 hosts for Spinifex: UEK R8, SELinux, a NetworkManager br-wan bridge, the NFV SIG repository, IPsec on libreswan and host updates."
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

> What differs when the Spinifex host runs Oracle Linux 10 instead of Debian or Ubuntu.

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

Spinifex installs on Oracle Linux 10.2 or a later 10.x release, running the UEK R8 kernel, on x86_64.
The installer, the release tarball and the install steps are the same as on Debian; this page covers what the host needs first and what the installer changes on it.
Follow it alongside [Single-Node Install](/docs/install).

**Supported on Oracle Linux:**

- A single node, or a three-node cluster in which every node runs Oracle Linux.
- Guests, VPCs, routed NAT, Elastic IPs, and Application and Network Load Balancers.
- Whole-GPU passthrough, on x86_64 GPU hosts (see [GPU Passthrough](/docs/gpu-passthrough)).

**Not supported on Oracle Linux yet:**

- Clusters of more than three nodes.
- Clusters that mix Oracle Linux with Debian or Ubuntu nodes, on any release.
- SELinux enforcing, aarch64, Oracle Linux 9 or earlier, the Red Hat Compatible Kernel, a ZFS root, and NVIDIA MIG.

Open vSwitch and OVN come from the CentOS NFV SIG, because Oracle ships neither for Oracle Linux 10.
The NFV SIG is a community repository, and Oracle does not support the packages it publishes.
Spinifex installs `openvswitch3.5` and `ovn25.03`, the versions Debian 13 ships, and its repository file lets the SIG supply nothing else.
QEMU comes from Oracle's KVM Utilities repository, because only that build has the `microvm` machine type Spinifex load balancers boot on.

An Oracle Linux node must be a dedicated Spinifex host.
Spinifex claims every `tap*` device and owns libreswan's `ipsec.service`, which it keeps stopped on a single node and runs for its own tunnels on a cluster node, so VPNs, hand-run QEMU guests and libreswan tunnels of your own cannot share the host.
Crypto policies other than `DEFAULT` and `FIPS` are untested, and so is Secure Boot.

## Prerequisites

### Operating system and kernel

Install Oracle Linux 10 with Oracle's ISO or a KVM template, then update it to the current minor release and reboot:

```bash
sudo dnf update -y
sudo reboot
```

After the reboot, check the release, the kernel and the CPU:

```bash
cat /etc/oracle-release        # Oracle Linux Server release 10.2 or later
uname -r                       # 6.12.0-...el10uek.x86_64
grep -c -w avx2 /proc/cpuinfo  # non-zero
```

The kernel must be UEK R8 (`6.12.0-*uek`), and it must be the running kernel: an installed but not booted `kernel-uek` is refused.
The CPU must be x86-64-v3 or newer (Intel Haswell, AMD Excavator or later), since the NFV SIG and KVM Utilities builds target it.
The installer names any missing CPU flag.

The installer adds `kernel-uek-modules` for the running kernel, which carries `openvswitch.ko` and pulls in `linux-firmware`, about 800 MB.
Oracle publishes modules for each kernel separately, so a host running an old kernel may find its modules gone from the repository; the installer then stops and asks for a kernel update and a reboot.

### SELinux

Spinifex ships no SELinux policy yet, so SELinux must be permissive or disabled, both now and at the next boot.
Oracle's images boot enforcing.

```bash
sudo setenforce 0
sudo sed -i 's/^SELINUX=.*/SELINUX=permissive/' /etc/selinux/config
getenforce                     # Permissive
```

The installer and `spx admin preflight` refuse a host that is enforcing in either place.

### WAN bridge with NetworkManager

As on every OS, the host's uplink must already sit on a Linux bridge named `br-wan`, with the host address and default route on the bridge (see [Single-Node Install](/docs/install#prerequisites)).
On Oracle Linux, NetworkManager owns `br-wan`; Spinifex leaves the bridge and its NIC to it and marks the links it creates itself as unmanaged.

Create the bridge from the console, not over SSH on the NIC being moved, since the host loses its address while the profile changes.
Replace `eno1` with the uplink NIC and `"System eno1"` with its current profile (`nmcli connection show`):

```bash
sudo nmcli connection add type bridge ifname br-wan con-name br-wan \
  bridge.stp no ipv4.method auto ipv6.method disabled
sudo nmcli connection add type bridge-slave ifname eno1 master br-wan con-name br-wan-port
sudo nmcli connection down "System eno1"
sudo nmcli connection up br-wan
```

For a static address, give the bridge `ipv4.method manual` with `ipv4.addresses`, `ipv4.gateway` and `ipv4.dns` instead of `ipv4.method auto`.

`bridge.stp no` matters: a NetworkManager bridge runs STP by default, which holds the port in its forwarding delay for about 15 seconds after every link up, long enough for DHCP on the bridge to time out.
Then verify, as on Debian:

```bash
ip -br link show br-wan        # UP
ip route                       # default via ... dev br-wan
```

### Network access during install

Besides `install.mulgadc.com` and the AWS CLI download, the install reaches `yum.oracle.com` for the Oracle Linux repositories, `mirror.stream.centos.org` for the NFV SIG packages, and `www.centos.org` for the NFV SIG signing key.
An air-gapped install on Oracle Linux is not documented yet.

## Instructions

## Step 1. Install Spinifex

Run the installer as a user with `sudo`, exactly as on Debian:

```bash
curl -fsSL https://install.mulgadc.com | bash
```

For a single node, continue with [Single-Node Install](/docs/install) from Step 2: `setup-ovn.sh --management`, `spx admin init` and `systemctl start spinifex.target`.
IPsec stays off on a single node, because there is no peer to encrypt traffic to.

For a cluster, prepare and install all three nodes this way, then continue with [Multi-Node Install](/docs/install-multi-node) from Step 2.

### Clusters

Every node in an Oracle Linux cluster runs Oracle Linux.
`install-node.sh` refuses a host list that mixes Oracle Linux with Debian or Ubuntu, but `spx admin join` does not check, so keep to one OS yourself when joining by hand.

IPsec encrypts the Geneve tunnels between nodes and is on by default, as on Debian.
On Oracle Linux it runs on libreswan, the IKE daemon Oracle ships, rather than strongSwan.
Once the node's IPsec certificate exists, the daemon unmasks and starts `ipsec.service` and `openvswitch-ipsec.service`.
`ovs-monitor-ipsec` then loads the node certificate, its key and the cluster CA into libreswan's NSS database at `/var/lib/ipsec/nss`, under nicknames that start with `ovs_cert`.

Between nodes, open UDP 500 for IKE and UDP 4500, which on Oracle Linux carries the tunnel traffic itself: Spinifex asks libreswan to wrap ESP in UDP rather than send it as IP protocol 50.
The Spinifex host firewall opens both, and ESP, to cluster peers; a firewall between the nodes must allow them too.
`spx admin init --ipsec=false` leaves the tunnels unencrypted, as on any OS.

## Step 2. Know What the Installer Changed

On Oracle Linux the installer makes these changes, which Debian hosts do not need:

- **Repositories.** It enables `ol10_UEKR8` and `ol10_kvm_utils`, and adds `qemu-kvm*` and `qemu-img*` to the `excludepkgs` of `ol10_appstream`, keeping any excludes already set, so a later AppStream build can never replace the KVM Utilities QEMU.
- **NFV SIG.** It writes `/etc/yum.repos.d/spinifex-nfv-sig.repo` with an `includepkgs=` list of the OVS and OVN packages, and imports the SIG key to `/etc/pki/rpm-gpg/RPM-GPG-KEY-CentOS-SIG-NFV` only when its fingerprint is `35154228174901BEFA8E69A621465E289D2A76A7`.
- **OVS and OVN user.** Both run as root, as on Debian, through `OVS_USER_ID` and `OVN_USER_ID` in `/etc/sysconfig/openvswitch` and `/etc/sysconfig/ovn`.
- **DNS.** Where systemd-resolved is installed but not running, it saves `/etc/resolv.conf` to `/var/lib/spinifex-reset/resolv.conf.orig`, starts resolved behind NetworkManager and links `/etc/resolv.conf` to the resolved stub. It then checks that names still resolve and stops, printing the commands that put DNS back, if they do not.
- **chrony.** It appends `confdir /etc/chrony.d` to `/etc/chrony.conf` once, and writes its step policy to `/etc/chrony.d/spinifex.conf`.
- **`spx` under `sudo`.** Oracle Linux's `sudo` path leaves out `/usr/local/bin`, so the installer links `/usr/bin/spx` to `/usr/local/bin/spx`.

The kernel modules list (`/etc/modules-load.d/spinifex.conf`) and the host firewall below apply on every OS.

## Step 3. Replace firewalld Rules You Depend On

The installer stops and masks firewalld, because with it running a packet must pass both firewalld's table and the Spinifex one, which silently breaks routed NAT, Elastic IPs and instance metadata.
When firewalld was active, the installer turns on the Spinifex nftables policy in its place; pass `--firewall=on` or `--firewall=off` to choose yourself.

firewalld's rules are not saved or migrated.
The Spinifex policy filters inbound traffic only and opens 53, 443, 3000, 8443 and 9999 to any source.
It accepts SSH from every IPv4 source until `$trusted_ssh_peers` in `/etc/spinifex/firewall/custom.nft` narrows it.
Restate any other restriction you had, such as SSH limited to a management subnet, in that file; [Host Firewall](/docs/host-firewall) shows how.

From the moment firewalld stops until the daemon first loads the Spinifex policy, the host accepts every connection.
Uninstalling Spinifex leaves firewalld unmasked but disabled; it does not turn it back on.

## Step 4. Keep the Host Updated

Do not enable `dnf-automatic`.
An Open vSwitch package upgrade restarts OVS, which interrupts guest networking, so update `openvswitch3.5`, `ovn25.03*` and `qemu-kvm*` in a maintenance window.
Stay on the current Oracle Linux 10 minor release, which is what Spinifex is tested on.

Before rebooting into a new kernel, check that its modules are installed, or OVS cannot start after the reboot:

```bash
rpm -q kernel-uek-core kernel-uek-modules
```

Each `kernel-uek-core` version needs a `kernel-uek-modules` of the same version; install a missing one with `sudo dnf install kernel-uek-modules-<version>`.

Track Oracle Linux security advisories (ELSA) for the host, the kernel and QEMU, for example with `dnf updateinfo list --security`.
Those advisories do not cover the NFV SIG packages, so follow Open vSwitch and OVN security announcements for those.

## Step 5. Uninstall

`/usr/local/share/spinifex/uninstall-spx.sh` removes Spinifex as on Debian.
On a cluster node it also stops `openvswitch-ipsec.service` and deletes the `ovs_cert*` entries from libreswan's NSS database, so the node can join a different cluster; other NSS identities and your connection files in `/etc/ipsec.d` stay.
It lists what it leaves in place on Oracle Linux: the repository changes and the SIG key, `OVS_USER_ID` and `OVN_USER_ID`, the OVS and OVN logrotate edits, the chrony include, `/etc/modules-load.d/spinifex.conf`, the SELinux mode, a masked `ipsec.service`, firewalld disabled, and any GPU passthrough configuration.
`--purge-deps` is refused on Oracle Linux, because dnf cannot tell the packages Spinifex installed from the host's own; remove them with `dnf remove` yourself.

## Troubleshooting

### Installer stops: SELinux is enforcing

Set SELinux permissive now and in `/etc/selinux/config`, as in [SELinux](#selinux), then run the installer again.
Fixing only one of the two is refused too.

### Installer stops: the running kernel is not UEK R8

Install `kernel-uek`, reboot into it, check `uname -r`, then run the installer again.

### Installer stops: kernel-uek-modules is no longer in the repositories

The running kernel is older than anything the repository still carries modules for.

```bash
sudo dnf update -y kernel-uek
sudo reboot
```

### Installer stops: a repository is unreachable

The message names the repository.
Check the host can reach `yum.oracle.com` and `mirror.stream.centos.org` over HTTPS, including through any proxy dnf uses, then run the installer again.

### DHCP on br-wan times out

Check STP is off on the bridge:

```bash
nmcli -g bridge.stp connection show br-wan
```

If it prints `yes`, run `sudo nmcli connection modify br-wan bridge.stp no` and `sudo nmcli connection up br-wan`.

### `sudo spx` finds no command

Check that `/usr/bin/spx` links to `/usr/local/bin/spx`; run the installer again if it does not, or call `sudo /usr/local/bin/spx` meanwhile.

### Cluster nodes form no IPsec tunnels

Check that both IPsec services run, and that libreswan holds the node's certificate and the cluster CA:

```bash
systemctl is-active ipsec.service openvswitch-ipsec.service
sudo certutil -L -d sql:/var/lib/ipsec/nss     # ovs_certkey_<node>, ovs_cert_cacert
sudo ipsec trafficstatus                       # one line per peer tunnel
```

If the services are inactive, check the daemon log with `journalctl -u spinifex-daemon` for the IPsec reconcile.
If the certificates are present but no tunnel is listed, check that UDP 500 and 4500 pass between the nodes in both directions.

### CA certificate not trusted

`sudo spx admin init` installs the CA automatically. To install it again by hand on Oracle Linux:

```bash
sudo cp /etc/spinifex/ca.pem /etc/pki/ca-trust/source/anchors/spinifex-ca.crt
sudo update-ca-trust
```
