# Example: ECS Quickstart on Spinifex
#
# Provisions a complete ECS stack against the Spinifex AWS gateway: a cluster,
# a task definition with a task role, N container instances launched from the
# spinifex-ecs-node AMI, and an awsvpc service fronted by an Application Load
# Balancer target group.
#
# Unlike the AWS-managed ECS-optimized AMI, a Spinifex container instance reaches
# the control plane over the gateway (TLS + SigV4), so its cloud-init user-data
# must carry a LAN-reachable gateway URL and the gateway CA. The agent draws its
# credentials from IMDS via the ecsInstanceRole instance profile, so no keys are
# written. This is exactly what the console's "provision capacity" action does
# for you; the workbook spells it out so it is reproducible from Terraform.
#
# Usage:
#   cd spinifex/docs/terraform-workbooks/ecs-quickstart
#   export AWS_PROFILE=spinifex
#   tofu init
#   tofu apply -var 'gateway_url=https://<host-lan-ip>:9999'
#
# After apply, fetch the ALB's public IP (the *.elb.spinifex.local DNS name does
# not resolve from your host) and curl it:
#
#   aws elbv2 describe-load-balancers --names ecs-quickstart-alb \
#     --query 'LoadBalancers[0].AvailabilityZones[].LoadBalancerAddresses[].IpAddress' \
#     --output text
#   curl http://<alb_public_ip>

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

variable "cluster_name" {
  type    = string
  default = "ecs-quickstart"
}

variable "instance_type" {
  type    = string
  default = "t3.small"
}

variable "container_count" {
  type        = number
  default     = 1
  description = "Container instances to launch (and the service desired count)."
}

variable "task_image" {
  type        = string
  default     = "docker.io/library/nginx:1.27-alpine"
  description = "Fully-qualified image the task runs; the instance pulls it via containerd."
}

variable "spinifex_endpoint" {
  type        = string
  default     = "https://127.0.0.1:9999"
  description = "Spinifex AWS gateway endpoint as seen from the host running Terraform."
}

variable "gateway_url" {
  type        = string
  description = "Gateway URL as seen from a guest VM (LAN-reachable, not 127.0.0.1), e.g. https://192.168.1.33:9999."
}

variable "gateway_ca_cert_path" {
  type        = string
  default     = "/etc/spinifex/ca.pem"
  description = "Host-readable path to the gateway TLS CA PEM, baked into each container instance."
}

variable "create_instance_role" {
  type        = bool
  default     = true
  description = "Create the ecsInstanceRole role + policy + instance profile. Set false if the ECS console already created it (it is account-global)."
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
    ecs                    = var.spinifex_endpoint
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

# Several spinifex-ecs-node revisions may be imported; take the newest.
data "aws_ami" "ecs_node" {
  most_recent = true
  owners      = ["000000000000"]

  filter {
    name   = "tag:spinifex:managed-by"
    values = ["ecs"]
  }
}

# ---------------------------------------------------------------------------
# VPC + two public subnets
# ---------------------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = "10.40.0.0/16"
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
  cidr_block              = "10.40.1.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = {
    Name = "${var.cluster_name}-subnet-a"
  }
}

resource "aws_subnet" "b" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.40.2.0/24"
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
# Security group — SSH + HTTP inbound, all outbound. Covers both the ALB and
# the awsvpc task ENIs (the ALB reaches each task's ENI on the container port).
# ---------------------------------------------------------------------------

resource "aws_security_group" "ecs" {
  name        = "${var.cluster_name}-sg"
  description = "ECS quickstart: SSH + HTTP inbound, all outbound"
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
    Name = "${var.cluster_name}-sg"
  }
}

# ---------------------------------------------------------------------------
# IAM — the ecsInstanceRole instance profile (instance creds via IMDS) and a
# task role for the task definition's task_role_arn.
#
# ecsInstanceRole is account-global: the name, EC2 trust, and ecs:* policy must
# match what the gateway whitelists for IMDS cred vending. Toggle creation off
# (create_instance_role=false) when the ECS console already created it.
# ---------------------------------------------------------------------------

resource "aws_iam_role" "ecs_instance" {
  count = var.create_instance_role ? 1 : 0

  name = "ecsInstanceRole"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_policy" "ecs_instance" {
  count = var.create_instance_role ? 1 : 0

  name = "ecsInstanceRolePolicy"
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = "ecs:*", Resource = "*" }]
  })
}

resource "aws_iam_role_policy_attachment" "ecs_instance" {
  count = var.create_instance_role ? 1 : 0

  role       = aws_iam_role.ecs_instance[0].name
  policy_arn = aws_iam_policy.ecs_instance[0].arn
}

resource "aws_iam_instance_profile" "ecs_instance" {
  count = var.create_instance_role ? 1 : 0

  name = "ecsInstanceRole"
  role = aws_iam_role.ecs_instance[0].name
}

resource "aws_iam_role" "task" {
  name = "${var.cluster_name}-task-role"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_policy" "task" {
  name = "${var.cluster_name}-task-policy"
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = ["s3:GetObject"], Resource = "*" }]
  })
}

resource "aws_iam_role_policy_attachment" "task" {
  role       = aws_iam_role.task.name
  policy_arn = aws_iam_policy.task.arn
}

# ---------------------------------------------------------------------------
# SSH key pair — RunInstances requires a key (the agent itself needs no SSH).
# ---------------------------------------------------------------------------

resource "tls_private_key" "node" {
  algorithm = "ED25519"
}

resource "aws_key_pair" "node" {
  key_name   = "${var.cluster_name}-node"
  public_key = tls_private_key.node.public_key_openssh
}

resource "local_file" "node_pem" {
  filename        = "${path.module}/${var.cluster_name}-node.pem"
  content         = tls_private_key.node.private_key_openssh
  file_permission = "0600"
}

# ---------------------------------------------------------------------------
# ECS cluster
# ---------------------------------------------------------------------------

resource "aws_ecs_cluster" "main" {
  name = var.cluster_name
}

# ---------------------------------------------------------------------------
# Task definition — awsvpc, one HTTP container on port 80, with a task role.
# ---------------------------------------------------------------------------

resource "aws_ecs_task_definition" "web" {
  family                   = "${var.cluster_name}-web"
  network_mode             = "awsvpc"
  requires_compatibilities = ["EC2"]
  task_role_arn            = aws_iam_role.task.arn

  container_definitions = jsonencode([{
    name      = "web"
    image     = var.task_image
    cpu       = 128
    memory    = 256
    essential = true
    portMappings = [{
      containerPort = 80
      protocol      = "tcp"
    }]
  }])
}

# ---------------------------------------------------------------------------
# Container instances — spinifex-ecs-node AMI with the ecsInstanceRole profile
# and cloud-config user-data seeding the agent's gateway endpoint + CA.
# ---------------------------------------------------------------------------

resource "aws_instance" "node" {
  count = var.container_count

  ami                    = data.aws_ami.ecs_node.id
  instance_type          = var.instance_type
  subnet_id              = aws_subnet.a.id
  vpc_security_group_ids = [aws_security_group.ecs.id]
  iam_instance_profile   = "ecsInstanceRole"
  key_name               = aws_key_pair.node.key_name

  # Plain (non-dash) heredoc: leading whitespace is literal, so the YAML
  # indentation and the CA's block-scalar indentation are exact. indent(6, ...)
  # right-pads the CA's continuation lines to match the first line's 6 spaces.
  user_data = <<USERDATA
#cloud-config
write_files:
  - path: /etc/spinifex-ecs/agent.env
    permissions: '0600'
    content: |
      ECS_GATEWAY_URL=${var.gateway_url}
      ECS_GATEWAY_CA=/etc/spinifex-ecs/gateway-ca.pem
      ECS_REGION=${var.region}
      ECS_CLUSTER=${var.cluster_name}
  - path: /etc/spinifex-ecs/gateway-ca.pem
    permissions: '0644'
    content: |
      ${indent(6, file(var.gateway_ca_cert_path))}
USERDATA

  tags = {
    Name = "ecs-node-${var.cluster_name}-${count.index}"
  }

  # Instance-profile creation must precede launch so IMDS can vend role creds.
  depends_on = [aws_iam_instance_profile.ecs_instance]
}

# ---------------------------------------------------------------------------
# Application Load Balancer + target group (awsvpc → target_type ip) + listener
# ---------------------------------------------------------------------------

resource "aws_lb" "web" {
  name               = "${var.cluster_name}-alb"
  internal           = false
  load_balancer_type = "application"
  security_groups    = [aws_security_group.ecs.id]
  subnets            = [aws_subnet.a.id, aws_subnet.b.id]

  tags = {
    Name = "${var.cluster_name}-alb"
  }
}

resource "aws_lb_target_group" "web" {
  name        = "${var.cluster_name}-tg"
  port        = 80
  protocol    = "HTTP"
  vpc_id      = aws_vpc.main.id
  target_type = "ip"

  health_check {
    path                = "/"
    protocol            = "HTTP"
    healthy_threshold   = 2
    unhealthy_threshold = 3
    timeout             = 5
    interval            = 10
  }

  tags = {
    Name = "${var.cluster_name}-tg"
  }
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.web.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.web.arn
  }
}

# ---------------------------------------------------------------------------
# ECS service — awsvpc, fronted by the target group. The scheduler registers
# each task's ENI IP in the target group on RUNNING.
# ---------------------------------------------------------------------------

resource "aws_ecs_service" "web" {
  name            = "${var.cluster_name}-web"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.web.arn
  desired_count   = var.container_count
  launch_type     = "EC2"

  network_configuration {
    subnets          = [aws_subnet.a.id]
    security_groups  = [aws_security_group.ecs.id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.web.arn
    container_name   = "web"
    container_port   = 80
  }

  # The container instances must be registered before the service can place.
  depends_on = [aws_lb_listener.http, aws_instance.node]
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------

output "note" {
  value = <<-EOT
    Container instances take ~30-60s to boot and register. Track progress with:

      aws ecs list-container-instances --cluster ${var.cluster_name}
      aws ecs describe-services --cluster ${var.cluster_name} \
        --services ${var.cluster_name}-web \
        --query 'services[0].[runningCount,desiredCount]'

    The ALB DNS name ends in .elb.spinifex.local and will not resolve from your
    host. Fetch its public IP and curl it:

      aws elbv2 describe-load-balancers --names ${var.cluster_name}-alb \
        --query 'LoadBalancers[0].AvailabilityZones[].LoadBalancerAddresses[].IpAddress' \
        --output text
  EOT
}

output "cluster_arn" {
  value = aws_ecs_cluster.main.arn
}

output "service_name" {
  value = aws_ecs_service.web.name
}

output "alb_name" {
  value = aws_lb.web.name
}

output "container_instance_ids" {
  value = aws_instance.node[*].id
}
