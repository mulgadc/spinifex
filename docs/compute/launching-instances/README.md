---
title: "Launching Instances"
seoTitle: "Launching AWS EC2 Instances on Spinifex — Spinifex Docs"
description: "Launch, manage, and connect to EC2-compatible virtual machines on Spinifex, with cloud-init, SSH key injection, VPC networking, and AWS lifecycle operations."
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
- [Stopping and Data Volumes](#stopping-and-data-volumes)
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

## Instructions

## Prerequisites

- A running Spinifex cluster (see [Setting Up Your Cluster](/docs/setting-up-your-cluster))
- AWS CLI configured with the `spinifex` profile:

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

## Stopping and Data Volumes

A guest that powers itself off (`poweroff`, `shutdown -h`) goes `stopping` then `stopped`, the same as `stop-instances`.
It is not restarted automatically.
`InstanceInitiatedShutdownBehavior=terminate` is not honoured yet: a guest shutdown always stops the instance.

Data volumes stay `in-use` and attached to a stopped instance, and `start-instances` mounts them again.
While the instance is stopped, `attach-volume` of one of its data volumes to another instance returns `VolumeInUse`, and `delete-volume` is refused.

`detach-volume` works on a stopped instance, with or without `--instance-id`.
The volume becomes `available` and the next start does not attach it.
The root volume of a stopped instance can be detached too, as on AWS.
The internal EFI variable store is not a customer volume: it never appears in `describe-volumes` or `BlockDeviceMappings`, and detaching it is refused with `OperationNotPermitted`.
The instance then cannot start: `start-instances` returns `InvalidParameterValue` with "Instance does not have a volume attached at root (/dev/sda1)".

`attach-volume` works on a stopped instance too.
The volume must be `available` and in the instance's availability zone, otherwise the call returns `VolumeInUse` or `InvalidVolume.ZoneMismatch`.
The attachment is recorded on the stopped instance and the volume becomes `in-use` straight away.
Attaching at the root device name (for example `/dev/sda1`) makes the volume the root again, and the next `start-instances` boots from it.

An instance stopped by an earlier build had its data volumes released to `available` while its record still listed them.
On start, a listed data volume that was deleted or is now attached to another instance is dropped with a warning in the daemon log, and the start continues.
A listed volume that is still `available` and unattached is attached again.
A missing root volume fails the start with the same `InvalidParameterValue` answer.

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

List only the types that can still be launched:

```bash
aws ec2 describe-instance-types \
  --filters Name=capacity,Values=true
```

Each type appears once, whatever its free capacity.
This filter is a Spinifex extension and AWS does not declare it, so it is ignored by a region that does not implement it.
AWS reports exhaustion at launch instead, with `InsufficientInstanceCapacity`, and so does Spinifex.

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
