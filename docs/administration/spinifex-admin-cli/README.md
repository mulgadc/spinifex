---
title: "Spinifex Admin CLI"
description: "Complete reference for the Spinifex administration CLI. Manage accounts, nodes, VMs, and services."
category: "Administration"
tags:
  - cli
  - admin
  - reference
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
---

# Spinifex Admin CLI

> Complete reference for the Spinifex administration CLI.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

The `spx` binary is the central administration tool for managing your Spinifex infrastructure. It provides commands for cluster initialization, account management, node operations, VM lifecycle, and service control.

**Binary location:** `/usr/local/bin/spx` (binary install) or `~/Development/mulga/spinifex/bin/spx` (source install)

All services in the Spinifex platform are managed through this single binary.

## Instructions

## Account Management

Create a new isolated account with an admin user and AdministratorAccess policy:

```bash
spx admin account create --name myteam
export AWS_PROFILE=spinifex-myteam
```

List all accounts:

```bash
spx admin account list
```



## Node Management

List nodes in the cluster:

```bash
spx get nodes
```

```
NAME    STATUS    IP              REGION           AZ               UPTIME   VMs
node1   Ready     127.0.0.1       ap-southeast-2   ap-southeast-2a  2m       0
node2   Ready     127.0.0.2       ap-southeast-2   ap-southeast-2a  2m       0
node3   Ready     127.0.0.3       ap-southeast-2   ap-southeast-2a  2m       0
```

## Monitor Resources

```bash
spx top nodes
```

Prints per-node CPU/memory usage and cluster-wide instance type availability.

## Image Management

```bash
spx admin images list
spx admin images import --name debian-13-arm64
```

Catalog imports verify the image against the catalog-declared SHA-256/SHA-512 digest before extraction. Use `--file` to import operator-supplied media (verification skipped — operator is responsible for integrity), or `--force` to re-download after a checksum mismatch.

### EKS node image

To run EKS, import the prebuilt node image from the catalog:

```bash
spx admin images import --name spinifex-eks-node
```

This pulls the Alpine + K3s node AMI from `iso.mulgadc.com`, verifies its
checksum, and registers it tagged `spinifex:managed-by=eks`. `eks create-cluster`
and `eks create-nodegroup` resolve the boot AMI by that tag, so no further
configuration is needed.

Operators publishing a freshly built node image to the mirror run the
**Publish EKS node AMI** GitHub Actions workflow (manual dispatch; uses the
`CF_R2_*` repo secrets) or, locally, `make publish-eks-node-image` (see
`scripts/publish-system-image.sh`; requires `R2_ENDPOINT` + R2-scoped
`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`).


## Cluster Shutdown

Coordinated, phased shutdown of the entire cluster (API/UI → VMs → viperblock → predastore → NATS/daemon):

```bash
spx admin cluster shutdown
```

## Troubleshooting

### Permission Denied Running Spinifex

The binary may not be executable. Fix permissions:

```bash
chmod +x /usr/local/bin/spx
```

If you get permission errors during operations, ensure you're running with appropriate privileges. Some OVN and networking commands require `sudo`.

### Services Fail to Start

Check the daemon logs for specific errors via `systemctl`/`journalctl`:

```bash
systemctl status 'spinifex-*'
journalctl -u spinifex-daemon -f
journalctl -u 'spinifex-*' -f
```

Common causes include port conflicts, missing OVN configuration, or untrusted CA certificates.
