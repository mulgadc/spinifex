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

### Import and issuance

Spinifex both stores externally-issued certificates and issues its own for load balancer listener references. Certificates are account-scoped, and a delete is refused while any listener still references the ARN — there is no force flag, matching AWS.

`RequestCertificate` mints an ARN immediately and returns `PENDING_VALIDATION`; it never issues inline except against a tenant private CA, which has no domain to validate. The validation mode is derived from deployment state rather than configured: the DNS provider API where a credential exists, a manual TXT record where the platform hosts the zone, and a private CA otherwise — the only option for a deployment with no publicly delegated domain.

Terraform's canonical certificate, DNS record and validation flow works unmodified in every mode. Where Spinifex owns the record write it emits no `ResourceRecord`, so iterating the validation options yields zero records and the validation resource still blocks correctly by polling until the certificate is issued.

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
