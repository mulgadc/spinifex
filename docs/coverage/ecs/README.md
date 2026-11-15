---
title: "ECS API Coverage"
seoTitle: "Elastic Container Service API Coverage — Spinifex Docs"
description: "The Amazon ECS API operations Spinifex implements, covering clusters, services, tasks, container instances and the task definitions they are launched from."
category: "Coverage"
sections:
  - overview
tags:
  - aws
  - compatibility
  - coverage
  - ecs
  - containers
  - orchestration
---

# ECS API Coverage

## Overview

Spinifex implements **31 operations** in the ECS `2014-11-13` API model.

### No Fargate

Clusters, services and tasks run on EC2 container instances. There is no Fargate launch type, and no operation below provides one.

### Operations

| Operation |
|---|
| `CreateCapacityProvider` |
| `CreateCluster` |
| `CreateService` |
| `DeleteCapacityProvider` |
| `DeleteCluster` |
| `DeleteService` |
| `DeregisterContainerInstance` |
| `DeregisterTaskDefinition` |
| `DescribeCapacityProviders` |
| `DescribeClusters` |
| `DescribeContainerInstances` |
| `DescribeServices` |
| `DescribeTaskDefinition` |
| `DescribeTasks` |
| `ListClusters` |
| `ListContainerInstances` |
| `ListServices` |
| `ListTagsForResource` |
| `ListTaskDefinitions` |
| `ListTasks` |
| `PutClusterCapacityProviders` |
| `RegisterContainerInstance` |
| `RegisterTaskDefinition` |
| `RunTask` |
| `StartTask` |
| `StopTask` |
| `SubmitTaskStateChange` |
| `TagResource` |
| `UntagResource` |
| `UpdateContainerInstancesState` |
| `UpdateService` |

### Requires AWS SDK v2

These 32 operations are in the pinned AWS model but not in AWS SDK for Go v1, the SDK Spinifex is built and tested on. They are out of scope until Spinifex moves to AWS SDK v2.

| Operation |
|---|
| `AssociateGatewayWithCluster` |
| `ContinueServiceDeployment` |
| `CreateDaemon` |
| `CreateExpressGatewayService` |
| `CreateGateway` |
| `DeleteDaemon` |
| `DeleteDaemonTaskDefinition` |
| `DeleteExpressGatewayService` |
| `DeleteGateway` |
| `DescribeDaemon` |
| `DescribeDaemonDeployments` |
| `DescribeDaemonRevisions` |
| `DescribeDaemonTaskDefinition` |
| `DescribeExpressGatewayService` |
| `DescribeGateway` |
| `DescribeGatewayResources` |
| `DescribeGatewayRoute` |
| `DescribeServiceDeployments` |
| `DescribeServiceRevisions` |
| `DisassociateGatewayFromCluster` |
| `ListDaemonDeployments` |
| `ListDaemonTaskDefinitions` |
| `ListDaemons` |
| `ListGatewayClusterAssociations` |
| `ListGatewayRoutes` |
| `ListGateways` |
| `ListServiceDeployments` |
| `RegisterDaemonTaskDefinition` |
| `StopServiceDeployment` |
| `UpdateDaemon` |
| `UpdateExpressGatewayService` |
| `UpdateGateway` |
