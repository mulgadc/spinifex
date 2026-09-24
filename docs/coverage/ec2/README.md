---
title: "EC2 API Coverage"
seoTitle: "Amazon EC2 API Coverage on Spinifex — Spinifex Docs"
description: "The Amazon EC2 API operations Spinifex implements, covering instances, EBS volumes, VPC networking, tags, security groups and the rest of the compute surface."
category: "Coverage"
sections:
  - overview
tags:
  - aws
  - compatibility
  - coverage
  - ec2
  - compute
  - vpc
---

# EC2 API Coverage

## Overview

Spinifex implements **126 operations** in the EC2 `2016-11-15` API model.

### Spot Instances Are a Mock

Spot Instance Requests are a mock over the on-demand `RunInstances` path. A request synchronously launches real VMs on the operator's own compute and is then reported `active` and `fulfilled`. There is no spot market: no bidding, no price rejection, no interruption and no reclamation, and instances are never taken back.

### Operations

| Operation |
|---|
| `AllocateAddress` |
| `AssociateAddress` |
| `AssociateIamInstanceProfile` |
| `AssociateRouteTable` |
| `AttachInternetGateway` |
| `AttachNetworkInterface` |
| `AttachVolume` |
| `AuthorizeSecurityGroupEgress` |
| `AuthorizeSecurityGroupIngress` |
| `CancelCapacityReservation` |
| `CancelSpotInstanceRequests` |
| `CopyImage` |
| `CopySnapshot` |
| `CreateCapacityReservation` |
| `CreateEgressOnlyInternetGateway` |
| `CreateImage` |
| `CreateInternetGateway` |
| `CreateKeyPair` |
| `CreateLaunchTemplate` |
| `CreateLaunchTemplateVersion` |
| `CreateNatGateway` |
| `CreateNetworkInterface` |
| `CreatePlacementGroup` |
| `CreateRoute` |
| `CreateRouteTable` |
| `CreateSecurityGroup` |
| `CreateSnapshot` |
| `CreateSubnet` |
| `CreateTags` |
| `CreateVolume` |
| `CreateVpc` |
| `DeleteEgressOnlyInternetGateway` |
| `DeleteInternetGateway` |
| `DeleteKeyPair` |
| `DeleteLaunchTemplate` |
| `DeleteLaunchTemplateVersions` |
| `DeleteNatGateway` |
| `DeleteNetworkInterface` |
| `DeletePlacementGroup` |
| `DeleteRoute` |
| `DeleteRouteTable` |
| `DeleteSecurityGroup` |
| `DeleteSnapshot` |
| `DeleteSubnet` |
| `DeleteTags` |
| `DeleteVolume` |
| `DeleteVpc` |
| `DeregisterImage` |
| `DescribeAccountAttributes` |
| `DescribeAddresses` |
| `DescribeAddressesAttribute` |
| `DescribeAvailabilityZones` |
| `DescribeCapacityReservations` |
| `DescribeEgressOnlyInternetGateways` |
| `DescribeIamInstanceProfileAssociations` |
| `DescribeImageAttribute` |
| `DescribeImages` |
| `DescribeInstanceAttribute` |
| `DescribeInstanceCreditSpecifications` |
| `DescribeInstanceStatus` |
| `DescribeInstanceTypeOfferings` |
| `DescribeInstanceTypes` |
| `DescribeInstances` |
| `DescribeInternetGateways` |
| `DescribeKeyPairs` |
| `DescribeLaunchTemplateVersions` |
| `DescribeLaunchTemplates` |
| `DescribeNatGateways` |
| `DescribeNetworkInterfaces` |
| `DescribePlacementGroups` |
| `DescribeRegions` |
| `DescribeRouteTables` |
| `DescribeSecurityGroupRules` |
| `DescribeSecurityGroups` |
| `DescribeSnapshots` |
| `DescribeSpotInstanceRequests` |
| `DescribeSubnets` |
| `DescribeTags` |
| `DescribeVolumeStatus` |
| `DescribeVolumes` |
| `DescribeVolumesModifications` |
| `DescribeVpcAttribute` |
| `DescribeVpcs` |
| `DetachInternetGateway` |
| `DetachNetworkInterface` |
| `DetachVolume` |
| `DisableEbsEncryptionByDefault` |
| `DisableSerialConsoleAccess` |
| `DisassociateAddress` |
| `DisassociateIamInstanceProfile` |
| `DisassociateRouteTable` |
| `EnableEbsEncryptionByDefault` |
| `EnableSerialConsoleAccess` |
| `GetConsoleOutput` |
| `GetEbsEncryptionByDefault` |
| `GetPasswordData` |
| `GetSecurityGroupsForVpc` |
| `GetSerialConsoleAccessStatus` |
| `ImportKeyPair` |
| `ModifyImageAttribute` |
| `ModifyInstanceAttribute` |
| `ModifyInstanceMetadataOptions` |
| `ModifyLaunchTemplate` |
| `ModifyNetworkInterfaceAttribute` |
| `ModifySecurityGroupRules` |
| `ModifySubnetAttribute` |
| `ModifyVolume` |
| `ModifyVpcAttribute` |
| `MonitorInstances` |
| `RebootInstances` |
| `RegisterImage` |
| `ReleaseAddress` |
| `ReplaceIamInstanceProfileAssociation` |
| `ReplaceRoute` |
| `ReplaceRouteTableAssociation` |
| `RequestSpotInstances` |
| `ResetImageAttribute` |
| `RevokeSecurityGroupEgress` |
| `RevokeSecurityGroupIngress` |
| `RunInstances` |
| `StartInstances` |
| `StopInstances` |
| `TerminateInstances` |
| `UnmonitorInstances` |
| `UpdateSecurityGroupRuleDescriptionsEgress` |
| `UpdateSecurityGroupRuleDescriptionsIngress` |
