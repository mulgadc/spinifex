# Example: GitOps on EKS with Argo CD + a persistent EBS-CSI volume
#
# The top rung of the EKS ladder. It extends eks-https-ingress (LBC + ACM HTTPS)
# and changes how the app is delivered: instead of Terraform applying the
# workload, the Argo CD addon syncs a more elaborate Spinifex-themed app from a
# git repository, and the app stores state on a Viperblock-backed EBS volume
# provisioned dynamically through the EBS-CSI driver.
#
# What it adds over eks-https-ingress:
#   * The argocd addon — GitOps continuous delivery, installed via the EKS API.
#   * The aws-ebs-csi-driver addon — dynamic EBS (Viperblock) PersistentVolumes.
#   * An access entry granting a second IAM principal read-only cluster access.
#
# The workloads/ module registers the (private) git repo with Argo CD, creates an
# Argo CD Application that syncs the app from it, and keeps the HTTPS Ingress
# (LBC + ACM) pointing at the git-managed Service. The app's manifests — including
# the PersistentVolumeClaim — live in the git repo (see ../../../../eks-demo-app).
#
# Usage:
#   cd spinifex/docs/terraform-workbooks/eks-gitops-argocd
#   export AWS_PROFILE=spinifex
#   tofu init && tofu apply
#   # build + push the demo image to the ECR repo this creates (see README),
#   # then: cd workloads && tofu init && tofu apply -var git_repo_url=... -var git_token=...
#   # finally: kubectl get ingress spinifex-demo -o wide  → open https://<address>

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.40, < 6.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = ">= 4.0"
    }
  }
}

# ---------------------------------------------------------------------------
# Variables
# ---------------------------------------------------------------------------

variable "region" {
  type    = string
  default = "ap-southeast-2"
}

variable "cluster_name" {
  type    = string
  default = "eks-gitops"
}

variable "k8s_version" {
  type    = string
  default = "1.32"
}

variable "node_instance_type" {
  type    = string
  default = "t3.large"
}

variable "node_desired_size" {
  type        = number
  default     = 1
  description = "Worker count. Use 1 for a single-node demo, or 3 for an HA-shaped cluster."

  validation {
    condition     = var.node_desired_size == 1 || var.node_desired_size == 3
    error_message = "node_desired_size must be 1 or 3."
  }
}

variable "node_port" {
  type        = number
  default     = 30080
  description = "NodePort the demo Service is published on and the ALB forwards to"
}

variable "cert_common_name" {
  type    = string
  default = "eks-gitops.spinifex.local"
}

variable "api_public_access_cidr" {
  type        = string
  default     = "0.0.0.0/0"
  description = "CIDR allowed to reach the public Kubernetes API endpoint; tighten in production"
}

variable "spinifex_endpoint" {
  type    = string
  default = "https://127.0.0.1:9999"
}

# ---------------------------------------------------------------------------
# Providers
# ---------------------------------------------------------------------------

provider "aws" {
  region = var.region

  endpoints {
    ec2 = var.spinifex_endpoint
    iam = var.spinifex_endpoint
    sts = var.spinifex_endpoint
    eks = var.spinifex_endpoint
    ecr = var.spinifex_endpoint
    acm = var.spinifex_endpoint
  }

  skip_credentials_validation = true
  skip_metadata_api_check     = true
  skip_requesting_account_id  = true
  skip_region_validation      = true
}

data "aws_availability_zones" "available" {
  state = "available"
}

# ---------------------------------------------------------------------------
# VPC — public subnets for the ALB + NAT, private subnets for the workers
# ---------------------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = "10.32.0.0/16"
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = {
    Name = "${var.cluster_name}-vpc"
  }
}

resource "aws_internet_gateway" "igw" {
  vpc_id = aws_vpc.main.id

  tags = {
    Name = "${var.cluster_name}-igw"
  }
}

resource "aws_subnet" "public_a" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.32.1.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = {
    Name = "${var.cluster_name}-public-a"
  }
}

resource "aws_subnet" "public_b" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.32.2.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = {
    Name = "${var.cluster_name}-public-b"
  }
}

resource "aws_subnet" "private_a" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.32.11.0/24"
  availability_zone = data.aws_availability_zones.available.names[0]

  tags = {
    Name = "${var.cluster_name}-private-a"
  }
}

resource "aws_subnet" "private_b" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.32.12.0/24"
  availability_zone = data.aws_availability_zones.available.names[0]

  tags = {
    Name = "${var.cluster_name}-private-b"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.igw.id
  }

  tags = {
    Name = "${var.cluster_name}-public-rt"
  }
}

resource "aws_route_table_association" "public_a" {
  subnet_id      = aws_subnet.public_a.id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table_association" "public_b" {
  subnet_id      = aws_subnet.public_b.id
  route_table_id = aws_route_table.public.id
}

# NAT gateway gives the private workers outbound internet to pull the demo image
# from ECR. The workers still join the cluster over the in-VPC private endpoint.
resource "aws_eip" "nat" {
  domain = "vpc"

  tags = {
    Name = "${var.cluster_name}-nat-eip"
  }
}

resource "aws_nat_gateway" "nat" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public_a.id

  depends_on = [aws_internet_gateway.igw]

  tags = {
    Name = "${var.cluster_name}-nat"
  }
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.nat.id
  }

  tags = {
    Name = "${var.cluster_name}-private-rt"
  }
}

resource "aws_route_table_association" "private_a" {
  subnet_id      = aws_subnet.private_a.id
  route_table_id = aws_route_table.private.id
}

resource "aws_route_table_association" "private_b" {
  subnet_id      = aws_subnet.private_b.id
  route_table_id = aws_route_table.private.id
}

# ---------------------------------------------------------------------------
# IAM — cluster role
# ---------------------------------------------------------------------------

resource "aws_iam_role" "cluster" {
  name = "${var.cluster_name}-cluster-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "eks.amazonaws.com" }
    }]
  })
}

resource "aws_iam_role_policy_attachment" "cluster" {
  role       = aws_iam_role.cluster.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
}

# ---------------------------------------------------------------------------
# IAM — node role
# ---------------------------------------------------------------------------

resource "aws_iam_role" "node" {
  name = "${var.cluster_name}-node-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "ec2.amazonaws.com" }
    }]
  })
}

resource "aws_iam_role_policy_attachment" "node_worker" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy"
}

resource "aws_iam_role_policy_attachment" "node_cni" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
}

resource "aws_iam_role_policy_attachment" "node_ecr" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
}

# ---------------------------------------------------------------------------
# IAM — a second principal to grant read-only cluster access to
# ---------------------------------------------------------------------------

resource "aws_iam_role" "viewer" {
  name = "${var.cluster_name}-viewer"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "ec2.amazonaws.com" }
    }]
  })
}

# ---------------------------------------------------------------------------
# ECR — repository the workers pull the demo image from
# ---------------------------------------------------------------------------

resource "aws_ecr_repository" "demo" {
  name = "spinifex-demo"

  tags = {
    Name = "${var.cluster_name}-demo"
  }
}

# ---------------------------------------------------------------------------
# EKS cluster — public + private endpoints, LBC-owned ingress
#
# The spinifex.io/managed-ingress = "false" tag disables K3s' built-in
# traefik/servicelb so the AWS Load Balancer Controller owns ingress, matching
# how ingress works on AWS EKS.
# ---------------------------------------------------------------------------

resource "aws_eks_cluster" "this" {
  name     = var.cluster_name
  role_arn = aws_iam_role.cluster.arn
  version  = var.k8s_version

  vpc_config {
    subnet_ids              = [aws_subnet.private_a.id, aws_subnet.private_b.id]
    endpoint_public_access  = true
    endpoint_private_access = true
    public_access_cidrs     = [var.api_public_access_cidr]
  }

  access_config {
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = true
  }

  depends_on = [aws_iam_role_policy_attachment.cluster]

  tags = {
    Name                          = var.cluster_name
    "spinifex.io/managed-ingress" = "false"
  }
}

# ---------------------------------------------------------------------------
# Managed node group — workers in the private subnets
# ---------------------------------------------------------------------------

resource "aws_eks_node_group" "workers" {
  cluster_name    = aws_eks_cluster.this.name
  node_group_name = "workers"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = [aws_subnet.private_a.id, aws_subnet.private_b.id]

  scaling_config {
    desired_size = var.node_desired_size
    min_size     = var.node_desired_size
    max_size     = var.node_desired_size * 2
  }

  instance_types = [var.node_instance_type]
  ami_type       = "AL2_x86_64"

  depends_on = [
    aws_iam_role_policy_attachment.node_worker,
    aws_iam_role_policy_attachment.node_cni,
    aws_iam_role_policy_attachment.node_ecr,
  ]

  tags = {
    Name = "${var.cluster_name}-workers"
  }
}

# ---------------------------------------------------------------------------
# Addon — AWS Load Balancer Controller
#
# Spinifex wires the controller's AWS credentials and ELB-eligible subnets at the
# node level, so no IRSA role or subnet tagging is needed here. addon_version is
# omitted: the AWS provider demands a v-prefixed version that the catalog rejects,
# so let Spinifex default to its catalog version.
# ---------------------------------------------------------------------------

resource "aws_eks_addon" "lbc" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "aws-load-balancer-controller"
  resolve_conflicts_on_create = "OVERWRITE"

  depends_on = [aws_eks_node_group.workers]

  tags = {
    Name = "${var.cluster_name}-lbc"
  }
}

# ---------------------------------------------------------------------------
# Addon — Argo CD (GitOps delivery)
#
# Installed through the EKS API; Spinifex stages the bundle host-side and the
# worker renders it into the K3s auto-deploy dir. The workloads module registers
# the git repo and creates the Argo CD Application that syncs the demo app.
# addon_version omitted (see the LBC note).
# ---------------------------------------------------------------------------

resource "aws_eks_addon" "argocd" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "argocd"
  resolve_conflicts_on_create = "OVERWRITE"

  depends_on = [aws_eks_node_group.workers]

  tags = {
    Name = "${var.cluster_name}-argocd"
  }
}

# ---------------------------------------------------------------------------
# Addon — EBS CSI driver (Viperblock-backed PersistentVolumes)
#
# Ships a default gp3 StorageClass (provisioner ebs.csi.aws.com). A PVC in the
# demo app's git manifests dynamically provisions a Viperblock-backed EBS volume
# so the app's state survives pod restarts. addon_version omitted (see LBC note).
# ---------------------------------------------------------------------------

resource "aws_eks_addon" "ebs_csi" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "aws-ebs-csi-driver"
  resolve_conflicts_on_create = "OVERWRITE"

  depends_on = [aws_eks_node_group.workers]

  tags = {
    Name = "${var.cluster_name}-ebs-csi"
  }
}

# ---------------------------------------------------------------------------
# Access entry — grant the viewer principal read-only access (API auth mode)
# ---------------------------------------------------------------------------

resource "aws_eks_access_entry" "viewer" {
  cluster_name  = aws_eks_cluster.this.name
  principal_arn = aws_iam_role.viewer.arn
  type          = "STANDARD"
}

resource "aws_eks_access_policy_association" "viewer" {
  cluster_name  = aws_eks_cluster.this.name
  principal_arn = aws_iam_role.viewer.arn
  policy_arn    = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"

  access_scope {
    type = "cluster"
  }

  depends_on = [aws_eks_access_entry.viewer]
}

# ---------------------------------------------------------------------------
# TLS — self-signed certificate imported into ACM
#
# Spinifex ACM supports ImportCertificate (not RequestCertificate), so the cert
# is generated locally by the tls provider and imported. The workloads Ingress
# attaches it to the ALB's HTTPS listener by ARN.
# ---------------------------------------------------------------------------

resource "tls_private_key" "ingress" {
  algorithm = "RSA"
  rsa_bits  = 2048
}

resource "tls_self_signed_cert" "ingress" {
  private_key_pem = tls_private_key.ingress.private_key_pem

  subject {
    common_name  = var.cert_common_name
    organization = "Spinifex EKS Demo"
  }

  dns_names             = [var.cert_common_name]
  validity_period_hours = 8760
  early_renewal_hours   = 720

  allowed_uses = [
    "key_encipherment",
    "digital_signature",
    "server_auth",
  ]
}

resource "aws_acm_certificate" "ingress" {
  private_key      = tls_private_key.ingress.private_key_pem
  certificate_body = tls_self_signed_cert.ingress.cert_pem

  tags = {
    Name = "${var.cluster_name}-ingress"
  }
}

# ---------------------------------------------------------------------------
# Let the ALB reach the workers' NodePort
#
# Spinifex auto-manages the nodegroup SG and admits only intra-cluster traffic.
# The LBC-provisioned ALB lives in this VPC, so admit the VPC CIDR on the
# NodePort. Look the SG up by its deterministic name and add one rule.
# ---------------------------------------------------------------------------

data "aws_security_group" "nodegroup" {
  filter {
    name   = "group-name"
    values = ["eks-cluster-${var.cluster_name}-nodegroup-sg"]
  }

  filter {
    name   = "vpc-id"
    values = [aws_vpc.main.id]
  }

  depends_on = [aws_eks_node_group.workers]
}

resource "aws_vpc_security_group_ingress_rule" "nodeport_from_vpc" {
  security_group_id = data.aws_security_group.nodegroup.id
  cidr_ipv4         = aws_vpc.main.cidr_block
  from_port         = var.node_port
  to_port           = var.node_port
  ip_protocol       = "tcp"

  tags = {
    Name = "${var.cluster_name}-alb-nodeport"
  }
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------

output "cluster_name" {
  value = aws_eks_cluster.this.name
}

output "region" {
  value = var.region
}

output "node_port" {
  value = var.node_port
}

output "node_desired_size" {
  value = var.node_desired_size
}

output "certificate_arn" {
  value = aws_acm_certificate.ingress.arn
}

output "viewer_principal_arn" {
  value = aws_iam_role.viewer.arn
}

output "ecr_repository_url" {
  value       = aws_ecr_repository.demo.repository_url
  description = "Push the demo-app image here, then apply the workloads module"
}

output "ingress_address_hint" {
  value = "After applying workloads: kubectl get ingress spinifex-demo -o jsonpath='{.status.loadBalancer.ingress[0].hostname}{\"\\n\"}' — then open https://<that-address> (self-signed cert: curl -k)."
}

output "update_kubeconfig" {
  value = "aws eks update-kubeconfig --name ${aws_eks_cluster.this.name} --region ${var.region}"
}
