---
title: "3-Node EKS behind an HTTPS ALB (ACM TLS)"
description: "Provision an HA-shaped EKS cluster with public + private API endpoints and three workers, deploy a demo app, and publish it through an internet-facing ALB that terminates TLS with an ACM-imported certificate, using Terraform on Spinifex."
category: "Terraform Workbooks"
tags:
  - terraform
  - eks
  - kubernetes
  - acm
  - tls
  - alb
  - elbv2
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

# Terraform: 3-Node EKS behind an HTTPS ALB

> The largest EKS workbook, and the clearest demo. An HA-shaped cluster with public + private API endpoints and three workers, running a demo app published over HTTPS through an internet-facing ALB that terminates TLS with an ACM-imported certificate.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

This workbook is sized for a **3-node Spinifex deployment** and is built to show off. It brings together the pieces from the smaller EKS examples, deploys a demo app across three workers, and fronts it with a TLS-terminating load balancer. When `apply` finishes, fetch the ALB's public IP and open `https://<ip>` — the `nginxdemos/hello` page reports which pod answered, and refreshing moves between the three replicas spread across the cluster. That single page demonstrates a multi-node Kubernetes cluster load-balancing behind HTTPS.

- **Public + private API endpoints.** `endpoint_public_access` serves `kubectl` and the Kubernetes provider from your host (CIDR-restricted); `endpoint_private_access` exposes an in-VPC private endpoint the workers use to join.
- **Private workers + NAT.** The three workers sit in private subnets. They *join* the cluster over the in-VPC private endpoint, and a NAT gateway gives them outbound access to pull the demo container image.
- **CoreDNS** installed as a managed addon.
- **TLS via ACM.** A self-signed certificate is generated with the `tls` provider and **imported into ACM** (Spinifex ACM supports `ImportCertificate`, not `RequestCertificate`). The ALB's HTTPS listener references the ACM ARN and forwards to the workers' NodePort.
- **The app, deployed by Terraform.** The Kubernetes provider deploys the Deployment + NodePort Service, and one security-group rule lets the ALB reach the NodePort.

<p align="center">
  <img src="../../../.github/assets/diagrams/tf-eks-https-ingress.svg" alt="3-node EKS — public+private API endpoints, ALB in public subnets terminating TLS, three workers in private subnets behind a NAT gateway" width="900">
</p>

**What you'll learn:**

- Running an EKS cluster with both public and private API endpoints
- Placing workers in private subnets that join over the private endpoint and egress via NAT
- Importing a certificate into ACM and terminating TLS on an ALB HTTPS listener
- Wiring an ALB target group to managed node-group workers via a NodePort, including the SG rule that allows it
- Deploying a load-balanced workload with the Kubernetes provider

**What gets created**

| Resource | Name | Purpose |
|---|---|---|
| VPC | `eks-https-vpc` | Isolated network (10.32.0.0/16) |
| Public Subnets | `eks-https-public-a/-b` | Host the ALB and NAT gateway |
| Private Subnets | `eks-https-private-a/-b` | Host the workers |
| Internet Gateway | `eks-https-igw` | Internet for the public subnets |
| NAT Gateway (+ EIP) | `eks-https-nat` | Outbound for the private workers (image pulls) |
| Route Tables | `eks-https-public-rt`, `eks-https-private-rt` | Public via IGW; private via NAT |
| IAM Roles | `eks-https-cluster-role`, `eks-https-node-role` | Control-plane and worker roles |
| EKS Cluster | `eks-https` | Public (CIDR-restricted) + private endpoints |
| Node Group | `workers` | Three `t3.large` workers in the private subnets |
| Addon | `coredns` | Cluster DNS |
| ACM Cert | `eks-https-ingress` | Self-signed cert imported into ACM |
| ALB + HTTPS Listener | `eks-https-alb` | Internet-facing, terminates TLS on :443 |
| Target Group | `eks-https-tg` | Instance targets on the NodePort |
| SG Ingress Rule | `eks-https-alb-nodeport` | Lets the ALB SG reach the worker NodePort |
| K8s Deployment + Service | `hello` | `nginxdemos/hello`, 3 replicas, NodePort |

**Spinifex specifics**

- ACM supports `ImportCertificate`, `DescribeCertificate`, `ListCertificates`, `DeleteCertificate` — there is no `RequestCertificate`/DNS-validation flow, hence the self-signed import.
- The ALB HTTPS listener resolves the cert from ACM. Valid SSL policies are `ELBSecurityPolicy-2016-08` (default) and `ELBSecurityPolicy-TLS13-1-2-2021-06`.
- The worker SG is auto-managed (`vpc_config.security_group_ids` is ignored) and admits only intra-cluster traffic. The ALB→NodePort rule is added by looking the SG up by its deterministic name and admitting the ALB SG.
- Worker discovery uses the `spinifex:eks-cluster` tag Spinifex stamps on node-group instances; the target-group attachment count is the known `node_desired_size`.
- Leave `addon_version` unset (see the addons workbook for why pinning it can't satisfy both the provider and the catalog).

**Prerequisites:**

- A 3-node Spinifex deployment, installed and running
- The Spinifex `eks-node` image available on the cluster
- OpenTofu or Terraform, plus `kubectl` and the AWS CLI

## Instructions

### Step 1. Get the Template

```bash
git clone --depth 1 --filter=blob:none --sparse https://github.com/mulgadc/spinifex.git spinifex-tf
cd spinifex-tf
git sparse-checkout set docs/terraform-workbooks
cd docs/terraform-workbooks/eks-https-ingress
```

Or create a `main.tf` file and paste the full configuration below.

<!-- INCLUDE: main.tf lang:hcl -->

### Step 2. Deploy

The cluster (with its ALB and ACM cert) and the demo app are **two root modules with separate state**. Apply the cluster first:

```bash
export AWS_PROFILE=spinifex
tofu init
tofu apply
```

Expect several minutes: the control plane bootstraps, three workers launch and join over the private endpoint, the NAT gateway comes up, CoreDNS installs, and the ALB is provisioned. Then deploy the demo app from the nested `workloads/` module:

```bash
cd workloads
tofu init
tofu apply
```

The ALB target group already points at the workers' NodePort; once these pods are running the targets turn healthy.

> **Why two modules?** The Kubernetes provider in `workloads/` reads the cluster endpoint from a live `data "aws_eks_cluster"` source, so it's only ever configured while the cluster exists. Keeping it out of the cluster module means `destroy` never tries to refresh a workload against a cluster that's already gone — the failure mode where the provider falls back to `http://localhost:80` and reports `connection refused`. Always destroy `workloads/` before the cluster.

> **Keep `AWS_PROFILE=spinifex` exported** for both applies — the Kubernetes provider authenticates with `aws eks get-token` against the Spinifex STS endpoint.

<!-- INCLUDE: workloads/main.tf lang:hcl -->

> **Tighten access in production.** `api_public_access_cidr` and `alb_ingress_cidr` both default to `0.0.0.0/0`. Set them to your own CIDR:
>
> ```bash
> export TF_VAR_api_public_access_cidr="203.0.113.10/32"
> export TF_VAR_alb_ingress_cidr="203.0.113.10/32"
> ```

### Step 3. Open the Demo over HTTPS

The ALB DNS name (`*.elb.spinifex.local`) won't resolve from your host, so fetch its public IP:

```bash
ALB_IP=$(aws elbv2 describe-load-balancers --names eks-https-alb \
  --query 'LoadBalancers[0].AvailabilityZones[].LoadBalancerAddresses[].IpAddress' \
  --output text)
```

The certificate is self-signed, so a browser will warn (proceed anyway) and `curl` needs `-k`:

```bash
curl -k https://$ALB_IP
curl -k https://$ALB_IP   # refresh: the "Server name" changes between pods
```

Open `https://$ALB_IP` in a browser and refresh to watch requests land on different pods across the three nodes.

### Step 4. Inspect (optional)

```bash
aws eks update-kubeconfig --name eks-https --region ap-southeast-2
kubectl get nodes
kubectl get pods -o wide          # three replicas, one per node

TG_ARN=$(aws elbv2 describe-target-groups --names eks-https-tg \
  --query 'TargetGroups[0].TargetGroupArn' --output text)
aws elbv2 describe-target-health --target-group-arn "$TG_ARN"
```

### Cleanup

Destroy in reverse — the demo app first (while the cluster is still up), then the cluster:

```bash
cd workloads
tofu destroy
cd ..
tofu destroy
```

## Troubleshooting

### Targets Unhealthy / ALB Returns 5xx

The ALB health-checks `/` on the NodePort. Targets turn healthy once the demo pods are running *and* the SG rule admits the ALB. Check, in order:

```bash
kubectl get pods -o wide                                  # pods Running?
aws ec2 describe-security-groups \
  --filters "Name=group-name,Values=eks-cluster-eks-https-nodegroup-sg" \
  --query 'SecurityGroups[0].IpPermissions'               # NodePort rule present?
aws elbv2 describe-target-health --target-group-arn "$TG_ARN"
```

If pods are `ImagePullBackOff`, the workers can't reach Docker Hub — confirm the NAT gateway is `available`:

```bash
aws ec2 describe-nat-gateways --query 'NatGateways[].[NatGatewayId,State]'
```

### Workers Never Join

The workers join over the private endpoint. Confirm it's enabled and the node group is progressing:

```bash
aws eks describe-cluster --name eks-https --query 'cluster.resourcesVpcConfig.endpointPrivateAccess'
aws eks describe-nodegroup --cluster-name eks-https --nodegroup-name workers --query 'nodegroup.status'
```

### Target-Group Attachment Errors at Apply

The attachment indexes the discovered worker IDs by `node_desired_size`. If `apply` errors with an index out of range, the workers weren't all tagged/running when the `aws_instances` lookup ran. Re-run `tofu apply` — the node group will be `ACTIVE` by then:

```bash
aws ec2 describe-instances --filters "Name=tag:spinifex:eks-cluster,Values=eks-https" \
  --query 'Reservations[].Instances[].[InstanceId,State.Name]' --output text
```

### kubernetes provider: connection refused / Unauthorized

The provider runs `aws eks get-token` against the Spinifex STS endpoint. If the cluster was still `CREATING` when the provider first connected, re-run `tofu apply`. Otherwise confirm:

```bash
aws sts get-caller-identity
aws eks describe-cluster --name eks-https --query 'cluster.status'
```

### ACM Import / HTTPS Listener Errors

```bash
aws acm list-certificates
aws acm describe-certificate --certificate-arn "$(tofu output -raw certificate_arn)" \
  --query 'Certificate.Status'
```

A `CertificateNotFound` on the listener means the ACM ARN didn't resolve — check the cert exists and belongs to the same account/profile.

### Provider Connection Refused

```bash
sudo systemctl status spinifex.target
curl -k https://localhost:9999/
```
