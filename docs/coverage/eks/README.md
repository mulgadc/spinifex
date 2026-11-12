---
title: "EKS API Coverage"
seoTitle: "Elastic Kubernetes Service API Coverage — Spinifex Docs"
description: "The Amazon EKS API operations Spinifex implements, covering clusters, nodegroups, add-ons, access entries and the identity providers they authenticate."
category: "Coverage"
sections:
  - overview
tags:
  - aws
  - compatibility
  - coverage
  - eks
  - kubernetes
  - containers
---

# EKS API Coverage

## Overview

Spinifex implements **27 operations** in the EKS `2017-11-01` API model.

### Operations

| Operation |
|---|
| `AssociateAccessPolicy` |
| `CreateAccessEntry` |
| `CreateAddon` |
| `CreateCluster` |
| `CreateNodegroup` |
| `DeleteAccessEntry` |
| `DeleteAddon` |
| `DeleteCluster` |
| `DeleteNodegroup` |
| `DescribeAccessEntry` |
| `DescribeAddon` |
| `DescribeAddonVersions` |
| `DescribeCluster` |
| `DescribeNodegroup` |
| `DisassociateAccessPolicy` |
| `ListAccessEntries` |
| `ListAccessPolicies` |
| `ListAddons` |
| `ListAssociatedAccessPolicies` |
| `ListClusters` |
| `ListNodegroups` |
| `ListTagsForResource` |
| `TagResource` |
| `UntagResource` |
| `UpdateAccessEntry` |
| `UpdateAddon` |
| `UpdateNodegroupConfig` |

### Requires AWS SDK v2

These 14 operations are in the pinned AWS model but not in AWS SDK for Go v1, the SDK Spinifex is built and tested on. They are out of scope until Spinifex moves to AWS SDK v2.

| Operation |
|---|
| `ActivateCertificateAuthority` |
| `CancelUpdate` |
| `CreateCapability` |
| `CreateCertificateAuthority` |
| `DeleteCapability` |
| `DeleteCertificateAuthority` |
| `DescribeCapability` |
| `DescribeCertificateAuthority` |
| `DescribeClusterVersions` |
| `DescribeInsightsRefresh` |
| `ListCapabilities` |
| `ListCertificateAuthorities` |
| `StartInsightsRefresh` |
| `UpdateCapability` |
