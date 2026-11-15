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

### CreateSnapshot Error Mapping

`CreateSnapshot` drains and snapshots the volume inside the EBS provider.
A provider that cannot snapshot a volume because it is in use, as the `qemunbd` provider cannot for an attached volume, answers `ErrVolumeInUse`, which becomes `IncorrectState` with the message `The volume 'vol-...' is in use. Detach it before creating a snapshot.`
The `viperblockd` provider drains a mounted export and snapshots it, so an attached volume succeeds there.
A provider that cannot answer now, for example because another node holds the volume, answers `ServiceUnavailable` so the caller retries.
Any other provider failure is `ServerInternal`.
The snapshot taken for `CreateImage` maps these errors the same way.

### EBS Encryption by Default Follows the Platform

Whether new volumes are encrypted is decided by the platform's encryption key, not by the account.
`GetEbsEncryptionByDefault` and `EnableEbsEncryptionByDefault` report `true` when the platform has an encryption key configured and `false` otherwise, and `EnableEbsEncryptionByDefault` does not change that.
`DisableEbsEncryptionByDefault` is refused with `OperationNotPermitted` and the message `EBS encryption by default cannot be disabled: this platform encrypts every volume` while the platform encrypts, and answers `false` where it does not.

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

### Requires AWS SDK v2

These 186 operations are in the pinned AWS model but not in AWS SDK for Go v1, the SDK Spinifex is built and tested on. They are out of scope until Spinifex moves to AWS SDK v2.

| Operation |
|---|
| `AcceptCapacityReservationBillingOwnership` |
| `AcceptTransitGatewayClientVpnAttachment` |
| `AssociateApplicationStatusCheck` |
| `AssociateCapacityReservationBillingOwner` |
| `AssociateRouteServer` |
| `AssociateSecurityGroupVpc` |
| `AttachImageWatermark` |
| `BatchModifyIpamRoutingPolicyRegistrations` |
| `CancelDeclarativePoliciesReport` |
| `CopyVolumes` |
| `CreateApplicationStatusCheck` |
| `CreateCapacityManagerDataExport` |
| `CreateCapacityReservationBySplitting` |
| `CreateCapacityReservationCancellationQuote` |
| `CreateCapacityReservationDateChangeQuote` |
| `CreateDelegateMacVolumeOwnershipTask` |
| `CreateImageUsageReport` |
| `CreateInterruptibleCapacityReservationAllocation` |
| `CreateIpamInternetRegistryAssociation` |
| `CreateIpamPolicy` |
| `CreateIpamPrefixListResolver` |
| `CreateIpamPrefixListResolverTarget` |
| `CreateIpamRoutingPolicyRegistration` |
| `CreateLocalGatewayVirtualInterface` |
| `CreateLocalGatewayVirtualInterfaceGroup` |
| `CreateMacSystemIntegrityProtectionModificationTask` |
| `CreateRouteServer` |
| `CreateRouteServerEndpoint` |
| `CreateRouteServerPeer` |
| `CreateSecondaryNetwork` |
| `CreateSecondarySubnet` |
| `CreateTransitGatewayMeteringPolicy` |
| `CreateTransitGatewayMeteringPolicyEntry` |
| `CreateTransitGatewayPolicyTableEntry` |
| `CreateVpcBlockPublicAccessExclusion` |
| `CreateVpcEncryptionControl` |
| `CreateVpnConcentrator` |
| `DeleteApplicationStatusCheck` |
| `DeleteCapacityManagerDataExport` |
| `DeleteClientVpnEndpointAuthorizationPolicy` |
| `DeleteImageUsageReport` |
| `DeleteIpamInternetRegistryAssociation` |
| `DeleteIpamPolicy` |
| `DeleteIpamPrefixListResolver` |
| `DeleteIpamPrefixListResolverTarget` |
| `DeleteIpamRoutingPolicyRegistration` |
| `DeleteLocalGatewayVirtualInterface` |
| `DeleteLocalGatewayVirtualInterfaceGroup` |
| `DeleteRouteServer` |
| `DeleteRouteServerEndpoint` |
| `DeleteRouteServerPeer` |
| `DeleteSecondaryNetwork` |
| `DeleteSecondarySubnet` |
| `DeleteTransitGatewayClientVpnAttachment` |
| `DeleteTransitGatewayMeteringPolicy` |
| `DeleteTransitGatewayMeteringPolicyEntry` |
| `DeleteTransitGatewayPolicyTableEntry` |
| `DeleteVpcBlockPublicAccessExclusion` |
| `DeleteVpcEncryptionControl` |
| `DeleteVpnConcentrator` |
| `DescribeAccountVpcEncryptionControl` |
| `DescribeApplicationStatus` |
| `DescribeApplicationStatusCheckAssociations` |
| `DescribeApplicationStatusChecks` |
| `DescribeCapacityBlockExtensionHistory` |
| `DescribeCapacityBlockExtensionOfferings` |
| `DescribeCapacityBlockStatus` |
| `DescribeCapacityBlocks` |
| `DescribeCapacityManagerDataExports` |
| `DescribeCapacityReservationBillingRequests` |
| `DescribeCapacityReservationCancellationQuotes` |
| `DescribeCapacityReservationDateChangeQuotes` |
| `DescribeCapacityReservationTopology` |
| `DescribeDeclarativePoliciesReports` |
| `DescribeImageReferences` |
| `DescribeImageUsageReportEntries` |
| `DescribeImageUsageReports` |
| `DescribeInstanceImageMetadata` |
| `DescribeInstanceSqlHaHistoryStates` |
| `DescribeInstanceSqlHaStates` |
| `DescribeIpamInternetRegistryAssociations` |
| `DescribeIpamPolicies` |
| `DescribeIpamPoolAllocations` |
| `DescribeIpamPrefixListResolverTargets` |
| `DescribeIpamPrefixListResolvers` |
| `DescribeMacModificationTasks` |
| `DescribeOutpostLags` |
| `DescribeRouteServerEndpoints` |
| `DescribeRouteServerPeers` |
| `DescribeRouteServers` |
| `DescribeSecondaryInterfaces` |
| `DescribeSecondaryNetworks` |
| `DescribeSecondarySubnets` |
| `DescribeSecurityGroupVpcAssociations` |
| `DescribeServiceLinkVirtualInterfaces` |
| `DescribeTransitGatewayMeteringPolicies` |
| `DescribeVpcBlockPublicAccessExclusions` |
| `DescribeVpcBlockPublicAccessOptions` |
| `DescribeVpcEncryptionControls` |
| `DescribeVpcEndpointAssociations` |
| `DescribeVpnConcentrators` |
| `DetachImageWatermark` |
| `DisableAllowedImagesSettings` |
| `DisableApplicationStatusCheckSuppression` |
| `DisableCapacityManager` |
| `DisableInstanceSqlHaStandbyDetections` |
| `DisableIpamPolicy` |
| `DisableRouteServerPropagation` |
| `DisassociateApplicationStatusCheck` |
| `DisassociateCapacityReservationBillingOwner` |
| `DisassociateRouteServer` |
| `DisassociateSecurityGroupVpc` |
| `EnableAllowedImagesSettings` |
| `EnableApplicationStatusCheckSuppression` |
| `EnableCapacityManager` |
| `EnableInstanceSqlHaStandbyDetections` |
| `EnableIpamInternetRegistryAssociation` |
| `EnableIpamPolicy` |
| `EnableRouteServerPropagation` |
| `ExportVerifiedAccessInstanceClientConfiguration` |
| `GetActiveVpnTunnelStatus` |
| `GetAllowedImagesSettings` |
| `GetCapacityManagerAttributes` |
| `GetCapacityManagerMetricData` |
| `GetCapacityManagerMetricDimensions` |
| `GetCapacityManagerMonitoredTagKeys` |
| `GetCapacityManagerSupportabilityScores` |
| `GetClientVpnEndpointAuthorizationPolicy` |
| `GetDeclarativePoliciesReportSummary` |
| `GetEnabledIpamPolicy` |
| `GetImageAncestry` |
| `GetImageFindings` |
| `GetIpamDiscoveredRoutes` |
| `GetIpamInternetRegistryAssociationAsns` |
| `GetIpamInternetRegistryAssociationCidrs` |
| `GetIpamPolicyAllocationRules` |
| `GetIpamPolicyOrganizationTargets` |
| `GetIpamPrefixListResolverRules` |
| `GetIpamPrefixListResolverVersionEntries` |
| `GetIpamPrefixListResolverVersions` |
| `GetIpamRouteOriginAuthorizations` |
| `GetIpamRouteProtectionFindings` |
| `GetIpamRoutingPolicyRegistrationDeltas` |
| `GetIpamRoutingPolicyRegistrations` |
| `GetManagedResourceVisibility` |
| `GetRouteServerAssociations` |
| `GetRouteServerPropagations` |
| `GetRouteServerRoutingDatabase` |
| `GetTransitGatewayAttachmentNatConfiguration` |
| `GetTransitGatewayMeteringPolicyEntries` |
| `GetVerifiedAccessEndpointTargets` |
| `GetVpcResourcesBlockingEncryptionEnforcement` |
| `ListVolumesInRecycleBin` |
| `ModifyAccountVpcEncryptionControl` |
| `ModifyApplicationStatusCheck` |
| `ModifyClientVpnEndpointAuthorizationPolicy` |
| `ModifyInstanceConnectEndpoint` |
| `ModifyInstanceCpuOptions` |
| `ModifyInstanceNetworkPerformanceOptions` |
| `ModifyIpamPolicyAllocationRules` |
| `ModifyIpamPoolAllocation` |
| `ModifyIpamPrefixListResolver` |
| `ModifyIpamPrefixListResolverTarget` |
| `ModifyIpamRoutingPolicyRegistration` |
| `ModifyManagedResourceVisibility` |
| `ModifyPublicIpDnsNameOptions` |
| `ModifyRouteServer` |
| `ModifyTransitGatewayAttachmentNatConfiguration` |
| `ModifyTransitGatewayMeteringPolicy` |
| `ModifyTransitGatewayPolicyTableEntry` |
| `ModifyVpcBlockPublicAccessExclusion` |
| `ModifyVpcBlockPublicAccessOptions` |
| `ModifyVpcEncryptionControl` |
| `ModifyVpcEndpointPayerResponsibility` |
| `MoveCapacityReservationInstances` |
| `PurchaseCapacityBlockExtension` |
| `RejectCapacityReservationBillingOwnership` |
| `RejectTransitGatewayClientVpnAttachment` |
| `ReplaceImageCriteriaInAllowedImagesSettings` |
| `ReplaceImageInstanceTypeSpecification` |
| `RestoreVolumeFromRecycleBin` |
| `StartDeclarativePoliciesReport` |
| `UpdateCapacityManagerMonitoredTagKeys` |
| `UpdateCapacityManagerOrganizationsAccess` |
| `UpdateInterruptibleCapacityReservationAllocation` |
| `ValidateSecurityGroupQuotasForInterface` |
