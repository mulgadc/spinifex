---
title: "GitOps on EKS (Argo CD + EBS-CSI)"
description: "Deliver a Spinifex-themed app to EKS with GitOps — the Argo CD addon syncs it from a git repo, an EBS-CSI (Viperblock) PersistentVolume holds its state, and it is served over HTTPS through the AWS Load Balancer Controller + ACM, using Terraform on Spinifex."
category: "Terraform Workbooks"
tags:
  - terraform
  - eks
  - kubernetes
  - argocd
  - gitops
  - ebs-csi
  - storage
  - workbook
resources:
  - title: "Argo CD"
    url: "https://argo-cd.readthedocs.io/"
  - title: "AWS Load Balancer Controller"
    url: "https://kubernetes-sigs.github.io/aws-load-balancer-controller/latest/"
  - title: "Terraform AWS Provider"
    url: "https://registry.terraform.io/providers/hashicorp/aws/latest"
  - title: "Terraform Kubernetes Provider"
    url: "https://registry.terraform.io/providers/hashicorp/kubernetes/latest"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
---

# Terraform: GitOps on EKS (Argo CD + EBS-CSI)

> The top rung of the EKS ladder. It extends [EKS HTTPS Ingress](../eks-https-ingress) and changes how the app is delivered: the **Argo CD** addon syncs a more elaborate Spinifex-themed app from a **git repo**, an **EBS-CSI** PersistentVolume (Viperblock-backed) holds its state, and it is still served over **HTTPS** through the AWS Load Balancer Controller + ACM.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

This workbook keeps everything from [EKS HTTPS Ingress](../eks-https-ingress) — the public/private VPC, the NAT gateway, the LBC addon, the ACM cert, the HTTPS Ingress — and adds the parts you reach for once an app is real: **GitOps delivery** and **persistent storage**.

Three changes over the previous rung:

1. **Argo CD delivers the app.** Instead of Terraform applying the Deployment, the **argocd** addon syncs the app from a git repository ([`mulgadc/eks-demo-app`](https://github.com/mulgadc/eks-demo-app)). Terraform only registers the repo credential and creates the Argo CD `Application`; Argo CD reconciles the manifests and self-heals drift.
2. **State lives on an EBS volume.** The **aws-ebs-csi-driver** addon ships a default gp3 StorageClass. The app's `PersistentVolumeClaim` (in the git repo) dynamically provisions a **Viperblock-backed EBS volume**; the demo's hit counter persists to it and survives pod restarts.
3. **Access-entry RBAC.** A second IAM principal is granted read-only cluster access via an access entry bound to `AmazonEKSViewPolicy`.

Terraform manages the cluster, the addons, the ACM cert, and the HTTPS Ingress; **Argo CD** manages the app's Deployment, Service, and PVC from git. The Ingress points at the Service Argo CD creates, so the app is reachable over HTTPS the moment Argo CD finishes its first sync.

**What you'll learn:**

- Installing the `argocd` and `aws-ebs-csi-driver` addons through the EKS API
- Registering a private git repo with Argo CD and driving an `Application` from Terraform
- Dynamically provisioning a Viperblock-backed EBS volume with a `PersistentVolumeClaim`
- Splitting ownership: Terraform owns infra + Ingress, Argo CD owns the workload
- Granting scoped read-only access with an EKS access entry

**What gets created**

| Resource | Name | Purpose |
|---|---|---|
| VPC + subnets | `eks-gitops-*` | Public/private network (10.32.0.0/16) with a NAT gateway |
| IAM Roles | `eks-gitops-cluster-role`, `-node-role`, `-viewer` | Control-plane, worker, and read-only viewer roles |
| ECR Repository | `spinifex-demo` | Holds the demo image the workers pull |
| EKS Cluster | `eks-gitops` | Public + private endpoints; `managed-ingress=false` |
| Node Group | `workers` | `node_desired_size` `t3.large` worker(s) — 1 or 3 |
| Addons | `aws-load-balancer-controller`, `argocd`, `aws-ebs-csi-driver` | Ingress, GitOps delivery, persistent storage |
| ACM Certificate | `eks-gitops-ingress` | Self-signed, imported; on the HTTPS listener |
| Access Entry + Assoc. | `eks-gitops-viewer` → `AmazonEKSViewPolicy` | Read-only, cluster scope |
| Argo CD repo Secret | `eks-demo-app-repo` | Credential for the private git repo |
| Argo CD Application | `spinifex-demo` | Syncs the app from git |
| K8s Ingress | `spinifex-demo` | `ingressClassName: alb`, HTTPS via the ACM cert |
| Git-managed (by Argo CD) | Deployment, Service, **PVC** | The app + its Viperblock-backed volume |

**Spinifex specifics**

- The `argocd` and `aws-ebs-csi-driver` bundles must be baked into the `eks-node` AMI — `describe-addon` returns `no baked bundle` otherwise.
- The EBS-CSI default StorageClass uses `provisioner: ebs.csi.aws.com`; a PVC against it provisions a **Viperblock-backed EBS volume**.
- **Verify dynamic provisioning on a live cluster.** EBS-CSI dynamic provisioning depends on the running Spinifex EBS/Viperblock data plane and may not be available on every build yet. If the PVC stays `Pending`, the app still serves (the counter falls back to in-memory) — confirm the volume binds on a stack where the driver is healthy.
- Leave addon versions unset (the AWS provider and the catalog disagree on the version-string format).
- The Argo CD `Application` is a `kubernetes_manifest`, so the `argoproj.io` CRDs must exist before `apply` — apply the parent and let the addon reach `ACTIVE` first.

**Prerequisites:**

- Spinifex running with an `eks-node` image carrying the LBC, Argo CD, and EBS-CSI bundles
- OpenTofu or Terraform, plus `kubectl`, the AWS CLI, and Docker
- The demo image built and pushed to ECR (see [`../demo-app/README.md`](../demo-app/README.md))
- A git repo with the app manifests ([`mulgadc/eks-demo-app`](https://github.com/mulgadc/eks-demo-app)); a read-only PAT if it is private

## Instructions

### Step 1. Get the Template

```bash
git clone --depth 1 --filter=blob:none --sparse https://github.com/mulgadc/spinifex.git spinifex-tf
cd spinifex-tf
git sparse-checkout set docs/terraform-workbooks
cd docs/terraform-workbooks/eks-gitops-argocd
```

<!-- INCLUDE: main.tf lang:hcl -->

### Step 2. Deploy the Cluster + Addons

```bash
export AWS_PROFILE=spinifex
tofu init
tofu apply        # add -var node_desired_size=3 for an HA-shaped cluster
```

Wait for the cluster to reach `ACTIVE` and for all three addons (`aws-load-balancer-controller`, `argocd`, `aws-ebs-csi-driver`) to report healthy:

```bash
aws eks list-addons --cluster-name eks-gitops
aws eks describe-addon --cluster-name eks-gitops --addon-name argocd --query 'addon.status'
```

### Step 3. Build and Push the Demo Image

```bash
cd ../demo-app
REGISTRY=$(cd ../eks-gitops-argocd && tofu output -raw ecr_repository_url)
REGISTRY_HOST=${REGISTRY%%/*}
aws ecr get-login-password | docker login --username AWS --password-stdin "$REGISTRY_HOST"
docker build -t "${REGISTRY}:latest" .
docker push "${REGISTRY}:latest"
cd ../eks-gitops-argocd
```

Point the app's manifests at this image: set the image ref in `eks-demo-app/manifests` (a kustomize `images:` override) to your `${REGISTRY}:latest`, and push that to the git repo.

### Step 4. Hand Delivery to Argo CD

```bash
cd workloads
tofu init
tofu apply \
  -var git_repo_url=https://github.com/mulgadc/eks-demo-app.git \
  -var git_token=<read-only-PAT>     # omit for a public repo
```

This registers the repo credential, creates the Argo CD `Application`, the demo app's HTTPS Ingress, and an HTTPS Ingress for the Argo CD UI itself. Argo CD then syncs the Deployment, Service, and PVC from git.

<!-- INCLUDE: workloads/main.tf lang:hcl -->

### Step 5. Watch the Sync and Open the App

The demo app and the Argo CD UI share **one ALB** (an LBC IngressGroup). On
`:443` the app is the catch-all and `argocd.eks-gitops.spinifex.local`
host-routes to the UI; Argo CD also gets a hostless `:8443` listener so it is
reachable on the raw ALB IP before DNS exists. Grab the shared ALB address once:

```bash
kubectl -n argocd get applications
kubectl get pods -o wide
kubectl get pvc                    # the EBS-CSI volume should be Bound

ALB_IP=$(kubectl get ingress spinifex-demo -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
echo "$ALB_IP"
```

**Reach it by raw IP (no DNS, no host header):**

| Service | URL |
|---|---|
| Demo app | `https://<ALB_IP>/` |
| Argo CD UI | `https://<ALB_IP>:8443/` |

```bash
curl -k https://"$ALB_IP"/            # app — returns the Spinifex page
curl -k https://"$ALB_IP":8443/       # Argo CD UI
```

The bare ALB IP serves the **app** on `:443`. Hitting `https://<ALB_IP>/` and
expecting Argo CD gives the app instead — use `:8443` for the UI. Once northstar
(or Route 53) resolves the hostnames to the ALB, `app.eks-gitops.spinifex.local`
and `argocd.eks-gitops.spinifex.local` both work on `:443`. Self-signed cert —
accept the browser warning.

The page shows the **"persisted to EBS volume"** badge and a hit counter that keeps climbing — delete the pod (`kubectl delete pod -l app=spinifex-demo`) and the count survives, proving the volume is durable.

### Step 6. Open the Argo CD UI

Managing deployments through the Argo CD console is the point of this workbook, so
the UI is exposed on the **same ALB** as the app, not behind a port-forward. The
`workloads` module adds a `NodePort` Service in front of `argocd-server` (the addon
ships it `ClusterIP` only) and two Ingresses in the shared group — a host-routed
one on `:443` and a hostless one on `:8443` for raw-IP access. `argocd-server`
serves TLS on its own port, so both use `backend-protocol: HTTPS`.

**Get the admin credentials.** Argo CD generates a one-time `admin` password into
a Secret on install; the username is always `admin`:

```bash
kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d ; echo
```

(`tofu output argocd_admin_password_cmd` prints this same command.)

Open the UI and log in as `admin`:

- By raw IP: `https://<ALB_IP>:8443/`
- By DNS once wired: `https://argocd.eks-gitops.spinifex.local`

The `spinifex-demo` Application shows the sync status, the resource tree, and live
diffs against git — change a manifest in the repo and watch Argo CD reconcile it.

If you'd rather not expose the UI at all, port-forward instead:

```bash
kubectl -n argocd port-forward svc/argocd-server 8080:443
# then open https://localhost:8080
```

### Cleanup

Destroy in reverse — the Argo CD Application + Ingress first (so Argo CD prunes the workload and the LBC tears the ALB down), then the cluster:

```bash
cd workloads
tofu destroy
cd ..
tofu destroy
```

## Troubleshooting

### Application Won't Sync

```bash
kubectl -n argocd get applications
kubectl -n argocd describe application spinifex-demo     # conditions show repo/auth errors
kubectl -n argocd logs deploy/argocd-repo-server --tail=50
```

For a private repo, confirm the `eks-demo-app-repo` Secret exists in the `argocd` namespace with the `argocd.argoproj.io/secret-type=repository` label and a valid token.

### `kubernetes_manifest` Fails: no matches for kind "Application"

The `argoproj.io` CRDs aren't installed yet. Apply the parent module and wait for the `argocd` addon to reach `ACTIVE` before applying `workloads/`.

### PVC Stuck in Pending

```bash
kubectl get pvc
kubectl describe pvc <name>
kubectl get storageclass
```

Confirm the `aws-ebs-csi-driver` addon is `ACTIVE` and the default StorageClass exists. EBS-CSI dynamic provisioning depends on the live Spinifex EBS/Viperblock data plane — verify on a stack where the driver is healthy. The app still serves with an in-memory counter if the volume never binds.

### The Ingress Has No Address

The LBC populates `status.loadBalancer` once it provisions the ALB. Check the addon and the controller logs:

```bash
kubectl describe ingress spinifex-demo
kubectl -n kube-system logs deploy/aws-load-balancer-controller --tail=50
```

Confirm the cluster carries `spinifex.io/managed-ingress = "false"` so the LBC owns ingress.

If the controller logs show `FailedBuildModel ... DescribeAvailabilityZones ...
403 ... AccessDenied`, the node role is missing the LBC permissions. The
controller runs with the node instance-profile credentials, so the
`${var.cluster_name}-node-lbc` policy (`aws_iam_policy.node_lbc`) must be
attached to the node role — re-run `tofu apply` on the parent module if it was
provisioned before that policy existed.

### Provider Connection Refused

```bash
sudo systemctl status spinifex.target
curl -k https://localhost:9999/
```
