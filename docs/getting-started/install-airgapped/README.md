---
title: "Air-Gapped Install"
description: "Deploy Spinifex in environments without internet connectivity. Covers using a pre-built release tarball, mirrored APT packages, and locally-staged cloud images."
category: "Getting Started"
tags:
  - install
  - air-gapped
  - offline
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "External Connection Inventory"
    url: "/docs/security/network-connections"
---

# Air-Gapped Installation

> Deploy Spinifex in environments without internet connectivity.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

In air-gapped or disconnected environments, Spinifex can be deployed without internet access. This guide covers preparing offline packages on a connected machine, creating USB deployment media, and installing on the target server with package verification.

## Instructions

## Step 1. Download the Release (on a connected machine)

Each Spinifex release publishes a self-contained tarball, the matching `setup.sh`, and a SHA-256 checksum to GitHub Releases. Resolve the latest tag and download the assets for your architecture:

```bash
ARCH=amd64   # or arm64
TAG=$(curl -fsSL https://api.github.com/repos/mulgadc/spinifex/releases/latest \
  | grep '"tag_name"' | cut -d'"' -f4)

BASE="https://github.com/mulgadc/spinifex/releases/download/${TAG}"
curl -fsSLO "${BASE}/spinifex-${TAG}-linux-${ARCH}.tar.gz"
curl -fsSLO "${BASE}/spinifex-${TAG}-linux-${ARCH}.tar.gz.sha256"
curl -fsSLO "${BASE}/setup.sh"

sha256sum -c "spinifex-${TAG}-linux-${ARCH}.tar.gz.sha256"
```

## Step 2. Stage Dependencies (on the connected machine)

Pre-download the APT packages installed by the production setup. `apt install --download-only` writes `.deb` files to `/var/cache/apt/archives/` without installing them, so the connected machine isn't modified:

```bash
sudo apt update
sudo apt install --download-only -y \
  nbdkit nbdkit-plugin-dev pkg-config \
  qemu-system-x86 qemu-utils qemu-kvm \
  ovmf qemu-efi-aarch64 \
  libvirt-daemon-system libvirt-clients libvirt-dev \
  ovn-central ovn-host openvswitch-switch \
  dhcpcd-base make gcc jq curl iproute2 netcat-openbsd \
  wget unzip xz-utils file
```

Download AWS CLI v2:

```bash
curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" \
  -o awscliv2.zip
```

Mirror the cloud image you intend to run as guest VMs:

```bash
mkdir -p images
curl -fsSL "https://cloud.debian.org/images/cloud/trixie/20260518-2482/debian-13-genericcloud-amd64-20260518-2482.qcow2" \
  -o images/debian-13-amd64.qcow2
```

## Step 3. Assemble Transfer Media

```bash
mkdir -p /media/spinifex-deploy/{tarball,apt-packages,aws,images}
cp spinifex-${TAG}-linux-${ARCH}.tar.gz /media/spinifex-deploy/tarball/
cp setup.sh /media/spinifex-deploy/
cp /var/cache/apt/archives/*.deb /media/spinifex-deploy/apt-packages/
cp awscliv2.zip /media/spinifex-deploy/aws/
cp images/*.qcow2 /media/spinifex-deploy/images/
```

## Step 4. Install on the Air-Gapped Target

Mount the media, install APT dependencies, install AWS CLI, then run `setup.sh` with the local tarball:

```bash
sudo mount /dev/sdb1 /mnt/usb

sudo dpkg -i /mnt/usb/apt-packages/*.deb
sudo apt-get install -f --no-download   # resolve any leftover deps from local cache

cd /tmp && unzip /mnt/usb/aws/awscliv2.zip && sudo ./aws/install

INSTALL_SPINIFEX_TARBALL=/mnt/usb/tarball/spinifex-*-linux-*.tar.gz \
INSTALL_SPINIFEX_SKIP_APT=1 \
INSTALL_SPINIFEX_SKIP_AWS=1 \
bash /mnt/usb/setup.sh
```

Run `setup.sh` without `sudo` — the script handles privilege escalation internally and ends by `exec`-ing into a `newgrp spinifex` subshell so your current shell picks up `spinifex` group membership. Without that membership, AWS CLI cannot traverse `/etc/spinifex/` (mode `0750`) to read `ca.pem` and Step 9 will fail with a TLS error. Type `exit` to leave the subshell when finished.

## Step 5. Setup OVN Networking

`spx admin init` requires OVN/OVS to be configured before the daemon can manage tenant networks. `setup-ovn.sh` ships in the tarball and runs purely against local commands (no network access required):

```bash
sudo /usr/local/share/spinifex/setup-ovn.sh --management
```

If your WAN interface is a physical NIC rather than a bridge, pass `--wan-bridge=br-wan --wan-iface=<nic>`.

## Step 6. Initialize Without Telemetry

```bash
sudo spx admin init --node node1 --nodes 1 --no-telemetry
```

This generates the cluster configuration, TLS certificates, installs the CA into the system trust store, and writes AWS CLI credentials. See [Single-Node Install](/docs/install) for what `spx admin init` does.

## Step 7. Start Services

`setup.sh` enables `spinifex.target` but does not start it on a fresh install. Start it now so predastore is online before you import images:

```bash
sudo systemctl start spinifex.target
```

## Step 8. Import Cloud Images

Register the pre-staged image with Spinifex:

```bash
sudo spx admin images import --file /mnt/usb/images/debian-13-amd64.qcow2 \
  --distro debian --version 13 --arch x86_64
```

## Step 9. Verify

```bash
export AWS_PROFILE=spinifex
aws ec2 describe-instance-types
aws ec2 describe-images
```

If both calls return data, your air-gapped install is working. Continue to [Setting Up Your Cluster](/docs/setting-up-your-cluster) to launch your first instance.

## Troubleshooting

### Missing APT Dependencies

`dpkg -i` does not resolve transitive dependencies. After installing, run:

```bash
sudo apt-get install -f --no-download
```

The `--no-download` flag forces apt to use only what's in the local cache. If a dependency is genuinely missing, add it to the `apt --download-only` step on the connected machine and re-stage.

### Setup.sh Tries to Download Anyway

Confirm both skip flags are exported and that `INSTALL_SPINIFEX_TARBALL` points at a readable file:

```bash
sudo INSTALL_SPINIFEX_TARBALL=/mnt/usb/tarball/spinifex-...tar.gz \
     INSTALL_SPINIFEX_SKIP_APT=1 \
     INSTALL_SPINIFEX_SKIP_AWS=1 \
     bash -x /mnt/usb/setup.sh
```

### Init Telemetry Attempted

`spx admin init` posts a one-shot record to `https://install.mulgadc.com/install`. Pass `--no-telemetry` on every `init` and `join` invocation, or export `SPX_NO_TELEMETRY=1` in the operator's shell profile.

### Image Import Fails

`spx admin images import --file` requires the distro/version/arch flags so the image registers in the catalogue. If the import succeeds but `aws ec2 describe-images` returns empty, check `journalctl -u spinifex-daemon -f` for predastore upload errors.
