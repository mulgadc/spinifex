# Example: Nginx Web Servers with ALB on Spinifex
#
# Deploys a VPC with two public subnets (ALB + NAT Gateway) and two private
# subnets hosting Nginx EC2 instances. An internet-facing Application Load
# Balancer distributes HTTP traffic between the private workers, and the
# NAT Gateway gives them outbound internet access so cloud-init can install
# Nginx from the Debian apt repository.
#
# Demonstrates: VPC, public and private subnets, internet gateway, NAT
# Gateway with Elastic IP, route tables, security group, key pair, cloud-init
# user-data, EC2 instances, ALB, target group, and listener.
#
# Usage:
#   cd spinifex/docs/terraform/nginx-alb
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

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.65.0, < 5.66.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = ">= 4.0"
    }
    local = {
      source  = "hashicorp/local"
      version = ">= 2.0"
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

variable "spinifex_endpoint" {
  type        = string
  default     = "https://localhost:9999"
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

data "aws_ami" "debian12" {
  most_recent = true
  owners      = ["000000000000"] # Spinifex system images

  filter {
    name   = "name"
    values = ["*debian-12*"]
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
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.20.1.0/24"
  availability_zone = data.aws_availability_zones.available.names[0]

  tags = {
    Name = "nginx-alb-public-a"
  }
}

resource "aws_subnet" "public_b" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.20.2.0/24"
  availability_zone = data.aws_availability_zones.available.names[0]

  tags = {
    Name = "nginx-alb-public-b"
  }
}

# ---------------------------------------------------------------------------
# Private Subnets (two AZs for the Nginx workers)
# ---------------------------------------------------------------------------

resource "aws_subnet" "private_a" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.20.11.0/24"
  availability_zone = data.aws_availability_zones.available.names[0]

  tags = {
    Name = "nginx-alb-private-a"
  }
}

resource "aws_subnet" "private_b" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.20.12.0/24"
  availability_zone = data.aws_availability_zones.available.names[0]

  tags = {
    Name = "nginx-alb-private-b"
  }
}

# ---------------------------------------------------------------------------
# NAT Gateway — outbound internet for the private subnets
#
# Background plumbing: the private-subnet workers need outbound connectivity
# during cloud-init so apt-get can install Nginx. A single NAT Gateway in
# public_a provides SNAT for both private subnets via the private route table.
# ---------------------------------------------------------------------------

resource "aws_eip" "nat" {
  domain = "vpc"

  tags = {
    Name = "nginx-alb-nat-eip"
  }
}

resource "aws_nat_gateway" "main" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public_a.id

  tags = {
    Name = "nginx-alb-nat"
  }

  depends_on = [aws_internet_gateway.igw]
}

# ---------------------------------------------------------------------------
# Route Tables — public subnets egress via IGW, private subnets via NAT GW
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

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.main.id
  }

  tags = {
    Name = "nginx-alb-private-rt"
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
# EC2 Instances — two Nginx servers with distinct landing pages
# ---------------------------------------------------------------------------

resource "aws_instance" "nginx_1" {
  ami           = data.aws_ami.debian12.id
  instance_type = "t3.small"

  subnet_id              = aws_subnet.private_a.id
  vpc_security_group_ids = [aws_security_group.web.id]
  key_name               = aws_key_pair.nginx.key_name

  # Workers pull nginx from apt via the NAT Gateway — creation must wait
  # until the NAT Gateway is available so cloud-init can reach the repos.
  depends_on = [aws_nat_gateway.main]

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
      <p><strong>Instance:</strong> $INSTANCE_ID (Server 1)</p>
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
    Name = "nginx-alb-1"
  }
}

resource "aws_instance" "nginx_2" {
  ami           = data.aws_ami.debian12.id
  instance_type = "t3.small"

  subnet_id              = aws_subnet.private_b.id
  vpc_security_group_ids = [aws_security_group.web.id]
  key_name               = aws_key_pair.nginx.key_name

  # Workers pull nginx from apt via the NAT Gateway — creation must wait
  # until the NAT Gateway is available so cloud-init can reach the repos.
  depends_on = [aws_nat_gateway.main]

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
      <p><strong>Instance:</strong> $INSTANCE_ID (Server 2)</p>
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
    Name = "nginx-alb-2"
  }
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
# Register both instances as targets
# ---------------------------------------------------------------------------

resource "aws_lb_target_group_attachment" "nginx_1" {
  target_group_arn = aws_lb_target_group.nginx.arn
  target_id        = aws_instance.nginx_1.id
  port             = 80
}

resource "aws_lb_target_group_attachment" "nginx_2" {
  target_group_arn = aws_lb_target_group.nginx.arn
  target_id        = aws_instance.nginx_2.id
  port             = 80
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
    EC2 instances can take 30+ seconds to boot after apply — if HTTP is
    unreachable, wait and retry.

    The Nginx instances have private IPs only. The ALB DNS name ends in
    .elb.spinifex.local and will not resolve from your host, so fetch the
    ALB's public IP with:

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

output "instance_1_id" {
  value = aws_instance.nginx_1.id
}

output "instance_1_private_ip" {
  value = aws_instance.nginx_1.private_ip
}

output "instance_2_id" {
  value = aws_instance.nginx_2.id
}

output "instance_2_private_ip" {
  value = aws_instance.nginx_2.private_ip
}
