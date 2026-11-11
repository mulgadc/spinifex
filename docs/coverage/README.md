---
title: "AWS API Coverage"
seoTitle: "Which AWS API Operations Spinifex Implements — Spinifex Docs"
description: "The AWS API operations Spinifex serves, generated from its gateway dispatch tables and the pinned AWS SDK service models on each build, service by service."
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

The platform serves **406 operations** across the AWS APIs below. Every page names the operations its service implements from the pinned model, generated from the dispatch tables on each build rather than written by hand.

| Service | Operations | Requires AWS SDK v2 |
|---|---:|---:|
| [ACM](/coverage/acm) | 9 | 25 |
| [EC2](/coverage/ec2) | 127 | 186 |
| [ECR](/coverage/ecr) | 23 | 15 |
| [ECS](/coverage/ecs) | 31 | 32 |
| [EKS](/coverage/eks) | 27 | 14 |
| [ELBv2](/coverage/elbv2) | 36 | 3 |
| [IAM](/coverage/iam) | 82 | 21 |
| [RDS](/coverage/rds) | 27 | 6 |
| [Route 53](/coverage/route53) | 13 | 1 |
| [S3](/coverage/s3) | 26 | 17 |
| [STS](/coverage/sts) | 5 | 3 |
| **Total** | **406** | **323** |

A further **323 operations** in the pinned models are not in AWS SDK for Go v1, the SDK Spinifex is built and tested on. They are out of scope until Spinifex moves to AWS SDK v2; each service page lists its own under Requires AWS SDK v2.
