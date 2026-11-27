package elbv2

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	resourcearn "github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
)

// maxLBNameLength is AWS's limit on a load balancer name.
const maxLBNameLength = 32

var lbNamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)

// ovnFixture selects the OVN create path. Only a test constructor sets it; the
// daemon and the synchronous EKS paths keep the appliance path.
type ovnFixture struct {
	owners  *fixtureOwnerStore
	parents *ec2vpc.Parents
	groups  *ec2vpc.GroupLookup
	// instances is the EC2 named instance lookup that checks registrations.
	instances projection.NamedInstanceLookup
	// vips reserves the private load balancer addresses. Nil refuses reservation.
	vips *ec2vpc.IPAM
}

// lbNameProblem applies AWS's load balancer name rules. It returns the reason
// a name breaks them, or "" for a valid name.
func lbNameProblem(name string) string {
	switch {
	case len(name) > maxLBNameLength:
		return fmt.Sprintf("The load balancer name '%s' cannot be longer than '%d' characters", name, maxLBNameLength)
	case strings.HasPrefix(name, "internal-"):
		return fmt.Sprintf("The load balancer name '%s' cannot begin with 'internal-'", name)
	case !lbNamePattern.MatchString(name):
		return fmt.Sprintf("The load balancer name '%s' can contain only alphanumeric characters and hyphens, and cannot begin or end with a hyphen", name)
	}
	return ""
}

func invalidOVNConfiguration(format string, args ...any) error {
	return awserrors.Errorf(awserrors.ErrorELBv2InvalidConfigurationRequest, format, args...)
}

// ovnFixtureShape checks the original request fields against the one supported
// profile: an internal IPv4 network load balancer with one subnet and one to
// five supplied groups. It returns the subnet and the groups in request order.
func ovnFixtureShape(input *elbv2.CreateLoadBalancerInput) (string, []string, error) {
	if aws.StringValue(input.Type) != LoadBalancerTypeNetwork {
		return "", nil, invalidOVNConfiguration("Only the network load balancer type is supported")
	}
	if aws.StringValue(input.Scheme) != SchemeInternal {
		return "", nil, invalidOVNConfiguration("Only the internal scheme is supported")
	}
	if t := aws.StringValue(input.IpAddressType); t != "" && t != IPAddressTypeIPv4 {
		return "", nil, invalidOVNConfiguration("Only the ipv4 IP address type is supported")
	}
	if aws.StringValue(input.CustomerOwnedIpv4Pool) != "" {
		return "", nil, invalidOVNConfiguration("Customer-owned IPv4 pools are not supported")
	}

	var subnetID string
	switch {
	case len(input.Subnets) == 1 && len(input.SubnetMappings) == 0:
		subnetID = aws.StringValue(input.Subnets[0])
	case len(input.Subnets) == 0 && len(input.SubnetMappings) == 1 && input.SubnetMappings[0] != nil:
		m := input.SubnetMappings[0]
		if aws.StringValue(m.PrivateIPv4Address) != "" || aws.StringValue(m.AllocationId) != "" || aws.StringValue(m.IPv6Address) != "" {
			return "", nil, invalidOVNConfiguration("Subnet mappings support only a subnet ID")
		}
		subnetID = aws.StringValue(m.SubnetId)
	default:
		return "", nil, invalidOVNConfiguration("Exactly one subnet is required")
	}
	if subnetID == "" {
		return "", nil, invalidOVNConfiguration("Exactly one subnet is required")
	}

	if len(input.SecurityGroups) == 0 || len(input.SecurityGroups) > maxLBSecurityGroups {
		return "", nil, invalidOVNConfiguration("Between 1 and %d security groups are required", maxLBSecurityGroups)
	}
	groups := make([]string, 0, len(input.SecurityGroups))
	for _, sg := range input.SecurityGroups {
		id := aws.StringValue(sg)
		if id == "" || slices.Contains(groups, id) {
			return "", nil, invalidOVNConfiguration("Security group IDs must be distinct and not empty")
		}
		groups = append(groups, id)
	}
	return subnetID, groups, nil
}

// createOVNFixtureLoadBalancer admits the supported request as a provisioning
// OVN root. It writes nothing until the shape, an equal sealed retry, the
// subnet and every group are checked.
func (s *ELBv2ServiceImpl) createOVNFixtureLoadBalancer(ctx context.Context, input *elbv2.CreateLoadBalancerInput, accountID string) (*elbv2.CreateLoadBalancerOutput, error) {
	name := aws.StringValue(input.Name)
	if name == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if problem := lbNameProblem(name); problem != "" {
		return nil, awserrors.Errorf(awserrors.ErrorValidationError, "%s", problem)
	}
	tags, err := tagsFromSDK(input.Tags)
	if err != nil {
		return nil, err
	}
	subnetID, groups, err := ovnFixtureShape(input)
	if err != nil {
		return nil, err
	}
	want := &LoadBalancerRecord{Type: LoadBalancerTypeNetwork, Scheme: SchemeInternal, Subnets: []string{subnetID}, SecurityGroups: groups, Tags: tags}

	existing, err := s.ovn.owners.GetSealed(ctx, name, accountID)
	if err != nil {
		slog.ErrorContext(ctx, "CreateLoadBalancer: fixture name read failed", "name", name, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if existing != nil {
		return ovnFixtureRetry(existing, want, name)
	}

	subnet, _, err := s.ovn.parents.Subnet(ctx, accountID, subnetID)
	if err != nil {
		slog.ErrorContext(ctx, "CreateLoadBalancer: subnet read failed", "subnetId", subnetID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if subnet == nil || subnet.Deleting {
		return nil, awserrors.Errorf(awserrors.ErrorELBv2SubnetNotFound, "The subnet ID '%s' is not valid", subnetID)
	}
	for _, groupID := range groups {
		vpcID, found, err := s.ovn.groups.GroupVPC(ctx, accountID, groupID)
		if err != nil {
			slog.ErrorContext(ctx, "CreateLoadBalancer: security group read failed", "groupId", groupID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if !found || vpcID != subnet.VpcId {
			return nil, awserrors.Errorf(awserrors.ErrorELBv2InvalidSecurityGroup, "Security group '%s' does not exist in VPC '%s'", groupID, subnet.VpcId)
		}
	}

	lbID := awsidentifiers.GenerateResourceID("lb")
	root := &LoadBalancerRecord{
		LoadBalancerArn: resourcearn.FormatELBv2LoadBalancer(s.region, accountID, name, lbID, LoadBalancerTypeNetwork),
		LoadBalancerID:  lbID,
		Name:            name,
		Scheme:          SchemeInternal,
		Type:            LoadBalancerTypeNetwork,
		Realization:     realizationOVN,
		State:           StateProvisioning,
		VpcId:           subnet.VpcId,
		SecurityGroups:  groups,
		Subnets:         []string{subnetID},
		AvailZones:      []AvailZoneInfo{{ZoneName: subnet.AvailabilityZone, SubnetId: subnetID}},
		IPAddressType:   IPAddressTypeIPv4,
		Tags:            tags,
		AccountID:       accountID,
		CreatedAt:       s.ovn.owners.now().UTC(),
	}
	claim, sealed, err := s.ovn.owners.Begin(ctx, root)
	switch {
	case errors.Is(err, errFixtureNameBusy):
		return nil, errDuplicateLoadBalancerName(name)
	case err != nil:
		slog.ErrorContext(ctx, "CreateLoadBalancer: fixture name claim failed", "name", name, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	case sealed != nil:
		return ovnFixtureRetry(sealed, want, name)
	}

	out, err := s.ovn.owners.admitAndSeal(ctx, claim)
	switch {
	case err == nil:
		slog.InfoContext(ctx, "CreateLoadBalancer accepted", "name", name, "lbArn", out.LoadBalancerArn, "realization", realizationOVN, "accountID", accountID)
		return &elbv2.CreateLoadBalancerOutput{LoadBalancers: []*elbv2.LoadBalancer{ovnFixtureToSDK(out)}}, nil
	case errors.Is(err, errFixtureParentBusy):
		return nil, invalidOVNConfiguration("The subnet or VPC is already used by another load balancer of this type")
	case errors.Is(err, errFixtureSubnetUnavailable), errors.Is(err, errFixtureVPCUnavailable):
		return nil, awserrors.Errorf(awserrors.ErrorELBv2SubnetNotFound, "The subnet ID '%s' is not valid", subnetID)
	}
	slog.ErrorContext(ctx, "CreateLoadBalancer: fixture admission failed", "name", name, "lbId", lbID, "err", err)
	return nil, errors.New(awserrors.ErrorServerInternal)
}

// ovnFixtureRetry returns the sealed root for an equal request and
// DuplicateLoadBalancerName for any other.
func ovnFixtureRetry(existing, want *LoadBalancerRecord, name string) (*elbv2.CreateLoadBalancerOutput, error) {
	if existing.Type == want.Type && existing.Scheme == want.Scheme &&
		sameStringSet(existing.Subnets, want.Subnets) && sameStringSet(existing.SecurityGroups, want.SecurityGroups) &&
		maps.Equal(existing.Tags, want.Tags) {
		return &elbv2.CreateLoadBalancerOutput{LoadBalancers: []*elbv2.LoadBalancer{ovnFixtureToSDK(existing)}}, nil
	}
	return nil, errDuplicateLoadBalancerName(name)
}

// ovnFixtureToSDK projects a root that has no endpoint yet: DNSName is absent
// and no zone carries an address.
func ovnFixtureToSDK(r *LoadBalancerRecord) *elbv2.LoadBalancer {
	lb := &elbv2.LoadBalancer{
		LoadBalancerArn:  aws.String(r.LoadBalancerArn),
		LoadBalancerName: aws.String(r.Name),
		Scheme:           aws.String(r.Scheme),
		Type:             aws.String(r.Type),
		IpAddressType:    aws.String(r.IPAddressType),
		CreatedTime:      aws.Time(r.CreatedAt),
		VpcId:            aws.String(r.VpcId),
		SecurityGroups:   aws.StringSlice(r.SecurityGroups),
		State:            &elbv2.LoadBalancerState{Code: aws.String(r.State)},
	}
	for _, az := range r.AvailZones {
		lb.AvailabilityZones = append(lb.AvailabilityZones, &elbv2.AvailabilityZone{
			ZoneName: aws.String(az.ZoneName),
			SubnetId: aws.String(az.SubnetId),
		})
	}
	return lb
}

// describeOVNFixtureLoadBalancer resolves exactly one load balancer ARN to its
// sealed root by point reads. The caller already refuses pagination with ARNs.
func (s *ELBv2ServiceImpl) describeOVNFixtureLoadBalancer(ctx context.Context, input *elbv2.DescribeLoadBalancersInput, accountID string) (*elbv2.DescribeLoadBalancersOutput, error) {
	if len(input.Names) > 0 || len(input.LoadBalancerArns) != 1 {
		return nil, awserrors.Errorf(awserrors.ErrorValidationError, "Specify exactly one load balancer ARN")
	}
	arn := aws.StringValue(input.LoadBalancerArns[0])
	root, err := s.ovn.owners.GetSealedByArn(ctx, arn, s.region, accountID)
	if err != nil {
		slog.ErrorContext(ctx, "DescribeLoadBalancers: fixture root read failed", "arn", arn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if root == nil {
		return nil, awserrors.Errorf(awserrors.ErrorELBv2LoadBalancerNotFound, "One or more load balancers not found")
	}
	return &elbv2.DescribeLoadBalancersOutput{LoadBalancers: []*elbv2.LoadBalancer{ovnFixtureToSDK(root)}}, nil
}

// refuseOVNFixtureDelete refuses deletion of a sealed root: its data path and
// parent release are not built yet. Other ARNs take the existing delete path.
func (s *ELBv2ServiceImpl) refuseOVNFixtureDelete(ctx context.Context, arn, accountID string) error {
	root, err := s.ovn.owners.GetSealedByArn(ctx, arn, s.region, accountID)
	if err != nil {
		slog.ErrorContext(ctx, "DeleteLoadBalancer: fixture root read failed", "arn", arn, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if root != nil {
		return awserrors.Errorf(awserrors.ErrorOperationNotPermitted, "Load balancer '%s' cannot be deleted yet", arn)
	}
	return nil
}
