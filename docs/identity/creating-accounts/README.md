---
title: "Creating Accounts"
description: "Create and manage isolated user accounts with their own resources."
category: "Identity"
tags:
  - accounts
  - iam
  - admin
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
---

# Creating Accounts

> Create and manage isolated user accounts with their own resources.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

Spinifex supports multi-tenant account isolation. Each account gets its own IAM credentials, AWS CLI profile, and isolated resource namespace.

Creating an account provisions a sequential 12-digit account ID, an `admin` user with an `AdministratorAccess` policy attached, and an access key pair. The credentials are written to `~/.aws/credentials` and `~/.aws/config` under a `spinifex-<name>` profile automatically.

## Instructions

## Create Account

Requires a running cluster. Run from a cluster node:

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
aws sts get-caller-identity
```

To create additional users and scoped permissions within the account, see [IAM Users and Policies](/docs/iam-users-and-policies).

## List Accounts

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

## Troubleshooting

## Credentials Not Working

Verify the AWS CLI configuration files exist and contain the correct profile:

```bash
cat ~/.aws/config
cat ~/.aws/credentials
```

Ensure the `AWS_PROFILE` environment variable matches the profile name:

```bash
echo $AWS_PROFILE
export AWS_PROFILE=spinifex-myteam
```
