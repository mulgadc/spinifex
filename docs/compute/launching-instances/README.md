---
title: "Launching Instances"
description: "Launch, manage, and connect to EC2-compatible virtual machines on Spinifex."
category: "Compute"
tags:
  - ec2
  - instances
  - vm
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "AWS EC2 CLI"
    url: "https://docs.aws.amazon.com/cli/latest/reference/ec2/"
---

# Launching Instances

> Launch, manage, and connect to EC2-compatible virtual machines on Spinifex.

## Table of Contents

- [Overview](#overview)
- [Launch](#launch)
- [Manage](#manage)
- [Modify Instance Attributes](#modify-instance-attributes)
- [Instance Metadata Options](#instance-metadata-options)
- [Spot Instances](#spot-instances)
- [Console Output](#console-output)
- [Instance Types](#instance-types)
- [SSH](#ssh)
- [Troubleshooting](#troubleshooting)
  - [Instance Fails to Boot](#instance-fails-to-boot)
  - [Cannot SSH Into Instance](#cannot-ssh-into-instance)

---

## Overview

Spinifex provides EC2-compatible VM management built on QEMU/KVM. Instances support cloud-init, SSH key injection, VPC networking, and standard AWS lifecycle operations.

**Supported operations:**

- `run-instances` — Launch new VMs
- `describe-instances` / `describe-instance-status` / `describe-instance-attribute` — Query state
- `stop-instances` / `start-instances` — Lifecycle
- `reboot-instances` — In-place restart
- `terminate-instances` — Permanent removal
- `modify-instance-attribute` — Change instance type, user data, or termination protection
- `modify-instance-metadata-options` — IMDS settings (IMDSv2 is always enforced)
- `request-spot-instances` / `describe-spot-instance-requests` / `cancel-spot-instance-requests` — Spot instance requests
- `get-console-output` — Retrieve serial console log
- `describe-instance-types` — List available instance types and capacity

## Instructions

## Prerequisites

Ensure the AWS profile is set:

```bash
export AWS_PROFILE=spinifex
```

## Launch

```bash
INSTANCE_ID=$(aws ec2 run-instances \
  --image-id $SPINIFEX_AMI \
  --instance-type t3.small \
  --key-name spinifex-key \
  --query 'Instances[0].InstanceId' --output text)
```

Other supported launch flags: `--subnet-id`, `--security-group-ids`, `--tag-specifications`, `--user-data`, `--iam-instance-profile`, `--block-device-mappings`, `--placement`, `--count`.

## Manage

```bash
aws ec2 describe-instances --instance-ids $INSTANCE_ID
aws ec2 describe-instance-status --instance-ids $INSTANCE_ID --include-all-instances
aws ec2 stop-instances --instance-ids $INSTANCE_ID
aws ec2 start-instances --instance-ids $INSTANCE_ID
aws ec2 terminate-instances --instance-ids $INSTANCE_ID
aws ec2 reboot-instances --instance-ids $INSTANCE_ID
```

## Modify Instance Attributes

Change instance type, user data, or termination protection. Instance type and user data require the instance to be **stopped** first.

### Change Instance Type

```bash
aws ec2 stop-instances --instance-ids $INSTANCE_ID

aws ec2 modify-instance-attribute \
  --instance-id $INSTANCE_ID \
  --instance-type t3.medium

aws ec2 start-instances --instance-ids $INSTANCE_ID
```

### Termination Protection

```bash
aws ec2 modify-instance-attribute \
  --instance-id $INSTANCE_ID \
  --disable-api-termination
```

## Instance Metadata Options

IMDSv2 is always enforced — `--http-tokens optional` and `--http-endpoint disabled` are rejected with `UnsupportedOperation`, matching AWS account-level IMDSv2 enforcement. The hop limit is adjustable (raise it for containerised workloads that reach IMDS through an extra network hop):

```bash
aws ec2 modify-instance-metadata-options \
  --instance-id $INSTANCE_ID \
  --http-put-response-hop-limit 2
```

## Spot Instances

Spot requests are supported as a compatibility layer over the on-demand path: requests launch real VMs on your own compute immediately and report `active`/`fulfilled`. There is no spot market — no bidding, interruption, or reclamation.

```bash
aws ec2 request-spot-instances \
  --instance-count 1 \
  --launch-specification '{"ImageId":"'$SPINIFEX_AMI'","InstanceType":"t3.small","KeyName":"spinifex-key"}'

aws ec2 describe-spot-instance-requests
aws ec2 cancel-spot-instance-requests --spot-instance-request-ids $SIR_ID
```

## Console Output

Retrieve the serial console log for a running instance. Output is base64-encoded.

```bash
aws ec2 get-console-output --instance-id $INSTANCE_ID
```

Decode the output:

```bash
aws ec2 get-console-output --instance-id $INSTANCE_ID \
  --query 'Output' --output text | base64 -d
```

## Instance Types

List instance types available on the current host. The catalog is generated from the host CPU (Intel, AMD, or ARM) and includes burstable (t-family), general purpose (m-family), compute optimised (c-family), and memory optimised (r-family) types.

```bash
aws ec2 describe-instance-types
```

Filter to a specific type:

```bash
aws ec2 describe-instance-types \
  --query "InstanceTypes[?InstanceType=='t3.small']"
```

Show capacity (how many of each type can still be launched):

```bash
aws ec2 describe-instance-types \
  --filters Name=capacity,Values=true
```

## SSH

To SSH into instances via their public IPs, see [Setting Up Your Cluster](/docs/setting-up-your-cluster).

## Troubleshooting

### Instance Fails to Boot

Check QEMU logs for the instance and verify the AMI architecture matches your host:

```bash
journalctl -u spinifex-daemon -f
aws ec2 describe-images --image-ids $SPINIFEX_AMI
```

If the AMI is for a different architecture (e.g. arm64 on an x86_64 host), import the correct image:

```bash
spx admin images list
spx admin images import --name debian-13-x86_64
```

### Cannot SSH Into Instance

cloud-init needs time to configure the instance after boot. Wait 30-60 seconds and retry.

If the connection **times out** rather than being refused, the security group is likely blocking port 22. The default security group denies all inbound traffic (matching AWS), so SSH must be explicitly allowed:

```bash
# Allow SSH from anywhere on the instance's security group
aws ec2 authorize-security-group-ingress \
  --group-id $SG_ID --protocol tcp --port 22 --cidr 0.0.0.0/0
```

See [VPC Networking — Security Groups](/docs/vpc-networking#security-groups) for scoping rules to a trusted CIDR.

Verify the SSH key was specified correctly when launching:

```bash
aws ec2 describe-instances --instance-ids $INSTANCE_ID
```

Check the `KeyName` field matches the key you're using to connect.
