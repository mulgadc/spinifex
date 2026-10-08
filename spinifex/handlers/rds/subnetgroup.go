package handlers_rds

import (
	"context"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/domains/rds/subnetgroup"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// AWS's own name rules. The name is a KV key rather than a DNS label, so the
// looser AWS character set is accepted as-is — but "default" is reserved.
const (
	maxDBGroupNameLen        = 255
	maxDBGroupDescriptionLen = 255
)

// The status AWS reports on a usable group. There is no asynchronous
// provisioning behind a subnet group here, so it is complete the moment it is
// written.
const subnetGroupStatusComplete = "Complete"

// CreateDBSubnetGroup implements the RDS CreateDBSubnetGroup action. AWS requires a group to span at
// least two AZs, but this platform is single-AZ and multi-AZ is a V2 milestone, so that rule is not
// enforced: rejecting on it would fail every stock Terraform module for no safety benefit. The
// constraint that does matter is enforced by the owner, because a group spanning two VPCs cannot host
// an instance at all.
func (s *Service) CreateDBSubnetGroup(ctx context.Context, input *rds.CreateDBSubnetGroupInput, accountID string) (*rds.CreateDBSubnetGroupOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	name := aws.StringValue(input.DBSubnetGroupName)
	if err := validateDBGroupName("DBSubnetGroupName", name); err != nil {
		return nil, err
	}
	description := aws.StringValue(input.DBSubnetGroupDescription)
	if err := validateDBGroupDescription("DBSubnetGroupDescription", description); err != nil {
		return nil, err
	}
	tags, err := validateTags(input.Tags)
	if err != nil {
		return nil, err
	}

	rec, err := s.subnetGroups().Create(ctx, accountID, subnetgroup.Spec{
		Name:        name,
		Description: description,
		SubnetIDs:   aws.StringValueSlice(input.SubnetIds),
		Tags:        tags,
	})
	if err != nil {
		return nil, err
	}
	return &rds.CreateDBSubnetGroupOutput{DBSubnetGroup: s.projectSubnetGroup(rec)}, nil
}

// DescribeDBSubnetGroups errors on a named group that does not exist, matching AWS; an unnamed request
// lists the account's groups. Filters is not read: AWS ignores it here, even an unknown or malformed entry.
func (s *Service) DescribeDBSubnetGroups(ctx context.Context, input *rds.DescribeDBSubnetGroupsInput, accountID string) (*rds.DescribeDBSubnetGroupsOutput, error) {
	if input == nil {
		input = &rds.DescribeDBSubnetGroupsInput{}
	}
	if name := aws.StringValue(input.DBSubnetGroupName); name != "" {
		rec, err := s.subnetGroups().Get(ctx, accountID, name)
		if err != nil {
			return nil, err
		}
		return &rds.DescribeDBSubnetGroupsOutput{DBSubnetGroups: []*rds.DBSubnetGroup{s.projectSubnetGroup(rec)}}, nil
	}

	recs, err := s.subnetGroups().List(ctx, accountID)
	if err != nil {
		return nil, err
	}
	groups := make([]*rds.DBSubnetGroup, 0, len(recs))
	for i := range recs {
		groups = append(groups, s.projectSubnetGroup(&recs[i]))
	}
	groups, next, err := Page(groups, subnetGroupPageKey, input.MaxRecords, input.Marker)
	if err != nil {
		return nil, err
	}
	return &rds.DescribeDBSubnetGroupsOutput{DBSubnetGroups: groups, Marker: next}, nil
}

// ModifyDBSubnetGroup replaces the group's subnet set with SubnetIds. An omitted or empty description
// keeps the stored one.
func (s *Service) ModifyDBSubnetGroup(ctx context.Context, input *rds.ModifyDBSubnetGroupInput, accountID string) (*rds.ModifyDBSubnetGroupOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	name := aws.StringValue(input.DBSubnetGroupName)
	if name == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBSubnetGroupName is required")
	}
	description := aws.StringValue(input.DBSubnetGroupDescription)
	if description != "" {
		if err := validateDBGroupDescription("DBSubnetGroupDescription", description); err != nil {
			return nil, err
		}
	}

	rec, err := s.subnetGroups().Modify(ctx, accountID, name, description, aws.StringValueSlice(input.SubnetIds))
	if err != nil {
		return nil, err
	}
	return &rds.ModifyDBSubnetGroupOutput{DBSubnetGroup: s.projectSubnetGroup(rec)}, nil
}

func (s *Service) DeleteDBSubnetGroup(ctx context.Context, input *rds.DeleteDBSubnetGroupInput, accountID string) (*rds.DeleteDBSubnetGroupOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	name := aws.StringValue(input.DBSubnetGroupName)
	if name == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBSubnetGroupName is required")
	}
	if err := s.subnetGroups().Delete(ctx, accountID, name); err != nil {
		return nil, err
	}
	return &rds.DeleteDBSubnetGroupOutput{}, nil
}

// The group's lifecycle owner, bound to this Service's networking and bucket.
func (s *Service) subnetGroups() *subnetgroup.Owner {
	return subnetgroup.New(s.deps.Network, instanceReferences{s}, s.bucket)
}

// The instance side of the group's in-use guard. It reads instance records
// directly until instances have their own lifecycle owner.
type instanceReferences struct{ s *Service }

var _ subnetgroup.Dependants = instanceReferences{}

func (r instanceReferences) InstancesUsing(ctx context.Context, accountID, name string) ([]string, error) {
	kv, err := r.s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return instancesUsingGroup(ctx, kv, func(rec *DBInstanceRecord) bool {
		return rec.DBSubnetGroupName == name
	})
}

// The identifiers of every instance in the account matching uses, sorted. Both
// group deletes share it: an in-use guard that missed one instance would strand
// a live database's configuration.
func instancesUsingGroup(ctx context.Context, kv *kvstore.Bucket, uses func(*DBInstanceRecord) bool) ([]string, error) {
	ids, err := ListDBInstanceIDs(ctx, kv)
	if err != nil {
		return nil, err
	}
	slices.Sort(ids)

	var users []string
	for _, id := range ids {
		var rec DBInstanceRecord
		found, err := getJSON(ctx, kv, DBInstanceKey(id), &rec)
		if err != nil {
			return nil, err
		}
		if found && uses(&rec) {
			users = append(users, id)
		}
	}
	return users, nil
}

func (s *Service) projectSubnetGroup(rec *subnetgroup.Record) *rds.DBSubnetGroup {
	if rec == nil {
		return nil
	}
	out := &rds.DBSubnetGroup{
		DBSubnetGroupName:        aws.String(rec.Name),
		DBSubnetGroupDescription: aws.String(rec.Description),
		DBSubnetGroupArn:         aws.String(FormatARN(ResourceKindDBSubnetGroup, s.region, rec.AccountID, rec.Name)),
		SubnetGroupStatus:        aws.String(subnetGroupStatusComplete),
		VpcId:                    aws.String(rec.VpcID),
		SupportedNetworkTypes:    aws.StringSlice([]string{networkTypeIPv4}),
	}
	for _, subnet := range rec.Subnets {
		// Empty rather than nil: AWS reports SubnetOutpost as {} on every subnet,
		// and the XML marshaller omits a nil pointer entirely.
		member := &rds.Subnet{
			SubnetIdentifier: aws.String(subnet.SubnetID),
			SubnetStatus:     aws.String("Active"),
			SubnetOutpost:    &rds.Outpost{},
		}
		if subnet.AvailabilityZone != "" {
			member.SubnetAvailabilityZone = &rds.AvailabilityZone{Name: aws.String(subnet.AvailabilityZone)}
		}
		out.Subnets = append(out.Subnets, member)
	}
	return out
}

// Shared by both group types: AWS applies the same name rules to each, and
// "default" is reserved on both because a default parameter group is implicit.
func validateDBGroupName(field, name string) error {
	switch {
	case name == "":
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s is required", field)
	case len(name) > maxDBGroupNameLen:
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%s must be at most %d characters", field, maxDBGroupNameLen)
	case !isLetter(rune(name[0])):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s must begin with a letter", field)
	case strings.EqualFold(name, "default"):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%s may not be \"default\", which the service reserves", field)
	}
	for _, r := range name {
		if !isLetter(r) && !isDigit(r) && r != '-' {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"%s may contain only letters, digits and hyphens", field)
		}
	}
	return nil
}

func validateDBGroupDescription(field, description string) error {
	switch {
	case description == "":
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s is required", field)
	case len(description) > maxDBGroupDescriptionLen:
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%s must be at most %d characters", field, maxDBGroupDescriptionLen)
	}
	return nil
}
