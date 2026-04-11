---
title: "Nginx Web Server (Load Balanced)"
description: "Deploy a VPC with two private EC2 instances running Nginx, fronted by an internet-facing ALB using Terraform on Spinifex."
category: "Terraform Workbooks"
tags:
  - terraform
  - nginx
  - ec2
  - elbv2
  - alb
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

# Terraform: Nginx Web Servers with ALB

> Deploys two Nginx web server instances behind an Application Load Balancer on Spinifex.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

Deploy two Nginx web servers behind an internet-facing Application Load Balancer on Spinifex using Terraform/OpenTofu. This workbook provisions a VPC with public and private subnets, an internet gateway and NAT Gateway, route tables, security group, SSH key pair, an application load balancer (ALB) and two EC2 instances with cloud-init user-data that installs and starts Nginx. Only the ALB is reachable from outside the VPC — the Nginx instances live in the **private subnets** and reach the internet only for cloud-init bootstrapping via the NAT Gateway.

```
                Internet
                    │
               ┌────▼────┐
               │   IGW   │
               └────┬────┘
         ┌──────────┼──────────┐
         │                     │
  ┌──────▼─────┐         ┌─────▼──────┐
  │  public_a  │         │  public_b  │
  │  ALB ENI   │         │  ALB ENI   │
  │  NAT GW    │         └────────────┘
  └──────┬─────┘
         │ SNAT for private subnets
  ┌──────▼─────┐         ┌────────────┐
  │ private_a  │         │ private_b  │
  │  nginx_1   │         │  nginx_2   │
  └────────────┘         └────────────┘
```

**What you'll learn:**

- Configuring the AWS Terraform provider to target Spinifex
- Creating a VPC with public + private subnets, an IGW and a NAT Gateway
- Provisioning a multi-AZ internet-facing ALB fronting private workers
- Provisioning an EC2 instance with cloud-init user-data
- Generating SSH key pairs with the TLS provider

**What gets created**

| Resource | Name | Purpose |
|---|---|---|
| VPC | `nginx-alb-vpc` | Isolated network (10.20.0.0/16) |
| Public Subnets | `nginx-alb-public-a`, `nginx-alb-public-b` | Two AZs hosting the ALB and NAT Gateway |
| Private Subnets | `nginx-alb-private-a`, `nginx-alb-private-b` | Two AZs hosting the Nginx workers |
| Internet Gateway | `nginx-alb-igw` | Routes internet traffic for the public subnets |
| Elastic IP | `nginx-alb-nat-eip` | Public address for the NAT Gateway |
| NAT Gateway | `nginx-alb-nat` | Outbound internet for the private subnets (cloud-init apt bootstrap) |
| Security Group | `nginx-alb-sg` | Allows SSH (22) and HTTP (80) inbound |
| EC2 Instances | `nginx-alb-1`, `nginx-alb-2` | Debian 12 with Nginx via cloud-init (private subnets) |
| ALB | `nginx-alb` | Internet-facing Application Load Balancer on port 80 |
| Target Group | `nginx-alb-tg` | HTTP health-checked group for both instances |
| Listener | HTTP :80 | Forwards traffic to the target group |

**Prerequisites:**

- Spinifex installed and running (see [Installing Spinifex](/docs/install))
- A Debian 12 AMI imported (see [Setting Up Your Cluster](/docs/setting-up-your-cluster))
- OpenTofu or Terraform installed
- `AWS_PROFILE=spinifex` configured

## Instructions

### Step 1. Get the Template

Clone the Terraform examples from the Spinifex repository:

```bash
git clone --depth 1 --filter=blob:none --sparse https://github.com/mulgadc/spinifex.git spinifex-tf
cd spinifex-tf
git sparse-checkout set docs/terraform
cd docs/terraform/nginx-alb
```

Or create a `main.tf` file and paste the full configuration below.

<!-- INCLUDE: main.tf lang:hcl -->

### Step 2. Deploy

```bash
export AWS_PROFILE=spinifex
tofu init
tofu apply
```

### Step 3. Verify

> **Note:** EC2 instances can take 30+ seconds to boot after apply, and the NAT Gateway must be `available` before cloud-init on the workers can reach the apt repository. If the ALB returns 5xx or HTTP is unreachable, wait and retry — the target group health checks need a moment to mark both instances healthy once Nginx has installed.

The ALB is internet-facing, but the DNS name Spinifex returns (`*.elb.spinifex.local`) will not resolve from your host. Fetch the ALB's public IP with the AWS CLI:

```bash
ALB_IP=$(aws elbv2 describe-load-balancers --names nginx-alb \
  --query 'LoadBalancers[0].AvailabilityZones[].LoadBalancerAddresses[].IpAddress' \
  --output text)
echo "ALB public IP: $ALB_IP"
```

Then hit the ALB — successive requests should alternate between Server 1 and Server 2:

```bash
curl http://$ALB_IP
curl http://$ALB_IP
```

Open `http://$ALB_IP` in your browser and refresh to see the page alternate content served from each instance.

The Nginx instances themselves only have private IPs (see the `instance_1_private_ip` / `instance_2_private_ip` outputs) and are only reachable from inside the VPC — go through the ALB.

Check target health via AWS CLI:

```bash
aws elbv2 describe-target-health --target-group-arn <tg_arn>
```

### Cleanup

```bash
tofu destroy
```

## Troubleshooting

### AMI Not Found

Ensure you have imported a Debian 12 image. Check available AMIs:

```bash
aws ec2 describe-images --owners 000000000000 --profile spinifex
```

### Provider Connection Refused

Verify Spinifex services are running:

```bash
sudo systemctl status spinifex.target
curl -k https://localhost:9999/
```

### ALB Returns 5xx / Targets Unhealthy

Give the instances a moment to finish cloud-init (Nginx has to install before it can answer health checks). Check target health:

```bash
TG_ARN=$(aws elbv2 describe-target-groups --names nginx-alb-tg \
  --query 'TargetGroups[0].TargetGroupArn' --output text)
aws elbv2 describe-target-health --target-group-arn "$TG_ARN"
```

If targets stay unhealthy, verify the instances are running:

```bash
aws ec2 describe-instances --profile spinifex
```

If cloud-init on the workers never finished, confirm the NAT Gateway is `available` (the private subnets rely on it for outbound apt access):

```bash
aws ec2 describe-nat-gateways --query 'NatGateways[].[NatGatewayId,State]'
```

### Nginx Not Responding

The Nginx instances have no public IP, so you can't SSH in directly from your host. If you need to inspect cloud-init logs, launch a small jump host in the same VPC or run commands via the Spinifex console, then:

```bash
ssh -i nginx-alb-demo.pem ec2-user@<instance_private_ip>
sudo journalctl -u cloud-init --no-pager
sudo systemctl status nginx
```
