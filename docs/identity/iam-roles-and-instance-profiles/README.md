---
title: "IAM Roles and Instance Profiles"
description: "Create IAM roles with trust policies, wrap them in instance profiles, and grant EC2 instances credentials without static keys."
category: "Identity"
tags:
  - iam
  - roles
  - instance profiles
  - trust policies
  - credentials
  - security
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "AWS IAM Roles Documentation"
    url: "https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles.html"
  - title: "AWS Instance Profiles Documentation"
    url: "https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_use_switch-role-ec2_instance-profiles.html"
---

# IAM Roles and Instance Profiles

> Create IAM roles with trust policies, wrap them in instance profiles, and grant EC2 instances credentials without static keys.

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [Roles](#roles)
- [Granting Permissions to a Role](#granting-permissions-to-a-role)
- [Instance Profiles](#instance-profiles)
- [Launching an Instance with a Role](#launching-an-instance-with-a-role)
- [Managing Profile Associations at Runtime](#managing-profile-associations-at-runtime)
- [Assuming a Role](#assuming-a-role)
- [Cleaning Up](#cleaning-up)
- [Command Reference](#command-reference)
- [Troubleshooting](#troubleshooting)

---

## Overview

A role is an IAM identity with permissions but no long-lived credentials. Instead of an access key pair, a role has a **trust policy** declaring who may assume it; whoever assumes the role receives short-lived, auto-rotating STS credentials.

An **instance profile** is the container that binds a role to an EC2 instance. An instance launched with a profile gets the role's credentials delivered through [IMDS](/docs/imds) — no static keys baked into the image, no `~/.aws/credentials` inside the guest.

The full arc is: create a role → attach permissions → wrap it in an instance profile → launch an instance with the profile → the guest picks up credentials automatically. Each instance profile holds exactly **one role** (matching AWS).

Like all IAM resources, roles and instance profiles are scoped to the account that creates them.

## Prerequisites

- A running Spinifex cluster with the AWS gateway started
- An account created via `spx admin account create` (see [Spinifex Admin CLI](/docs/spinifex-admin-cli))
- AWS CLI configured with the account's profile:

```bash
export AWS_PROFILE=spinifex-myteam
```

## Instructions

## Roles

### Create a Role

Every role needs a trust policy. For a role that EC2 instances will use, trust the `ec2.amazonaws.com` service principal:

```bash
cat > /tmp/ec2-trust.json << 'EOF'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Service": "ec2.amazonaws.com" },
      "Action": "sts:AssumeRole"
    }
  ]
}
EOF

aws iam create-role --role-name app-server \
  --assume-role-policy-document file:///tmp/ec2-trust.json \
  --description "Role for app servers"
```

```json
{
  "Role": {
    "Path": "/",
    "RoleName": "app-server",
    "RoleId": "AROA1A2B3C4D5E6F7890",
    "Arn": "arn:aws:iam::000000000001:role/app-server",
    "CreateDate": "2026-07-03T10:00:00Z",
    "AssumeRolePolicyDocument": {
      "Version": "2012-10-17",
      "Statement": [
        {
          "Effect": "Allow",
          "Principal": { "Service": "ec2.amazonaws.com" },
          "Action": "sts:AssumeRole"
        }
      ]
    },
    "Description": "Role for app servers",
    "MaxSessionDuration": 3600
  }
}
```

To let a user (rather than EC2) assume the role, trust an `AWS` principal instead:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "AWS": "arn:aws:iam::000000000001:user/admin" },
      "Action": "sts:AssumeRole"
    }
  ]
}
```

**Trust policy validation:** Spinifex rejects `NotPrincipal`, `NotAction`, empty `Principal` blocks, and empty-string `Action` elements at write time with `MalformedPolicyDocument`. `Condition` blocks are rejected except the `StringEquals` form used for web identity federation.

### Get, List, and Update Roles

```bash
aws iam get-role --role-name app-server
aws iam list-roles
aws iam list-roles --path-prefix /services/
```

Update the description or the maximum STS session duration (3600 seconds by default):

```bash
aws iam update-role --role-name app-server \
  --description "Updated" --max-session-duration 7200
```

Replace the trust policy on an existing role:

```bash
aws iam update-assume-role-policy --role-name app-server \
  --policy-document file:///tmp/ec2-trust.json
```

## Granting Permissions to a Role

A freshly created role can do nothing. Grant permissions the same two ways as users: attach managed policies, or embed inline policies.

### Attach a Managed Policy

```bash
aws iam attach-role-policy --role-name app-server \
  --policy-arn arn:aws:iam::000000000001:policy/S3ReadOnly

aws iam list-attached-role-policies --role-name app-server
aws iam detach-role-policy --role-name app-server \
  --policy-arn arn:aws:iam::000000000001:policy/S3ReadOnly
```

See [IAM Users and Policies](/docs/iam-users-and-policies) for creating managed policies and the policy document format.

### Inline Role Policies

Inline policies live inside the role and are deleted with it:

```bash
aws iam put-role-policy --role-name app-server \
  --policy-name ec2-describe \
  --policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["ec2:DescribeInstances"],"Resource":"*"}]}'

aws iam list-role-policies --role-name app-server
aws iam get-role-policy --role-name app-server --policy-name ec2-describe
aws iam delete-role-policy --role-name app-server --policy-name ec2-describe
```

## Instance Profiles

Create a profile and add the role to it:

```bash
aws iam create-instance-profile --instance-profile-name app-server-profile
aws iam add-role-to-instance-profile \
  --instance-profile-name app-server-profile --role-name app-server
```

Inspect it — the `Roles` array shows the bound role:

```bash
aws iam get-instance-profile --instance-profile-name app-server-profile
aws iam list-instance-profiles
aws iam list-instance-profiles-for-role --role-name app-server
```

A profile holds at most one role; adding a second returns `LimitExceeded`. To swap roles, remove the current one first:

```bash
aws iam remove-role-from-instance-profile \
  --instance-profile-name app-server-profile --role-name app-server
```

## Launching an Instance with a Role

Pass the profile by name (or ARN) at launch:

```bash
aws ec2 run-instances \
  --image-id ami-0dd52c90440ff4150 \
  --instance-type t3.micro \
  --subnet-id subnet-a0e5fc381376d82a1 \
  --iam-instance-profile Name=app-server-profile
```

The instance description shows the association:

```bash
aws ec2 describe-instances --instance-ids i-d2e09ff7de71b6341 \
  --query 'Reservations[0].Instances[0].IamInstanceProfile'
```

```json
{
  "Arn": "arn:aws:iam::000000000001:instance-profile/app-server-profile",
  "Id": "AIPA1A2B3C4D5E6F7890"
}
```

Inside the guest, the AWS CLI and SDKs pick up the role's credentials from IMDS with no configuration — see [IMDS](/docs/imds) for fetching them manually, rotation timing, and limits.

## Managing Profile Associations at Runtime

Profiles can be attached to, removed from, or swapped on a **running** instance — no relaunch needed.

List associations:

```bash
aws ec2 describe-iam-instance-profile-associations \
  --filters Name=instance-id,Values=i-d2e09ff7de71b6341
```

```json
{
  "IamInstanceProfileAssociations": [
    {
      "AssociationId": "iip-assoc-9d7a617c605e337cd",
      "InstanceId": "i-d2e09ff7de71b6341",
      "IamInstanceProfile": {
        "Arn": "arn:aws:iam::000000000001:instance-profile/app-server-profile"
      },
      "State": "associated"
    }
  ]
}
```

Attach a profile to an instance launched without one:

```bash
aws ec2 associate-iam-instance-profile \
  --instance-id i-d2e09ff7de71b6341 \
  --iam-instance-profile Name=app-server-profile
```

Swap to a different profile (returns a new association ID):

```bash
aws ec2 replace-iam-instance-profile-association \
  --association-id iip-assoc-9d7a617c605e337cd \
  --iam-instance-profile Name=other-profile
```

Remove the profile — the guest's `iam/` metadata subtree starts returning 404:

```bash
aws ec2 disassociate-iam-instance-profile \
  --association-id iip-assoc-9d7a617c605e337cd
```

## Assuming a Role

Users and services can assume a role directly with STS, provided the trust policy allows their principal:

```bash
aws sts assume-role \
  --role-arn arn:aws:iam::000000000001:role/deploy \
  --role-session-name release-42
```

```json
{
  "Credentials": {
    "AccessKeyId": "ASIA1A2B3C4D5E6F7890",
    "SecretAccessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
    "SessionToken": "IQoJb3JpZ2luX2VjE...",
    "Expiration": "2026-07-03T11:00:00Z"
  },
  "AssumedRoleUser": {
    "AssumedRoleId": "AROA1A2B3C4D5E6F7890:release-42",
    "Arn": "arn:aws:sts::000000000001:assumed-role/deploy/release-42"
  }
}
```

The returned `ASIA`-prefixed credentials are temporary, valid up to the role's `MaxSessionDuration`.

## Cleaning Up

Deletion order matters — a role must be empty and unreferenced before it can be deleted:

```bash
# 1. Remove the role from any instance profiles
aws iam remove-role-from-instance-profile \
  --instance-profile-name app-server-profile --role-name app-server
aws iam delete-instance-profile --instance-profile-name app-server-profile

# 2. Delete inline policies and detach managed policies
aws iam delete-role-policy --role-name app-server --policy-name ec2-describe
aws iam detach-role-policy --role-name app-server \
  --policy-arn arn:aws:iam::000000000001:policy/S3ReadOnly

# 3. Delete the role
aws iam delete-role --role-name app-server
```

## Command Reference

| Command                                     | Key Parameters                                                                | Notes                                |
| ------------------------------------------- | ----------------------------------------------------------------------------- | ------------------------------------ |
| `create-role`                               | `--role-name`, `--assume-role-policy-document`, `--path`, `--description`, `--max-session-duration`, `--tags` | Trust policy required                |
| `get-role`                                  | `--role-name`                                                                  |                                      |
| `list-roles`                                | `--path-prefix`                                                                |                                      |
| `update-role`                               | `--role-name`, `--description`, `--max-session-duration`                      |                                      |
| `update-assume-role-policy`                 | `--role-name`, `--policy-document`                                            | Replaces the trust policy            |
| `delete-role`                               | `--role-name`                                                                  | Must have no policies or profiles    |
| `attach-role-policy` / `detach-role-policy` | `--role-name`, `--policy-arn`                                                 |                                      |
| `list-attached-role-policies`               | `--role-name`                                                                  |                                      |
| `put-role-policy`                           | `--role-name`, `--policy-name`, `--policy-document`                           | Inline policy                        |
| `get-role-policy` / `delete-role-policy`    | `--role-name`, `--policy-name`                                                |                                      |
| `list-role-policies`                        | `--role-name`                                                                  | Inline policy names                  |
| `tag-role` / `untag-role` / `list-role-tags` | `--role-name`, `--tags`/`--tag-keys`                                          |                                      |
| `create-instance-profile`                   | `--instance-profile-name`, `--path`, `--tags`                                 |                                      |
| `get-instance-profile`                      | `--instance-profile-name`                                                      | Includes bound role                  |
| `list-instance-profiles`                    | `--path-prefix`                                                                |                                      |
| `list-instance-profiles-for-role`           | `--role-name`                                                                  |                                      |
| `add-role-to-instance-profile`              | `--instance-profile-name`, `--role-name`                                      | Max 1 role per profile               |
| `remove-role-from-instance-profile`         | `--instance-profile-name`, `--role-name`                                      |                                      |
| `delete-instance-profile`                   | `--instance-profile-name`                                                      | Must contain no role                 |
| `associate-iam-instance-profile`            | `--instance-id`, `--iam-instance-profile`                                     | Works on running instances           |
| `disassociate-iam-instance-profile`         | `--association-id`                                                             |                                      |
| `replace-iam-instance-profile-association`  | `--association-id`, `--iam-instance-profile`                                  | Returns a new association ID         |
| `describe-iam-instance-profile-associations`| `--association-ids`, `--filters` (instance-id, state)                         |                                      |

## Troubleshooting

### MalformedPolicyDocument When Creating a Role

The trust policy is invalid. In addition to the JSON checks that apply to all policies, trust policies must not use `NotPrincipal`, `NotAction`, an empty `Principal` block, or an empty-string `Action`. `Condition` blocks are rejected except `StringEquals` conditions used with web identity federation.

### DeleteConflict When Deleting a Role

The role still has attached managed policies, inline policies, or is bound to an instance profile. Remove all three first:

```bash
aws iam list-attached-role-policies --role-name app-server
aws iam list-role-policies --role-name app-server
aws iam list-instance-profiles-for-role --role-name app-server
```

### DeleteConflict When Deleting an Instance Profile

The profile still contains a role:

```bash
aws iam remove-role-from-instance-profile \
  --instance-profile-name app-server-profile --role-name app-server
aws iam delete-instance-profile --instance-profile-name app-server-profile
```

### LimitExceeded When Adding a Role to a Profile

The profile already holds a role — the limit is one per profile. Remove the existing role first, or create a second profile.

### InvalidIamInstanceProfile.NotFound at Launch

The profile name or ARN passed to `run-instances` does not exist in your account. Check the spelling:

```bash
aws iam list-instance-profiles
```

### Instance Has No Credentials in IMDS

If the `iam/` metadata subtree returns 404, the instance has no associated profile. Attach one without relaunching:

```bash
aws ec2 associate-iam-instance-profile \
  --instance-id i-d2e09ff7de71b6341 \
  --iam-instance-profile Name=app-server-profile
```

If the profile is associated but contains no role, the `security-credentials/` listing is empty; add the role and credentials appear on the next request.
