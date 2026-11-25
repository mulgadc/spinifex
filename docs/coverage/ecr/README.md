---
title: "ECR API Coverage"
seoTitle: "Elastic Container Registry API Coverage — Spinifex Docs"
description: "The Amazon ECR API operations Spinifex implements, alongside the OCI distribution endpoint that carries the image layers for every repository it serves."
category: "Coverage"
sections:
  - overview
tags:
  - aws
  - compatibility
  - coverage
  - ecr
  - containers
  - registry
---

# ECR API Coverage

## Overview

Spinifex implements **23 operations** in the ECR `2015-09-21` API model.

### Layer transfer and replication are stubs

`BatchCheckLayerAvailability`, `InitiateLayerUpload`, `UploadLayerPart`, `CompleteLayerUpload` and `GetDownloadUrlForLayer` are stubs.
`docker` and other OCI clients push and pull over the `/v2/` endpoint on the same host, after `docker login` with the token from `GetAuthorizationToken`.

Registry replication is a stub, because a deployment has no second region to replicate to.

### Operations

| Operation |
|---|
| `BatchDeleteImage` |
| `BatchGetImage` |
| `CreateRepository` |
| `DeleteLifecyclePolicy` |
| `DeleteRepository` |
| `DeleteRepositoryPolicy` |
| `DescribeImages` |
| `DescribeRegistry` |
| `DescribeRepositories` |
| `GetAuthorizationToken` |
| `GetLifecyclePolicy` |
| `GetLifecyclePolicyPreview` |
| `GetRepositoryPolicy` |
| `ListImages` |
| `ListTagsForResource` |
| `PutImage` |
| `PutImageScanningConfiguration` |
| `PutImageTagMutability` |
| `PutLifecyclePolicy` |
| `SetRepositoryPolicy` |
| `StartLifecyclePolicyPreview` |
| `TagResource` |
| `UntagResource` |
