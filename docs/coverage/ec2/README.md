---
title: "EC2 API Coverage"
seoTitle: "Amazon EC2 API Coverage on Spinifex — Spinifex Docs"
description: "Every operation in the Amazon EC2 API model and whether Spinifex implements it, covering instances, EBS volumes, VPC networking, tags and security groups."
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

Spinifex implements **122 of the 625** operations (**19.5%**) in the EC2 `2016-11-15` API model.

### Spot Instances Are a Mock

Spot Instance Requests are a mock over the on-demand `RunInstances` path. A request synchronously launches real VMs on the operator's own compute and is then reported `active` and `fulfilled`. There is no spot market: no bidding, no price rejection, no interruption and no reclamation, and instances are never taken back.

### Operations

| Operation | Status |
|---|---|
| `AcceptAddressTransfer` | ❌ Not implemented |
| `AcceptReservedInstancesExchangeQuote` | ❌ Not implemented |
| `AcceptTransitGatewayMulticastDomainAssociations` | ❌ Not implemented |
| `AcceptTransitGatewayPeeringAttachment` | ❌ Not implemented |
| `AcceptTransitGatewayVpcAttachment` | ❌ Not implemented |
| `AcceptVpcEndpointConnections` | ❌ Not implemented |
| `AcceptVpcPeeringConnection` | ❌ Not implemented |
| `AdvertiseByoipCidr` | ❌ Not implemented |
| `AllocateAddress` | ✅ Implemented |
| `AllocateHosts` | ❌ Not implemented |
| `AllocateIpamPoolCidr` | ❌ Not implemented |
| `ApplySecurityGroupsToClientVpnTargetNetwork` | ❌ Not implemented |
| `AssignIpv6Addresses` | ❌ Not implemented |
| `AssignPrivateIpAddresses` | ❌ Not implemented |
| `AssignPrivateNatGatewayAddress` | ❌ Not implemented |
| `AssociateAddress` | ✅ Implemented |
| `AssociateClientVpnTargetNetwork` | ❌ Not implemented |
| `AssociateDhcpOptions` | ❌ Not implemented |
| `AssociateEnclaveCertificateIamRole` | ❌ Not implemented |
| `AssociateIamInstanceProfile` | ✅ Implemented |
| `AssociateInstanceEventWindow` | ❌ Not implemented |
| `AssociateIpamByoasn` | ❌ Not implemented |
| `AssociateIpamResourceDiscovery` | ❌ Not implemented |
| `AssociateNatGatewayAddress` | ❌ Not implemented |
| `AssociateRouteTable` | ✅ Implemented |
| `AssociateSubnetCidrBlock` | ❌ Not implemented |
| `AssociateTransitGatewayMulticastDomain` | ❌ Not implemented |
| `AssociateTransitGatewayPolicyTable` | ❌ Not implemented |
| `AssociateTransitGatewayRouteTable` | ❌ Not implemented |
| `AssociateTrunkInterface` | ❌ Not implemented |
| `AssociateVpcCidrBlock` | ❌ Not implemented |
| `AttachClassicLinkVpc` | ❌ Not implemented |
| `AttachInternetGateway` | ✅ Implemented |
| `AttachNetworkInterface` | ✅ Implemented |
| `AttachVerifiedAccessTrustProvider` | ❌ Not implemented |
| `AttachVolume` | ✅ Implemented |
| `AttachVpnGateway` | ❌ Not implemented |
| `AuthorizeClientVpnIngress` | ❌ Not implemented |
| `AuthorizeSecurityGroupEgress` | ✅ Implemented |
| `AuthorizeSecurityGroupIngress` | ✅ Implemented |
| `BundleInstance` | ❌ Not implemented |
| `CancelBundleTask` | ❌ Not implemented |
| `CancelCapacityReservation` | ✅ Implemented |
| `CancelCapacityReservationFleets` | ❌ Not implemented |
| `CancelConversionTask` | ❌ Not implemented |
| `CancelExportTask` | ❌ Not implemented |
| `CancelImageLaunchPermission` | ❌ Not implemented |
| `CancelImportTask` | ❌ Not implemented |
| `CancelReservedInstancesListing` | ❌ Not implemented |
| `CancelSpotFleetRequests` | ❌ Not implemented |
| `CancelSpotInstanceRequests` | ✅ Implemented |
| `ConfirmProductInstance` | ❌ Not implemented |
| `CopyFpgaImage` | ❌ Not implemented |
| `CopyImage` | ✅ Implemented |
| `CopySnapshot` | ✅ Implemented |
| `CreateCapacityReservation` | ✅ Implemented |
| `CreateCapacityReservationFleet` | ❌ Not implemented |
| `CreateCarrierGateway` | ❌ Not implemented |
| `CreateClientVpnEndpoint` | ❌ Not implemented |
| `CreateClientVpnRoute` | ❌ Not implemented |
| `CreateCoipCidr` | ❌ Not implemented |
| `CreateCoipPool` | ❌ Not implemented |
| `CreateCustomerGateway` | ❌ Not implemented |
| `CreateDefaultSubnet` | ❌ Not implemented |
| `CreateDefaultVpc` | ❌ Not implemented |
| `CreateDhcpOptions` | ❌ Not implemented |
| `CreateEgressOnlyInternetGateway` | ✅ Implemented |
| `CreateFleet` | ❌ Not implemented |
| `CreateFlowLogs` | ❌ Not implemented |
| `CreateFpgaImage` | ❌ Not implemented |
| `CreateImage` | ✅ Implemented |
| `CreateInstanceConnectEndpoint` | ❌ Not implemented |
| `CreateInstanceEventWindow` | ❌ Not implemented |
| `CreateInstanceExportTask` | ❌ Not implemented |
| `CreateInternetGateway` | ✅ Implemented |
| `CreateIpam` | ❌ Not implemented |
| `CreateIpamExternalResourceVerificationToken` | ❌ Not implemented |
| `CreateIpamPool` | ❌ Not implemented |
| `CreateIpamResourceDiscovery` | ❌ Not implemented |
| `CreateIpamScope` | ❌ Not implemented |
| `CreateKeyPair` | ✅ Implemented |
| `CreateLaunchTemplate` | ✅ Implemented |
| `CreateLaunchTemplateVersion` | ✅ Implemented |
| `CreateLocalGatewayRoute` | ❌ Not implemented |
| `CreateLocalGatewayRouteTable` | ❌ Not implemented |
| `CreateLocalGatewayRouteTableVirtualInterfaceGroupAssociation` | ❌ Not implemented |
| `CreateLocalGatewayRouteTableVpcAssociation` | ❌ Not implemented |
| `CreateManagedPrefixList` | ❌ Not implemented |
| `CreateNatGateway` | ✅ Implemented |
| `CreateNetworkAcl` | ❌ Not implemented |
| `CreateNetworkAclEntry` | ❌ Not implemented |
| `CreateNetworkInsightsAccessScope` | ❌ Not implemented |
| `CreateNetworkInsightsPath` | ❌ Not implemented |
| `CreateNetworkInterface` | ✅ Implemented |
| `CreateNetworkInterfacePermission` | ❌ Not implemented |
| `CreatePlacementGroup` | ✅ Implemented |
| `CreatePublicIpv4Pool` | ❌ Not implemented |
| `CreateReplaceRootVolumeTask` | ❌ Not implemented |
| `CreateReservedInstancesListing` | ❌ Not implemented |
| `CreateRestoreImageTask` | ❌ Not implemented |
| `CreateRoute` | ✅ Implemented |
| `CreateRouteTable` | ✅ Implemented |
| `CreateSecurityGroup` | ✅ Implemented |
| `CreateSnapshot` | ✅ Implemented |
| `CreateSnapshots` | ❌ Not implemented |
| `CreateSpotDatafeedSubscription` | ❌ Not implemented |
| `CreateStoreImageTask` | ❌ Not implemented |
| `CreateSubnet` | ✅ Implemented |
| `CreateSubnetCidrReservation` | ❌ Not implemented |
| `CreateTags` | ✅ Implemented |
| `CreateTrafficMirrorFilter` | ❌ Not implemented |
| `CreateTrafficMirrorFilterRule` | ❌ Not implemented |
| `CreateTrafficMirrorSession` | ❌ Not implemented |
| `CreateTrafficMirrorTarget` | ❌ Not implemented |
| `CreateTransitGateway` | ❌ Not implemented |
| `CreateTransitGatewayConnect` | ❌ Not implemented |
| `CreateTransitGatewayConnectPeer` | ❌ Not implemented |
| `CreateTransitGatewayMulticastDomain` | ❌ Not implemented |
| `CreateTransitGatewayPeeringAttachment` | ❌ Not implemented |
| `CreateTransitGatewayPolicyTable` | ❌ Not implemented |
| `CreateTransitGatewayPrefixListReference` | ❌ Not implemented |
| `CreateTransitGatewayRoute` | ❌ Not implemented |
| `CreateTransitGatewayRouteTable` | ❌ Not implemented |
| `CreateTransitGatewayRouteTableAnnouncement` | ❌ Not implemented |
| `CreateTransitGatewayVpcAttachment` | ❌ Not implemented |
| `CreateVerifiedAccessEndpoint` | ❌ Not implemented |
| `CreateVerifiedAccessGroup` | ❌ Not implemented |
| `CreateVerifiedAccessInstance` | ❌ Not implemented |
| `CreateVerifiedAccessTrustProvider` | ❌ Not implemented |
| `CreateVolume` | ✅ Implemented |
| `CreateVpc` | ✅ Implemented |
| `CreateVpcEndpoint` | ❌ Not implemented |
| `CreateVpcEndpointConnectionNotification` | ❌ Not implemented |
| `CreateVpcEndpointServiceConfiguration` | ❌ Not implemented |
| `CreateVpcPeeringConnection` | ❌ Not implemented |
| `CreateVpnConnection` | ❌ Not implemented |
| `CreateVpnConnectionRoute` | ❌ Not implemented |
| `CreateVpnGateway` | ❌ Not implemented |
| `DeleteCarrierGateway` | ❌ Not implemented |
| `DeleteClientVpnEndpoint` | ❌ Not implemented |
| `DeleteClientVpnRoute` | ❌ Not implemented |
| `DeleteCoipCidr` | ❌ Not implemented |
| `DeleteCoipPool` | ❌ Not implemented |
| `DeleteCustomerGateway` | ❌ Not implemented |
| `DeleteDhcpOptions` | ❌ Not implemented |
| `DeleteEgressOnlyInternetGateway` | ✅ Implemented |
| `DeleteFleets` | ❌ Not implemented |
| `DeleteFlowLogs` | ❌ Not implemented |
| `DeleteFpgaImage` | ❌ Not implemented |
| `DeleteInstanceConnectEndpoint` | ❌ Not implemented |
| `DeleteInstanceEventWindow` | ❌ Not implemented |
| `DeleteInternetGateway` | ✅ Implemented |
| `DeleteIpam` | ❌ Not implemented |
| `DeleteIpamExternalResourceVerificationToken` | ❌ Not implemented |
| `DeleteIpamPool` | ❌ Not implemented |
| `DeleteIpamResourceDiscovery` | ❌ Not implemented |
| `DeleteIpamScope` | ❌ Not implemented |
| `DeleteKeyPair` | ✅ Implemented |
| `DeleteLaunchTemplate` | ✅ Implemented |
| `DeleteLaunchTemplateVersions` | ✅ Implemented |
| `DeleteLocalGatewayRoute` | ❌ Not implemented |
| `DeleteLocalGatewayRouteTable` | ❌ Not implemented |
| `DeleteLocalGatewayRouteTableVirtualInterfaceGroupAssociation` | ❌ Not implemented |
| `DeleteLocalGatewayRouteTableVpcAssociation` | ❌ Not implemented |
| `DeleteManagedPrefixList` | ❌ Not implemented |
| `DeleteNatGateway` | ✅ Implemented |
| `DeleteNetworkAcl` | ❌ Not implemented |
| `DeleteNetworkAclEntry` | ❌ Not implemented |
| `DeleteNetworkInsightsAccessScope` | ❌ Not implemented |
| `DeleteNetworkInsightsAccessScopeAnalysis` | ❌ Not implemented |
| `DeleteNetworkInsightsAnalysis` | ❌ Not implemented |
| `DeleteNetworkInsightsPath` | ❌ Not implemented |
| `DeleteNetworkInterface` | ✅ Implemented |
| `DeleteNetworkInterfacePermission` | ❌ Not implemented |
| `DeletePlacementGroup` | ✅ Implemented |
| `DeletePublicIpv4Pool` | ❌ Not implemented |
| `DeleteQueuedReservedInstances` | ❌ Not implemented |
| `DeleteRoute` | ✅ Implemented |
| `DeleteRouteTable` | ✅ Implemented |
| `DeleteSecurityGroup` | ✅ Implemented |
| `DeleteSnapshot` | ✅ Implemented |
| `DeleteSpotDatafeedSubscription` | ❌ Not implemented |
| `DeleteSubnet` | ✅ Implemented |
| `DeleteSubnetCidrReservation` | ❌ Not implemented |
| `DeleteTags` | ✅ Implemented |
| `DeleteTrafficMirrorFilter` | ❌ Not implemented |
| `DeleteTrafficMirrorFilterRule` | ❌ Not implemented |
| `DeleteTrafficMirrorSession` | ❌ Not implemented |
| `DeleteTrafficMirrorTarget` | ❌ Not implemented |
| `DeleteTransitGateway` | ❌ Not implemented |
| `DeleteTransitGatewayConnect` | ❌ Not implemented |
| `DeleteTransitGatewayConnectPeer` | ❌ Not implemented |
| `DeleteTransitGatewayMulticastDomain` | ❌ Not implemented |
| `DeleteTransitGatewayPeeringAttachment` | ❌ Not implemented |
| `DeleteTransitGatewayPolicyTable` | ❌ Not implemented |
| `DeleteTransitGatewayPrefixListReference` | ❌ Not implemented |
| `DeleteTransitGatewayRoute` | ❌ Not implemented |
| `DeleteTransitGatewayRouteTable` | ❌ Not implemented |
| `DeleteTransitGatewayRouteTableAnnouncement` | ❌ Not implemented |
| `DeleteTransitGatewayVpcAttachment` | ❌ Not implemented |
| `DeleteVerifiedAccessEndpoint` | ❌ Not implemented |
| `DeleteVerifiedAccessGroup` | ❌ Not implemented |
| `DeleteVerifiedAccessInstance` | ❌ Not implemented |
| `DeleteVerifiedAccessTrustProvider` | ❌ Not implemented |
| `DeleteVolume` | ✅ Implemented |
| `DeleteVpc` | ✅ Implemented |
| `DeleteVpcEndpointConnectionNotifications` | ❌ Not implemented |
| `DeleteVpcEndpointServiceConfigurations` | ❌ Not implemented |
| `DeleteVpcEndpoints` | ❌ Not implemented |
| `DeleteVpcPeeringConnection` | ❌ Not implemented |
| `DeleteVpnConnection` | ❌ Not implemented |
| `DeleteVpnConnectionRoute` | ❌ Not implemented |
| `DeleteVpnGateway` | ❌ Not implemented |
| `DeprovisionByoipCidr` | ❌ Not implemented |
| `DeprovisionIpamByoasn` | ❌ Not implemented |
| `DeprovisionIpamPoolCidr` | ❌ Not implemented |
| `DeprovisionPublicIpv4PoolCidr` | ❌ Not implemented |
| `DeregisterImage` | ✅ Implemented |
| `DeregisterInstanceEventNotificationAttributes` | ❌ Not implemented |
| `DeregisterTransitGatewayMulticastGroupMembers` | ❌ Not implemented |
| `DeregisterTransitGatewayMulticastGroupSources` | ❌ Not implemented |
| `DescribeAccountAttributes` | ✅ Implemented |
| `DescribeAddressTransfers` | ❌ Not implemented |
| `DescribeAddresses` | ✅ Implemented |
| `DescribeAddressesAttribute` | ✅ Implemented |
| `DescribeAggregateIdFormat` | ❌ Not implemented |
| `DescribeAvailabilityZones` | ✅ Implemented |
| `DescribeAwsNetworkPerformanceMetricSubscriptions` | ❌ Not implemented |
| `DescribeBundleTasks` | ❌ Not implemented |
| `DescribeByoipCidrs` | ❌ Not implemented |
| `DescribeCapacityBlockOfferings` | ❌ Not implemented |
| `DescribeCapacityReservationFleets` | ❌ Not implemented |
| `DescribeCapacityReservations` | ✅ Implemented |
| `DescribeCarrierGateways` | ❌ Not implemented |
| `DescribeClassicLinkInstances` | ❌ Not implemented |
| `DescribeClientVpnAuthorizationRules` | ❌ Not implemented |
| `DescribeClientVpnConnections` | ❌ Not implemented |
| `DescribeClientVpnEndpoints` | ❌ Not implemented |
| `DescribeClientVpnRoutes` | ❌ Not implemented |
| `DescribeClientVpnTargetNetworks` | ❌ Not implemented |
| `DescribeCoipPools` | ❌ Not implemented |
| `DescribeConversionTasks` | ❌ Not implemented |
| `DescribeCustomerGateways` | ❌ Not implemented |
| `DescribeDhcpOptions` | ❌ Not implemented |
| `DescribeEgressOnlyInternetGateways` | ✅ Implemented |
| `DescribeElasticGpus` | ❌ Not implemented |
| `DescribeExportImageTasks` | ❌ Not implemented |
| `DescribeExportTasks` | ❌ Not implemented |
| `DescribeFastLaunchImages` | ❌ Not implemented |
| `DescribeFastSnapshotRestores` | ❌ Not implemented |
| `DescribeFleetHistory` | ❌ Not implemented |
| `DescribeFleetInstances` | ❌ Not implemented |
| `DescribeFleets` | ❌ Not implemented |
| `DescribeFlowLogs` | ❌ Not implemented |
| `DescribeFpgaImageAttribute` | ❌ Not implemented |
| `DescribeFpgaImages` | ❌ Not implemented |
| `DescribeHostReservationOfferings` | ❌ Not implemented |
| `DescribeHostReservations` | ❌ Not implemented |
| `DescribeHosts` | ❌ Not implemented |
| `DescribeIamInstanceProfileAssociations` | ✅ Implemented |
| `DescribeIdFormat` | ❌ Not implemented |
| `DescribeIdentityIdFormat` | ❌ Not implemented |
| `DescribeImageAttribute` | ✅ Implemented |
| `DescribeImages` | ✅ Implemented |
| `DescribeImportImageTasks` | ❌ Not implemented |
| `DescribeImportSnapshotTasks` | ❌ Not implemented |
| `DescribeInstanceAttribute` | ✅ Implemented |
| `DescribeInstanceConnectEndpoints` | ❌ Not implemented |
| `DescribeInstanceCreditSpecifications` | ✅ Implemented |
| `DescribeInstanceEventNotificationAttributes` | ❌ Not implemented |
| `DescribeInstanceEventWindows` | ❌ Not implemented |
| `DescribeInstanceStatus` | ✅ Implemented |
| `DescribeInstanceTopology` | ❌ Not implemented |
| `DescribeInstanceTypeOfferings` | ✅ Implemented |
| `DescribeInstanceTypes` | ✅ Implemented |
| `DescribeInstances` | ✅ Implemented |
| `DescribeInternetGateways` | ✅ Implemented |
| `DescribeIpamByoasn` | ❌ Not implemented |
| `DescribeIpamExternalResourceVerificationTokens` | ❌ Not implemented |
| `DescribeIpamPools` | ❌ Not implemented |
| `DescribeIpamResourceDiscoveries` | ❌ Not implemented |
| `DescribeIpamResourceDiscoveryAssociations` | ❌ Not implemented |
| `DescribeIpamScopes` | ❌ Not implemented |
| `DescribeIpams` | ❌ Not implemented |
| `DescribeIpv6Pools` | ❌ Not implemented |
| `DescribeKeyPairs` | ✅ Implemented |
| `DescribeLaunchTemplateVersions` | ✅ Implemented |
| `DescribeLaunchTemplates` | ✅ Implemented |
| `DescribeLocalGatewayRouteTableVirtualInterfaceGroupAssociations` | ❌ Not implemented |
| `DescribeLocalGatewayRouteTableVpcAssociations` | ❌ Not implemented |
| `DescribeLocalGatewayRouteTables` | ❌ Not implemented |
| `DescribeLocalGatewayVirtualInterfaceGroups` | ❌ Not implemented |
| `DescribeLocalGatewayVirtualInterfaces` | ❌ Not implemented |
| `DescribeLocalGateways` | ❌ Not implemented |
| `DescribeLockedSnapshots` | ❌ Not implemented |
| `DescribeMacHosts` | ❌ Not implemented |
| `DescribeManagedPrefixLists` | ❌ Not implemented |
| `DescribeMovingAddresses` | ❌ Not implemented |
| `DescribeNatGateways` | ✅ Implemented |
| `DescribeNetworkAcls` | ❌ Not implemented |
| `DescribeNetworkInsightsAccessScopeAnalyses` | ❌ Not implemented |
| `DescribeNetworkInsightsAccessScopes` | ❌ Not implemented |
| `DescribeNetworkInsightsAnalyses` | ❌ Not implemented |
| `DescribeNetworkInsightsPaths` | ❌ Not implemented |
| `DescribeNetworkInterfaceAttribute` | ❌ Not implemented |
| `DescribeNetworkInterfacePermissions` | ❌ Not implemented |
| `DescribeNetworkInterfaces` | ✅ Implemented |
| `DescribePlacementGroups` | ✅ Implemented |
| `DescribePrefixLists` | ❌ Not implemented |
| `DescribePrincipalIdFormat` | ❌ Not implemented |
| `DescribePublicIpv4Pools` | ❌ Not implemented |
| `DescribeRegions` | ✅ Implemented |
| `DescribeReplaceRootVolumeTasks` | ❌ Not implemented |
| `DescribeReservedInstances` | ❌ Not implemented |
| `DescribeReservedInstancesListings` | ❌ Not implemented |
| `DescribeReservedInstancesModifications` | ❌ Not implemented |
| `DescribeReservedInstancesOfferings` | ❌ Not implemented |
| `DescribeRouteTables` | ✅ Implemented |
| `DescribeScheduledInstanceAvailability` | ❌ Not implemented |
| `DescribeScheduledInstances` | ❌ Not implemented |
| `DescribeSecurityGroupReferences` | ❌ Not implemented |
| `DescribeSecurityGroupRules` | ✅ Implemented |
| `DescribeSecurityGroups` | ✅ Implemented |
| `DescribeSnapshotAttribute` | ❌ Not implemented |
| `DescribeSnapshotTierStatus` | ❌ Not implemented |
| `DescribeSnapshots` | ✅ Implemented |
| `DescribeSpotDatafeedSubscription` | ❌ Not implemented |
| `DescribeSpotFleetInstances` | ❌ Not implemented |
| `DescribeSpotFleetRequestHistory` | ❌ Not implemented |
| `DescribeSpotFleetRequests` | ❌ Not implemented |
| `DescribeSpotInstanceRequests` | ✅ Implemented |
| `DescribeSpotPriceHistory` | ⛔ Not applicable [1](#notes) |
| `DescribeStaleSecurityGroups` | ❌ Not implemented |
| `DescribeStoreImageTasks` | ❌ Not implemented |
| `DescribeSubnets` | ✅ Implemented |
| `DescribeTags` | ✅ Implemented |
| `DescribeTrafficMirrorFilterRules` | ❌ Not implemented |
| `DescribeTrafficMirrorFilters` | ❌ Not implemented |
| `DescribeTrafficMirrorSessions` | ❌ Not implemented |
| `DescribeTrafficMirrorTargets` | ❌ Not implemented |
| `DescribeTransitGatewayAttachments` | ❌ Not implemented |
| `DescribeTransitGatewayConnectPeers` | ❌ Not implemented |
| `DescribeTransitGatewayConnects` | ❌ Not implemented |
| `DescribeTransitGatewayMulticastDomains` | ❌ Not implemented |
| `DescribeTransitGatewayPeeringAttachments` | ❌ Not implemented |
| `DescribeTransitGatewayPolicyTables` | ❌ Not implemented |
| `DescribeTransitGatewayRouteTableAnnouncements` | ❌ Not implemented |
| `DescribeTransitGatewayRouteTables` | ❌ Not implemented |
| `DescribeTransitGatewayVpcAttachments` | ❌ Not implemented |
| `DescribeTransitGateways` | ❌ Not implemented |
| `DescribeTrunkInterfaceAssociations` | ❌ Not implemented |
| `DescribeVerifiedAccessEndpoints` | ❌ Not implemented |
| `DescribeVerifiedAccessGroups` | ❌ Not implemented |
| `DescribeVerifiedAccessInstanceLoggingConfigurations` | ❌ Not implemented |
| `DescribeVerifiedAccessInstances` | ❌ Not implemented |
| `DescribeVerifiedAccessTrustProviders` | ❌ Not implemented |
| `DescribeVolumeAttribute` | ❌ Not implemented |
| `DescribeVolumeStatus` | ✅ Implemented |
| `DescribeVolumes` | ✅ Implemented |
| `DescribeVolumesModifications` | ✅ Implemented |
| `DescribeVpcAttribute` | ✅ Implemented |
| `DescribeVpcClassicLink` | ❌ Not implemented |
| `DescribeVpcClassicLinkDnsSupport` | ❌ Not implemented |
| `DescribeVpcEndpointConnectionNotifications` | ❌ Not implemented |
| `DescribeVpcEndpointConnections` | ❌ Not implemented |
| `DescribeVpcEndpointServiceConfigurations` | ❌ Not implemented |
| `DescribeVpcEndpointServicePermissions` | ❌ Not implemented |
| `DescribeVpcEndpointServices` | ❌ Not implemented |
| `DescribeVpcEndpoints` | ❌ Not implemented |
| `DescribeVpcPeeringConnections` | ❌ Not implemented |
| `DescribeVpcs` | ✅ Implemented |
| `DescribeVpnConnections` | ❌ Not implemented |
| `DescribeVpnGateways` | ❌ Not implemented |
| `DetachClassicLinkVpc` | ❌ Not implemented |
| `DetachInternetGateway` | ✅ Implemented |
| `DetachNetworkInterface` | ✅ Implemented |
| `DetachVerifiedAccessTrustProvider` | ❌ Not implemented |
| `DetachVolume` | ✅ Implemented |
| `DetachVpnGateway` | ❌ Not implemented |
| `DisableAddressTransfer` | ❌ Not implemented |
| `DisableAwsNetworkPerformanceMetricSubscription` | ❌ Not implemented |
| `DisableEbsEncryptionByDefault` | ✅ Implemented |
| `DisableFastLaunch` | ❌ Not implemented |
| `DisableFastSnapshotRestores` | ❌ Not implemented |
| `DisableImage` | ❌ Not implemented |
| `DisableImageBlockPublicAccess` | ❌ Not implemented |
| `DisableImageDeprecation` | ❌ Not implemented |
| `DisableImageDeregistrationProtection` | ❌ Not implemented |
| `DisableIpamOrganizationAdminAccount` | ❌ Not implemented |
| `DisableSerialConsoleAccess` | ✅ Implemented |
| `DisableSnapshotBlockPublicAccess` | ❌ Not implemented |
| `DisableTransitGatewayRouteTablePropagation` | ❌ Not implemented |
| `DisableVgwRoutePropagation` | ❌ Not implemented |
| `DisableVpcClassicLink` | ❌ Not implemented |
| `DisableVpcClassicLinkDnsSupport` | ❌ Not implemented |
| `DisassociateAddress` | ✅ Implemented |
| `DisassociateClientVpnTargetNetwork` | ❌ Not implemented |
| `DisassociateEnclaveCertificateIamRole` | ❌ Not implemented |
| `DisassociateIamInstanceProfile` | ✅ Implemented |
| `DisassociateInstanceEventWindow` | ❌ Not implemented |
| `DisassociateIpamByoasn` | ❌ Not implemented |
| `DisassociateIpamResourceDiscovery` | ❌ Not implemented |
| `DisassociateNatGatewayAddress` | ❌ Not implemented |
| `DisassociateRouteTable` | ✅ Implemented |
| `DisassociateSubnetCidrBlock` | ❌ Not implemented |
| `DisassociateTransitGatewayMulticastDomain` | ❌ Not implemented |
| `DisassociateTransitGatewayPolicyTable` | ❌ Not implemented |
| `DisassociateTransitGatewayRouteTable` | ❌ Not implemented |
| `DisassociateTrunkInterface` | ❌ Not implemented |
| `DisassociateVpcCidrBlock` | ❌ Not implemented |
| `EnableAddressTransfer` | ❌ Not implemented |
| `EnableAwsNetworkPerformanceMetricSubscription` | ❌ Not implemented |
| `EnableEbsEncryptionByDefault` | ✅ Implemented |
| `EnableFastLaunch` | ❌ Not implemented |
| `EnableFastSnapshotRestores` | ❌ Not implemented |
| `EnableImage` | ❌ Not implemented |
| `EnableImageBlockPublicAccess` | ❌ Not implemented |
| `EnableImageDeprecation` | ❌ Not implemented |
| `EnableImageDeregistrationProtection` | ❌ Not implemented |
| `EnableIpamOrganizationAdminAccount` | ❌ Not implemented |
| `EnableReachabilityAnalyzerOrganizationSharing` | ❌ Not implemented |
| `EnableSerialConsoleAccess` | ✅ Implemented |
| `EnableSnapshotBlockPublicAccess` | ❌ Not implemented |
| `EnableTransitGatewayRouteTablePropagation` | ❌ Not implemented |
| `EnableVgwRoutePropagation` | ❌ Not implemented |
| `EnableVolumeIO` | ❌ Not implemented |
| `EnableVpcClassicLink` | ❌ Not implemented |
| `EnableVpcClassicLinkDnsSupport` | ❌ Not implemented |
| `ExportClientVpnClientCertificateRevocationList` | ❌ Not implemented |
| `ExportClientVpnClientConfiguration` | ❌ Not implemented |
| `ExportImage` | ❌ Not implemented |
| `ExportTransitGatewayRoutes` | ❌ Not implemented |
| `GetAssociatedEnclaveCertificateIamRoles` | ❌ Not implemented |
| `GetAssociatedIpv6PoolCidrs` | ❌ Not implemented |
| `GetAwsNetworkPerformanceData` | ❌ Not implemented |
| `GetCapacityReservationUsage` | ❌ Not implemented |
| `GetCoipPoolUsage` | ❌ Not implemented |
| `GetConsoleOutput` | ✅ Implemented |
| `GetConsoleScreenshot` | ❌ Not implemented |
| `GetDefaultCreditSpecification` | ❌ Not implemented |
| `GetEbsDefaultKmsKeyId` | ❌ Not implemented |
| `GetEbsEncryptionByDefault` | ✅ Implemented |
| `GetFlowLogsIntegrationTemplate` | ❌ Not implemented |
| `GetGroupsForCapacityReservation` | ❌ Not implemented |
| `GetHostReservationPurchasePreview` | ❌ Not implemented |
| `GetImageBlockPublicAccessState` | ❌ Not implemented |
| `GetInstanceMetadataDefaults` | ❌ Not implemented |
| `GetInstanceTpmEkPub` | ❌ Not implemented |
| `GetInstanceTypesFromInstanceRequirements` | ❌ Not implemented |
| `GetInstanceUefiData` | ❌ Not implemented |
| `GetIpamAddressHistory` | ❌ Not implemented |
| `GetIpamDiscoveredAccounts` | ❌ Not implemented |
| `GetIpamDiscoveredPublicAddresses` | ❌ Not implemented |
| `GetIpamDiscoveredResourceCidrs` | ❌ Not implemented |
| `GetIpamPoolAllocations` | ❌ Not implemented |
| `GetIpamPoolCidrs` | ❌ Not implemented |
| `GetIpamResourceCidrs` | ❌ Not implemented |
| `GetLaunchTemplateData` | ❌ Not implemented |
| `GetManagedPrefixListAssociations` | ❌ Not implemented |
| `GetManagedPrefixListEntries` | ❌ Not implemented |
| `GetNetworkInsightsAccessScopeAnalysisFindings` | ❌ Not implemented |
| `GetNetworkInsightsAccessScopeContent` | ❌ Not implemented |
| `GetPasswordData` | ✅ Implemented |
| `GetReservedInstancesExchangeQuote` | ❌ Not implemented |
| `GetSecurityGroupsForVpc` | ❌ Not implemented |
| `GetSerialConsoleAccessStatus` | ✅ Implemented |
| `GetSnapshotBlockPublicAccessState` | ❌ Not implemented |
| `GetSpotPlacementScores` | ❌ Not implemented |
| `GetSubnetCidrReservations` | ❌ Not implemented |
| `GetTransitGatewayAttachmentPropagations` | ❌ Not implemented |
| `GetTransitGatewayMulticastDomainAssociations` | ❌ Not implemented |
| `GetTransitGatewayPolicyTableAssociations` | ❌ Not implemented |
| `GetTransitGatewayPolicyTableEntries` | ❌ Not implemented |
| `GetTransitGatewayPrefixListReferences` | ❌ Not implemented |
| `GetTransitGatewayRouteTableAssociations` | ❌ Not implemented |
| `GetTransitGatewayRouteTablePropagations` | ❌ Not implemented |
| `GetVerifiedAccessEndpointPolicy` | ❌ Not implemented |
| `GetVerifiedAccessGroupPolicy` | ❌ Not implemented |
| `GetVpnConnectionDeviceSampleConfiguration` | ❌ Not implemented |
| `GetVpnConnectionDeviceTypes` | ❌ Not implemented |
| `GetVpnTunnelReplacementStatus` | ❌ Not implemented |
| `ImportClientVpnClientCertificateRevocationList` | ❌ Not implemented |
| `ImportImage` | ❌ Not implemented |
| `ImportInstance` | ❌ Not implemented |
| `ImportKeyPair` | ✅ Implemented |
| `ImportSnapshot` | ❌ Not implemented |
| `ImportVolume` | ❌ Not implemented |
| `ListImagesInRecycleBin` | ❌ Not implemented |
| `ListSnapshotsInRecycleBin` | ❌ Not implemented |
| `LockSnapshot` | ❌ Not implemented |
| `ModifyAddressAttribute` | ❌ Not implemented |
| `ModifyAvailabilityZoneGroup` | ❌ Not implemented |
| `ModifyCapacityReservation` | ❌ Not implemented |
| `ModifyCapacityReservationFleet` | ❌ Not implemented |
| `ModifyClientVpnEndpoint` | ❌ Not implemented |
| `ModifyDefaultCreditSpecification` | ❌ Not implemented |
| `ModifyEbsDefaultKmsKeyId` | ❌ Not implemented |
| `ModifyFleet` | ❌ Not implemented |
| `ModifyFpgaImageAttribute` | ❌ Not implemented |
| `ModifyHosts` | ❌ Not implemented |
| `ModifyIdFormat` | ❌ Not implemented |
| `ModifyIdentityIdFormat` | ❌ Not implemented |
| `ModifyImageAttribute` | ✅ Implemented |
| `ModifyInstanceAttribute` | ✅ Implemented |
| `ModifyInstanceCapacityReservationAttributes` | ❌ Not implemented |
| `ModifyInstanceCreditSpecification` | ❌ Not implemented |
| `ModifyInstanceEventStartTime` | ❌ Not implemented |
| `ModifyInstanceEventWindow` | ❌ Not implemented |
| `ModifyInstanceMaintenanceOptions` | ❌ Not implemented |
| `ModifyInstanceMetadataDefaults` | ❌ Not implemented |
| `ModifyInstanceMetadataOptions` | ✅ Implemented |
| `ModifyInstancePlacement` | ❌ Not implemented |
| `ModifyIpam` | ❌ Not implemented |
| `ModifyIpamPool` | ❌ Not implemented |
| `ModifyIpamResourceCidr` | ❌ Not implemented |
| `ModifyIpamResourceDiscovery` | ❌ Not implemented |
| `ModifyIpamScope` | ❌ Not implemented |
| `ModifyLaunchTemplate` | ✅ Implemented |
| `ModifyLocalGatewayRoute` | ❌ Not implemented |
| `ModifyManagedPrefixList` | ❌ Not implemented |
| `ModifyNetworkInterfaceAttribute` | ✅ Implemented |
| `ModifyPrivateDnsNameOptions` | ❌ Not implemented |
| `ModifyReservedInstances` | ❌ Not implemented |
| `ModifySecurityGroupRules` | ❌ Not implemented |
| `ModifySnapshotAttribute` | ❌ Not implemented |
| `ModifySnapshotTier` | ❌ Not implemented |
| `ModifySpotFleetRequest` | ❌ Not implemented |
| `ModifySubnetAttribute` | ✅ Implemented |
| `ModifyTrafficMirrorFilterNetworkServices` | ❌ Not implemented |
| `ModifyTrafficMirrorFilterRule` | ❌ Not implemented |
| `ModifyTrafficMirrorSession` | ❌ Not implemented |
| `ModifyTransitGateway` | ❌ Not implemented |
| `ModifyTransitGatewayPrefixListReference` | ❌ Not implemented |
| `ModifyTransitGatewayVpcAttachment` | ❌ Not implemented |
| `ModifyVerifiedAccessEndpoint` | ❌ Not implemented |
| `ModifyVerifiedAccessEndpointPolicy` | ❌ Not implemented |
| `ModifyVerifiedAccessGroup` | ❌ Not implemented |
| `ModifyVerifiedAccessGroupPolicy` | ❌ Not implemented |
| `ModifyVerifiedAccessInstance` | ❌ Not implemented |
| `ModifyVerifiedAccessInstanceLoggingConfiguration` | ❌ Not implemented |
| `ModifyVerifiedAccessTrustProvider` | ❌ Not implemented |
| `ModifyVolume` | ✅ Implemented |
| `ModifyVolumeAttribute` | ❌ Not implemented |
| `ModifyVpcAttribute` | ✅ Implemented |
| `ModifyVpcEndpoint` | ❌ Not implemented |
| `ModifyVpcEndpointConnectionNotification` | ❌ Not implemented |
| `ModifyVpcEndpointServiceConfiguration` | ❌ Not implemented |
| `ModifyVpcEndpointServicePayerResponsibility` | ❌ Not implemented |
| `ModifyVpcEndpointServicePermissions` | ❌ Not implemented |
| `ModifyVpcPeeringConnectionOptions` | ❌ Not implemented |
| `ModifyVpcTenancy` | ❌ Not implemented |
| `ModifyVpnConnection` | ❌ Not implemented |
| `ModifyVpnConnectionOptions` | ❌ Not implemented |
| `ModifyVpnTunnelCertificate` | ❌ Not implemented |
| `ModifyVpnTunnelOptions` | ❌ Not implemented |
| `MonitorInstances` | ✅ Implemented |
| `MoveAddressToVpc` | ❌ Not implemented |
| `MoveByoipCidrToIpam` | ❌ Not implemented |
| `ProvisionByoipCidr` | ❌ Not implemented |
| `ProvisionIpamByoasn` | ❌ Not implemented |
| `ProvisionIpamPoolCidr` | ❌ Not implemented |
| `ProvisionPublicIpv4PoolCidr` | ❌ Not implemented |
| `PurchaseCapacityBlock` | ❌ Not implemented |
| `PurchaseHostReservation` | ❌ Not implemented |
| `PurchaseReservedInstancesOffering` | ❌ Not implemented |
| `PurchaseScheduledInstances` | ❌ Not implemented |
| `RebootInstances` | ✅ Implemented |
| `RegisterImage` | ✅ Implemented |
| `RegisterInstanceEventNotificationAttributes` | ❌ Not implemented |
| `RegisterTransitGatewayMulticastGroupMembers` | ❌ Not implemented |
| `RegisterTransitGatewayMulticastGroupSources` | ❌ Not implemented |
| `RejectTransitGatewayMulticastDomainAssociations` | ❌ Not implemented |
| `RejectTransitGatewayPeeringAttachment` | ❌ Not implemented |
| `RejectTransitGatewayVpcAttachment` | ❌ Not implemented |
| `RejectVpcEndpointConnections` | ❌ Not implemented |
| `RejectVpcPeeringConnection` | ❌ Not implemented |
| `ReleaseAddress` | ✅ Implemented |
| `ReleaseHosts` | ❌ Not implemented |
| `ReleaseIpamPoolAllocation` | ❌ Not implemented |
| `ReplaceIamInstanceProfileAssociation` | ✅ Implemented |
| `ReplaceNetworkAclAssociation` | ❌ Not implemented |
| `ReplaceNetworkAclEntry` | ❌ Not implemented |
| `ReplaceRoute` | ✅ Implemented |
| `ReplaceRouteTableAssociation` | ✅ Implemented |
| `ReplaceTransitGatewayRoute` | ❌ Not implemented |
| `ReplaceVpnTunnel` | ❌ Not implemented |
| `ReportInstanceStatus` | ❌ Not implemented |
| `RequestSpotFleet` | ❌ Not implemented |
| `RequestSpotInstances` | ✅ Implemented |
| `ResetAddressAttribute` | ❌ Not implemented |
| `ResetEbsDefaultKmsKeyId` | ❌ Not implemented |
| `ResetFpgaImageAttribute` | ❌ Not implemented |
| `ResetImageAttribute` | ✅ Implemented |
| `ResetInstanceAttribute` | ❌ Not implemented |
| `ResetNetworkInterfaceAttribute` | ❌ Not implemented |
| `ResetSnapshotAttribute` | ❌ Not implemented |
| `RestoreAddressToClassic` | ❌ Not implemented |
| `RestoreImageFromRecycleBin` | ❌ Not implemented |
| `RestoreManagedPrefixListVersion` | ❌ Not implemented |
| `RestoreSnapshotFromRecycleBin` | ❌ Not implemented |
| `RestoreSnapshotTier` | ❌ Not implemented |
| `RevokeClientVpnIngress` | ❌ Not implemented |
| `RevokeSecurityGroupEgress` | ✅ Implemented |
| `RevokeSecurityGroupIngress` | ✅ Implemented |
| `RunInstances` | ✅ Implemented |
| `RunScheduledInstances` | ❌ Not implemented |
| `SearchLocalGatewayRoutes` | ❌ Not implemented |
| `SearchTransitGatewayMulticastGroups` | ❌ Not implemented |
| `SearchTransitGatewayRoutes` | ❌ Not implemented |
| `SendDiagnosticInterrupt` | ❌ Not implemented |
| `StartInstances` | ✅ Implemented |
| `StartNetworkInsightsAccessScopeAnalysis` | ❌ Not implemented |
| `StartNetworkInsightsAnalysis` | ❌ Not implemented |
| `StartVpcEndpointServicePrivateDnsVerification` | ❌ Not implemented |
| `StopInstances` | ✅ Implemented |
| `TerminateClientVpnConnections` | ❌ Not implemented |
| `TerminateInstances` | ✅ Implemented |
| `UnassignIpv6Addresses` | ❌ Not implemented |
| `UnassignPrivateIpAddresses` | ❌ Not implemented |
| `UnassignPrivateNatGatewayAddress` | ❌ Not implemented |
| `UnlockSnapshot` | ❌ Not implemented |
| `UnmonitorInstances` | ✅ Implemented |
| `UpdateSecurityGroupRuleDescriptionsEgress` | ❌ Not implemented |
| `UpdateSecurityGroupRuleDescriptionsIngress` | ❌ Not implemented |
| `WithdrawByoipCidr` | ❌ Not implemented |

### Notes

1. On owned hardware there is no spot-to-on-demand price differential, so any figure would be invented.
