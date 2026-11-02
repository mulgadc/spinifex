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

Spinifex implements **36 operations** in the ELBv2 `2015-12-01` API model.

### Two data planes

The data plane is a system-managed load balancer VM, launched automatically during `CreateLoadBalancer`. Application Load Balancers run HAProxy for the L7 surface — rules, fixed responses and redirects over HTTP and HTTPS. Network Load Balancers run nginx `stream` for the L4 surface — TCP, UDP, TLS and TCP_UDP — because HAProxy cannot load-balance UDP. The agent selects the engine from the configuration the control plane delivers, so the choice follows the load balancer type and is not separately configurable.

### Target health and routing

Traffic goes only to targets that are healthy or still `initial`.
A target that has failed its health check is removed from the data plane on the next render and put back when it passes again.
When every target in a group is unhealthy, the load balancer fails open and sends traffic to all of them rather than refusing it.
A Network Load Balancer renders every registered server in that case, and an Application Load Balancer falls back to unchecked backup servers that HAProxy uses only when every checked server is down.
`DescribeTargetHealth` keeps reporting the targets as unhealthy while this happens.
Draining targets and targets without an address never receive traffic.

An `initial` target receives traffic before its first successful check.
This differs from AWS, where a target takes no traffic until it passes.
Health state is reset to `initial` for every target when the control plane restarts, so excluding `initial` targets would drop all traffic after a restart.

Changing the health check of a target group with `ModifyTargetGroup` takes effect on the next render of every load balancer that uses it.
The time from a backend failing to traffic stopping is the health check interval times the unhealthy threshold, plus the agent poll of about 5 seconds and the heartbeat that reports the result.
Recovery takes the interval times the healthy threshold, plus the same delays.

Measured on a three-node cluster with a 5 second interval and thresholds of 2:

| Event | Time |
| --- | --- |
| Backend starts failing to reported `unhealthy` | 21 s |
| Backend recovers to reported `healthy` | 15 s, serving again 6 s later |
| Every backend failing to reported `unhealthy` | 12 s, fail open immediate |

A TCP connection held through a Network Load Balancer survived four configuration reloads while the other backend changed state, because the agent applies nginx with a graceful reload and HAProxy with a graceful restart.
The HAProxy configuration with the backup servers was accepted by a live Application Load Balancer, but its rendered text was not dumped and checked by hand.

### Forwarding headers

An Application Load Balancer sets three headers on every HTTP and HTTPS request it forwards.
`X-Forwarded-For` carries the client address, `X-Forwarded-Proto` is `http` or `https` for the listener that accepted the request, and `X-Forwarded-Port` is that listener's port.
`X-Forwarded-Proto` and `X-Forwarded-Port` are always overwritten, so a value sent by the client never reaches the target.
Network Load Balancers do not touch the stream.

`routing.http.xff_header_processing.mode` decides what happens to `X-Forwarded-For`:

| Mode | Behaviour |
| --- | --- |
| `append` (default) | Keeps what the client sent and adds the client address last, as one comma-separated line such as `203.0.113.9, 192.0.2.10`. |
| `preserve` | Forwards the client's value unchanged and adds nothing. The header is absent when the client sent none. |
| `remove` | Drops the header, including any client value. |

`routing.http.xff_client_port.enabled` (default `false`) appends the client's source port in `append` mode, as `192.0.2.10:54321`.
An IPv6 client is written `[2001:db8::1]:54321`.
The setting has no effect in `preserve` and `remove` modes.

`ModifyLoadBalancerAttributes` accepts only `append`, `preserve` or `remove` for the mode and `true` or `false` for the client port, and rejects anything else with `ValidationError`.
A change re-renders the load balancer configuration, and a call that leaves both values as they were does not.
In a live test each change reached the target within one 3 second poll.

A client that sends `X-Forwarded-For` more than once has its lines joined into the single comma-separated value.
At most eight client lines are joined, and any beyond the eighth are dropped, while the client address is still added last.

Every other `routing.http.*` attribute is stored and returned by `DescribeLoadBalancerAttributes` but has no effect on the data plane.

Measured on a three-node cluster, with the caller on the first node:

| Listener | `X-Forwarded-For` the target saw |
| --- | --- |
| Internet-facing HTTP | The node's real address, `192.168.0.120` |
| Internet-facing HTTPS | The same address, with `https` and port 443 |
| Internal HTTP | The calling instance's private address |
| Internet-facing HTTP with a client value of `203.0.113.9` | `203.0.113.9, 192.168.0.120` on one line |

A packet capture on the load balancer's interface showed HAProxy receiving the real client address, so nothing translates it first.
Behaviour against AWS was not compared, and the cases above were not run with an IPv6 client.

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
| `DescribeListenerAttributes` |
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
| `ModifyListenerAttributes` |
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
