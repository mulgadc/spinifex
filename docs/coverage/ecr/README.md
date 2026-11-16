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

### Two endpoints, one registry

Repository metadata is served over the AWS API on the gateway endpoint. Image data moves over the OCI Distribution `/v2/` endpoint on that same host, authenticated by the bearer token `GetAuthorizationToken` mints for `docker login`.

The split explains the stubs below. The layer-transfer operations — `BatchCheckLayerAvailability`, `InitiateLayerUpload`, `UploadLayerPart`, `CompleteLayerUpload` and `GetDownloadUrlForLayer` — are registered stubs because the `/v2/` endpoint carries that traffic instead. A client using `docker` or any OCI-compatible tool never calls them.

Registry replication is a stub for the same kind of reason: a deployment is a single registry, with no cross-region peer to replicate to. `DescribeRegistry` reports this as a replication configuration with no rules.

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
