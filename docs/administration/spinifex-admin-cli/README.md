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

Create a new isolated account. This provisions a sequential 12-digit account ID, an `admin` user with an `AdministratorAccess` policy attached, and an access key pair. The credentials are written to `~/.aws/credentials` and `~/.aws/config` under a `spinifex-<name>` profile automatically. Requires a running cluster; run from a cluster node:

```bash
spx admin account create --name myteam
```

```
Account created successfully!
  Account ID:        000000000002
  Account Name:      myteam
  Admin User:        admin
  Access Key ID:     AKIA1A2B3C4D5E6F7890ABCD
  Secret Access Key: wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
  AWS Profile:       spinifex-myteam

Use with:
  AWS_PROFILE=spinifex-myteam aws ec2 describe-instances
```

> **The secret access key is only shown once.** It is saved to `~/.aws/credentials` on the node where you ran the command; copy it from there if you need it elsewhere.

Set the profile to start using the account:

```bash
export AWS_PROFILE=spinifex-myteam
```

To create additional users and scoped permissions within the account, see [IAM Users and Policies](/docs/iam-users-and-policies).

List all accounts:

```bash
spx admin account list
```

```
ACCOUNT ID     NAME                 STATUS     CREATED
----------     ----                 ------     -------
000000000000   system               ACTIVE     2026-07-03 02:17
000000000001   spinifex             ACTIVE     2026-07-03 02:17
000000000002   myteam               ACTIVE     2026-07-03 03:32
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
