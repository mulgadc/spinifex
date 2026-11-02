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

Spinifex implements **127 operations** in the EC2 `2016-11-15` API model.

### Spot Instances Are a Mock

Spot Instance Requests are a mock over the on-demand `RunInstances` path. A request synchronously launches real VMs on the operator's own compute and is then reported `active` and `fulfilled`. There is no spot market: no bidding, no price rejection, no interruption and no reclamation, and instances are never taken back.

### AMI Launch Permissions Are Refused

An AMI cannot be shared with another account or made public. `ModifyImageAttribute` refuses `LaunchPermission`, `UserIds`, `UserGroups`, `OrganizationArns`, `OrganizationalUnitArns` and `OperationType`, and `ResetImageAttribute` resets only `description`, not `launchPermission`. Each refusal is `InvalidParameterValue`. Terraform's `aws_ami_launch_permission` therefore cannot be created. `description` is the only image attribute that can be modified or reset.

### RegisterImage Registers From a Snapshot

`RegisterImage` takes a root EBS snapshot in `BlockDeviceMappings` and writes an image that points at it.
`EnaSupport` is supported, with `true` and `false` both accepted and stored on the image.
`DescribeImages` returns `EnaSupport` when the image has one and leaves the member out when it was never set, as AWS does.
`CopyImage` carries the value to the copy, and `CreateImage` sets it to `true` because every instance reports ENA support.
`RunInstances` does not read the flag, so an image registered with `EnaSupport=false` is not held back from instance types that need ENA on AWS, since guests use virtio networking either way.

`RegisterImage` refuses `ImageLocation`, `KernelId`, `RamdiskId`, `TpmSupport`, `ImdsSupport` and `SriovNetSupport`, and any `VirtualizationType` other than `hvm`.
Each refusal is `InvalidParameterValue`, and the message names the parameter and the value, for example `Value (aki-123) for parameter KernelId is invalid. KernelId is not supported.`
The same form is used for a bad `Architecture`, `BootMode` or `Name` length.
A missing `Name`, `BlockDeviceMappings` or root snapshot answers `MissingParameter` and names what is missing.
Registering with `--ena-support`, `--no-ena-support` and neither, copying the image, and three refusals were run on a three-node cluster with the AWS CLI.
Launching an instance from an `EnaSupport` image was not run, so the launch path is unchanged by construction and not by test.

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
| `DescribeVpcEndpoints` |
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
