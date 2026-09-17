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

Predastore implements **23 operations** in the S3 `2006-03-01` API model.

### Predastore serves this endpoint

S3 is the one surface the AWS gateway does not answer itself. Object storage runs on [Predastore](https://github.com/mulgadc/predastore), which serves the S3 REST API directly over its own endpoint.

### Routed is not the same as conforming

This page says an operation is routed to a handler. It does not say the handler's behaviour matches S3 in every case.

That behaviour is measured separately, against the `ceph/s3-tests` suite Ceph RGW, MinIO and Garage are all validated with, and the results are published in Predastore's [S3 compatibility report](https://github.com/mulgadc/predastore/blob/dev/docs/S3-COMPATIBILITY.md).

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
| `GetObject` |
| `HeadBucket` |
| `HeadObject` |
| `ListBuckets` |
| `ListMultipartUploads` |
| `ListObjects` |
| `ListObjectsV2` |
| `ListParts` |
| `PutBucketTagging` |
| `PutObject` |
| `UploadPart` |
| `UploadPartCopy` |
