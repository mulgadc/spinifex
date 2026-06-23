---
title: "EKS Quickstart"
description: "Stand up a minimal managed-Kubernetes cluster and a browsable Spinifex-themed demo app — VPC, IAM roles, an EKS cluster, a one- or three-worker node group, an ECR repository, and a load-balanced web page — using Terraform on Spinifex."
category: "Terraform Workbooks"
tags:
  - terraform
  - eks
  - kubernetes
  - iam
  - vpc
  - workbook
resources:
  - title: "Terraform AWS Provider"
    url: "https://registry.terraform.io/providers/hashicorp/aws/latest"
  - title: "Terraform Kubernetes Provider"
    url: "https://registry.terraform.io/providers/hashicorp/kubernetes/latest"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "OpenTofu"
    url: "https://opentofu.org/"
---

# Terraform: EKS Quickstart

> The smallest end-to-end EKS example on Spinifex: a VPC, the two IAM roles EKS needs, a one- or three-worker cluster, an ECR repository, and a Spinifex-themed demo app you can open in a browser.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

Provision a managed Kubernetes cluster on Spinifex with Terraform/OpenTofu — and finish with something you can actually see. As well as the cluster, this workbook creates an **ECR repository**, then deploys the Spinifex-themed demo app onto the cluster and publishes it on the worker's public IP. When `apply` finishes, open the **`demo_url`** output: the page reports the **pod, node, cluster, and region** that answered. Refresh it and the answering pod changes, so even a non-technical viewer can watch Kubernetes scheduling and load-balancing in real time.

Under the hood it keeps things minimal: a VPC with two public subnets, an IAM role for the control plane, an IAM role for the workers with the three managed policies AWS-managed node groups expect, a public-endpoint `aws_eks_cluster`, and a node group sized by **`node_desired_size`** (1 for a single-node demo, or 3 for an HA-shaped cluster). You build the demo image and push it to the ECR repository, then Terraform uses the **Kubernetes provider** (authenticating exactly like `kubectl` does, via `aws eks get-token`) to deploy the Deployment + NodePort Service.

**What you'll learn:**

- Configuring the AWS provider to target Spinifex's `eks`, `ec2`, `iam` and `sts` endpoints
- Creating the EKS cluster and worker IAM roles with faithful managed-policy attachments
- Provisioning an `aws_eks_cluster` and a managed `aws_eks_node_group`
- Pointing the Kubernetes provider at the new cluster and deploying a workload from the same `apply`
- Opening one rule on the auto-managed worker security group to expose a NodePort

**What gets created**

| Resource | Name | Purpose |
|---|---|---|
| VPC | `eks-quickstart-vpc` | Isolated network (10.30.0.0/16) |
| Subnets | `eks-quickstart-subnet-a/-b` | Public subnets for the cluster and worker |
| Internet Gateway | `eks-quickstart-igw` | Egress so the worker can pull the demo image |
| IAM Roles | `eks-quickstart-cluster-role`, `eks-quickstart-node-role` | Control-plane and worker roles |
| ECR Repository | `spinifex-demo` | Holds the demo image the workers pull |
| EKS Cluster | `eks-quickstart` | Public API endpoint, API auth mode, Kubernetes 1.32 |
| Node Group | `default` | `node_desired_size` `t3.medium` worker(s) — 1 or 3 |
| SG Ingress Rule | `eks-quickstart-demo-nodeport` | Opens the demo NodePort on the auto-managed worker SG |
| K8s Deployment | `spinifex-demo` | The themed demo image from ECR, 2 replicas |
| K8s Service | `spinifex-demo` | NodePort publishing the demo on the worker |

**Spinifex specifics**

- The cluster's security groups are **auto-managed** — `vpc_config.security_group_ids` is ignored. To reach a NodePort, this workbook looks the worker SG up by its deterministic name (`eks-cluster-<name>-nodegroup-sg`) and adds a single ingress rule.
- The worker AMI is always Spinifex's `eks-node` image; `ami_type` is recorded but does **not** select the image.
- `authentication_mode` must be `"API"` (the `API_AND_CONFIG_MAP` mode is rejected).
- The workers need outbound internet (here via the IGW) to pull the demo container image.

**Prerequisites:**

- Spinifex installed and running (see [Installing Spinifex](/docs/install))
- The Spinifex `eks-node` image available on the cluster
- OpenTofu or Terraform, plus `kubectl` and the AWS CLI
- Docker, to build and push the demo image to ECR

## Instructions

### Step 1. Get the Template

```bash
git clone --depth 1 --filter=blob:none --sparse https://github.com/mulgadc/spinifex.git spinifex-tf
cd spinifex-tf
git sparse-checkout set docs/terraform-workbooks
cd docs/terraform-workbooks/eks-quickstart
```

Or create a `main.tf` file and paste the full configuration below.

<!-- INCLUDE: main.tf lang:hcl -->

### Step 2. Deploy

The cluster and the demo app are **two root modules with separate state**. Apply the cluster first:

```bash
export AWS_PROFILE=spinifex
tofu init
tofu apply
```

This creates the cluster (which bootstraps a control-plane VM and brings up k3s — a few minutes in `CREATING`), launches the worker(s), creates the `spinifex-demo` ECR repository, and opens the NodePort.

Set the worker count with `node_desired_size` (`1` for a single node, `3` for an HA-shaped cluster):

```bash
tofu apply -var node_desired_size=3
```

Once the cluster is `ACTIVE`, **build and push the demo image** to the ECR repository this created (full commands in [`../demo-app/README.md`](../demo-app/README.md)):

```bash
cd ../demo-app
REGISTRY=$(cd ../eks-quickstart && tofu output -raw ecr_repository_url)
REGISTRY_HOST=${REGISTRY%%/*}
aws ecr get-login-password | docker login --username AWS --password-stdin "$REGISTRY_HOST"
docker build -t "${REGISTRY}:latest" .
docker push "${REGISTRY}:latest"
cd ../eks-quickstart
```

Then deploy the demo app from the nested `workloads/` module — it defaults `demo_image` to the parent's ECR repository at `:latest`:

```bash
cd workloads
tofu init
tofu apply
```

> **Why two modules?** The Kubernetes provider in `workloads/` reads the cluster endpoint from a live `data "aws_eks_cluster"` source, so it's only ever configured while the cluster exists. Keeping it out of the cluster module means `destroy` never tries to refresh a workload against a cluster that's already gone — the failure mode where the provider falls back to `http://localhost:80` and reports `connection refused`. Always destroy `workloads/` before the cluster.

> **Same profile for the Kubernetes provider.** The Kubernetes provider authenticates by shelling out to `aws eks get-token`, which has to reach the Spinifex STS endpoint. Keep `AWS_PROFILE=spinifex` exported for both applies.

<!-- INCLUDE: workloads/main.tf lang:hcl -->

### Step 3. Open the Demo

Run from the cluster module directory (`cd ..` if you're still in `workloads/`):

```bash
tofu output demo_url
```

Open that URL in a browser. You'll see the Spinifex-themed page reporting the **pod, node, cluster, and region** that handled the request. Refresh a few times — with two replicas, the pod name alternates, demonstrating that the Service is load-balancing across the cluster.

### Step 4. Inspect with kubectl (optional)

```bash
aws eks update-kubeconfig --name eks-quickstart --region ap-southeast-2
kubectl get nodes
kubectl get pods -o wide
```

`kubectl get pods -o wide` shows the demo pods and which node each landed on.

### Cleanup

Destroy in reverse — the demo app first (while the cluster is still up), then the cluster:

```bash
cd workloads
tofu destroy
cd ..
tofu destroy
```

## Troubleshooting

### Demo URL Doesn't Load

The page is served from a NodePort on the worker's public IP. Work through the chain:

```bash
# Is the worker running with a public IP?
aws ec2 describe-instances --filters "Name=tag:spinifex:eks-cluster,Values=eks-quickstart" \
  --query 'Reservations[].Instances[].[InstanceId,State.Name,PublicIpAddress]' --output text

# Did the demo pods roll out?
kubectl get pods -o wide

# Is the NodePort rule on the worker SG?
aws ec2 describe-security-groups --filters "Name=group-name,Values=eks-cluster-eks-quickstart-nodegroup-sg" \
  --query 'SecurityGroups[0].IpPermissions'
```

If the pods are `Pending`, the worker may not be `Ready` yet — give it a moment. If they're `ImagePullBackOff`, the worker can't pull from ECR: confirm you built and pushed the image (`tofu output ecr_repository_url`), that the IGW route and the worker's public IP are in place, and that the node role carries `AmazonEC2ContainerRegistryReadOnly`.

### kubernetes provider: connection refused / Unauthorized

The provider runs `aws eks get-token` against the Spinifex STS endpoint. Confirm the AWS CLI is still pointed at Spinifex and the cluster is `ACTIVE`:

```bash
aws sts get-caller-identity
aws eks describe-cluster --name eks-quickstart --query 'cluster.status'
```

If the cluster was still `CREATING` when the provider first tried to connect, just re-run `tofu apply`.

### Cluster Stuck in CREATING

Control-plane bootstrap takes a few minutes. Confirm the underlying VM came up:

```bash
aws eks describe-cluster --name eks-quickstart --query 'cluster.status'
aws ec2 describe-instances --profile spinifex
```

### Provider Connection Refused

```bash
sudo systemctl status spinifex.target
curl -k https://localhost:9999/
```
