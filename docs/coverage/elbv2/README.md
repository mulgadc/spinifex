---
title: "ELBv2 API Coverage"
seoTitle: "Elastic Load Balancing v2 API Coverage — Spinifex Docs"
description: "Every operation in the Elastic Load Balancing v2 API model and whether Spinifex implements it, for both the Application and Network Load Balancers it serves."
category: "Coverage"
sections:
  - overview
tags:
  - aws
  - compatibility
  - coverage
  - elbv2
  - load balancing
  - networking
---

# ELBv2 API Coverage

## Overview

Spinifex implements **34 of the 46** operations (**73.9%**) in the ELBv2 `2015-12-01` API model.

### Two data planes

The data plane is a system-managed load balancer VM, launched automatically during `CreateLoadBalancer`. Application Load Balancers run HAProxy for the L7 surface — rules, fixed responses and redirects over HTTP and HTTPS. Network Load Balancers run nginx `stream` for the L4 surface — TCP, UDP, TLS and TCP_UDP — because HAProxy cannot load-balance UDP. The agent selects the engine from the configuration the control plane delivers, so the choice follows the load balancer type and is not separately configurable.

### Operations

| Operation | Status |
|---|---|
| `AddListenerCertificates` | ✅ Implemented |
| `AddTags` | ✅ Implemented |
| `AddTrustStoreRevocations` | ❌ Not implemented |
| `CreateListener` | ✅ Implemented |
| `CreateLoadBalancer` | ✅ Implemented |
| `CreateRule` | ✅ Implemented |
| `CreateTargetGroup` | ✅ Implemented |
| `CreateTrustStore` | ❌ Not implemented |
| `DeleteListener` | ✅ Implemented |
| `DeleteLoadBalancer` | ✅ Implemented |
| `DeleteRule` | ✅ Implemented |
| `DeleteSharedTrustStoreAssociation` | ❌ Not implemented |
| `DeleteTargetGroup` | ✅ Implemented |
| `DeleteTrustStore` | ❌ Not implemented |
| `DeregisterTargets` | ✅ Implemented |
| `DescribeAccountLimits` | ✅ Implemented |
| `DescribeListenerCertificates` | ✅ Implemented |
| `DescribeListeners` | ✅ Implemented |
| `DescribeLoadBalancerAttributes` | ✅ Implemented |
| `DescribeLoadBalancers` | ✅ Implemented |
| `DescribeRules` | ✅ Implemented |
| `DescribeSSLPolicies` | ✅ Implemented |
| `DescribeTags` | ✅ Implemented |
| `DescribeTargetGroupAttributes` | ✅ Implemented |
| `DescribeTargetGroups` | ✅ Implemented |
| `DescribeTargetHealth` | ✅ Implemented |
| `DescribeTrustStoreAssociations` | ❌ Not implemented |
| `DescribeTrustStoreRevocations` | ❌ Not implemented |
| `DescribeTrustStores` | ❌ Not implemented |
| `GetResourcePolicy` | ❌ Not implemented |
| `GetTrustStoreCaCertificatesBundle` | ❌ Not implemented |
| `GetTrustStoreRevocationContent` | ❌ Not implemented |
| `ModifyListener` | ✅ Implemented |
| `ModifyLoadBalancerAttributes` | ✅ Implemented |
| `ModifyRule` | ✅ Implemented |
| `ModifyTargetGroup` | ✅ Implemented |
| `ModifyTargetGroupAttributes` | ✅ Implemented |
| `ModifyTrustStore` | ❌ Not implemented |
| `RegisterTargets` | ✅ Implemented |
| `RemoveListenerCertificates` | ✅ Implemented |
| `RemoveTags` | ✅ Implemented |
| `RemoveTrustStoreRevocations` | ❌ Not implemented |
| `SetIpAddressType` | ✅ Implemented |
| `SetRulePriorities` | ✅ Implemented |
| `SetSecurityGroups` | ✅ Implemented |
| `SetSubnets` | ✅ Implemented |
| `DescribeListenerAttributes` | 🔒 Outside the pinned model |
| `ModifyListenerAttributes` | 🔒 Outside the pinned model |
