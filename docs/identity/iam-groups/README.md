---
title: "IAM Groups"
description: "Organise IAM users into groups and manage permissions once for the whole team."
category: "Identity"
tags:
  - iam
  - groups
  - users
  - policies
  - security
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "AWS IAM Groups Documentation"
    url: "https://docs.aws.amazon.com/IAM/latest/UserGuide/id_groups.html"
---

# IAM Groups

> Organise IAM users into groups and manage permissions once for the whole team.

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [Groups](#groups)
- [Membership](#membership)
- [Group Policies](#group-policies)
- [Putting It All Together](#putting-it-all-together)
- [Troubleshooting](#troubleshooting)

---

## Overview

A group is a collection of IAM users. Policies attached to a group apply to every member, so instead of attaching the same policy to each developer individually, you attach it once to a `developers` group and manage membership.

Members inherit both the group's **attached managed policies** and its **inline policies**, combined with any policies on the user itself. Policy evaluation is the same as everywhere else: an explicit `Deny` in any applicable policy wins, and anything not allowed is denied.

Groups cannot be nested, and a group is not a principal — it cannot sign requests or own access keys. Like all IAM resources, groups are scoped to the account that creates them.

## Prerequisites

- A running Spinifex cluster with the AWS gateway started
- AWS CLI configured with your account's profile. The default `spinifex` account created at cluster setup works out of the box; to work in a separate account, create one with `spx admin account create` (see [Spinifex Admin CLI](/docs/spinifex-admin-cli)):

```bash
export AWS_PROFILE=spinifex
```

## Instructions

## Groups

### Create a Group

```bash
aws iam create-group --group-name developers
```

```json
{
  "Group": {
    "Path": "/",
    "GroupName": "developers",
    "GroupId": "AGPA1A2B3C4D5E6F7890",
    "Arn": "arn:aws:iam::000000000001:group/developers",
    "CreateDate": "2026-07-03T10:00:00Z"
  }
}
```

Use `--path` to organise groups hierarchically:

```bash
aws iam create-group --group-name ops --path /teams/
```

### Get and List Groups

`get-group` returns the group and its members:

```bash
aws iam get-group --group-name developers
aws iam list-groups
aws iam list-groups --path-prefix /teams/
```

### Delete a Group

A group must have no members and no policies before it can be deleted — see [Cleaning up a group](#deleteconflict-when-deleting-a-group).

```bash
aws iam delete-group --group-name developers
```

## Membership

Add and remove users:

```bash
aws iam add-user-to-group --group-name developers --user-name carol
aws iam remove-user-from-group --group-name developers --user-name carol
```

List a group's members (the `Users` array of `get-group`):

```bash
aws iam get-group --group-name developers --query 'Users[].UserName'
```

List the groups a user belongs to:

```bash
aws iam list-groups-for-user --user-name carol --query 'Groups[].GroupName'
```

> **Group membership blocks user deletion.** `delete-user` returns `DeleteConflict` while the user is still in any group — remove them from all groups first.

## Group Policies

### Attach a Managed Policy

```bash
aws iam attach-group-policy --group-name developers \
  --policy-arn arn:aws:iam::000000000001:policy/EC2ReadOnly

aws iam list-attached-group-policies --group-name developers

aws iam detach-group-policy --group-name developers \
  --policy-arn arn:aws:iam::000000000001:policy/EC2ReadOnly
```

See [IAM Users and Policies](/docs/iam-users-and-policies) for creating managed policies and the policy document format.

### Inline Group Policies

Inline policies live inside the group and are deleted with it:

```bash
aws iam put-group-policy --group-name developers \
  --policy-name keypair-mgmt \
  --policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["ec2:CreateKeyPair","ec2:DeleteKeyPair"],"Resource":"*"}]}'

aws iam list-group-policies --group-name developers
aws iam get-group-policy --group-name developers --policy-name keypair-mgmt
aws iam delete-group-policy --group-name developers --policy-name keypair-mgmt
```

Members are authorised by the union of the group's attached and inline policies. A user in `developers` with the policies above can call `ec2:DescribeInstances` (via the attached `EC2ReadOnly`) and `ec2:CreateKeyPair` (via the inline policy), while anything not granted — say `ec2:RunInstances` — is denied.

## Putting It All Together

Onboard a team with shared permissions:

```bash
# 1. Create the group and grant it permissions
aws iam create-group --group-name developers
aws iam attach-group-policy --group-name developers \
  --policy-arn arn:aws:iam::000000000001:policy/EC2ReadOnly

# 2. Create users and add them to the group
aws iam create-user --user-name carol
aws iam add-user-to-group --group-name developers --user-name carol

# 3. Create access keys and configure profiles as usual
aws iam create-access-key --user-name carol

# 4. Verify: carol can describe instances via group policy, nothing more
AWS_PROFILE=spinifex-carol aws ec2 describe-instances
AWS_PROFILE=spinifex-carol aws ec2 run-instances --image-id ami-123 --instance-type t3.micro
# ^ AccessDenied — not granted by the group
```

To change the whole team's permissions later, edit the group's policies once — no per-user changes needed.

## Troubleshooting

### DeleteConflict When Deleting a Group

The group still has members, attached policies, or inline policies. Empty it first:

```bash
# Members
aws iam get-group --group-name developers --query 'Users[].UserName'
aws iam remove-user-from-group --group-name developers --user-name carol

# Attached policies
aws iam list-attached-group-policies --group-name developers
aws iam detach-group-policy --group-name developers --policy-arn arn:aws:iam::000000000001:policy/EC2ReadOnly

# Inline policies
aws iam list-group-policies --group-name developers
aws iam delete-group-policy --group-name developers --policy-name keypair-mgmt

aws iam delete-group --group-name developers
```

### DeleteConflict When Deleting a User

Group membership counts as a subordinate entity, alongside access keys and attached policies. Find and leave the user's groups:

```bash
aws iam list-groups-for-user --user-name carol
aws iam remove-user-from-group --group-name developers --user-name carol
```

### NoSuchEntity

The group or user referenced does not exist — both `get-group` on a missing group and `add-user-to-group` with a missing user return this. Check spelling with `list-groups` / `list-users`.

### Member Still Denied After Attaching a Group Policy

Check the policy landed on the right group and the user is actually a member:

```bash
aws iam list-groups-for-user --user-name carol
aws iam list-attached-group-policies --group-name developers
aws iam list-group-policies --group-name developers
```

Remember an explicit `Deny` in any policy that applies to the user — their own or any of their groups' — overrides the `Allow`.
