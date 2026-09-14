---
title: "AWS API Coverage"
seoTitle: "Which AWS API Operations Spinifex Implements — Spinifex Docs"
description: "Operation-level coverage of every AWS API Spinifex serves, generated from its gateway dispatch tables and the pinned AWS SDK service models on each build."
category: "Coverage"
sections:
  - overview
tags:
  - aws
  - compatibility
  - coverage
  - api
  - operations
---

# AWS API Coverage

## Overview

Spinifex serves the AWS APIs below. Every page counts the operations in the pinned `aws-sdk-go v1.55.8` `api-2.json` model for its service and reports, operation by operation, whether Spinifex implements it.

| Service | Implemented | Modelled | Coverage |
|---|---:|---:|---:|
| [ACM](/coverage/acm) | 9 | 15 | 60.0% |
| [EC2](/coverage/ec2) | 125 | 625 | 20.0% |
| [ECR](/coverage/ecr) | 19 | 47 | 40.4% |
| [ECS](/coverage/ecs) | 31 | 56 | 55.4% |
| [EKS](/coverage/eks) | 34 | 56 | 60.7% |
| [ELBv2](/coverage/elbv2) | 34 | 46 | 73.9% |
| [IAM](/coverage/iam) | 75 | 159 | 47.2% |
| [RDS](/coverage/rds) | 26 | 162 | 16.0% |
| [STS](/coverage/sts) | 5 | 8 | 62.5% |
