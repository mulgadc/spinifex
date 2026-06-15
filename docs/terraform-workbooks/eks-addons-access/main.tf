# Example: EKS with an Addon + Access-Entry RBAC (with a visible demo app)
#
# Builds on eks-quickstart: same single-worker cluster and browsable demo app,
# but adds two things you reach for on a real cluster:
#
#   * a managed addon (CoreDNS) installed through the EKS API, and
#   * a second IAM principal granted scoped, read-only access via an EKS access
#     entry + access-policy association (authentication_mode = "API").
#
# After `apply`, open the demo_url output — the page reports the pod that
# served it, so a refresh shows requests landing on different replicas. CoreDNS
# being healthy is what lets in-cluster name resolution work.
#
# Usage:
#   cd spinifex/docs/terraform-workbooks/eks-addons-access
#   export AWS_PROFILE=spinifex
#   tofu init && tofu apply
#   # then open the demo_url output

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.40, < 6.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.20"
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
  default = "eks-addons"
}

variable "k8s_version" {
  type    = string
  default = "1.32"
}

variable "instance_type" {
  type    = string
  default = "t3.medium"
}

variable "node_port" {
  type    = number
  default = 30080
}

variable "replicas" {
  type    = number
  default = 2
}

variable "browse_cidr" {
  type        = string
  default     = "0.0.0.0/0"
  description = "CIDR allowed to reach the demo NodePort; tighten to your own IP in production"
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
  }

  skip_credentials_validation = true
  skip_metadata_api_check     = true
  skip_requesting_account_id  = true
  skip_region_validation      = true
}

provider "kubernetes" {
  host                   = aws_eks_cluster.this.endpoint
  cluster_ca_certificate = base64decode(aws_eks_cluster.this.certificate_authority[0].data)

  exec {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--cluster-name", aws_eks_cluster.this.name, "--region", var.region]
  }
}

data "aws_availability_zones" "available" {
  state = "available"
}

# ---------------------------------------------------------------------------
# VPC + two public subnets
# ---------------------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = "10.31.0.0/16"
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

resource "aws_subnet" "a" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.31.1.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = {
    Name = "${var.cluster_name}-subnet-a"
  }
}

resource "aws_subnet" "b" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.31.2.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = {
    Name = "${var.cluster_name}-subnet-b"
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

resource "aws_route_table_association" "a" {
  subnet_id      = aws_subnet.a.id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table_association" "b" {
  subnet_id      = aws_subnet.b.id
  route_table_id = aws_route_table.public.id
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
# EKS cluster
# ---------------------------------------------------------------------------

resource "aws_eks_cluster" "this" {
  name     = var.cluster_name
  role_arn = aws_iam_role.cluster.arn
  version  = var.k8s_version

  vpc_config {
    subnet_ids              = [aws_subnet.a.id, aws_subnet.b.id]
    endpoint_public_access  = true
    endpoint_private_access = false
  }

  access_config {
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = true
  }

  depends_on = [aws_iam_role_policy_attachment.cluster]

  tags = {
    Name = var.cluster_name
  }
}

# ---------------------------------------------------------------------------
# Node group
# ---------------------------------------------------------------------------

resource "aws_eks_node_group" "default" {
  cluster_name    = aws_eks_cluster.this.name
  node_group_name = "default"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = [aws_subnet.a.id, aws_subnet.b.id]

  scaling_config {
    desired_size = 1
    min_size     = 1
    max_size     = 2
  }

  instance_types = [var.instance_type]
  ami_type       = "AL2_x86_64"

  depends_on = [
    aws_iam_role_policy_attachment.node_worker,
    aws_iam_role_policy_attachment.node_cni,
    aws_iam_role_policy_attachment.node_ecr,
  ]

  tags = {
    Name = "${var.cluster_name}-default"
  }
}

# ---------------------------------------------------------------------------
# Addon — CoreDNS
# ---------------------------------------------------------------------------

resource "aws_eks_addon" "coredns" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "coredns"
  resolve_conflicts_on_create = "OVERWRITE"

  # addon_version omitted on purpose. Spinifex defaults an unset version to its
  # catalog default (coredns 1.11.1). The AWS provider rejects a bare "1.11.1"
  # (it demands a v-prefixed form) which Spinifex's catalog would in turn
  # reject — so pinning is a dead end here; let the server choose.
  depends_on = [aws_eks_node_group.default]

  tags = {
    Name = "${var.cluster_name}-coredns"
  }
}

# ---------------------------------------------------------------------------
# Access entry — grant the viewer principal read-only access
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
# Open the NodePort on the auto-managed nodegroup SG (see eks-quickstart)
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

  depends_on = [aws_eks_node_group.default]
}

resource "aws_vpc_security_group_ingress_rule" "nodeport" {
  security_group_id = data.aws_security_group.nodegroup.id
  cidr_ipv4         = var.browse_cidr
  from_port         = var.node_port
  to_port           = var.node_port
  ip_protocol       = "tcp"

  tags = {
    Name = "${var.cluster_name}-demo-nodeport"
  }
}

# ---------------------------------------------------------------------------
# Demo workload — deployed after CoreDNS so name resolution is up
# ---------------------------------------------------------------------------

resource "kubernetes_deployment_v1" "hello" {
  metadata {
    name      = "hello"
    namespace = "default"
    labels    = { app = "hello" }
  }

  spec {
    replicas = var.replicas

    selector {
      match_labels = { app = "hello" }
    }

    template {
      metadata {
        labels = { app = "hello" }
      }

      spec {
        container {
          name  = "hello"
          image = "nginxdemos/hello"

          port {
            container_port = 80
          }
        }
      }
    }
  }

  depends_on = [aws_eks_addon.coredns]
}

resource "kubernetes_service_v1" "hello" {
  metadata {
    name      = "hello"
    namespace = "default"
  }

  spec {
    selector = { app = "hello" }
    type     = "NodePort"

    port {
      port        = 80
      target_port = 80
      node_port   = var.node_port
    }
  }

  depends_on = [kubernetes_deployment_v1.hello]
}

data "aws_instances" "workers" {
  instance_tags = {
    "spinifex:eks-cluster" = aws_eks_cluster.this.name
  }

  depends_on = [aws_eks_node_group.default]
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------

output "cluster_name" {
  value = aws_eks_cluster.this.name
}

output "viewer_principal_arn" {
  value = aws_iam_role.viewer.arn
}

output "demo_url" {
  value       = "http://${data.aws_instances.workers.public_ips[0]}:${var.node_port}"
  description = "Open in a browser; refresh to see different pods answer"
}

output "update_kubeconfig" {
  value = "aws eks update-kubeconfig --name ${aws_eks_cluster.this.name} --region ${var.region}"
}
