# Example 3: S3-Backed Web Application
#
# Deploys an EC2 instance running a simple file-sharing webapp backed by S3
# (Predastore). Users can upload files through a web form and browse uploaded
# content — demonstrating Terraform managing both compute (Spinifex) and
# object storage (Predastore) resources together.
#
# Architecture:
#
#   Browser ──HTTP──▶ EC2 Instance (Flask webapp, port 80)
#                         │
#                         ▼ S3 API (boto3)
#                     Predastore (port 8443)
#
# Prerequisites:
#   - Spinifex services running (gateway on port 9999)
#   - Predastore running (S3 API on port 8443)
#   - The EC2 instance must be able to reach the Predastore endpoint.
#     Set `predastore_host` to the IP reachable from inside the VPC
#     (e.g. the host's br-wan IP, NOT localhost).
#
# Usage:
#   cd spinifex/scripts/iac/aws/examples/03-s3-webapp
#   export AWS_PROFILE=spinifex
#   tofu init && tofu apply
#
# After apply:
#   curl http://<public_ip>          # File browser UI
#   ssh -i s3-webapp-demo.pem ec2-user@<public_ip>

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
  description = "Spinifex AWS gateway endpoint (EC2/IAM)"
}

variable "predastore_endpoint" {
  type        = string
  default     = "https://127.0.0.1:8443"
  description = "Predastore S3 endpoint (for Terraform to create buckets)"
}

variable "predastore_host" {
  type        = string
  description = "Predastore host:port reachable from inside the VPC (e.g. 192.168.1.10:8443)"
}

variable "s3_access_key" {
  type        = string
  description = "S3 access key for Predastore"
}

variable "s3_secret_key" {
  type        = string
  sensitive   = true
  description = "S3 secret key for Predastore"
}

variable "bucket_name" {
  type    = string
  default = "webapp-uploads"
}

# ---------------------------------------------------------------------------
# Provider — EC2 via Spinifex gateway, S3 via Predastore
# ---------------------------------------------------------------------------

provider "aws" {
  region     = var.region
  access_key = var.s3_access_key
  secret_key = var.s3_secret_key

  endpoints {
    ec2 = var.spinifex_endpoint
    s3  = var.predastore_endpoint
    iam = var.spinifex_endpoint
    sts = var.spinifex_endpoint
  }

  s3_use_path_style = true

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
  owners      = ["000000000000"]

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
# S3 Bucket (created on Predastore)
# ---------------------------------------------------------------------------

resource "aws_s3_bucket" "uploads" {
  bucket = var.bucket_name
}

# ---------------------------------------------------------------------------
# SSH Key Pair
# ---------------------------------------------------------------------------

resource "tls_private_key" "webapp" {
  algorithm = "ED25519"
}

resource "aws_key_pair" "webapp" {
  key_name   = "s3-webapp-demo"
  public_key = tls_private_key.webapp.public_key_openssh
}

resource "local_file" "webapp_pem" {
  filename        = "${path.module}/s3-webapp-demo.pem"
  content         = tls_private_key.webapp.private_key_openssh
  file_permission = "0600"
}

# ---------------------------------------------------------------------------
# VPC + Public Subnet
# ---------------------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = "10.30.0.0/16"
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = {
    Name = "s3-webapp-demo-vpc"
  }
}

resource "aws_internet_gateway" "igw" {
  vpc_id = aws_vpc.main.id

  tags = {
    Name = "s3-webapp-demo-igw"
  }
}

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.30.1.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = {
    Name = "s3-webapp-demo-public"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.igw.id
  }

  tags = {
    Name = "s3-webapp-demo-public-rt"
  }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

# ---------------------------------------------------------------------------
# Security Group — SSH + HTTP inbound, all outbound
# ---------------------------------------------------------------------------

resource "aws_security_group" "webapp" {
  name        = "s3-webapp-demo-sg"
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
    Name = "s3-webapp-demo-sg"
  }
}

# ---------------------------------------------------------------------------
# EC2 Instance — Flask webapp that talks to Predastore S3
# ---------------------------------------------------------------------------

resource "aws_instance" "webapp" {
  ami           = data.aws_ami.ubuntu.id
  instance_type = var.instance_type

  subnet_id              = aws_subnet.public.id
  vpc_security_group_ids = [aws_security_group.webapp.id]
  key_name               = aws_key_pair.webapp.key_name

  associate_public_ip_address = true

  user_data_base64 = base64encode(<<-USERDATA
    #!/bin/bash
    set -euo pipefail

    # Install dependencies
    apt-get update -y
    apt-get install -y python3-pip python3-venv

    # Create app directory and virtualenv
    mkdir -p /opt/webapp
    python3 -m venv /opt/webapp/venv
    /opt/webapp/venv/bin/pip install flask boto3

    # Write S3 credentials config
    cat > /opt/webapp/.env <<'ENVFILE'
    S3_ENDPOINT=https://${var.predastore_host}
    S3_BUCKET=${var.bucket_name}
    S3_ACCESS_KEY=${var.s3_access_key}
    S3_SECRET_KEY=${var.s3_secret_key}
    S3_REGION=${var.region}
    ENVFILE

    # Write the Flask application
    cat > /opt/webapp/app.py <<'PYEOF'
    import os, io, urllib3
    from flask import Flask, request, redirect, url_for, Response

    # Suppress TLS warnings for self-signed certs
    urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

    # Load env
    env = {}
    with open("/opt/webapp/.env") as f:
        for line in f:
            line = line.strip()
            if "=" in line and not line.startswith("#"):
                k, v = line.split("=", 1)
                env[k] = v

    import boto3
    from botocore.config import Config

    s3 = boto3.client(
        "s3",
        endpoint_url=env["S3_ENDPOINT"],
        aws_access_key_id=env["S3_ACCESS_KEY"],
        aws_secret_access_key=env["S3_SECRET_KEY"],
        region_name=env["S3_REGION"],
        verify=False,
        config=Config(s3={"addressing_style": "path"}),
    )

    BUCKET = env["S3_BUCKET"]
    app = Flask(__name__)

    @app.route("/")
    def index():
        # List objects in the bucket
        try:
            resp = s3.list_objects_v2(Bucket=BUCKET)
            objects = resp.get("Contents", [])
        except Exception as e:
            objects = []

        rows = ""
        for obj in objects:
            key = obj["Key"]
            size = obj["Size"]
            rows += f'<tr><td><a href="/files/{key}">{key}</a></td><td>{size} bytes</td></tr>\n'

        return f"""<!DOCTYPE html>
    <html>
    <head><title>Spinifex S3 File Browser</title></head>
    <body style="font-family: sans-serif; max-width: 700px; margin: 40px auto;">
      <h1>Spinifex S3 File Browser</h1>
      <p>Bucket: <code>{BUCKET}</code></p>

      <h2>Upload a File</h2>
      <form method="POST" action="/upload" enctype="multipart/form-data">
        <input type="file" name="file" required>
        <button type="submit">Upload</button>
      </form>

      <h2>Files</h2>
      <table border="1" cellpadding="6" cellspacing="0" style="border-collapse: collapse;">
        <tr><th>Key</th><th>Size</th></tr>
        {rows if rows else '<tr><td colspan="2">No files yet</td></tr>'}
      </table>

      <hr>
      <p><small>Powered by Spinifex + Predastore</small></p>
    </body>
    </html>"""

    @app.route("/upload", methods=["POST"])
    def upload():
        f = request.files.get("file")
        if not f or not f.filename:
            return redirect("/")
        s3.put_object(Bucket=BUCKET, Key=f.filename, Body=f.read())
        return redirect("/")

    @app.route("/files/<path:key>")
    def download(key):
        try:
            obj = s3.get_object(Bucket=BUCKET, Key=key)
            return Response(
                obj["Body"].read(),
                headers={"Content-Disposition": f'inline; filename="{key}"'},
            )
        except Exception:
            return "Not found", 404

    if __name__ == "__main__":
        app.run(host="0.0.0.0", port=80)
    PYEOF

    # Create a systemd service so the webapp starts on boot
    cat > /etc/systemd/system/s3-webapp.service <<'SVCEOF'
    [Unit]
    Description=S3 File Browser Webapp
    After=network.target

    [Service]
    Type=simple
    ExecStart=/opt/webapp/venv/bin/python /opt/webapp/app.py
    WorkingDirectory=/opt/webapp
    Restart=always
    RestartSec=3

    [Install]
    WantedBy=multi-user.target
    SVCEOF

    systemctl daemon-reload
    systemctl enable s3-webapp
    systemctl start s3-webapp
  USERDATA
  )

  tags = {
    Name = "s3-webapp-demo"
  }
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------

output "note" {
  value = "EC2 instances can take 30+ seconds to boot after apply. If SSH or HTTP is unreachable, wait and retry."
}

output "instance_id" {
  value = aws_instance.webapp.id
}

output "public_ip" {
  value = aws_instance.webapp.public_ip
}

output "bucket_name" {
  value = aws_s3_bucket.uploads.id
}

output "ssh_command" {
  value = "ssh -i s3-webapp-demo.pem ec2-user@${aws_instance.webapp.public_ip}"
}

output "web_url" {
  value = "http://${aws_instance.webapp.public_ip}"
}
