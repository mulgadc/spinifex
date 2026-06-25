---
title: "Bastion with Private Subnet"
description: "Deploy a bastion host to access an isolated private compute instance with no internet connectivity."
category: "Terraform Workbooks"
tags:
  - terraform
  - bastion
  - vpc
  - security
  - private subnet
  - workbook
resources:
  - title: "Terraform AWS Provider"
    url: "https://registry.terraform.io/providers/hashicorp/aws/latest"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "OpenTofu"
    url: "https://opentofu.org/"
---

# Terraform: Bastion with Private Subnet

> Deploy a bastion host to access an isolated private compute instance with no internet connectivity.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

Deploy a VPC with public and private subnets where the private subnet has no internet access. A bastion host in the public subnet is the only way to reach instances in the private subnet. This pattern is used for sensitive workloads that must remain isolated from the internet — the private instance cannot make or receive any connections outside the VPC.

**Architecture:**

<p align="center">
  <img src="../../../.github/assets/diagrams/tf-bastion.svg" alt="Bastion architecture — WAN reaches bastion in public subnet, SSH hop to app server in private subnet" width="900">
</p>

**What you'll learn:**

- Creating public and private subnets in a VPC
- Isolating compute instances from the internet using route tables and security groups
- Using a bastion host as the sole access point to private resources
- SSH hopping through a bastion to reach private instances

**Use cases:**

- Sensitive data processing that must not have internet egress
- Internal services that should only be reachable within the VPC
- Compliance workloads requiring network isolation

**Prerequisites:**

- Spinifex installed and running (see [Installing Spinifex](/docs/install))
- An Ubuntu 26.04 AMI imported (see [Setting Up Your Cluster](/docs/setting-up-your-cluster))
- OpenTofu or Terraform installed

## Instructions

### Step 1. Get the Template

Clone the Terraform examples from the Spinifex repository:

```bash
git clone --depth 1 --filter=blob:none --sparse https://github.com/mulgadc/spinifex.git spinifex-tf
cd spinifex-tf
git sparse-checkout set docs/terraform
cd docs/terraform/bastion-private-subnet
```

Or create a `main.tf` file and paste the full configuration below.

<!-- INCLUDE: main.tf lang:hcl -->

### Step 2. Deploy

The workbook defaults to `t3.small` (2 vCPU, 2 GiB) for both bastion and private instance. On clusters without that type registered, override with `TF_VAR_instance_type` — query what's available with `aws ec2 describe-instance-types`.

```bash
export AWS_PROFILE=spinifex
tofu init
tofu apply
```

### Step 3. Connect

> **Note:** EC2 instances can take 30+ seconds to boot after apply. If SSH is unreachable, wait and retry.

SSH into the bastion:

```bash
ssh -i bastion-demo.pem ubuntu@<bastion_public_ip>
```

From the bastion, SSH to the private instance. The key is pre-installed at `~/.ssh/bastion-demo.pem` via cloud-init:

```bash
ssh -i ~/.ssh/bastion-demo.pem ubuntu@<private_ip>
```

### Step 4. Verify Isolation

From the private instance, confirm there is no internet connectivity:

```bash
# This should time out — the private instance has no route to the internet
curl --connect-timeout 5 https://deb.debian.org || echo "No internet access (expected)"
```

The private instance can only communicate within the VPC (`10.20.0.0/16`). Its security group restricts egress to VPC-internal traffic only.

### Clean Up

```bash
tofu destroy
```

## Troubleshooting

### Cannot SSH to Private Instance

The private instance has no public IP — it is only reachable from the bastion. SSH to the bastion first, then use the pre-installed key to hop:

```bash
ssh -i bastion-demo.pem ubuntu@<bastion_ip>
ssh -i ~/.ssh/bastion-demo.pem ubuntu@<private_ip>
```

If the key is missing on the bastion, check that cloud-init completed successfully:

```bash
sudo journalctl -u cloud-init --no-pager
ls -la ~/.ssh/bastion-demo.pem
```

### Private Instance Can Reach the Internet

If the private instance unexpectedly has internet access, check that its route table has no default route:

```bash
aws ec2 describe-route-tables --profile spinifex
```

The private route table should have no `0.0.0.0/0` route. Also verify the private security group egress is restricted to the VPC CIDR (`10.20.0.0/16`).

### AMI Not Found

Ensure you have imported an Ubuntu 26.04 image:

```bash
aws ec2 describe-images --owners 000000000000 --profile spinifex
```
