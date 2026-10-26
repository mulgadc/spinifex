---
title: "Air-Gapped Install"
seoTitle: "Install Spinifex in an Air-Gapped Network — Spinifex Docs"
description: "Deploy Spinifex without internet access, using a pre-built release tarball, APT or RPM packages staged on a connected machine, and locally staged cloud images."
category: "Install"
tags:
  - install
  - air-gapped
  - offline
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "External Connection Inventory"
    url: "/docs/network-connections"
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

On an Oracle Linux target, stage RPMs instead of the APT packages below, as in [Oracle Linux Hosts](#oracle-linux-hosts); the AWS CLI and cloud image steps are the same.

Pre-download the APT packages installed by the production setup. `apt install --download-only` writes `.deb` files to `/var/cache/apt/archives/` without installing them, so the connected machine isn't modified:

```bash
sudo apt update
sudo apt install --download-only -y \
  nbdkit nbdkit-plugin-dev pkg-config \
  qemu-system-x86 qemu-utils \
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

Mirror the cloud image you intend to run as guest VMs, together with the sums file it was published with. Keep the upstream filename, because the import looks the image up in the sums file by name:

```bash
mkdir -p images
cd images
curl -fsSLO "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2"
curl -fsSLO "https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS"
sha512sum -c --ignore-missing SHA512SUMS
cd ..
```

The sums file only proves the image matches it; a sums file fetched over the same channel as the image can be tampered with alongside it. Where the vendor publishes a signature for the sums file (for example Ubuntu's `SHA256SUMS.gpg`), download it too and check it with `gpg --verify` against the vendor's signing key on this connected machine.

## Step 3. Assemble Transfer Media

```bash
mkdir -p /media/spinifex-deploy/{tarball,apt-packages,aws,images}
cp spinifex-${TAG}-linux-${ARCH}.tar.gz /media/spinifex-deploy/tarball/
cp setup.sh /media/spinifex-deploy/
cp /var/cache/apt/archives/*.deb /media/spinifex-deploy/apt-packages/
cp awscliv2.zip /media/spinifex-deploy/aws/
cp images/*.qcow2 images/SHA512SUMS /media/spinifex-deploy/images/
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

Register the pre-staged image with Spinifex, verifying it against the sums file carried across with it:

```bash
sudo spx admin images import --file /mnt/usb/images/debian-13-genericcloud-amd64.qcow2 \
  --checksum /mnt/usb/images/SHA512SUMS \
  --distro debian --version 13 --arch x86_64 --boot-mode uefi
```

A digest mismatch exits before anything is written to storage. On success the import prints the verified digest, which `spx admin images describe --image-id <ami>` shows again later.

## Step 9. Verify

```bash
export AWS_PROFILE=spinifex
aws ec2 describe-instance-types
aws ec2 describe-images
```

If both calls return data, your air-gapped install is working. Continue to [Setting Up Your Cluster](/docs/setting-up-your-cluster) to launch your first instance.

## Oracle Linux Hosts

This path has not been run end to end on an air-gapped Oracle Linux host yet.
It replaces the APT parts of Steps 2 to 4; every other step is the same.

With `INSTALL_SPINIFEX_SKIP_APT=1`, `setup.sh` installs no packages and sets up no repositories on Oracle Linux, so the RPMs must come from a connected Oracle Linux machine that already has them configured.
Use a connected Oracle Linux 10 host on the same minor release as the target, and run the online installer on it once, or `SETUP_STAGES=deps bash setup.sh`, which adds the UEK R8, KVM Utilities and NFV SIG repositories and the AppStream QEMU exclude.
Then download the package list from `OL_RUNTIME_PACKAGES` in `setup.sh` with all its dependencies, together with the modules for the kernel the target runs (`uname -r` on the target):

```bash
TARGET_KERNEL=6.12.0-...el10uek.x86_64   # uname -r on the target
mkdir -p rpms
sudo dnf download --resolve --alldeps --destdir rpms   nbdkit nbdkit-basic-filters   qemu-kvm qemu-img edk2-ovmf seabios-bin less pciutils   jq curl iproute ethtool nmap-ncat wget unzip xz file   ovn25.03-central ovn25.03-host openvswitch3.5 openvswitch3.5-ipsec libreswan dhcpcd   chrony nftables iptables-nft systemd-resolved   "kernel-uek-modules-${TARGET_KERNEL}"
```

Copy `rpms/` to the transfer media in place of `apt-packages/`.
On the target, disable the network repositories before installing, so that neither dnf nor the installer tries to reach them:

```bash
sudo dnf config-manager --set-disabled '*'
sudo dnf install -y --disablerepo='*' /mnt/usb/rpms/*.rpm
```

Then run `setup.sh` as in Step 4, with `INSTALL_SPINIFEX_SKIP_APT=1`.

The installer's DNS handover to systemd-resolved still runs.
It waits up to 10 seconds for `resolvectl dns` to list a DNS server, so the target's `br-wan` profile needs one, such as the site's internal resolver.
With every repository disabled, it warns that there is no repository host to look up and carries on.

Because the installer skipped the repository setup, the target has no NFV SIG repository and no AppStream QEMU exclude.
If the host later reaches a repository mirror, run `setup.sh` once without `INSTALL_SPINIFEX_SKIP_APT` before updating, so a later AppStream QEMU cannot replace the KVM Utilities one.
Every new `kernel-uek` brought across needs its `kernel-uek-modules` too; see [Oracle Linux Hosts](/docs/install-oracle-linux) for keeping the host updated.

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

### Oracle Linux: the package repository host no longer resolves

The installer checks that the first enabled repository's host still resolves after it hands DNS to systemd-resolved.
On an air-gapped host that host is unreachable by design: disable the network repositories with `sudo dnf config-manager --set-disabled '*'` and run `setup.sh` again.

### Init Telemetry Attempted

`spx admin init` posts a one-shot record to `https://install.mulgadc.com/install`. Pass `--no-telemetry` on every `init` and `join` invocation, or export `SPX_NO_TELEMETRY=1` in the operator's shell profile.

### Image Import Fails

`spx admin images import --file` requires the distro/version/arch/boot-mode flags so the image registers in the catalogue. A `checksum entry for image filename not found` error means the image was renamed after download; restore the upstream filename so it matches its line in the sums file. If the import succeeds but `aws ec2 describe-images` returns empty, check `journalctl -u spinifex-daemon -f` for predastore upload errors.
