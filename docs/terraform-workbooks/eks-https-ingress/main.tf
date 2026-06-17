# Example: 3-Node EKS behind an HTTPS ALB (ACM TLS) with a load-balanced app
#
# The largest EKS workbook, and the one with the clearest payoff. It stands up
# an HA-shaped cluster sized for a 3-node Spinifex deployment, deploys a demo
# web app across the workers, and publishes it through an internet-facing ALB
# that terminates TLS with a certificate held in ACM.
#
# After `apply`, fetch the ALB's public IP (see alb_public_ip_hint) and open
# https://<ip> in a browser. The page reports which pod answered; refresh and
# it moves between the three replicas spread across the cluster — a live picture
# of a multi-node Kubernetes cluster load-balancing behind HTTPS.
#
# What it builds:
#   * VPC with two public subnets (ALB + NAT) and two private subnets (workers).
#   * A NAT gateway so the private workers can pull the demo container image.
#   * EKS cluster with BOTH a public endpoint (kubectl from your host) and a
#     private endpoint (the in-VPC path the workers use to join).
#   * A managed node group of three workers (t3.large) in the private subnets.
#   * CoreDNS addon.
#   * A self-signed cert imported into ACM (Spinifex ACM supports
#     ImportCertificate, not RequestCertificate), wired to the ALB's HTTPS
#     listener, which forwards to the workers' NodePort.
#   * The demo Deployment + NodePort Service, deployed by the Kubernetes
#     provider, plus the one SG rule that lets the ALB reach the NodePort.
#
# Usage:
#   cd spinifex/docs/terraform-workbooks/eks-https-ingress
#   export AWS_PROFILE=spinifex
#   tofu init && tofu apply

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
  default = "eks-https"
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
  default     = 3
  description = "Worker count; spread across a 3-node Spinifex deployment for HA"
}

variable "node_port" {
  type        = number
  default     = 30080
  description = "NodePort the ALB forwards to and the demo Service is published on"
}

variable "cert_common_name" {
  type    = string
  default = "eks-https.spinifex.local"
}

variable "api_public_access_cidr" {
  type        = string
  default     = "0.0.0.0/0"
  description = "CIDR allowed to reach the public Kubernetes API endpoint; tighten in production"
}

variable "alb_ingress_cidr" {
  type        = string
  default     = "0.0.0.0/0"
  description = "CIDR allowed to reach the ALB HTTPS listener"
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
    ec2                    = var.spinifex_endpoint
    iam                    = var.spinifex_endpoint
    sts                    = var.spinifex_endpoint
    eks                    = var.spinifex_endpoint
    acm                    = var.spinifex_endpoint
    elasticloadbalancingv2 = var.spinifex_endpoint
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

# NAT gateway gives the private workers outbound internet to pull the demo
# container image. The workers still *join* the cluster over the in-VPC private
# endpoint, not through the NAT.
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
# EKS cluster — public + private endpoints
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
    Name = var.cluster_name
  }
}

# ---------------------------------------------------------------------------
# Managed node group — three workers in the private subnets
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
# Addon — CoreDNS
# ---------------------------------------------------------------------------

resource "aws_eks_addon" "coredns" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "coredns"
  resolve_conflicts_on_create = "OVERWRITE"

  # addon_version omitted on purpose. Spinifex defaults an unset version to its
  # catalog default (coredns 1.11.1). The AWS provider also rejects a bare
  # "1.11.1" (it demands a v-prefixed form) which Spinifex's catalog would in
  # turn reject — so pinning is a dead end here; let the server choose.
  depends_on = [aws_eks_node_group.workers]

  tags = {
    Name = "${var.cluster_name}-coredns"
  }
}

# ---------------------------------------------------------------------------
# TLS — self-signed certificate imported into ACM
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
# ALB — internet-facing, HTTPS terminated with the ACM cert
# ---------------------------------------------------------------------------

resource "aws_security_group" "alb" {
  name        = "${var.cluster_name}-alb-sg"
  description = "ALB HTTPS inbound, NodePort to workers"
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "HTTPS"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = [var.alb_ingress_cidr]
  }

  egress {
    description = "All outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "${var.cluster_name}-alb-sg"
  }
}

resource "aws_lb" "ingress" {
  name               = "${var.cluster_name}-alb"
  internal           = false
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = [aws_subnet.public_a.id, aws_subnet.public_b.id]

  tags = {
    Name = "${var.cluster_name}-alb"
  }
}

resource "aws_lb_target_group" "workers" {
  name        = "${var.cluster_name}-tg"
  port        = var.node_port
  protocol    = "HTTP"
  target_type = "instance"
  vpc_id      = aws_vpc.main.id

  health_check {
    path                = "/"
    protocol            = "HTTP"
    matcher             = "200-399"
    healthy_threshold   = 2
    unhealthy_threshold = 3
    timeout             = 5
    interval            = 10
  }

  tags = {
    Name = "${var.cluster_name}-tg"
  }
}

# Discover the launched workers by their Spinifex EKS tag so the ALB can target
# them. depends_on the node group: the lookup must run after the workers exist.
data "aws_instances" "workers" {
  instance_tags = {
    "spinifex:eks-cluster" = aws_eks_cluster.this.name
  }

  depends_on = [aws_eks_node_group.workers]
}

# count is the known desired size, so it's resolvable at plan time even though
# the worker IDs themselves are only known after the node group launches.
resource "aws_lb_target_group_attachment" "workers" {
  count            = var.node_desired_size
  target_group_arn = aws_lb_target_group.workers.arn
  target_id        = data.aws_instances.workers.ids[count.index]
  port             = var.node_port
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.ingress.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = aws_acm_certificate.ingress.arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.workers.arn
  }
}

# ---------------------------------------------------------------------------
# Let the ALB reach the workers' NodePort
#
# Spinifex auto-manages the nodegroup SG and admits only intra-cluster traffic.
# Look it up by its deterministic name and admit the ALB SG on the NodePort.
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

resource "aws_vpc_security_group_ingress_rule" "nodeport_from_alb" {
  security_group_id            = data.aws_security_group.nodegroup.id
  referenced_security_group_id = aws_security_group.alb.id
  from_port                    = var.node_port
  to_port                      = var.node_port
  ip_protocol                  = "tcp"

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

output "alb_dns_name" {
  value = aws_lb.ingress.dns_name
}

output "alb_public_ip_hint" {
  value = "aws elbv2 describe-load-balancers --names ${var.cluster_name}-alb --query 'LoadBalancers[0].AvailabilityZones[].LoadBalancerAddresses[].IpAddress' --output text"
}

output "demo_url_hint" {
  value = "Fetch the ALB IP via alb_public_ip_hint, then open https://<that-ip> (self-signed cert: curl -k). Refresh to see different pods answer."
}

output "update_kubeconfig" {
  value = "aws eks update-kubeconfig --name ${aws_eks_cluster.this.name} --region ${var.region}"
}
