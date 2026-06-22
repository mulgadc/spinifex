# Spinifex EKS demo app

A small, Spinifex-themed web app shared by the EKS Terraform workbooks. It
replaces `nginxdemos/hello` with something production-shaped that you build
yourself and push to **ECR**, then pull onto your EKS workers.

The page reports the pod, node, cluster, and region that answered the request,
plus a hit counter — refresh and watch requests land on different pods across a
multi-node cluster.

## What it does

- `GET /` — themed HTML page.
- `GET /api/info` — JSON of the same facts.
- `GET /healthz` — liveness/readiness probe (`200 ok`), used by the ALB target
  group health check.

All configuration is read from the environment, so one image serves every tier:

| Variable | Purpose | Source |
|---|---|---|
| `POD_NAME`, `NODE_NAME`, `POD_NAMESPACE` | identity shown on the page | downward API |
| `CLUSTER_NAME`, `AWS_REGION` | cluster/region labels | deployment env |
| `APP_TITLE` | page heading | deployment env (default `Spinifex EKS`) |
| `PORT` | listen port | default `8080` |
| `DATA_DIR` | when set, the hit counter is persisted here and survives pod restarts | mounted **EBS-CSI PVC** in `eks-gitops-argocd` |

When `DATA_DIR` points at a writable volume the page shows a **"persisted to EBS
volume"** badge — the gitops workbook mounts a Viperblock-backed PersistentVolume
there to demonstrate durable storage.

## Build and push to ECR

The image is named `spinifex-demo`. Build it once and push it to a repository in
your Spinifex ECR; every workbook references it by its ECR URI.

```bash
export AWS_PROFILE=spinifex
REGION=ap-southeast-2

# 1. Create the repository (the workbooks also create it via Terraform; skip if
#    it already exists).
aws ecr create-repository --repository-name spinifex-demo --region "$REGION"

# 2. Resolve the registry host and authenticate Docker.
REGISTRY=$(aws ecr describe-repositories --repository-names spinifex-demo \
  --region "$REGION" --query 'repositories[0].repositoryUri' --output text)
REGISTRY_HOST=${REGISTRY%%/*}
aws ecr get-login-password --region "$REGION" \
  | docker login --username AWS --password-stdin "$REGISTRY_HOST"

# 3. Build, tag, and push.
docker build -t spinifex-demo:latest .
docker tag spinifex-demo:latest "$REGISTRY:latest"
docker push "$REGISTRY:latest"

echo "Image: $REGISTRY:latest"
```

Pass the resulting `$REGISTRY:latest` value to a workbook as the `demo_image`
variable (or accept its default, which resolves the URI from the ECR repository
the workbook creates).

## Run locally

```bash
go run .            # http://localhost:8080
DATA_DIR=/tmp/demo go run .   # exercise the persisted-counter path
```
