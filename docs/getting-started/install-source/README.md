---
title: "Source Install"
description: "Build Spinifex from source for development, custom builds, or contributing."
category: "Getting Started"
tags:
  - install
  - source
  - development
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "Go Downloads"
    url: "https://go.dev/dl/"
---

# Source Installation

> Build Spinifex from source for development, custom builds, or contributing.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

This guide builds Spinifex from source. For production deployments, the [binary installer](/docs/install) is recommended.

**Requirements:**

- Ubuntu 26.04 / Debian 13
- Go 1.26.3+
- GCC, make, pkg-config
- QEMU/KVM, OVN/Open vSwitch, AWS CLI v2, dhcpcd-base

## Instructions

## Step 1. Install Dependencies

```bash
mkdir -p ~/Development/mulga && cd ~/Development/mulga
git clone https://github.com/mulgadc/spinifex.git
sudo make -C spinifex quickinstall
export PATH=$PATH:/usr/local/go/bin
```

## Step 2. Clone and Build

```bash
cd spinifex
./scripts/clone-deps.sh
```

## Step 3. Development Initialisation

```bash
./scripts/dev-install.sh
```

This bootstraps a single-node development environment: builds binaries, configures OVN, initialises the cluster, installs the CA certificate, and starts all services.

## Step 4. Verify Installation

```bash
export AWS_PROFILE=spinifex
aws ec2 describe-instance-types
```

If this returns a list of available instance types, your installation is working.

**Congratulations! Spinifex is installed from source.**

Continue to [Setting Up Your Cluster](/docs/setting-up-your-cluster) to import an AMI, create a VPC, and launch your first instance.

## Troubleshooting

### Go Not Found in PATH

```bash
export PATH=$PATH:/usr/local/go/bin
```

### CA Certificate Not Trusted

```bash
sudo cp /etc/spinifex/ca.pem /usr/local/share/ca-certificates/spinifex-ca.crt
sudo update-ca-certificates
```
