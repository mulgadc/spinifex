---
title: "IAM Users and Policies"
description: "Create IAM users, manage access keys, and control permissions with policies."
category: "Identity"
tags:
  - iam
  - users
  - policies
  - access keys
  - security
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "AWS IAM CLI Reference"
    url: "https://docs.aws.amazon.com/cli/latest/reference/iam/"
  - title: "IAM Policy Reference"
    url: "https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_elements.html"
---

# IAM Users and Policies

> Create IAM users, manage access keys, and control permissions with policies.

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [Users](#users)
- [Access Keys](#access-keys)
- [Policies](#policies)
- [Attaching Policies](#attaching-policies)
- [Putting It All Together](#putting-it-all-together)
- [Command Reference](#command-reference)
- [Troubleshooting](#troubleshooting)

---

## Overview

Spinifex implements AWS-compatible IAM covering user management, access key lifecycle, policy CRUD, and policy attachment. Inline user policies (`put-user-policy`), user and policy tagging, and IAM roles for EC2 instances (see [IMDS](/docs/imds)) are also supported. All IAM resources are scoped to the account that creates them — users in one account cannot see or modify resources in another.

When you create an account with `spx admin account create`, Spinifex bootstraps a root user with an `AdministratorAccess` policy and writes the credentials to `~/.aws/credentials`. From there, you use the standard AWS CLI to manage additional users and permissions.

**How authentication works:** Every AWS CLI request is signed with SigV4 using an access key pair. The gateway verifies the signature, resolves the caller's account, and evaluates attached policies before routing the request. The root user (account `000000000000`) bypasses policy evaluation entirely.

## Prerequisites

- A running Spinifex cluster with the AWS gateway started
- An account created via `spx admin account create` (see [Spinifex Admin CLI](/docs/spinifex-admin-cli))
- AWS CLI configured with the account's profile:

```bash
export AWS_PROFILE=spinifex-myteam
```

## Instructions

## Users

### Create a User

```bash
aws iam create-user --user-name alice
```

The response includes the user's ARN, unique ID, and creation date:

```json
{
  "User": {
    "UserName": "alice",
    "UserId": "AIDA1A2B3C4D5E6F7890",
    "Arn": "arn:aws:iam::000000000001:user/alice",
    "Path": "/",
    "CreateDate": "2026-03-24T10:00:00Z"
  }
}
```

Use `--path` to organise users into hierarchical groups:

```bash
aws iam create-user --user-name bob --path /developers/
```

### Get a User

```bash
aws iam get-user --user-name alice
```

### List Users

```bash
aws iam list-users
```

Filter by path prefix:

```bash
aws iam list-users --path-prefix /developers/
```

### Delete a User

Before deleting a user, remove all access keys and detach all policies:

```bash
# Remove access keys
aws iam list-access-keys --user-name alice
aws iam delete-access-key --user-name alice --access-key-id AKIA...

# Detach policies
aws iam list-attached-user-policies --user-name alice
aws iam detach-user-policy --user-name alice \
  --policy-arn arn:aws:iam::000000000001:policy/MyPolicy

# Now delete
aws iam delete-user --user-name alice
```

## Access Keys

Access keys are how users authenticate with the AWS CLI. Each user can have up to **2 access keys** at a time, allowing key rotation without downtime.

### Create an Access Key

```bash
aws iam create-access-key --user-name alice
```

```json
{
  "AccessKey": {
    "UserName": "alice",
    "AccessKeyId": "AKIA1A2B3C4D5E6F7890ABCD",
    "Status": "Active",
    "SecretAccessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
    "CreateDate": "2026-03-24T10:05:00Z"
  }
}
```

> **The secret access key is only shown once.** Save it immediately. If lost, delete the key and create a new one.

Configure a profile for the new key. A Spinifex profile also needs the gateway endpoint and CA bundle (copy the values from your existing `spinifex-*` profile in `~/.aws/config`):

```bash
aws configure set aws_access_key_id AKIA1A2B3C4D5E6F7890ABCD --profile spinifex-alice
aws configure set aws_secret_access_key wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY --profile spinifex-alice
aws configure set region ap-southeast-2 --profile spinifex-alice
aws configure set endpoint_url https://localhost:9999 --profile spinifex-alice
aws configure set ca_bundle /var/lib/spinifex/config/ca.pem --profile spinifex-alice
```

Then set `AWS_PROFILE=spinifex-alice` to use it.

### List Access Keys

```bash
aws iam list-access-keys --user-name alice
```

### Rotate Access Keys

Create a second key, update your configuration, then delete the old one:

```bash
# 1. Create new key (while old key still works)
aws iam create-access-key --user-name alice

# 2. Update the profile with the new key
aws configure set aws_access_key_id AKIA_NEW_KEY_ID --profile spinifex-alice
aws configure set aws_secret_access_key NEW_SECRET --profile spinifex-alice

# 3. Verify the new key works
AWS_PROFILE=spinifex-alice aws sts get-caller-identity

# 4. Delete the old key
aws iam delete-access-key --user-name alice --access-key-id AKIA_OLD_KEY_ID
```

### Deactivate an Access Key

Temporarily disable a key without deleting it:

```bash
aws iam update-access-key --user-name alice \
  --access-key-id AKIA1A2B3C4D5E6F7890ABCD \
  --status Inactive
```

Reactivate it later:

```bash
aws iam update-access-key --user-name alice \
  --access-key-id AKIA1A2B3C4D5E6F7890ABCD \
  --status Active
```

## Policies

Policies are JSON documents that define what actions a user can perform on which resources.

### Policy Document Format

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["ec2:RunInstances", "ec2:DescribeInstances"],
      "Resource": "*"
    }
  ]
}
```

Each statement has:

- **Effect** — `Allow` or `Deny`
- **Action** — Service actions (e.g. `ec2:RunInstances`, `s3:GetObject`). Supports wildcards: `ec2:*`, `s3:Get*`, or `*` for all actions.
- **Resource** — Target resources. Use `*` for all resources.

**Evaluation order:** An explicit `Deny` always wins. If no statement matches, access is denied by default.

### Create a Policy

Save the policy document to a file, then create the policy:

```bash
cat > /tmp/ec2-readonly.json << 'EOF'
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": [
                "ec2:DescribeInstances",
                "ec2:DescribeImages",
                "ec2:DescribeKeyPairs",
                "ec2:DescribeSecurityGroups",
                "ec2:DescribeSubnets",
                "ec2:DescribeVpcs"
            ],
            "Resource": "*"
        }
    ]
}
EOF

aws iam create-policy \
  --policy-name EC2ReadOnly \
  --policy-document file:///tmp/ec2-readonly.json
```

```json
{
  "Policy": {
    "PolicyName": "EC2ReadOnly",
    "PolicyId": "ANPA1A2B3C4D5E6F7890",
    "Arn": "arn:aws:iam::000000000001:policy/EC2ReadOnly",
    "Path": "/",
    "DefaultVersionId": "v1",
    "CreateDate": "2026-03-24T10:10:00Z"
  }
}
```

### Common Policy Examples

**Full administrator access:**

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "*",
      "Resource": "*"
    }
  ]
}
```

**S3 read/write (for hybrid sync, backups):**

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:ListBucket",
        "s3:DeleteObject"
      ],
      "Resource": "*"
    }
  ]
}
```

**EC2 operator (launch and manage instances, no VPC changes):**

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances",
        "ec2:StartInstances",
        "ec2:StopInstances",
        "ec2:TerminateInstances",
        "ec2:DescribeInstances",
        "ec2:DescribeImages"
      ],
      "Resource": "*"
    }
  ]
}
```

**Deny terminate (attach alongside a broader Allow to prevent accidental termination):**

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Deny",
      "Action": "ec2:TerminateInstances",
      "Resource": "*"
    }
  ]
}
```

### Get a Policy

```bash
aws iam get-policy \
  --policy-arn arn:aws:iam::000000000001:policy/EC2ReadOnly
```

### Get the Policy Document

Use `get-policy-version` with version `v1` to retrieve the actual JSON document:

```bash
aws iam get-policy-version \
  --policy-arn arn:aws:iam::000000000001:policy/EC2ReadOnly \
  --version-id v1
```

### List Policies

```bash
aws iam list-policies
```

### Delete a Policy

A policy must be detached from all users before it can be deleted:

```bash
aws iam delete-policy \
  --policy-arn arn:aws:iam::000000000001:policy/EC2ReadOnly
```

## Attaching Policies

Policies have no effect until attached to a user.

### Attach a Policy to a User

```bash
aws iam attach-user-policy --user-name alice \
  --policy-arn arn:aws:iam::000000000001:policy/EC2ReadOnly
```

### List a User's Policies

```bash
aws iam list-attached-user-policies --user-name alice
```

### Detach a Policy

```bash
aws iam detach-user-policy --user-name alice \
  --policy-arn arn:aws:iam::000000000001:policy/EC2ReadOnly
```

## Putting It All Together

A complete workflow for onboarding a developer with scoped EC2 and S3 access:

```bash
# 1. Create the user
aws iam create-user --user-name dev-carol --path /developers/

# 2. Create an access key
aws iam create-access-key --user-name dev-carol
# Save the AccessKeyId and SecretAccessKey from the output

# 3. Create a scoped policy
cat > /tmp/dev-policy.json << 'EOF'
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Sid": "EC2Access",
            "Effect": "Allow",
            "Action": ["ec2:*"],
            "Resource": "*"
        },
        {
            "Sid": "S3ReadOnly",
            "Effect": "Allow",
            "Action": ["s3:GetObject", "s3:ListBucket"],
            "Resource": "*"
        },
        {
            "Sid": "NoTerminate",
            "Effect": "Deny",
            "Action": "ec2:TerminateInstances",
            "Resource": "*"
        }
    ]
}
EOF

aws iam create-policy \
  --policy-name DeveloperAccess \
  --policy-document file:///tmp/dev-policy.json

# 4. Attach the policy
aws iam attach-user-policy --user-name dev-carol \
  --policy-arn arn:aws:iam::000000000001:policy/DeveloperAccess

# 5. Configure the developer's AWS CLI profile with the key from step 2.
#    endpoint_url and ca_bundle: copy from an existing spinifex-* profile in ~/.aws/config
aws configure set aws_access_key_id AKIA_CAROL_KEY_ID --profile spinifex-carol
aws configure set aws_secret_access_key CAROL_SECRET --profile spinifex-carol
aws configure set region ap-southeast-2 --profile spinifex-carol
aws configure set endpoint_url https://localhost:9999 --profile spinifex-carol
aws configure set ca_bundle /var/lib/spinifex/config/ca.pem --profile spinifex-carol

# 6. Verify
AWS_PROFILE=spinifex-carol aws ec2 describe-instances
AWS_PROFILE=spinifex-carol aws ec2 terminate-instances --instance-ids i-123
# ^ This will be denied by the NoTerminate statement
```

## Command Reference

| Command                       | Key Parameters                               | Notes                             |
| ----------------------------- | -------------------------------------------- | --------------------------------- |
| `create-user`                 | `--user-name`, `--path`                      | Path default: `/`                 |
| `get-user`                    | `--user-name`                                |                                   |
| `list-users`                  | `--path-prefix`                              |                                   |
| `delete-user`                 | `--user-name`                                | Must have no keys or policies     |
| `create-access-key`           | `--user-name`                                | Max 2 per user; secret shown once |
| `list-access-keys`            | `--user-name`                                |                                   |
| `delete-access-key`           | `--user-name`, `--access-key-id`             |                                   |
| `update-access-key`           | `--user-name`, `--access-key-id`, `--status` | `Active` or `Inactive`            |
| `create-policy`               | `--policy-name`, `--policy-document`         | Max 6144 bytes                    |
| `get-policy`                  | `--policy-arn`                               | Metadata only                     |
| `get-policy-version`          | `--policy-arn`, `--version-id`               | Use `v1`; includes document       |
| `list-policy-versions`        | `--policy-arn`                               |                                   |
| `list-policies`               |                                              |                                   |
| `delete-policy`               | `--policy-arn`                               | Must not be attached              |
| `attach-user-policy`          | `--user-name`, `--policy-arn`                | Idempotent                        |
| `detach-user-policy`          | `--user-name`, `--policy-arn`                |                                   |
| `list-attached-user-policies` | `--user-name`                                |                                   |

## Troubleshooting

### AccessDenied on IAM Commands

The calling user must have IAM permissions. Attach a policy with `iam:*` actions, or use the root account profile:

```bash
export AWS_PROFILE=spinifex-myteam
aws iam list-users
```

### InvalidClientTokenId

The access key is either inactive or does not exist. Check the key status:

```bash
aws iam list-access-keys --user-name alice
```

If the key shows `Inactive`, reactivate it:

```bash
aws iam update-access-key --user-name alice \
  --access-key-id AKIA... --status Active
```

If the key was deleted, create a new one.

### DeleteConflict When Deleting a User

The user still has access keys or attached policies. Remove them first:

```bash
# Check for access keys
aws iam list-access-keys --user-name alice

# Check for attached policies
aws iam list-attached-user-policies --user-name alice
```

### DeleteConflict When Deleting a Policy

The policy is still attached to one or more users. Detach it from all users before deleting:

```bash
# Find who has it attached, then detach
aws iam detach-user-policy --user-name alice \
  --policy-arn arn:aws:iam::000000000001:policy/MyPolicy

aws iam delete-policy \
  --policy-arn arn:aws:iam::000000000001:policy/MyPolicy
```

### LimitExceeded When Creating Access Keys

Each user can have at most 2 access keys. Delete an existing key before creating a new one:

```bash
aws iam list-access-keys --user-name alice
aws iam delete-access-key --user-name alice --access-key-id AKIA_OLD
aws iam create-access-key --user-name alice
```

### MalformedPolicyDocument

The policy JSON is invalid. Check that:

- `Version` is exactly `"2012-10-17"`
- At least one `Statement` exists
- Each statement has `Effect` (`Allow`/`Deny`), `Action`, and `Resource`
- The document is under 6144 bytes
- The JSON is well-formed (no trailing commas, correct quoting)
