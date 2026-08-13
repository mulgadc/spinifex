# Example: RDS Quickstart on Spinifex
#
# Provisions a managed PostgreSQL instance and a client VM that connects to it:
# a VPC, a DB subnet group, a parameter group, an aws_db_instance, and a client
# instance whose cloud-init installs psql and points it at the endpoint.
#
# A Spinifex DB instance runs in a system-owned VM you never see; what lands in
# your VPC is a single ENI in one of the DB subnet group's subnets. The endpoint
# is therefore ALWAYS private — there is no publicly_accessible mode — so the
# only way to reach the database is from inside the VPC, which is what the
# client instance here is for.
#
# Two subnets: the client sits in a public one, because it needs an
# internet-gateway route to apt-get a psql, and the database in a private one
# with no route off the VPC at all. The endpoint is reachable from any subnet of
# the VPC, so the tier that talks to the database does not have to share the
# subnet the endpoint ENI landed in.
#
# Usage:
#   cd spinifex/docs/terraform-workbooks/rds-quickstart
#   export AWS_PROFILE=spinifex
#   tofu init
#   tofu apply -var 'db_password=<choose-one>'
#
# After apply, SSH to the client and connect (PGHOST/PGUSER/PGDATABASE are set
# from /etc/profile.d, and the password comes from ~/.pgpass):
#
#   ssh -i rds-quickstart-client.pem ubuntu@<client_public_ip>
#   psql -c 'SELECT version();'
#
# Creating the instance boots a VM, runs initdb and waits for the in-guest agent
# to report healthy, so the apply takes several minutes before the client is
# even launched.

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

variable "name" {
  type        = string
  default     = "rds-quickstart"
  description = "Prefix for every resource this workbook creates."
}

variable "spinifex_endpoint" {
  type        = string
  default     = "https://127.0.0.1:9999"
  description = "Spinifex AWS gateway endpoint as seen from the host running Terraform."
}

variable "instance_type" {
  type        = string
  default     = "t3.small"
  description = "EC2 type for the psql client VM. Not the DB — see db_instance_class."
}

# db.* classes are a naming facade over the platform's EC2 sizing table, and only
# a curated subset is offered. describe-orderable-db-instance-options is not
# implemented, so pin the class here rather than reaching for the
# aws_rds_orderable_db_instance data source.
variable "db_instance_class" {
  type    = string
  default = "db.t3.micro"
}

# PostgreSQL 18 is the only version this platform serves; any other value is
# rejected at create rather than silently substituted.
variable "engine_version" {
  type    = string
  default = "18"
}

variable "db_name" {
  type        = string
  default     = "appdb"
  description = "Database created inside the instance at bootstrap."
}

variable "db_username" {
  type        = string
  default     = "appuser"
  description = "Master user. postgres, rdsadmin, rds_superuser and pg_* are reserved by the engine."
}

# No '/', '"', '@' or spaces: the characters the API rejects because they break a
# connection string or the engine's own role syntax.
variable "db_password" {
  type        = string
  default     = "QuickstartS3cret1"
  sensitive   = true
  description = "Master password. Override this — the default exists so the workbook applies unattended."
}

variable "allocated_storage" {
  type        = number
  default     = 20
  description = "Data-volume size in GiB. Grow-only, and a grow is stop/start with downtime."
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
    rds = var.spinifex_endpoint
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

# The client is an ordinary Ubuntu guest — the engine image is system-owned and
# never launched by a customer.
data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["000000000000"]

  filter {
    name   = "name"
    values = ["*ubuntu-26.04*", "*ubuntu-24.04*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

# ---------------------------------------------------------------------------
# VPC — a public client subnet and a private DB subnet
# ---------------------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = "10.60.0.0/16"
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = {
    Name = "${var.name}-vpc"
  }
}

resource "aws_internet_gateway" "igw" {
  vpc_id = aws_vpc.main.id

  tags = {
    Name = "${var.name}-igw"
  }
}

# Public: an internet-gateway route and a public address on launch, which is how
# the client reaches apt and how you SSH to it.
resource "aws_subnet" "client" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.60.1.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = {
    Name = "${var.name}-client-subnet"
  }
}

# Private: no association, so it stays on the main route table create-vpc writes,
# which routes inside the VPC and nowhere else. The endpoint ENI lands here.
resource "aws_subnet" "db" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.60.2.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = false

  tags = {
    Name = "${var.name}-db-subnet"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.igw.id
  }

  tags = {
    Name = "${var.name}-public-rt"
  }
}

resource "aws_route_table_association" "client" {
  subnet_id      = aws_subnet.client.id
  route_table_id = aws_route_table.public.id
}

# ---------------------------------------------------------------------------
# Security groups — the client SG is the only source the DB SG admits
# ---------------------------------------------------------------------------

resource "aws_security_group" "client" {
  name        = "${var.name}-client-sg"
  description = "RDS quickstart client: SSH inbound, all outbound"
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "SSH"
    from_port   = 22
    to_port     = 22
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
    Name = "${var.name}-client-sg"
  }
}

# Attached to the endpoint ENI, where it compiles to an OVN ACL on the DB VM's
# customer-facing interface. Nothing else in the VPC can open :5432.
resource "aws_security_group" "db" {
  name        = "${var.name}-db-sg"
  description = "RDS quickstart database: postgres from the client SG only"
  vpc_id      = aws_vpc.main.id

  ingress {
    description     = "PostgreSQL from the client"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.client.id]
  }

  egress {
    description = "All outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "${var.name}-db-sg"
  }
}

# ---------------------------------------------------------------------------
# DB subnet group + parameter group
# ---------------------------------------------------------------------------

resource "aws_db_subnet_group" "main" {
  name        = "${var.name}-subnets"
  description = "RDS quickstart DB subnets"
  subnet_ids  = [aws_subnet.db.id]

  tags = {
    Name = "${var.name}-subnets"
  }
}

# Dynamic parameters are written into the engine's config and reloaded live;
# static ones are recorded pending-reboot and adopted by the next
# reboot-db-instance. log_min_duration_statement is dynamic.
resource "aws_db_parameter_group" "main" {
  name        = "${var.name}-pg18"
  family      = "postgres18"
  description = "RDS quickstart parameters"

  parameter {
    name  = "log_min_duration_statement"
    value = "500"
  }

  tags = {
    Name = "${var.name}-pg18"
  }
}

# ---------------------------------------------------------------------------
# The DB instance
#
# storage_encrypted is set explicitly because the platform only offers encrypted
# storage: the API rejects false, and leaving it unset would read back as a
# permanent diff against an instance that reports true.
# ---------------------------------------------------------------------------

resource "aws_db_instance" "main" {
  identifier     = var.name
  engine         = "postgres"
  engine_version = var.engine_version
  instance_class = var.db_instance_class

  allocated_storage = var.allocated_storage
  storage_type      = "gp3"
  storage_encrypted = true

  db_name  = var.db_name
  username = var.db_username
  password = var.db_password

  db_subnet_group_name   = aws_db_subnet_group.main.name
  vpc_security_group_ids = [aws_security_group.db.id]
  parameter_group_name   = aws_db_parameter_group.main.name

  # Daily COW snapshots of the data volume. Retention caps at 7 days; 0 turns
  # automated backups off. Point-in-time recovery is not implemented.
  backup_retention_period = 7

  # A final snapshot pins the data volume alive until that snapshot is deleted,
  # which would outlive a `tofu destroy`. Take one for anything you care about.
  skip_final_snapshot = true
  deletion_protection = false

  apply_immediately = true

  tags = {
    Name = var.name
  }
}

# ---------------------------------------------------------------------------
# Client VM — psql inside the VPC, which is the only place the endpoint exists,
# from the client subnet rather than the endpoint's own
# ---------------------------------------------------------------------------

resource "tls_private_key" "client" {
  algorithm = "ED25519"
}

resource "aws_key_pair" "client" {
  key_name   = "${var.name}-client"
  public_key = tls_private_key.client.public_key_openssh
}

resource "local_file" "client_pem" {
  filename        = "${path.module}/${var.name}-client.pem"
  content         = tls_private_key.client.private_key_openssh
  file_permission = "0600"
}

resource "aws_instance" "client" {
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = var.instance_type
  subnet_id              = aws_subnet.client.id
  vpc_security_group_ids = [aws_security_group.client.id]
  key_name               = aws_key_pair.client.key_name

  # The password goes to ~/.pgpass at 0600 rather than into the environment, so
  # it is not readable from another user's `ps` or from /etc/profile.d.
  #
  # It is written from runcmd, not write_files: write_files runs before
  # users-groups, so a file under /home/ubuntu lands before the user exists and
  # takes the home directory root-owned with it.
  user_data = <<-USERDATA
    #cloud-config
    package_update: true
    packages:
      - postgresql-client
    write_files:
      - path: /etc/profile.d/rds-quickstart.sh
        permissions: '0644'
        content: |
          export PGHOST=${aws_db_instance.main.address}
          export PGPORT=${aws_db_instance.main.port}
          export PGUSER=${var.db_username}
          export PGDATABASE=${var.db_name}
    runcmd:
      - install -o ubuntu -g ubuntu -m 0600 /dev/null /home/ubuntu/.pgpass
      - echo '${aws_db_instance.main.address}:${aws_db_instance.main.port}:${var.db_name}:${var.db_username}:${var.db_password}' > /home/ubuntu/.pgpass
  USERDATA

  tags = {
    Name = "${var.name}-client"
  }
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------

output "note" {
  value = <<-EOT
    The endpoint is private. Connect from the client VM, not from this host:

      ssh -i ${var.name}-client.pem ubuntu@${aws_instance.client.public_ip}
      psql -c 'SELECT version();'

    cloud-init installs postgresql-client on first boot, so give the client a
    minute after apply before the first psql.

    TLS is required, and psql negotiates it on its own, so the commands above
    just work; only a client explicitly setting sslmode=disable is refused. To
    verify the certificate as well, fetch the cluster CA in the client with

      curl -fsS http://169.254.169.254/spinifex/ca.pem -o ~/ca.pem

    and add sslmode=verify-full sslrootcert=~/ca.pem. Both the endpoint name
    and the endpoint IP are in the certificate's SAN set.
  EOT
}

output "db_endpoint" {
  description = "host:port. Reachable only from inside the VPC."
  value       = aws_db_instance.main.endpoint
}

output "db_address" {
  value = aws_db_instance.main.address
}

output "db_identifier" {
  value = aws_db_instance.main.identifier
}

output "client_public_ip" {
  value = aws_instance.client.public_ip
}

output "ssh_to_client" {
  value = "ssh -i ${var.name}-client.pem ubuntu@${aws_instance.client.public_ip}"
}
