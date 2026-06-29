---
title: "ECS Quickstart"
description: "Stand up a complete AWS-compatible ECS stack on Spinifex with Terraform — a VPC, IAM roles, a cluster, a task definition with a task role, container instances launched from the ECS node image, and an awsvpc service fronted by an Application Load Balancer."
category: "Terraform Workbooks"
tags:
  - terraform
  - ecs
  - containers
  - alb
  - iam
  - vpc
  - workbook
sections:
  - overview
  - prerequisites
  - instructions
  - troubleshooting
resources:
  - title: "ECS Service Documentation"
    url: "/docs/ecs"
  - title: "Terraform AWS Provider"
    url: "https://registry.terraform.io/providers/hashicorp/aws/latest"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "OpenTofu"
    url: "https://opentofu.org/"
---

# Terraform: ECS Quickstart

> The smallest end-to-end ECS example on Spinifex: a VPC, the IAM roles ECS needs, a cluster, a task definition, container instances booted from the ECS node image, and an `awsvpc` service load-balanced behind an Application Load Balancer.

## Overview

This workbook is the Terraform-native equivalent of the console's **Provision capacity** action. It provisions a full ECS stack in one `apply`: a VPC with two public subnets, the `ecsInstanceRole` instance profile, a task IAM role, an ECS cluster, an `awsvpc` task definition running nginx, one or more container instances launched from the `spinifex-ecs-node` AMI, and an Application Load Balancer with a target group the service registers into.

Because a Spinifex container instance reaches the control plane over the **gateway** (TLS + SigV4) rather than a managed AWS endpoint, the workbook makes the two things the console injects for you explicit: a **LAN-reachable** gateway URL and the gateway CA, both baked into the instance's cloud-init user-data. The agent draws its credentials from IMDS via the `ecsInstanceRole` instance profile, so no static keys are written.

## Prerequisites

- **Spinifex running**, with the AWS CLI configured for the `spinifex` profile (see [Installing Spinifex](/docs/install)) and OpenTofu (or Terraform) installed.
- **The `spinifex-ecs-node` AMI imported** — resolved here by the `tag:spinifex:managed-by=ecs` filter:

  ```bash
  aws ec2 describe-images \
    --filters 'Name=tag:spinifex:managed-by,Values=ecs' \
    --query 'Images[].[ImageId,Name]' --output text
  ```

- **A LAN-reachable gateway URL** — the host's WAN/bridge IP, not `127.0.0.1` (a guest VM cannot reach the host loopback). For example `https://192.168.1.33:9999`.
- **The gateway CA PEM** readable at `gateway_ca_cert_path` (default `/etc/spinifex/ca.pem`).

## Instructions

### 1. Fetch the workbook

```bash
git clone --depth 1 --filter=blob:none --sparse https://github.com/mulgadc/spinifex.git spinifex-tf
cd spinifex-tf
git sparse-checkout set docs/terraform-workbooks
cd docs/terraform-workbooks/ecs-quickstart
```

### 2. Apply

```bash
export AWS_PROFILE=spinifex

tofu init
tofu apply -var 'gateway_url=https://<host-lan-ip>:9999'
```

`ecsInstanceRole` is account-global. If you have already used the console's provision-capacity action it exists already, so skip re-creating it:

```bash
tofu apply \
  -var 'gateway_url=https://<host-lan-ip>:9999' \
  -var 'create_instance_role=false'
```

### 3. Verify

Container instances take ~30-60s to boot and register:

```bash
aws ecs list-container-instances --cluster ecs-quickstart
aws ecs describe-services --cluster ecs-quickstart --services ecs-quickstart-web \
  --query 'services[0].[runningCount,desiredCount]'
```

The ALB DNS name ends in `.elb.spinifex.local` and does not resolve from your host. Fetch its public IP and curl it:

```bash
aws elbv2 describe-load-balancers --names ecs-quickstart-alb \
  --query 'LoadBalancers[0].AvailabilityZones[].LoadBalancerAddresses[].IpAddress' \
  --output text
curl http://<alb_public_ip>
```

### 4. Variables

| Variable | Default | Purpose |
|---|---|---|
| `region` | `ap-southeast-2` | AWS region. |
| `cluster_name` | `ecs-quickstart` | Cluster + resource name prefix. |
| `instance_type` | `t3.small` | Container instance type. |
| `container_count` | `1` | Container instances + service desired count. |
| `task_image` | `docker.io/library/nginx:1.27-alpine` | Image the task runs. |
| `spinifex_endpoint` | `https://127.0.0.1:9999` | Gateway as seen from the host running Terraform. |
| `gateway_url` | _(required)_ | Gateway as seen from a guest VM (LAN-reachable). |
| `gateway_ca_cert_path` | `/etc/spinifex/ca.pem` | Host-readable gateway CA PEM. |
| `create_instance_role` | `true` | Create `ecsInstanceRole`; set `false` if it already exists. |

### 5. Teardown

```bash
tofu destroy -var 'gateway_url=https://<host-lan-ip>:9999'
```

`DeleteCluster` cascades through the service, its tasks, and the container instance registrations, so the destroy round-trips cleanly.

## Troubleshooting

**Container instances never register.** The agent registers over the gateway, not a managed endpoint. Confirm `gateway_url` is the host's **LAN-reachable** IP (not `127.0.0.1`) and that `gateway_ca_cert_path` points at the real gateway CA. Because `cloud-init write_files` runs once per instance, a corrected user-data needs an instance **replacement** — `tofu apply -replace='aws_instance.node[0]'` — not an in-place modify.

**`apply` fails resolving the AMI.** The `spinifex-ecs-node` image is not imported, or its `spinifex:managed-by=ecs` tag is missing. Re-run the `describe-images` check in Prerequisites.

**`MissingParameter` on the instance.** The gateway's `RunInstances` requires a `KeyName`; the workbook generates a key pair for you, so this only appears if you have stripped that out.

**Service `runningCount` stays below `desiredCount`.** No instance has free capacity, or tasks cannot pull the image. Confirm at least one `ACTIVE` container instance and that the subnets have an Internet Gateway route.

**`create_instance_role` conflict.** If `ecsInstanceRole` already exists (from the console), pass `-var 'create_instance_role=false'` so Terraform does not try to recreate it.
