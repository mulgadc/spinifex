---
title: "Moving an AWS Workload to Mulga"
description: "Migrate existing AWS workloads to Spinifex using compatible APIs, SDKs, and Terraform."
category: "Migration"
tags:
  - migration
  - aws
  - terraform
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "AWS CLI Reference"
    url: "https://docs.aws.amazon.com/cli/latest/reference/"
  - title: "Terraform AWS Provider"
    url: "https://registry.terraform.io/providers/hashicorp/aws/latest"
---

# Moving an AWS Workload to Mulga

> Migrate existing AWS workloads to Spinifex using compatible APIs, SDKs, and Terraform.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [Troubleshooting](#troubleshooting)

---

## Overview

Spinifex provides drop-in compatibility with AWS APIs, making it possible to migrate existing workloads with minimal changes.

**Supported Services:** EC2, VPC, EBS, S3, IAM, STS, ELBv2 (ALB/NLB), ACM, ECR, ECS, EKS

**Compatible Tools:** AWS CLI, AWS SDKs, Terraform, kubectl (via `aws eks get-token`), any S3-compatible client

## Instructions

## Prerequisites

Ensure the AWS profile is set:

```bash
export AWS_PROFILE=spinifex
```

## Configure AWS CLI

```bash
aws ec2 describe-instances
aws s3 ls
```

## Terraform

```hcl
provider "aws" {
  region     = "ap-southeast-2"
  access_key = "your-spinifex-access-key"
  secret_key = "your-spinifex-secret-key"

  endpoints {
    ec2                = "https://localhost:9999"
    iam                = "https://localhost:9999"
    sts                = "https://localhost:9999"
    elbv2              = "https://localhost:9999"
    acm                = "https://localhost:9999"
    ecr                = "https://localhost:9999"
    ecs                = "https://localhost:9999"
    eks                = "https://localhost:9999"
    s3                 = "https://localhost:8443"
  }

  skip_credentials_validation = true
  skip_metadata_api_check     = true
  skip_requesting_account_id  = true
  skip_region_validation      = true
}
```

## Hybrid S3 Sync

```bash
aws s3 sync s3://local-bucket/ s3://cloud-bucket/ --source-region spinifex --region us-east-1
```

## Troubleshooting

## Terraform Provider Errors

Ensure all four skip flags are set in your provider configuration:

```hcl
skip_credentials_validation = true
skip_metadata_api_check     = true
skip_requesting_account_id  = true
skip_region_validation      = true
```

Without these, Terraform will try to validate credentials and metadata against real AWS endpoints.

## S3 Signature Errors

Spinifex uses AWS Signature V4. Ensure your AWS CLI is version 2.0 or higher:

```bash
aws --version
```

If using an older version, upgrade:

```bash
curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip"
unzip awscliv2.zip
sudo ./aws/install --update
```
