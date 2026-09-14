---
title: "STS API Coverage"
seoTitle: "AWS Security Token Service API Coverage — Spinifex Docs"
description: "Every operation in the AWS STS API model and whether Spinifex implements it, covering role assumption, session tokens and web identity federation (IRSA)."
category: "Coverage"
sections:
  - overview
tags:
  - aws
  - compatibility
  - coverage
  - sts
  - identity
  - credentials
---

# STS API Coverage

## Overview

Spinifex implements **5 of the 8** operations (**62.5%**) in the STS `2011-06-15` API model.

### Trust policies

Trust policies are validated at write time rather than silently narrowed at assume time. `NotPrincipal`, `NotAction`, empty-string `Action` elements and empty `Principal` blocks are all rejected as malformed.

`Condition` blocks are rejected except on `AssumeRoleWithWebIdentity` with `StringEquals`, which is the shape IRSA needs and which Spinifex evaluates at assume time against the token's issuer, subject and audience. Anything wider is refused rather than accepted and ignored, because an accepted-but-unevaluated condition is a silent over-grant.

### Parameters that are refused, not ignored

Several inputs the model describes are deliberately rejected rather than accepted as no-ops: inline session policies and policy ARNs, session tags, and MFA serial numbers and token codes. Each of them would otherwise appear to restrict or strengthen a session that in fact carries the role's full permissions.

### Operations

| Operation | Status |
|---|---|
| `AssumeRole` | ✅ Implemented |
| `AssumeRoleWithSAML` | ❌ Not implemented |
| `AssumeRoleWithWebIdentity` | ✅ Implemented |
| `DecodeAuthorizationMessage` | ❌ Not implemented |
| `GetAccessKeyInfo` | ✅ Implemented |
| `GetCallerIdentity` | ✅ Implemented |
| `GetFederationToken` | ❌ Not implemented |
| `GetSessionToken` | ✅ Implemented |
