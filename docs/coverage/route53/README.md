---
title: "Route 53 API Coverage"
seoTitle: "Amazon Route 53 API Coverage on Spinifex — Spinifex Docs"
description: "The Amazon Route 53 API operations Spinifex implements, covering the hosted zones it serves, their resource record sets, change status and tags on each zone."
category: "Coverage"
sections:
  - overview
tags:
  - aws
  - compatibility
  - coverage
  - route53
  - dns
---

# Route 53 API Coverage

## Overview

Spinifex implements **13 operations** in the Route 53 `2013-04-01` API model.

### Operations

| Operation |
|---|
| `ChangeResourceRecordSets` |
| `ChangeTagsForResource` |
| `CreateHostedZone` |
| `DeleteHostedZone` |
| `GetChange` |
| `GetHostedZone` |
| `GetHostedZoneCount` |
| `ListHostedZones` |
| `ListHostedZonesByName` |
| `ListResourceRecordSets` |
| `ListTagsForResource` |
| `ListTagsForResources` |
| `UpdateHostedZoneComment` |

### Requires AWS SDK v2

These 1 operations are in the pinned AWS model but not in AWS SDK for Go v1, the SDK Spinifex is built and tested on. They are out of scope until Spinifex moves to AWS SDK v2.

| Operation |
|---|
| `UpdateHostedZoneFeatures` |
