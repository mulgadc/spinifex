---
title: "Placement Groups"
description: "Create and manage spread and cluster placement groups for hardware-level instance placement control."
category: "Compute"
tags:
  - ec2
  - placement
  - spread
  - cluster
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "AWS Placement Groups"
    url: "https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/placement-groups.html"
---

# Placement Groups

> Create and manage spread and cluster placement groups for hardware-level instance placement control.

## Table of Contents

- [Overview](#overview)
- [Create](#create)
- [Launch Into a Placement Group](#launch-into-a-placement-group)
- [Describe](#describe)
- [Delete](#delete)
- [Troubleshooting](#troubleshooting)
  - [InsufficientInstanceCapacity Error](#insufficientinstancecapacity-error)
  - [Cannot Delete Placement Group](#cannot-delete-placement-group)

---

## Overview

Placement groups control how Spinifex distributes instances across physical hosts. Two strategies are supported:

- **Spread** — One instance per physical host, maximising fault isolation
- **Cluster** — All instances on the same host, minimising latency

The `partition` strategy is not supported.

**Supported operations:**

- `create-placement-group` — Create a new group
- `describe-placement-groups` — Query groups with optional filters
- `delete-placement-group` — Remove an empty group

## Instructions

## Prerequisites

- A running Spinifex cluster (see [Setting Up Your Cluster](/docs/setting-up-your-cluster))
- AWS CLI configured with the `spinifex` profile:

```bash
export AWS_PROFILE=spinifex
```

## Create

```bash
aws ec2 create-placement-group \
  --group-name my-spread-group \
  --strategy spread
```

```bash
aws ec2 create-placement-group \
  --group-name my-cluster-group \
  --strategy cluster
```

## Launch Into a Placement Group

```bash
aws ec2 run-instances \
  --image-id $SPINIFEX_AMI \
  --instance-type t3.small \
  --key-name spinifex-key \
  --placement GroupName=my-spread-group \
  --count 3
```

## Describe

```bash
aws ec2 describe-placement-groups
aws ec2 describe-placement-groups --group-names my-spread-group
aws ec2 describe-placement-groups --group-ids pg-abc123
aws ec2 describe-placement-groups \
  --filters Name=strategy,Values=spread
```

## Delete

The group must have no running instances before it can be deleted:

```bash
aws ec2 terminate-instances --instance-ids $INSTANCE_ID
aws ec2 delete-placement-group --group-name my-spread-group
```

## Troubleshooting

### InsufficientInstanceCapacity Error

Not enough distinct physical hosts for a spread launch. Reduce `--count` or terminate existing instances to free host slots:

```bash
spx admin nodes list
```

### Cannot Delete Placement Group

The group still has running instances. Terminate them first:

```bash
aws ec2 describe-instances \
  --filters Name=placement-group-name,Values=my-spread-group
aws ec2 terminate-instances --instance-ids $INSTANCE_ID
```
