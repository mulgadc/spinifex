---
title: "S3-Backed Web App"
description: "Deploy a Flask file-sharing web application backed by S3 (Predastore) using Terraform on Spinifex."
category: "Terraform Workbooks"
tags:
  - terraform
  - s3
  - predastore
  - flask
  - webapp
  - workbook
  - imds
  - sts
  - instance-profile
resources:
  - title: "Terraform AWS Provider"
    url: "https://registry.terraform.io/providers/hashicorp/aws/latest"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "Predastore (S3)"
    url: "https://github.com/mulgadc/predastore"
  - title: "OpenTofu"
    url: "https://opentofu.org/"
---

# Terraform: S3-Backed Web App

> Deploy a Flask file-sharing web application backed by S3 (Predastore) using Terraform on Spinifex.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

Deploy an EC2 instance running a Flask file-sharing web application backed by S3 (Predastore). Users can upload files through a web form and browse uploaded content — demonstrating Terraform managing both compute and object storage together.

This workbook uses the idiomatic AWS credential model: the instance is launched with an **IAM instance profile** and the app pulls short-lived STS credentials from **IMDS** (`169.254.169.254`) through boto3's default credential chain. **No long-lived S3 keys are baked into the instance.** The Terraform run still uses the operator's admin credentials (to create the bucket, role, profile, and instance), but the running instance authenticates to S3 with credentials it fetches at runtime and which expire in ~1 hour.

**Architecture:**

<p align="center">
  <img src="../../../.github/assets/diagrams/tf-s3-webapp.svg" alt="S3 webapp — browser to Flask EC2 instance; the instance fetches short-lived STS credentials from IMDS (169.254.169.254) via its instance profile, then calls Predastore over the S3 API" width="900">
</p>

**What you'll learn:**

- Configuring the AWS provider with both Spinifex and Predastore endpoints
- Creating S3 buckets on Predastore via Terraform
- Defining an IAM role, least-privilege managed policy, and instance profile, and passing the role to an instance (`iam:PassRole`)
- How the instance obtains short-lived credentials from IMDS via boto3's default credential chain — no static keys on the instance
- Deploying a Python webapp with cloud-init that signs S3 requests with the STS session token

**Prerequisites:**

- Spinifex installed and running (see [Installing Spinifex](/docs/install))
- Predastore running (S3 API on port 8443)
- An Ubuntu 26.04 AMI imported (see [Setting Up Your Cluster](/docs/setting-up-your-cluster))
- OpenTofu or Terraform installed
- The operator identity running the Terraform apply (`AWS_PROFILE=spinifex`) must be allowed to manage IAM and pass the role: `iam:CreateRole`, `iam:CreatePolicy`, `iam:AttachRolePolicy`, `iam:CreateInstanceProfile`, `iam:AddRoleToInstanceProfile`, and `iam:PassRole` on the new role. The bootstrap admin profile satisfies this.
- The EC2 instance must be able to reach Predastore — use the host's br-wan IP, not localhost

## Instructions

### Step 1. Get the Template

Clone the Terraform examples from the Spinifex repository:

```bash
git clone --depth 1 --filter=blob:none --sparse https://github.com/mulgadc/spinifex.git spinifex-tf
cd spinifex-tf
git sparse-checkout set docs/terraform
cd docs/terraform/s3-webapp
```

Or create the files manually and paste the full configuration below.

### Step 2. Create terraform.tfvars

Before deploying, create a `terraform.tfvars` with your Predastore credentials. The `predastore_host` must be reachable from inside the VPC — use the host's br-wan or LAN IP, not localhost.

<!-- INCLUDE: terraform.tfvars.example lang:hcl -->

### Step 3. Create main.tf

<!-- INCLUDE: main.tf lang:hcl -->

### Step 4. Deploy

The workbook defaults to `t3.small` (2 vCPU, 2 GiB). On clusters without that type registered, override with `TF_VAR_instance_type` — query what's available with `aws ec2 describe-instance-types`.

```bash
export AWS_PROFILE=spinifex
tofu init
tofu apply
```

### Step 5. Test the Application

> **Note:** EC2 instances can take 30+ seconds to boot after apply. If SSH or HTTP is unreachable, wait and retry.

Open the `web_url` output in your browser. You should see the file browser UI. Upload a file and verify it appears in the list.

```bash
# Verify via CLI
curl http://<public_ip>

# Check the S3 bucket directly
aws s3 ls s3://webapp-uploads/ --profile spinifex --endpoint-url https://localhost:8443
```

### Clean Up

```bash
tofu destroy
```

## Troubleshooting

### Predastore Connection Refused from Instance

The EC2 instance cannot reach `localhost` on the host. Set `predastore_host` to the host's br-wan or LAN IP address:

```hcl
predastore_host = "192.168.1.10:8443"
```

### S3 Bucket Creation Fails

Verify Predastore is running and accessible:

```bash
curl -k https://localhost:8443/
aws s3 ls --profile spinifex --endpoint-url https://localhost:8443
```

### Flask App Not Starting

SSH into the instance and check the service:

```bash
ssh -i s3-webapp-demo.pem ubuntu@<public_ip>
sudo systemctl status s3-webapp
sudo journalctl -u s3-webapp --no-pager -n 50
```

### Upload Fails with `403 AccessDenied`

The instance reached S3 but was not authorized. The `.env` file deliberately holds **no** keys — credentials come from IMDS:

```bash
ssh -i s3-webapp-demo.pem ubuntu@<public_ip>
cat /opt/webapp/.env   # S3_ENDPOINT / S3_BUCKET / S3_REGION only — no keys
```

Check that:

- the managed policy is attached to the role (`s3:ListBucket` on the bucket, `s3:GetObject`/`s3:PutObject` on its objects), and the policy's bucket name matches `bucket_name`;
- the assumed-role session's account matches the bucket owner's account — Predastore enforces bucket ownership. Both are created by the same operator identity, so they align by construction; a mismatch surfaces as `403`.

### `NoCredentialsError` / No Credentials

boto3 could not obtain credentials, which means the IMDS credential chain did not resolve. From the instance:

```bash
ssh -i s3-webapp-demo.pem ubuntu@<public_ip>

# IMDSv2: fetch a token, then the role name behind the instance profile
TOKEN=$(curl -sX PUT http://169.254.169.254/latest/api/token \
  -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')
curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/iam/security-credentials/

# Confirm the resolved identity — expect assumed-role/s3-webapp-role/<instance-id>
aws sts get-caller-identity --endpoint-url https://<predastore_host>
```

If the role name is empty, the instance profile was not attached — confirm `iam_instance_profile` on the instance and that `iam:PassRole` is allowed for the operator.

### AMI Not Found

Ensure you have imported an Ubuntu 26.04 image:

```bash
aws ec2 describe-images --owners 000000000000 --profile spinifex
```
