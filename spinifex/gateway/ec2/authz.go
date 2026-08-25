// Package gateway_ec2 resolves the resource ARNs an EC2 request authorizes
// against. Without it every EC2 action is evaluated against the literal "*", so
// a resource-scoped Deny never fires and a resource-scoped Allow never grants.
package gateway_ec2

import (
	"errors"
	"sort"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awsec2query"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// The resource a policy check evaluates against when the request names nothing
// in particular: the describes, and the actions whose identifier cannot be
// resolved without a lookup the gate cannot do.
const anyResource = "*"

// Where an action's identifier lives in its parsed SDK input and what it
// resolves to. Paths are tried in order, matching handler precedence. No paths
// means kind is a resource the action creates, whose id does not exist yet.
type resourceScope struct {
	paths    []string
	kind     arn.EC2ResourceType
	byPrefix bool
	optional bool
}

// Scopes shared across actions. Names ending in New resolve to <type>/*, which
// is what AWS evaluates for a resource the call is about to create.
//
// Where an action accepts a name as well as an id, only the id is read:
// GroupName is EC2-Classic, PublicIp is not an allocation id, and a launch
// template ARN carries the template id. A name-only request leaves the resource
// unresolved rather than building an ARN that names nothing.
var (
	instanceListScope = &resourceScope{paths: []string{"InstanceIds"}, kind: arn.EC2Instance}
	instanceScope     = &resourceScope{paths: []string{"InstanceId"}, kind: arn.EC2Instance}
	instanceOptScope  = &resourceScope{paths: []string{"InstanceId"}, kind: arn.EC2Instance, optional: true}
	instanceNewScope  = &resourceScope{kind: arn.EC2Instance}

	volumeScope    = &resourceScope{paths: []string{"VolumeId"}, kind: arn.EC2Volume}
	volumeNewScope = &resourceScope{kind: arn.EC2Volume}

	imageScope    = &resourceScope{paths: []string{"ImageId"}, kind: arn.EC2Image}
	imageNewScope = &resourceScope{kind: arn.EC2Image}

	snapshotScope    = &resourceScope{paths: []string{"SnapshotId"}, kind: arn.EC2Snapshot}
	snapshotOptScope = &resourceScope{paths: []string{"SnapshotId"}, kind: arn.EC2Snapshot, optional: true}
	snapshotNewScope = &resourceScope{kind: arn.EC2Snapshot}

	vpcScope    = &resourceScope{paths: []string{"VpcId"}, kind: arn.EC2VPC}
	vpcOptScope = &resourceScope{paths: []string{"VpcId"}, kind: arn.EC2VPC, optional: true}
	vpcNewScope = &resourceScope{kind: arn.EC2VPC}

	subnetScope       = &resourceScope{paths: []string{"SubnetId"}, kind: arn.EC2Subnet}
	subnetOptScope    = &resourceScope{paths: []string{"SubnetId"}, kind: arn.EC2Subnet, optional: true}
	runSubnetOptScope = &resourceScope{paths: []string{"SubnetId", "NetworkInterfaces.SubnetId"}, kind: arn.EC2Subnet, optional: true}
	subnetNewScope    = &resourceScope{kind: arn.EC2Subnet}

	securityGroupScope    = &resourceScope{paths: []string{"GroupId"}, kind: arn.EC2SecurityGroup}
	securityGroupsOptList = &resourceScope{paths: []string{"SecurityGroupIds", "Groups"}, kind: arn.EC2SecurityGroup, optional: true}
	runSecurityGroups     = &resourceScope{paths: []string{"SecurityGroupIds", "NetworkInterfaces.Groups"}, kind: arn.EC2SecurityGroup, optional: true}
	securityGroupNewScope = &resourceScope{kind: arn.EC2SecurityGroup}

	routeTableScope    = &resourceScope{paths: []string{"RouteTableId"}, kind: arn.EC2RouteTable}
	routeTableNewScope = &resourceScope{kind: arn.EC2RouteTable}

	igwScope    = &resourceScope{paths: []string{"InternetGatewayId"}, kind: arn.EC2InternetGateway}
	igwNewScope = &resourceScope{kind: arn.EC2InternetGateway}

	eigwScope    = &resourceScope{paths: []string{"EgressOnlyInternetGatewayId"}, kind: arn.EC2EgressOnlyInternetGateway}
	eigwNewScope = &resourceScope{kind: arn.EC2EgressOnlyInternetGateway}

	eniScope    = &resourceScope{paths: []string{"NetworkInterfaceId"}, kind: arn.EC2NetworkInterface}
	eniOptScope = &resourceScope{paths: []string{"NetworkInterfaceId"}, kind: arn.EC2NetworkInterface, optional: true}
	eniNewScope = &resourceScope{kind: arn.EC2NetworkInterface}

	addressScope    = &resourceScope{paths: []string{"AllocationId"}, kind: arn.EC2ElasticIP}
	addressOptScope = &resourceScope{paths: []string{"AllocationId"}, kind: arn.EC2ElasticIP, optional: true}
	addressNewScope = &resourceScope{kind: arn.EC2ElasticIP}

	natGatewayScope    = &resourceScope{paths: []string{"NatGatewayId"}, kind: arn.EC2NATGateway}
	natGatewayOptScope = &resourceScope{paths: []string{"NatGatewayId"}, kind: arn.EC2NATGateway, optional: true}
	natGatewayNewScope = &resourceScope{kind: arn.EC2NATGateway}

	keyPairScope    = &resourceScope{paths: []string{"KeyPairId", "KeyName"}, kind: arn.EC2KeyPair}
	keyPairOptScope = &resourceScope{paths: []string{"KeyName"}, kind: arn.EC2KeyPair, optional: true}

	placementGroupScope = &resourceScope{paths: []string{"GroupName"}, kind: arn.EC2PlacementGroup}

	launchTemplateScope    = &resourceScope{paths: []string{"LaunchTemplateId"}, kind: arn.EC2LaunchTemplate}
	launchTemplateNewScope = &resourceScope{kind: arn.EC2LaunchTemplate}

	capacityReservationScope    = &resourceScope{paths: []string{"CapacityReservationId"}, kind: arn.EC2CapacityReservation}
	capacityReservationNewScope = &resourceScope{kind: arn.EC2CapacityReservation}

	spotRequestListScope = &resourceScope{paths: []string{"SpotInstanceRequestIds"}, kind: arn.EC2SpotInstancesRequest}
	spotRequestNewScope  = &resourceScope{kind: arn.EC2SpotInstancesRequest}
	taggedResourcesScope = &resourceScope{paths: []string{"Resources"}, byPrefix: true}
	gatewayOptScope      = &resourceScope{paths: []string{"GatewayId"}, byPrefix: true, optional: true}

	spotImageScope  = &resourceScope{paths: []string{"LaunchSpecification.ImageId"}, kind: arn.EC2Image}
	spotSubnetScope = &resourceScope{
		paths:    []string{"LaunchSpecification.SubnetId", "LaunchSpecification.NetworkInterfaces.SubnetId"},
		kind:     arn.EC2Subnet,
		optional: true,
	}
	spotSecurityGroups = &resourceScope{
		paths:    []string{"LaunchSpecification.SecurityGroupIds", "LaunchSpecification.NetworkInterfaces.Groups"},
		kind:     arn.EC2SecurityGroup,
		optional: true,
	}
	spotKeyPairScope = &resourceScope{paths: []string{"LaunchSpecification.KeyName"}, kind: arn.EC2KeyPair, optional: true}
)

// unscoped is the explicit "this action authorizes account-wide" entry. It is a
// value in its own right, not a missing row: the completeness test cannot tell
// a sparse table from an incomplete one, so every action carries an entry.
var unscoped = []*resourceScope{{}}

// ec2Scopes covers every action in the gateway's EC2 dispatch table. An action
// missing from here is a bug, not an unscoped action — see ResourceARNs.
var ec2Scopes = map[string][]*resourceScope{
	// Instances.
	"RunInstances":                  {instanceNewScope, volumeNewScope, imageScope, runSubnetOptScope, runSecurityGroups, keyPairOptScope},
	"StartInstances":                {instanceListScope},
	"StopInstances":                 {instanceListScope},
	"RebootInstances":               {instanceListScope},
	"TerminateInstances":            {instanceListScope},
	"MonitorInstances":              {instanceListScope},
	"UnmonitorInstances":            {instanceListScope},
	"ModifyInstanceAttribute":       {instanceScope},
	"ModifyInstanceMetadataOptions": {instanceScope},
	"GetConsoleOutput":              {instanceScope},
	"GetPasswordData":               {instanceScope},
	"AssociateIamInstanceProfile":   {instanceScope},

	// The association id needs a NATS lookup to reach the instance behind it, so
	// a Deny scoped to that instance stays inert.
	"DisassociateIamInstanceProfile":       unscoped,
	"ReplaceIamInstanceProfileAssociation": unscoped,

	// Volumes and snapshots.
	"CreateVolume":   {volumeNewScope, snapshotOptScope},
	"DeleteVolume":   {volumeScope},
	"ModifyVolume":   {volumeScope},
	"AttachVolume":   {volumeScope, instanceScope},
	"DetachVolume":   {volumeScope, instanceOptScope},
	"CreateSnapshot": {snapshotNewScope, volumeScope},
	"DeleteSnapshot": {snapshotScope},
	// The source snapshot lives in SourceRegion, so an ARN built from gw.Region
	// would name a resource in the wrong region.
	"CopySnapshot": {snapshotNewScope},

	// Images.
	"CreateImage":          {imageNewScope, instanceScope},
	"RegisterImage":        {imageNewScope},
	"DeregisterImage":      {imageScope},
	"ModifyImageAttribute": {imageScope},
	"ResetImageAttribute":  {imageScope},
	// Source image is in SourceRegion, as with CopySnapshot.
	"CopyImage": {imageNewScope},

	// Key pairs.
	"CreateKeyPair": {keyPairScope},
	"ImportKeyPair": {keyPairScope},
	"DeleteKeyPair": {keyPairScope},

	// VPCs and subnets.
	"CreateVpc":             {vpcNewScope},
	"DeleteVpc":             {vpcScope},
	"ModifyVpcAttribute":    {vpcScope},
	"CreateSubnet":          {subnetNewScope, vpcScope},
	"DeleteSubnet":          {subnetScope},
	"ModifySubnetAttribute": {subnetScope},

	// Route tables.
	"CreateRouteTable":             {routeTableNewScope, vpcScope},
	"DeleteRouteTable":             {routeTableScope},
	"CreateRoute":                  {routeTableScope, gatewayOptScope, natGatewayOptScope},
	"ReplaceRoute":                 {routeTableScope, gatewayOptScope},
	"DeleteRoute":                  {routeTableScope},
	"AssociateRouteTable":          {routeTableScope, subnetOptScope, gatewayOptScope},
	"ReplaceRouteTableAssociation": {routeTableScope},
	// Carries only an association id, which needs a lookup to reach the table.
	"DisassociateRouteTable": unscoped,

	// Gateways.
	"CreateInternetGateway":           {igwNewScope},
	"DeleteInternetGateway":           {igwScope},
	"AttachInternetGateway":           {igwScope, vpcScope},
	"DetachInternetGateway":           {igwScope, vpcScope},
	"CreateEgressOnlyInternetGateway": {eigwNewScope, vpcScope},
	"DeleteEgressOnlyInternetGateway": {eigwScope},
	"CreateNatGateway":                {natGatewayNewScope, subnetScope, addressOptScope},
	"DeleteNatGateway":                {natGatewayScope},

	// Network interfaces.
	"CreateNetworkInterface":          {eniNewScope, subnetScope, securityGroupsOptList},
	"DeleteNetworkInterface":          {eniScope},
	"ModifyNetworkInterfaceAttribute": {eniScope},
	"AttachNetworkInterface":          {eniScope, instanceScope},
	// Carries only an attachment id, which needs a lookup to reach the interface.
	"DetachNetworkInterface": unscoped,

	// Security groups.
	"CreateSecurityGroup":           {securityGroupNewScope, vpcOptScope},
	"DeleteSecurityGroup":           {securityGroupScope},
	"AuthorizeSecurityGroupIngress": {securityGroupScope},
	"AuthorizeSecurityGroupEgress":  {securityGroupScope},
	"RevokeSecurityGroupIngress":    {securityGroupScope},
	"RevokeSecurityGroupEgress":     {securityGroupScope},

	// Addresses.
	"AllocateAddress":  {addressNewScope},
	"ReleaseAddress":   {addressScope},
	"AssociateAddress": {addressScope, instanceOptScope, eniOptScope},
	// Carries an eipassoc- id, which needs a lookup to reach the address.
	"DisassociateAddress": unscoped,

	// Placement groups.
	"CreatePlacementGroup": {placementGroupScope},
	"DeletePlacementGroup": {placementGroupScope},

	// Launch templates.
	"CreateLaunchTemplate":         {launchTemplateNewScope},
	"CreateLaunchTemplateVersion":  {launchTemplateScope},
	"ModifyLaunchTemplate":         {launchTemplateScope},
	"DeleteLaunchTemplate":         {launchTemplateScope},
	"DeleteLaunchTemplateVersions": {launchTemplateScope},

	// Capacity reservations and spot.
	"CreateCapacityReservation":  {capacityReservationNewScope},
	"CancelCapacityReservation":  {capacityReservationScope},
	"RequestSpotInstances":       {spotRequestNewScope, instanceNewScope, volumeNewScope, spotImageScope, spotSubnetScope, spotSecurityGroups, spotKeyPairScope},
	"CancelSpotInstanceRequests": {spotRequestListScope},

	// Tags. The type comes from each id's prefix; an unrecognised prefix has no
	// correct ARN, so it contributes "*" rather than a plausible-looking one.
	"CreateTags": {taggedResourcesScope},
	"DeleteTags": {taggedResourcesScope},

	// Account-level settings: no resource to name.
	"EnableEbsEncryptionByDefault":  unscoped,
	"DisableEbsEncryptionByDefault": unscoped,
	"GetEbsEncryptionByDefault":     unscoped,
	"EnableSerialConsoleAccess":     unscoped,
	"DisableSerialConsoleAccess":    unscoped,
	"GetSerialConsoleAccessStatus":  unscoped,

	// Describes. "*" is fidelity, not a stub: EC2 describe actions do not
	// support resource-level permissions in AWS either.
	"DescribeAccountAttributes":              unscoped,
	"DescribeAddresses":                      unscoped,
	"DescribeAddressesAttribute":             unscoped,
	"DescribeAvailabilityZones":              unscoped,
	"DescribeCapacityReservations":           unscoped,
	"DescribeEgressOnlyInternetGateways":     unscoped,
	"DescribeIamInstanceProfileAssociations": unscoped,
	"DescribeImageAttribute":                 unscoped,
	"DescribeImages":                         unscoped,
	"DescribeInstanceAttribute":              unscoped,
	"DescribeInstanceCreditSpecifications":   unscoped,
	"DescribeInstanceStatus":                 unscoped,
	"DescribeInstanceTypes":                  unscoped,
	"DescribeInstances":                      unscoped,
	"DescribeInternetGateways":               unscoped,
	"DescribeKeyPairs":                       unscoped,
	"DescribeLaunchTemplateVersions":         unscoped,
	"DescribeLaunchTemplates":                unscoped,
	"DescribeNatGateways":                    unscoped,
	"DescribeNetworkInterfaces":              unscoped,
	"DescribePlacementGroups":                unscoped,
	"DescribeRegions":                        unscoped,
	"DescribeRouteTables":                    unscoped,
	"DescribeSecurityGroupRules":             unscoped,
	"DescribeSecurityGroups":                 unscoped,
	"DescribeSnapshots":                      unscoped,
	"DescribeSpotInstanceRequests":           unscoped,
	"DescribeSubnets":                        unscoped,
	"DescribeTags":                           unscoped,
	"DescribeVolumeStatus":                   unscoped,
	"DescribeVolumes":                        unscoped,
	"DescribeVolumesModifications":           unscoped,
	"DescribeVpcAttribute":                   unscoped,
	"DescribeVpcs":                           unscoped,
}

// HasScope reports whether the action has a scope table entry. ec2Actions lives
// in package gateway, so its completeness test needs this.
func HasScope(action string) bool {
	_, ok := ec2Scopes[action]
	return ok
}

// ScopedActions returns every action the scope table covers, so a scope left
// behind by a deleted or renamed action fails the completeness test too.
func ScopedActions() []string {
	actions := make([]string, 0, len(ec2Scopes))
	for action := range ec2Scopes {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	return actions
}

// ResourceARNs builds every resource the action's policy check evaluates from
// the same parsed SDK input the handler receives. A missing action is a bug.
func ResourceARNs(action, region, accountID string, input any) ([]string, error) {
	scopes, ok := ec2Scopes[action]
	if !ok {
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}

	resources := make([]string, 0, len(scopes))
	seen := make(map[string]struct{})
	for _, scope := range scopes {
		resolved, err := scope.resolve(region, accountID, input)
		if err != nil {
			return nil, err
		}
		for _, resource := range resolved {
			if _, ok := seen[resource]; ok {
				continue
			}
			seen[resource] = struct{}{}
			resources = append(resources, resource)
			if len(resources) > awsec2query.MaxSliceLen {
				return nil, errors.New(awserrors.ErrorMalformedQueryString)
			}
		}
	}
	if len(resources) == 0 {
		return []string{anyResource}, nil
	}
	return resources, nil
}

// resolve returns the ARNs one scope contributes, which may be none.
func (s *resourceScope) resolve(region, accountID string, input any) ([]string, error) {
	if len(s.paths) == 0 {
		if s.kind == "" {
			return []string{anyResource}, nil
		}
		return []string{arn.FormatEC2(s.kind, region, accountID, anyResource)}, nil
	}

	ids := s.identifiers(input)
	if len(ids) == 0 {
		if s.optional {
			return nil, nil
		}
		return []string{anyResource}, nil
	}
	if len(ids) > awsec2query.MaxSliceLen {
		return nil, errors.New(awserrors.ErrorMalformedQueryString)
	}

	resources := make([]string, 0, len(ids))
	for _, id := range ids {
		kind := s.kind
		if s.byPrefix {
			resolved, ok := arn.EC2TypeForID(id)
			if !ok {
				resources = append(resources, anyResource)
				continue
			}
			kind = resolved
		}
		resources = append(resources, arn.FormatEC2(kind, region, accountID, id))
	}
	return resources, nil
}

// identifiers reads the first populated field path, matching handler
// precedence. Values are deduplicated before policy evaluation.
func (s *resourceScope) identifiers(input any) []string {
	for _, path := range s.paths {
		values := awsec2query.StringValuesAt(input, path)
		seen := make(map[string]struct{}, len(values))
		ids := make([]string, 0, len(values))
		for _, value := range values {
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			ids = append(ids, value)
		}
		if len(ids) > 0 {
			return ids
		}
	}
	return nil
}
