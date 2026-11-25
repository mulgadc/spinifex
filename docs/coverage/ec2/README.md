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

Spinifex implements **161 operations** in the EC2 `2016-11-15` API model.

### Spot Instances Are a Mock

Spot Instance Requests launch on-demand instances and report them as `active` and `fulfilled`.
There is no spot market: no bidding, no price rejection and no interruption.

### AMI Launch Permissions Are Refused

An AMI cannot be shared with another account or made public.
`ModifyImageAttribute` and `ResetImageAttribute` accept only `description`, and refuse launch permission changes with `InvalidParameterValue`.
Terraform's `aws_ami_launch_permission` therefore cannot be created.

### RegisterImage Refuses Some Parameters

`ImageLocation`, `KernelId`, `RamdiskId`, `TpmSupport`, `ImdsSupport`, `SriovNetSupport` and any `VirtualizationType` other than `hvm` are refused with `InvalidParameterValue`.

`EnaSupport` is stored and returned, but does not limit which instance types can launch the image.

### EBS Encryption by Default Follows the Platform

The platform, not the account, decides whether new volumes are encrypted.
`GetEbsEncryptionByDefault` reports `true` when the platform encrypts volumes.
While it does, `DisableEbsEncryptionByDefault` is refused with `OperationNotPermitted`.

### Operations

| Operation |
|---|
| `AcceptVpcPeeringConnection` |
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
| `CreateTransitGateway` |
| `CreateTransitGatewayVpcAttachment` |
| `CreateVolume` |
| `CreateVpc` |
| `CreateVpcEndpoint` |
| `CreateVpcPeeringConnection` |
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
| `DeleteTransitGateway` |
| `DeleteTransitGatewayVpcAttachment` |
| `DeleteVolume` |
| `DeleteVpc` |
| `DeleteVpcEndpoints` |
| `DeleteVpcPeeringConnection` |
| `DeregisterImage` |
| `DescribeAccountAttributes` |
| `DescribeAddresses` |
| `DescribeAddressesAttribute` |
| `DescribeAggregateIdFormat` |
| `DescribeAvailabilityZones` |
| `DescribeCapacityReservations` |
| `DescribeEgressOnlyInternetGateways` |
| `DescribeIamInstanceProfileAssociations` |
| `DescribeIdFormat` |
| `DescribeIdentityIdFormat` |
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
| `DescribeManagedPrefixLists` |
| `DescribeNatGateways` |
| `DescribeNetworkInterfaceAttribute` |
| `DescribeNetworkInterfaces` |
| `DescribePlacementGroups` |
| `DescribePrefixLists` |
| `DescribePrincipalIdFormat` |
| `DescribeRegions` |
| `DescribeRouteTables` |
| `DescribeSecurityGroupRules` |
| `DescribeSecurityGroups` |
| `DescribeSnapshotAttribute` |
| `DescribeSnapshots` |
| `DescribeSpotInstanceRequests` |
| `DescribeSubnets` |
| `DescribeTags` |
| `DescribeTransitGatewayVpcAttachments` |
| `DescribeTransitGateways` |
| `DescribeVolumeAttribute` |
| `DescribeVolumeStatus` |
| `DescribeVolumes` |
| `DescribeVolumesModifications` |
| `DescribeVpcAttribute` |
| `DescribeVpcEndpointServices` |
| `DescribeVpcEndpoints` |
| `DescribeVpcPeeringConnections` |
| `DescribeVpcs` |
| `DetachInternetGateway` |
| `DetachNetworkInterface` |
| `DetachVolume` |
| `DisableEbsEncryptionByDefault` |
| `DisableImageDeprecation` |
| `DisableImageDeregistrationProtection` |
| `DisableSerialConsoleAccess` |
| `DisassociateAddress` |
| `DisassociateIamInstanceProfile` |
| `DisassociateRouteTable` |
| `EnableEbsEncryptionByDefault` |
| `EnableImageDeprecation` |
| `EnableImageDeregistrationProtection` |
| `EnableSerialConsoleAccess` |
| `GetConsoleOutput` |
| `GetDefaultCreditSpecification` |
| `GetEbsEncryptionByDefault` |
| `GetInstanceUefiData` |
| `GetManagedPrefixListEntries` |
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
| `ModifyVpcEndpoint` |
| `ModifyVpcPeeringConnectionOptions` |
| `MonitorInstances` |
| `RebootInstances` |
| `RegisterImage` |
| `RejectVpcPeeringConnection` |
| `ReleaseAddress` |
| `ReplaceIamInstanceProfileAssociation` |
| `ReplaceRoute` |
| `ReplaceRouteTableAssociation` |
| `RequestSpotInstances` |
| `ResetImageAttribute` |
| `ResetInstanceAttribute` |
| `ResetNetworkInterfaceAttribute` |
| `RevokeSecurityGroupEgress` |
| `RevokeSecurityGroupIngress` |
| `RunInstances` |
| `StartInstances` |
| `StopInstances` |
| `TerminateInstances` |
| `UnmonitorInstances` |
| `UpdateSecurityGroupRuleDescriptionsEgress` |
| `UpdateSecurityGroupRuleDescriptionsIngress` |
