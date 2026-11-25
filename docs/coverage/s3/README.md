---
title: "S3 API Coverage"
seoTitle: "Amazon S3 API Coverage on Spinifex — Spinifex Docs"
description: "The Amazon S3 API operations Predastore serves on the platform's S3 endpoint, covering buckets, objects, multipart uploads and the policies that guard them."
category: "Coverage"
sections:
  - overview
tags:
  - aws
  - compatibility
  - coverage
  - s3
  - storage
  - objects
---

# S3 API Coverage

## Overview

Predastore implements **26 operations** in the S3 `2006-03-01` API model.

### Routed is not the same as conforming

This page shows which operations are routed to a handler.
Behaviour differences from S3 are measured with the `ceph/s3-tests` suite and published in Predastore's [S3 compatibility report](https://github.com/mulgadc/predastore/blob/dev/docs/S3-COMPATIBILITY.md).

### Operations

| Operation |
|---|
| `AbortMultipartUpload` |
| `CompleteMultipartUpload` |
| `CopyObject` |
| `CreateBucket` |
| `CreateMultipartUpload` |
| `DeleteBucket` |
| `DeleteBucketTagging` |
| `DeleteObject` |
| `DeleteObjects` |
| `GetBucketLocation` |
| `GetBucketTagging` |
| `GetBucketVersioning` |
| `GetObject` |
| `HeadBucket` |
| `HeadObject` |
| `ListBuckets` |
| `ListMultipartUploads` |
| `ListObjectVersions` |
| `ListObjects` |
| `ListObjectsV2` |
| `ListParts` |
| `PutBucketTagging` |
| `PutBucketVersioning` |
| `PutObject` |
| `UploadPart` |
| `UploadPartCopy` |
