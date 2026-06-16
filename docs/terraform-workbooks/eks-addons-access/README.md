---
title: "EKS with Addons + Access-Entry RBAC"
description: "Extend a single-node EKS cluster with a managed Argo CD addon, access-entry RBAC, and a browsable load-balanced demo app, using Terraform on Spinifex."
category: "Terraform Workbooks"
tags:
  - terraform
  - eks
  - kubernetes
  - addons
  - rbac
  - iam
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

# Terraform: EKS with Addons + Access-Entry RBAC

> Builds on the quickstart by installing a managed addon (Argo CD) and granting a second IAM principal read-only cluster access — then deploys the same browsable, load-balanced demo app so you can see the result.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

This workbook takes the same single-node cluster (and the same browsable demo app) from [EKS Quickstart](../eks-quickstart) and adds the two things you reach for on almost any real cluster:

1. **A managed addon.** Argo CD is installed through the EKS API (`aws_eks_addon`) rather than `kubectl apply`, so its lifecycle is managed by the cluster. Spinifex stages the addon's manifests host-side, then the worker renders them into the K3s auto-deploy dir. The demo app is deployed *after* the addon reaches `ACTIVE`, so a healthy page also confirms the addon delivery pipeline worked end to end.
2. **Access-entry RBAC.** A second IAM role is granted read-only access via an `aws_eks_access_entry` plus an `aws_eks_access_policy_association` bound to the AWS-managed `AmazonEKSViewPolicy`. This is the modern access-entry path (`authentication_mode = "API"`) — no `aws-auth` ConfigMap.

When `apply` finishes, open the **`demo_url`** output and refresh — the `nginxdemos/hello` page reports which pod served each request.

**What you'll learn:**

- Installing a managed EKS addon with `aws_eks_addon`
- Registering an IAM principal with `aws_eks_access_entry` and scoping it with `aws_eks_access_policy_association`
- The difference between the cluster creator's admin access and a scoped viewer
- Deploying a workload with the Kubernetes provider once the addon is healthy

**What gets created**

| Resource | Name | Purpose |
|---|---|---|
| VPC | `eks-addons-vpc` | Isolated network (10.31.0.0/16) |
| Subnets | `eks-addons-subnet-a/-b` | Public subnets for the cluster and worker |
| Internet Gateway | `eks-addons-igw` | Egress so the worker can pull the demo image |
| IAM Roles | `eks-addons-cluster-role`, `eks-addons-node-role` | Control-plane and worker roles |
| IAM Role | `eks-addons-viewer` | Stand-in principal granted read-only access |
| EKS Cluster | `eks-addons` | Public endpoint, API auth mode |
| Node Group | `default` | One `t3.medium` worker |
| Addon | `argocd` | GitOps CD, managed via the EKS API |
| Access Entry + Assoc. | `eks-addons-viewer` → `AmazonEKSViewPolicy` | Read-only, cluster scope |
| SG Ingress Rule | `eks-addons-demo-nodeport` | Opens the demo NodePort on the worker SG |
| K8s Deployment + Service | `hello` | `nginxdemos/hello`, 2 replicas, NodePort |

**Spinifex specifics**

- Addon catalog is limited. Spinifex currently serves `argocd`, `coredns`, `aws-load-balancer-controller` and `spinifex-noop`. There is no `vpc-cni`, `kube-proxy`, `ebs-csi` or `metrics-server`. Only addons with a baked bundle shipped in the `eks-node` AMI install successfully — `argocd` and `spinifex-noop` are the bundles shipped today.
- **Leave `addon_version` unset.** Spinifex defaults an unset version to its catalog default (`argocd` 3.0.23). Pinning is a dead end: the AWS provider rejects a bare `3.0.23` (it requires the `v`-prefixed `v3.0.23-eksbuild.N` form), while Spinifex's catalog only accepts the bare `3.0.23` — so no single string satisfies both.
- Cluster access policies are the four AWS-managed `cluster-access-policy/Amazon EKS{ClusterAdmin,Admin,Edit,View}Policy` ARNs.
- The worker SG is auto-managed and the worker AMI is always the `eks-node` image; the NodePort is exposed by adding one rule to the looked-up worker SG.

**Prerequisites:**

- Spinifex installed and running
- The Spinifex `eks-node` image available on the cluster
- OpenTofu or Terraform, plus `kubectl` and the AWS CLI

## Instructions

### Step 1. Get the Template

```bash
git clone --depth 1 --filter=blob:none --sparse https://github.com/mulgadc/spinifex.git spinifex-tf
cd spinifex-tf
git sparse-checkout set docs/terraform-workbooks
cd docs/terraform-workbooks/eks-addons-access
```

Or create a `main.tf` file and paste the full configuration below.

<!-- INCLUDE: main.tf lang:hcl -->

### Step 2. Deploy

```bash
export AWS_PROFILE=spinifex
tofu init
tofu apply
```

Allow a few minutes for the cluster to reach `ACTIVE` and the worker to register. Argo CD installs onto the worker, then Terraform deploys the demo app.

> Keep `AWS_PROFILE=spinifex` exported for the whole `apply` — the Kubernetes provider runs `aws eks get-token` against the Spinifex STS endpoint.

### Step 3. Open the Demo

```bash
tofu output demo_url
```

Open it in a browser and refresh — the `nginxdemos/hello` page shows the pod that answered, alternating between the two replicas.

### Step 4. Verify the Addon and Access Entry

```bash
aws eks list-addons --cluster-name eks-addons
aws eks describe-addon --cluster-name eks-addons --addon-name argocd --query 'addon.status'

aws eks list-access-entries --cluster-name eks-addons
aws eks list-associated-access-policies \
  --cluster-name eks-addons \
  --principal-arn "$(tofu output -raw viewer_principal_arn)"
```

The viewer principal is bound to `AmazonEKSViewPolicy` at cluster scope — it can read across the cluster but cannot mutate anything. The principal that ran `apply` keeps full cluster-admin from `bootstrap_cluster_creator_admin_permissions`.

### Cleanup

```bash
tofu destroy
```

## Troubleshooting

### Demo URL Doesn't Load

Same chain as the quickstart — check the worker's public IP, that the demo pods are `Running`, and that the NodePort rule landed on the worker SG:

```bash
kubectl get pods -o wide
aws ec2 describe-security-groups --filters "Name=group-name,Values=eks-cluster-eks-addons-nodegroup-sg" \
  --query 'SecurityGroups[0].IpPermissions'
```

If the addon never reaches `ACTIVE`, confirm Argo CD's pods came up on the worker:

```bash
kubectl -n argocd get pods
kubectl -n argocd get deploy
```

### Addon Stuck in CREATING / DEGRADED

Argo CD needs a schedulable worker with headroom (it runs ~6 pods). Confirm the node group is `ACTIVE` and the node is `Ready`:

```bash
aws eks describe-nodegroup --cluster-name eks-addons --nodegroup-name default --query 'nodegroup.status'
kubectl get nodes
```

### Addon Version Rejected

This workbook leaves `addon_version` unset so Spinifex picks its catalog default. If you add an explicit `addon_version`, expect a wall: the AWS provider requires the `v`-prefixed `v3.0.23-eksbuild.N` form, but Spinifex's catalog only knows the bare `3.0.23` — a value that passes one is rejected by the other. Prefer omitting the field.

### Addon Fails With "no baked bundle"

Spinifex only installs addons whose manifests are baked into the `eks-node` AMI. If `describe-addon` reports `CREATE_FAILED` with `no baked bundle for version <v>`, the running worker image predates that addon. Rebuild and republish the `eks-node` image so the bundle ships, then recreate the addon.

### Access Entry Not Taking Effect

Assume the viewer role and confirm it can read but not write:

```bash
kubectl auth can-i list pods --all-namespaces   # yes
kubectl auth can-i create deployments           # no
```

### Provider Connection Refused

```bash
sudo systemctl status spinifex.target
curl -k https://localhost:9999/
```
