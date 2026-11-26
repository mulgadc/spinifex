package account

import (
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
)

// idFormatResource is one resource type AWS reports in the ID-format calls,
// with the switch-over deadline AWS gives for it in ap-southeast-2.
type idFormatResource struct {
	name     string
	deadline time.Time
}

var (
	deadline2016 = time.Date(2016, 12, 7, 12, 0, 0, 0, time.UTC)
	deadline2018 = time.Date(2018, 8, 9, 12, 0, 0, 0, time.UTC)
)

// idFormatResources is every resource type AWS lists, in the order
// DescribeIdFormat returns them. Every Spinifex ID is already long.
var idFormatResources = []idFormatResource{
	{"reservation", deadline2016},
	{"instance", deadline2016},
	{"volume", deadline2016},
	{"snapshot", deadline2016},
	{"network-interface-attachment", deadline2018},
	{"network-interface", deadline2018},
	{"elastic-ip-allocation", deadline2018},
	{"elastic-ip-association", deadline2018},
	{"vpc", deadline2018},
	{"subnet", deadline2018},
	{"route-table", deadline2018},
	{"route-table-association", deadline2018},
	{"network-acl", deadline2018},
	{"network-acl-association", deadline2018},
	{"dhcp-options", deadline2018},
	{"internet-gateway", deadline2018},
	{"vpc-cidr-block-association", deadline2018},
	{"vpc-ipv6-cidr-block-association", deadline2018},
	{"subnet-ipv6-cidr-block-association", deadline2018},
	{"vpc-peering-connection", deadline2018},
	{"security-group", deadline2018},
	{"flow-log", deadline2018},
	{"vpc-endpoint", deadline2018},
	{"conversion-task", deadline2018},
	{"export-task", deadline2018},
	{"import-task", deadline2018},
	{"image", deadline2018},
	{"bundle", deadline2018},
	{"customer-gateway", deadline2018},
	{"vpn-connection", deadline2018},
	{"vpn-gateway", deadline2018},
}

// aggregateDeadlineShift is how much earlier DescribeAggregateIdFormat
// reports each deadline than the per-principal calls do.
const aggregateDeadlineShift = 10 * time.Hour

// idFormatStatuses returns the statuses for the named resource types, or all
// of them when none is named, in the fixed order. An unknown type is
// InvalidResourceType.Unknown.
func idFormatStatuses(resources []string, shift time.Duration) ([]*ec2.IdFormat, error) {
	for _, r := range resources {
		if !slices.ContainsFunc(idFormatResources, func(f idFormatResource) bool { return f.name == r }) {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidResourceTypeUnknown, "Invalid resource type")
		}
	}
	statuses := []*ec2.IdFormat{}
	for _, f := range idFormatResources {
		if len(resources) > 0 && !slices.Contains(resources, f.name) {
			continue
		}
		statuses = append(statuses, &ec2.IdFormat{
			Resource:   aws.String(f.name),
			UseLongIds: aws.Bool(true),
			Deadline:   aws.Time(f.deadline.Add(-shift)),
		})
	}
	return statuses, nil
}

func optionalResource(resource *string) []string {
	if resource == nil {
		return nil
	}
	return []string{*resource}
}

// DescribeIdFormat implements the EC2 DescribeIdFormat action.
func DescribeIdFormat(input *ec2.DescribeIdFormatInput) (*ec2.DescribeIdFormatOutput, error) {
	statuses, err := idFormatStatuses(optionalResource(input.Resource), 0)
	if err != nil {
		return nil, err
	}
	return &ec2.DescribeIdFormatOutput{Statuses: statuses}, nil
}

// DescribeIdentityIdFormat implements the EC2 DescribeIdentityIdFormat action.
// Like AWS it does not check that the principal exists, only that its ARN is
// an IAM root, user or role in the caller's account.
func DescribeIdentityIdFormat(input *ec2.DescribeIdentityIdFormatInput, accountID string) (*ec2.DescribeIdentityIdFormatOutput, error) {
	if input.PrincipalArn == nil {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "The request must contain the parameter principalArn")
	}
	principal := *input.PrincipalArn
	if !isAccountPrincipal(principal, accountID) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidTargetArnUnknown, "Invalid TargetArn: %s", principal)
	}
	statuses, err := idFormatStatuses(optionalResource(input.Resource), 0)
	if err != nil {
		return nil, err
	}
	return &ec2.DescribeIdentityIdFormatOutput{Statuses: statuses}, nil
}

func isAccountPrincipal(principal, accountID string) bool {
	service, region, account, resource, ok := arn.Split(principal)
	if !ok || service != "iam" || region != "" || account != accountID {
		return false
	}
	if resource == "root" {
		return true
	}
	for _, kind := range []arn.IAMResourceType{arn.IAMUser, arn.IAMRole} {
		if name, found := strings.CutPrefix(resource, string(kind)+"/"); found && name != "" {
			return true
		}
	}
	return false
}

// DescribeAggregateIdFormat implements the EC2 DescribeAggregateIdFormat
// action. AWS reports its deadlines at 02:00 rather than 12:00 UTC.
func DescribeAggregateIdFormat(_ *ec2.DescribeAggregateIdFormatInput) (*ec2.DescribeAggregateIdFormatOutput, error) {
	statuses, _ := idFormatStatuses(nil, aggregateDeadlineShift)
	return &ec2.DescribeAggregateIdFormatOutput{
		Statuses:             statuses,
		UseLongIdsAggregated: aws.Bool(true),
	}, nil
}

// DescribePrincipalIdFormat implements the EC2 DescribePrincipalIdFormat
// action. The account root is the only principal, so one page holds it all.
// The gateway checks MaxResults; any valid token resumes at the one principal.
func DescribePrincipalIdFormat(input *ec2.DescribePrincipalIdFormatInput, accountID string) (*ec2.DescribePrincipalIdFormatOutput, error) {
	if _, err := paging.DecodeToken(aws.StringValue(input.NextToken)); err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"Value ( %s ) for parameter NextToken is invalid. The token is invalid.", aws.StringValue(input.NextToken))
	}
	statuses, err := idFormatStatuses(aws.StringValueSlice(input.Resources), 0)
	if err != nil {
		return nil, err
	}
	return &ec2.DescribePrincipalIdFormatOutput{
		Principals: []*ec2.PrincipalIdFormat{{
			Arn:      aws.String(arn.FormatIAMRoot(accountID)),
			Statuses: statuses,
		}},
	}, nil
}
