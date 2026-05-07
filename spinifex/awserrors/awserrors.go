package awserrors

type ErrorMessage struct {
	HTTPCode int
	Message  string
}

var (
	ErrorAccountDisabled                                       = "AccountDisabled"
	ErrorActiveVpcPeeringConnectionPerVpcLimitExceeded         = "ActiveVpcPeeringConnectionPerVpcLimitExceeded"
	ErrorAddressLimitExceeded                                  = "AddressLimitExceeded"
	ErrorAsnConflict                                           = "AsnConflict"
	ErrorAttachmentLimitExceeded                               = "AttachmentLimitExceeded"
	ErrorAuthFailure                                           = "AuthFailure"
	ErrorBandwidthLimitExceeded                                = "BandwidthLimitExceeded"
	ErrorBlocked                                               = "Blocked"
	ErrorBootForVolumeTypeUnsupported                          = "BootForVolumeTypeUnsupported"
	ErrorBundlingInProgress                                    = "BundlingInProgress"
	ErrorCannotDelete                                          = "CannotDelete"
	ErrorCapacityBlockDescribeLimitExceeded                    = "CapacityBlockDescribeLimitExceeded"
	ErrorClientInvalidParameterValue                           = "ClientInvalidParameterValue"
	ErrorClientVpnAuthorizationRuleLimitExceeded               = "ClientVpnAuthorizationRuleLimitExceeded"
	ErrorClientVpnCertificateRevocationListLimitExceeded       = "ClientVpnCertificateRevocationListLimitExceeded"
	ErrorClientVpnEndpointAssociationExists                    = "ClientVpnEndpointAssociationExists"
	ErrorClientVpnEndpointLimitExceeded                        = "ClientVpnEndpointLimitExceeded"
	ErrorClientVpnRouteLimitExceeded                           = "ClientVpnRouteLimitExceeded"
	ErrorClientVpnTerminateConnectionsLimitExceeded            = "ClientVpnTerminateConnectionsLimitExceeded"
	ErrorConcurrentCreateImageNoRebootLimitExceeded            = "ConcurrentCreateImageNoRebootLimitExceeded"
	ErrorConcurrentSnapshotLimitExceeded                       = "ConcurrentSnapshotLimitExceeded"
	ErrorConcurrentTagAccess                                   = "ConcurrentTagAccess"
	ErrorCreditSpecificationUpdateInProgress                   = "CreditSpecificationUpdateInProgress"
	ErrorCustomerGatewayLimitExceeded                          = "CustomerGatewayLimitExceeded"
	ErrorCustomerKeyHasBeenRevoked                             = "CustomerKeyHasBeenRevoked"
	ErrorDeclarativePoliciesAccessDeniedException              = "DeclarativePoliciesAccessDeniedException"
	ErrorDeclarativePoliciesNotEnabledException                = "DeclarativePoliciesNotEnabledException"
	ErrorDefaultSubnetAlreadyExistsInAvailabilityZone          = "DefaultSubnetAlreadyExistsInAvailabilityZone"
	ErrorDefaultVpcAlreadyExists                               = "DefaultVpcAlreadyExists"
	ErrorDefaultVpcDoesNotExist                                = "DefaultVpcDoesNotExist"
	ErrorDeleteConversionTaskError                             = "DeleteConversionTaskError"
	ErrorDependencyViolation                                   = "DependencyViolation"
	ErrorDiskImageSizeTooLarge                                 = "DiskImageSizeTooLarge"
	ErrorDryRunOperation                                       = "DryRunOperation"
	ErrorDuplicateSubnetsInSameZone                            = "DuplicateSubnetsInSameZone"
	ErrorEncryptedVolumesNotSupported                          = "EncryptedVolumesNotSupported"
	ErrorExistingVpcEndpointConnections                        = "ExistingVpcEndpointConnections"
	ErrorFilterLimitExceeded                                   = "FilterLimitExceeded"
	ErrorFleetNotInModifiableState                             = "FleetNotInModifiableState"
	ErrorFlowLogAlreadyExists                                  = "FlowLogAlreadyExists"
	ErrorFlowLogsLimitExceeded                                 = "FlowLogsLimitExceeded"
	ErrorGatewayNotAttached                                    = "Gateway.NotAttached"
	ErrorHostAlreadyCoveredByReservation                       = "HostAlreadyCoveredByReservation"
	ErrorHostLimitExceeded                                     = "HostLimitExceeded"
	ErrorIdempotentInstanceTerminated                          = "IdempotentInstanceTerminated"
	ErrorIdempotentParameterMismatch                           = "IdempotentParameterMismatch"
	ErrorInaccessibleStorageLocation                           = "InaccessibleStorageLocation"
	ErrorInaccessibleStorageLocationException                  = "InaccessibleStorageLocationException"
	ErrorIncompatibleHostRequirements                          = "IncompatibleHostRequirements"
	ErrorIncompleteSignature                                   = "IncompleteSignature"
	ErrorIncorrectInstanceState                                = "IncorrectInstanceState"
	ErrorIncorrectModificationState                            = "IncorrectModificationState"
	ErrorIncorrectSpotRequestState                             = "IncorrectSpotRequestState"
	ErrorIncorrectState                                        = "IncorrectState"
	ErrorIncorrectStateException                               = "IncorrectStateException"
	ErrorInstanceCreditSpecificationNotSupported               = "InstanceCreditSpecification.NotSupported"
	ErrorInstanceEventStartTimeCannotChange                    = "InstanceEventStartTimeCannotChange"
	ErrorInstanceLimitExceeded                                 = "InstanceLimitExceeded"
	ErrorInstanceTpmEkPubNotFound                              = "InstanceTpmEkPubNotFound"
	ErrorInsufficientAddressCapacity                           = "InsufficientAddressCapacity"
	ErrorInsufficientCapacity                                  = "InsufficientCapacity"
	ErrorInsufficientCapacityOnHost                            = "InsufficientCapacityOnHost"
	ErrorInsufficientFreeAddressesInSubnet                     = "InsufficientFreeAddressesInSubnet"
	ErrorInsufficientHostCapacity                              = "InsufficientHostCapacity"
	ErrorInsufficientInstanceCapacity                          = "InsufficientInstanceCapacity"
	ErrorInsufficientReservedInstanceCapacity                  = "InsufficientReservedInstanceCapacity"
	ErrorInsufficientReservedInstancesCapacity                 = "InsufficientReservedInstancesCapacity"
	ErrorInsufficientVolumeCapacity                            = "InsufficientVolumeCapacity"
	ErrorInterfaceInUseByTrafficMirrorSession                  = "InterfaceInUseByTrafficMirrorSession"
	ErrorInterfaceInUseByTrafficMirrorTarget                   = "InterfaceInUseByTrafficMirrorTarget"
	ErrorInternalError                                         = "InternalError"
	ErrorInternalFailure                                       = "InternalFailure"
	ErrorInternetGatewayLimitExceeded                          = "InternetGatewayLimitExceeded"
	ErrorInvalidAMIAttributeItemValue                          = "InvalidAMIAttributeItemValue"
	ErrorInvalidAMIIDMalformed                                 = "InvalidAMIID.Malformed"
	ErrorInvalidAMIIDNotFound                                  = "InvalidAMIID.NotFound"
	ErrorInvalidAMIIDUnavailable                               = "InvalidAMIID.Unavailable"
	ErrorInvalidAMINameDuplicate                               = "InvalidAMIName.Duplicate"
	ErrorInvalidAMINameMalformed                               = "InvalidAMIName.Malformed"
	ErrorInvalidAction                                         = "InvalidAction"
	ErrorInvalidAddressLocked                                  = "InvalidAddress.Locked"
	ErrorInvalidAddressMalformed                               = "InvalidAddress.Malformed"
	ErrorInvalidAddressNotFound                                = "InvalidAddress.NotFound"
	ErrorInvalidAddressIDNotFound                              = "InvalidAddressID.NotFound"
	ErrorInvalidAffinity                                       = "InvalidAffinity"
	ErrorInvalidAllocationIDNotFound                           = "InvalidAllocationID.NotFound"
	ErrorInvalidAssociationIDNotFound                          = "InvalidAssociationID.NotFound"
	ErrorInvalidAttachmentNotFound                             = "InvalidAttachment.NotFound"
	ErrorInvalidAttachmentIDNotFound                           = "InvalidAttachmentID.NotFound"
	ErrorInvalidAutoPlacement                                  = "InvalidAutoPlacement"
	ErrorInvalidAvailabilityZone                               = "InvalidAvailabilityZone"
	ErrorInvalidBlockDeviceMapping                             = "InvalidBlockDeviceMapping"
	ErrorInvalidBundleIDNotFound                               = "InvalidBundleID.NotFound"
	ErrorInvalidCapacityBlockOfferingIdExpired                 = "InvalidCapacityBlockOfferingIdExpired"
	ErrorInvalidCapacityBlockOfferingIdMalformed               = "InvalidCapacityBlockOfferingIdMalformed"
	ErrorInvalidCapacityBlockOfferingIdNotFound                = "InvalidCapacityBlockOfferingIdNotFound"
	ErrorInvalidCapacityReservationIdMalformed                 = "InvalidCapacityReservationId.Malformed"
	ErrorInvalidCapacityReservationIdNotFound                  = "InvalidCapacityReservationId.NotFound"
	ErrorInvalidCapacityReservationStatePendingActivation      = "InvalidCapacityReservationState.PendingActivation"
	ErrorInvalidCarrierGatewayIDNotFound                       = "InvalidCarrierGatewayID.NotFound"
	ErrorInvalidCharacter                                      = "InvalidCharacter"
	ErrorInvalidCidrInUse                                      = "InvalidCidr.InUse"
	ErrorInvalidClientToken                                    = "InvalidClientToken"
	ErrorInvalidClientTokenId                                  = "InvalidClientTokenId"
	ErrorInvalidClientVpnActiveAssociationNotFound             = "InvalidClientVpnActiveAssociationNotFound"
	ErrorInvalidClientVpnAssociationIdNotFound                 = "InvalidClientVpnAssociationIdNotFound"
	ErrorInvalidClientVpnConnectionIdNotFound                  = "InvalidClientVpnConnection.IdNotFound"
	ErrorInvalidClientVpnConnectionUserNotFound                = "InvalidClientVpnConnection.UserNotFound"
	ErrorInvalidClientVpnDuplicateAssociationException         = "InvalidClientVpnDuplicateAssociationException"
	ErrorInvalidClientVpnDuplicateAuthorizationRule            = "InvalidClientVpnDuplicateAuthorizationRule"
	ErrorInvalidClientVpnDuplicateRoute                        = "InvalidClientVpnDuplicateRoute"
	ErrorInvalidClientVpnEndpointAuthorizationRuleNotFound     = "InvalidClientVpnEndpointAuthorizationRuleNotFound"
	ErrorInvalidClientVpnEndpointIdNotFound                    = "InvalidClientVpnEndpointId.NotFound"
	ErrorInvalidClientVpnRouteNotFound                         = "InvalidClientVpnRouteNotFound"
	ErrorInvalidClientVpnSubnetIdDifferentAccount              = "InvalidClientVpnSubnetId.DifferentAccount"
	ErrorInvalidClientVpnSubnetIdDuplicateAz                   = "InvalidClientVpnSubnetId.DuplicateAz"
	ErrorInvalidClientVpnSubnetIdNotFound                      = "InvalidClientVpnSubnetId.NotFound"
	ErrorInvalidClientVpnSubnetIdOverlappingCidr               = "InvalidClientVpnSubnetId.OverlappingCidr"
	ErrorInvalidConversionTaskId                               = "InvalidConversionTaskId"
	ErrorInvalidConversionTaskIdMalformed                      = "InvalidConversionTaskId.Malformed"
	ErrorInvalidCpuCredits                                     = "InvalidCpuCredits"
	ErrorInvalidCpuCreditsMalformed                            = "InvalidCpuCredits.Malformed"
	ErrorInvalidCustomerGatewayDuplicateIpAddress              = "InvalidCustomerGateway.DuplicateIpAddress"
	ErrorInvalidCustomerGatewayIDNotFound                      = "InvalidCustomerGatewayID.NotFound"
	ErrorInvalidCustomerGatewayIdMalformed                     = "InvalidCustomerGatewayId.Malformed"
	ErrorInvalidCustomerGatewayState                           = "InvalidCustomerGatewayState"
	ErrorInvalidDeclarativePoliciesReportIdMalformed           = "InvalidDeclarativePoliciesReportId.Malformed"
	ErrorInvalidDhcpOptionIDNotFound                           = "InvalidDhcpOptionID.NotFound"
	ErrorInvalidDhcpOptionsIDNotFound                          = "InvalidDhcpOptionsID.NotFound"
	ErrorInvalidDhcpOptionsIdMalformed                         = "InvalidDhcpOptionsId.Malformed"
	ErrorInvalidEgressOnlyInternetGatewayIdMalformed           = "InvalidEgressOnlyInternetGatewayId.Malformed"
	ErrorInvalidEgressOnlyInternetGatewayIdNotFound            = "InvalidEgressOnlyInternetGatewayId.NotFound"
	ErrorInvalidElasticGpuIDMalformed                          = "InvalidElasticGpuID.Malformed"
	ErrorInvalidElasticGpuIDNotFound                           = "InvalidElasticGpuID.NotFound"
	ErrorInvalidExportTaskIDMalformed                          = "InvalidExportTaskID.Malformed"
	ErrorInvalidExportTaskIDNotFound                           = "InvalidExportTaskID.NotFound"
	ErrorInvalidFilter                                         = "InvalidFilter"
	ErrorInvalidFlowLogIdNotFound                              = "InvalidFlowLogId.NotFound"
	ErrorInvalidFormat                                         = "InvalidFormat"
	ErrorInvalidFpgaImageIDMalformed                           = "InvalidFpgaImageID.Malformed"
	ErrorInvalidFpgaImageIDNotFound                            = "InvalidFpgaImageID.NotFound"
	ErrorInvalidGatewayIDNotFound                              = "InvalidGatewayID.NotFound"
	ErrorInvalidGroupDuplicate                                 = "InvalidGroup.Duplicate"
	ErrorInvalidGroupInUse                                     = "InvalidGroup.InUse"
	ErrorInvalidGroupNotFound                                  = "InvalidGroup.NotFound"
	ErrorInvalidGroupReserved                                  = "InvalidGroup.Reserved"
	ErrorInvalidGroupIdMalformed                               = "InvalidGroupId.Malformed"
	ErrorInvalidHostConfiguration                              = "InvalidHostConfiguration"
	ErrorInvalidHostIDMalformed                                = "InvalidHostID.Malformed"
	ErrorInvalidHostIDNotFound                                 = "InvalidHostID.NotFound"
	ErrorInvalidHostId                                         = "InvalidHostId"
	ErrorInvalidHostIdMalformed                                = "InvalidHostId.Malformed"
	ErrorInvalidHostIdNotFound                                 = "InvalidHostId.NotFound"
	ErrorInvalidHostReservationIdMalformed                     = "InvalidHostReservationId.Malformed"
	ErrorInvalidHostReservationOfferingIdMalformed             = "InvalidHostReservationOfferingId.Malformed"
	ErrorInvalidHostState                                      = "InvalidHostState"
	ErrorInvalidID                                             = "InvalidID"
	ErrorInvalidIPAddressInUse                                 = "InvalidIPAddress.InUse"
	ErrorInvalidIamInstanceProfileArnMalformed                 = "InvalidIamInstanceProfileArn.Malformed"
	ErrorInvalidInput                                          = "InvalidInput"
	ErrorInvalidInstanceAttributeValue                         = "InvalidInstanceAttributeValue"
	ErrorInvalidInstanceConnectEndpointIdMalformed             = "InvalidInstanceConnectEndpointId.Malformed"
	ErrorInvalidInstanceConnectEndpointIdNotFound              = "InvalidInstanceConnectEndpointId.NotFound"
	ErrorInvalidInstanceCreditSpecification                    = "InvalidInstanceCreditSpecification"
	ErrorInvalidInstanceCreditSpecificationDuplicateInstanceId = "InvalidInstanceCreditSpecification.DuplicateInstanceId"
	ErrorInvalidInstanceEventIDNotFound                        = "InvalidInstanceEventIDNotFound"
	ErrorInvalidInstanceEventStartTime                         = "InvalidInstanceEventStartTime"
	ErrorInvalidInstanceFamily                                 = "InvalidInstanceFamily"
	ErrorInvalidInstanceID                                     = "InvalidInstanceID"
	ErrorInvalidInstanceIDMalformed                            = "InvalidInstanceID.Malformed"
	ErrorInvalidInstanceIDNotFound                             = "InvalidInstanceID.NotFound"
	ErrorInvalidInstanceIDNotLinkable                          = "InvalidInstanceID.NotLinkable"
	ErrorInvalidInstanceState                                  = "InvalidInstanceState"
	ErrorInvalidInstanceType                                   = "InvalidInstanceType"
	ErrorInvalidInterfaceIpAddressLimitExceeded                = "InvalidInterface.IpAddressLimitExceeded"
	ErrorInvalidInternetGatewayIDNotFound                      = "InvalidInternetGatewayID.NotFound"
	ErrorInvalidInternetGatewayIdMalformed                     = "InvalidInternetGatewayId.Malformed"
	ErrorInvalidKernelIdMalformed                              = "InvalidKernelId.Malformed"
	ErrorInvalidKeyFormat                                      = "InvalidKey.Format"
	ErrorInvalidKeyPairDuplicate                               = "InvalidKeyPair.Duplicate"
	ErrorInvalidKeyPairFormat                                  = "InvalidKeyPair.Format"
	ErrorInvalidKeyPairNotFound                                = "InvalidKeyPair.NotFound"
	ErrorInvalidLaunchTargets                                  = "InvalidLaunchTargets"
	ErrorInvalidLaunchTemplateIdMalformed                      = "InvalidLaunchTemplateId.Malformed"
	ErrorInvalidLaunchTemplateIdNotFound                       = "InvalidLaunchTemplateId.NotFound"
	ErrorInvalidLaunchTemplateIdVersionNotFound                = "InvalidLaunchTemplateId.VersionNotFound"
	ErrorInvalidLaunchTemplateNameAlreadyExistsException       = "InvalidLaunchTemplateName.AlreadyExistsException"
	ErrorInvalidLaunchTemplateNameMalformedException           = "InvalidLaunchTemplateName.MalformedException"
	ErrorInvalidLaunchTemplateNameNotFoundException            = "InvalidLaunchTemplateName.NotFoundException"
	ErrorInvalidManifest                                       = "InvalidManifest"
	ErrorInvalidMaxResults                                     = "InvalidMaxResults"
	ErrorInvalidNatGatewayIDNotFound                           = "InvalidNatGatewayID.NotFound"
	ErrorInvalidNetworkAclEntryNotFound                        = "InvalidNetworkAclEntry.NotFound"
	ErrorInvalidNetworkAclIDNotFound                           = "InvalidNetworkAclID.NotFound"
	ErrorInvalidNetworkAclIdMalformed                          = "InvalidNetworkAclId.Malformed"
	ErrorInvalidNetworkInterfaceInUse                          = "InvalidNetworkInterface.InUse"
	ErrorInvalidNetworkInterfaceNotFound                       = "InvalidNetworkInterface.NotFound"
	ErrorInvalidNetworkInterfaceAttachmentIdMalformed          = "InvalidNetworkInterfaceAttachmentId.Malformed"
	ErrorInvalidNetworkInterfaceIDNotFound                     = "InvalidNetworkInterfaceID.NotFound"
	ErrorInvalidNetworkInterfaceIdMalformed                    = "InvalidNetworkInterfaceId.Malformed"
	ErrorInvalidNetworkLoadBalancerArnMalformed                = "InvalidNetworkLoadBalancerArn.Malformed"
	ErrorInvalidNetworkLoadBalancerArnNotFound                 = "InvalidNetworkLoadBalancerArn.NotFound"
	ErrorInvalidNextToken                                      = "InvalidNextToken"
	ErrorInvalidOptionConflict                                 = "InvalidOption.Conflict"
	ErrorInvalidPaginationToken                                = "InvalidPaginationToken"
	ErrorInvalidParameter                                      = "InvalidParameter"
	ErrorInvalidParameterCombination                           = "InvalidParameterCombination"
	ErrorInvalidParameterDependency                            = "InvalidParameterDependency"
	ErrorInvalidParameterValue                                 = "InvalidParameterValue"
	ErrorInvalidPermissionDuplicate                            = "InvalidPermission.Duplicate"
	ErrorInvalidPermissionMalformed                            = "InvalidPermission.Malformed"
	ErrorInvalidPermissionNotFound                             = "InvalidPermission.NotFound"
	ErrorInvalidPlacementGroupDuplicate                        = "InvalidPlacementGroup.Duplicate"
	ErrorInvalidPlacementGroupInUse                            = "InvalidPlacementGroup.InUse"
	ErrorInvalidPlacementGroupUnknown                          = "InvalidPlacementGroup.Unknown"
	ErrorInvalidPlacementGroupIdMalformed                      = "InvalidPlacementGroupId.Malformed"
	ErrorInvalidPolicyDocument                                 = "InvalidPolicyDocument"
	ErrorInvalidPrefixListIDNotFound                           = "InvalidPrefixListID.NotFound"
	ErrorInvalidPrefixListIdMalformed                          = "InvalidPrefixListId.Malformed"
	ErrorInvalidProductInfo                                    = "InvalidProductInfo"
	ErrorInvalidPurchaseTokenExpired                           = "InvalidPurchaseToken.Expired"
	ErrorInvalidPurchaseTokenMalformed                         = "InvalidPurchaseToken.Malformed"
	ErrorInvalidQuantity                                       = "InvalidQuantity"
	ErrorInvalidQueryParameter                                 = "InvalidQueryParameter"
	ErrorInvalidRamDiskIdMalformed                             = "InvalidRamDiskId.Malformed"
	ErrorInvalidRegion                                         = "InvalidRegion"
	ErrorInvalidRequest                                        = "InvalidRequest"
	ErrorInvalidReservationIDMalformed                         = "InvalidReservationID.Malformed"
	ErrorInvalidReservationIDNotFound                          = "InvalidReservationID.NotFound"
	ErrorInvalidReservedInstancesId                            = "InvalidReservedInstancesId"
	ErrorInvalidReservedInstancesOfferingId                    = "InvalidReservedInstancesOfferingId"
	ErrorInvalidResourceConfigurationArnMalformed              = "InvalidResourceConfigurationArn.Malformed"
	ErrorInvalidResourceConfigurationArnNotFound               = "InvalidResourceConfigurationArn.NotFound"
	ErrorInvalidResourceTypeUnknown                            = "InvalidResourceType.Unknown"
	ErrorInvalidRouteInvalidState                              = "InvalidRoute.InvalidState"
	ErrorInvalidRouteMalformed                                 = "InvalidRoute.Malformed"
	ErrorInvalidRouteNotFound                                  = "InvalidRoute.NotFound"
	ErrorInvalidRouteTableIDNotFound                           = "InvalidRouteTableID.NotFound"
	ErrorInvalidRouteTableIdMalformed                          = "InvalidRouteTableId.Malformed"
	ErrorInvalidScheduledInstance                              = "InvalidScheduledInstance"
	ErrorInvalidSecurityRequestHasExpired                      = "InvalidSecurity.RequestHasExpired"
	ErrorInvalidSecurityGroupIDNotFound                        = "InvalidSecurityGroupID.NotFound"
	ErrorInvalidSecurityGroupIdMalformed                       = "InvalidSecurityGroupId.Malformed"
	ErrorInvalidSecurityGroupRuleIdMalformed                   = "InvalidSecurityGroupRuleId.Malformed"
	ErrorInvalidSecurityGroupRuleIdNotFound                    = "InvalidSecurityGroupRuleId.NotFound"
	ErrorInvalidServiceName                                    = "InvalidServiceName"
	ErrorInvalidSnapshotInUse                                  = "InvalidSnapshot.InUse"
	ErrorInvalidSnapshotNotFound                               = "InvalidSnapshot.NotFound"
	ErrorInvalidSnapshotIDMalformed                            = "InvalidSnapshotID.Malformed"
	ErrorInvalidSpotDatafeedNotFound                           = "InvalidSpotDatafeed.NotFound"
	ErrorInvalidSpotFleetRequestConfig                         = "InvalidSpotFleetRequestConfig"
	ErrorInvalidSpotFleetRequestIdMalformed                    = "InvalidSpotFleetRequestId.Malformed"
	ErrorInvalidSpotFleetRequestIdNotFound                     = "InvalidSpotFleetRequestId.NotFound"
	ErrorInvalidSpotInstanceRequestIDMalformed                 = "InvalidSpotInstanceRequestID.Malformed"
	ErrorInvalidSpotInstanceRequestIDNotFound                  = "InvalidSpotInstanceRequestID.NotFound"
	ErrorInvalidState                                          = "InvalidState"
	ErrorInvalidStateTransition                                = "InvalidStateTransition"
	ErrorInvalidSubnet                                         = "InvalidSubnet"
	ErrorInvalidSubnetConflict                                 = "InvalidSubnet.Conflict"
	ErrorInvalidSubnetRange                                    = "InvalidSubnet.Range"
	ErrorInvalidSubnetIDMalformed                              = "InvalidSubnetID.Malformed"
	ErrorInvalidSubnetIDNotFound                               = "InvalidSubnetID.NotFound"
	ErrorInvalidTagKeyMalformed                                = "InvalidTagKey.Malformed"
	ErrorInvalidTargetArnUnknown                               = "InvalidTargetArn.Unknown"
	ErrorInvalidTargetException                                = "InvalidTargetException"
	ErrorInvalidTenancy                                        = "InvalidTenancy"
	ErrorInvalidTime                                           = "InvalidTime"
	ErrorInvalidTrafficMirrorFilterNotFound                    = "InvalidTrafficMirrorFilterNotFound"
	ErrorInvalidTrafficMirrorFilterRuleNotFound                = "InvalidTrafficMirrorFilterRuleNotFound"
	ErrorInvalidTrafficMirrorSessionNotFound                   = "InvalidTrafficMirrorSessionNotFound"
	ErrorInvalidTrafficMirrorTargetNoFound                     = "InvalidTrafficMirrorTargetNoFound"
	ErrorInvalidUserIDMalformed                                = "InvalidUserID.Malformed"
	ErrorInvalidVolumeNotFound                                 = "InvalidVolume.NotFound"
	ErrorInvalidVolumeZoneMismatch                             = "InvalidVolume.ZoneMismatch"
	ErrorInvalidVolumeIDDuplicate                              = "InvalidVolumeID.Duplicate"
	ErrorInvalidVolumeIDMalformed                              = "InvalidVolumeID.Malformed"
	ErrorInvalidVolumeIDZoneMismatch                           = "InvalidVolumeID.ZoneMismatch"
	ErrorInvalidVpcEndpointNotFound                            = "InvalidVpcEndpoint.NotFound"
	ErrorInvalidVpcEndpointIdMalformed                         = "InvalidVpcEndpointId.Malformed"
	ErrorInvalidVpcEndpointIdNotFound                          = "InvalidVpcEndpointId.NotFound"
	ErrorInvalidVpcEndpointServiceNotFound                     = "InvalidVpcEndpointService.NotFound"
	ErrorInvalidVpcEndpointServiceIdNotFound                   = "InvalidVpcEndpointServiceId.NotFound"
	ErrorInvalidVpcEndpointServiceIdIdMalformed                = "InvalidVpcEndpointServiceIdId.Malformed"
	ErrorInvalidVpcEndpointType                                = "InvalidVpcEndpointType"
	ErrorInvalidVpcIDMalformed                                 = "InvalidVpcID.Malformed"
	ErrorInvalidVpcIDNotFound                                  = "InvalidVpcID.NotFound"
	ErrorInvalidVpcPeeringConnectionIDNotFound                 = "InvalidVpcPeeringConnectionID.NotFound"
	ErrorInvalidVpcPeeringConnectionIdMalformed                = "InvalidVpcPeeringConnectionId.Malformed"
	ErrorInvalidVpcPeeringConnectionStateDnsHostnamesDisabled  = "InvalidVpcPeeringConnectionState.DnsHostnamesDisabled"
	ErrorInvalidVpcRange                                       = "InvalidVpcRange"
	ErrorInvalidVpcState                                       = "InvalidVpcState"
	ErrorInvalidVpnConnectionInvalidState                      = "InvalidVpnConnection.InvalidState"
	ErrorInvalidVpnConnectionInvalidType                       = "InvalidVpnConnection.InvalidType"
	ErrorInvalidVpnConnectionID                                = "InvalidVpnConnectionID"
	ErrorInvalidVpnConnectionIDNotFound                        = "InvalidVpnConnectionID.NotFound"
	ErrorInvalidVpnGatewayAttachmentNotFound                   = "InvalidVpnGatewayAttachment.NotFound"
	ErrorInvalidVpnGatewayIDNotFound                           = "InvalidVpnGatewayID.NotFound"
	ErrorInvalidVpnGatewayState                                = "InvalidVpnGatewayState"
	ErrorInvalidZoneNotFound                                   = "InvalidZone.NotFound"
	ErrorKeyPairLimitExceeded                                  = "KeyPairLimitExceeded"
	ErrorLegacySecurityGroup                                   = "LegacySecurityGroup"
	ErrorLimitPriceExceeded                                    = "LimitPriceExceeded"
	ErrorLogDestinationNotFound                                = "LogDestinationNotFound"
	ErrorLogDestinationPermissionIssue                         = "LogDestinationPermissionIssue"
	ErrorMalformedQueryString                                  = "MalformedQueryString"
	ErrorMaxConfigLimitExceededException                       = "MaxConfigLimitExceededException"
	ErrorMaxIOPSLimitExceeded                                  = "MaxIOPSLimitExceeded"
	ErrorMaxScheduledInstanceCapacityExceeded                  = "MaxScheduledInstanceCapacityExceeded"
	ErrorMaxSpotFleetRequestCountExceeded                      = "MaxSpotFleetRequestCountExceeded"
	ErrorMaxSpotInstanceCountExceeded                          = "MaxSpotInstanceCountExceeded"
	ErrorMaxTemplateLimitExceeded                              = "MaxTemplateLimitExceeded"
	ErrorMaxTemplateVersionLimitExceeded                       = "MaxTemplateVersionLimitExceeded"
	ErrorMissingAction                                         = "MissingAction"
	ErrorMissingAuthenticationToken                            = "MissingAuthenticationToken"
	ErrorMissingInput                                          = "MissingInput"
	ErrorMissingParameter                                      = "MissingParameter"
	ErrorNatGatewayLimitExceeded                               = "NatGatewayLimitExceeded"
	ErrorNatGatewayMalformed                                   = "NatGatewayMalformed"
	ErrorNatGatewayNotFound                                    = "NatGatewayNotFound"
	ErrorNetworkAclEntryAlreadyExists                          = "NetworkAclEntryAlreadyExists"
	ErrorNetworkAclEntryLimitExceeded                          = "NetworkAclEntryLimitExceeded"
	ErrorNetworkAclLimitExceeded                               = "NetworkAclLimitExceeded"
	ErrorNetworkInterfaceLimitExceeded                         = "NetworkInterfaceLimitExceeded"
	ErrorNetworkInterfaceNotSupported                          = "NetworkInterfaceNotSupported"
	ErrorNetworkLoadBalancerNotFoundException                  = "NetworkLoadBalancerNotFoundException"
	ErrorNlbInUseByTrafficMirrorTargetException                = "NlbInUseByTrafficMirrorTargetException"
	ErrorNoSuchVersion                                         = "NoSuchVersion"
	ErrorNonEBSInstance                                        = "NonEBSInstance"
	ErrorNotExportable                                         = "NotExportable"
	ErrorOperationNotPermitted                                 = "OperationNotPermitted"
	ErrorOptInRequired                                         = "OptInRequired"
	ErrorOutstandingVpcPeeringConnectionLimitExceeded          = "OutstandingVpcPeeringConnectionLimitExceeded"
	ErrorPendingSnapshotLimitExceeded                          = "PendingSnapshotLimitExceeded"
	ErrorPendingVerification                                   = "PendingVerification"
	ErrorPendingVpcPeeringConnectionLimitExceeded              = "PendingVpcPeeringConnectionLimitExceeded"
	ErrorPlacementGroupLimitExceeded                           = "PlacementGroupLimitExceeded"
	ErrorPrivateIpAddressLimitExceeded                         = "PrivateIpAddressLimitExceeded"
	ErrorRequestEntityTooLarge                                 = "RequestEntityTooLarge"
	ErrorRequestExpired                                        = "RequestExpired"
	ErrorRequestLimitExceeded                                  = "RequestLimitExceeded"
	ErrorRequestResourceCountExceeded                          = "RequestResourceCountExceeded"
	ErrorReservationCapacityExceeded                           = "ReservationCapacityExceeded"
	ErrorReservedInstancesCountExceeded                        = "ReservedInstancesCountExceeded"
	ErrorReservedInstancesLimitExceeded                        = "ReservedInstancesLimitExceeded"
	ErrorReservedInstancesUnavailable                          = "ReservedInstancesUnavailable"
	ErrorResourceAlreadyAssigned                               = "Resource.AlreadyAssigned"
	ErrorResourceAlreadyAssociated                             = "Resource.AlreadyAssociated"
	ErrorResourceCountExceeded                                 = "ResourceCountExceeded"
	ErrorResourceCountLimitExceeded                            = "ResourceCountLimitExceeded"
	ErrorResourceLimitExceeded                                 = "ResourceLimitExceeded"
	ErrorRetryableError                                        = "RetryableError"
	ErrorRouteAlreadyExists                                    = "RouteAlreadyExists"
	ErrorRouteLimitExceeded                                    = "RouteLimitExceeded"
	ErrorRouteTableLimitExceeded                               = "RouteTableLimitExceeded"
	ErrorRulesPerSecurityGroupLimitExceeded                    = "RulesPerSecurityGroupLimitExceeded"
	ErrorScheduledInstanceLimitExceeded                        = "ScheduledInstanceLimitExceeded"
	ErrorScheduledInstanceParameterMismatch                    = "ScheduledInstanceParameterMismatch"
	ErrorScheduledInstanceSlotNotOpen                          = "ScheduledInstanceSlotNotOpen"
	ErrorScheduledInstanceSlotUnavailable                      = "ScheduledInstanceSlotUnavailable"
	ErrorSecurityGroupLimitExceeded                            = "SecurityGroupLimitExceeded"
	ErrorSecurityGroupsPerInstanceLimitExceeded                = "SecurityGroupsPerInstanceLimitExceeded"
	ErrorSecurityGroupsPerInterfaceLimitExceeded               = "SecurityGroupsPerInterfaceLimitExceeded"
	ErrorSerialConsoleSessionUnavailable                       = "SerialConsoleSessionUnavailable"
	ErrorServerInternal                                        = "ServerInternal"
	ErrorServiceUnavailable                                    = "ServiceUnavailable"
	ErrorSignatureDoesNotMatch                                 = "SignatureDoesNotMatch"
	ErrorSnapshotCopyUnsupportedInterRegion                    = "SnapshotCopyUnsupported.InterRegion"
	ErrorSnapshotCreationPerVolumeRateExceeded                 = "SnapshotCreationPerVolumeRateExceeded"
	ErrorSnapshotLimitExceeded                                 = "SnapshotLimitExceeded"
	ErrorSpotMaxPriceTooLow                                    = "SpotMaxPriceTooLow"
	ErrorSubnetLimitExceeded                                   = "SubnetLimitExceeded"
	ErrorTagLimitExceeded                                      = "TagLimitExceeded"
	ErrorTagPolicyViolation                                    = "TagPolicyViolation"
	ErrorThrottling                                            = "Throttling"
	ErrorTargetCapacityLimitExceededException                  = "TargetCapacityLimitExceededException"
	ErrorTrafficMirrorFilterInUse                              = "TrafficMirrorFilterInUse"
	ErrorTrafficMirrorFilterLimitExceeded                      = "TrafficMirrorFilterLimitExceeded"
	ErrorTrafficMirrorFilterRuleAlreadyExists                  = "TrafficMirrorFilterRuleAlreadyExists"
	ErrorTrafficMirrorFilterRuleLimitExceeded                  = "TrafficMirrorFilterRuleLimitExceeded"
	ErrorTrafficMirrorSessionLimitExceeded                     = "TrafficMirrorSessionLimitExceeded"
	ErrorTrafficMirrorSessionsPerInterfaceLimitExceeded        = "TrafficMirrorSessionsPerInterfaceLimitExceeded"
	ErrorTrafficMirrorSessionsPerTargetLimitExceeded           = "TrafficMirrorSessionsPerTargetLimitExceeded"
	ErrorTrafficMirrorSourcesPerTargetLimitExceeded            = "TrafficMirrorSourcesPerTargetLimitExceeded"
	ErrorTrafficMirrorTargetInUseException                     = "TrafficMirrorTargetInUseException"
	ErrorTrafficMirrorTargetLimitExceeded                      = "TrafficMirrorTargetLimitExceeded"
	ErrorUnauthorizedOperation                                 = "UnauthorizedOperation"
	ErrorUnavailable                                           = "Unavailable"
	ErrorUnavailableHostRequirements                           = "UnavailableHostRequirements"
	ErrorUnfulfillableCapacity                                 = "UnfulfillableCapacity"
	ErrorUnknownParameter                                      = "UnknownParameter"
	ErrorUnknownPrincipalTypeUnsupported                       = "UnknownPrincipalType.Unsupported"
	ErrorUnknownVolumeType                                     = "UnknownVolumeType"
	ErrorUnsupported                                           = "Unsupported"
	ErrorUnsupportedException                                  = "UnsupportedException"
	ErrorUnsupportedHibernationConfiguration                   = "UnsupportedHibernationConfiguration"
	ErrorUnsupportedHostConfiguration                          = "UnsupportedHostConfiguration"
	ErrorUnsupportedInstanceAttribute                          = "UnsupportedInstanceAttribute"
	ErrorUnsupportedInstanceTypeOnHost                         = "UnsupportedInstanceTypeOnHost"
	ErrorUnsupportedOperation                                  = "UnsupportedOperation"
	ErrorUnsupportedProtocol                                   = "UnsupportedProtocol"
	ErrorUnsupportedTenancy                                    = "UnsupportedTenancy"
	ErrorUpdateLimitExceeded                                   = "UpdateLimitExceeded"
	ErrorVPCIdNotSpecified                                     = "VPCIdNotSpecified"
	ErrorVPCResourceNotSpecified                               = "VPCResourceNotSpecified"
	ErrorValidationError                                       = "ValidationError"
	ErrorVcpuLimitExceeded                                     = "VcpuLimitExceeded"
	ErrorVolumeIOPSLimit                                       = "VolumeIOPSLimit"
	ErrorVolumeInUse                                           = "VolumeInUse"
	ErrorVolumeLimitExceeded                                   = "VolumeLimitExceeded"
	ErrorVolumeModificationSizeLimitExceeded                   = "VolumeModificationSizeLimitExceeded"
	ErrorVolumeTypeNotAvailableInZone                          = "VolumeTypeNotAvailableInZone"
	ErrorVpcEndpointLimitExceeded                              = "VpcEndpointLimitExceeded"
	ErrorVpcLimitExceeded                                      = "VpcLimitExceeded"
	ErrorVpcPeeringConnectionAlreadyExists                     = "VpcPeeringConnectionAlreadyExists"
	ErrorVpcPeeringConnectionsPerVpcLimitExceeded              = "VpcPeeringConnectionsPerVpcLimitExceeded"
	ErrorVpnConnectionLimitExceeded                            = "VpnConnectionLimitExceeded"
	ErrorVpnGatewayAttachmentLimitExceeded                     = "VpnGatewayAttachmentLimitExceeded"
	ErrorVpnGatewayLimitExceeded                               = "VpnGatewayLimitExceeded"
	ErrorZonesMismatched                                       = "ZonesMismatched"

	// IAM-specific error codes
	ErrorIAMNoSuchEntity            = "NoSuchEntity"
	ErrorIAMEntityAlreadyExists     = "EntityAlreadyExists"
	ErrorIAMDeleteConflict          = "DeleteConflict"
	ErrorIAMLimitExceeded           = "LimitExceeded"
	ErrorIAMInvalidInput            = ErrorInvalidInput // alias for IAM usage
	ErrorIAMMalformedPolicyDocument = "MalformedPolicyDocument"
	ErrorAccessDenied               = "AccessDenied"

	// ELBv2-specific error codes
	ErrorELBv2LoadBalancerNotFound         = "LoadBalancerNotFound"
	ErrorELBv2TargetGroupNotFound          = "TargetGroupNotFound"
	ErrorELBv2ListenerNotFound             = "ListenerNotFound"
	ErrorELBv2DuplicateLoadBalancer        = "DuplicateLoadBalancerName"
	ErrorELBv2DuplicateTargetGroup         = "DuplicateTargetGroupName"
	ErrorELBv2DuplicateListener            = "DuplicateListener"
	ErrorELBv2TooManyLoadBalancers         = "TooManyLoadBalancers"
	ErrorELBv2TooManyTargetGroups          = "TooManyTargetGroups"
	ErrorELBv2TooManyListeners             = "TooManyListeners"
	ErrorELBv2TooManyTargets               = "TooManyRegistrationsForTargetId"
	ErrorELBv2InvalidTarget                = "InvalidTarget"
	ErrorELBv2TargetGroupInUse             = "ResourceInUse"
	ErrorELBv2InvalidSecurityGroup         = "InvalidSecurityGroup"
	ErrorELBv2InvalidScheme                = "InvalidScheme"
	ErrorELBv2SubnetNotFound               = "SubnetNotFound"
	ErrorELBv2AvailabilityZoneNotSupported = "AvailabilityZoneNotSupported"
	ErrorELBv2InvalidConfigurationRequest  = "InvalidConfigurationRequest"
)

// ValidErrorCode returns the error code if it exists in ErrorLookup,
// otherwise returns ErrorServerInternal. Use this to sanitize error strings
// before sending them to clients.
func ValidErrorCode(code string) string {
	if _, ok := ErrorLookup[code]; ok {
		return code
	}
	return ErrorServerInternal
}

var ErrorLookup = map[string]ErrorMessage{
	ErrorAccountDisabled: {HTTPCode: 400, Message: "The functionality you have requested has been administratively disabled for this account."},
	ErrorActiveVpcPeeringConnectionPerVpcLimitExceeded:         {HTTPCode: 400, Message: "You've reached the limit on the number of active VPC peering connections you can have for the specified VPC."},
	ErrorAddressLimitExceeded:                                  {HTTPCode: 400, Message: "You've reached the limit on the number of Elastic IP addresses that you can allocate. For more information, see Elastic IP address limit."},
	ErrorAsnConflict:                                           {HTTPCode: 400, Message: "The Autonomous System Numbers (ASNs) of the specified customer gateway and the specified virtual private gateway are the same."},
	ErrorAttachmentLimitExceeded:                               {HTTPCode: 400, Message: "You've reached the limit on the number of Amazon EBS volumes or network interfaces that can be attached to a single instance. The number of Amazon EBS volumes that you can attach to an instance depends on the instance type. For more information, see Amazon EBS volume limits for Amazon EC2 instances in the Amazon EC2 User Guide."},
	ErrorAuthFailure:                                           {HTTPCode: 403, Message: "The provided credentials could not be validated. You might not be authorized to carry out the request; for example, trying to associate an Elastic IP address that is not yours, or trying to use an AMI for which you do not have permissions. Ensure that your account is authorized to use Amazon EC2, that your credit card details are correct, and that you are using the correct credentials."},
	ErrorBandwidthLimitExceeded:                                {HTTPCode: 500, Message: "You've reached the limit on the network bandwidth that is available to an Amazon EC2 instance. For more information, see Amazon EC2 instance network bandwidth."},
	ErrorBlocked:                                               {HTTPCode: 403, Message: "Your account is currently blocked. Contact Support if you have questions."},
	ErrorBootForVolumeTypeUnsupported:                          {HTTPCode: 400, Message: "The specified volume type cannot be used as a boot volume. For more information, see Amazon EBS volume types."},
	ErrorBundlingInProgress:                                    {HTTPCode: 400, Message: "The specified instance already has a bundling task in progress."},
	ErrorCannotDelete:                                          {HTTPCode: 400, Message: "You cannot delete the 'default' security group in your VPC, but you can change its rules. For more information, see Amazon EC2 security groups."},
	ErrorCapacityBlockDescribeLimitExceeded:                    {HTTPCode: 400, Message: "You've reached the limit for this account. The returned message provides details."},
	ErrorClientInvalidParameterValue:                           {HTTPCode: 400, Message: "A parameter specified in a request is not valid, is unsupported, or cannot be used. The returned message provides an explanation of the error value. For example, if you are launching an instance, you can't specify a security group and subnet that are in different VPCs."},
	ErrorClientVpnAuthorizationRuleLimitExceeded:               {HTTPCode: 400, Message: "You've reached the limit on the number of authorization rules that can be added to a single Client VPN endpoint."},
	ErrorClientVpnCertificateRevocationListLimitExceeded:       {HTTPCode: 400, Message: "You've reached the limit on the number of client certificate revocation lists that can be added to a single Client VPN endpoint."},
	ErrorClientVpnEndpointAssociationExists:                    {HTTPCode: 409, Message: "The specified target network is already associated with the Client VPN endpoint."},
	ErrorClientVpnEndpointLimitExceeded:                        {HTTPCode: 400, Message: "You've reached the limit on the number of Client VPN endpoints that you can create."},
	ErrorClientVpnRouteLimitExceeded:                           {HTTPCode: 400, Message: "You've reached the limit on the number of routes that can be added to a single Client VPN endpoint."},
	ErrorClientVpnTerminateConnectionsLimitExceeded:            {HTTPCode: 400, Message: "The number of client connections you're attempting to terminate exceeds the limit."},
	ErrorConcurrentCreateImageNoRebootLimitExceeded:            {HTTPCode: 400, Message: "The maximum number of concurrent CreateImage requests for the instance has been reached. Wait for the current CreateImage requests to complete, and then retry your request."},
	ErrorConcurrentSnapshotLimitExceeded:                       {HTTPCode: 409, Message: "You've reached the limit on the number of concurrent snapshots you can create on the specified volume. Wait until the 'pending' requests have completed, and check that you do not have snapshots that are in an incomplete state, such as 'error', which count against your concurrent snapshot limit."},
	ErrorConcurrentTagAccess:                                   {HTTPCode: 400, Message: "You can't run simultaneous commands to modify a tag for a specific resource. Allow sufficient wait time for the previous request to complete, then retry your request."},
	ErrorCreditSpecificationUpdateInProgress:                   {HTTPCode: 400, Message: "The default credit specification for the instance family is currently being updated. It takes about five minutes to complete. For more information, see Set the default credit specification for the account."},
	ErrorCustomerGatewayLimitExceeded:                          {HTTPCode: 400, Message: "You've reached the limit on the number of customer gateways you can create for the AWS Region. For more information, see Amazon VPC quotas."},
	ErrorCustomerKeyHasBeenRevoked:                             {HTTPCode: 400, Message: "The KMS key cannot be accessed. For more information, see Amazon EBS encryption."},
	ErrorDeclarativePoliciesAccessDeniedException:              {HTTPCode: 404, Message: "You do not have sufficient access to perform this action, or the specified TargetId does not exist, or the specified TargetId is not in your organization. To generate an account status report for declarative policies, the caller must be the management account or a delegated administrator for the organization and the specified TargetId must belong to your organization."},
	ErrorDeclarativePoliciesNotEnabledException:                {HTTPCode: 400, Message: "Trusted access is not enabled. Trusted access must be enabled for the service for which the declarative policy will enforce a baseline configuration. The API uses the following service principal to identify the EC2 service: ec2.amazonaws.com. For more information on how to enable trusted access with the AWS CLI and AWS SDKs, see Using Organizations with other AWS services."},
	ErrorDefaultSubnetAlreadyExistsInAvailabilityZone:          {HTTPCode: 409, Message: "A default subnet already exists in the specified Availability Zone. You can have only one default subnet per Availability Zone."},
	ErrorDefaultVpcAlreadyExists:                               {HTTPCode: 409, Message: "A default VPC already exists in the AWS Region. You can only have one default VPC per Region."},
	ErrorDefaultVpcDoesNotExist:                                {HTTPCode: 400, Message: "There is no default VPC in which to carry out the request. If you've deleted your default VPC, you can create a new one. For more information, see Create a default VPC."},
	ErrorDeleteConversionTaskError:                             {HTTPCode: 400, Message: "The conversion task cannot be canceled."},
	ErrorDependencyViolation:                                   {HTTPCode: 400, Message: "The specified object has dependent resources. A number of resources in a VPC may have dependent resources, which prevent you from deleting or detaching them. Remove the dependencies first, then retry your request. For example, this error occurs if you try to delete a security group in a VPC that is in use by another security group."},
	ErrorDiskImageSizeTooLarge:                                 {HTTPCode: 400, Message: "The disk image exceeds the allowed limit (for instance or volume import)."},
	ErrorDryRunOperation:                                       {HTTPCode: 412, Message: "The user has the required permissions, so the request would have succeeded, but the DryRun parameter was used."},
	ErrorDuplicateSubnetsInSameZone:                            {HTTPCode: 400, Message: "For an interface VPC endpoint, you can specify only one subnet per Availability Zone."},
	ErrorEncryptedVolumesNotSupported:                          {HTTPCode: 400, Message: "Encrypted Amazon EBS volumes may only be attached to instances that support Amazon EBS encryption. For more information, see Amazon EBS encryption."},
	ErrorExistingVpcEndpointConnections:                        {HTTPCode: 400, Message: "You cannot delete a VPC endpoint service configuration or change the load balancers for the endpoint service if there are endpoints attached to the service."},
	ErrorFilterLimitExceeded:                                   {HTTPCode: 400, Message: "The request uses too many filters or too many filter values."},
	ErrorFleetNotInModifiableState:                             {HTTPCode: 400, Message: "The Spot Fleet request must be in the active state in order to modify it. For more information, see Spot Fleet request types."},
	ErrorFlowLogAlreadyExists:                                  {HTTPCode: 409, Message: "A flow log with the specified configuration already exists."},
	ErrorFlowLogsLimitExceeded:                                 {HTTPCode: 400, Message: "You've reached the limit on the number of flow logs you can create. For more information, see Amazon VPC quotas."},
	ErrorGatewayNotAttached:                                    {HTTPCode: 400, Message: "An internet gateway is not attached to a VPC. If you are trying to detach an internet gateway, ensure that you specify the correct VPC. If you are trying to associate an Elastic IP address with a network interface or an instance, ensure that an internet gateway is attached to the relevant VPC."},
	ErrorHostAlreadyCoveredByReservation:                       {HTTPCode: 400, Message: "The specified Dedicated Host is already covered by a reservation."},
	ErrorHostLimitExceeded:                                     {HTTPCode: 400, Message: "You've reached the limit on the number of Dedicated Hosts that you can allocate. For more information, see Dedicated Hosts."},
	ErrorIdempotentInstanceTerminated:                          {HTTPCode: 400, Message: "The request to launch an instance uses the same client token as a previous request for which the instance has been terminated."},
	ErrorIdempotentParameterMismatch:                           {HTTPCode: 400, Message: "The request uses the same client token as a previous, but non-identical request. Do not reuse a client token with different requests, unless the requests are identical."},
	ErrorInaccessibleStorageLocation:                           {HTTPCode: 400, Message: "The specified Amazon S3 URL cannot be accessed. Check the access permissions for the URL."},
	ErrorInaccessibleStorageLocationException:                  {HTTPCode: 400, Message: "The specified Amazon S3 bucket can't be accessed. An S3 bucket must be available before generating an account status report for declarative policies (you can create a new one or use an existing one), you must own the bucket, it must be in the same Region in which the request was made, and it must have an appropriate bucket policy. For a sample S3 policy, see Sample Amazon S3 policy under Examples."},
	ErrorIncompatibleHostRequirements:                          {HTTPCode: 400, Message: "There are no available or compatible Dedicated Hosts available on which to launch or start the instance."},
	ErrorIncompleteSignature:                                   {HTTPCode: 400, Message: "The request signature does not conform to AWS standards."},
	ErrorIncorrectInstanceState:                                {HTTPCode: 409, Message: "The instance is in an incorrect state for the requested action. For example, some instance attributes, such as user data, can only be modified if the instance is in a 'stopped' state. If you are associating an Elastic IP address with a network interface, ensure that the instance that the interface is attached to is not in the 'pending' state."},
	ErrorIncorrectModificationState:                            {HTTPCode: 400, Message: "A new modification action on an EBS Elastic Volume cannot occur because the volume is currently being modified."},
	ErrorIncorrectSpotRequestState:                             {HTTPCode: 400, Message: "The Spot Instance request is in an incorrect state for the request. Spot request status information can help you track your Amazon EC2 Spot Instance requests. For more information, see Spot request status."},
	ErrorIncorrectState:                                        {HTTPCode: 400, Message: "The resource is in an incorrect state for the request. This error can occur if you are trying to attach a volume that is still being created or detach a volume that is not in the 'available' state. Verify that the volume is in the 'available' state. If you are creating a snapshot, ensure that the previous request to create a snapshot on the same volume has completed. If you are deleting a virtual private gateway, ensure that it's detached from the VPC."},
	ErrorIncorrectStateException:                               {HTTPCode: 400, Message: "The resource is in an incorrect state for the request. This error can occur if you're trying to cancel the generation of an account status report for declarative policies that already has the complete, cancelled, or error status. Cancelation is only possible for reports with a running status. This error can also occur when you're trying to get a summary for a report that has not yet reached the completed status. This error can also occur if you start report generation while another report is being generated. Only one report per organization can be generated at a time. For more information, see Generating the account status report for declarative policies."},
	ErrorInstanceCreditSpecificationNotSupported:               {HTTPCode: 400, Message: "The specified instance does not use CPU credits for CPU usage; only T2 instances use CPU credits for CPU usage."},
	ErrorInstanceEventStartTimeCannotChange:                    {HTTPCode: 400, Message: "The specified scheduled event start time does not meet the requirements for rescheduling a scheduled event. For more information, see Limitations."},
	ErrorInstanceLimitExceeded:                                 {HTTPCode: 400, Message: "You've reached the limit on the number of instances you can run concurrently. This error can occur if you are launching an instance or if you are creating a Capacity Reservation. Capacity Reservations count towards your On-Demand Instance limits. If your request fails due to limit constraints, increase your On-Demand Instance limit for the required instance type and try again. For more information, see EC2 On-Demand instance limits."},
	ErrorInstanceTpmEkPubNotFound:                              {HTTPCode: 400, Message: "The public Trusted Platform Module (TPM) Endorsement Key (EK) cannot be found."},
	ErrorInsufficientAddressCapacity:                           {HTTPCode: 503, Message: "Not enough available addresses to satisfy your minimum request. Reduce the number of addresses you are requesting or wait for additional capacity to become available."},
	ErrorInsufficientCapacity:                                  {HTTPCode: 503, Message: "There is not enough capacity to fulfill your import instance request. You can wait for additional capacity to become available."},
	ErrorInsufficientCapacityOnHost:                            {HTTPCode: 400, Message: "There is not enough capacity on the Dedicated Host to launch or start the instance."},
	ErrorInsufficientFreeAddressesInSubnet:                     {HTTPCode: 503, Message: "The specified subnet does not contain enough free private IP addresses to fulfill your request. Use the DescribeSubnets request to view how many IP addresses are available (unused) in your subnet. IP addresses associated with stopped instances are considered unavailable."},
	ErrorInsufficientHostCapacity:                              {HTTPCode: 503, Message: "There is not enough capacity to fulfill your Dedicated Host request. Reduce the number of Dedicated Hosts in your request, or wait for additional capacity to become available."},
	ErrorInsufficientInstanceCapacity:                          {HTTPCode: 400, Message: "There is not enough capacity to fulfill your request. This error can occur if you launch a new instance, restart a stopped instance, create a new Capacity Reservation, or modify an existing Capacity Reservation. Reduce the number of instances in your request, or wait for additional capacity to become available. You can also try launching an instance by selecting different instance types (which you can resize at a later stage). The returned message might also give specific guidance about how to solve the problem."},
	ErrorInsufficientReservedInstanceCapacity:                  {HTTPCode: 503, Message: "Not enough available Reserved Instances to satisfy your minimum request. Reduce the number of Reserved Instances in your request or wait for additional capacity to become available."},
	ErrorInsufficientReservedInstancesCapacity:                 {HTTPCode: 400, Message: "There is insufficient capacity for the requested Reserved Instances."},
	ErrorInsufficientVolumeCapacity:                            {HTTPCode: 503, Message: "There is not enough capacity to fulfill your EBS volume provision request. You can try to provision a different volume type, EBS volume in a different availability zone, or you can wait for additional capacity to become available."},
	ErrorInterfaceInUseByTrafficMirrorSession:                  {HTTPCode: 409, Message: "The Traffic Mirror source that you are trying to create uses an interface that is already associated with a session. An interface can only be associated with a session, or with a target, but not both."},
	ErrorInterfaceInUseByTrafficMirrorTarget:                   {HTTPCode: 409, Message: "The Traffic Mirror source that you are trying to create uses an interface that is already associated with a target. An interface can only be associated with a session, or with a target, but not both. If the interface is associated with a target, it cannot be associated with another target."},
	ErrorInternalError:                                         {HTTPCode: 500, Message: "An internal error has occurred. Retry your request, but if the problem persists, contact us with details by posting a message on AWS re:Post."},
	ErrorInternalFailure:                                       {HTTPCode: 404, Message: "The request processing has failed because of an unknown error, exception, or failure."},
	ErrorInternetGatewayLimitExceeded:                          {HTTPCode: 400, Message: "You've reached the limit on the number of internet gateways that you can create. For more information, see Amazon VPC quotas."},
	ErrorInvalidAMIAttributeItemValue:                          {HTTPCode: 400, Message: "The value of an item added to, or removed from, an image attribute is not valid. If you are specifying a userId, check that it is in the form of an AWS account ID, without hyphens."},
	ErrorInvalidAMIIDMalformed:                                 {HTTPCode: 400, Message: "The specified AMI ID is malformed. Ensure that you provide the full AMI ID, in the form ami-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidAMIIDNotFound:                                  {HTTPCode: 400, Message: "The specified AMI doesn't exist. Check the AMI ID, and ensure that you specify the AWS Region in which the AMI is located, if it's not in the default Region. This error might also occur if you specified an incorrect kernel ID when launching an instance. This error might also occur if the AMI doesn't meet your Allowed AMIs criteria. To use this AMI, you must change the Allowed AMIs criteria. For more information, see Control the discovery and use of AMIs in Amazon EC2 with Allowed AMIs."},
	ErrorInvalidAMIIDUnavailable:                               {HTTPCode: 400, Message: "The specified AMI has been deregistered and is no longer available, or is not in a state from which you can launch an instance or modify attributes."},
	ErrorInvalidAMINameDuplicate:                               {HTTPCode: 409, Message: "The specified AMI name is already in use by another AMI. If you have recently deregistered an AMI with the same name, allow enough time for the change to propagate through the system, and retry your request."},
	ErrorInvalidAMINameMalformed:                               {HTTPCode: 400, Message: "AMI names must be between 3 and 128 characters long, and may only contain letters, numbers, and the following special characters: '-', '_', '.', '/', '(', and ')'."},
	ErrorInvalidAction:                                         {HTTPCode: 400, Message: "The action or operation requested is not valid. Verify that the action is typed correctly."},
	ErrorInvalidAddressLocked:                                  {HTTPCode: 400, Message: "The specified Elastic IP address cannot be released from your account. A reverse DNS record may be associated with the Elastic IP address. To unlock the address, contact Support."},
	ErrorInvalidAddressMalformed:                               {HTTPCode: 400, Message: "The specified IP address is not valid. Ensure that you provide the address in the form xx.xx.xx.xx; for example, 55.123.45.67"},
	ErrorInvalidAddressNotFound:                                {HTTPCode: 400, Message: "The specified Elastic IP address that you are describing cannot be found. Ensure that you specify the AWS Region in which the IP address is located, if it's not in the default Region."},
	ErrorInvalidAddressIDNotFound:                              {HTTPCode: 400, Message: "The specified allocation ID for the Elastic IP address you are trying to release cannot be found. Ensure that you specify the AWS Region in which the IP address is located, if it's not in the default Region."},
	ErrorInvalidAffinity:                                       {HTTPCode: 400, Message: "The specified affinity value is not valid."},
	ErrorInvalidAllocationIDNotFound:                           {HTTPCode: 404, Message: "The specified allocation ID you are trying to describe or associate does not exist. Ensure that you specify the AWS Region in which the IP address is located, if it's not in the default Region."},
	ErrorInvalidAssociationIDNotFound:                          {HTTPCode: 404, Message: "The specified association ID (for an Elastic IP address, a route table, or network ACL) does not exist. Ensure that you specify the AWS Region in which the association ID is located, if it's not in the default Region."},
	ErrorInvalidAttachmentNotFound:                             {HTTPCode: 400, Message: "Indicates an attempt to detach a volume from an instance to which it is not attached."},
	ErrorInvalidAttachmentIDNotFound:                           {HTTPCode: 404, Message: "The specified network interface attachment does not exist."},
	ErrorInvalidAutoPlacement:                                  {HTTPCode: 400, Message: "The specified value for auto-placement is not valid."},
	ErrorInvalidAvailabilityZone:                               {HTTPCode: 400, Message: "The specified Availability Zone is not valid."},
	ErrorInvalidBlockDeviceMapping:                             {HTTPCode: 400, Message: "A block device mapping parameter is not valid. The returned message indicates the incorrect value."},
	ErrorInvalidBundleIDNotFound:                               {HTTPCode: 400, Message: "The specified bundle task ID cannot be found. Ensure that you specify the AWS Region in which the bundle task is located, if it's not in the default Region."},
	ErrorInvalidCapacityBlockOfferingIdExpired:                 {HTTPCode: 400, Message: "The Capacity Block offering ID is no longer available."},
	ErrorInvalidCapacityBlockOfferingIdMalformed:               {HTTPCode: 400, Message: "The Capacity Block offering ID is malformed."},
	ErrorInvalidCapacityBlockOfferingIdNotFound:                {HTTPCode: 400, Message: "The Capacity Block offering ID cannot be found for this account."},
	ErrorInvalidCapacityReservationIdMalformed:                 {HTTPCode: 400, Message: "The ID for the Capacity Reservation is malformed. Ensure that you specify the Capacity Reservation ID in the form cr-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidCapacityReservationIdNotFound:                  {HTTPCode: 404, Message: "The specified Capacity Reservation ID does not exist."},
	ErrorInvalidCapacityReservationStatePendingActivation:      {HTTPCode: 400, Message: "Your Capacity Block is not active yet."},
	ErrorInvalidCarrierGatewayIDNotFound:                       {HTTPCode: 400, Message: "The specified carrier gateway ID cannot be found. Ensure that you specify the AWS Region in which the carrier gateway is located, if it's not in the default Region."},
	ErrorInvalidCharacter:                                      {HTTPCode: 400, Message: "A specified character is invalid."},
	ErrorInvalidCidrInUse:                                      {HTTPCode: 409, Message: "The specified inside tunnel CIDR is already in use by another VPN tunnel for the virtual private gateway."},
	ErrorInvalidClientToken:                                    {HTTPCode: 400, Message: "The specified client token is not valid. For more information, see Ensuring idempotency."},
	ErrorInvalidClientTokenId:                                  {HTTPCode: 403, Message: "The X.509 certificate or credentials provided do not exist in our records."},
	ErrorInvalidClientVpnActiveAssociationNotFound:             {HTTPCode: 409, Message: "You cannot perform this action on the Client VPN endpoint while it is in the pending-association state."},
	ErrorInvalidClientVpnAssociationIdNotFound:                 {HTTPCode: 400, Message: "The specified target network association cannot be found."},
	ErrorInvalidClientVpnConnectionIdNotFound:                  {HTTPCode: 400, Message: "The specified Client VPN endpoint cannot be found."},
	ErrorInvalidClientVpnConnectionUserNotFound:                {HTTPCode: 400, Message: "The specified user does not have an active connection to the specified Client VPN endpoint."},
	ErrorInvalidClientVpnDuplicateAssociationException:         {HTTPCode: 400, Message: "The specified target network has already been associated with the Client VPN endpoint."},
	ErrorInvalidClientVpnDuplicateAuthorizationRule:            {HTTPCode: 400, Message: "The specified authorization has already been added to the Client VPN endpoint."},
	ErrorInvalidClientVpnDuplicateRoute:                        {HTTPCode: 400, Message: "The specified route has already been added to the Client VPN endpoint."},
	ErrorInvalidClientVpnEndpointAuthorizationRuleNotFound:     {HTTPCode: 400, Message: "The specified authorization rule cannot be found."},
	ErrorInvalidClientVpnEndpointIdNotFound:                    {HTTPCode: 400, Message: "The specified Client VPN Endpoint cannot be found."},
	ErrorInvalidClientVpnRouteNotFound:                         {HTTPCode: 400, Message: "The specified route cannot be found."},
	ErrorInvalidClientVpnSubnetIdDifferentAccount:              {HTTPCode: 400, Message: "The specified subnet belongs to a different account."},
	ErrorInvalidClientVpnSubnetIdDuplicateAz:                   {HTTPCode: 409, Message: "You have already associated a subnet from this Availability Zone with the Client VPN endpoint."},
	ErrorInvalidClientVpnSubnetIdNotFound:                      {HTTPCode: 400, Message: "The specified subnet cannot be found in the VPN with which the Client VPN endpoint is associated."},
	ErrorInvalidClientVpnSubnetIdOverlappingCidr:               {HTTPCode: 400, Message: "The specified target network's CIDR range overlaps with the Client VPN endpoint's client CIDR range."},
	ErrorInvalidConversionTaskId:                               {HTTPCode: 400, Message: "The specified conversion task ID (for instance or volume import) is not valid."},
	ErrorInvalidConversionTaskIdMalformed:                      {HTTPCode: 400, Message: "The specified conversion task ID (for instance or volume import) is malformed. Ensure that you've specified the ID in the form import-i-xxxxxxxx."},
	ErrorInvalidCpuCredits:                                     {HTTPCode: 400, Message: "The specified CpuCredit value is invalid. Valid values are standard and unlimited."},
	ErrorInvalidCpuCreditsMalformed:                            {HTTPCode: 400, Message: "The specified CpuCredit value is invalid. Valid values are standard and unlimited."},
	ErrorInvalidCustomerGatewayDuplicateIpAddress:              {HTTPCode: 409, Message: "There is a conflict among the specified gateway IP addresses. Each VPN connection in an AWS Region must be created with a unique customer gateway IP address (across all AWS accounts). For more information, see Your customer gateway device in the AWS Site-to-Site VPN User Guide."},
	ErrorInvalidCustomerGatewayIDNotFound:                      {HTTPCode: 400, Message: "The specified customer gateway ID cannot be found. Ensure that you specify the AWS Region in which the customer gateway is located, if it's not in the default Region."},
	ErrorInvalidCustomerGatewayIdMalformed:                     {HTTPCode: 400, Message: "The specified customer gateway ID is malformed, or cannot be found. Specify the ID in the form cgw-xxxxxxxx, and ensure that you specify the AWS Region in which the customer gateway is located, if it's not in the default Region."},
	ErrorInvalidCustomerGatewayState:                           {HTTPCode: 400, Message: "The customer gateway is not in the available state, and therefore cannot be used."},
	ErrorInvalidDeclarativePoliciesReportIdMalformed:           {HTTPCode: 400, Message: "The specified account status report ID for declarative policies is malformed. Ensure that you specify the ID in the form p-xxxxxxxxxx. For more information, see Generating the account status report for declarative policies."},
	ErrorInvalidDhcpOptionIDNotFound:                           {HTTPCode: 404, Message: "The specified DHCP options set does not exist. Ensure that you specify the AWS Region in which the DHCP options set is located, if it's not in the default Region."},
	ErrorInvalidDhcpOptionsIDNotFound:                          {HTTPCode: 404, Message: "The specified DHCP options set does not exist. Ensure that you specify the AWS Region in which the DHCP options set is located, if it's not in the default Region."},
	ErrorInvalidDhcpOptionsIdMalformed:                         {HTTPCode: 400, Message: "The specified DHCP options set ID is malformed. Ensure that you provide the full DHCP options set ID in the request, in the form dopt-xxxxxxxx."},
	ErrorInvalidEgressOnlyInternetGatewayIdMalformed:           {HTTPCode: 400, Message: "The specified egress-only internet gateway ID is malformed. Ensure that you specify the ID in the form eigw-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidEgressOnlyInternetGatewayIdNotFound:            {HTTPCode: 404, Message: "The specified egress-only internet gateway does not exist."},
	ErrorInvalidElasticGpuIDMalformed:                          {HTTPCode: 400, Message: "The specified Elastic GPU ID is malformed. Ensure that you specify the ID in the form egpu-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidElasticGpuIDNotFound:                           {HTTPCode: 404, Message: "The specified Elastic GPU does not exist."},
	ErrorInvalidExportTaskIDMalformed:                          {HTTPCode: 400, Message: "The specified export task ID cannot be found."},
	ErrorInvalidExportTaskIDNotFound:                           {HTTPCode: 400, Message: "The specified export task ID is malformed. Ensure that you specify the ID in the form export-ami-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidFilter:                                         {HTTPCode: 400, Message: "The specified filter is not valid."},
	ErrorInvalidFlowLogIdNotFound:                              {HTTPCode: 404, Message: "The specified flow log does not exist."},
	ErrorInvalidFormat:                                         {HTTPCode: 400, Message: "The specified disk format (for the instance or volume import) is not valid."},
	ErrorInvalidFpgaImageIDMalformed:                           {HTTPCode: 400, Message: "The specified Amazon FPGA image (AFI) ID is malformed. Ensure that you provide the full AFI ID in the request, in the form afi-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidFpgaImageIDNotFound:                            {HTTPCode: 404, Message: "The specified Amazon FPGA image (AFI) ID does not exist. Ensure that you specify the AWS Region in which the AFI is located, if it's not in the default Region."},
	ErrorInvalidGatewayIDNotFound:                              {HTTPCode: 404, Message: "The specified gateway does not exist."},
	ErrorInvalidGroupDuplicate:                                 {HTTPCode: 400, Message: "You cannot create a security group with the same name as an existing security group in the same VPC."},
	ErrorInvalidGroupInUse:                                     {HTTPCode: 409, Message: "The specified security group can't be deleted because it's in use by another security group. You can remove dependencies by modifying or deleting rules in the affected security groups."},
	ErrorInvalidGroupNotFound:                                  {HTTPCode: 404, Message: "The specified security group does not exist. This error can occur because the ID of a recently created security group has not propagated through the system. For more information, see Ensuring idempotency. You can't specify a security group that is in a different AWS Region or VPC than the request."},
	ErrorInvalidGroupReserved:                                  {HTTPCode: 400, Message: "The name 'default' is reserved, and cannot be used to create a new security group."},
	ErrorInvalidGroupIdMalformed:                               {HTTPCode: 400, Message: "The specified security group ID is malformed. Ensure that you provide the full security group ID in the request, in the form sg-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidHostConfiguration:                              {HTTPCode: 400, Message: "The specified Dedicated Host configuration is not supported."},
	ErrorInvalidHostIDMalformed:                                {HTTPCode: 400, Message: "The specified Dedicated Host ID is not formed correctly. Ensure that you provide the full ID in the form h-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidHostIDNotFound:                                 {HTTPCode: 404, Message: "The specified Dedicated Host ID does not exist. Ensure that you specify the AWS Region in which the Dedicated Host is located, if it's not in the default Region."},
	ErrorInvalidHostId:                                         {HTTPCode: 400, Message: "The specified Dedicated Host ID is not valid."},
	ErrorInvalidHostIdMalformed:                                {HTTPCode: 400, Message: "The specified Dedicated Host ID is not formed correctly. Ensure that you provide the full ID in the form h-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidHostIdNotFound:                                 {HTTPCode: 404, Message: "The specified Dedicated Host ID does not exist. Ensure that you specify the region in which the Dedicated Host is located, if it's not in the default region."},
	ErrorInvalidHostReservationIdMalformed:                     {HTTPCode: 400, Message: "The specified Dedicated Host Reservation ID is not formed correctly. Ensure that you provide the full ID in the form hr-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidHostReservationOfferingIdMalformed:             {HTTPCode: 400, Message: "The specified Dedicated Host Reservation offering is not formed correctly. Ensure that you provide the full ID in the form hro-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidHostState:                                      {HTTPCode: 400, Message: "The Dedicated Host must be in the available state to complete the operation."},
	ErrorInvalidID:                                             {HTTPCode: 400, Message: "The specified ID for the resource you are trying to tag is not valid. Ensure that you provide the full resource ID; for example, ami-2bb65342 for an AMI. If you're using the command line tools on a Windows system, you might need to use quotation marks for the key-value pair; for example, \"Name=TestTag\"."},
	ErrorInvalidIPAddressInUse:                                 {HTTPCode: 409, Message: "The specified IP address is already in use. If you are trying to release an address, you must first disassociate it from the instance."},
	ErrorInvalidIamInstanceProfileArnMalformed:                 {HTTPCode: 400, Message: "The specified IAM instance profile ARN is not valid. For more information about valid ARN formats, see Amazon Resource Names (ARNs)."},
	ErrorInvalidInput:                                          {HTTPCode: 400, Message: "An input parameter in the request is not valid. For example, you may have specified an incorrect Reserved Instance listing ID in the request or the Reserved Instance you tried to list cannot be sold in the Reserved Instances Marketplace (for example, if it has a scope of Region, or is a Convertible Reserved Instance)."},
	ErrorInvalidInstanceAttributeValue:                         {HTTPCode: 400, Message: "The specified instance attribute value is not valid. This error is most commonly encountered when trying to set the InstanceType/--instance-type attribute to an unrecognized value."},
	ErrorInvalidInstanceConnectEndpointIdMalformed:             {HTTPCode: 400, Message: "The specified EC2 Instance Connect Endpoint ID is malformed. Ensure that you specify the ID in the form eice-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidInstanceConnectEndpointIdNotFound:              {HTTPCode: 404, Message: "The specified EC2 Instance Connect Endpoint does not exist."},
	ErrorInvalidInstanceCreditSpecification:                    {HTTPCode: 409, Message: "If you are modifying the credit option for CPU usage for T2 instances, the request may not contain duplicate instance IDs."},
	ErrorInvalidInstanceCreditSpecificationDuplicateInstanceId: {HTTPCode: 409, Message: "If you are modifying the credit option for CPU usage for T2 instances, the request may not contain duplicate instance IDs."},
	ErrorInvalidInstanceEventIDNotFound:                        {HTTPCode: 400, Message: "The specified ID of the event whose date and time you are modifying cannot be found. Verify the ID of the event and try your request again."},
	ErrorInvalidInstanceEventStartTime:                         {HTTPCode: 400, Message: "The specified scheduled event start time does not meet the requirements for rescheduling a scheduled event. For more information, see Limitations."},
	ErrorInvalidInstanceFamily:                                 {HTTPCode: 400, Message: "The instance family is not supported for this request. For example, the instance family for the Dedicated Host Reservation offering is different from the instance family of the Dedicated Hosts. Or, you can only modify the default credit specification for burstable performance instance families (T2, T3, and T3a). For more information, see Set the default credit specification for the account."},
	ErrorInvalidInstanceID:                                     {HTTPCode: 400, Message: "This error can occur when trying to perform an operation on an instance that has multiple network interfaces. A network interface can have individual attributes; therefore, you may need to specify the network interface ID as part of the request, or use a different request. For example, each network interface in an instance can have a source/destination check flag. To modify this attribute, modify the network interface attribute, and not the instance attribute. To create a route in a route table, provide a specific network interface ID as part of the request."},
	ErrorInvalidInstanceIDMalformed:                            {HTTPCode: 400, Message: "The specified instance ID is malformed. Ensure that you provide the full instance ID in the request, in the form i-xxxxxxxx or i-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidInstanceIDNotFound:                             {HTTPCode: 404, Message: "The specified instance does not exist. This error might occur because the ID of a recently created instance has not propagated through the system. For more information, see Ensuring idempotency."},
	ErrorInvalidInstanceIDNotLinkable:                          {HTTPCode: 400, Message: "The specified instance cannot be linked to the specified VPC. This error may also occur if the instance was recently launched, and its ID has not yet propagated through the system. Wait a few minutes, or wait until the instance is in the running state, and then try again."},
	ErrorInvalidInstanceState:                                  {HTTPCode: 400, Message: "The instance is not in an appropriate state to complete the request. If you're modifying the instance placement, the instance must be in the stopped state."},
	ErrorInvalidInstanceType:                                   {HTTPCode: 400, Message: "The instance type is not supported for this request. For example, you can only bundle instance store-backed Windows instances."},
	ErrorInvalidInterfaceIpAddressLimitExceeded:                {HTTPCode: 400, Message: "The number of private IP addresses for a specified network interface exceeds the limit for the type of instance you are trying to launch. For more information, see IP addresses per network interface per instance type."},
	ErrorInvalidInternetGatewayIDNotFound:                      {HTTPCode: 404, Message: "The specified internet gateway does not exist. Ensure that you specify the AWS Region in which the internet gateway is located, if it's not in the default Region."},
	ErrorInvalidInternetGatewayIdMalformed:                     {HTTPCode: 400, Message: "The specified internet gateway ID is malformed. Ensure that you provide the full ID in the request, in the form igw-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidKernelIdMalformed:                              {HTTPCode: 400, Message: "The specified kernel ID is not valid. Ensure that you specify the kernel ID in the form aki-xxxxxxxx."},
	ErrorInvalidKeyFormat:                                      {HTTPCode: 400, Message: "The key pair is not specified in a valid OpenSSH public key format."},
	ErrorInvalidKeyPairDuplicate:                               {HTTPCode: 409, Message: "The key pair name already exists in that AWS Region. If you are creating or importing a key pair, ensure that you use a unique name."},
	ErrorInvalidKeyPairFormat:                                  {HTTPCode: 400, Message: "The format of the public key you are attempting to import is not valid."},
	ErrorInvalidKeyPairNotFound:                                {HTTPCode: 404, Message: "The specified key pair name does not exist. Ensure that you specify the AWS Region in which the key pair is located, if it's not in the default Region."},
	ErrorInvalidLaunchTargets:                                  {HTTPCode: 400, Message: "One or more specified targets are invalid. Verify the capacity for the Capacity Reservation selected or verify the ID."},
	ErrorInvalidLaunchTemplateIdMalformed:                      {HTTPCode: 400, Message: "The ID for the launch template is malformed. Ensure that you specify the launch template ID in the form lt-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidLaunchTemplateIdNotFound:                       {HTTPCode: 404, Message: "The specified launch template ID does not exist. Ensure that you specify the AWS Region in which the launch template is located."},
	ErrorInvalidLaunchTemplateIdVersionNotFound:                {HTTPCode: 404, Message: "The specified launch template version does not exist."},
	ErrorInvalidLaunchTemplateNameAlreadyExistsException:       {HTTPCode: 409, Message: "The specified launch template name is already in use."},
	ErrorInvalidLaunchTemplateNameMalformedException:           {HTTPCode: 400, Message: "The specified launch template name is invalid. A launch template name must be between 3 and 128 characters, and may contain letters, numbers, and the following characters: '-', '_', '.', '/', '(', and ')'."},
	ErrorInvalidLaunchTemplateNameNotFoundException:            {HTTPCode: 404, Message: "The specified launch template name does not exist. Check the spelling of the name and ensure that you specify the AWS Region in which the launch template is located. Launch template names are case-sensitive."},
	ErrorInvalidManifest:                                       {HTTPCode: 400, Message: "The specified AMI has an unparsable manifest, or you may not have access to the location of the manifest file in Amazon S3."},
	ErrorInvalidMaxResults:                                     {HTTPCode: 400, Message: "The specified value for MaxResults is not valid."},
	ErrorInvalidNatGatewayIDNotFound:                           {HTTPCode: 404, Message: "The specified NAT gateway ID does not exist. Ensure that you specify the AWS Region in which the NAT gateway is located, if it's not in the default Region."},
	ErrorInvalidNetworkAclEntryNotFound:                        {HTTPCode: 404, Message: "The specified network ACL entry does not exist."},
	ErrorInvalidNetworkAclIDNotFound:                           {HTTPCode: 404, Message: "The specified network ACL does not exist. Ensure that you specify the AWS Region in which the network ACL is located, if it's not in the default Region."},
	ErrorInvalidNetworkAclIdMalformed:                          {HTTPCode: 400, Message: "The specified network ACL ID is malformed. Ensure that you provide the ID in the form acl-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidNetworkInterfaceInUse:                          {HTTPCode: 409, Message: "The specified interface is currently in use and cannot be deleted or attached to another instance. Ensure that you have detached the network interface first. If a network interface is in use, you may also receive the InvalidParameterValue error."},
	ErrorInvalidNetworkInterfaceNotFound:                       {HTTPCode: 404, Message: "The specified network interface does not exist."},
	ErrorInvalidNetworkInterfaceAttachmentIdMalformed:          {HTTPCode: 400, Message: "The ID for the network interface attachment is malformed. Ensure that you use the attachment ID rather than the network interface ID, in the form eni-attach-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidNetworkInterfaceIDNotFound:                     {HTTPCode: 404, Message: "The specified network interface does not exist. Ensure that you specify the AWS Region in which the network interface is located, if it's not in the default Region."},
	ErrorInvalidNetworkInterfaceIdMalformed:                    {HTTPCode: 400, Message: "The specified network interface ID is malformed. Ensure that you specify the network interface ID in the form eni-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidNetworkLoadBalancerArnMalformed:                {HTTPCode: 400, Message: "The specified Network Load Balancer ARN is malformed. Ensure that you specify the ARN in the form arn:aws:elasticloadbalancing:region:account-id:loadbalancer/net/load-balancer-name/load-balancer-id."},
	ErrorInvalidNetworkLoadBalancerArnNotFound:                 {HTTPCode: 404, Message: "The specified Network Load Balancer ARN does not exist."},
	ErrorInvalidNextToken:                                      {HTTPCode: 400, Message: "The specified NextToken is not valid."},
	ErrorInvalidOptionConflict:                                 {HTTPCode: 409, Message: "A VPN connection between the virtual private gateway and the customer gateway already exists."},
	ErrorInvalidPaginationToken:                                {HTTPCode: 403, Message: "The specified pagination token is not valid or is expired."},
	ErrorInvalidParameter:                                      {HTTPCode: 400, Message: "A parameter specified in a request is not valid, is unsupported, or cannot be used. The returned message provides an explanation of the error value. For example, if you are launching an instance, you can't specify a security group and subnet that are in different VPCs."},
	ErrorInvalidParameterCombination:                           {HTTPCode: 400, Message: "Indicates an incorrect combination of parameters, or a missing parameter. For example, trying to terminate an instance without specifying the instance ID."},
	ErrorInvalidParameterDependency:                            {HTTPCode: 400, Message: "Indicates an incorrect combination of parameters, or a missing parameter. For example, trying to terminate an instance without specifying the instance ID."},
	ErrorInvalidParameterValue:                                 {HTTPCode: 400, Message: "A value specified in a parameter is not valid, is unsupported, or cannot be used. Ensure that you specify a resource by using its full ID. The returned message provides an explanation of the error value."},
	ErrorInvalidPermissionDuplicate:                            {HTTPCode: 409, Message: "The specified inbound or outbound rule already exists for that security group."},
	ErrorInvalidPermissionMalformed:                            {HTTPCode: 400, Message: "The specified security group rule is malformed. If you are specifying an IP address range, ensure that you use CIDR notation; for example, 203.0.113.0/24."},
	ErrorInvalidPermissionNotFound:                             {HTTPCode: 404, Message: "The specified rule does not exist in this security group."},
	ErrorInvalidPlacementGroupDuplicate:                        {HTTPCode: 409, Message: "The specified placement group already exists in that AWS Region."},
	ErrorInvalidPlacementGroupInUse:                            {HTTPCode: 409, Message: "The specified placement group is in use. If you are trying to delete a placement group, ensure that its instances have been terminated."},
	ErrorInvalidPlacementGroupUnknown:                          {HTTPCode: 400, Message: "The specified placement group cannot be found. Ensure that you specify the AWS Region in which the placement group is located, if it's not in the default Region."},
	ErrorInvalidPlacementGroupIdMalformed:                      {HTTPCode: 400, Message: "The specified placement group ID is malformed. Ensure that you specify the ID in the form pg-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidPolicyDocument:                                 {HTTPCode: 400, Message: "The specified policy document is not a valid JSON policy document."},
	ErrorInvalidPrefixListIDNotFound:                           {HTTPCode: 404, Message: "The specified prefix list ID does not exist."},
	ErrorInvalidPrefixListIdMalformed:                          {HTTPCode: 400, Message: "The specified prefix list ID is malformed. Ensure that you provide the ID in the form pl-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidProductInfo:                                    {HTTPCode: 400, Message: "(AWS Marketplace) The product code is not valid."},
	ErrorInvalidPurchaseTokenExpired:                           {HTTPCode: 403, Message: "The specified purchase token has expired."},
	ErrorInvalidPurchaseTokenMalformed:                         {HTTPCode: 400, Message: "The specified purchase token is not valid."},
	ErrorInvalidQuantity:                                       {HTTPCode: 400, Message: "The specified quantity of Dedicated Hosts is not valid."},
	ErrorInvalidQueryParameter:                                 {HTTPCode: 400, Message: "The AWS query string is malformed or does not adhere to AWS standards."},
	ErrorInvalidRamDiskIdMalformed:                             {HTTPCode: 400, Message: "The specified RAM disk ID is not valid. Ensure that you specify the RAM disk ID in the form ari-xxxxxxxx."},
	ErrorInvalidRegion:                                         {HTTPCode: 400, Message: "The specified AWS Region is not valid. For copying a snapshot or image, specify the source Region using its Region code, for example, us-west-2."},
	ErrorInvalidRequest:                                        {HTTPCode: 400, Message: "The request is not valid. The returned message provides details about the nature of the error."},
	ErrorInvalidReservationIDMalformed:                         {HTTPCode: 400, Message: "The specified reservation ID is not valid."},
	ErrorInvalidReservationIDNotFound:                          {HTTPCode: 404, Message: "The specified reservation does not exist."},
	ErrorInvalidReservedInstancesId:                            {HTTPCode: 404, Message: "The specified Reserved Instance does not exist."},
	ErrorInvalidReservedInstancesOfferingId:                    {HTTPCode: 404, Message: "The specified Reserved Instances offering does not exist."},
	ErrorInvalidResourceConfigurationArnMalformed:              {HTTPCode: 400, Message: "The specified resource configuration ARN is malformed."},
	ErrorInvalidResourceConfigurationArnNotFound:               {HTTPCode: 404, Message: "The specified resource configuration ARN does not exist."},
	ErrorInvalidResourceTypeUnknown:                            {HTTPCode: 400, Message: "The specified resource type is not supported or is not valid. To view resource types that support longer IDs, use DescribeIdFormat."},
	ErrorInvalidRouteInvalidState:                              {HTTPCode: 400, Message: "The specified route is not valid."},
	ErrorInvalidRouteMalformed:                                 {HTTPCode: 400, Message: "The specified route is not valid. If you are deleting a route in a VPN connection, ensure that you've entered the value for the CIDR block correctly."},
	ErrorInvalidRouteNotFound:                                  {HTTPCode: 404, Message: "The specified route does not exist in the specified route table. Ensure that you indicate the exact CIDR range for the route in the request. This error can also occur if you've specified a route table ID in the request that does not exist."},
	ErrorInvalidRouteTableIDNotFound:                           {HTTPCode: 404, Message: "The specified route table does not exist. Ensure that you specify the AWS Region in which the route table is located, if it's not in the default Region."},
	ErrorInvalidRouteTableIdMalformed:                          {HTTPCode: 400, Message: "The specified route table ID is malformed. Ensure that you specify the route table ID in the form rtb-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidScheduledInstance:                              {HTTPCode: 404, Message: "The specified Scheduled Instance does not exist."},
	ErrorInvalidSecurityRequestHasExpired:                      {HTTPCode: 400, Message: "The difference between the request timestamp and the AWS server time is greater than 5 minutes. Ensure that your system clock is accurate and configured to use the correct time zone."},
	ErrorInvalidSecurityGroupIDNotFound:                        {HTTPCode: 404, Message: "The specified security group does not exist."},
	ErrorInvalidSecurityGroupIdMalformed:                       {HTTPCode: 400, Message: "The specified security group ID is not valid. Ensure that you specify the security group ID in the form sg-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidSecurityGroupRuleIdMalformed:                   {HTTPCode: 400, Message: "The specified security group rule ID is not valid. Ensure that you specify the security group rule ID in the form sgr-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidSecurityGroupRuleIdNotFound:                    {HTTPCode: 404, Message: "The specified security group rule does not exist."},
	ErrorInvalidServiceName:                                    {HTTPCode: 400, Message: "The name of the service is not valid. To get a list of available service names, use DescribeVpcEndpointServices."},
	ErrorInvalidSnapshotInUse:                                  {HTTPCode: 409, Message: "The snapshot that you are trying to delete is in use by one or more AMIs."},
	ErrorInvalidSnapshotNotFound:                               {HTTPCode: 404, Message: "The specified snapshot does not exist. Ensure that you specify the AWS Region in which the snapshot is located, if it's not in the default Region."},
	ErrorInvalidSnapshotIDMalformed:                            {HTTPCode: 400, Message: "The snapshot ID is not valid."},
	ErrorInvalidSpotDatafeedNotFound:                           {HTTPCode: 400, Message: "You have no data feed for Spot Instances."},
	ErrorInvalidSpotFleetRequestConfig:                         {HTTPCode: 400, Message: "The Spot Fleet request configuration is not valid. Ensure that you provide valid values for all of the configuration parameters; for example, a valid AMI ID. Limits apply on the target capacity and the number of launch specifications per Spot Fleet request. For more information, see Fleet quotas."},
	ErrorInvalidSpotFleetRequestIdMalformed:                    {HTTPCode: 400, Message: "The specified Spot Fleet request ID is malformed. Ensure that you specify the Spot Fleet request ID in the form sfr- followed by 36 characters, including hyphens; for example, sfr-123f8fc2-11aa-22bb-33cc-example12710."},
	ErrorInvalidSpotFleetRequestIdNotFound:                     {HTTPCode: 404, Message: "The specified Spot Fleet request ID does not exist. Ensure that you specify the AWS Region in which the Spot Fleet request is located, if it's not in the default Region."},
	ErrorInvalidSpotInstanceRequestIDMalformed:                 {HTTPCode: 400, Message: "The specified Spot Instance request ID is not valid. Ensure that you specify the Spot Instance request ID in the form sir-xxxxxxxx."},
	ErrorInvalidSpotInstanceRequestIDNotFound:                  {HTTPCode: 404, Message: "The specified Spot Instance request ID does not exist. Ensure that you specify the AWS Region in which the Spot Instance request is located, if it's not in the default Region."},
	ErrorInvalidState:                                          {HTTPCode: 400, Message: "The specified resource is not in the correct state for the request; for example, if you are trying to enable monitoring on a recently terminated instance, or if you are trying to create a snapshot when a previous identical request has not yet completed."},
	ErrorInvalidStateTransition:                                {HTTPCode: 400, Message: "The specified VPC peering connection is not in the correct state for the request. For example, you may be trying to accept a VPC peering request that has failed, or that was rejected."},
	ErrorInvalidSubnet:                                         {HTTPCode: 404, Message: "The specified subnet ID is not valid or does not exist."},
	ErrorInvalidSubnetConflict:                                 {HTTPCode: 409, Message: "The specified CIDR block conflicts with that of another subnet in your VPC."},
	ErrorInvalidSubnetRange:                                    {HTTPCode: 400, Message: "The CIDR block you've specified for the subnet is not valid. The allowed block size is between a /28 netmask and /16 netmask."},
	ErrorInvalidSubnetIDMalformed:                              {HTTPCode: 400, Message: "The specified subnet ID is malformed. Ensure that you specify the ID in the form subnet-xxxxxxxxxxxxxxxxx"},
	ErrorInvalidSubnetIDNotFound:                               {HTTPCode: 404, Message: "or InvalidSubnetId.NotFound \tThe specified subnet does not exist."},
	ErrorInvalidTagKeyMalformed:                                {HTTPCode: 400, Message: "The specified tag key is not valid. Tag keys cannot be empty or null, and cannot start with aws:."},
	ErrorInvalidTargetArnUnknown:                               {HTTPCode: 404, Message: "The specified ARN for the specified user or role is not valid or does not exist."},
	ErrorInvalidTargetException:                                {HTTPCode: 404, Message: "The specified TargetId is not valid, does not exist, or is in another organization. You can only generate an account status report for declarative policies in your own organization. Ensure that you specify the TargetId in one of the following forms: r-xxxx, ou-xxxx-xxxxxxxx, or a 12-digit account ID in the form xxxxxxxxxxxx."},
	ErrorInvalidTenancy:                                        {HTTPCode: 400, Message: "The tenancy of the instance or VPC is not supported for the requested action. For example, you cannot modify the tenancy of an instance or VPC that has a tenancy attribute of default."},
	ErrorInvalidTime:                                           {HTTPCode: 400, Message: "The specified timestamp is not valid."},
	ErrorInvalidTrafficMirrorFilterNotFound:                    {HTTPCode: 404, Message: "The specified Traffic Mirror filter does not exist."},
	ErrorInvalidTrafficMirrorFilterRuleNotFound:                {HTTPCode: 404, Message: "The specified Traffic Mirror filter rule does not exist."},
	ErrorInvalidTrafficMirrorSessionNotFound:                   {HTTPCode: 404, Message: "The specified Traffic Mirror session does not exist."},
	ErrorInvalidTrafficMirrorTargetNoFound:                     {HTTPCode: 404, Message: "The specified Traffic Mirror target does not exist."},
	ErrorInvalidUserIDMalformed:                                {HTTPCode: 400, Message: "The specified user or owner is not valid. If you are performing a DescribeImages request, you must specify a valid value for the owner or executableBy parameters, such as an AWS account ID. If you are performing a DescribeSnapshots request, you must specify a valid value for the owner or restorableBy parameters."},
	ErrorInvalidVolumeNotFound:                                 {HTTPCode: 404, Message: "The specified volume does not exist."},
	ErrorInvalidVolumeZoneMismatch:                             {HTTPCode: 400, Message: "The specified volume is not in the same Availability Zone as the specified instance. You can only attach an Amazon EBS volume to an instance if they are in the same Availability Zone."},
	ErrorInvalidVolumeIDDuplicate:                              {HTTPCode: 409, Message: "The Amazon EBS volume already exists."},
	ErrorInvalidVolumeIDMalformed:                              {HTTPCode: 400, Message: "The specified volume ID is not valid. Check the letter-number combination carefully."},
	ErrorInvalidVolumeIDZoneMismatch:                           {HTTPCode: 400, Message: "The specified volume and instance are in different Availability Zones."},
	ErrorInvalidVpcEndpointNotFound:                            {HTTPCode: 404, Message: "The specified VPC endpoint does not exist. If you are performing a bulk request that is partially successful or unsuccessful, the response includes a list of the unsuccessful items. If the request succeeds, the list is empty."},
	ErrorInvalidVpcEndpointIdMalformed:                         {HTTPCode: 400, Message: "The specified VPC endpoint ID is malformed. Use the full VPC endpoint ID in the request, in the form vpce-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidVpcEndpointIdNotFound:                          {HTTPCode: 404, Message: "The specified VPC endpoint does not exist. If you are performing a bulk request that is partially successful or unsuccessful, the response includes a list of the unsuccessful items. If the request succeeds, the list is empty."},
	ErrorInvalidVpcEndpointServiceNotFound:                     {HTTPCode: 404, Message: "The specified VPC endpoint service does not exist. If you are performing a bulk request that is partially successful or unsuccessful, the response includes a list of the unsuccessful items. If the request succeeds, the list is empty."},
	ErrorInvalidVpcEndpointServiceIdNotFound:                   {HTTPCode: 404, Message: "The specified VPC endpoint service does not exist. If you are performing a bulk request that is partially successful or unsuccessful, the response includes a list of the unsuccessful items. If the request succeeds, the list is empty."},
	ErrorInvalidVpcEndpointServiceIdIdMalformed:                {HTTPCode: 400, Message: "The specified VPC endpoint service ID is malformed. Ensure that you specify the ID in the form vpc-svc-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidVpcEndpointType:                                {HTTPCode: 400, Message: "The specified VPC endpoint type is not valid."},
	ErrorInvalidVpcIDMalformed:                                 {HTTPCode: 400, Message: "The specified VPC ID is malformed. Ensure that you've specified the ID in the form vpc-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidVpcIDNotFound:                                  {HTTPCode: 404, Message: "The specified VPC does not exist."},
	ErrorInvalidVpcPeeringConnectionIDNotFound:                 {HTTPCode: 404, Message: "The specified VPC peering connection ID does not exist."},
	ErrorInvalidVpcPeeringConnectionIdMalformed:                {HTTPCode: 400, Message: "The specified VPC peering connection ID is malformed. Ensure that you provide the ID in the form pcx-xxxxxxxxxxxxxxxxx."},
	ErrorInvalidVpcPeeringConnectionStateDnsHostnamesDisabled:  {HTTPCode: 400, Message: "To enable DNS hostname resolution for the VPC peering connection, DNS hostname support must be enabled for the VPCs."},
	ErrorInvalidVpcRange:                                       {HTTPCode: 400, Message: "The specified CIDR block range is not valid. The block range must be between a /28 netmask and /16 netmask. For more information, see VPC CIDR blocks."},
	ErrorInvalidVpcState:                                       {HTTPCode: 400, Message: "The specified VPC already has a virtual private gateway attached to it."},
	ErrorInvalidVpnConnectionInvalidState:                      {HTTPCode: 400, Message: "The VPN connection must be in the available state to complete the request."},
	ErrorInvalidVpnConnectionInvalidType:                       {HTTPCode: 400, Message: "The specified VPN connection does not support static routes."},
	ErrorInvalidVpnConnectionID:                                {HTTPCode: 400, Message: "The specified VPN connection ID cannot be found."},
	ErrorInvalidVpnConnectionIDNotFound:                        {HTTPCode: 404, Message: "The specified VPN connection ID does not exist."},
	ErrorInvalidVpnGatewayAttachmentNotFound:                   {HTTPCode: 404, Message: "An attachment between the specified virtual private gateway and specified VPC does not exist. This error can also occur if you've specified an incorrect VPC ID in the request."},
	ErrorInvalidVpnGatewayIDNotFound:                           {HTTPCode: 404, Message: "The specified virtual private gateway does not exist."},
	ErrorInvalidVpnGatewayState:                                {HTTPCode: 400, Message: "The virtual private gateway is not in an available state."},
	ErrorInvalidZoneNotFound:                                   {HTTPCode: 404, Message: "The specified Availability Zone does not exist, or is not available for you to use. Use the DescribeAvailabilityZones request to list the Availability Zones that are currently available to you. Specify the full name of the Availability Zone: for example, us-east-1a."},
	ErrorKeyPairLimitExceeded:                                  {HTTPCode: 400, Message: "You've reached the limit on the number of key pairs that you can have in this AWS Region. For more information, see Amazon EC2 key pairs."},
	ErrorLegacySecurityGroup:                                   {HTTPCode: 400, Message: "Any VPC created using an API version older than 2011-01-01 may have the 2009-07-15-default security group. You must delete this security group before you can attach an internet gateway to the VPC."},
	ErrorLimitPriceExceeded:                                    {HTTPCode: 400, Message: "The cost of the total order is greater than the specified limit price (instance count * price)."},
	ErrorLogDestinationNotFound:                                {HTTPCode: 404, Message: "The specified Amazon S3 bucket does not exist. Ensure that you have specified the ARN for an existing Amazon S3 bucket, and that the ARN is in the correct format."},
	ErrorLogDestinationPermissionIssue:                         {HTTPCode: 400, Message: "You do not have sufficient permissions to publish flow logs to the specific Amazon S3 bucket."},
	ErrorMalformedQueryString:                                  {HTTPCode: 400, Message: "The query string contains a syntax error."},
	ErrorMaxConfigLimitExceededException:                       {HTTPCode: 400, Message: "You\u2019ve exceeded your maximum allowed Spot placement configurations. You can retry configurations that you used within the last 24 hours, or wait for 24 hours before specifying a new configuration. For more information, see Spot placement score."},
	ErrorMaxIOPSLimitExceeded:                                  {HTTPCode: 400, Message: "You've reached the limit on your IOPS usage for the AWS Region. For more information, see Quotas for Amazon EBS."},
	ErrorMaxScheduledInstanceCapacityExceeded:                  {HTTPCode: 400, Message: "You've attempted to launch more instances than you purchased."},
	ErrorMaxSpotFleetRequestCountExceeded:                      {HTTPCode: 400, Message: "You've reached one or both of these limits: the total number of Spot Fleet requests that you can make, or the total number of instances in all Spot Fleets for the AWS Region (the target capacity). For more information, see Fleet quotas."},
	ErrorMaxSpotInstanceCountExceeded:                          {HTTPCode: 400, Message: "You've reached the limit on the number of Spot Instances that you can launch. The limit depends on the instance type. For more information, see Spot Instance limits."},
	ErrorMaxTemplateLimitExceeded:                              {HTTPCode: 400, Message: "You've reached the limit on the number of launch templates you can create. For more information, see Launch template restrictions."},
	ErrorMaxTemplateVersionLimitExceeded:                       {HTTPCode: 400, Message: "You've reached the limit on the number of launch template versions that you can create. For more information, see Launch template restrictions."},
	ErrorMissingAction:                                         {HTTPCode: 400, Message: "The request is missing an action or a required parameter."},
	ErrorMissingAuthenticationToken:                            {HTTPCode: 403, Message: "The request must contain valid credentials."},
	ErrorMissingInput:                                          {HTTPCode: 400, Message: "An input parameter is missing."},
	ErrorMissingParameter:                                      {HTTPCode: 400, Message: "The request is missing a required parameter. Ensure that you have supplied all the required parameters for the request; for example, the resource ID."},
	ErrorNatGatewayLimitExceeded:                               {HTTPCode: 400, Message: "You've reached the limit on the number of NAT gateways that you can create. For more information, see Amazon VPC quotas."},
	ErrorNatGatewayMalformed:                                   {HTTPCode: 400, Message: "The specified NAT gateway ID is not formed correctly. Ensure that you specify the NAT gateway ID in the form nat-xxxxxxxxxxxxxxxxx."},
	ErrorNatGatewayNotFound:                                    {HTTPCode: 404, Message: "The specified NAT gateway does not exist."},
	ErrorNetworkAclEntryAlreadyExists:                          {HTTPCode: 409, Message: "The specified rule number already exists in this network ACL."},
	ErrorNetworkAclEntryLimitExceeded:                          {HTTPCode: 400, Message: "You've reached the limit on the number of rules that you can add to the network ACL. For more information, see Amazon VPC quotas."},
	ErrorNetworkAclLimitExceeded:                               {HTTPCode: 400, Message: "You've reached the limit on the number of network ACLs that you can create for the specified VPC. For more information, see Amazon VPC quotas."},
	ErrorNetworkInterfaceLimitExceeded:                         {HTTPCode: 400, Message: "You've reached the limit on the number of network interfaces that you can create. For more information, see Amazon VPC quotas."},
	ErrorNetworkInterfaceNotSupported:                          {HTTPCode: 400, Message: "The network interface is not supported for Traffic Mirroring."},
	ErrorNetworkLoadBalancerNotFoundException:                  {HTTPCode: 404, Message: "The specified Network Load Balancer does not exist."},
	ErrorNlbInUseByTrafficMirrorTargetException:                {HTTPCode: 400, Message: "The Network Load Balancer is already configured as a Traffic Mirror target."},
	ErrorNoSuchVersion:                                         {HTTPCode: 404, Message: "The specified API version does not exist."},
	ErrorNonEBSInstance:                                        {HTTPCode: 400, Message: "The specified instance does not support Amazon EBS. Restart the instance and try again, to ensure that the code is run on an instance with updated code."},
	ErrorNotExportable:                                         {HTTPCode: 400, Message: "The specified instance cannot be exported. You can only export certain instances. For more information, see Considerations for instance export."},
	ErrorOperationNotPermitted:                                 {HTTPCode: 400, Message: "The specified operation is not allowed. This error can occur for a number of reasons; for example, you might be trying to terminate an instance that has termination protection enabled, or trying to detach the primary network interface (eth0) from an instance."},
	ErrorOptInRequired:                                         {HTTPCode: 403, Message: "You are not authorized to use the requested service. Ensure that you have subscribed to the service you are trying to use. If you are new to AWS, your account might take some time to be activated while your credit card details are being verified."},
	ErrorOutstandingVpcPeeringConnectionLimitExceeded:          {HTTPCode: 400, Message: "You've reached the limit on the number of VPC peering connection requests that you can create for the specified VPC."},
	ErrorPendingSnapshotLimitExceeded:                          {HTTPCode: 409, Message: "You've reached the limit on the number of Amazon EBS snapshots that you can have in the pending state."},
	ErrorPendingVerification:                                   {HTTPCode: 409, Message: "Your account is pending verification. Until the verification process is complete, you may not be able to carry out requests with this account. If you have questions, contact Support."},
	ErrorPendingVpcPeeringConnectionLimitExceeded:              {HTTPCode: 409, Message: "You've reached the limit on the number of pending VPC peering connections that you can have."},
	ErrorPlacementGroupLimitExceeded:                           {HTTPCode: 400, Message: "You've reached the limit on the number of placement groups that you can have."},
	ErrorPrivateIpAddressLimitExceeded:                         {HTTPCode: 400, Message: "You've reached the limit on the number of private IP addresses that you can assign to the specified network interface for that type of instance. For more information, see IP addresses per network interface."},
	ErrorRequestEntityTooLarge:                                 {HTTPCode: 413, Message: "Request body exceeds the maximum allowed size."},
	ErrorRequestExpired:                                        {HTTPCode: 403, Message: "The request reached the service more than 15 minutes after the date stamp on the request or more than 15 minutes after the request expiration date (such as for presigned URLs), or the date stamp on the request is more than 15 minutes in the future. If you're using temporary security credentials, this error can also occur if the credentials have expired. For more information, see Temporary security credentials in the IAM User Guide."},
	ErrorRequestLimitExceeded:                                  {HTTPCode: 503, Message: "The maximum request rate permitted by the Amazon EC2 APIs has been exceeded for your account. For best results, use an increasing or variable sleep interval between requests. For more information, see Query API request rate."},
	ErrorRequestResourceCountExceeded:                          {HTTPCode: 409, Message: "Details in your Spot request exceed the numbers allowed by the Spot service in one of the following ways, depending on the action that generated the error: \u2014If you get this error when you submitted a request for Spot Instances, check the number of Spot Instances specified in your request. The number shouldn't exceed the 3,000 maximum allowed per request. Resend your Spot Instance request and specify a number less than 3,000. If your account's regional Spot request limit is greater than 3,000 instances, you can access these instances by submitting multiple smaller requests. \u2014If you get this error when you sent Describe Spot Instance requests, check the number of requests for Spot Instance data, the amount of data you requested, and how often you sent the request. The frequency with which you requested the data combined with the amount of data exceeds the levels allowed by the Spot service. Try again and submit fewer large Describe requests over longer intervals."},
	ErrorReservationCapacityExceeded:                           {HTTPCode: 400, Message: "The targeted Capacity Reservation does not enough available instance capacity to fulfill your request. Either increase the instance capacity for the targeted Capacity Reservation, or target a different Capacity Reservation."},
	ErrorReservedInstancesCountExceeded:                        {HTTPCode: 400, Message: "You've reached the limit for the number of Reserved Instances."},
	ErrorReservedInstancesLimitExceeded:                        {HTTPCode: 400, Message: "Your current quota does not allow you to purchase the required number of Reserved Instances."},
	ErrorReservedInstancesUnavailable:                          {HTTPCode: 400, Message: "The requested Reserved Instances are not available."},
	ErrorResourceAlreadyAssigned:                               {HTTPCode: 400, Message: "The specified private IP address is already assigned to a resource. Unassign the private IP first, or use a different private IP address."},
	ErrorResourceAlreadyAssociated:                             {HTTPCode: 409, Message: "The specified resource is already in use. For example, in EC2-VPC, you cannot associate an Elastic IP address with an instance if it's already associated with another instance. You also cannot attach an internet gateway to more than one VPC at a time."},
	ErrorResourceCountExceeded:                                 {HTTPCode: 400, Message: "You have exceeded the number of resources allowed for this request; for example, if you try to launch more instances than AWS allows in a single request. This limit is separate from your individual resource limit. If you get this error, break up your request into smaller requests; for example, if you are launching 15 instances, try launching 5 instances in 3 separate requests."},
	ErrorResourceCountLimitExceeded:                            {HTTPCode: 400, Message: "You have exceeded a resource limit for creating routes."},
	ErrorResourceLimitExceeded:                                 {HTTPCode: 400, Message: "You have exceeded an Amazon EC2 resource limit. For example, you might have too many snapshot copies in progress."},
	ErrorRetryableError:                                        {HTTPCode: 400, Message: "A request submitted by an AWS service on your behalf could not be completed. The requesting service might automatically retry the request."},
	ErrorRouteAlreadyExists:                                    {HTTPCode: 409, Message: "A route for the specified CIDR block already exists in this route table."},
	ErrorRouteLimitExceeded:                                    {HTTPCode: 400, Message: "You've reached the limit on the number of routes that you can add to a route table."},
	ErrorRouteTableLimitExceeded:                               {HTTPCode: 400, Message: "You've reached the limit on the number of route tables that you can create for the specified VPC. For more information about route table limits, see Amazon VPC quotas."},
	ErrorRulesPerSecurityGroupLimitExceeded:                    {HTTPCode: 400, Message: "You've reached the limit on the number of rules that you can add to a security group. For more information, see Amazon VPC quotas."},
	ErrorScheduledInstanceLimitExceeded:                        {HTTPCode: 400, Message: "You've reached the limit on the number of Scheduled Instances that you can purchase."},
	ErrorScheduledInstanceParameterMismatch:                    {HTTPCode: 400, Message: "The launch specification does not match the details for the Scheduled Instance."},
	ErrorScheduledInstanceSlotNotOpen:                          {HTTPCode: 400, Message: "You can launch a Scheduled Instance only during its scheduled time periods."},
	ErrorScheduledInstanceSlotUnavailable:                      {HTTPCode: 400, Message: "The requested Scheduled Instance is no longer available during this scheduled time period."},
	ErrorSecurityGroupLimitExceeded:                            {HTTPCode: 400, Message: "You've reached the limit on the number of security groups that you can create, or that you can assign to an instance."},
	ErrorSecurityGroupsPerInstanceLimitExceeded:                {HTTPCode: 400, Message: "You've reached the limit on the number of security groups that you can assign to an instance. For more information, see Amazon EC2 security groups."},
	ErrorSecurityGroupsPerInterfaceLimitExceeded:               {HTTPCode: 400, Message: "You've reached the limit on the number of security groups you can associate with the specified network interface. For more information, see Amazon VPC quotas."},
	ErrorSerialConsoleSessionUnavailable:                       {HTTPCode: 403, Message: "The serial console access is not enabled for this account. Use EnableSerialConsoleAccess to enable access."},
	ErrorServerInternal:                                        {HTTPCode: 500, Message: "An internal error has occurred. Retry your request, but if the problem persists, contact us with details by posting a message on AWS re:Post."},
	ErrorServiceUnavailable:                                    {HTTPCode: 503, Message: "The request has failed due to a temporary failure of the server."},
	ErrorSignatureDoesNotMatch:                                 {HTTPCode: 403, Message: "The request signature that Amazon has does not match the signature that you provided. Check your AWS credentials and signing method."},
	ErrorSnapshotCopyUnsupportedInterRegion:                    {HTTPCode: 400, Message: "Inter-region snapshot copy is not supported for this AWS Region."},
	ErrorSnapshotCreationPerVolumeRateExceeded:                 {HTTPCode: 400, Message: "The rate limit for creating concurrent snapshots of an EBS volume has been exceeded. Wait at least 15 seconds between concurrent volume snapshots."},
	ErrorSnapshotLimitExceeded:                                 {HTTPCode: 400, Message: "You've reached the limit on the number of Amazon EBS snapshots that you can create."},
	ErrorSpotMaxPriceTooLow:                                    {HTTPCode: 400, Message: "The request can't be fulfilled yet because your maximum price is below the Spot price. In this case, no instance is launched and your request remains open."},
	ErrorSubnetLimitExceeded:                                   {HTTPCode: 400, Message: "You've reached the limit on the number of subnets that you can create for the specified VPC. For more information, see Amazon VPC quotas."},
	ErrorTagLimitExceeded:                                      {HTTPCode: 400, Message: "You've reached the limit on the number of tags that you can assign to the specified resource. For more information, see Tag restrictions."},
	ErrorTagPolicyViolation:                                    {HTTPCode: 400, Message: "You attempted to create or update a resource with tags that are not compliant with the tag policy requirements for this account. For more information, see Grant permission to tag resources during creation."},
	ErrorThrottling:                                            {HTTPCode: 503, Message: "Rate exceeded."},
	ErrorTargetCapacityLimitExceededException:                  {HTTPCode: 400, Message: "The value for targetCapacity exceeds your limit on the amount of Spot placement target capacity you can explore. Reduce the targetCapacity value, and try again. For more information, see Spot placement score."},
	ErrorTrafficMirrorFilterInUse:                              {HTTPCode: 400, Message: "The Traffic Mirror filter cannot be deleted because a Traffic Mirror session is currently using it."},
	ErrorTrafficMirrorFilterLimitExceeded:                      {HTTPCode: 400, Message: "The maximum number of Traffic Mirror filters has been exceeded."},
	ErrorTrafficMirrorFilterRuleAlreadyExists:                  {HTTPCode: 409, Message: "The Traffic Mirror filter rule already exists."},
	ErrorTrafficMirrorFilterRuleLimitExceeded:                  {HTTPCode: 400, Message: "The maximum number of Traffic Mirror filter rules has been exceeded."},
	ErrorTrafficMirrorSessionLimitExceeded:                     {HTTPCode: 400, Message: "The maximum number of Traffic Mirror sessions has been exceeded."},
	ErrorTrafficMirrorSessionsPerInterfaceLimitExceeded:        {HTTPCode: 400, Message: "The allowed number of Traffic Mirror sessions for the specified network interface has been exceeded."},
	ErrorTrafficMirrorSessionsPerTargetLimitExceeded:           {HTTPCode: 400, Message: "The maximum number of Traffic Mirror sessions for the specified Traffic Mirror target has been exceeded."},
	ErrorTrafficMirrorSourcesPerTargetLimitExceeded:            {HTTPCode: 400, Message: "The maximum number of Traffic Mirror sources for the specified Traffic Mirror target has been exceeded."},
	ErrorTrafficMirrorTargetInUseException:                     {HTTPCode: 400, Message: "The Traffic Mirror target cannot be deleted because a Traffic Mirror session is currently using it."},
	ErrorTrafficMirrorTargetLimitExceeded:                      {HTTPCode: 400, Message: "The maximum number of Traffic Mirror targets has been exceeded."},
	ErrorUnauthorizedOperation:                                 {HTTPCode: 403, Message: "You are not authorized to perform this operation. Check your IAM policies, and ensure that you are using the correct credentials. For more information, see Identity and access management for Amazon EC2. If the returned message is encoded, you can decode it using the DecodeAuthorizationMessage action. For more information, see DecodeAuthorizationMessage in the AWS Security Token Service API Reference."},
	ErrorUnavailable:                                           {HTTPCode: 503, Message: "The server is overloaded and can't handle the request."},
	ErrorUnavailableHostRequirements:                           {HTTPCode: 400, Message: "There are no valid Dedicated Hosts available on which you can launch an instance."},
	ErrorUnfulfillableCapacity:                                 {HTTPCode: 400, Message: "At this time there isn't enough spare capacity to fulfill your request for Spot Instances. You can wait a few minutes to see whether capacity becomes available for your request. Alternatively, create a more flexible request. For example, include additional instance types, include additional Availability Zones, or use the capacity-optimized allocation strategy."},
	ErrorUnknownParameter:                                      {HTTPCode: 404, Message: "An unknown or unrecognized parameter was supplied. Requests that could cause this error include supplying a misspelled parameter or a parameter that is not supported for the specified API version."},
	ErrorUnknownPrincipalTypeUnsupported:                       {HTTPCode: 400, Message: "The principal type is not supported."},
	ErrorUnknownVolumeType:                                     {HTTPCode: 400, Message: "The specified volume type is unsupported. The supported volume types are gp2, io1, st1, sc1, and standard."},
	ErrorUnsupported:                                           {HTTPCode: 400, Message: "The specified request is unsupported. For example, you might be trying to launch an instance in an Availability Zone that currently has constraints on that instance type. The returned message provides details of the unsupported request."},
	ErrorUnsupportedException:                                  {HTTPCode: 400, Message: "Capacity Reservations are not supported for this Region."},
	ErrorUnsupportedHibernationConfiguration:                   {HTTPCode: 400, Message: "The instance could not be launched because one or more parameter values do not meet the prerequisites for enabling hibernation. For more information, see Hibernation Prerequisites. Alternatively, the instance could not be hibernated because it is not enabled for hibernation."},
	ErrorUnsupportedHostConfiguration:                          {HTTPCode: 400, Message: "The specified Dedicated Host configuration is unsupported. For more information about supported configurations, see Dedicated Hosts."},
	ErrorUnsupportedInstanceAttribute:                          {HTTPCode: 400, Message: "The specified attribute cannot be modified."},
	ErrorUnsupportedInstanceTypeOnHost:                         {HTTPCode: 400, Message: "The instance type is not supported on the Dedicated Host. For more information about supported instance types, see Amazon EC2 Dedicated Hosts Pricing."},
	ErrorUnsupportedOperation:                                  {HTTPCode: 400, Message: "The specified request includes an unsupported operation. For example, you can't stop an instance that's instance store-backed. Or you might be trying to launch an instance type that is not supported by the specified AMI. The returned message provides details of the unsupported operation."},
	ErrorUnsupportedProtocol:                                   {HTTPCode: 400, Message: "SOAP has been deprecated and is no longer supported."},
	ErrorUnsupportedTenancy:                                    {HTTPCode: 400, Message: "The specified tenancy is unsupported. You can change the tenancy of a VPC to default only."},
	ErrorUpdateLimitExceeded:                                   {HTTPCode: 400, Message: "The default credit specification for an instance family can be modified only once in a rolling 5-minute period, and up to four times in a rolling 24-hour period. For more information, see Set the default credit specification for the account."},
	ErrorVPCIdNotSpecified:                                     {HTTPCode: 400, Message: "You have no default VPC in which to carry out the request. Specify a VPC or subnet ID or, in the case of security groups, specify the ID and not the security group name. If you deleted your default VPC, you can create a new one. For more information, see Create a default VPC."},
	ErrorVPCResourceNotSpecified:                               {HTTPCode: 400, Message: "The specified resource can be used only in a VPC; for example, T2 instances. Ensure that you have a VPC in your account, and then specify a subnet ID or network interface ID in the request."},
	ErrorValidationError:                                       {HTTPCode: 400, Message: "The input fails to satisfy the constraints specified by an AWS service."},
	ErrorVcpuLimitExceeded:                                     {HTTPCode: 400, Message: "You've reached the limit on the number of vCPUs (virtual processing units) assigned to the running instances in your account. You are limited to running one or more On-Demand instances in an AWS account, and Amazon EC2 measures usage towards each limit based on the total number of vCPUs that are assigned to the running On-Demand instances in your AWS account. If your request fails due to limit constraints, increase your On-Demand instance limits and try again. For more information, see EC2 On-Demand instance limits."},
	ErrorVolumeIOPSLimit:                                       {HTTPCode: 400, Message: "The maximum IOPS limit for the volume has been reached. For more information, see Amazon EBS volume types."},
	ErrorVolumeInUse:                                           {HTTPCode: 400, Message: "The specified Amazon EBS volume is attached to an instance. Ensure that the specified volume is in an \u2018available\u2019 state."},
	ErrorVolumeLimitExceeded:                                   {HTTPCode: 400, Message: "You've reached the limit on your Amazon EBS volume storage. For more information, see Quotas for Amazon EBS."},
	ErrorVolumeModificationSizeLimitExceeded:                   {HTTPCode: 400, Message: "You've reached the limit on your Amazon EBS volume modification storage in this Region. For more information, see Quotas for Amazon EBS."},
	ErrorVolumeTypeNotAvailableInZone:                          {HTTPCode: 400, Message: "The specified Availability Zone does not support Provisioned IOPS SSD volumes. Try launching your instance in a different Availability Zone, or don't specify a zone in the request. If you're creating a volume, try specifying a different Availability Zone in the request."},
	ErrorVpcEndpointLimitExceeded:                              {HTTPCode: 400, Message: "You've reached the limit on the number of VPC endpoints that you can create in the AWS Region. For more information, see Amazon VPC quotas."},
	ErrorVpcLimitExceeded:                                      {HTTPCode: 400, Message: "You've reached the limit on the number of VPCs that you can create in the AWS Region. For more information about VPC limits, see Amazon VPC quotas."},
	ErrorVpcPeeringConnectionAlreadyExists:                     {HTTPCode: 409, Message: "A VPC peering connection between the VPCs already exists."},
	ErrorVpcPeeringConnectionsPerVpcLimitExceeded:              {HTTPCode: 400, Message: "You've reached the limit on the number of VPC peering connections that you can have per VPC. For more information, see Amazon VPC quotas."},
	ErrorVpnConnectionLimitExceeded:                            {HTTPCode: 400, Message: "You've reached the limit on the number of VPN connections that you can create. For more information, see Amazon VPC quotas."},
	ErrorVpnGatewayAttachmentLimitExceeded:                     {HTTPCode: 400, Message: "You've reached the limit on the number of VPCs that can be attached to the specified virtual private gateway."},
	ErrorVpnGatewayLimitExceeded:                               {HTTPCode: 400, Message: "You've reached the limit on the number of virtual private gateways that you can create. For more information about limits, see Amazon VPC quotas."},
	ErrorZonesMismatched:                                       {HTTPCode: 400, Message: "The Availability Zone for the instance does not match that of the Dedicated Host"},

	// IAM error codes
	ErrorIAMNoSuchEntity:            {HTTPCode: 404, Message: "The request was rejected because it referenced a resource entity that does not exist."},
	ErrorIAMEntityAlreadyExists:     {HTTPCode: 409, Message: "The request was rejected because it attempted to create a resource that already exists."},
	ErrorIAMDeleteConflict:          {HTTPCode: 409, Message: "The request was rejected because it attempted to delete a resource that has attached subordinate entities."},
	ErrorIAMLimitExceeded:           {HTTPCode: 409, Message: "The request was rejected because it attempted to create resources beyond the current AWS account limits."},
	ErrorIAMMalformedPolicyDocument: {HTTPCode: 400, Message: "The policy document is malformed."},
	ErrorAccessDenied:               {HTTPCode: 403, Message: "User is not authorized to perform this action."},

	// ELBv2 error codes
	ErrorELBv2LoadBalancerNotFound:         {HTTPCode: 400, Message: "One or more load balancers not found."},
	ErrorELBv2TargetGroupNotFound:          {HTTPCode: 400, Message: "One or more target groups not found."},
	ErrorELBv2ListenerNotFound:             {HTTPCode: 400, Message: "One or more listeners not found."},
	ErrorELBv2DuplicateLoadBalancer:        {HTTPCode: 400, Message: "A load balancer with the same name already exists."},
	ErrorELBv2DuplicateTargetGroup:         {HTTPCode: 400, Message: "A target group with the same name already exists."},
	ErrorELBv2DuplicateListener:            {HTTPCode: 400, Message: "A listener with the specified port already exists on this load balancer."},
	ErrorELBv2TooManyLoadBalancers:         {HTTPCode: 400, Message: "You've reached the limit on the number of load balancers for your account."},
	ErrorELBv2TooManyTargetGroups:          {HTTPCode: 400, Message: "You've reached the limit on the number of target groups for your account."},
	ErrorELBv2TooManyListeners:             {HTTPCode: 400, Message: "You've reached the limit on the number of listeners per load balancer."},
	ErrorELBv2TooManyTargets:               {HTTPCode: 400, Message: "You've reached the limit on the number of times a target can be registered with a target group."},
	ErrorELBv2InvalidTarget:                {HTTPCode: 400, Message: "The specified target does not exist, is not in the same VPC as the target group, or has an unsupported instance type."},
	ErrorELBv2TargetGroupInUse:             {HTTPCode: 400, Message: "The target group is currently in use by a listener or rule."},
	ErrorELBv2InvalidSecurityGroup:         {HTTPCode: 400, Message: "The specified security group does not exist."},
	ErrorELBv2InvalidScheme:                {HTTPCode: 400, Message: "The specified scheme is not valid. Specify 'internet-facing' or 'internal'."},
	ErrorELBv2SubnetNotFound:               {HTTPCode: 400, Message: "The specified subnet does not exist."},
	ErrorELBv2AvailabilityZoneNotSupported: {HTTPCode: 400, Message: "The specified Availability Zone is not supported."},
	ErrorELBv2InvalidConfigurationRequest:  {HTTPCode: 400, Message: "Security groups are not supported for load balancers with type 'network'."},
}
