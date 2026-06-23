# Example: EKS Quickstart on Spinifex
#
# A minimal managed-Kubernetes cluster that ends in something you can see: a
# VPC, the two IAM roles EKS needs, a one- or three-worker cluster, an ECR
# repository, and the Spinifex-themed demo app deployed onto it by Terraform and
# exposed on the worker's public IP.
#
# Once `apply` finishes, open the demo_url output in a browser. The page reports
# the pod, node, cluster, and region that answered — refresh and it bounces
# between the app's replicas, showing the cluster is really scheduling and
# load-balancing pods.
#
# Demonstrates: VPC + subnets, EKS cluster + node IAM roles, an aws_eks_cluster
# and managed aws_eks_node_group, an ECR repository the workers pull from, the
# Kubernetes provider authenticating to the cluster, and a Deployment + NodePort
# Service reachable from your browser.
#
# Usage:
#   cd spinifex/docs/terraform-workbooks/eks-quickstart
#   export AWS_PROFILE=spinifex
#   tofu init && tofu apply
#   # build + push the demo image to the ECR repo this creates (see README),
#   # then: cd workloads && tofu init && tofu apply
#   # finally open the demo_url output

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.40, < 6.0"
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
  default = "eks-quickstart"
}

variable "k8s_version" {
  type        = string
  default     = "1.32"
  description = "Kubernetes minor version for the control plane and workers"
}

variable "instance_type" {
  type    = string
  default = "t3.medium"
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
  description = "NodePort the demo Service is published on"
}

variable "browse_cidr" {
  type        = string
  default     = "0.0.0.0/0"
  description = "CIDR allowed to reach the demo NodePort; tighten to your own IP in production"
}

variable "spinifex_endpoint" {
  type        = string
  default     = "https://127.0.0.1:9999"
  description = "Spinifex AWS gateway endpoint"
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
  }

  skip_credentials_validation = true
  skip_metadata_api_check     = true
  skip_requesting_account_id  = true
  skip_region_validation      = true
}

# ---------------------------------------------------------------------------
# Data sources
# ---------------------------------------------------------------------------

data "aws_availability_zones" "available" {
  state = "available"
}

# ---------------------------------------------------------------------------
# VPC + two public subnets (workers get public IPs to pull the demo image)
# ---------------------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = "10.30.0.0/16"
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
  cidr_block              = "10.30.1.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = {
    Name = "${var.cluster_name}-subnet-a"
  }
}

resource "aws_subnet" "b" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.30.2.0/24"
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
# IAM — EKS cluster role
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
# IAM — worker node role
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
# ECR — repository the workers pull the demo image from
#
# Build and push the demo-app image here before applying the workloads module
# (see ../demo-app/README.md). The node role already carries
# AmazonEC2ContainerRegistryReadOnly, so workers can pull from it.
# ---------------------------------------------------------------------------

resource "aws_ecr_repository" "demo" {
  name = "spinifex-demo"

  # tofu destroy must remove the repo even though it still holds the pushed demo
  # image; without this, DeleteRepository (force=false) fails RepositoryNotEmpty.
  force_delete = true
}

# ---------------------------------------------------------------------------
# EKS cluster — public API endpoint, API (access-entry) auth mode
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
# Managed node group — one worker
# ---------------------------------------------------------------------------

resource "aws_eks_node_group" "default" {
  cluster_name    = aws_eks_cluster.this.name
  node_group_name = "default"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = [aws_subnet.a.id, aws_subnet.b.id]

  scaling_config {
    desired_size = var.node_desired_size
    min_size     = var.node_desired_size
    max_size     = var.node_desired_size * 2
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
# Open the NodePort on the auto-managed nodegroup SG
#
# Spinifex creates the worker SG itself (vpc_config.security_group_ids is
# ignored) and admits only intra-cluster traffic. To reach the demo NodePort
# from a browser, look the SG up by its deterministic name and add one rule.
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
# Discover the worker's public IP for the demo URL
# ---------------------------------------------------------------------------

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

output "region" {
  value = var.region
}

output "node_port" {
  value = var.node_port
}

output "node_desired_size" {
  value = var.node_desired_size
}

output "ecr_repository_url" {
  value       = aws_ecr_repository.demo.repository_url
  description = "Push the demo-app image here, then apply the workloads module"
}

output "demo_url" {
  value       = "http://${data.aws_instances.workers.public_ips[0]}:${var.node_port}"
  description = "Open in a browser; refresh to see different pods answer"
}

output "update_kubeconfig" {
  value = "aws eks update-kubeconfig --name ${aws_eks_cluster.this.name} --region ${var.region}"
}
