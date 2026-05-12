# Example 1: Nginx Web Server on Spinifex
#
# Deploys a VPC with a public subnet and an EC2 instance running Nginx.
# Demonstrates: VPC, subnet, internet gateway, route table, security group,
# key pair, cloud-init user-data, and EC2 instance provisioning.
#
# Usage:
#   cd spinifex/scripts/iac/aws/examples/01-nginx-webserver
#   export AWS_PROFILE=spinifex
#   tofu init && tofu apply
#
# After apply:
#   curl http://<public_ip>        # Nginx welcome page
#   ssh -i nginx-demo.pem ec2-user@<public_ip>

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
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

variable "instance_type" {
  type    = string
  default = "t3.small"
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
    ec2 = var.spinifex_endpoint
    iam = var.spinifex_endpoint
    sts = var.spinifex_endpoint
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

data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["000000000000"] # Spinifex system images

  filter {
    name   = "name"
    values = ["*ubuntu-24.04*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }

  filter {
    name   = "root-device-type"
    values = ["ebs"]
  }
}

# ---------------------------------------------------------------------------
# SSH Key Pair
# ---------------------------------------------------------------------------

resource "tls_private_key" "nginx" {
  algorithm = "ED25519"
}

resource "aws_key_pair" "nginx" {
  key_name   = "nginx-demo"
  public_key = tls_private_key.nginx.public_key_openssh
}

resource "local_file" "nginx_pem" {
  filename        = "${path.module}/nginx-demo.pem"
  content         = tls_private_key.nginx.private_key_openssh
  file_permission = "0600"
}

# ---------------------------------------------------------------------------
# VPC
# ---------------------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = "10.10.0.0/16"
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = {
    Name = "nginx-demo-vpc"
  }
}

# ---------------------------------------------------------------------------
# Internet Gateway — gives the public subnet a route to the internet
# ---------------------------------------------------------------------------

resource "aws_internet_gateway" "igw" {
  vpc_id = aws_vpc.main.id

  tags = {
    Name = "nginx-demo-igw"
  }
}

# ---------------------------------------------------------------------------
# Public Subnet
# ---------------------------------------------------------------------------

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.10.1.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = {
    Name = "nginx-demo-public"
  }
}

# ---------------------------------------------------------------------------
# Route Table — send 0.0.0.0/0 through the internet gateway
# ---------------------------------------------------------------------------

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.igw.id
  }

  tags = {
    Name = "nginx-demo-public-rt"
  }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

# ---------------------------------------------------------------------------
# Security Group — SSH + HTTP inbound, all outbound
# ---------------------------------------------------------------------------

resource "aws_security_group" "web" {
  name        = "nginx-demo-sg"
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
    Name = "nginx-demo-sg"
  }
}

# ---------------------------------------------------------------------------
# EC2 Instance — Ubuntu 26.04 with Nginx installed via cloud-init
# ---------------------------------------------------------------------------

resource "aws_instance" "nginx" {
  ami           = data.aws_ami.ubuntu.id
  instance_type = var.instance_type

  subnet_id              = aws_subnet.public.id
  vpc_security_group_ids = [aws_security_group.web.id]
  key_name               = aws_key_pair.nginx.key_name

  associate_public_ip_address = true

  user_data_base64 = base64encode(<<-USERDATA
    #!/bin/bash
    set -euo pipefail

    # Install Nginx
    apt-get update -y
    apt-get install -y nginx

    # Write a custom landing page
    cat > /var/www/html/index.html <<'HTML'
    <!DOCTYPE html>
    <html>
    <head><title>Spinifex Demo</title></head>
    <body style="font-family: sans-serif; max-width: 600px; margin: 80px auto;">
      <h1>Hello from Spinifex!</h1>
      <p>This Nginx server was deployed with Terraform on Spinifex infrastructure.</p>
      <hr>
      <p><small>Instance provisioned via cloud-init user-data.</small></p>
    </body>
    </html>
    HTML

    # Ensure Nginx is running
    systemctl enable nginx
    systemctl restart nginx
  USERDATA
  )

  tags = {
    Name = "nginx-demo"
  }
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------

output "note" {
  value = "EC2 instances can take 30+ seconds to boot after apply. If SSH or HTTP is unreachable, wait and retry."
}

output "instance_id" {
  value = aws_instance.nginx.id
}

output "public_ip" {
  value = aws_instance.nginx.public_ip
}

output "ssh_command" {
  value = "ssh -i nginx-demo.pem ec2-user@${aws_instance.nginx.public_ip}"
}

output "web_url" {
  value = "http://${aws_instance.nginx.public_ip}"
}
