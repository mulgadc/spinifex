// Package gateway_ec2 resolves the resource ARNs an EC2 request authorizes
// against. Without it every EC2 action is evaluated against the literal "*", so
// a resource-scoped Deny never fires and a resource-scoped Allow never grants.
package gateway_ec2

import (
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// The resource a policy check evaluates against when the request names nothing
// in particular: the describes, and the actions whose identifier cannot be
// resolved without a lookup the gate cannot do.
const anyResource = "*"

// Where an action's identifier lives and what it resolves to. Actions carry one
// scope per resource IAM evaluates, matching AWS.
//
// params is tried in order, covering both the aliases awsec2query accepts for
// one field (Ids/Id) and the alternatives that name the same resource
// (KeyName/KeyPairId). No params means kind is the resource the action creates,
// whose id does not exist yet. An optional scope drops out when the request
// does not name it; a required one widens to "*" instead.
type resourceScope struct {
	params   []string
	kind     arn.EC2ResourceType
	list     bool
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
	instanceListScope = &resourceScope{params: []string{"InstanceIds", "InstanceId"}, kind: arn.EC2Instance, list: true}
	instanceScope     = &resourceScope{params: []string{"InstanceId"}, kind: arn.EC2Instance}
	instanceOptScope  = &resourceScope{params: []string{"InstanceId"}, kind: arn.EC2Instance, optional: true}
	instanceNewScope  = &resourceScope{kind: arn.EC2Instance}

	volumeScope    = &resourceScope{params: []string{"VolumeId"}, kind: arn.EC2Volume}
	volumeNewScope = &resourceScope{kind: arn.EC2Volume}

	imageScope    = &resourceScope{params: []string{"ImageId"}, kind: arn.EC2Image}
	imageNewScope = &resourceScope{kind: arn.EC2Image}

	snapshotScope    = &resourceScope{params: []string{"SnapshotId"}, kind: arn.EC2Snapshot}
	snapshotOptScope = &resourceScope{params: []string{"SnapshotId"}, kind: arn.EC2Snapshot, optional: true}
	snapshotNewScope = &resourceScope{kind: arn.EC2Snapshot}

	vpcScope    = &resourceScope{params: []string{"VpcId"}, kind: arn.EC2VPC}
	vpcOptScope = &resourceScope{params: []string{"VpcId"}, kind: arn.EC2VPC, optional: true}
	vpcNewScope = &resourceScope{kind: arn.EC2VPC}

	subnetScope    = &resourceScope{params: []string{"SubnetId"}, kind: arn.EC2Subnet}
	subnetOptScope = &resourceScope{params: []string{"SubnetId"}, kind: arn.EC2Subnet, optional: true}
	subnetNewScope = &resourceScope{kind: arn.EC2Subnet}

	securityGroupScope    = &resourceScope{params: []string{"GroupId"}, kind: arn.EC2SecurityGroup}
	securityGroupsOptList = &resourceScope{params: []string{"SecurityGroupIds", "SecurityGroupId", "Groups"}, kind: arn.EC2SecurityGroup, list: true, optional: true}
	securityGroupNewScope = &resourceScope{kind: arn.EC2SecurityGroup}

	routeTableScope    = &resourceScope{params: []string{"RouteTableId"}, kind: arn.EC2RouteTable}
	routeTableNewScope = &resourceScope{kind: arn.EC2RouteTable}

	igwScope    = &resourceScope{params: []string{"InternetGatewayId"}, kind: arn.EC2InternetGateway}
	igwNewScope = &resourceScope{kind: arn.EC2InternetGateway}

	eigwScope    = &resourceScope{params: []string{"EgressOnlyInternetGatewayId"}, kind: arn.EC2EgressOnlyInternetGateway}
	eigwNewScope = &resourceScope{kind: arn.EC2EgressOnlyInternetGateway}

	eniScope    = &resourceScope{params: []string{"NetworkInterfaceId"}, kind: arn.EC2NetworkInterface}
	eniOptScope = &resourceScope{params: []string{"NetworkInterfaceId"}, kind: arn.EC2NetworkInterface, optional: true}
	eniNewScope = &resourceScope{kind: arn.EC2NetworkInterface}

	addressScope    = &resourceScope{params: []string{"AllocationId"}, kind: arn.EC2ElasticIP}
	addressOptScope = &resourceScope{params: []string{"AllocationId"}, kind: arn.EC2ElasticIP, optional: true}
	addressNewScope = &resourceScope{kind: arn.EC2ElasticIP}

	natGatewayScope    = &resourceScope{params: []string{"NatGatewayId"}, kind: arn.EC2NATGateway}
	natGatewayNewScope = &resourceScope{kind: arn.EC2NATGateway}

	keyPairScope    = &resourceScope{params: []string{"KeyName", "KeyPairId"}, kind: arn.EC2KeyPair}
	keyPairOptScope = &resourceScope{params: []string{"KeyName"}, kind: arn.EC2KeyPair, optional: true}

	placementGroupScope = &resourceScope{params: []string{"GroupName"}, kind: arn.EC2PlacementGroup}

	launchTemplateScope    = &resourceScope{params: []string{"LaunchTemplateId"}, kind: arn.EC2LaunchTemplate}
	launchTemplateNewScope = &resourceScope{kind: arn.EC2LaunchTemplate}

	capacityReservationScope    = &resourceScope{params: []string{"CapacityReservationId"}, kind: arn.EC2CapacityReservation}
	capacityReservationNewScope = &resourceScope{kind: arn.EC2CapacityReservation}

	spotRequestListScope    = &resourceScope{params: []string{"SpotInstanceRequestIds", "SpotInstanceRequestId"}, kind: arn.EC2SpotInstancesRequest, list: true}
	spotRequestNewScope     = &resourceScope{kind: arn.EC2SpotInstancesRequest}
	taggedResourcesScope    = &resourceScope{params: []string{"Resources", "ResourceId", "resourceId"}, list: true, byPrefix: true}
	gatewayByPrefixOptScope = &resourceScope{params: []string{"GatewayId"}, byPrefix: true, optional: true}
)

// unscoped is the explicit "this action authorizes account-wide" entry. It is a
// value in its own right, not a missing row: the completeness test cannot tell
// a sparse table from an incomplete one, so every action carries an entry.
var unscoped = []*resourceScope{{}}

// ec2Scopes covers every action in the gateway's EC2 dispatch table. An action
// missing from here is a bug, not an unscoped action — see ResourceARNs.
var ec2Scopes = map[string][]*resourceScope{
	// Instances.
	"RunInstances":                  {instanceNewScope, volumeNewScope, imageScope, subnetOptScope, securityGroupsOptList, keyPairOptScope},
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
	"CreateRoute":                  {routeTableScope, gatewayByPrefixOptScope},
	"ReplaceRoute":                 {routeTableScope, gatewayByPrefixOptScope},
	"DeleteRoute":                  {routeTableScope},
	"AssociateRouteTable":          {routeTableScope, subnetOptScope, gatewayByPrefixOptScope},
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
	"RequestSpotInstances":       {spotRequestNewScope},
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

// ResourceARNs builds every resource the action's policy check evaluates.
// Unlike RDS there is no "action missing from the table" fallback to "*": the
// table is exhaustive and the completeness test proves it, so a missing entry
// is a programming error rather than a shape to tolerate. The dispatcher has
// already rejected an unknown action one line above the check.
func ResourceARNs(action, region, accountID string, q map[string]string) ([]string, error) {
	scopes, ok := ec2Scopes[action]
	// Rejected here too, not just by the dispatcher: a caller that skipped the
	// action check must not get a resource back for ec2:<garbage>.
	if !ok {
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}

	resources := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		resources = append(resources, scope.resolve(region, accountID, q)...)
	}
	// Every member was optional and absent, so the request names nothing.
	if len(resources) == 0 {
		return []string{anyResource}, nil
	}
	return resources, nil
}

// resolve returns the ARNs one scope contributes, which may be none.
func (s *resourceScope) resolve(region, accountID string, q map[string]string) []string {
	if len(s.params) == 0 {
		if s.kind == "" {
			return []string{anyResource}
		}
		// The resource the call creates has no id yet, which is the ARN AWS
		// evaluates a create against.
		return []string{arn.FormatEC2(s.kind, region, accountID, anyResource)}
	}

	ids := s.identifiers(q)
	if len(ids) == 0 {
		// A missing required member widens to "*" rather than failing here, so a
		// malformed request stays the handler's validation fault.
		if s.optional {
			return nil
		}
		return []string{anyResource}
	}

	resources := make([]string, 0, len(ids))
	for _, id := range ids {
		kind := s.kind
		if s.byPrefix {
			resolved, ok := arn.EC2TypeForID(id)
			// An id whose prefix names no type cannot name a real resource, so
			// the inert fence protects nothing; a sentinel type would fence the
			// wrong object instead.
			if !ok {
				resources = append(resources, anyResource)
				continue
			}
			kind = resolved
		}
		resources = append(resources, arn.FormatEC2(kind, region, accountID, id))
	}
	return resources
}

// identifiers takes the first parameter carrying a value, in the order
// awsec2query resolves them.
func (s *resourceScope) identifiers(q map[string]string) []string {
	for _, param := range s.params {
		if !s.list {
			if v := q[param]; v != "" {
				return []string{v}
			}
			continue
		}
		if ids := collectIndexed(q, param); len(ids) > 0 {
			return ids
		}
	}
	return nil
}

// collectIndexed gathers an indexed list: param.1, param.2, or the member-wrapped
// param.member.1 form the query parser also accepts.
//
// Gaps are not treated as terminators. A non-conforming client can send a
// non-contiguous list, and stopping at the gap would silently drop the resources
// after it, which is the under-check this exists to close.
func collectIndexed(q map[string]string, param string) []string {
	for _, prefix := range []string{param + ".member.", param + "."} {
		if ids := collectNumbered(q, prefix); len(ids) > 0 {
			return ids
		}
	}
	return nil
}

// collectNumbered returns the values of prefix+<digits>, ordered numerically so
// the denial log is reproducible. The digits-only test is what stops
// Filter.1.Value.1 being collected by a scope whose parameter is Filter.
func collectNumbered(q map[string]string, prefix string) []string {
	type indexed struct {
		n     int
		value string
	}
	var found []indexed
	for key, value := range q {
		rest, ok := strings.CutPrefix(key, prefix)
		if !ok || value == "" {
			continue
		}
		n, err := strconv.Atoi(rest)
		if err != nil || n < 1 {
			continue
		}
		found = append(found, indexed{n: n, value: value})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].n < found[j].n })

	ids := make([]string, 0, len(found))
	for _, f := range found {
		ids = append(ids, f.value)
	}
	return ids
}
