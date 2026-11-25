---
title: "ELBv2 API Coverage"
seoTitle: "Elastic Load Balancing v2 API Coverage — Spinifex Docs"
description: "The Elastic Load Balancing v2 API operations Spinifex implements, for both the Application and Network Load Balancers it serves, listeners and target groups."
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

Spinifex implements **34 operations** in the ELBv2 `2015-12-01` API model.

### Initial targets receive traffic

A target in the `initial` state receives traffic before its first successful health check.
On AWS, a target receives no traffic until it passes.

### Unused attributes

Only the `X-Forwarded-For` attributes of `routing.http.*` change the data plane.
Other `routing.http.*` attributes are stored and returned by `DescribeLoadBalancerAttributes`, but have no effect.

### Operations

| Operation |
|---|
| `AddListenerCertificates` |
| `AddTags` |
| `CreateListener` |
| `CreateLoadBalancer` |
| `CreateRule` |
| `CreateTargetGroup` |
| `DeleteListener` |
| `DeleteLoadBalancer` |
| `DeleteRule` |
| `DeleteTargetGroup` |
| `DeregisterTargets` |
| `DescribeAccountLimits` |
| `DescribeListenerCertificates` |
| `DescribeListeners` |
| `DescribeLoadBalancerAttributes` |
| `DescribeLoadBalancers` |
| `DescribeRules` |
| `DescribeSSLPolicies` |
| `DescribeTags` |
| `DescribeTargetGroupAttributes` |
| `DescribeTargetGroups` |
| `DescribeTargetHealth` |
| `ModifyListener` |
| `ModifyLoadBalancerAttributes` |
| `ModifyRule` |
| `ModifyTargetGroup` |
| `ModifyTargetGroupAttributes` |
| `RegisterTargets` |
| `RemoveListenerCertificates` |
| `RemoveTags` |
| `SetIpAddressType` |
| `SetRulePriorities` |
| `SetSecurityGroups` |
| `SetSubnets` |
