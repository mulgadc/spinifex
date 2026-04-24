# Installing Spinifex in a VM (MacOS)

For evaluation / quick POC use only.

Spinifex is designed for bare-metal hardware, edge nodes and data-centre use. However, follow this guide to install Spinifex on a MacOS VM to demonstrate the capabilities (dev/testing only).

Note this tutorial is for Apple Silicon (ARM64)

## UTM installation

For this tutorial download UTM.app on Mac to simplify the VM installation.

* UTM - [https://mac.getutm.app](https://mac.getutm.app)



## Download Debian ISO

Next, download the Debian ISO (ARM64, Apple Silicon)

* Debian 13.4 - [https://cdimage.debian.org/debian-cd/current/arm64/iso-cd/debian-13.4.0-arm64-netinst.iso](https://cdimage.debian.org/debian-cd/current/arm64/iso-cd/debian-13.4.0-arm64-netinst.iso)

## Login

Using the VM console, login using the `spinifex` user and your specified password. A few additional steps are required via the console, before you can SSH into the machine for easier administration.

## Install prerequisites

For a minimal Debian install additional dependencies are required prior to the Spinifex installation:

```
su root
apt update
apt install sudo curl bridge-utils
```

### Enable sudoers

Enable sudo for `spinifex` user as the root user:

```
su root
echo "spinifex ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/spinifex
```

## Networking

Spinifex requires a Linux bridge for networking to correctly function, given EC2 instances and resources can obtain an IP address from the upstream DHCP server or a static public IP address range you specify.

This is a requirement step for valid installation.

View the existing `/etc/network/interfaces` for the network configuration.

```sh
# This file describes the network interfaces available on your system
# and how to activate them. For more information, see interfaces(5).

source /etc/network/interfaces.d/*

# The loopback network interface
auto lo
iface lo inet loopback

# The primary network interface
allow-hotplug enp0s1
iface enp0s1 inet dhcp
# This is an autoconfigured IPv6 interface
iface enp0s1 inet6 auto
```

Edit `/etc/network/interfaces` note replace `enp0s1` with your network adapter.

```sh
# This file describes the network interfaces available on your system
# and how to activate them. For more information, see interfaces(5).

source /etc/network/interfaces.d/*

# The loopback network interface
auto lo
iface lo inet loopback

# The primary network interface - toggle to manual
allow-hotplug enp0s1
iface enp0s1 inet manual
# This is an autoconfigured IPv6 interface - toggle to manual
iface enp0s1 inet6 manual

# Spinifex Config
# WAN bridge (Spinifex EC2 instances connect here)
# VMs (EC2) / containers <-> br-wan <-> enp0s1 <-> external network
auto br-wan
iface br-wan inet dhcp
    bridge_ports enp0s1
    bridge_stp off
    bridge_fd 0
    bridge_maxwait 0
```

Walkthrough of changes:

* Adds eno3 into the bridge
* eno3 becomes a slave interface
* All traffic goes through br-wan
* Spinifex EC2 (VMs) in a public subnet, obtain an IP address from the WAN upstream

### Restart networking

Make sure you are logged into the console, not via SSH for these steps. Otherwise you will lose networking to the instance.

Replace `enp0s1` with your network adapter.

```bash
ifdown enp0s1
systemctl restart networking
```

Next, retrieve the IP address of the new `br-wan` bridge from your upstream DHCP server.

```bash
ip a show dev enp0s1
```

```bash
6: br-wan: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP group default qlen 1000
    link/ether 12:c2:bc:f3:f6:7b brd ff:ff:ff:ff:ff:ff
    inet 192.168.0.249/24 brd 192.168.0.255 scope global dynamic noprefixroute br-wan
       valid_lft 86103sec preferred_lft 75303sec
    inet6 fd0d:9d60:d09b:0:ee77:630d:3237:9a10/64 scope global dynamic mngtmpaddr noprefixroute
       valid_lft 86282sec preferred_lft 86282sec
    inet6 fe80::85c0:f2aa:f0bf:e951/64 scope link
       valid_lft forever preferred_lft forever
```

Note the IP `192.168.0.249`

# Spinifex Installation

## Launch installer

Next, optionally SSH into your instance (using the IP above) or continue using the console.

Replace with your IP below.

```
ssh spinifex@<your-ip>
```

As the `spinifex` user launch the binary installer:

```bash
su spinifex
curl -fsSL https://install.mulgadc.com | bash
```

```bash
[INFO] Spinifex installer

[INFO] Detected OS: Debian GNU/Linux 13 (trixie)
[INFO] Detected architecture: aarch64 (arm64)
[INFO] Installing system dependencies...
[INFO] System dependencies installed
[INFO] Installing AWS CLI v2...
[INFO] AWS CLI installed: aws-cli/2.34.30 Python/3.14.4 Linux/6.12.74+deb13+1-arm64 exe/aarch64.debian.13
[INFO] Service users created (spinifex-{nats,gw,daemon,storage,viperblock,vpcd,ui})
/etc/sudoers.d/spinifex-network: parsed OK
[INFO] Scoped sudoers rules installed for spinifex-daemon and spinifex-vpcd
[INFO] Downloading Spinifex (latest channel, arm64)...
[INFO] Verifying checksum...
[INFO] Checksum verified
[INFO] Extracting...
[INFO] Installing files...
[INFO]   /usr/local/bin/spx
[INFO]   /usr/lib/aarch64-linux-gnu/nbdkit/plugins/nbdkit-viperblock-plugin.so
[INFO]   /usr/local/share/spinifex/setup-ovn.sh
[INFO] Creating directories...
[INFO] Generated /etc/spinifex/systemd.env
[INFO]   /var/lib/spinifex/predastore-start.sh
[INFO]   /var/lib/spinifex/wait-for-nats.sh
[INFO] Fixing file ownership for privilege separation...
[INFO] File ownership updated
[INFO] Installing systemd units...
[INFO]   /etc/systemd/system/spinifex-awsgw.service
[INFO]   /etc/systemd/system/spinifex-daemon.service
[INFO]   /etc/systemd/system/spinifex-nats.service
[INFO]   /etc/systemd/system/spinifex-predastore.service
[INFO]   /etc/systemd/system/spinifex.target
[INFO]   /etc/systemd/system/spinifex-ui.service
[INFO]   /etc/systemd/system/spinifex-viperblock.service
[INFO]   /etc/systemd/system/spinifex-vpcd.service
Created symlink '/etc/systemd/system/multi-user.target.wants/spinifex.target' → '/etc/systemd/system/spinifex.target'.
[INFO] Systemd units installed and enabled (per-service users)
[INFO] Fresh install detected, skipping migrations
[INFO] Logrotate config installed

============================================
  Spinifex installed successfully
============================================

  Version:      spinifex v1.1.0 (b7b6390) linux/arm64
  Architecture: arm64
  Service users: spinifex-{nats,gw,daemon,storage,viperblock,vpcd,ui}
  Binary:       /usr/local/bin/spx
  Config:       /etc/spinifex/
  Data:         /var/lib/spinifex/
  Logs:         /var/log/spinifex/
```

## Setup OVN networking

Since the WAN interface is already a bridge (e.g. br-wan) run the Openvswitch (OVN) install script:

```bash
sudo /usr/local/share/spinifex/setup-ovn.sh --management
```

Output:

```bash
Auto-detected Linux bridge: br-wan (default route)
  Will create OVS bridge br-ext + veth pair to link them
Auto-detected encap IP: 192.168.0.249
Auto-detected chassis ID: chassis-spinifex
=== Spinifex OVN Compute Node Setup ===
  Management node:  true
  WAN bridge:       br-ext (veth)
  Linux bridge:     br-wan (linked via veth pair)
  OVN Remote (SB):  tcp:127.0.0.1:6642
  Encap IP:         192.168.0.249
  Chassis ID:       chassis-spinifex
...
```

## Spinifex Initialize

```bash
sudo spx admin init --node node1 --nodes 1
```

## Start services

```bash
sudo systemctl start spinifex.target
```

## Verify installation

Connect to the local AWS gateway and issue a sample command using the AWS CLI tool, all running on your local infrastructure, no cloud required.

```bash
export AWS_PROFILE=spinifex
aws ec2 describe-instance-types
```

> ***Note:*** The system may require ~30 seconds to initalise prior to accepting connections for the command to complete above.

Output:

```json
{
    "InstanceTypes": [
        {
            "InstanceType": "t4g.nano",
            "CurrentGeneration": true,
            "SupportedRootDeviceTypes": [
                "ebs"
            ],
            "SupportedVirtualizationTypes": [
                "hvm"
            ],
            "Hypervisor": "kvm",
            "ProcessorInfo": {
                "SupportedArchitectures": [
                    "arm64"
                ]
            },
            "VCpuInfo": {
                "DefaultVCpus": 2
            },
            "MemoryInfo": {
                "SizeInMiB": 512
            },
            "PlacementGroupInfo": {
                "SupportedStrategies": [
                    "cluster",
                    "spread"
                ]
            },
            "BurstablePerformanceSupported": true
        },
    ]
}
```

# Installation Complete

Congratulations! 🥳

Your installation of Spinifex is now complete on a VM running on a Mac.

# Next steps
