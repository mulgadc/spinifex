---
title: "Nginx Web Server"
description: "Deploy a VPC with a public subnet and an EC2 instance running Nginx using Terraform on Spinifex."
category: "Terraform Workbooks"
tags:
  - terraform
  - nginx
  - ec2
  - vpc
  - workbook
resources:
  - title: "Terraform AWS Provider"
    url: "https://registry.terraform.io/providers/hashicorp/aws/latest"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "OpenTofu"
    url: "https://opentofu.org/"
---

# Terraform: Nginx Web Server

> Deploy a VPC with a public subnet and an EC2 instance running Nginx using Terraform on Spinifex.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

Deploy a complete Nginx web server on Spinifex using Terraform/OpenTofu. This workbook provisions a VPC, public subnet, internet gateway, route table, security group, SSH key pair, and an EC2 instance with cloud-init user-data that installs and starts Nginx.

**What you'll learn:**

- Configuring the AWS Terraform provider to target Spinifex
- Creating a VPC with public internet access
- Provisioning an EC2 instance with cloud-init user-data
- Generating SSH key pairs with the TLS provider

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
cd docs/terraform/nginx-webserver
```

Or create a `main.tf` file and paste the full configuration below.

<!-- INCLUDE: main.tf lang:hcl -->

### Step 2. Deploy

The workbook defaults to `t3.small` (2 vCPU, 2 GiB). On clusters without that type registered, override with `TF_VAR_instance_type` — query what's available with `aws ec2 describe-instance-types`.

```bash
export AWS_PROFILE=spinifex
tofu init
tofu apply
```

### Step 3. Verify

> **Note:** EC2 instances can take 30+ seconds to boot after apply. If SSH or HTTP is unreachable, wait and retry.

After apply completes, use the outputs to test:

```bash
curl http://<public_ip>
ssh -i nginx-demo.pem ubuntu@<public_ip>
```

Open the `web_url` output in your browser to see the Nginx welcome page.

### Clean Up

```bash
tofu destroy
```

## Troubleshooting

### AMI Not Found

Ensure you have imported an Ubuntu 26.04 image. Check available AMIs:

```bash
aws ec2 describe-images --owners 000000000000 --profile spinifex
```

### Provider Connection Refused

Verify Spinifex services are running:

```bash
sudo systemctl status spinifex.target
curl -k https://localhost:9999/
```

### SSH Connection Timeout

Check that the security group allows inbound SSH (port 22) and that the instance has a public IP assigned. Verify the instance is running:

```bash
aws ec2 describe-instances --profile spinifex
```

### Nginx Not Responding

SSH into the instance and check cloud-init logs:

```bash
ssh -i nginx-demo.pem ubuntu@<public_ip>
sudo journalctl -u cloud-init --no-pager
sudo systemctl status nginx
```
