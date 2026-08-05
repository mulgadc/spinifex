# AWS model operation coverage

Generated from the cached `aws-sdk-go v1.55.8` `api-2.json` models and Spinifex's authoritative gateway dispatch tables.

Implemented means a modelled operation is registered to a real handler. Stub and deliberately unsupported handlers are reported separately. Shape conformance does not imply behavioural conformance.

| Service | API version | Modelled | Registered | Implemented | Stubbed | Unsupported | Missing | Extra |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| acm | 2015-12-08 | 15 | 9 | 9 | 0 | 0 | 6 | 0 |
| ec2 | 2016-11-15 | 625 | 119 | 119 | 0 | 0 | 506 | 0 |
| ecr | 2015-09-21 | 47 | 39 | 19 | 13 | 7 | 11 | 3 |
| ecs | 2014-11-13 | 56 | 39 | 31 | 5 | 0 | 20 | 3 |
| elasticloadbalancingv2 | 2015-12-01 | 46 | 37 | 33 | 0 | 0 | 13 | 4 |
| iam | 2010-05-08 | 159 | 75 | 75 | 0 | 0 | 84 | 0 |
| s3 | 2006-03-01 | 99 | — | — | — | — | — | — |
| sts | 2011-06-15 | 8 | 4 | 4 | 0 | 0 | 4 | 0 |

## acm

Implements **9 of 15** modelled operations (60.0%).

<details><summary>Implemented (9)</summary>

`AddTagsToCertificate`, `DeleteCertificate`, `DescribeCertificate`, `GetCertificate`, `ImportCertificate`, `ListCertificates`, `ListTagsForCertificate`, `RemoveTagsFromCertificate`

`RequestCertificate`


</details>

<details><summary>Missing from dispatch (6)</summary>

`ExportCertificate`, `GetAccountConfiguration`, `PutAccountConfiguration`, `RenewCertificate`, `ResendValidationEmail`, `UpdateCertificateOptions`


</details>

<details><summary>Registered stubs (0)</summary>

None.

</details>

<details><summary>Deliberately unsupported (0)</summary>

None.

</details>

<details><summary>Registered outside the pinned model (0)</summary>

None.

</details>

## ec2

Implements **119 of 625** modelled operations (19.0%).

<details><summary>Implemented (119)</summary>

`AllocateAddress`, `AssociateAddress`, `AssociateIamInstanceProfile`, `AssociateRouteTable`, `AttachInternetGateway`, `AttachNetworkInterface`, `AttachVolume`, `AuthorizeSecurityGroupEgress`

`AuthorizeSecurityGroupIngress`, `CancelCapacityReservation`, `CancelSpotInstanceRequests`, `CopyImage`, `CopySnapshot`, `CreateCapacityReservation`, `CreateEgressOnlyInternetGateway`, `CreateImage`

`CreateInternetGateway`, `CreateKeyPair`, `CreateLaunchTemplate`, `CreateLaunchTemplateVersion`, `CreateNatGateway`, `CreateNetworkInterface`, `CreatePlacementGroup`, `CreateRoute`

`CreateRouteTable`, `CreateSecurityGroup`, `CreateSnapshot`, `CreateSubnet`, `CreateTags`, `CreateVolume`, `CreateVpc`, `DeleteEgressOnlyInternetGateway`

`DeleteInternetGateway`, `DeleteKeyPair`, `DeleteLaunchTemplate`, `DeleteLaunchTemplateVersions`, `DeleteNatGateway`, `DeleteNetworkInterface`, `DeletePlacementGroup`, `DeleteRoute`

`DeleteRouteTable`, `DeleteSecurityGroup`, `DeleteSnapshot`, `DeleteSubnet`, `DeleteTags`, `DeleteVolume`, `DeleteVpc`, `DeregisterImage`

`DescribeAccountAttributes`, `DescribeAddresses`, `DescribeAddressesAttribute`, `DescribeAvailabilityZones`, `DescribeCapacityReservations`, `DescribeEgressOnlyInternetGateways`, `DescribeIamInstanceProfileAssociations`, `DescribeImageAttribute`

`DescribeImages`, `DescribeInstanceAttribute`, `DescribeInstanceCreditSpecifications`, `DescribeInstanceStatus`, `DescribeInstanceTypes`, `DescribeInstances`, `DescribeInternetGateways`, `DescribeKeyPairs`

`DescribeLaunchTemplateVersions`, `DescribeLaunchTemplates`, `DescribeNatGateways`, `DescribeNetworkInterfaces`, `DescribePlacementGroups`, `DescribeRegions`, `DescribeRouteTables`, `DescribeSecurityGroupRules`

`DescribeSecurityGroups`, `DescribeSnapshots`, `DescribeSpotInstanceRequests`, `DescribeSubnets`, `DescribeTags`, `DescribeVolumeStatus`, `DescribeVolumes`, `DescribeVolumesModifications`

`DescribeVpcAttribute`, `DescribeVpcs`, `DetachInternetGateway`, `DetachNetworkInterface`, `DetachVolume`, `DisableEbsEncryptionByDefault`, `DisableSerialConsoleAccess`, `DisassociateAddress`

`DisassociateIamInstanceProfile`, `DisassociateRouteTable`, `EnableEbsEncryptionByDefault`, `EnableSerialConsoleAccess`, `GetConsoleOutput`, `GetEbsEncryptionByDefault`, `GetPasswordData`, `GetSerialConsoleAccessStatus`

`ImportKeyPair`, `ModifyImageAttribute`, `ModifyInstanceAttribute`, `ModifyInstanceMetadataOptions`, `ModifyLaunchTemplate`, `ModifyNetworkInterfaceAttribute`, `ModifySubnetAttribute`, `ModifyVolume`

`ModifyVpcAttribute`, `RebootInstances`, `RegisterImage`, `ReleaseAddress`, `ReplaceIamInstanceProfileAssociation`, `ReplaceRoute`, `ReplaceRouteTableAssociation`, `RequestSpotInstances`

`ResetImageAttribute`, `RevokeSecurityGroupEgress`, `RevokeSecurityGroupIngress`, `RunInstances`, `StartInstances`, `StopInstances`, `TerminateInstances`


</details>

<details><summary>Missing from dispatch (506)</summary>

`AcceptAddressTransfer`, `AcceptReservedInstancesExchangeQuote`, `AcceptTransitGatewayMulticastDomainAssociations`, `AcceptTransitGatewayPeeringAttachment`, `AcceptTransitGatewayVpcAttachment`, `AcceptVpcEndpointConnections`, `AcceptVpcPeeringConnection`, `AdvertiseByoipCidr`

`AllocateHosts`, `AllocateIpamPoolCidr`, `ApplySecurityGroupsToClientVpnTargetNetwork`, `AssignIpv6Addresses`, `AssignPrivateIpAddresses`, `AssignPrivateNatGatewayAddress`, `AssociateClientVpnTargetNetwork`, `AssociateDhcpOptions`

`AssociateEnclaveCertificateIamRole`, `AssociateInstanceEventWindow`, `AssociateIpamByoasn`, `AssociateIpamResourceDiscovery`, `AssociateNatGatewayAddress`, `AssociateSubnetCidrBlock`, `AssociateTransitGatewayMulticastDomain`, `AssociateTransitGatewayPolicyTable`

`AssociateTransitGatewayRouteTable`, `AssociateTrunkInterface`, `AssociateVpcCidrBlock`, `AttachClassicLinkVpc`, `AttachVerifiedAccessTrustProvider`, `AttachVpnGateway`, `AuthorizeClientVpnIngress`, `BundleInstance`

`CancelBundleTask`, `CancelCapacityReservationFleets`, `CancelConversionTask`, `CancelExportTask`, `CancelImageLaunchPermission`, `CancelImportTask`, `CancelReservedInstancesListing`, `CancelSpotFleetRequests`

`ConfirmProductInstance`, `CopyFpgaImage`, `CreateCapacityReservationFleet`, `CreateCarrierGateway`, `CreateClientVpnEndpoint`, `CreateClientVpnRoute`, `CreateCoipCidr`, `CreateCoipPool`

`CreateCustomerGateway`, `CreateDefaultSubnet`, `CreateDefaultVpc`, `CreateDhcpOptions`, `CreateFleet`, `CreateFlowLogs`, `CreateFpgaImage`, `CreateInstanceConnectEndpoint`

`CreateInstanceEventWindow`, `CreateInstanceExportTask`, `CreateIpam`, `CreateIpamExternalResourceVerificationToken`, `CreateIpamPool`, `CreateIpamResourceDiscovery`, `CreateIpamScope`, `CreateLocalGatewayRoute`

`CreateLocalGatewayRouteTable`, `CreateLocalGatewayRouteTableVirtualInterfaceGroupAssociation`, `CreateLocalGatewayRouteTableVpcAssociation`, `CreateManagedPrefixList`, `CreateNetworkAcl`, `CreateNetworkAclEntry`, `CreateNetworkInsightsAccessScope`, `CreateNetworkInsightsPath`

`CreateNetworkInterfacePermission`, `CreatePublicIpv4Pool`, `CreateReplaceRootVolumeTask`, `CreateReservedInstancesListing`, `CreateRestoreImageTask`, `CreateSnapshots`, `CreateSpotDatafeedSubscription`, `CreateStoreImageTask`

`CreateSubnetCidrReservation`, `CreateTrafficMirrorFilter`, `CreateTrafficMirrorFilterRule`, `CreateTrafficMirrorSession`, `CreateTrafficMirrorTarget`, `CreateTransitGateway`, `CreateTransitGatewayConnect`, `CreateTransitGatewayConnectPeer`

`CreateTransitGatewayMulticastDomain`, `CreateTransitGatewayPeeringAttachment`, `CreateTransitGatewayPolicyTable`, `CreateTransitGatewayPrefixListReference`, `CreateTransitGatewayRoute`, `CreateTransitGatewayRouteTable`, `CreateTransitGatewayRouteTableAnnouncement`, `CreateTransitGatewayVpcAttachment`

`CreateVerifiedAccessEndpoint`, `CreateVerifiedAccessGroup`, `CreateVerifiedAccessInstance`, `CreateVerifiedAccessTrustProvider`, `CreateVpcEndpoint`, `CreateVpcEndpointConnectionNotification`, `CreateVpcEndpointServiceConfiguration`, `CreateVpcPeeringConnection`

`CreateVpnConnection`, `CreateVpnConnectionRoute`, `CreateVpnGateway`, `DeleteCarrierGateway`, `DeleteClientVpnEndpoint`, `DeleteClientVpnRoute`, `DeleteCoipCidr`, `DeleteCoipPool`

`DeleteCustomerGateway`, `DeleteDhcpOptions`, `DeleteFleets`, `DeleteFlowLogs`, `DeleteFpgaImage`, `DeleteInstanceConnectEndpoint`, `DeleteInstanceEventWindow`, `DeleteIpam`

`DeleteIpamExternalResourceVerificationToken`, `DeleteIpamPool`, `DeleteIpamResourceDiscovery`, `DeleteIpamScope`, `DeleteLocalGatewayRoute`, `DeleteLocalGatewayRouteTable`, `DeleteLocalGatewayRouteTableVirtualInterfaceGroupAssociation`, `DeleteLocalGatewayRouteTableVpcAssociation`

`DeleteManagedPrefixList`, `DeleteNetworkAcl`, `DeleteNetworkAclEntry`, `DeleteNetworkInsightsAccessScope`, `DeleteNetworkInsightsAccessScopeAnalysis`, `DeleteNetworkInsightsAnalysis`, `DeleteNetworkInsightsPath`, `DeleteNetworkInterfacePermission`

`DeletePublicIpv4Pool`, `DeleteQueuedReservedInstances`, `DeleteSpotDatafeedSubscription`, `DeleteSubnetCidrReservation`, `DeleteTrafficMirrorFilter`, `DeleteTrafficMirrorFilterRule`, `DeleteTrafficMirrorSession`, `DeleteTrafficMirrorTarget`

`DeleteTransitGateway`, `DeleteTransitGatewayConnect`, `DeleteTransitGatewayConnectPeer`, `DeleteTransitGatewayMulticastDomain`, `DeleteTransitGatewayPeeringAttachment`, `DeleteTransitGatewayPolicyTable`, `DeleteTransitGatewayPrefixListReference`, `DeleteTransitGatewayRoute`

`DeleteTransitGatewayRouteTable`, `DeleteTransitGatewayRouteTableAnnouncement`, `DeleteTransitGatewayVpcAttachment`, `DeleteVerifiedAccessEndpoint`, `DeleteVerifiedAccessGroup`, `DeleteVerifiedAccessInstance`, `DeleteVerifiedAccessTrustProvider`, `DeleteVpcEndpointConnectionNotifications`

`DeleteVpcEndpointServiceConfigurations`, `DeleteVpcEndpoints`, `DeleteVpcPeeringConnection`, `DeleteVpnConnection`, `DeleteVpnConnectionRoute`, `DeleteVpnGateway`, `DeprovisionByoipCidr`, `DeprovisionIpamByoasn`

`DeprovisionIpamPoolCidr`, `DeprovisionPublicIpv4PoolCidr`, `DeregisterInstanceEventNotificationAttributes`, `DeregisterTransitGatewayMulticastGroupMembers`, `DeregisterTransitGatewayMulticastGroupSources`, `DescribeAddressTransfers`, `DescribeAggregateIdFormat`, `DescribeAwsNetworkPerformanceMetricSubscriptions`

`DescribeBundleTasks`, `DescribeByoipCidrs`, `DescribeCapacityBlockOfferings`, `DescribeCapacityReservationFleets`, `DescribeCarrierGateways`, `DescribeClassicLinkInstances`, `DescribeClientVpnAuthorizationRules`, `DescribeClientVpnConnections`

`DescribeClientVpnEndpoints`, `DescribeClientVpnRoutes`, `DescribeClientVpnTargetNetworks`, `DescribeCoipPools`, `DescribeConversionTasks`, `DescribeCustomerGateways`, `DescribeDhcpOptions`, `DescribeElasticGpus`

`DescribeExportImageTasks`, `DescribeExportTasks`, `DescribeFastLaunchImages`, `DescribeFastSnapshotRestores`, `DescribeFleetHistory`, `DescribeFleetInstances`, `DescribeFleets`, `DescribeFlowLogs`

`DescribeFpgaImageAttribute`, `DescribeFpgaImages`, `DescribeHostReservationOfferings`, `DescribeHostReservations`, `DescribeHosts`, `DescribeIdFormat`, `DescribeIdentityIdFormat`, `DescribeImportImageTasks`

`DescribeImportSnapshotTasks`, `DescribeInstanceConnectEndpoints`, `DescribeInstanceEventNotificationAttributes`, `DescribeInstanceEventWindows`, `DescribeInstanceTopology`, `DescribeInstanceTypeOfferings`, `DescribeIpamByoasn`, `DescribeIpamExternalResourceVerificationTokens`

`DescribeIpamPools`, `DescribeIpamResourceDiscoveries`, `DescribeIpamResourceDiscoveryAssociations`, `DescribeIpamScopes`, `DescribeIpams`, `DescribeIpv6Pools`, `DescribeLocalGatewayRouteTableVirtualInterfaceGroupAssociations`, `DescribeLocalGatewayRouteTableVpcAssociations`

`DescribeLocalGatewayRouteTables`, `DescribeLocalGatewayVirtualInterfaceGroups`, `DescribeLocalGatewayVirtualInterfaces`, `DescribeLocalGateways`, `DescribeLockedSnapshots`, `DescribeMacHosts`, `DescribeManagedPrefixLists`, `DescribeMovingAddresses`

`DescribeNetworkAcls`, `DescribeNetworkInsightsAccessScopeAnalyses`, `DescribeNetworkInsightsAccessScopes`, `DescribeNetworkInsightsAnalyses`, `DescribeNetworkInsightsPaths`, `DescribeNetworkInterfaceAttribute`, `DescribeNetworkInterfacePermissions`, `DescribePrefixLists`

`DescribePrincipalIdFormat`, `DescribePublicIpv4Pools`, `DescribeReplaceRootVolumeTasks`, `DescribeReservedInstances`, `DescribeReservedInstancesListings`, `DescribeReservedInstancesModifications`, `DescribeReservedInstancesOfferings`, `DescribeScheduledInstanceAvailability`

`DescribeScheduledInstances`, `DescribeSecurityGroupReferences`, `DescribeSnapshotAttribute`, `DescribeSnapshotTierStatus`, `DescribeSpotDatafeedSubscription`, `DescribeSpotFleetInstances`, `DescribeSpotFleetRequestHistory`, `DescribeSpotFleetRequests`

`DescribeSpotPriceHistory`, `DescribeStaleSecurityGroups`, `DescribeStoreImageTasks`, `DescribeTrafficMirrorFilterRules`, `DescribeTrafficMirrorFilters`, `DescribeTrafficMirrorSessions`, `DescribeTrafficMirrorTargets`, `DescribeTransitGatewayAttachments`

`DescribeTransitGatewayConnectPeers`, `DescribeTransitGatewayConnects`, `DescribeTransitGatewayMulticastDomains`, `DescribeTransitGatewayPeeringAttachments`, `DescribeTransitGatewayPolicyTables`, `DescribeTransitGatewayRouteTableAnnouncements`, `DescribeTransitGatewayRouteTables`, `DescribeTransitGatewayVpcAttachments`

`DescribeTransitGateways`, `DescribeTrunkInterfaceAssociations`, `DescribeVerifiedAccessEndpoints`, `DescribeVerifiedAccessGroups`, `DescribeVerifiedAccessInstanceLoggingConfigurations`, `DescribeVerifiedAccessInstances`, `DescribeVerifiedAccessTrustProviders`, `DescribeVolumeAttribute`

`DescribeVpcClassicLink`, `DescribeVpcClassicLinkDnsSupport`, `DescribeVpcEndpointConnectionNotifications`, `DescribeVpcEndpointConnections`, `DescribeVpcEndpointServiceConfigurations`, `DescribeVpcEndpointServicePermissions`, `DescribeVpcEndpointServices`, `DescribeVpcEndpoints`

`DescribeVpcPeeringConnections`, `DescribeVpnConnections`, `DescribeVpnGateways`, `DetachClassicLinkVpc`, `DetachVerifiedAccessTrustProvider`, `DetachVpnGateway`, `DisableAddressTransfer`, `DisableAwsNetworkPerformanceMetricSubscription`

`DisableFastLaunch`, `DisableFastSnapshotRestores`, `DisableImage`, `DisableImageBlockPublicAccess`, `DisableImageDeprecation`, `DisableImageDeregistrationProtection`, `DisableIpamOrganizationAdminAccount`, `DisableSnapshotBlockPublicAccess`

`DisableTransitGatewayRouteTablePropagation`, `DisableVgwRoutePropagation`, `DisableVpcClassicLink`, `DisableVpcClassicLinkDnsSupport`, `DisassociateClientVpnTargetNetwork`, `DisassociateEnclaveCertificateIamRole`, `DisassociateInstanceEventWindow`, `DisassociateIpamByoasn`

`DisassociateIpamResourceDiscovery`, `DisassociateNatGatewayAddress`, `DisassociateSubnetCidrBlock`, `DisassociateTransitGatewayMulticastDomain`, `DisassociateTransitGatewayPolicyTable`, `DisassociateTransitGatewayRouteTable`, `DisassociateTrunkInterface`, `DisassociateVpcCidrBlock`

`EnableAddressTransfer`, `EnableAwsNetworkPerformanceMetricSubscription`, `EnableFastLaunch`, `EnableFastSnapshotRestores`, `EnableImage`, `EnableImageBlockPublicAccess`, `EnableImageDeprecation`, `EnableImageDeregistrationProtection`

`EnableIpamOrganizationAdminAccount`, `EnableReachabilityAnalyzerOrganizationSharing`, `EnableSnapshotBlockPublicAccess`, `EnableTransitGatewayRouteTablePropagation`, `EnableVgwRoutePropagation`, `EnableVolumeIO`, `EnableVpcClassicLink`, `EnableVpcClassicLinkDnsSupport`

`ExportClientVpnClientCertificateRevocationList`, `ExportClientVpnClientConfiguration`, `ExportImage`, `ExportTransitGatewayRoutes`, `GetAssociatedEnclaveCertificateIamRoles`, `GetAssociatedIpv6PoolCidrs`, `GetAwsNetworkPerformanceData`, `GetCapacityReservationUsage`

`GetCoipPoolUsage`, `GetConsoleScreenshot`, `GetDefaultCreditSpecification`, `GetEbsDefaultKmsKeyId`, `GetFlowLogsIntegrationTemplate`, `GetGroupsForCapacityReservation`, `GetHostReservationPurchasePreview`, `GetImageBlockPublicAccessState`

`GetInstanceMetadataDefaults`, `GetInstanceTpmEkPub`, `GetInstanceTypesFromInstanceRequirements`, `GetInstanceUefiData`, `GetIpamAddressHistory`, `GetIpamDiscoveredAccounts`, `GetIpamDiscoveredPublicAddresses`, `GetIpamDiscoveredResourceCidrs`

`GetIpamPoolAllocations`, `GetIpamPoolCidrs`, `GetIpamResourceCidrs`, `GetLaunchTemplateData`, `GetManagedPrefixListAssociations`, `GetManagedPrefixListEntries`, `GetNetworkInsightsAccessScopeAnalysisFindings`, `GetNetworkInsightsAccessScopeContent`

`GetReservedInstancesExchangeQuote`, `GetSecurityGroupsForVpc`, `GetSnapshotBlockPublicAccessState`, `GetSpotPlacementScores`, `GetSubnetCidrReservations`, `GetTransitGatewayAttachmentPropagations`, `GetTransitGatewayMulticastDomainAssociations`, `GetTransitGatewayPolicyTableAssociations`

`GetTransitGatewayPolicyTableEntries`, `GetTransitGatewayPrefixListReferences`, `GetTransitGatewayRouteTableAssociations`, `GetTransitGatewayRouteTablePropagations`, `GetVerifiedAccessEndpointPolicy`, `GetVerifiedAccessGroupPolicy`, `GetVpnConnectionDeviceSampleConfiguration`, `GetVpnConnectionDeviceTypes`

`GetVpnTunnelReplacementStatus`, `ImportClientVpnClientCertificateRevocationList`, `ImportImage`, `ImportInstance`, `ImportSnapshot`, `ImportVolume`, `ListImagesInRecycleBin`, `ListSnapshotsInRecycleBin`

`LockSnapshot`, `ModifyAddressAttribute`, `ModifyAvailabilityZoneGroup`, `ModifyCapacityReservation`, `ModifyCapacityReservationFleet`, `ModifyClientVpnEndpoint`, `ModifyDefaultCreditSpecification`, `ModifyEbsDefaultKmsKeyId`

`ModifyFleet`, `ModifyFpgaImageAttribute`, `ModifyHosts`, `ModifyIdFormat`, `ModifyIdentityIdFormat`, `ModifyInstanceCapacityReservationAttributes`, `ModifyInstanceCreditSpecification`, `ModifyInstanceEventStartTime`

`ModifyInstanceEventWindow`, `ModifyInstanceMaintenanceOptions`, `ModifyInstanceMetadataDefaults`, `ModifyInstancePlacement`, `ModifyIpam`, `ModifyIpamPool`, `ModifyIpamResourceCidr`, `ModifyIpamResourceDiscovery`

`ModifyIpamScope`, `ModifyLocalGatewayRoute`, `ModifyManagedPrefixList`, `ModifyPrivateDnsNameOptions`, `ModifyReservedInstances`, `ModifySecurityGroupRules`, `ModifySnapshotAttribute`, `ModifySnapshotTier`

`ModifySpotFleetRequest`, `ModifyTrafficMirrorFilterNetworkServices`, `ModifyTrafficMirrorFilterRule`, `ModifyTrafficMirrorSession`, `ModifyTransitGateway`, `ModifyTransitGatewayPrefixListReference`, `ModifyTransitGatewayVpcAttachment`, `ModifyVerifiedAccessEndpoint`

`ModifyVerifiedAccessEndpointPolicy`, `ModifyVerifiedAccessGroup`, `ModifyVerifiedAccessGroupPolicy`, `ModifyVerifiedAccessInstance`, `ModifyVerifiedAccessInstanceLoggingConfiguration`, `ModifyVerifiedAccessTrustProvider`, `ModifyVolumeAttribute`, `ModifyVpcEndpoint`

`ModifyVpcEndpointConnectionNotification`, `ModifyVpcEndpointServiceConfiguration`, `ModifyVpcEndpointServicePayerResponsibility`, `ModifyVpcEndpointServicePermissions`, `ModifyVpcPeeringConnectionOptions`, `ModifyVpcTenancy`, `ModifyVpnConnection`, `ModifyVpnConnectionOptions`

`ModifyVpnTunnelCertificate`, `ModifyVpnTunnelOptions`, `MonitorInstances`, `MoveAddressToVpc`, `MoveByoipCidrToIpam`, `ProvisionByoipCidr`, `ProvisionIpamByoasn`, `ProvisionIpamPoolCidr`

`ProvisionPublicIpv4PoolCidr`, `PurchaseCapacityBlock`, `PurchaseHostReservation`, `PurchaseReservedInstancesOffering`, `PurchaseScheduledInstances`, `RegisterInstanceEventNotificationAttributes`, `RegisterTransitGatewayMulticastGroupMembers`, `RegisterTransitGatewayMulticastGroupSources`

`RejectTransitGatewayMulticastDomainAssociations`, `RejectTransitGatewayPeeringAttachment`, `RejectTransitGatewayVpcAttachment`, `RejectVpcEndpointConnections`, `RejectVpcPeeringConnection`, `ReleaseHosts`, `ReleaseIpamPoolAllocation`, `ReplaceNetworkAclAssociation`

`ReplaceNetworkAclEntry`, `ReplaceTransitGatewayRoute`, `ReplaceVpnTunnel`, `ReportInstanceStatus`, `RequestSpotFleet`, `ResetAddressAttribute`, `ResetEbsDefaultKmsKeyId`, `ResetFpgaImageAttribute`

`ResetInstanceAttribute`, `ResetNetworkInterfaceAttribute`, `ResetSnapshotAttribute`, `RestoreAddressToClassic`, `RestoreImageFromRecycleBin`, `RestoreManagedPrefixListVersion`, `RestoreSnapshotFromRecycleBin`, `RestoreSnapshotTier`

`RevokeClientVpnIngress`, `RunScheduledInstances`, `SearchLocalGatewayRoutes`, `SearchTransitGatewayMulticastGroups`, `SearchTransitGatewayRoutes`, `SendDiagnosticInterrupt`, `StartNetworkInsightsAccessScopeAnalysis`, `StartNetworkInsightsAnalysis`

`StartVpcEndpointServicePrivateDnsVerification`, `TerminateClientVpnConnections`, `UnassignIpv6Addresses`, `UnassignPrivateIpAddresses`, `UnassignPrivateNatGatewayAddress`, `UnlockSnapshot`, `UnmonitorInstances`, `UpdateSecurityGroupRuleDescriptionsEgress`

`UpdateSecurityGroupRuleDescriptionsIngress`, `WithdrawByoipCidr`


</details>

<details><summary>Registered stubs (0)</summary>

None.

</details>

<details><summary>Deliberately unsupported (0)</summary>

None.

</details>

<details><summary>Registered outside the pinned model (0)</summary>

None.

</details>

## ecr

Implements **19 of 47** modelled operations (40.4%).

<details><summary>Implemented (19)</summary>

`BatchDeleteImage`, `BatchGetImage`, `CreateRepository`, `DeleteLifecyclePolicy`, `DeleteRepository`, `DeleteRepositoryPolicy`, `DescribeImages`, `DescribeRepositories`

`GetAuthorizationToken`, `GetLifecyclePolicy`, `GetLifecyclePolicyPreview`, `GetRepositoryPolicy`, `ListImages`, `ListTagsForResource`, `PutImage`, `PutImageTagMutability`

`PutLifecyclePolicy`, `SetRepositoryPolicy`, `StartLifecyclePolicyPreview`


</details>

<details><summary>Missing from dispatch (11)</summary>

`CreatePullThroughCacheRule`, `CreateRepositoryCreationTemplate`, `DeletePullThroughCacheRule`, `DeleteRegistryPolicy`, `DeleteRepositoryCreationTemplate`, `DescribeImageReplicationStatus`, `DescribePullThroughCacheRules`, `DescribeRepositoryCreationTemplates`

`UpdatePullThroughCacheRule`, `UpdateRepositoryCreationTemplate`, `ValidatePullThroughCacheRule`


</details>

<details><summary>Registered stubs (13)</summary>

`BatchCheckLayerAvailability`, `CompleteLayerUpload`, `DescribeRegistry`, `GetDownloadUrlForLayer`, `GetRegistryPolicy`, `InitiateLayerUpload`, `ListRepositories`, `PutRegistryPolicy`

`PutReplicationConfiguration`, `ReplicateImage`, `TagResource`, `UntagResource`, `UploadLayerPart`


</details>

<details><summary>Deliberately unsupported (7)</summary>

`BatchGetRepositoryScanningConfiguration`, `DescribeImageScanFindings`, `GetImageScanningConfiguration`, `GetRegistryScanningConfiguration`, `PutImageScanningConfiguration`, `PutRegistryScanningConfiguration`, `StartImageScan`


</details>

<details><summary>Registered outside the pinned model (3)</summary>

`GetImageScanningConfiguration`, `ListRepositories`, `ReplicateImage`


</details>

## ecs

Implements **31 of 56** modelled operations (55.4%).

<details><summary>Implemented (31)</summary>

`CreateCapacityProvider`, `CreateCluster`, `CreateService`, `DeleteCapacityProvider`, `DeleteCluster`, `DeleteService`, `DeregisterContainerInstance`, `DeregisterTaskDefinition`

`DescribeCapacityProviders`, `DescribeClusters`, `DescribeContainerInstances`, `DescribeServices`, `DescribeTaskDefinition`, `DescribeTasks`, `ListClusters`, `ListContainerInstances`

`ListServices`, `ListTagsForResource`, `ListTaskDefinitions`, `ListTasks`, `PutClusterCapacityProviders`, `RegisterContainerInstance`, `RegisterTaskDefinition`, `RunTask`

`StartTask`, `StopTask`, `SubmitTaskStateChange`, `TagResource`, `UntagResource`, `UpdateContainerInstancesState`, `UpdateService`


</details>

<details><summary>Missing from dispatch (20)</summary>

`CreateTaskSet`, `DeleteAccountSetting`, `DeleteAttributes`, `DeleteTaskDefinitions`, `DeleteTaskSet`, `DescribeTaskSets`, `DiscoverPollEndpoint`, `ExecuteCommand`

`GetTaskProtection`, `ListAttributes`, `PutAccountSettingDefault`, `PutAttributes`, `SubmitAttachmentStateChanges`, `SubmitContainerStateChange`, `UpdateCapacityProvider`, `UpdateClusterSettings`

`UpdateContainerAgent`, `UpdateServicePrimaryTaskSet`, `UpdateTaskProtection`, `UpdateTaskSet`


</details>

<details><summary>Registered stubs (5)</summary>

`ListAccountSettings`, `ListServicesByNamespace`, `ListTaskDefinitionFamilies`, `PutAccountSetting`, `UpdateCluster`


</details>

<details><summary>Deliberately unsupported (0)</summary>

None.

</details>

<details><summary>Registered outside the pinned model (3)</summary>

`PollAssignments`, `ProvisionCapacity`, `ReportTaskGPU`


</details>

## elasticloadbalancingv2

Implements **33 of 46** modelled operations (71.7%).

<details><summary>Implemented (33)</summary>

`AddListenerCertificates`, `AddTags`, `CreateListener`, `CreateLoadBalancer`, `CreateRule`, `CreateTargetGroup`, `DeleteListener`, `DeleteLoadBalancer`

`DeleteRule`, `DeleteTargetGroup`, `DeregisterTargets`, `DescribeListenerCertificates`, `DescribeListeners`, `DescribeLoadBalancerAttributes`, `DescribeLoadBalancers`, `DescribeRules`

`DescribeSSLPolicies`, `DescribeTags`, `DescribeTargetGroupAttributes`, `DescribeTargetGroups`, `DescribeTargetHealth`, `ModifyListener`, `ModifyLoadBalancerAttributes`, `ModifyRule`

`ModifyTargetGroup`, `ModifyTargetGroupAttributes`, `RegisterTargets`, `RemoveListenerCertificates`, `RemoveTags`, `SetIpAddressType`, `SetRulePriorities`, `SetSecurityGroups`

`SetSubnets`


</details>

<details><summary>Missing from dispatch (13)</summary>

`AddTrustStoreRevocations`, `CreateTrustStore`, `DeleteSharedTrustStoreAssociation`, `DeleteTrustStore`, `DescribeAccountLimits`, `DescribeTrustStoreAssociations`, `DescribeTrustStoreRevocations`, `DescribeTrustStores`

`GetResourcePolicy`, `GetTrustStoreCaCertificatesBundle`, `GetTrustStoreRevocationContent`, `ModifyTrustStore`, `RemoveTrustStoreRevocations`


</details>

<details><summary>Registered stubs (0)</summary>

None.

</details>

<details><summary>Deliberately unsupported (0)</summary>

None.

</details>

<details><summary>Registered outside the pinned model (4)</summary>

`DescribeListenerAttributes`, `GetLBConfig`, `LBAgentHeartbeat`, `ModifyListenerAttributes`


</details>

## iam

Implements **75 of 159** modelled operations (47.2%).

<details><summary>Implemented (75)</summary>

`AddRoleToInstanceProfile`, `AddUserToGroup`, `AttachGroupPolicy`, `AttachRolePolicy`, `AttachUserPolicy`, `CreateAccessKey`, `CreateGroup`, `CreateInstanceProfile`

`CreateOpenIDConnectProvider`, `CreatePolicy`, `CreateRole`, `CreateUser`, `DeleteAccessKey`, `DeleteGroup`, `DeleteGroupPolicy`, `DeleteInstanceProfile`

`DeleteOpenIDConnectProvider`, `DeletePolicy`, `DeleteRole`, `DeleteRolePolicy`, `DeleteUser`, `DeleteUserPolicy`, `DetachGroupPolicy`, `DetachRolePolicy`

`DetachUserPolicy`, `GetAccountSummary`, `GetGroup`, `GetGroupPolicy`, `GetInstanceProfile`, `GetOpenIDConnectProvider`, `GetPolicy`, `GetPolicyVersion`

`GetRole`, `GetRolePolicy`, `GetUser`, `GetUserPolicy`, `ListAccessKeys`, `ListAttachedGroupPolicies`, `ListAttachedRolePolicies`, `ListAttachedUserPolicies`

`ListGroupPolicies`, `ListGroups`, `ListGroupsForUser`, `ListInstanceProfileTags`, `ListInstanceProfiles`, `ListInstanceProfilesForRole`, `ListOpenIDConnectProviderTags`, `ListOpenIDConnectProviders`

`ListPolicies`, `ListPolicyTags`, `ListPolicyVersions`, `ListRolePolicies`, `ListRoleTags`, `ListRoles`, `ListUserPolicies`, `ListUserTags`

`ListUsers`, `PutGroupPolicy`, `PutRolePolicy`, `PutUserPolicy`, `RemoveRoleFromInstanceProfile`, `RemoveUserFromGroup`, `TagInstanceProfile`, `TagOpenIDConnectProvider`

`TagPolicy`, `TagRole`, `TagUser`, `UntagInstanceProfile`, `UntagOpenIDConnectProvider`, `UntagPolicy`, `UntagRole`, `UntagUser`

`UpdateAccessKey`, `UpdateAssumeRolePolicy`, `UpdateRole`


</details>

<details><summary>Missing from dispatch (84)</summary>

`AddClientIDToOpenIDConnectProvider`, `ChangePassword`, `CreateAccountAlias`, `CreateLoginProfile`, `CreatePolicyVersion`, `CreateSAMLProvider`, `CreateServiceLinkedRole`, `CreateServiceSpecificCredential`

`CreateVirtualMFADevice`, `DeactivateMFADevice`, `DeleteAccountAlias`, `DeleteAccountPasswordPolicy`, `DeleteLoginProfile`, `DeletePolicyVersion`, `DeleteRolePermissionsBoundary`, `DeleteSAMLProvider`

`DeleteSSHPublicKey`, `DeleteServerCertificate`, `DeleteServiceLinkedRole`, `DeleteServiceSpecificCredential`, `DeleteSigningCertificate`, `DeleteUserPermissionsBoundary`, `DeleteVirtualMFADevice`, `EnableMFADevice`

`GenerateCredentialReport`, `GenerateOrganizationsAccessReport`, `GenerateServiceLastAccessedDetails`, `GetAccessKeyLastUsed`, `GetAccountAuthorizationDetails`, `GetAccountPasswordPolicy`, `GetContextKeysForCustomPolicy`, `GetContextKeysForPrincipalPolicy`

`GetCredentialReport`, `GetLoginProfile`, `GetMFADevice`, `GetOrganizationsAccessReport`, `GetSAMLProvider`, `GetSSHPublicKey`, `GetServerCertificate`, `GetServiceLastAccessedDetails`

`GetServiceLastAccessedDetailsWithEntities`, `GetServiceLinkedRoleDeletionStatus`, `ListAccountAliases`, `ListEntitiesForPolicy`, `ListMFADeviceTags`, `ListMFADevices`, `ListPoliciesGrantingServiceAccess`, `ListSAMLProviderTags`

`ListSAMLProviders`, `ListSSHPublicKeys`, `ListServerCertificateTags`, `ListServerCertificates`, `ListServiceSpecificCredentials`, `ListSigningCertificates`, `ListVirtualMFADevices`, `PutRolePermissionsBoundary`

`PutUserPermissionsBoundary`, `RemoveClientIDFromOpenIDConnectProvider`, `ResetServiceSpecificCredential`, `ResyncMFADevice`, `SetDefaultPolicyVersion`, `SetSecurityTokenServicePreferences`, `SimulateCustomPolicy`, `SimulatePrincipalPolicy`

`TagMFADevice`, `TagSAMLProvider`, `TagServerCertificate`, `UntagMFADevice`, `UntagSAMLProvider`, `UntagServerCertificate`, `UpdateAccountPasswordPolicy`, `UpdateGroup`

`UpdateLoginProfile`, `UpdateOpenIDConnectProviderThumbprint`, `UpdateRoleDescription`, `UpdateSAMLProvider`, `UpdateSSHPublicKey`, `UpdateServerCertificate`, `UpdateServiceSpecificCredential`, `UpdateSigningCertificate`

`UpdateUser`, `UploadSSHPublicKey`, `UploadServerCertificate`, `UploadSigningCertificate`


</details>

<details><summary>Registered stubs (0)</summary>

None.

</details>

<details><summary>Deliberately unsupported (0)</summary>

None.

</details>

<details><summary>Registered outside the pinned model (0)</summary>

None.

</details>

## s3

Operation coverage is not enumerable: Spinifex delegates the S3 REST surface to Predastore, which has no operation-name dispatch table to compare mechanically.

## sts

Implements **4 of 8** modelled operations (50.0%).

<details><summary>Implemented (4)</summary>

`AssumeRole`, `AssumeRoleWithWebIdentity`, `GetCallerIdentity`, `GetSessionToken`


</details>

<details><summary>Missing from dispatch (4)</summary>

`AssumeRoleWithSAML`, `DecodeAuthorizationMessage`, `GetAccessKeyInfo`, `GetFederationToken`


</details>

<details><summary>Registered stubs (0)</summary>

None.

</details>

<details><summary>Deliberately unsupported (0)</summary>

None.

</details>

<details><summary>Registered outside the pinned model (0)</summary>

None.

</details>
