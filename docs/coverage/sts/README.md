---
title: "STS API Coverage"
seoTitle: "AWS Security Token Service API Coverage — Spinifex Docs"
description: "The AWS STS API operations Spinifex implements, covering role assumption, session tokens and web identity federation (IRSA), with those it does not offer."
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

Spinifex implements **5 operations** in the STS `2011-06-15` API model.

### Trust policies

Trust policies are validated when they are written.
`NotPrincipal`, `NotAction`, empty `Action` elements and empty `Principal` blocks are rejected as malformed.

`Condition` blocks are rejected, except `StringEquals` on `AssumeRoleWithWebIdentity`, which IRSA uses.
Spinifex evaluates that condition against the token's issuer, subject and audience.

### Parameters that are refused

Session policies, policy ARNs, session tags, and MFA serial numbers and token codes are rejected.
A session always has the full permissions of its role.

### Operations

| Operation |
|---|
| `AssumeRole` |
| `AssumeRoleWithWebIdentity` |
| `GetAccessKeyInfo` |
| `GetCallerIdentity` |
| `GetSessionToken` |
