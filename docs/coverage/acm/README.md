---
title: "ACM API Coverage"
seoTitle: "AWS Certificate Manager API Coverage — Spinifex Docs"
description: "The AWS Certificate Manager API operations Spinifex implements, generated from the gateway dispatch tables and the pinned AWS service model on each build."
category: "Coverage"
sections:
  - overview
tags:
  - aws
  - compatibility
  - coverage
  - acm
  - certificates
  - tls
---

# ACM API Coverage

## Overview

Spinifex implements **9 operations** in the ACM `2015-12-08` API model.

### Validation

`RequestCertificate` ignores `ValidationMethod`, and `EMAIL` validation is not offered.
The platform selects the method from its configuration:

| Platform configuration | Validation | `ResourceRecord` |
| --- | --- | --- |
| DNS provider credential | The platform writes the record | None |
| Hosted zone, no provider credential | You publish a TXT record at `_acme-challenge.<domain>.` | TXT, not the CNAME that AWS returns |
| No public domain | Issued at once by a private CA | None |

A certificate validated by a manual TXT record is not renewed automatically.

### Operations

| Operation |
|---|
| `AddTagsToCertificate` |
| `DeleteCertificate` |
| `DescribeCertificate` |
| `GetCertificate` |
| `ImportCertificate` |
| `ListCertificates` |
| `ListTagsForCertificate` |
| `RemoveTagsFromCertificate` |
| `RequestCertificate` |
