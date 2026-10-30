# Example: Nginx Web Servers with ALB on Spinifex
#
# Deploys a VPC with two public subnets hosting Nginx EC2 instances and an
# internet-facing Application Load Balancer. Workers sit in public subnets
# with auto-assigned public IPs purely so cloud-init can apt-install nginx.
# The ALB targets them by primary private IP (target_type=instance), so
# load-balanced traffic stays on the private VPC network.
#
# In a production deployment, workers would be in private subnets with
# nginx baked into a custom AMI (or installed via a private repository
# mirror), and a NAT Gateway would be unnecessary for this workload.
#
# Demonstrates: VPC, public subnets, internet gateway, route tables,
# security group, key pair, cloud-init user-data, EC2 instances with
# auto-assigned public IPs, an optional placement group, ALB, target group,
# and listener.
#
# server_count sets how many web servers launch (default 2). Setting
# placement_strategy = "spread" puts each one on a different Spinifex node,
# so a three-node cluster with server_count = 3 can lose any node and keep
# serving through the ALB.
#
# Usage:
#   cd spinifex/docs/terraform-workbooks/nginx-alb
#   export AWS_PROFILE=spinifex
#   tofu init && tofu apply
#
# After apply, fetch the ALB's public IP (the *.elb.spinifex.local DNS
# name does not resolve from your host):
#
#   aws elbv2 describe-load-balancers --names nginx-alb \
#     --query 'LoadBalancers[0].AvailabilityZones[].LoadBalancerAddresses[].IpAddress' \
#     --output text
#
# Then:
#   curl http://<alb_public_ip>    # Load-balanced Nginx (alternates between instances)
#
# Three servers, one per node:
#   tofu apply -var server_count=3 -var placement_strategy=spread

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "5.65.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "4.4.1"
    }
    local = {
      source  = "hashicorp/local"
      version = "2.9.1"
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

variable "instance_type" {
  type    = string
  default = "t3.small"
}

variable "server_count" {
  type        = number
  default     = 2
  description = "Number of Nginx web servers behind the ALB"

  validation {
    condition     = var.server_count >= 1
    error_message = "server_count must be at least 1."
  }
}

variable "placement_strategy" {
  type        = string
  default     = ""
  description = "Placement group strategy for the web servers: spread (one per node), cluster (all on one node), or empty for none"

  validation {
    condition     = contains(["", "spread", "cluster"], var.placement_strategy)
    error_message = "placement_strategy must be spread, cluster, or empty."
  }
}

variable "spinifex_endpoint" {
  type        = string
  default     = "https://127.0.0.1:9999"
  description = "Spinifex AWS gateway endpoint"
}

# ---------------------------------------------------------------------------
# Provider — point the AWS provider at Spinifex
# ---------------------------------------------------------------------------

provider "aws" {
  region = var.region

  endpoints {
    ec2                    = var.spinifex_endpoint
    iam                    = var.spinifex_endpoint
    sts                    = var.spinifex_endpoint
    elasticloadbalancingv2 = var.spinifex_endpoint
  }

  skip_metadata_api_check = true
  skip_region_validation  = true
}

# ---------------------------------------------------------------------------
# Data sources
# ---------------------------------------------------------------------------

data "aws_availability_zones" "available" {
  state = "available"
}

data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["000000000000"] # Spinifex system images

  filter {
    name   = "name"
    values = ["*ubuntu-26.04*", "*ubuntu-24.04*"]
  }
}

# ---------------------------------------------------------------------------
# SSH Key Pair
# ---------------------------------------------------------------------------

resource "tls_private_key" "nginx" {
  algorithm = "ED25519"
}

resource "aws_key_pair" "nginx" {
  key_name   = "nginx-alb-demo"
  public_key = tls_private_key.nginx.public_key_openssh
}

resource "local_file" "nginx_pem" {
  filename        = "${path.module}/nginx-alb-demo.pem"
  content         = tls_private_key.nginx.private_key_openssh
  file_permission = "0600"
}

# ---------------------------------------------------------------------------
# VPC
# ---------------------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = "10.20.0.0/16"
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = {
    Name = "nginx-alb-vpc"
  }
}

# ---------------------------------------------------------------------------
# Internet Gateway
# ---------------------------------------------------------------------------

resource "aws_internet_gateway" "igw" {
  vpc_id = aws_vpc.main.id

  tags = {
    Name = "nginx-alb-igw"
  }
}

# ---------------------------------------------------------------------------
# Public Subnets (two AZs for the ALB and NAT Gateway)
# ---------------------------------------------------------------------------

resource "aws_subnet" "public_a" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.20.1.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = {
    Name = "nginx-alb-public-a"
  }
}

resource "aws_subnet" "public_b" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.20.2.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = {
    Name = "nginx-alb-public-b"
  }
}

# ---------------------------------------------------------------------------
# Route Table — public subnets egress via IGW
# ---------------------------------------------------------------------------

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.igw.id
  }

  tags = {
    Name = "nginx-alb-public-rt"
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

# ---------------------------------------------------------------------------
# Security Group — SSH + HTTP inbound, all outbound
# ---------------------------------------------------------------------------

resource "aws_security_group" "web" {
  name        = "nginx-alb-sg"
  description = "Allow SSH and HTTP inbound"
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "SSH"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    description = "HTTP"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "All outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "nginx-alb-sg"
  }
}

# ---------------------------------------------------------------------------
# Placement Group — optional; spread puts each web server on its own node
# ---------------------------------------------------------------------------

resource "aws_placement_group" "web" {
  count    = var.placement_strategy == "" ? 0 : 1
  name     = "nginx-alb-${var.placement_strategy}"
  strategy = var.placement_strategy
}

# ---------------------------------------------------------------------------
# EC2 Instances — Nginx servers with distinct landing pages, alternating
# between the two public subnets
# ---------------------------------------------------------------------------

resource "aws_instance" "nginx" {
  count = var.server_count

  ami             = data.aws_ami.ubuntu.id
  instance_type   = var.instance_type
  placement_group = one(aws_placement_group.web[*].name)

  subnet_id              = count.index % 2 == 0 ? aws_subnet.public_a.id : aws_subnet.public_b.id
  vpc_security_group_ids = [aws_security_group.web.id]
  key_name               = aws_key_pair.nginx.key_name

  user_data_base64 = base64encode(<<-USERDATA
    #!/bin/bash
    set -euo pipefail

    apt-get update -y
    apt-get install -y nginx

    INSTANCE_ID=$(cat /var/lib/cloud/data/instance-id 2>/dev/null || hostname)
    cat > /var/www/html/index.html <<HTML
    <!DOCTYPE html>
    <html>
    <head><title>Spinifex ALB Demo</title></head>
    <body style="font-family: sans-serif; max-width: 600px; margin: 80px auto;">
      <h1>Hello from Spinifex!</h1>
      <p><strong>Instance:</strong> $INSTANCE_ID (Server ${count.index + 1})</p>
      <p>This Nginx server is behind an Application Load Balancer.</p>
      <hr>
      <p><small>Provisioned via cloud-init user-data.</small></p>
    </body>
    </html>
    HTML

    systemctl enable nginx
    systemctl restart nginx
  USERDATA
  )

  tags = {
    Name = "nginx-alb-${count.index + 1}"
  }
}

# Earlier versions declared the two servers separately.
moved {
  from = aws_instance.nginx_1
  to   = aws_instance.nginx[0]
}

moved {
  from = aws_instance.nginx_2
  to   = aws_instance.nginx[1]
}

# ---------------------------------------------------------------------------
# Application Load Balancer
# ---------------------------------------------------------------------------

resource "aws_lb" "web" {
  name               = "nginx-alb"
  internal           = false
  load_balancer_type = "application"
  security_groups    = [aws_security_group.web.id]
  subnets            = [aws_subnet.public_a.id, aws_subnet.public_b.id]

  tags = {
    Name = "nginx-alb"
  }
}

# ---------------------------------------------------------------------------
# Target Group — HTTP health-checked on port 80
# ---------------------------------------------------------------------------

resource "aws_lb_target_group" "nginx" {
  name     = "nginx-alb-tg"
  port     = 80
  protocol = "HTTP"
  vpc_id   = aws_vpc.main.id

  health_check {
    path                = "/"
    protocol            = "HTTP"
    healthy_threshold   = 2
    unhealthy_threshold = 3
    timeout             = 5
    interval            = 10
  }

  tags = {
    Name = "nginx-alb-tg"
  }
}

# ---------------------------------------------------------------------------
# Register every web server as a target
# ---------------------------------------------------------------------------

resource "aws_lb_target_group_attachment" "nginx" {
  count = var.server_count

  target_group_arn = aws_lb_target_group.nginx.arn
  target_id        = aws_instance.nginx[count.index].id
  port             = 80
}

moved {
  from = aws_lb_target_group_attachment.nginx_1
  to   = aws_lb_target_group_attachment.nginx[0]
}

moved {
  from = aws_lb_target_group_attachment.nginx_2
  to   = aws_lb_target_group_attachment.nginx[1]
}

# ---------------------------------------------------------------------------
# Listener — forward HTTP :80 to the target group
# ---------------------------------------------------------------------------

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.web.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.nginx.arn
  }
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------

output "note" {
  value = <<-EOT
    EC2 instances can take 30+ seconds to boot after apply, and Nginx installs
    after that — if HTTP is unreachable, wait and retry.

    The ALB DNS name ends in .elb.spinifex.local and will not resolve from
    your host, so fetch the ALB's public IP with:

      aws elbv2 describe-load-balancers --names nginx-alb \
        --query 'LoadBalancers[0].AvailabilityZones[].LoadBalancerAddresses[].IpAddress' \
        --output text

    Then: curl http://<that-ip>
  EOT
}

output "alb_name" {
  value = aws_lb.web.name
}

output "alb_arn" {
  value = aws_lb.web.arn
}

output "alb_dns_name" {
  value = aws_lb.web.dns_name
}

output "instance_ids" {
  value = aws_instance.nginx[*].id
}

output "instance_private_ips" {
  value = aws_instance.nginx[*].private_ip
}

output "instance_public_ips" {
  value = aws_instance.nginx[*].public_ip
}

output "placement_group" {
  value = one(aws_placement_group.web[*].name)
}
